package hypervisor

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// listenUnix opens a unix listener at path for the rest of the test. Nothing accepts: a
// unix connect completes once the connection is queued, which is all a dial needs.
func listenUnix(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

// The automatic socket choice — the modular QEMU daemon's socket when it exists, the
// monolithic or proxy socket otherwise — made afresh on every dial, so a socket that
// appears after Lightstack started is found on the next attempt.
func TestSocketDialer(t *testing.T) {
	dial := func(t *testing.T, d *socketDialer) error {
		t.Helper()
		c, err := d.Dial()
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	auto := LibvirtTarget{}

	t.Run("only virtqemud-sock", func(t *testing.T) {
		dir := socketDir(t)
		listenUnix(t, filepath.Join(dir, "virtqemud-sock"))
		d := newSocketDialer(auto, dir, time.Second)
		if err := dial(t, d); err != nil {
			t.Fatalf("Dial: %v", err)
		}
		if got, want := d.Dialed(), filepath.Join(dir, "virtqemud-sock"); got != want {
			t.Errorf("dialed %s; want %s", got, want)
		}
	})

	t.Run("only libvirt-sock", func(t *testing.T) {
		dir := socketDir(t)
		listenUnix(t, filepath.Join(dir, "libvirt-sock"))
		d := newSocketDialer(auto, dir, time.Second)
		if err := dial(t, d); err != nil {
			t.Fatalf("Dial: %v", err)
		}
		if got, want := d.Dialed(), filepath.Join(dir, "libvirt-sock"); got != want {
			t.Errorf("dialed %s; want %s", got, want)
		}
	})

	t.Run("both prefers virtqemud-sock", func(t *testing.T) {
		dir := socketDir(t)
		listenUnix(t, filepath.Join(dir, "virtqemud-sock"))
		listenUnix(t, filepath.Join(dir, "libvirt-sock"))
		d := newSocketDialer(auto, dir, time.Second)
		if err := dial(t, d); err != nil {
			t.Fatalf("Dial: %v", err)
		}
		if got, want := d.Dialed(), filepath.Join(dir, "virtqemud-sock"); got != want {
			t.Errorf("dialed %s; want %s", got, want)
		}
	})

	t.Run("neither names both", func(t *testing.T) {
		dir := socketDir(t)
		err := dial(t, newSocketDialer(auto, dir, time.Second))
		if err == nil {
			t.Fatal("Dial succeeded with no socket present")
		}
		for _, name := range []string{"virtqemud-sock", "libvirt-sock"} {
			if !strings.Contains(err.Error(), filepath.Join(dir, name)) {
				t.Errorf("error %q does not name %s", err, filepath.Join(dir, name))
			}
		}
	})

	t.Run("a socket that appears later is found on the next dial", func(t *testing.T) {
		dir := socketDir(t)
		d := newSocketDialer(auto, dir, time.Second)
		if err := dial(t, d); err == nil {
			t.Fatal("first Dial succeeded with no socket present")
		}
		listenUnix(t, filepath.Join(dir, "virtqemud-sock"))
		if err := dial(t, d); err != nil {
			t.Fatalf("second Dial: %v", err)
		}
		if got, want := d.Dialed(), filepath.Join(dir, "virtqemud-sock"); got != want {
			t.Errorf("dialed %s; want %s", got, want)
		}
	})

	t.Run("symlinks are followed", func(t *testing.T) {
		dir, elsewhere := socketDir(t), socketDir(t)
		// libvirt-sock linking to a live socket elsewhere — the host workaround — is used.
		listenUnix(t, filepath.Join(elsewhere, "real-sock"))
		if err := os.Symlink(filepath.Join(elsewhere, "real-sock"), filepath.Join(dir, "libvirt-sock")); err != nil {
			t.Fatal(err)
		}
		// A dangling virtqemud-sock link is not a socket that exists.
		if err := os.Symlink(filepath.Join(elsewhere, "gone"), filepath.Join(dir, "virtqemud-sock")); err != nil {
			t.Fatal(err)
		}
		d := newSocketDialer(auto, dir, time.Second)
		if err := dial(t, d); err != nil {
			t.Fatalf("Dial: %v", err)
		}
		if got, want := d.Dialed(), filepath.Join(dir, "libvirt-sock"); got != want {
			t.Errorf("dialed %s; want %s", got, want)
		}
	})

	t.Run("an explicit socket is dialed whatever else exists", func(t *testing.T) {
		dir, elsewhere := socketDir(t), socketDir(t)
		listenUnix(t, filepath.Join(dir, "virtqemud-sock"))
		listenUnix(t, filepath.Join(dir, "libvirt-sock"))
		custom := filepath.Join(elsewhere, "custom-sock")
		listenUnix(t, custom)
		d := newSocketDialer(LibvirtTarget{Socket: custom}, dir, time.Second)
		if err := dial(t, d); err != nil {
			t.Fatalf("Dial: %v", err)
		}
		if got := d.Dialed(); got != custom {
			t.Errorf("dialed %s; want %s", got, custom)
		}
	})

	t.Run("a missing explicit socket is named", func(t *testing.T) {
		custom := filepath.Join(socketDir(t), "custom-sock")
		err := dial(t, newSocketDialer(LibvirtTarget{Socket: custom}, socketDir(t), time.Second))
		if err == nil || !strings.Contains(err.Error(), custom) {
			t.Errorf("Dial error %v; want one naming %s", err, custom)
		}
	})
}

// delayedDialer connects to a fake libvirtd after delay, over a fresh pipe. A delay past the
// attempt's bound makes a connection that does not exist until the attempt has been given up on.
type delayedDialer struct {
	delay  time.Duration
	fake   *fakeLibvirtd
	served chan struct{} // closed once the fake stops serving the connection
}

func (d *delayedDialer) Dial() (net.Conn, error) {
	time.Sleep(d.delay)
	client, server := net.Pipe()
	go func() {
		d.fake.serve(server)
		close(d.served)
	}()
	return client, nil
}

func (d *delayedDialer) Dialed() string { return "delayed-sock" }

// An attempt that outlives its bound is given up, and its connection closed, not leaked: the
// caller has moved on, and nothing else would ever close it. That holds for a connection whose
// handshake libvirt never answers, and for one that opens only after the attempt was given up on,
// whether libvirt then answers its handshake or not.
func TestDialWithinClosesAnAbandonedConnection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		fake  *fakeLibvirtd
	}{
		{"opened in time, the handshake never answered", 0, &fakeLibvirtd{silentConns: 1}},
		{"opened too late, the handshake answered", 300 * time.Millisecond, &fakeLibvirtd{}},
		{"opened too late, the handshake never answered", 300 * time.Millisecond, &fakeLibvirtd{silentConns: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &delayedDialer{delay: tc.delay, fake: tc.fake, served: make(chan struct{})}
			if _, _, err := dialWithin(d, 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "delayed-sock") {
				t.Fatalf("dialWithin = %v; want a timeout naming the dialer's socket", err)
			}
			select {
			case <-d.served:
			case <-time.After(5 * time.Second):
				t.Fatal("the abandoned attempt's connection is still open")
			}
		})
	}
}

// closeSpy records whether the connection it wraps was closed.
type closeSpy struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeSpy) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// dyingDialer hands out a connection whose peer reads the request and vanishes, so the
// handshake fails on EOF — the shape of a socket-activated daemon that accepts and then dies
// before answering.
type dyingDialer struct{ spy *closeSpy }

func (d *dyingDialer) Dial() (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		buf := make([]byte, 4096)
		_, _ = server.Read(buf)
		_ = server.Close()
	}()
	d.spy.Conn = client
	return d.spy, nil
}

func (d *dyingDialer) Dialed() string { return "dying-sock" }

// A handshake that fails within the bound closes the connection the attempt opened. go-libvirt
// does not: by the time the failure surfaces its read loop has already recorded the drop, and
// socket.Disconnect returns without closing. Net connections are not closed by the garbage
// collector either, so a daemon that crash-looped leaked one descriptor per attempt, every
// second to five.
func TestDialWithinClosesAConnectionWhoseHandshakeFails(t *testing.T) {
	spy := &closeSpy{}
	if _, _, err := dialWithin(&dyingDialer{spy: spy}, time.Second); err == nil {
		t.Fatal("dialWithin succeeded against a peer that vanished mid-handshake")
	}
	if !spy.closed.Load() {
		t.Error("the connection a failed handshake opened was not closed")
	}
}

// spyingDialer wraps another dialer's connections, recording whether each was closed.
type spyingDialer struct {
	inner libvirtDialer

	mu    sync.Mutex
	spies []*closeSpy
}

func (d *spyingDialer) Dial() (net.Conn, error) {
	conn, err := d.inner.Dial()
	if err != nil {
		return nil, err
	}
	spy := &closeSpy{Conn: conn}
	d.mu.Lock()
	d.spies = append(d.spies, spy)
	d.mu.Unlock()
	return spy, nil
}

func (d *spyingDialer) Dialed() string { return d.inner.Dialed() }

// first is the connection the dialer handed out first.
func (d *spyingDialer) first() *closeSpy {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.spies[0]
}

// A connection that drops is replaced, and the socket behind it is closed. Nothing else closed
// it: go-libvirt stops where its read loop stopped, net connections are not closed by the
// garbage collector, and its Disconnect returns at once for a connection that already dropped.
// The leak was one descriptor per libvirtd restart — a package upgrade, a crash — which is the
// very event the reconnect loop exists to survive.
func TestDroppedConnectionSocketIsClosed(t *testing.T) {
	pd := &pipeDialer{}
	d := &spyingDialer{inner: pd}
	m := &VMManager{mode: "real", stubVMs: make(map[string]*stubVM)}
	if err := m.connect(d, time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer m.Close()

	pd.drop()
	waitFor(t, "a new dial after libvirtd went away", func() bool { return pd.dialCount() >= 2 })
	if !d.first().closed.Load() {
		t.Error("the socket behind a dropped connection was not closed")
	}
}
