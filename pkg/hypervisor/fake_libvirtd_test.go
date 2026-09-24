package hypervisor

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/digitalocean/go-libvirt/socket"
)

// Remote-protocol procedures (libvirt's remote_protocol.x) the fake libvirtd answers
// with a payload, or that tests look for among the calls it received.
const (
	procDomainCreate         = 9
	procDomainDestroy        = 12
	procDomainLookupByUUID   = 24
	procDomainReboot         = 27
	procAuthList             = 66
	procDomainIsActive       = 150
	procConnectGetLibVersion = 157
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
	active bool // what DomainIsActive reports; set before serving

	mu    sync.Mutex
	calls []uint32
}

func (f *fakeLibvirtd) serve(conn net.Conn) {
	defer conn.Close()
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
		return fakeDomain
	case procDomainIsActive:
		active := make([]byte, 4)
		if f.active {
			binary.BigEndian.PutUint32(active, 1)
		}
		return active
	}
	return nil
}

// fakeDomain is the XDR encoding of the one domain the fake knows: its name as an XDR
// string, padded to four bytes, then a 16-byte UUID and an id.
var fakeDomain = func() []byte {
	name := "instance-fake"
	b := binary.BigEndian.AppendUint32(nil, uint32(len(name)))
	b = append(b, name...)
	b = append(b, make([]byte, (4-len(name)%4)%4)...)
	b = append(b, make([]byte, 16)...)
	return binary.BigEndian.AppendUint32(b, 1)
}()

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
		case procDomainCreate, procDomainDestroy, procDomainLookupByUUID, procDomainReboot, procDomainIsActive:
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

func (d *pipeDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

// waitFor polls cond until it holds, failing the test after two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func newConnectedManager(t *testing.T, d *pipeDialer) *VMManager {
	t.Helper()
	m := &VMManager{mode: "real", stubVMs: make(map[string]*stubVM)}
	if err := m.connect(d, time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return m
}
