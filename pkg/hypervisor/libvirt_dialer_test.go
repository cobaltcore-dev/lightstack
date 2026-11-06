package hypervisor

import (
	"net"
	"os"
	"path/filepath"
	"strings"
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
