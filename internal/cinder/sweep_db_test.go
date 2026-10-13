package cinder

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// At start, the volumes and snapshots this node was creating when it stopped go to error
// and their backing is reclaimed (FR-033, research R13). Another node's rows — a
// multi-node PostgreSQL deployment — and rows written before node names existed are
// left alone (SC-010). A reclaim that fails is logged; the sweep goes on.
func TestSweepInterruptedReservations(t *testing.T) {
	requireDB(t)
	project := cinderProject(t, nil)
	ctx := context.Background()
	node := "sweep-" + uuid.NewString()[:8]
	seed := func(status string, reservedBy any) string {
		id := uuid.NewString()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, reserved_by)
			 VALUES ($1, 'spec028-sweep', $2, $3, 1, $4, $5)`, id, project, seededUserID, status, reservedBy); err != nil {
			t.Fatalf("seed a volume: %v", err)
		}
		return id
	}
	own, ownUnreclaimable := seed("creating", node), seed("creating", node)
	otherNode, legacy, ownDone := seed("creating", node+"-other"), seed("creating", nil), seed("available", node)
	snapshot := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO snapshots (id, name, volume_id, project_id, size_gb, status, reserved_by)
		 VALUES ($1, 'spec028-sweep', $2, $3, 1, 'creating', $4)`, snapshot, ownDone, project, node); err != nil {
		t.Fatalf("seed a snapshot: %v", err)
	}

	fake := &fakeBacking{failDelete: map[string]bool{ownUnreclaimable: true}}
	svc, _ := backingRouter(t, fake, node)
	svc.SweepInterruptedReservations(ctx)

	for id, want := range map[string]string{
		own: "error", ownUnreclaimable: "error", otherNode: "creating", legacy: "creating", ownDone: "available",
	} {
		if status, _, _, _, _ := reservedVolume(t, id); status != want {
			t.Errorf("volume %s is %q after the sweep; want %q", id, status, want)
		}
	}
	var snapStatus string
	if err := testPool.QueryRow(ctx, `SELECT status FROM snapshots WHERE id = $1`, snapshot).Scan(&snapStatus); err != nil || snapStatus != "error" {
		t.Errorf("snapshot is %q (%v) after the sweep; want error", snapStatus, err)
	}
	deleted := fake.calls(&fake.deleted)
	if !slices.Contains(deleted, own) || !slices.Contains(deleted, ownUnreclaimable) {
		t.Errorf("backings reclaimed %v; want both of this node's volumes tried", deleted)
	}
	if slices.Contains(deleted, otherNode) || slices.Contains(deleted, legacy) {
		t.Errorf("backings reclaimed %v include rows this node does not own", deleted)
	}
	if !slices.Contains(fake.calls(&fake.deletedSnapshots), snapshot) {
		t.Errorf("the snapshot's backing was not reclaimed")
	}
}
