package hypervisor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The server's hypervisor starts unconnected and dials in the background: startup never waits
// on libvirt, a socket that appears later is connected without a restart, and until then a
// call fails as it does during a lost connection — never with stub behaviour.
func TestStartVMManagerConnectsWhenSocketAppears(t *testing.T) {
	dir := socketDir(t)
	useRunDir(t, dir)
	useTempBase(t) // the loop prepares the console socket directory once connected
	logs := captureLog(t)

	start := time.Now()
	m, err := StartVMManager("qemu:///system", "real")
	if err != nil {
		t.Fatalf("StartVMManager with no libvirt socket: %v; want an unconnected manager", err)
	}
	t.Cleanup(m.Close)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("StartVMManager took %s; startup must not wait on libvirt", elapsed)
	}
	if err := m.DeleteVM(context.Background(), "00000000-0000-0000-0000-000000000001"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("DeleteVM while unconnected: %v; want a not-connected error", err)
	}
	// The socket appears only once an attempt has failed without it, so the connection below
	// is a later attempt's, not a first dial that happened to run after the socket appeared.
	waitForWithin(t, "the first attempt to fail", 5*time.Second, func() bool {
		return strings.Contains(logs.String(), "libvirt connection attempt failed")
	})

	serveFakeOnSocket(t, filepath.Join(dir, "virtqemud-sock"), &fakeLibvirtd{})
	waitForWithin(t, "the connection once the socket appears", 10*time.Second, func() bool {
		conn := m.conn.Load()
		if conn == nil {
			return false
		}
		_, err := conn.ConnectGetLibVersion()
		return err == nil
	})
}

// A daemon that accepts the connection and never answers holds one attempt for 1 second at
// most; the loop abandons it and the next attempt connects.
func TestStartVMManagerAbandonsSilentHandshake(t *testing.T) {
	dir := socketDir(t)
	useRunDir(t, dir)
	useTempBase(t) // the loop prepares the console socket directory once connected
	f := &fakeLibvirtd{silentConns: 1}
	serveFakeOnSocket(t, filepath.Join(dir, "virtqemud-sock"), f)

	m, err := StartVMManager("qemu:///system", "real")
	if err != nil {
		t.Fatalf("StartVMManager: %v", err)
	}
	t.Cleanup(m.Close)
	waitForWithin(t, "a connection after the silent one", 10*time.Second, func() bool {
		return m.conn.Load() != nil
	})
	f.mu.Lock()
	conns := f.conns
	f.mu.Unlock()
	if conns < 2 {
		t.Errorf("connected after %d connection(s); want the silent one abandoned and a second made", conns)
	}
}

// Close stops the background dialing: no connection is made afterwards, even once a socket
// appears.
func TestStartVMManagerCloseStopsDialing(t *testing.T) {
	dir := socketDir(t)
	useRunDir(t, dir)
	m, err := StartVMManager("qemu:///system", "real")
	if err != nil {
		t.Fatalf("StartVMManager: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	m.Close()
	m.Close() // idempotent, as for a connected manager

	select {
	case <-m.stopped:
	default:
		t.Fatal("the connection loop is still running after Close")
	}
	f := &fakeLibvirtd{}
	serveFakeOnSocket(t, filepath.Join(dir, "virtqemud-sock"), f)
	time.Sleep(1200 * time.Millisecond) // longer than the first backoff step
	f.mu.Lock()
	conns := f.conns
	f.mu.Unlock()
	if conns != 0 {
		t.Errorf("dialed %d time(s) after Close; want none", conns)
	}
}

// StartVMManager refuses a libvirt_uri Lightstack cannot honour, as configuration load does.
func TestStartVMManagerRefusesUnsupportedURI(t *testing.T) {
	if _, err := StartVMManager("qemu+tcp://h/system", "real"); err == nil {
		t.Fatal("StartVMManager accepted qemu+tcp://h/system")
	}
	// Stub mode reads no URI.
	m, err := StartVMManager("qemu+tcp://h/system", "stub")
	if err != nil {
		t.Fatalf("StartVMManager in stub mode: %v", err)
	}
	m.Close()
}

// Until a connection opens — and while a lost one is re-established — every libvirt call fails
// at once with the not-connected error, and nothing answers as stub mode would (SPEC-031
// FR-019). The manager here is already prepared, so the guest starts get past their refusal and
// meet the missing connection too.
func TestEveryCallFailsWithoutAConnection(t *testing.T) {
	m := &VMManager{mode: "real", stubVMs: make(map[string]*stubVM)}
	m.prepared.Store(true)
	ctx := context.Background()
	const id = "11111111-2222-3333-4444-555555555555"
	for name, call := range map[string]func() error{
		"create":      func() error { _, err := m.CreateVM(ctx, "<domain/>"); return err },
		"delete":      func() error { return m.DeleteVM(ctx, id) },
		"reboot":      func() error { return m.RebootVM(ctx, id, false) },
		"hard reboot": func() error { return m.RebootVM(ctx, id, true) },
		"stop":        func() error { return m.StopVM(ctx, id, 0) },
		"start":       func() error { return m.StartVM(ctx, id) },
		"state":       func() error { _, _, err := m.GetVMState(ctx, id); return err },
		"attach":      func() error { return m.AttachDevice(ctx, id, "<disk/>") },
		"detach":      func() error { return m.DetachDevice(ctx, id, "<disk/>") },
		"suspend":     func() error { return m.SuspendVM(ctx, id) },
		"resume":      func() error { return m.ResumeVM(ctx, id) },
	} {
		if err := call(); !errors.Is(err, errNotConnected) {
			t.Errorf("%s without a connection: %v; want the not-connected error", name, err)
		}
	}
}
