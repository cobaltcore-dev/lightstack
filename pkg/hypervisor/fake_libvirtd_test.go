package hypervisor

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt/socket"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// Remote-protocol procedures (libvirt's remote_protocol.x) the fake libvirtd answers
// with a payload, or that tests look for among the calls it received.
const (
	procConnectGetCapabilities = 7
	procDomainCreate           = 9
	procDomainDefineXML        = 11
	procDomainGetXMLDesc       = 14
	procDomainDestroy          = 12
	procDomainLookupByUUID     = 24
	procDomainReboot           = 27
	procAuthList               = 66
	procDomainIsActive         = 150
	procConnectGetLibVersion   = 157
)

// fakeLibvirtd serves libvirt's remote protocol for a single domain. Each reply carries
// its call's serial, as libvirtd's do. go-libvirt's own libvirttest mock numbers replies
// from a counter of its own, which cannot follow a client across a reconnect: the
// client's serials carry on while a fresh mock starts again at one.
//
// It records every call's procedure, so a test can assert what a VMManager method
// actually asked libvirt to do. A call it has no payload for gets an empty successful
// reply.
type fakeLibvirtd struct {
	active  bool     // what DomainIsActive reports; set before serving
	stopped bool     // DomainLookupByUUID reports ID -1, as libvirt does for a domain not running
	caps    string   // what ConnectGetCapabilities reports; set before serving
	xml     string   // what DomainGetXMLDesc reports; set before serving
	uuid    [16]byte // the one domain's UUID, in DomainDefineXML's and DomainLookupByUUID's replies

	// capsDelay holds back the first ConnectGetCapabilities reply, across all connections,
	// as a socket-activated daemon probing QEMU on its first call does. Later calls on the
	// same connection queue behind it, as they would on the daemon.
	capsDelay time.Duration
	// silentConns is how many connections, the first ones, are read but never answered: a
	// daemon that accepted the socket and then hangs.
	silentConns int

	mu          sync.Mutex
	calls       []uint32
	conns       int
	capsDelayed bool
}

// setCaps changes what ConnectGetCapabilities reports from the next call on: libvirt's
// configuration changing between connections, as across a daemon restart.
func (f *fakeLibvirtd) setCaps(caps string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caps = caps
}

// capsCalls is how many ConnectGetCapabilities calls the fake has received.
func (f *fakeLibvirtd) capsCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.calls {
		if p == procConnectGetCapabilities {
			n++
		}
	}
	return n
}

func (f *fakeLibvirtd) serve(conn net.Conn) {
	defer conn.Close()
	f.mu.Lock()
	f.conns++
	silent := f.conns <= f.silentConns
	f.mu.Unlock()
	if silent {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	for {
		// A packet is its length (these 4 bytes included), a 24-byte header —
		// program, version, procedure, type, serial, status — and a payload.
		var head [28]byte
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			return
		}
		rest := int64(binary.BigEndian.Uint32(head[0:4])) - int64(len(head))
		if _, err := io.CopyN(io.Discard, conn, rest); err != nil {
			return
		}
		proc := binary.BigEndian.Uint32(head[12:16])
		f.record(proc)
		if proc == procConnectGetCapabilities {
			f.mu.Lock()
			delay := f.capsDelay
			if f.capsDelayed {
				delay = 0
			}
			f.capsDelayed = true
			f.mu.Unlock()
			time.Sleep(delay)
		}

		payload := f.payload(proc)
		reply := make([]byte, len(head)+len(payload))
		binary.BigEndian.PutUint32(reply[0:4], uint32(len(reply)))
		copy(reply[4:16], head[4:16]) // program, version, procedure
		binary.BigEndian.PutUint32(reply[16:20], socket.Reply)
		copy(reply[20:24], head[20:24]) // the call's serial
		binary.BigEndian.PutUint32(reply[24:28], socket.StatusOK)
		copy(reply[len(head):], payload)
		if _, err := conn.Write(reply); err != nil {
			return
		}
	}
}

func (f *fakeLibvirtd) payload(proc uint32) []byte {
	switch proc {
	case procAuthList:
		return make([]byte, 4) // an empty list: no authentication required
	case procConnectGetLibVersion:
		return make([]byte, 8)
	case procDomainLookupByUUID:
		if f.stopped {
			return f.domain(-1)
		}
		return f.domain(1)
	case procDomainDefineXML:
		return f.domain(-1) // defined, not yet running
	case procConnectGetCapabilities:
		f.mu.Lock()
		defer f.mu.Unlock()
		return xdrString(f.caps)
	case procDomainGetXMLDesc:
		return xdrString(f.xml)
	case procDomainIsActive:
		active := make([]byte, 4)
		if f.active {
			binary.BigEndian.PutUint32(active, 1)
		}
		return active
	}
	return nil
}

// xdrString is s as an XDR string: its length, the bytes, then padding to four.
func xdrString(s string) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(s)))
	b = append(b, s...)
	return append(b, make([]byte, (4-len(s)%4)%4)...)
}

// domain is the XDR encoding of the one domain the fake knows: its name as an XDR
// string, padded to four bytes, then its 16-byte UUID and the given id.
func (f *fakeLibvirtd) domain(id int32) []byte {
	name := "instance-fake"
	b := binary.BigEndian.AppendUint32(nil, uint32(len(name)))
	b = append(b, name...)
	b = append(b, make([]byte, (4-len(name)%4)%4)...)
	b = append(b, f.uuid[:]...)
	return binary.BigEndian.AppendUint32(b, uint32(id))
}

func (f *fakeLibvirtd) record(proc uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, proc)
}

// domainCalls returns the domain procedures received, in order: what a VMManager
// method did, without the connection handshake around it.
func (f *fakeLibvirtd) domainCalls() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uint32
	for _, p := range f.calls {
		switch p {
		case procDomainCreate, procDomainDefineXML, procDomainDestroy, procDomainLookupByUUID, procDomainReboot, procDomainIsActive:
			out = append(out, p)
		}
	}
	return out
}

// pipeDialer stands in for libvirtd's socket: every dial reaches a fake libvirtd over a
// fresh in-memory pipe, and drop makes the current connection go away, as a libvirtd
// restart does.
type pipeDialer struct {
	fake *fakeLibvirtd // nil: a fresh default fake per dial

	mu     sync.Mutex
	server net.Conn
	dials  int
}

func (d *pipeDialer) Dial() (net.Conn, error) {
	client, server := net.Pipe()
	f := d.fake
	if f == nil {
		f = &fakeLibvirtd{}
	}
	go f.serve(server)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.server = server
	d.dials++
	return client, nil
}

func (d *pipeDialer) drop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.server.Close()
}

// abandon is drop, under the name dialWithin calls to give up on a handshake.
func (d *pipeDialer) abandon() { d.drop() }

func (d *pipeDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

// waitFor polls cond until it holds, failing the test after two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForWithin(t, what, 2*time.Second, cond)
}

// waitForWithin polls cond until it holds, failing the test after d. The connection and
// preparation retries need more than waitFor's two seconds: a first reply held past the
// 1-second bound, then a retry, already takes about that long.
func waitForWithin(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// socketDir is a fresh directory with a short path. A unix socket path must fit the
// 108-byte sun_path, which t.TempDir's name-derived paths can exceed in a subtest.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serveFakeOnSocket serves f on a real unix socket at path until the test ends: libvirt's
// socket as a daemon's socket unit provides it.
func serveFakeOnSocket(t *testing.T, path string, f *fakeLibvirtd) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
}

// useTempBase points the state path at a directory of the test's own until it ends. A guest
// start makes the instance's directory QEMU's before it launches (SPEC-031 FR-014), which
// needs somewhere writable to create it.
func useTempBase(t *testing.T) {
	t.Helper()
	prev := paths.Base()
	paths.SetBase(t.TempDir())
	t.Cleanup(func() { paths.SetBase(prev) })
}

func newConnectedManager(t *testing.T, d *pipeDialer) *VMManager {
	t.Helper()
	m := &VMManager{mode: "real", stubVMs: make(map[string]*stubVM)}
	if err := m.connect(d, time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Stop the connection loop with the test: a loop left running keeps dialing, preparing and
	// logging into the next test, whose log capture then races with it.
	t.Cleanup(m.Close)
	// As the connection loop would have: starts are refused until the console socket
	// directory is QEMU's (FR-017a), which these tests are not about.
	m.prepared.Store(true)
	return m
}

// newConnectedManagerWith is newConnectedManager against one given fake libvirtd.
func newConnectedManagerWith(t *testing.T, f *fakeLibvirtd) *VMManager {
	t.Helper()
	return newConnectedManager(t, &pipeDialer{fake: f})
}
