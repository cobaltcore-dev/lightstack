package hypervisor

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LibvirtTarget is where nova.libvirt_uri says to connect: an explicit socket, or, with
// Socket empty, the automatic choice between the modular QEMU daemon's socket and the
// monolithic (or proxy) daemon's socket.
type LibvirtTarget struct {
	Socket string
}

// libvirtRunDir is the directory libvirt's daemons put their sockets in. Distributions that
// use /run link /var/run to it. A variable so tests can point it at a directory of their own.
var libvirtRunDir = "/var/run/libvirt"

const (
	// virtqemudSocket exists only where libvirt's modular QEMU daemon runs.
	virtqemudSocket = "virtqemud-sock"
	// libvirtSocket is the monolithic libvirtd's socket, or virtproxyd's on a host that runs
	// the modular daemons behind the proxy. Either way it reaches the QEMU driver.
	libvirtSocket = "libvirt-sock"

	acceptedLibvirtURIs = "qemu:///system or qemu+unix:///system (chooses virtqemud-sock, else libvirt-sock), " +
		"either with ?socket=<absolute path>, or an absolute socket path"
)

// ParseLibvirtURI reads nova.libvirt_uri. It accepts only what Lightstack can honour: the
// local QEMU system URI, which leaves the socket to the automatic choice, the same URI naming
// its socket, or a bare absolute socket path. Anything else — another driver, a session URI, a
// remote transport or host, a relative path, any other parameter — is refused with one
// message naming the value and the accepted forms, instead of being dialed as a file.
func ParseLibvirtURI(uri string) (LibvirtTarget, error) {
	refuse := fmt.Errorf("nova.libvirt_uri %q is not supported; accepted: %s", uri, acceptedLibvirtURIs)
	if uri == "" {
		return LibvirtTarget{}, nil
	}
	if strings.HasPrefix(uri, "/") {
		return LibvirtTarget{Socket: uri}, nil
	}
	u, err := url.Parse(uri)
	if err != nil {
		return LibvirtTarget{}, refuse
	}
	if (u.Scheme != "qemu" && u.Scheme != "qemu+unix") ||
		u.Opaque != "" || u.User != nil || u.Host != "" || u.Path != "/system" || u.Fragment != "" {
		return LibvirtTarget{}, refuse
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return LibvirtTarget{}, refuse
	}
	var target LibvirtTarget
	for key, values := range query {
		if key != "socket" || len(values) != 1 || !filepath.IsAbs(values[0]) {
			return LibvirtTarget{}, refuse
		}
		target.Socket = values[0]
	}
	return target, nil
}

// socketDialer connects to libvirt's socket for a target. It implements go-libvirt's
// socket.Dialer, which is asked for a connection on every connect and reconnect, so the
// automatic choice is made afresh each time: a daemon whose socket appears after Lightstack
// started is found on the next attempt.
//
// It remembers the socket it dialed last, for the log line naming it, and the connection it
// returned, so a caller can close it to abandon a handshake libvirt does not answer.
type socketDialer struct {
	target  LibvirtTarget
	runDir  string
	timeout time.Duration

	mu     sync.Mutex
	dialed string
	conn   net.Conn
}

func newSocketDialer(target LibvirtTarget, runDir string, timeout time.Duration) *socketDialer {
	return &socketDialer{target: target, runDir: runDir, timeout: timeout}
}

// Dial connects to the target's socket: the explicit one, or the modular QEMU daemon's
// socket when it exists and the monolithic one otherwise. "Exists" follows symlinks.
func (d *socketDialer) Dial() (net.Conn, error) {
	path, err := d.choose()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, d.timeout)
	d.mu.Lock()
	d.dialed, d.conn = path, conn
	d.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("dialing libvirt at %s: %w", path, err)
	}
	return conn, nil
}

func (d *socketDialer) choose() (string, error) {
	if d.target.Socket != "" {
		return d.target.Socket, nil
	}
	modular := filepath.Join(d.runDir, virtqemudSocket)
	monolithic := filepath.Join(d.runDir, libvirtSocket)
	for _, path := range []string{modular, monolithic} {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no libvirt socket: neither %s nor %s exists", modular, monolithic)
}

// Dialed is the socket the last Dial chose, or "" before any.
func (d *socketDialer) Dialed() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dialed
}

// abandon closes the connection the last Dial returned. go-libvirt has no deadline on its
// handshake; closing the connection under it is what makes a hung handshake return.
func (d *socketDialer) abandon() {
	d.mu.Lock()
	conn := d.conn
	d.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}
