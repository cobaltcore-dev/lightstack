package hypervisor

import (
	"bytes"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// syncBuffer is a bytes.Buffer safe for the logger's writes from other goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog sends the global logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Logger
	log.Logger = zerolog.New(buf)
	t.Cleanup(func() { log.Logger = prev })
	return buf
}

// useRunDir points the automatic socket choice at dir for the rest of the test.
func useRunDir(t *testing.T, dir string) {
	t.Helper()
	prev := libvirtRunDir
	libvirtRunDir = dir
	t.Cleanup(func() { libvirtRunDir = prev })
}

// With the default URI, Lightstack dials the modular QEMU daemon's socket where only the
// modular daemons run, libvirtd's where it runs, and names the socket it used.
func TestNewVMManagerSocketChoice(t *testing.T) {
	for _, sock := range []string{"virtqemud-sock", "libvirt-sock"} {
		t.Run("only "+sock, func(t *testing.T) {
			dir := socketDir(t)
			useRunDir(t, dir)
			logs := captureLog(t)
			serveFakeOnSocket(t, filepath.Join(dir, sock), &fakeLibvirtd{})

			m, err := NewVMManager("qemu:///system", "real")
			if err != nil {
				t.Fatalf("NewVMManager: %v", err)
			}
			t.Cleanup(m.Close)
			if _, err := m.conn.Load().ConnectGetLibVersion(); err != nil {
				t.Fatalf("libvirt does not answer: %v", err)
			}
			if out := logs.String(); !strings.Contains(out, "connected to libvirt") || !strings.Contains(out, filepath.Join(dir, sock)) {
				t.Errorf("log does not name the socket dialed (%s):\n%s", filepath.Join(dir, sock), out)
			}
		})
	}

	t.Run("neither", func(t *testing.T) {
		dir := socketDir(t)
		useRunDir(t, dir)
		_, err := NewVMManager("qemu:///system", "real")
		if err == nil {
			t.Fatal("NewVMManager succeeded with no socket present")
		}
		for _, sock := range []string{"virtqemud-sock", "libvirt-sock"} {
			if !strings.Contains(err.Error(), filepath.Join(dir, sock)) {
				t.Errorf("error %q does not name %s", err, filepath.Join(dir, sock))
			}
		}
	})
}

// A daemon that accepts the connection and never answers must not hold the attempt: it is
// bounded at 1 second, dial and handshake together (Constitution Article III).
func TestNewVMManagerBoundsTheAttempt(t *testing.T) {
	dir := socketDir(t)
	useRunDir(t, dir)
	sock := filepath.Join(dir, "virtqemud-sock")
	serveFakeOnSocket(t, sock, &fakeLibvirtd{silentConns: 1})

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		m, err := NewVMManager("qemu:///system", "real")
		if err == nil {
			m.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("NewVMManager succeeded against a daemon that never answers")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("gave up after %s; want about 1s", elapsed)
		}
		if !strings.Contains(err.Error(), sock) {
			t.Errorf("error %q does not name %s", err, sock)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NewVMManager still waiting after 5s on a daemon that never answers")
	}
}

// A socket named in libvirt_uri is the one dialed, even where both automatic sockets exist
// (SPEC-031 FR-008): the automatic choice is only for the default URI.
func TestExplicitSocketWins(t *testing.T) {
	dir := socketDir(t)
	useRunDir(t, dir)
	automatic := []*fakeLibvirtd{{}, {}}
	serveFakeOnSocket(t, filepath.Join(dir, "virtqemud-sock"), automatic[0])
	serveFakeOnSocket(t, filepath.Join(dir, "libvirt-sock"), automatic[1])
	custom := filepath.Join(dir, "custom-sock")
	explicit := &fakeLibvirtd{}
	serveFakeOnSocket(t, custom, explicit)

	conns := func(f *fakeLibvirtd) int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.conns
	}
	for _, uri := range []string{custom, "qemu+unix:///system?socket=" + custom} {
		m, err := NewVMManager(uri, "real")
		if err != nil {
			t.Fatalf("NewVMManager(%q): %v", uri, err)
		}
		m.Close()
	}
	if got := conns(explicit); got != 2 {
		t.Errorf("the named socket took %d connection(s); want one per URI, 2", got)
	}
	for i, f := range automatic {
		if got := conns(f); got != 0 {
			t.Errorf("automatic socket %d was dialed %d time(s) with a socket named", i, got)
		}
	}
}
