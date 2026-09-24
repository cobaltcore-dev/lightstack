package nova_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNovaListMigrations_Contract tests GET /v2.1/:project_id/os-migrations
func TestNovaListMigrations_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	url := client.ServiceURL("os-migrations")
	req, err := http.NewRequest("GET", url, nil)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Migrations []map[string]interface{} `json:"migrations"`
	}
	err = json.Unmarshal(respBody, &result)
	require.NoError(t, err)

	assert.NotNil(t, result.Migrations)
}

// TestNovaListServerMigrations_Contract tests GET /v2.1/:project_id/servers/:id/migrations
func TestNovaListServerMigrations_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	serverID, cleanup := getTestServerID(t, client)
	defer cleanup()

	url := client.ServiceURL("servers", serverID, "migrations")
	req, err := http.NewRequest("GET", url, nil)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Migrations []map[string]interface{} `json:"migrations"`
	}
	err = json.Unmarshal(respBody, &result)
	require.NoError(t, err)

	assert.NotNil(t, result.Migrations)
}

// TestNovaGetServerMigration_Contract tests GET /v2.1/:project_id/servers/:id/migrations/:id
func TestNovaGetServerMigration_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	serverID, cleanup := getTestServerID(t, client)
	defer cleanup()

	migrationID := createTestMigration(t, client, serverID)

	url := client.ServiceURL("servers", serverID, "migrations", migrationID)
	req, err := http.NewRequest("GET", url, nil)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Migration map[string]interface{} `json:"migration"`
	}
	err = json.Unmarshal(respBody, &result)
	require.NoError(t, err)

	assert.NotEmpty(t, result.Migration["id"])
	assert.Equal(t, serverID, result.Migration["server_uuid"])
}

// TestNovaDeleteServerMigration_Contract tests DELETE /v2.1/:project_id/servers/:id/migrations/:id
func TestNovaDeleteServerMigration_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	serverID, cleanup := getTestServerID(t, client)
	defer cleanup()

	migrationID := createTestMigration(t, client, serverID)

	// Test: Delete (cancel) migration
	url := client.ServiceURL("servers", serverID, "migrations", migrationID)
	req, err := http.NewRequest("DELETE", url, nil)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

// TestNovaForceCompleteMigration_Contract tests POST /v2.1/:project_id/servers/:id/migrations/:id/action
func TestNovaForceCompleteMigration_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	serverID, cleanup := getTestServerID(t, client)
	defer cleanup()

	migrationID := createTestMigration(t, client, serverID)

	// Test: Force complete migration
	action := map[string]interface{}{
		"force_complete": nil,
	}

	body, _ := json.Marshal(action)
	url := client.ServiceURL("servers", serverID, "migrations", migrationID, "action")
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
}

// getTestServerID creates a throwaway test server in the project and returns
// its ID alongside a cleanup function. Callers should `defer cleanup()`.
//
// Previously this helper just listed existing servers and returned the first,
// or "" — which made every migration test skip against a fresh project and
// coupled them to whatever state earlier tests left behind. Always creating
// keeps the tests deterministic.
func getTestServerID(t *testing.T, client *gophercloud.ServiceClient) (string, func()) {
	t.Helper()
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "migration-test-fixture",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  testImageID(t),
	}).Extract()
	require.NoError(t, err, "Failed to create test server fixture")

	cleanup := func() {
		servers.Delete(client, server.ID)
	}
	return server.ID, cleanup
}

// createTestMigration starts a (stub) cold migration through the API and returns its id
// from the server's migration list. It used to INSERT the row with
// `docker exec lightstack-postgres psql …` and discard the result, so against any
// database but that one container the row never existed and every test that needed
// it answered 404. Rows go with the server: server_migrations cascades on delete.
func createTestMigration(t *testing.T, client *gophercloud.ServiceClient, serverID string) string {
	t.Helper()

	// migrate refuses anything but ACTIVE or SHUTOFF, and a new server starts in BUILD.
	for i := 0; ; i++ {
		s, err := servers.Get(client, serverID).Extract()
		if err == nil && s.Status == "ACTIVE" {
			break
		}
		require.Less(t, i, 30, "test server never reached ACTIVE")
		time.Sleep(time.Second)
	}

	code, body := postServerAction(t, client, client.ServiceURL("servers", serverID, "action"),
		map[string]any{"migrate": nil})
	require.Equal(t, http.StatusAccepted, code, "migrate: %s", body)

	req, err := http.NewRequest(http.MethodGet, client.ServiceURL("servers", serverID, "migrations"), nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	var list struct {
		Migrations []struct {
			ID string `json:"id"`
		} `json:"migrations"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.NotEmpty(t, list.Migrations, "the migrate action recorded no migration")
	return list.Migrations[0].ID
}
