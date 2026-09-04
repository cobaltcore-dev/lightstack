package keystone_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-014 FR-005/FR-007: the OpenStack EC2-credentials API round-trip
// (create -> list -> show -> delete) at /v3/users/:id/credentials/OS-EC2.
func TestKeystoneEC2CredentialLifecycle_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := setupKeystoneClient(t)

	const userID = "00000000-0000-0000-0000-000000000001"    // seeded admin user
	const projectID = "00000000-0000-0000-0000-000000000002" // seeded default project

	base := client.ServiceURL("users", userID, "credentials", "OS-EC2")

	// --- Create ---
	body, _ := json.Marshal(map[string]any{"tenant_id": projectID})
	req, _ := http.NewRequest("POST", base, bytes.NewReader(body))
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create body: %s", respBody)

	var created struct {
		Credential map[string]any `json:"credential"`
	}
	require.NoError(t, json.Unmarshal(respBody, &created))
	access, _ := created.Credential["access"].(string)
	secret, _ := created.Credential["secret"].(string)
	require.NotEmpty(t, access, "access key must be returned")
	require.NotEmpty(t, secret, "secret key must be returned")
	assert.Equal(t, projectID, created.Credential["tenant_id"])

	one := client.ServiceURL("users", userID, "credentials", "OS-EC2", access)

	// Cleanup regardless of later assertions.
	defer func() {
		dreq, _ := http.NewRequest("DELETE", one, nil)
		dreq.Header.Set("X-Auth-Token", client.TokenID)
		if dresp, e := http.DefaultClient.Do(dreq); e == nil {
			dresp.Body.Close()
		}
	}()

	// --- List (the new access key must appear) ---
	lreq, _ := http.NewRequest("GET", base, nil)
	lreq.Header.Set("X-Auth-Token", client.TokenID)
	lresp, err := http.DefaultClient.Do(lreq)
	require.NoError(t, err)
	lbody, _ := io.ReadAll(lresp.Body)
	lresp.Body.Close()
	assert.Equal(t, http.StatusOK, lresp.StatusCode)
	assert.Contains(t, string(lbody), access, "listed credentials should include the new access key")

	// --- Show one by access key ---
	greq, _ := http.NewRequest("GET", one, nil)
	greq.Header.Set("X-Auth-Token", client.TokenID)
	gresp, err := http.DefaultClient.Do(greq)
	require.NoError(t, err)
	gbody, _ := io.ReadAll(gresp.Body)
	gresp.Body.Close()
	assert.Equal(t, http.StatusOK, gresp.StatusCode)
	assert.Contains(t, string(gbody), access)

	// --- Delete (revocation) ---
	dreq, _ := http.NewRequest("DELETE", one, nil)
	dreq.Header.Set("X-Auth-Token", client.TokenID)
	dresp, err := http.DefaultClient.Do(dreq)
	require.NoError(t, err)
	dresp.Body.Close()
	assert.Equal(t, http.StatusNoContent, dresp.StatusCode)

	// --- Show after delete -> 404 ---
	g2, _ := http.NewRequest("GET", one, nil)
	g2.Header.Set("X-Auth-Token", client.TokenID)
	g2resp, err := http.DefaultClient.Do(g2)
	require.NoError(t, err)
	g2resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, g2resp.StatusCode)
}
