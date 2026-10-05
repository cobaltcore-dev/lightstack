package contract_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/keypairs"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-009 WS-4 contract test (API-observable, stub-safe). A server created with
// user_data + a keypair has its metadata served — instance-id, user_data, and the
// SSH public key — over the EC2 endpoints. Identity uses the X-Instance-ID test
// header; the source-IP path (from inside a VM) is in test/ws4_metadata_test.sh.

func ws4MetadataURL() string {
	return getEnvOrDefault("LIGHTSTACK_METADATA_URL", "http://localhost:8775")
}

func ws4MetaGet(t *testing.T, path, instanceID string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", ws4MetadataURL()+path, nil)
	require.NoError(t, err)
	req.Header.Set("X-Instance-ID", instanceID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

func TestWS4_MetadataUserDataKey(t *testing.T) {
	SkipIfLightstackNotRunning(t)
	nova := SetupComputeV2Client(t)
	glance := SetupImageV2Client(t)

	// Omit PublicKey so Lightstack generates the keypair (Nova behavior) and returns it.
	kp, err := keypairs.Create(nova, keypairs.CreateOpts{Name: "ws4-key"}).Extract()
	require.NoError(t, err)
	require.NotEmpty(t, kp.PublicKey, "Lightstack should generate and return a public key")
	defer func() { _ = keypairs.Delete(nova, kp.Name, nil).ExtractErr() }()

	img, err := images.Create(glance, images.CreateOpts{
		Name: "ws4-img", ContainerFormat: "bare", DiskFormat: "raw",
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = images.Delete(glance, img.ID).ExtractErr() }()
	UploadTestImageData(t, glance, img.ID) // server create refuses an image that is not active

	userData := "#!/bin/sh\ntouch /tmp/ws4-userdata-ran\n"
	base := servers.CreateOpts{
		Name: "ws4-vm", FlavorRef: flavorM1Small, ImageRef: img.ID,
		UserData: []byte(userData),
	}
	srv, err := servers.Create(nova, keypairs.CreateOptsExt{
		CreateOptsBuilder: base,
		KeyName:           kp.Name,
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = servers.Delete(nova, srv.ID).ExtractErr() }()

	// instance-id is served (EC2 endpoint), keyed by the X-Instance-ID test header.
	code, id := ws4MetaGet(t, "/2009-04-04/meta-data/instance-id", srv.ID)
	assert.Equal(t, http.StatusOK, code, "instance-id endpoint should answer")
	assert.NotEmpty(t, id, "instance-id should be non-empty")

	// user_data round-trips: create API persisted it, metadata serves it back.
	code, ud := ws4MetaGet(t, "/2009-04-04/user-data", srv.ID)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, strings.TrimSpace(userData), ud, "served user_data should match what was submitted")

	// the injected SSH key is served on the EC2 public-keys subtree (the path cirros uses).
	// The EC2 public-keys subtree (the path cirros uses) must serve an SSH key —
	// this is the SPEC-009 WS-4 fix for the previously-404'ing openssh-key endpoint.
	code, key := ws4MetaGet(t, "/2009-04-04/meta-data/public-keys/0/openssh-key", srv.ID)
	assert.Equal(t, http.StatusOK, code, "openssh-key endpoint should answer")
	assert.True(t, strings.HasPrefix(key, "ssh-"), "openssh-key should serve a valid SSH public key, got %q", key)
}
