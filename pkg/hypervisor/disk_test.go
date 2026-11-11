package hypervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// The per-instance directory and its console.log path are referenced verbatim in
// the domain XML (<log file='.../console.log'>), so the layout is a contract — a
// drift here is exactly what broke boot-from-volume once (SPEC-009 WS-0/WS-5).
func TestInstancePaths(t *testing.T) {
	if got, want := InstanceDir("iid"), "/var/lib/lightstack/instances/iid"; got != want {
		t.Errorf("InstanceDir() = %q, want %q", got, want)
	}
	if got, want := ConsoleLogPath("iid"), "/var/lib/lightstack/instances/iid/console.log"; got != want {
		t.Errorf("ConsoleLogPath() = %q, want %q", got, want)
	}
}

// In real mode the per-instance directory must end up owned by the user libvirt runs
// QEMU as, so QEMU can create the guest's serial console.log there (the <log file>
// chardev it opens itself). QEMU's owner here is one a new directory does not get, so
// the test passes only if PrepareInstanceDir really chowns.
func TestPrepareInstanceDirOwnsDirInRealMode(t *testing.T) {
	old := paths.Base()
	t.Cleanup(func() { paths.SetBase(old) })
	paths.SetBase(t.TempDir())

	uid, gid := otherOwner(t)
	m := &VMManager{mode: "real"}
	m.qemuOwner.Store(&dacOwner{uid: uid, gid: gid})

	if err := m.PrepareInstanceDir("iid"); err != nil {
		t.Fatalf("PrepareInstanceDir: %v", err)
	}
	if gotUID, gotGID, _ := statOwnerMode(t, InstanceDir("iid")); gotUID != uid || gotGID != gid {
		t.Errorf("instance dir owner = %d:%d, want %d:%d", gotUID, gotGID, uid, gid)
	}
}

// The owner PrepareInstanceDir gives the per-instance directory is the one libvirt
// reports for QEMU in its capabilities, read once at startup by PrepareConsoleSocketDir.
// Without that hand-over the directory keeps this process's owner, and with an
// unprivileged QEMU every guest fails to start on its console.log.
func TestPrepareInstanceDirTakesOwnerFromCapabilities(t *testing.T) {
	old := paths.Base()
	t.Cleanup(func() { paths.SetBase(old) })
	paths.SetBase(t.TempDir())

	uid, gid := otherOwner(t)
	label := fmt.Sprintf("<baselabel type='kvm'>+%d:+%d</baselabel>", uid, gid)
	m := newConnectedManagerWith(t, &fakeLibvirtd{caps: capsWith(dacModel(label))})
	if err := m.PrepareConsoleSocketDir(t.Context(), paths.ConsoleSocketDir()); err != nil {
		t.Fatalf("PrepareConsoleSocketDir: %v", err)
	}

	if err := m.PrepareInstanceDir("iid"); err != nil {
		t.Fatalf("PrepareInstanceDir: %v", err)
	}
	if gotUID, gotGID, _ := statOwnerMode(t, InstanceDir("iid")); gotUID != uid || gotGID != gid {
		t.Errorf("instance dir owner = %d:%d, want %d:%d", gotUID, gotGID, uid, gid)
	}
}

// otherOwner returns an owner that a directory this process creates does not get, but
// that the process can still chown it to, so a test can see the chown happen. The
// candidates are nobody:nogroup, for root, and this user with each of its groups, since
// an unprivileged owner may give a file to any group it belongs to. Each is tried on a
// scratch file first: root in a user namespace may have no one else mapped. Skips when
// none works.
func otherOwner(t *testing.T) (uid, gid int) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "chown-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatalf("creating %s: %v", probe, err)
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("getgroups: %v", err)
	}
	self := dacOwner{uid: os.Geteuid(), gid: os.Getegid()}
	candidates := []dacOwner{{uid: 65534, gid: 65534}}
	for _, g := range groups {
		candidates = append(candidates, dacOwner{uid: self.uid, gid: g})
	}
	for _, c := range candidates {
		if c != self && os.Chown(probe, c.uid, c.gid) == nil {
			return c.uid, c.gid
		}
	}
	t.Skip("seeing a chown needs an owner this process may give a file to: root, or a second group")
	return 0, 0
}

// Stub mode (and real mode before libvirt capabilities have been read) just creates the
// directory, with no chown — there is no guest and no QEMU user to own it.
func TestPrepareInstanceDirStubModeJustCreates(t *testing.T) {
	old := paths.Base()
	t.Cleanup(func() { paths.SetBase(old) })
	paths.SetBase(t.TempDir())

	m := &VMManager{mode: "stub"}
	if err := m.PrepareInstanceDir("iid"); err != nil {
		t.Fatalf("PrepareInstanceDir (stub): %v", err)
	}
	if _, err := os.Stat(InstanceDir("iid")); err != nil {
		t.Fatalf("instance dir not created in stub mode: %v", err)
	}
}
