package cinder

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestCreateBootVolumeFromImageRecordsABootableVolume: the compute service's
// boot-from-new-volume path (SPEC-020 FR-025) gets back a volume it can boot from at
// once: available, bootable, sized as asked, owned by the caller, and carrying the image
// it was made from.
func TestCreateBootVolumeFromImageRecordsABootableVolume(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := NewService(testPool, "stub", "", "")
	imageID := uuid.New().String()

	id, err := svc.CreateBootVolumeFromImage(ctx, "vm-1-boot", seededProjectID, seededUserID, imageID, 3)
	if err != nil {
		t.Fatalf("CreateBootVolumeFromImage: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM volumes WHERE id = $1", id) })

	var (
		name, project, user, status, volumeType string
		size                                    int
		bootable                                bool
		source                                  *string
	)
	if err := testPool.QueryRow(ctx, `
		SELECT name, project_id::text, user_id::text, status, COALESCE(volume_type, ''), size_gb, bootable, source_image_id::text
		FROM volumes WHERE id = $1`, id).Scan(&name, &project, &user, &status, &volumeType, &size, &bootable, &source); err != nil {
		t.Fatalf("read back volume %s: %v", id, err)
	}
	if name != "vm-1-boot" || project != seededProjectID || user != seededUserID {
		t.Errorf("name/project/user = %q/%s/%s, want vm-1-boot/%s/%s", name, project, user, seededProjectID, seededUserID)
	}
	if status != "available" || !bootable || size != 3 {
		t.Errorf("status/bootable/size = %s/%v/%d, want available/true/3", status, bootable, size)
	}
	if volumeType != defaultVolumeType {
		t.Errorf("volume_type = %q, want %q", volumeType, defaultVolumeType)
	}
	if source == nil || *source != imageID {
		t.Errorf("source_image_id = %v, want %s", source, imageID)
	}
}

// TestVolumeFromImageFetchesTheImageBaseFirst: a local-backed volume made from an image
// is a copy of the image's raw base on this node. An RGW/S3-backed image's base is only
// there once something has fetched it, so the volume path fetches it first, verified
// against the image's checksum, as an image boot does (SPEC-014). A failed fetch refuses
// the volume; none is recorded. Backends that read no local base never fetch.
func TestVolumeFromImageFetchesTheImageBaseFirst(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	imageID := uuid.New().String()
	const checksum = "0123456789abcdef0123456789abcdef"
	if _, err := testPool.Exec(ctx,
		`INSERT INTO images (id, name, project_id, status, checksum) VALUES ($1, 'fetch-first', $2, 'active', $3)`,
		imageID, seededProjectID, checksum); err != nil {
		t.Fatalf("seed image: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volumes WHERE source_image_id = $1", imageID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM images WHERE id = $1", imageID)
	})

	for _, tt := range []struct {
		mode      string
		wantFetch bool
	}{
		{"local", true},
		{"local,rbd", true},
		{"stub", false},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			// The backing client stays a stub: in the local modes the failing fetch below
			// must stop the create before any backing store is touched.
			svc := NewService(testPool, "stub", "", "")
			svc.mode = tt.mode
			type fetch struct{ image, checksum string }
			var fetched []fetch
			svc.fetchImageBase = func(_ context.Context, id, sum string) (string, error) {
				fetched = append(fetched, fetch{id, sum})
				return "", errors.New("image backend unreachable")
			}

			id, err := svc.CreateBootVolumeFromImage(ctx, "fetch-first-"+tt.mode, seededProjectID, seededUserID, imageID, 1)
			if !tt.wantFetch {
				if err != nil {
					t.Fatalf("CreateBootVolumeFromImage in %s mode: %v; want no fetch, so no failure", tt.mode, err)
				}
				if len(fetched) != 0 {
					t.Errorf("%s mode fetched %v; it reads no local base", tt.mode, fetched)
				}
				return
			}

			if len(fetched) != 1 || fetched[0] != (fetch{imageID, checksum}) {
				t.Errorf("fetched %v, want one fetch of %s against checksum %s", fetched, imageID, checksum)
			}
			var unavailable *volumeBackingError
			if !errors.As(err, &unavailable) {
				t.Fatalf("CreateBootVolumeFromImage with a failing fetch: id=%q err=%v; want a backing error", id, err)
			}
			var n int
			if err := testPool.QueryRow(ctx, "SELECT COUNT(*) FROM volumes WHERE name = $1", "fetch-first-"+tt.mode).Scan(&n); err != nil {
				t.Fatalf("count volumes: %v", err)
			}
			if n != 0 {
				t.Errorf("%d volume(s) recorded after a failed fetch; want none", n)
			}
		})
	}
}
