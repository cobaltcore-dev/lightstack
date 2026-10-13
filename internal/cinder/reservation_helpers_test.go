package cinder

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeBacking stands in for the block storage (volumeBacking). It records what was
// attempted, created and deleted, and can fail a create, hold it until released, or run
// a step while it is under way — what another request might do meanwhile.
type fakeBacking struct {
	mu               sync.Mutex
	attempted        []string // volume and snapshot ids a create was asked for
	created          []string
	deleted          []string
	deletedSnapshots []string
	fail             error           // a create fails with this
	during           func(id string) // runs while a create is under way
	hold             chan struct{}   // a create waits for it to close
}

func (f *fakeBacking) create(id string) error {
	f.mu.Lock()
	f.attempted = append(f.attempted, id)
	f.mu.Unlock()
	if f.during != nil {
		f.during(id)
	}
	if f.hold != nil {
		<-f.hold
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.created = append(f.created, id)
	return nil
}

func (f *fakeBacking) CreateVolume(_ context.Context, id string, _ int) error { return f.create(id) }
func (f *fakeBacking) CreateVolumeFromImage(_ context.Context, id, _ string, _ int) error {
	return f.create(id)
}
func (f *fakeBacking) CreateSnapshot(_ context.Context, _, snapshotID string) error {
	return f.create(snapshotID)
}

func (f *fakeBacking) DeleteVolume(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeBacking) DeleteSnapshot(_ context.Context, _, snapshotID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedSnapshots = append(f.deletedSnapshots, snapshotID)
	return nil
}

// calls returns a copy of one of the fake's records.
func (f *fakeBacking) calls(record *[]string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), *record...)
}

// backingRouter serves a service whose block storage is fake, reserving under node.
func backingRouter(t *testing.T, fake *fakeBacking, node string) (*Service, *gin.Engine) {
	t.Helper()
	svc := NewService(testPool, "stub", "", "", node)
	svc.cephClient = fake
	return svc, cinderRouter(svc)
}

// cinderProject creates a project of its own with the given block-storage limits (none:
// the code defaults) and removes it, with its volumes and snapshots, when the test ends.
func cinderProject(t *testing.T, limits map[string]int) string {
	t.Helper()
	ctx := context.Background()
	project := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, description, enabled, domain_id)
		 VALUES ($1, $2, '', true, (SELECT id FROM domains WHERE name = 'Default'))`,
		project, "cinder-"+project[:8]); err != nil {
		t.Fatalf("create a project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM images WHERE project_id = $1`, project)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, project)
	})
	for resource, limit := range limits {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO cinder_quotas (project_id, resource, "limit") VALUES ($1, $2, $3)`,
			project, resource, limit); err != nil {
			t.Fatalf("set the %s quota: %v", resource, err)
		}
	}
	return project
}

// reservedVolume reads what the reservation tests look at.
func reservedVolume(t *testing.T, id string) (status string, reservedBy *string, bootable bool, sourceImage *string, found bool) {
	t.Helper()
	err := testPool.QueryRow(context.Background(),
		`SELECT status, reserved_by, bootable, source_image_id::text FROM volumes WHERE id = $1`, id,
	).Scan(&status, &reservedBy, &bootable, &sourceImage)
	if err != nil {
		return "", nil, false, nil, false
	}
	return status, reservedBy, bootable, sourceImage, true
}

// registerOwner inserts a reservation owner with a chosen heartbeat, so a sweep test can
// stage a stopped owner (heartbeat past the stale window) or a live one (fresh). The row
// is removed when the test ends.
func registerOwner(t *testing.T, ownerID, node string, heartbeat time.Time) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO reservation_owners (owner_id, node_name, last_heartbeat) VALUES ($1, $2, $3)`,
		ownerID, node, heartbeat); err != nil {
		t.Fatalf("register owner %s: %v", ownerID, err)
	}
	t.Cleanup(func() { deleteOwner(ownerID) })
}

// ownerExists reports whether a reservation owner row survives.
func ownerExists(t *testing.T, ownerID string) bool {
	t.Helper()
	var exists bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM reservation_owners WHERE owner_id = $1)`, ownerID).Scan(&exists); err != nil {
		t.Fatalf("check owner %s: %v", ownerID, err)
	}
	return exists
}

// deleteOwner removes a reservation owner row, for test cleanup.
func deleteOwner(ownerID string) {
	_, _ = testPool.Exec(context.Background(), `DELETE FROM reservation_owners WHERE owner_id = $1`, ownerID)
}
