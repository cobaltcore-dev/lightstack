package nova_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/quotasets"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FR-035a (SPEC-028): a server create over the project's instance quota answers 403 with
// upstream's message — TooManyInstances (nova/exception.py), raised as HTTPForbidden on
// create — not 413 overLimit. The project is the test's own: filling the shared project's
// quota would refuse the creates of tests running alongside.
func TestNovaServerCreateOverQuotaIs403_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	adminProvider, _ := setupAdminClient(t)
	member, projectID := freshProjectMember(t, adminProvider)

	admin, err := openstack.NewComputeV2(adminProvider, gophercloud.EndpointOpts{})
	require.NoError(t, err)
	detail, err := quotasets.GetDetail(admin, projectID).Extract()
	require.NoError(t, err, "reading the project's quota")
	used := detail.Instances.InUse
	_, err = quotasets.Update(admin, projectID, quotasets.UpdateOpts{Instances: &used}).Extract()
	require.NoError(t, err, "setting the instances quota to the project's usage")
	// Deleting the project at the end takes its quota with it.

	compute, err := openstack.NewComputeV2(member, gophercloud.EndpointOpts{})
	require.NoError(t, err)
	server, err := servers.Create(compute, servers.CreateOpts{
		Name: "spec028-over-quota", FlavorRef: "m1.small", ImageRef: publicTestImage(t, adminProvider),
	}).Extract()
	if err == nil {
		_ = servers.Delete(compute, server.ID).ExtractErr()
		t.Fatalf("a server was created over the instance quota")
	}
	var unexpected gophercloud.ErrUnexpectedResponseCode
	require.True(t, errors.As(err, &unexpected), "create failed with %v; want an unexpected response code", err)
	assert.Equal(t, http.StatusForbidden, unexpected.Actual, "body: %s", unexpected.Body)
	assert.Contains(t, string(unexpected.Body),
		fmt.Sprintf("Quota exceeded for instances: Requested 1, but already used %d of %d instances", used, used))
}

// publicTestImage creates an active image every project may boot, removed at the end.
func publicTestImage(t *testing.T, adminProvider *gophercloud.ProviderClient) string {
	t.Helper()
	client, err := openstack.NewImageServiceV2(adminProvider, gophercloud.EndpointOpts{})
	require.NoError(t, err)
	public := images.ImageVisibilityPublic
	img, err := images.Create(client, images.CreateOpts{
		Name: "spec028-public-image", ContainerFormat: "bare", DiskFormat: "raw", Visibility: &public,
	}).Extract()
	require.NoError(t, err, "creating a public image")
	t.Cleanup(func() { _ = images.Delete(client, img.ID).ExtractErr() })
	contract.UploadTestImageData(t, client, img.ID)
	return img.ID
}
