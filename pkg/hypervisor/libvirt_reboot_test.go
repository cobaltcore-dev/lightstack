package hypervisor

import (
	"slices"
	"testing"
)

const rebootTestUUID = "11111111-2222-3333-4444-555555555555"

func rebootAgainst(t *testing.T, f *fakeLibvirtd, hard bool) []uint32 {
	t.Helper()
	useTempBase(t)
	m := newConnectedManager(t, &pipeDialer{fake: f})
	defer m.Close()
	if err := m.RebootVM(t.Context(), rebootTestUUID, hard); err != nil {
		t.Fatalf("RebootVM(hard=%v): %v", hard, err)
	}
	return f.domainCalls()
}

// A hard reboot power-cycles the domain, as upstream's _hard_reboot does by destroying
// and re-creating it: a new qemu process, which a hung guest cannot ignore. It used to
// send the same guest-cooperative DomainReboot as a soft one.
func TestRebootVMHardPowerCyclesAnActiveDomain(t *testing.T) {
	got := rebootAgainst(t, &fakeLibvirtd{active: true}, true)
	want := []uint32{procDomainLookupByUUID, procDomainIsActive, procDomainDestroy, procDomainCreate}
	if !slices.Equal(got, want) {
		t.Errorf("hard reboot sent %v; want %v: look up, check active, destroy, create", got, want)
	}
}

// A hard reboot of a domain that is not running starts it, as upstream allows a HARD
// reboot of a stopped server. There is nothing to destroy.
func TestRebootVMHardStartsAnInactiveDomain(t *testing.T) {
	got := rebootAgainst(t, &fakeLibvirtd{active: false}, true)
	want := []uint32{procDomainLookupByUUID, procDomainIsActive, procDomainCreate}
	if !slices.Equal(got, want) {
		t.Errorf("hard reboot sent %v; want %v: look up, check active, create", got, want)
	}
}

// A soft reboot asks the guest to restart itself, and does nothing more.
func TestRebootVMSoftAsksTheGuest(t *testing.T) {
	got := rebootAgainst(t, &fakeLibvirtd{active: true}, false)
	want := []uint32{procDomainLookupByUUID, procDomainReboot}
	if !slices.Equal(got, want) {
		t.Errorf("soft reboot sent %v; want %v", got, want)
	}
}
