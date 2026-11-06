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
	if uri == "" {
		return LibvirtTarget{}, nil
	}
	if strings.HasPrefix(uri, "/") {
		return LibvirtTarget{Socket: uri}, nil
	}
	if u, err := url.Parse(uri); err == nil && isLocalSystemURI(u) {
		if socket, ok := socketParam(u.RawQuery); ok {
			return LibvirtTarget{Socket: socket}, nil
		}
	}
	return LibvirtTarget{}, fmt.Errorf("nova.libvirt_uri %q is not supported; accepted: %s", uri, acceptedLibvirtURIs)
}

// isLocalSystemURI reports whether u is qemu:///system or qemu+unix:///system: the QEMU
// driver's system instance on this host, with no user, host or fragment.
func isLocalSystemURI(u *url.URL) bool {
	return (u.Scheme == "qemu" || u.Scheme == "qemu+unix") &&
		u.Opaque == "" && u.User == nil && u.Host == "" && u.Path == "/system" && u.Fragment == ""
}

// socketParam reads the one parameter a libvirt_uri may carry, socket=<absolute path>, once.
// No parameter gives "": the automatic choice. Any other parameter, or a socket that is
// repeated or not absolute, is not ok.
func socketParam(rawQuery string) (socket string, ok bool) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", false
	}
	for key, values := range query {
		if key != "socket" || len(values) != 1 || !filepath.IsAbs(values[0]) {
			return "", false
		}
		socket = values[0]
	}
	return socket, true
}

// socketDialer connects to libvirt's socket for a target. It implements go-libvirt's
// socket.Dialer, which is asked for a connection on every connect and reconnect, so the
// automatic choice is made afresh each time: a daemon whose socket appears after Lightstack
// started is found on the next attempt.
//
// It remembers the socket of its latest attempt, for the log line and errors naming it. Giving up
// on an attempt is the attempt's business (dialAttempt), not the dialer's.
type socketDialer struct {
	target  LibvirtTarget
	runDir  string
	timeout time.Duration

	mu     sync.Mutex
	dialed string
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
	d.mu.Lock()
	d.dialed = path // recorded first, so an attempt abandoned mid-dial still names its socket
	d.mu.Unlock()
	conn, err := net.DialTimeout("unix", path, d.timeout)
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

// Dialed is the socket the latest Dial chose, or "" before any.
func (d *socketDialer) Dialed() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dialed
}
