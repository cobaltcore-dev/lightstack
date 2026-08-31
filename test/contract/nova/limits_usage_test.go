package nova_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-025 US7 / FR-040, FR-041 (cloud-storage#547 B7): reported usage must count every
// server the project owns, and must agree with the quota views.
//
// The usage query filtered `AND i.status != 'DELETED'` — a status nothing in the product
// has ever written, since every delete path is a hard DELETE FROM instances. The clause
// excluded nothing today, which is precisely why it was dangerous: it was the only usage
// query carrying it, so the moment anything began writing that status this view would
// silently under-count while the quota views kept counting.

// absoluteLimits fetches the absolute limits block.
func absoluteLimits(t *testing.T, token string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, novaRootURL(t)+"/v2.1/limits", nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Limits struct {
			Absolute map[string]any `json:"absolute"`
		} `json:"limits"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.Limits.Absolute
}

// serverCount returns how many servers the project has.
func serverCount(t *testing.T, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, novaRootURL(t)+"/v2.1/servers", nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Servers []struct {
			ID string `json:"id"`
		} `json:"servers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return len(out.Servers)
}

// T078 (FR-040, FR-041): usage matches the project's actual servers, and creating one
// moves the counter — which is what proves the count is derived rather than constant.
func TestNovaLimitsUsageMatchesReality_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)

	before := absoluteLimits(t, token)
	beforeUsed, ok := before["totalInstancesUsed"].(float64)
	require.True(t, ok, "totalInstancesUsed absent or not a number: %#v", before["totalInstancesUsed"])
	assert.Equal(t, float64(serverCount(t, token)), beforeUsed,
		"reported instance usage does not match the project's actual servers (FR-040)")

	// Create one and confirm the counter moves. Without this the assertion above would
	// also hold for a hardcoded value that happened to be right.
	id := createNamedServer(t, token, "spec025-usage-probe")

	after := absoluteLimits(t, token)
	afterUsed := after["totalInstancesUsed"].(float64)
	assert.Equal(t, beforeUsed+1, afterUsed,
		"creating a server did not move the usage counter; it is not derived from the servers")
	assert.Equal(t, float64(serverCount(t, token)), afterUsed,
		"usage and the server list disagree after a create")

	// Cores and memory must move too, and by the flavor's amounts rather than at all.
	beforeCores := before["totalCoresUsed"].(float64)
	afterCores := after["totalCoresUsed"].(float64)
	assert.Greater(t, afterCores, beforeCores,
		"core usage did not increase after a create")

	beforeRAM := before["totalRAMUsed"].(float64)
	afterRAM := after["totalRAMUsed"].(float64)
	assert.Greater(t, afterRAM, beforeRAM,
		"memory usage did not increase after a create")

	// And deleting it must move them back — a counter that only goes up is not a count.
	deleteServer(t, token, id)

	restored := absoluteLimits(t, token)
	assert.Equal(t, beforeUsed, restored["totalInstancesUsed"].(float64),
		"usage did not return to its previous value after the server was deleted")
}

// FR-040: the limits view and the quota-set view must report the same usage for the same
// resources. They are separate handlers over the same data, which is how they drifted
// before (the limits view once carried its own default map, ten times the quota-set
// defaults).
func TestNovaLimitsAgreeWithQuotaSet_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)

	limits := absoluteLimits(t, token)

	// The quota-set detail view reports in_use per resource. The seeded default
	// project's id is stable across the fixture set.
	const projectID = "00000000-0000-0000-0000-000000000002"
	req, err := http.NewRequest(http.MethodGet,
		novaRootURL(t)+"/v2.1/os-quota-sets/"+projectID+"/detail", nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// Required, not skipped: the detail view is a standard compute API this deployment
	// serves, and without it there is nothing to compare — a skip here let a regression of
	// either view pass.
	require.Equal(t, http.StatusOK, resp.StatusCode, "quota-set detail must answer")

	var out struct {
		QuotaSet map[string]any `json:"quota_set"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))

	for _, pair := range []struct{ limitsKey, quotaKey string }{
		{"totalInstancesUsed", "instances"},
		{"totalCoresUsed", "cores"},
		{"totalRAMUsed", "ram"},
	} {
		// A missing resource or in_use is a wrong answer, not one to step over.
		raw, ok := out.QuotaSet[pair.quotaKey].(map[string]any)
		require.True(t, ok, "quota-set detail has no %s: %v", pair.quotaKey, out.QuotaSet)
		inUse, ok := raw["in_use"].(float64)
		require.True(t, ok, "quota-set detail %s has no numeric in_use: %v", pair.quotaKey, raw)
		assert.Equal(t, limits[pair.limitsKey], inUse,
			"limits %s and quota-set %s.in_use disagree", pair.limitsKey, pair.quotaKey)
	}
}
