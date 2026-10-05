package contract_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/stretchr/testify/require"
)

func SetupNeutronClient(t *testing.T) *gophercloud.ServiceClient {
	t.Helper()

	authURL := getEnvOrDefault("OS_AUTH_URL", "http://localhost:35357/v3")
	username := getEnvOrDefault("OS_USERNAME", "admin")
	password := getEnvOrDefault("OS_PASSWORD", "secret")
	projectName := getEnvOrDefault("OS_PROJECT_NAME", "default")
	domainName := getEnvOrDefault("OS_USER_DOMAIN_NAME", "Default")

	opts := gophercloud.AuthOptions{
		IdentityEndpoint: authURL,
		Username:         username,
		Password:         password,
		TenantName:       projectName,
		DomainName:       domainName,
	}

	provider, err := openstack.AuthenticatedClient(opts)
	require.NoError(t, err, "Failed to create authenticated client")

	client, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err, "Failed to create Neutron client")

	return client
}

// SetupOpenStackClient creates an authenticated OpenStack client for contract testing.
// It uses environment variables (OS_AUTH_URL, OS_USERNAME, etc.) to connect to lightstack.
// This function follows the TDD pattern from Constitution Article III.
func SetupOpenStackClient(t *testing.T) *gophercloud.ProviderClient {
	t.Helper()

	authURL := getEnvOrDefault("OS_AUTH_URL", "http://localhost:35357/v3")
	username := getEnvOrDefault("OS_USERNAME", "admin")
	password := getEnvOrDefault("OS_PASSWORD", "secret")
	projectName := getEnvOrDefault("OS_PROJECT_NAME", "default")
	domainName := getEnvOrDefault("OS_USER_DOMAIN_NAME", "Default")

	opts := gophercloud.AuthOptions{
		IdentityEndpoint: authURL,
		Username:         username,
		Password:         password,
		TenantName:       projectName,
		DomainName:       domainName,
	}

	provider, err := openstack.AuthenticatedClient(opts)
	require.NoError(t, err, "Failed to create authenticated OpenStack client")

	return provider
}

// SetupIdentityV3Client creates a Keystone v3 client for contract testing.
func SetupIdentityV3Client(t *testing.T) *gophercloud.ServiceClient {
	t.Helper()

	provider := SetupOpenStackClient(t)
	client, err := openstack.NewIdentityV3(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err, "Failed to create Keystone v3 client")

	return client
}

// SetupComputeV2Client creates a Nova v2.1 client for contract testing.
func SetupComputeV2Client(t *testing.T) *gophercloud.ServiceClient {
	t.Helper()

	provider := SetupOpenStackClient(t)
	client, err := openstack.NewComputeV2(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err, "Failed to create Nova v2.1 client")

	return client
}

// SetupNetworkV2Client creates a Neutron v2 client for contract testing.
func SetupNetworkV2Client(t *testing.T) *gophercloud.ServiceClient {
	t.Helper()

	provider := SetupOpenStackClient(t)
	client, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err, "Failed to create Neutron v2 client")

	return client
}

// SetupBlockStorageV3Client creates a Cinder v3 client for contract testing.
func SetupBlockStorageV3Client(t *testing.T) *gophercloud.ServiceClient {
	t.Helper()

	provider := SetupOpenStackClient(t)
	client, err := openstack.NewBlockStorageV3(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err, "Failed to create Cinder v3 client")

	return client
}

// SetupImageV2Client creates a Glance v2 client for contract testing.
func SetupImageV2Client(t *testing.T) *gophercloud.ServiceClient {
	t.Helper()

	provider := SetupOpenStackClient(t)
	client, err := openstack.NewImageServiceV2(provider, gophercloud.EndpointOpts{})
	require.NoError(t, err, "Failed to create Glance v2 client")

	return client
}

// CreateTestImage creates an active private image in the test client's project and deletes
// it when the test ends. Server create and rebuild, volume create from an image and
// os-reimage refuse an image that does not exist, that the caller cannot see, or that is
// not active, as upstream does, so a test that needs an image uses a real one.
//
// Its data is a few bytes, not a bootable disk: none of those checks reads it. A test that
// boots a real guest uploads its own.
func CreateTestImage(t *testing.T) string {
	t.Helper()
	client := SetupImageV2Client(t)
	img, err := images.Create(client, images.CreateOpts{
		Name:            "contract-test-image",
		ContainerFormat: "bare",
		DiskFormat:      "raw",
	}).Extract()
	require.NoError(t, err, "Failed to create the test image")
	// Runs after the test's own deferred server and volume deletes, so the image is no
	// longer in use by then.
	t.Cleanup(func() { _ = images.Delete(client, img.ID).ExtractErr() })
	UploadTestImageData(t, client, img.ID)
	return img.ID
}

// UploadTestImageData uploads a few bytes to an image, which makes it active.
func UploadTestImageData(t *testing.T, client *gophercloud.ServiceClient, imageID string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, client.ServiceURL("images", imageID, "file"),
		strings.NewReader("lightstack contract test image"))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "Failed to upload the test image's data")
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "uploading the test image's data")
}

// getEnvOrDefault returns environment variable value or default if not set.
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// GetProjectIDFromToken returns the project_id of the authenticated user by
// querying Keystone (/v3/projects). Useful for tests that need to construct
// resource URLs scoped to the caller's project but don't want to depend on
// the project having pre-existing data.
//
// Returns the first project Keystone reports for the token — for contract
// tests authenticating as the seeded admin user, this is the "default"
// project.
func GetProjectIDFromToken(t *testing.T, client *gophercloud.ServiceClient) string {
	t.Helper()

	authURL := getEnvOrDefault("OS_AUTH_URL", "http://localhost:35357/v3")
	req, err := http.NewRequest("GET", authURL+"/projects", nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result struct {
		Projects []map[string]interface{} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	require.NotEmpty(t, result.Projects, "Keystone returned no projects for this token")

	projectID, ok := result.Projects[0]["id"].(string)
	require.True(t, ok && projectID != "", "Project entry missing usable id")
	return projectID
}

// SkipIfLightstackNotRunning skips the test if Lightstack is not running.
// Use this in contract tests to avoid false failures in CI.
func SkipIfLightstackNotRunning(t *testing.T) {
	t.Helper()

	authURL := getEnvOrDefault("OS_AUTH_URL", "http://localhost:35357/v3")
	opts := gophercloud.AuthOptions{
		IdentityEndpoint: authURL,
		Username:         "admin",
		Password:         "secret",
		TenantName:       "default",
		DomainName:       "Default",
	}

	_, err := openstack.AuthenticatedClient(opts)
	if err != nil {
		t.Skipf("Lightstack not running at %s: %v", authURL, err)
	}
}
