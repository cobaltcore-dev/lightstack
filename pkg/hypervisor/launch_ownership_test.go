package hypervisor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// SPEC-031 FR-014, FR-018: immediately before every guest start the instance's directory is
// made QEMU's, so QEMU can write the guest's console.log. Before, only the create paths did
// it, and only when QEMU's user was already known: self-heal skipped it, and a directory made
// while the user was unknown, or by an earlier release, stayed root-owned.
func TestGuestStartMakesTheInstanceDirQEMUs(t *testing.T) {
	uid, gid := otherOwner(t)
	useTempBase(t)

	const id = "11111111-2222-3333-4444-555555555555"
	domainUUID, err := parseUUID(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		start func(m *VMManager) error
	}{
		{"create", func(m *VMManager) error { _, err := m.CreateVM(context.Background(), "<domain/>"); return err }},
		{"start", func(m *VMManager) error { return m.StartVM(context.Background(), id) }},
		{"hard reboot", func(m *VMManager) error { return m.RebootVM(context.Background(), id, true) }},
		{"resume", func(m *VMManager) error { return m.ResumeVM(context.Background(), id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := InstanceDir(id)
			_ = os.RemoveAll(dir)
			if err := os.MkdirAll(dir, 0o755); err != nil { // the test's own owner: not QEMU's
				t.Fatal(err)
			}
			f := &fakeLibvirtd{caps: dacFor(uid, gid), uuid: domainUUID, stopped: true}
			m := newConnectedManagerWith(t, f)
			// Unprepared: the start's single on-demand attempt prepares it first (FR-013).
			m.prepared.Store(false)
			consoleDir := filepath.Join(t.TempDir(), "console-sockets")
			m.consoleDirPath.Store(&consoleDir)

			if err := tc.start(m); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if gotUID, gotGID, _ := statOwnerMode(t, dir); gotUID != uid || gotGID != gid {
				t.Errorf("instance directory owner %d:%d; want QEMU's %d:%d", gotUID, gotGID, uid, gid)
			}
			if !slices.Contains(f.domainCalls(), procDomainCreate) {
				t.Errorf("the guest was never started: calls %v", f.domainCalls())
			}
		})
	}
}
