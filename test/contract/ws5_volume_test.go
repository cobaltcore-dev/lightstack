package contract_test

import (
	"testing"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/volumeattach"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-009 WS-5 contract test (API-observable, stub-safe). The real-mode device
// visibility + cross-detach persistence checks are in test/ws5_attach_real_test.sh.

func ws5WaitVolumeStatus(t *testing.T, c *gophercloud.ServiceClient, id, want string) string {
	t.Helper()
	var last string
	for range 25 {
		v, err := volumes.Get(c, id).Extract()
		require.NoError(t, err)
		last = v.Status
		if v.Status == want {
			return v.Status
		}
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

// TestWS5_VolumeAttachLifecycle (SC-009 / FR-021, FR-022): a volume reaches
// `available` synchronously on create, goes `in-use` on attach to a server and
// back to `available` on detach — all via the standard API.
func TestWS5_VolumeAttachLifecycle(t *testing.T) {
	SkipIfLightstackNotRunning(t)
	cinder := SetupBlockStorageV3Client(t)
	nova := SetupComputeV2Client(t)
	glance := SetupImageV2Client(t)

	img, err := images.Create(glance, images.CreateOpts{
		Name: "ws5-img", ContainerFormat: "bare", DiskFormat: "raw",
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = images.Delete(glance, img.ID).ExtractErr() }()

	srv, err := servers.Create(nova, servers.CreateOpts{
		Name: "ws5-vm", FlavorRef: flavorM1Small, ImageRef: img.ID,
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = servers.Delete(nova, srv.ID).ExtractErr() }()

	vol, err := volumes.Create(cinder, volumes.CreateOpts{Name: "ws5-vol", Size: 1}).Extract()
	require.NoError(t, err)
	defer func() { _ = volumes.Delete(cinder, vol.ID, volumes.DeleteOpts{}).ExtractErr() }()

	// Synchronous create: available immediately (no async state machine).
	assert.Equal(t, "available", ws5WaitVolumeStatus(t, cinder, vol.ID, "available"),
		"volume should be available right after create")

	// Synchronous attach: in-use as soon as the call returns.
	att, err := volumeattach.Create(nova, srv.ID, volumeattach.CreateOpts{VolumeID: vol.ID}).Extract()
	require.NoError(t, err)
	assert.Equal(t, vol.ID, att.VolumeID, "attachment should reference the volume")
	assert.Equal(t, "in-use", ws5WaitVolumeStatus(t, cinder, vol.ID, "in-use"),
		"volume should be in-use after attach")

	// Detach returns it to available. Delete by volume ID — in the Nova API the
	// attachment identifier IS the volume id (Lightstack's detach handler keys off it).
	require.NoError(t, volumeattach.Delete(nova, srv.ID, vol.ID).ExtractErr())
	assert.Equal(t, "available", ws5WaitVolumeStatus(t, cinder, vol.ID, "available"),
		"volume should be available after detach")
}
