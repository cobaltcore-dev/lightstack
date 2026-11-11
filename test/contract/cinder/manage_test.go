package cinder_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCinderManageVolume_Contract tests POST /v3/:project_id/os-volume-manage
func TestCinderManageVolume_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupCinderClient(t)

	// Manage existing volume
	payload := map[string]interface{}{
		"volume": map[string]interface{}{
			"host":        "host@backend#pool",
			"ref":         map[string]interface{}{"source-name": "existing-volume"},
			"name":        "managed-volume",
			"volume_type": "default",
		},
	}

	body, _ := json.Marshal(payload)
	url := client.ServiceURL("os-volume-manage")
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		Volume map[string]interface{} `json:"volume"`
	}
	err = json.Unmarshal(respBody, &result)
	require.NoError(t, err)

	assert.NotEmpty(t, result.Volume["id"])
	assert.Equal(t, "managed-volume", result.Volume["name"])

	// Cleanup
	volumeID := result.Volume["id"].(string)
	deleteURL := client.ServiceURL("volumes", volumeID)
	deleteReq, _ := http.NewRequest("DELETE", deleteURL, nil)
	deleteReq.Header.Set("X-Auth-Token", client.TokenID)
	http.DefaultClient.Do(deleteReq)
}

// TestCinderListManageableVolumes_Contract tests GET /v3/:project_id/manageable_volumes
func TestCinderListManageableVolumes_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupCinderClient(t)

	url := client.ServiceURL("manageable_volumes") + "?host=host@backend"
	req, err := http.NewRequest("GET", url, nil)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		ManageableVolumes []map[string]interface{} `json:"manageable-volumes"`
	}
	err = json.Unmarshal(respBody, &result)
	require.NoError(t, err)

	assert.NotNil(t, result.ManageableVolumes)
}

// TestCinderUnmanageVolume_Contract tests POST /v3/:project_id/volumes/:id/action (os-unmanage)
func TestCinderUnmanageVolume_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupCinderClient(t)

	// Create volume first via direct DB insert
	volumeID := createDBVolume(t, "test-unmanage-volume")

	// Unmanage volume
	action := map[string]interface{}{
		"os-unmanage": nil,
	}

	body, _ := json.Marshal(action)
	url := client.ServiceURL("volumes", volumeID, "action")
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
}

// TestCinderManageSnapshot_Contract tests POST /v3/:project_id/os-snapshot-manage
func TestCinderManageSnapshot_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupCinderClient(t)

	// Manage existing snapshot
	payload := map[string]interface{}{
		"snapshot": map[string]interface{}{
			"volume_id": "fake-volume-id",
			"ref":       map[string]interface{}{"source-name": "existing-snapshot"},
			"name":      "managed-snapshot",
		},
	}

	body, _ := json.Marshal(payload)
	url := client.ServiceURL("os-snapshot-manage")
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Accept 202 or 404 (if volume doesn't exist)
	assert.Contains(t, []int{http.StatusAccepted, http.StatusNotFound}, resp.StatusCode)
}

// Helper to create test volume in database
func createDBVolume(t *testing.T, name string) string {
	t.Helper()

	volumeID := uuid.New().String()
	contract.Seed(t,
		`INSERT INTO volumes (id, name, size_gb, status, project_id, created_at, updated_at)
		 VALUES ($1, $2, 1, 'available', (SELECT id FROM projects WHERE name = 'default' LIMIT 1), NOW(), NOW())`,
		volumeID, name)
	return volumeID
}
