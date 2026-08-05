package nova_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNovaServerEvacuate_Contract tests POST /v2.1/servers/:id/action (evacuate)
func TestNovaServerEvacuate_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	// Create server
	createOpts := servers.CreateOpts{
		Name:      "test-evacuate-server",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  "00000000-0000-0000-0000-000000000001",
	}

	server, err := servers.Create(client, createOpts).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	// Test: Evacuate via direct HTTP (gophercloud doesn't support it)
	url := client.ServiceURL("servers", server.ID, "action")
	body := strings.NewReader(`{"evacuate": {}}`)
	req, err := http.NewRequest("POST", url, body)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Accept 200/202/409 (evacuate accepted or already in progress)
	assert.Contains(t, []int{200, 202, 409}, resp.StatusCode, "Evacuate should not return 404")
}

// TestNovaServerMigrate_Contract tests POST /v2.1/servers/:id/action (migrate)
func TestNovaServerMigrate_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	createOpts := servers.CreateOpts{
		Name:      "test-migrate-server",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  "00000000-0000-0000-0000-000000000001",
	}

	server, err := servers.Create(client, createOpts).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	// Test: Migrate via direct HTTP
	url := client.ServiceURL("servers", server.ID, "action")
	body := strings.NewReader(`{"migrate": null}`)
	req, err := http.NewRequest("POST", url, body)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Contains(t, []int{202, 409}, resp.StatusCode, "Migrate should not return 404")
}

// TestNovaServerLiveMigrate_Contract tests POST /v2.1/servers/:id/action (os-migrateLive).
//
// Previously marked FLAKY. The race is real but benign: server create returns 202 and
// finishes asynchronously, so a live-migrate issued immediately hits either an ACTIVE
// instance (202) or one still building (409). Both are legitimate, and the test
// accepts both. What actually failed was the 409 branch's body assertion, which
// demanded a top-level "error" key — a shape neither Lightstack nor upstream Nova emits. It
// only looked intermittent because the 409 branch is only taken under load.
func TestNovaServerLiveMigrate_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	createOpts := servers.CreateOpts{
		Name:      "test-live-migrate-server",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  "00000000-0000-0000-0000-000000000001",
	}

	server, err := servers.Create(client, createOpts).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	// Test: Live migrate via direct HTTP
	url := client.ServiceURL("servers", server.ID, "action")
	body := strings.NewReader(`{"os-migrateLive": {"host": null, "block_migration": false}}`)
	req, err := http.NewRequest("POST", url, body)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Read the body ONCE. Two reads of the same response drain it: the second returns "",
	// and every diagnostic below then reports an empty body for a response that had one.
	// The two guards overlap — any status outside {202, 409} enters both — and the status
	// check is a soft assert, so execution reaches the second block regardless.
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "reading the response body")

	// Debug: Print response if not expected status
	if resp.StatusCode != 202 && resp.StatusCode != 409 {
		t.Logf("Unexpected status %d, body: %s", resp.StatusCode, string(respBody))
	}

	// Should accept 202 or 409 (conflict if instance not ACTIVE)
	assert.Contains(t, []int{202, 409}, resp.StatusCode, "LiveMigrate should not return 404")

	// Verify response body is valid JSON for non-202 responses
	if resp.StatusCode != 202 {
		var result map[string]interface{}
		err = json.Unmarshal(respBody, &result)
		require.NoErrorf(t, err, "error body should be JSON, got %s", string(respBody))

		// Nova wraps faults in an envelope named after the fault, not under a generic
		// "error" key: a 409 is {"conflictingRequest": {"code": 409, "message": "..."}}.
		fault, ok := result["conflictingRequest"].(map[string]interface{})
		require.Truef(t, ok, "409 body should carry a conflictingRequest fault, got %s", string(respBody))
		assert.EqualValues(t, resp.StatusCode, fault["code"], "fault code should match the HTTP status")
		assert.NotEmpty(t, fault["message"], "fault should carry a message")
	}
}
