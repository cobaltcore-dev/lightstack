package nova_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"testing"

	"github.com/google/uuid"
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

	migrationID := createTestMigration(t, serverID)

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

	// Cleanup
	cleanupTestMigration(t, migrationID)
}

// TestNovaDeleteServerMigration_Contract tests DELETE /v2.1/:project_id/servers/:id/migrations/:id
func TestNovaDeleteServerMigration_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	serverID, cleanup := getTestServerID(t, client)
	defer cleanup()

	migrationID := createTestMigration(t, serverID)

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

	migrationID := createTestMigration(t, serverID)

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

	// Cleanup
	cleanupTestMigration(t, migrationID)
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

// Helper to create test migration in database
func createTestMigration(t *testing.T, serverID string) string {
	t.Helper()

	migrationID := uuid.New().String()

	cmd := `docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
	INSERT INTO server_migrations (id, server_uuid, source_node, dest_node, status, migration_type, created_at, updated_at)
	VALUES ('` + migrationID + `', '` + serverID + `', 'node1', 'node2', 'migrating', 'live-migration', NOW(), NOW())
	ON CONFLICT DO NOTHING;"`

	exec.Command("sh", "-c", cmd).Run()

	return migrationID
}

// Helper to cleanup test migration
func cleanupTestMigration(t *testing.T, migrationID string) {
	t.Helper()

	cmd := `docker exec lightstack-postgres psql -U lightstack -d lightstack -c "DELETE FROM server_migrations WHERE id='` + migrationID + `';"`
	exec.Command("sh", "-c", cmd).Run()
}
