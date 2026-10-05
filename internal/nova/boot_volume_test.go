package nova

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Boot-from-new-volume at the unit level (SPEC-020 FR-025).
//
// A root block_device_mapping_v2 entry with source_type=image and destination_type=volume —
// the dashboard wizard's default — must create a volume from the image and boot from it.
// Booting the image directly would accept volume_size and delete_on_termination and discard
// them. These tests inject a block-storage service, so the size rule and the creation path
// are exercised wherever they run.

// fakeCinder records what compute asked for.
type fakeCinder struct {
	t        *testing.T
	calls    []fakeCinderCall
	failWith error
}

type fakeCinderCall struct {
	name, projectID, imageID string
	sizeGB                   int
	volumeID                 string // set once the volume exists
}

func (f *fakeCinder) CreateBootVolumeFromImage(_ context.Context, name, projectID, userID, imageID string, sizeGB int) (string, error) {
	f.calls = append(f.calls, fakeCinderCall{name: name, projectID: projectID, imageID: imageID, sizeGB: sizeGB})
	if f.failWith != nil {
		return "", f.failWith
	}
	id := uuid.New().String()
	// Record a real, available, bootable volume so the downstream boot-from-volume path
	// behaves as it would against the real service.
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, bootable, source_image_id, volume_type, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, 'available', true, $6, '__DEFAULT__', now(), now())`,
		id, name, projectID, userID, sizeGB, imageID); err != nil {
		return "", err
	}
	f.calls[len(f.calls)-1].volumeID = id
	if f.t != nil {
		f.t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM volume_attachments WHERE volume_id = $1`, id)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, id)
		})
	}
	return id, nil
}

// newBootVolumeService builds a compute service with a fake block-storage service.
func newBootVolumeService(t *testing.T, cinder CinderService) (*Service, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mgr, err := hypervisor.NewVMManager("", "stub")
	if err != nil {
		t.Fatalf("building stub VM manager: %v", err)
	}
	svc := NewService(testPool, "", "real", nil)
	svc.vmManager = mgr
	if cinder != nil {
		svc.SetCinderService(cinder)
	}
	t.Cleanup(svc.Shutdown)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("project_id", c.GetHeader("X-Test-Project"))
		c.Set("user_id", c.GetHeader("X-Test-User"))
		c.Set("roles", strings.Split(c.GetHeader("X-Test-Roles"), ","))
	})
	svc.RegisterRoutes(&r.RouterGroup)
	return svc, r
}

// bootHdr is a plain project member of the seeded project.
func bootHdr() map[string]string {
	return map[string]string{
		"X-Test-Project": seededProjectID,
		"X-Test-User":    seededUserID,
		"X-Test-Roles":   "member",
	}
}

// cleanupServerNamed removes, at test end, every instance row the test created under name.
func cleanupServerNamed(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM instances WHERE name = $1 AND project_id = $2`, name, seededProjectID)
	})
}

// countServersNamed reports how many instance rows exist for a name.
func countServersNamed(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM instances WHERE name = $1 AND project_id = $2`, name, seededProjectID,
	).Scan(&n); err != nil {
		t.Fatalf("counting servers: %v", err)
	}
	return n
}

// seededImageID returns an active image, seeding one when the database has none.
func seededImageID(t *testing.T) string {
	t.Helper()
	var id string
	err := testPool.QueryRow(t.Context(),
		`SELECT id::text FROM images WHERE status = 'active' LIMIT 1`).Scan(&id)
	if err == nil {
		return id
	}
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO images (name, project_id, status, visibility, disk_format, container_format)
		 VALUES ('fr025-img', $1, 'active', 'public', 'qcow2', 'bare') RETURNING id::text`,
		seededProjectID).Scan(&id); err != nil {
		t.Fatalf("seeding an image: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM images WHERE id = $1`, id)
	})
	return id
}

// wizardBody is the shape the dashboard's default launch sends.
func wizardBody(name, image string, size any, dot bool) string {
	sizeField := ""
	if size != nil {
		sizeField = fmt.Sprintf(`"volume_size": %v,`, size)
	}
	return fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","block_device_mapping_v2":[{
		"boot_index": 0, "uuid": %q, "source_type": "image", "destination_type": "volume",
		%s "delete_on_termination": %t}]}}`, name, image, sizeField, dot)
}

// The volume is created, at the requested size, from the requested image, for the caller.
func TestBootFromNewVolumeCreatesTheVolume(t *testing.T) {
	requireDB(t)
	fake := &fakeCinder{t: t}
	_, r := newBootVolumeService(t, fake)

	const name = "fr025-bfv"
	cleanupServerNamed(t, name)
	image := seededImageID(t)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers", wizardBody(name, image, 9, true), bootHdr())
	if code != http.StatusAccepted && code != http.StatusCreated {
		t.Fatalf("create = %d; want accepted. body: %s", code, body)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("%d volume creations; want exactly 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.sizeGB != 9 {
		t.Errorf("volume created at %d GiB; the request asked for 9", got.sizeGB)
	}
	if got.imageID != image {
		t.Errorf("volume created from image %q; want %q", got.imageID, image)
	}
	if got.projectID != seededProjectID {
		t.Errorf("volume owned by %q; want the requesting project %q", got.projectID, seededProjectID)
	}
	// Upstream creates an image-backed boot volume with an empty name
	// (nova/virt/block_device.py, _create_volume); the dashboard lists it by id.
	if got.name != "" {
		t.Errorf("volume named %q; upstream leaves it unnamed", got.name)
	}
}

// An omitted size uses the flavor's disk, and is never zero.
func TestBootVolumeSizeDefaultsToTheFlavorDisk(t *testing.T) {
	requireDB(t)
	fake := &fakeCinder{t: t}
	_, r := newBootVolumeService(t, fake)

	const name = "fr025-bfv-nosize"
	cleanupServerNamed(t, name)

	var flavorDisk int
	if err := testPool.QueryRow(t.Context(),
		`SELECT disk_gb FROM flavors WHERE name = 'm1.small'`).Scan(&flavorDisk); err != nil {
		t.Fatalf("reading the flavor: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers", wizardBody(name, seededImageID(t), nil, true), bootHdr())
	if code != http.StatusAccepted && code != http.StatusCreated {
		t.Fatalf("create = %d. body: %s", code, body)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("%d volume creations; want 1", len(fake.calls))
	}
	if got := fake.calls[0].sizeGB; got <= 0 {
		t.Errorf("defaulted size = %d; must never be zero", got)
	} else if got < flavorDisk {
		t.Errorf("defaulted size = %d; the flavor's disk is %d", got, flavorDisk)
	}
}

// A size the image does not fit in is refused before any volume exists. The floor is
// max(min_disk, actual size), not min_disk alone.
func TestBootVolumeTooSmallIsRefused(t *testing.T) {
	requireDB(t)
	fake := &fakeCinder{t: t}
	_, r := newBootVolumeService(t, fake)

	// An image that states no minimum but is genuinely large. min_disk_gb alone would
	// accept a 1 GiB volume here.
	var imageID string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO images (name, project_id, status, visibility, disk_format, container_format, min_disk_gb, size_bytes)
		 VALUES ('fr025-big', $1, 'active', 'public', 'qcow2', 'bare', 0, $2) RETURNING id::text`,
		seededProjectID, 6*(1<<30), // 6 GiB of content, min_disk 0
	).Scan(&imageID); err != nil {
		t.Fatalf("seeding a large image: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM images WHERE id = $1`, imageID)
	})

	const name = "fr025-bfv-small"
	cleanupServerNamed(t, name)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers", wizardBody(name, imageID, 1, true), bootHdr())
	if code != http.StatusBadRequest {
		t.Errorf("create with a 1 GiB volume for a 6 GiB image = %d; want 400. body: %s", code, body)
	}
	if len(fake.calls) != 0 {
		t.Errorf("%d volumes created for a refused request; want 0", len(fake.calls))
	}
	if n := countServersNamed(t, name); n != 0 {
		t.Errorf("%d instance rows for a refused request; want 0", n)
	}
}

// An image that does not exist is refused before any volume exists, with upstream's
// answer, and nothing from the database lookup reaches the response.
func TestBootVolumeFromAnUnknownImageIsRefused(t *testing.T) {
	requireDB(t)
	fake := &fakeCinder{t: t}
	_, r := newBootVolumeService(t, fake)

	for _, image := range []string{uuid.New().String(), "not-a-uuid"} {
		t.Run(image, func(t *testing.T) {
			const name = "fr025-bfv-noimage"
			cleanupServerNamed(t, name)

			code, body := doReq(r, http.MethodPost, "/v2.1/servers", wizardBody(name, image, 4, true), bootHdr())
			if code != http.StatusBadRequest {
				t.Errorf("create from image %q = %d; want 400. body: %s", image, code, body)
			}
			if !strings.Contains(body, "Can not find requested image") {
				t.Errorf("body %s; want upstream's \"Can not find requested image\"", body)
			}
			for _, leak := range []string{"SQLSTATE", "no rows", "uuid"} {
				if strings.Contains(body, leak) {
					t.Errorf("body %s carries database detail %q", body, leak)
				}
			}
			if len(fake.calls) != 0 {
				t.Errorf("%d volumes created for a refused request; want 0", len(fake.calls))
			}
			if n := countServersNamed(t, name); n != 0 {
				t.Errorf("%d instance rows for a refused request; want 0", n)
			}
		})
	}
}

// A boot volume made for a request that then fails goes with the request: nothing is left
// behind. The instance row cannot be written for a name longer than its column, and the
// length is not checked up front, so the request fails after the volume exists.
func TestBootVolumeIsRemovedWhenTheServerCannotBeRecorded(t *testing.T) {
	requireDB(t)
	fake := &fakeCinder{t: t}
	_, r := newBootVolumeService(t, fake)

	name := strings.Repeat("v", 300)
	code, body := doReq(r, http.MethodPost, "/v2.1/servers", wizardBody(name, seededImageID(t), 4, true), bootHdr())
	if code != http.StatusInternalServerError {
		t.Fatalf("create with a %d-character name = %d; this test needs the instance insert to fail. body: %s", len(name), code, body)
	}
	if len(fake.calls) != 1 || fake.calls[0].volumeID == "" {
		t.Fatalf("volume creations = %+v; want one, made before the instance row", fake.calls)
	}
	var left int
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM volumes WHERE id = $1`, fake.calls[0].volumeID).Scan(&left); err != nil {
		t.Fatalf("counting volumes: %v", err)
	}
	if left != 0 {
		t.Errorf("the boot volume outlived the failed create; want it removed")
	}
}

// A volume that cannot be created fails the boot visibly. It never falls back to booting
// the image directly, which would be the silent downgrade this path exists to remove.
func TestBootVolumeFailureNeverFallsBackToAnImageBoot(t *testing.T) {
	requireDB(t)
	fake := &fakeCinder{t: t, failWith: errors.New("backing store unavailable")}
	_, r := newBootVolumeService(t, fake)

	const name = "fr025-bfv-fail"
	cleanupServerNamed(t, name)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers", wizardBody(name, seededImageID(t), 4, true), bootHdr())
	if code == http.StatusAccepted || code == http.StatusCreated {
		t.Errorf("the create was accepted despite the volume failing (%d) — a silent downgrade. body: %s", code, body)
	}
	if n := countServersNamed(t, name); n != 0 {
		t.Errorf("%d instance rows after a failed volume creation; want 0", n)
	}
}

// The paths that already worked are untouched.
func TestExistingBootPathsDoNotCreateVolumes(t *testing.T) {
	requireDB(t)
	image := seededImageID(t)

	t.Run("explicit imageRef", func(t *testing.T) {
		fake := &fakeCinder{t: t}
		_, r := newBootVolumeService(t, fake)
		const name = "fr025-imageref"
		cleanupServerNamed(t, name)

		body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q}}`, name, image)
		if code, resp := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr()); code != http.StatusAccepted && code != http.StatusCreated {
			t.Fatalf("create = %d. body: %s", code, resp)
		}
		if len(fake.calls) != 0 {
			t.Errorf("an explicit imageRef created %d volumes; want 0", len(fake.calls))
		}
	})

	t.Run("destination_type=local", func(t *testing.T) {
		fake := &fakeCinder{t: t}
		_, r := newBootVolumeService(t, fake)
		const name = "fr025-local"
		cleanupServerNamed(t, name)

		body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","block_device_mapping_v2":[{
			"boot_index": 0, "uuid": %q, "source_type": "image", "destination_type": "local"}]}}`, name, image)
		if code, resp := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr()); code != http.StatusAccepted && code != http.StatusCreated {
			t.Fatalf("create = %d. body: %s", code, resp)
		}
		if len(fake.calls) != 0 {
			t.Errorf("destination_type=local created %d volumes; want 0", len(fake.calls))
		}
	})
}

// Without a block-storage service the request is refused, not silently downgraded.
func TestBootVolumeWithoutBlockStorageIsRefused(t *testing.T) {
	requireDB(t)
	_, r := newBootVolumeService(t, nil)

	const name = "fr025-nocinder"
	cleanupServerNamed(t, name)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers", wizardBody(name, seededImageID(t), 4, true), bootHdr())
	if code != http.StatusBadRequest {
		t.Errorf("create = %d with no block-storage service; want 400 rather than a silent image boot. body: %s", code, body)
	}
}

// volume_size is read as upstream's request schema accepts it (parameter_types.volume_size):
// an integer from 1 to 2^31-1, as a number (gin yields json.Number) or a string of digits.
// Anything else is refused rather than replaced by the default.
func TestBDMVolumeSizeIsReadAsUpstreamAcceptsIt(t *testing.T) {
	for _, tc := range []struct {
		raw    any
		want   int
		wantOK bool
	}{
		{nil, 0, true}, // absent: the default applies
		{json.Number("25"), 25, true},
		{25, 25, true},
		{float64(25), 25, true},
		{"25", 25, true},
		{json.Number("0"), 0, false},
		{json.Number("-1"), 0, false},
		{json.Number("2.5"), 0, false},
		{json.Number("2147483648"), 0, false},
		{"0", 0, false},
		{" 25 ", 0, false},
		{"", 0, false},
		{"not-a-number", 0, false},
		{true, 0, false},
	} {
		if got, ok := bdmVolumeSize(tc.raw); got != tc.want || ok != tc.wantOK {
			t.Errorf("bdmVolumeSize(%#v) = (%d, %v); want (%d, %v)", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

// A volume_size upstream's request schema refuses is refused here too, rather than
// replaced by the flavor's disk: the caller would get a volume of a size they never asked
// for.
func TestMalformedBootVolumeSizeIsRefused(t *testing.T) {
	requireDB(t)
	fake := &fakeCinder{t: t}
	_, r := newBootVolumeService(t, fake)

	const name = "fr025-bfv-badsize"
	cleanupServerNamed(t, name)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers", wizardBody(name, seededImageID(t), `"abc"`, true), bootHdr())
	if code != http.StatusBadRequest {
		t.Errorf("create with volume_size \"abc\" = %d; want 400. body: %s", code, body)
	}
	if len(fake.calls) != 0 {
		t.Errorf("%d volumes created for a refused request; want 0", len(fake.calls))
	}
	if n := countServersNamed(t, name); n != 0 {
		t.Errorf("%d instance rows for a refused request; want 0", n)
	}
}

// delete_on_termination, both directions.
//
// Exercises DeleteServer directly, with the root attachment seeded rather than produced by
// a boot: the boot path creates a directory under the root-owned /var/lib/lightstack, a
// real filesystem operation not worth faking. What matters is whether DeleteServer honours
// the flag on a boot volume.
func TestBootVolumeDeleteOnTermination(t *testing.T) {
	requireDB(t)

	for _, tc := range []struct {
		label       string
		dot         bool
		wantPresent bool
	}{
		{"true deletes the volume with the server", true, false},
		{"false keeps the volume and releases it", false, true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			_, r := newBootVolumeService(t, &fakeCinder{t: t})

			name := fmt.Sprintf("fr025-dot-%t", tc.dot)

			// A server, a bootable volume, and the root attachment the
			// boot-from-new-volume path records.
			var instanceID string
			if err := testPool.QueryRow(t.Context(),
				`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state)
				 VALUES ($1, $2, $3, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1)
				 RETURNING id::text`, name, seededProjectID, seededUserID).Scan(&instanceID); err != nil {
				t.Fatalf("seeding the instance: %v", err)
			}
			t.Cleanup(func() {
				_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, instanceID)
			})

			volumeID := uuid.New().String()
			if _, err := testPool.Exec(t.Context(),
				`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status, bootable, attached_to_instance_id, volume_type, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, 2, 'in-use', true, $5, '__DEFAULT__', now(), now())`,
				volumeID, name+"-boot", seededProjectID, seededUserID, instanceID); err != nil {
				t.Fatalf("seeding the volume: %v", err)
			}
			t.Cleanup(func() {
				_, _ = testPool.Exec(context.Background(), `DELETE FROM volumes WHERE id = $1`, volumeID)
			})

			if _, err := testPool.Exec(t.Context(),
				`INSERT INTO volume_attachments (id, volume_id, instance_id, device, delete_on_termination, attached_at)
				 VALUES ($1, $2, $3, '/dev/vda', $4, now())`,
				uuid.New().String(), volumeID, instanceID, tc.dot); err != nil {
				t.Fatalf("seeding the root attachment: %v", err)
			}

			if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+instanceID, "", bootHdr()); code != http.StatusNoContent && code != http.StatusAccepted {
				t.Fatalf("delete = %d. body: %s", code, body)
			}

			var remaining int
			if err := testPool.QueryRow(t.Context(),
				`SELECT count(*) FROM volumes WHERE id = $1`, volumeID).Scan(&remaining); err != nil {
				t.Fatalf("counting the volume: %v", err)
			}
			if present := remaining == 1; present != tc.wantPresent {
				t.Errorf("delete_on_termination=%t: volume present after delete = %t, want %t",
					tc.dot, present, tc.wantPresent)
			}

			if tc.wantPresent {
				// A retained volume must be reusable, not left in-use pointing at a
				// server that no longer exists.
				var status string
				var attachedTo *string
				if err := testPool.QueryRow(t.Context(),
					`SELECT status, attached_to_instance_id::text FROM volumes WHERE id = $1`,
					volumeID).Scan(&status, &attachedTo); err != nil {
					t.Fatalf("reading the retained volume: %v", err)
				}
				if status != "available" {
					t.Errorf("retained volume status = %q; want available so it can be reused", status)
				}
				if attachedTo != nil {
					t.Errorf("retained volume still attached to %q; the server is gone", *attachedTo)
				}
			}

			// Either way, no attachment row may survive the server.
			var attachments int
			if err := testPool.QueryRow(t.Context(),
				`SELECT count(*) FROM volume_attachments WHERE instance_id = $1`, instanceID).Scan(&attachments); err != nil {
				t.Fatalf("counting attachments: %v", err)
			}
			if attachments != 0 {
				t.Errorf("%d attachment rows survived the server", attachments)
			}
		})
	}
}
