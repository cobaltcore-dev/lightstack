package nova_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
)

// TestNovaListTenantUsage_Contract tests GET /v2.1/:project_id/os-simple-tenant-usage
func TestNovaListTenantUsage_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	// Query parameters for date range
	start := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	end := time.Now().Format(time.RFC3339)

	url := client.ServiceURL("os-simple-tenant-usage") + "?start=" + start + "&end=" + end
	req, err := http.NewRequest("GET", url, nil)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		TenantUsages []map[string]interface{} `json:"tenant_usages"`
	}
	err = json.Unmarshal(respBody, &result)
	require.NoError(t, err)

	assert.NotNil(t, result.TenantUsages)
}

// TestNovaGetTenantUsage_Contract tests GET /v2.1/:project_id/os-simple-tenant-usage/:tenant_id
func TestNovaGetTenantUsage_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	// Get project ID from token
	projectID := contract.GetProjectIDFromToken(t, client)

	start := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	end := time.Now().Format(time.RFC3339)

	url := client.ServiceURL("os-simple-tenant-usage", projectID) + "?start=" + start + "&end=" + end
	req, err := http.NewRequest("GET", url, nil)
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	respBody, _ := io.ReadAll(resp.Body)
	var result struct {
		TenantUsage map[string]interface{} `json:"tenant_usage"`
	}
	err = json.Unmarshal(respBody, &result)
	require.NoError(t, err)

	assert.NotEmpty(t, result.TenantUsage)
	assert.Equal(t, projectID, result.TenantUsage["tenant_id"])
}
