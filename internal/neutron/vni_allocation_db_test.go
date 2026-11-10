package neutron

import (
	"context"
	"testing"
)

// TestAllocateVNITakesTheLowestFreeVNI pins allocateVNI on both backends: the lowest VNI
// of the range no network holds, gaps included, and an error once the range is full.
func TestAllocateVNITakesTheLowestFreeVNI(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	// A range at the top of the VNI space, which no other test allocates from.
	const start, end = 16777200, 16777204
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			"DELETE FROM network_vni_allocations WHERE vni BETWEEN $1 AND $2", start, end)
	})
	for _, vni := range []int{start, start + 1, start + 3} {
		if _, err := testPool.Exec(ctx,
			"INSERT INTO network_vni_allocations (network_id, vni) VALUES ($1, $2)",
			seedNetwork(t, "vni-held"), vni); err != nil {
			t.Fatalf("hold VNI %d: %v", vni, err)
		}
	}

	vc := NewVXLANCoordinator(testPool, nil, nil, 0, start, end)
	for _, want := range []int{start + 2, start + 4} {
		got, err := vc.allocateVNI(ctx, seedNetwork(t, "vni-new"))
		if err != nil {
			t.Fatalf("allocateVNI: %v; want %d", err, want)
		}
		if got != want {
			t.Errorf("allocateVNI = %d; want %d, the lowest free VNI", got, want)
		}
	}
	if got, err := vc.allocateVNI(ctx, seedNetwork(t, "vni-none-left")); err == nil {
		t.Errorf("allocateVNI on a full range = %d; want an error", got)
	}
}

// An inverted range (start > end) is empty: the anchor row is guarded, so allocateVNI
// errors rather than handing out `start` (SRC-21).
func TestAllocateVNIInvertedRangeIsEmpty(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	vc := NewVXLANCoordinator(testPool, nil, nil, 0, 16777204, 16777200) // start > end
	if got, err := vc.allocateVNI(ctx, seedNetwork(t, "vni-inverted")); err == nil {
		t.Errorf("allocateVNI on an inverted range = %d; want an error, not the start VNI", got)
	}
}
