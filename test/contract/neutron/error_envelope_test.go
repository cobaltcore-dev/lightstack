package neutron_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	contract "github.com/cobaltcore-dev/o3k/test/contract"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-024 (cloud-storage#546) US2 / B10 — the error envelope, verified through a real client.
//
// The unit tests in internal/neutron exercise the handler; these exist because Constitution Article I
// requires verification with real OpenStack clients, and because the envelope is precisely the thing
// an SDK parses. Note the existing neutron contract suite passes *today* despite B10: gophercloud
// keys off the HTTP status, not the body, which is why 59 green contract tests never caught this.
// So these assert on the raw body as well as on the client's behaviour.

const contractAbsentID = "00000000-0000-0000-0000-999999999999"

// rawNeutronGET issues an authenticated raw request through the gophercloud provider, so the body is
// exactly what a client would receive — gophercloud's typed errors discard it.
func rawNeutronGET(t *testing.T, client *gophercloud.ServiceClient, url string) (int, map[string]any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body), "response is not JSON: %s", string(raw))
	return resp.StatusCode, body
}

// assertNeutronEnvelope pins the upstream shape: wrapper `NeutronError`, inner keys exactly
// type/message/detail, and no numeric `code`.
func assertNeutronEnvelope(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	require.Len(t, body, 1, "expected exactly one top-level key, got %v", body)
	inner, ok := body["NeutronError"].(map[string]any)
	require.True(t, ok, "expected wrapper key `NeutronError`, got %v — this is B10", body)

	assert.NotContains(t, inner, "code", "Neutron errors carry no numeric `code`; upstream has no such field")
	assert.Contains(t, inner, "type")
	assert.Contains(t, inner, "message")
	assert.Contains(t, inner, "detail")
	assert.Len(t, inner, 3, "expected exactly type/message/detail, got %v", inner)
	return inner
}

// TestNeutronErrorEnvelope_Contract covers E1, E2 and E3 through the SDK's own endpoint resolution.
func TestNeutronErrorEnvelope_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := contract.SetupNeutronClient(t)

	// The typed client path: gophercloud must still classify this as a 404. If the envelope change
	// broke status handling, this fails first and points at the cause.
	_, err := networks.Get(client, contractAbsentID).Extract()
	require.Error(t, err, "expected an error for an absent network")
	assert.IsType(t, gophercloud.ErrDefault404{}, err, "client must still see a 404")

	for _, tc := range []struct{ resource, wantType string }{
		{"networks", "NetworkNotFound"},
		{"subnets", "SubnetNotFound"},
		{"ports", "PortNotFound"},
		{"routers", "RouterNotFound"},
		{"security-groups", "SecurityGroupNotFound"},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			status, body := rawNeutronGET(t, client, client.ServiceURL(tc.resource, contractAbsentID))
			assert.Equal(t, http.StatusNotFound, status)
			inner := assertNeutronEnvelope(t, body)
			assert.Equal(t, tc.wantType, inner["type"], "each resource names its own condition")
		})
	}
}

// TestNeutronErrorEnvelopeUnauthenticated_Contract covers E5 for real — no token at all, so the
// response comes from the shared auth middleware at engine level, before any route group exists.
// This is the path a group-scoped marker would miss.
func TestNeutronErrorEnvelopeUnauthenticated_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := contract.SetupNeutronClient(t)

	req, err := http.NewRequest(http.MethodGet, client.ServiceURL("networks"), nil)
	require.NoError(t, err)
	// deliberately no X-Auth-Token

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body), "response is not JSON: %s", string(raw))
	inner := assertNeutronEnvelope(t, body)
	assert.Equal(t, "NotAuthorized", inner["type"])
}

// TestNeutronErrorEnvelopeUnroutedPath_Contract covers E6 — NoRoute, which is registered on the
// engine and never runs inside a route group.
func TestNeutronErrorEnvelopeUnroutedPath_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := contract.SetupNeutronClient(t)

	status, body := rawNeutronGET(t, client, client.ServiceURL("nonexistent"))
	assert.Equal(t, http.StatusNotFound, status)
	inner := assertNeutronEnvelope(t, body)
	assert.Equal(t, "NotFound", inner["type"])
}

// TestOtherServicesKeepTheirEnvelope_Contract covers E10 / FR-010: this feature changes the network
// service only. Compute and block storage must still answer `itemNotFound` with a numeric `code`,
// which is what test/error_handling_test.sh asserts too.
func TestOtherServicesKeepTheirEnvelope_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := contract.SetupNeutronClient(t)

	novaURL := os.Getenv("O3K_NOVA_URL")
	if novaURL == "" {
		novaURL = "http://localhost:8774"
	}

	req, err := http.NewRequest(http.MethodGet, novaURL+"/v2.1/servers/"+contractAbsentID, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body), "response is not JSON: %s", string(raw))

	inner, ok := body["itemNotFound"].(map[string]any)
	require.True(t, ok, "compute must keep its own envelope, got %v", body)
	assert.Contains(t, inner, "code", "compute envelope keeps its numeric code")
	assert.NotContains(t, body, "NeutronError", "the Neutron envelope must not leak into compute")
}
