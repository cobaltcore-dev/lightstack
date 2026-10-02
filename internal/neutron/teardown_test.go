package neutron

import (
	"context"
	"testing"
)

// In stub mode the network purge must be a no-op (no DB access, no host calls) —
// it returns early before any enumeration. Real-mode behavior is host-validated on
// a cluster (SPEC-010 / cloud-storage#472).
func TestPurgeRealModeNetworkState_StubIsNoOp(t *testing.T) {
	svc := NewService(testPool, "stub", nil)
	if err := svc.PurgeRealModeNetworkState(context.Background()); err != nil {
		t.Fatalf("stub-mode network purge should be a no-op, got: %v", err)
	}
}
