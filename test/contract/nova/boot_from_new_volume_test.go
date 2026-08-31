package nova_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
)

// SPEC-025 US9 (F16, absorbed from the closed #537): boot-from-new-volume was ADOPTED,
// not implemented.
//
// The dashboard's launch wizard defaults "Create New Volume = Yes" and sends the image
// as a source_type=image / destination_type=volume mapping rather than as imageRef. Lightstack
// booted the image directly and created no volume, so volume_size and
// delete_on_termination were accepted and discarded. A user who asked for persistent
// storage was told it worked and did not have it.

// volumeRow is the subset of a volume this test inspects.
type volumeRow struct {
	ID       string `json:"id"`
	Size     int    `json:"size"`
	Status   string `json:"status"`
	Bootable string `json:"bootable"`
}

// listVolumes returns the project's volumes, from the block-storage endpoint the service
// catalog advertises rather than an assumed localhost.
func listVolumes(t *testing.T, token string) []volumeRow {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, contract.SetupBlockStorageV3Client(t).ServiceURL("volumes", "detail"), nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Volumes []volumeRow `json:"volumes"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.Volumes
}

// volumeIDs returns the ids of the project's volumes.
func volumeIDs(t *testing.T, token string) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, v := range listVolumes(t, token) {
		ids[v.ID] = true
	}
	return ids
}

// bootVolumesSince returns the bootable volumes that did not exist when before was taken.
//
// A boot volume is unnamed, as upstream creates it, so it is recognised by appearing
// rather than by name. Counting only bootable volumes keeps an empty volume another test
// package creates in the same project meanwhile from being mistaken for it.
func bootVolumesSince(t *testing.T, token string, before map[string]bool) []volumeRow {
	t.Helper()
	var created []volumeRow
	for _, v := range listVolumes(t, token) {
		if !before[v.ID] && v.Bootable == "true" {
			created = append(created, v)
		}
	}
	return created
}

// bootFromNewVolume posts a wizard-shaped create and returns the status and body.
func bootFromNewVolume(t *testing.T, token, name, image string, size any, dot bool) (int, string) {
	t.Helper()
	sizeField := ""
	if size != nil {
		sizeField = fmt.Sprintf(`"volume_size": %v,`, size)
	}
	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","block_device_mapping_v2":[{
		"boot_index": 0, "uuid": %q, "source_type": "image", "destination_type": "volume",
		%s "delete_on_termination": %t}]}}`, name, image, sizeField, dot)
	return createServerRaw(t, token, body)
}

// T092/T093/T094/T096: what holds in EVERY storage mode.
//
// The happy path cannot be asserted at this level unconditionally. With
// cinder.storage_mode=local the volume's backing is created by copying the image's raw
// base, and a deployment without that base on this node legitimately refuses — the
// refusal IS the requirement (FR-055: never fall back to booting the image directly).
// With storage_mode=stub the same request succeeds.
//
// So this asserts the invariant common to both, and the size rule and creation path are
// covered by unit tests that inject a block-storage service (internal/nova/boot_volume_test.go).
func TestNovaBootFromNewVolume_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	image := firstImageID(t, token)

	const name = "spec025-bfv"
	before := volumeIDs(t, token)
	code, body := bootFromNewVolume(t, token, name, image, 3, true)

	var out struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out.Server.ID != "" {
		t.Cleanup(func() { deleteServer(t, token, out.Server.ID) })
	}

	switch code {
	case http.StatusAccepted, http.StatusCreated, http.StatusOK:
		// The volume must exist, be bootable, and be the requested size.
		created := bootVolumesSince(t, token, before)
		require.Len(t, created, 1,
			"the create succeeded but no bootable volume appeared — the image was booted directly (FR-049, FR-054)")
		assert.Equal(t, 3, created[0].Size, "the requested volume_size must be honoured (FR-050)")

	case http.StatusServiceUnavailable, http.StatusBadRequest:
		// Refused. That is correct when the backing cannot be provisioned — but it
		// must be a clean refusal: no server, and no orphaned volume.
		assert.Empty(t, out.Server.ID,
			"a refused boot-from-volume must not leave a server behind")
		assert.Empty(t, bootVolumesSince(t, token, before),
			"a refused boot-from-volume must not leave a volume behind")
		t.Logf("boot volume refused (%d) — correct for a deployment whose storage cannot provision it: %s", code, body)

	default:
		t.Fatalf("unexpected status %d for the wizard's default launch shape: %s", code, body)
	}
}

// T095 (FR-056): the two paths that already worked must be untouched.
func TestNovaExistingBootPathsUnchanged_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	image := firstImageID(t, token)

	t.Run("an explicit imageRef still boots the image directly", func(t *testing.T) {
		const name = "spec025-imageref"
		before := volumeIDs(t, token)
		id := createNamedServer(t, token, name)
		require.NotEmpty(t, id)

		// No volume may appear: this request did not ask for one.
		assert.Empty(t, bootVolumesSince(t, token, before),
			"an explicit imageRef must not create a volume (FR-056)")
	})

	t.Run("a mapping with destination_type=local is not turned into a volume", func(t *testing.T) {
		const name = "spec025-bdm-local"
		before := volumeIDs(t, token)
		body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","block_device_mapping_v2":[{
			"boot_index": 0, "uuid": %q, "source_type": "image", "destination_type": "local"}]}}`,
			name, image)
		code, resp := createServerRaw(t, token, body)
		require.Contains(t, []int{http.StatusAccepted, http.StatusCreated, http.StatusOK}, code, resp)

		var out struct {
			Server struct{ ID string } `json:"server"`
		}
		_ = json.Unmarshal([]byte(resp), &out)
		t.Cleanup(func() { deleteServer(t, token, out.Server.ID) })

		assert.Empty(t, bootVolumesSince(t, token, before),
			"destination_type=local must not create a volume")
	})
}
