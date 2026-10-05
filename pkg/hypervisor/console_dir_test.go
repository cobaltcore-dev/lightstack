package hypervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// capsWith is a capabilities document carrying the given secmodels, shaped like the
// one libvirt reports (docs/formatcaps.rst): an AppArmor model next to the DAC one.
func capsWith(secmodels string) string {
	return "<capabilities><host><uuid>7b55704c-29f4-11b2-a85c-9dc6ff50623f</uuid>" +
		secmodels + "</host><guest><os_type>hvm</os_type></guest></capabilities>"
}

const apparmorModel = "<secmodel><model>apparmor</model><doi>0</doi></secmodel>"

func dacModel(labels string) string {
	return "<secmodel><model>dac</model><doi>0</doi>" + labels + "</secmodel>"
}

// QEMU's own uid:gid comes from the DAC security model's base label, which libvirt
// formats as "+uid:+gid". The kvm label is what a KVM domain runs as; the qemu label
// is the fallback when no kvm one is reported (SPEC-021 FR-017a).
func TestQEMUDACOwner(t *testing.T) {
	for _, tc := range []struct {
		name     string
		caps     string
		uid, gid int
		wantErr  bool
	}{
		{"libvirt's documented example", capsWith(apparmorModel + dacModel(
			"<baselabel type='kvm'>+77:+77</baselabel><baselabel type='qemu'>+77:+77</baselabel>")), 77, 77, false},
		{"kvm wins over qemu", capsWith(dacModel(
			"<baselabel type='qemu'>+1:+2</baselabel><baselabel type='kvm'>+64055:+108</baselabel>")), 64055, 108, false},
		{"qemu when no kvm label", capsWith(dacModel("<baselabel type='qemu'>+64055:+108</baselabel>")), 64055, 108, false},
		{"root", capsWith(dacModel("<baselabel type='kvm'>+0:+0</baselabel>")), 0, 0, false},
		{"no dac model", capsWith(apparmorModel), 0, 0, true},
		{"dac model without a label", capsWith(dacModel("")), 0, 0, true},
		{"not numeric", capsWith(dacModel("<baselabel type='kvm'>+libvirt-qemu:+kvm</baselabel>")), 0, 0, true},
		{"no separator", capsWith(dacModel("<baselabel type='kvm'>+64055</baselabel>")), 0, 0, true},
		{"not xml", "nope", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid, gid, err := qemuDACOwner(tc.caps)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("qemuDACOwner = %d:%d; want an error", uid, gid)
				}
				return
			}
			if err != nil {
				t.Fatalf("qemuDACOwner: %v", err)
			}
			if uid != tc.uid || gid != tc.gid {
				t.Errorf("owner = %d:%d; want %d:%d", uid, gid, tc.uid, tc.gid)
			}
		})
	}
}

// statOwnerMode reports a path's owner and permission bits.
func statOwnerMode(t *testing.T, path string) (uid, gid int, mode os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), fi.Mode().Perm()
}

// The socket directory is QEMU's and closed to everyone else (0750). A missing parent is
// created traversable (0755), because QEMU must still reach its disks under the storage
// root. An existing directory with a wider mode is corrected.
func TestEnsureOwnedDir(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid() // chown to oneself needs no privilege

	t.Run("missing, with a missing parent", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		dir := filepath.Join(root, "console-sockets")
		if err := ensureOwnedDir(dir, uid, gid); err != nil {
			t.Fatalf("ensureOwnedDir: %v", err)
		}
		if gotUID, gotGID, mode := statOwnerMode(t, dir); gotUID != uid || gotGID != gid || mode != 0o750 {
			t.Errorf("dir = %d:%d %o; want %d:%d 750", gotUID, gotGID, mode, uid, gid)
		}
		if _, _, mode := statOwnerMode(t, root); mode != 0o755 {
			t.Errorf("parent mode = %o; want 755 so QEMU can traverse it", mode)
		}
	})

	t.Run("existing and too wide", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "console-sockets")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := ensureOwnedDir(dir, uid, gid); err != nil {
			t.Fatalf("ensureOwnedDir: %v", err)
		}
		if _, _, mode := statOwnerMode(t, dir); mode != 0o750 {
			t.Errorf("mode = %o; want 750", mode)
		}
	})
}

// PrepareConsoleSocketDir reads QEMU's owner from libvirt and records the outcome:
// a prepared directory lets starts through, a failure keeps them refused and says why.
func TestPrepareConsoleSocketDir(t *testing.T) {
	label := fmt.Sprintf("<baselabel type='kvm'>+%d:+%d</baselabel>", os.Getuid(), os.Getgid())

	t.Run("prepared", func(t *testing.T) {
		m := newConnectedManagerWith(t, &fakeLibvirtd{caps: capsWith(apparmorModel + dacModel(label))})
		dir := filepath.Join(t.TempDir(), "console-sockets")
		if err := m.PrepareConsoleSocketDir(t.Context(), dir); err != nil {
			t.Fatalf("PrepareConsoleSocketDir: %v", err)
		}
		if err := m.consoleDirReady(); err != nil {
			t.Errorf("consoleDirReady after a successful prepare: %v", err)
		}
		if _, _, mode := statOwnerMode(t, dir); mode != 0o750 {
			t.Errorf("mode = %o; want 750", mode)
		}
	})

	t.Run("no owner reported", func(t *testing.T) {
		m := newConnectedManagerWith(t, &fakeLibvirtd{caps: capsWith(apparmorModel)})
		dir := filepath.Join(t.TempDir(), "console-sockets")
		if err := m.PrepareConsoleSocketDir(t.Context(), dir); err == nil {
			t.Fatal("PrepareConsoleSocketDir succeeded without a DAC label to read the owner from")
		}
		err := m.consoleDirReady()
		if !errors.Is(err, ErrConsoleSocketDirNotReady) {
			t.Fatalf("consoleDirReady = %v; want ErrConsoleSocketDirNotReady", err)
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error %q does not name the directory", err)
		}
	})
}

// Until the socket directory is QEMU's, every path that starts a guest is refused before
// libvirt is asked anything that would fail inside it: no domain is defined or started,
// and a hard reboot never powers a guest off that it then could not start (FR-017a).
func TestStartsRefusedUntilConsoleDirReady(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	for _, tc := range []struct {
		name  string
		start func(m *VMManager) error
	}{
		{"create", func(m *VMManager) error { _, err := m.CreateVM(t.Context(), "<domain/>"); return err }},
		{"start", func(m *VMManager) error { return m.StartVM(t.Context(), id) }},
		{"hard reboot", func(m *VMManager) error { return m.RebootVM(t.Context(), id, true) }},
		{"resume", func(m *VMManager) error { return m.ResumeVM(t.Context(), id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeLibvirtd{active: true}
			m := newConnectedManagerWith(t, f)
			m.consoleDir.Store(nil) // never prepared

			err := tc.start(m)
			if !errors.Is(err, ErrConsoleSocketDirNotReady) {
				t.Fatalf("err = %v; want ErrConsoleSocketDirNotReady", err)
			}
			for _, p := range f.domainCalls() {
				if p == procDomainCreate || p == procDomainDefineXML || p == procDomainDestroy {
					t.Errorf("libvirt was asked procedure %d before the refusal", p)
				}
			}
		})
	}
}
