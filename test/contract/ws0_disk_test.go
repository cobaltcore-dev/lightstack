package contract_test

import (
	"bytes"
	"net/http"
	"os"
	"testing"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-009 WS-0 contract tests (API-observable). Real-VM-boot, overlay, and
// console assertions live in the host scripts test/ws0_*_test.sh (need libvirt).

// flavorM1Small is the seeded m1.small flavor UUID (see nova contract tests).
const flavorM1Small = "00000000-0000-0000-0000-000000000010"

func ws0DeleteImageHTTP(t *testing.T, client *gophercloud.ServiceClient, imageID string) int {
	t.Helper()
	req, err := http.NewRequest("DELETE", client.ServiceURL("images", imageID), nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestWS0_ImageInUseGuard (FR-004): an image referenced by an instance cannot
// be deleted (409); once the instance is gone, deletion succeeds.
func TestWS0_ImageInUseGuard(t *testing.T) {
	SkipIfLightstackNotRunning(t)
	imgClient := SetupImageV2Client(t)
	novaClient := SetupComputeV2Client(t)

	img, err := images.Create(imgClient, images.CreateOpts{
		Name:            "ws0-inuse-img",
		ContainerFormat: "bare",
		DiskFormat:      "raw",
	}).Extract()
	require.NoError(t, err)
	imageDeleted := false
	defer func() {
		if !imageDeleted {
			_ = images.Delete(imgClient, img.ID).ExtractErr()
		}
	}()
	UploadTestImageData(t, imgClient, img.ID) // server create refuses an image that is not active

	srv, err := servers.Create(novaClient, servers.CreateOpts{
		Name:      "ws0-inuse-vm",
		FlavorRef: flavorM1Small,
		ImageRef:  img.ID,
	}).Extract()
	require.NoError(t, err)
	serverDeleted := false
	defer func() {
		if !serverDeleted {
			_ = servers.Delete(novaClient, srv.ID).ExtractErr()
		}
	}()

	// In-use image deletion must be rejected with 409.
	assert.Equal(t, http.StatusConflict, ws0DeleteImageHTTP(t, imgClient, img.ID),
		"deleting an in-use image must return 409 Conflict")

	// After the server is gone, the image must delete cleanly.
	require.NoError(t, servers.Delete(novaClient, srv.ID).ExtractErr())
	serverDeleted = true
	assert.Equal(t, http.StatusNoContent, ws0DeleteImageHTTP(t, imgClient, img.ID),
		"deleting an unused image must succeed")
	imageDeleted = true
}

// TestWS0_ImageNormalizesToRaw (FR-003 / T009): an uploaded qcow2 image is
// normalized to a raw base on ingest. Requires LIGHTSTACK_TEST_QCOW2 (a real qcow2
// file) since normalization runs qemu-img convert.
func TestWS0_ImageNormalizesToRaw(t *testing.T) {
	SkipIfLightstackNotRunning(t)
	qcow2 := os.Getenv("LIGHTSTACK_TEST_QCOW2")
	if qcow2 == "" {
		t.Skip("set LIGHTSTACK_TEST_QCOW2 to a qcow2 file path to exercise raw normalization (WS-0 T009)")
	}
	data, err := os.ReadFile(qcow2)
	require.NoError(t, err)

	imgClient := SetupImageV2Client(t)
	img, err := images.Create(imgClient, images.CreateOpts{
		Name:            "ws0-normalize-img",
		ContainerFormat: "bare",
		DiskFormat:      "qcow2",
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = images.Delete(imgClient, img.ID).ExtractErr() }()

	req, err := http.NewRequest("PUT", imgClient.ServiceURL("images", img.ID, "file"), bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", imgClient.TokenID)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	got, err := images.Get(imgClient, img.ID).Extract()
	require.NoError(t, err)
	assert.Equal(t, "raw", string(got.DiskFormat), "image must be normalized to raw on ingest")
}
