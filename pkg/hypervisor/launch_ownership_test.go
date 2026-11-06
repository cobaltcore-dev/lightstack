package hypervisor

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// guestStart is one way a guest's QEMU process is launched.
type guestStart struct {
	name  string
	start func(m *VMManager) error
}

// guestStarts are the four of them, for the domain id. Each reaches DomainCreate in a function
// of its own; create also serves self-heal.
func guestStarts(id string) []guestStart {
	return []guestStart{
		{"create", func(m *VMManager) error { _, err := m.CreateVM(context.Background(), "<domain/>"); return err }},
		{"start", func(m *VMManager) error { return m.StartVM(context.Background(), id) }},
		{"hard reboot", func(m *VMManager) error { return m.RebootVM(context.Background(), id, true) }},
		{"resume", func(m *VMManager) error { return m.ResumeVM(context.Background(), id) }},
	}
}

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
	for _, tc := range guestStarts(id) {
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

// SPEC-031 FR-014, last sentence: a start whose instance directory cannot be made QEMU's does
// not launch the guest. A create undefines the domain it defined, and a hard reboot fails before
// it powers the guest off, so the failure never leaves a guest off and unable to start.
func TestGuestStartRefusedWhenInstanceDirCannotBePrepared(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	domainUUID, err := parseUUID(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range guestStarts(id) {
		t.Run(tc.name, func(t *testing.T) {
			useTempBase(t)
			// A regular file where the instances directory belongs: creating a directory under
			// it fails for every user, root included.
			if err := os.WriteFile(paths.InstancesDir(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			f := &fakeLibvirtd{uuid: domainUUID, stopped: true, active: true}
			m := newConnectedManagerWith(t, f)

			if err := tc.start(m); err == nil {
				t.Fatalf("%s succeeded with an instance directory that cannot be prepared", tc.name)
			}
			calls := f.domainCalls()
			if slices.Contains(calls, procDomainCreate) {
				t.Errorf("the guest was launched: calls %v", calls)
			}
			if slices.Contains(calls, procDomainDestroy) {
				t.Errorf("the guest was powered off before the refusal: calls %v", calls)
			}
			if tc.name == "create" && !slices.Contains(calls, procDomainUndefine) {
				t.Errorf("the defined domain was left behind: calls %v", calls)
			}
		})
	}
}

// A hard reboot that is refused — an id that is not a UUID, a guest libvirt does not have —
// leaves no instance directory behind: the directory is made QEMU's only for a guest that
// exists, and still before it is powered off (SPEC-031 FR-014).
func TestHardRebootRefusalCreatesNoInstanceDir(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		fake     *fakeLibvirtd
		want     error // nil: any error
	}{
		{"an id that is not a UUID", "not-a-uuid", &fakeLibvirtd{}, nil},
		{"a guest libvirt does not have", "11111111-2222-3333-4444-555555555555", &fakeLibvirtd{absent: true}, ErrDomainNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempBase(t)
			m := newConnectedManagerWith(t, tc.fake)

			err := m.RebootVM(context.Background(), tc.id, true)
			if err == nil {
				t.Fatal("the hard reboot succeeded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v; want %v", err, tc.want)
			}
			if _, statErr := os.Stat(InstanceDir(tc.id)); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the refused reboot left %s behind (stat: %v)", InstanceDir(tc.id), statErr)
			}
		})
	}
}
