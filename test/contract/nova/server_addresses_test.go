package nova_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
)

// SPEC-025 US6 / FR-036: the single-server read and the server listing must report the
// SAME addresses.
//
// They are built by the same helper, but from two different call sites — and the
// argument that matters (the instance's owning project, not the caller's) is passed
// separately at each. A divergence here would mean one view widened and the other did
// not, which is exactly how an administrator ends up seeing an empty address map for
// another tenant's server.

// serverAddresses fetches one server's address map.
func serverAddresses(t *testing.T, token, id string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, novaRootURL(t)+"/v2.1/servers/"+id, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Server struct {
			Addresses map[string]any `json:"addresses"`
		} `json:"server"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.Server.Addresses
}

// listedAddresses fetches the address map for one server out of the detail listing.
func listedAddresses(t *testing.T, token, id string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, novaRootURL(t)+"/v2.1/servers/detail", nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Servers []struct {
			ID        string         `json:"id"`
			Addresses map[string]any `json:"addresses"`
		} `json:"servers"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	for _, s := range out.Servers {
		if s.ID == id {
			return s.Addresses
		}
	}
	t.Fatalf("server %s absent from the detail listing", id)
	return nil
}

// T073 (FR-036): the two views agree, and every entry they report carries a kind.
func TestNovaServerAddressesAgreeAcrossViews_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)

	// Boot onto a real network: with no ports the address map is empty and comparing
	// two empty maps proves nothing. The floating-address cases are covered
	// exhaustively by the unit tests, which can seed an association directly.
	netID := createNetworkWithSubnet(t, token, "spec025-addr-net", "10.244.90.0/24")
	id := createNamedServerOnNetwork(t, token, "spec025-addr-views", netID)

	// The boot is asynchronous: CreateServer is answered before the port is allocated,
	// so reading immediately is a race. In isolation there is enough slack to hide it;
	// under a full suite run there is not — which is how this surfaced.
	single := awaitAddresses(t, token, id)
	listed := listedAddresses(t, token, id)

	// Guard the guard: comparing two empty maps would pass whatever the code did, so
	// assert there is something to compare before comparing it.
	require.NotEmpty(t, single,
		"the server never got an address, so this test would prove nothing")

	assert.True(t, reflect.DeepEqual(single, listed),
		"the single read and the listing disagree:\n  single: %#v\n  listed: %#v", single, listed)

	// Whatever entries exist must be tagged. An untagged entry means something built
	// an address without going through the shared helper.
	for network, raw := range single {
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		for i, e := range entries {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := entry["OS-EXT-IPS:type"].(string)
			assert.Contains(t, []string{"fixed", "floating"}, kind,
				"network %s entry %d has kind %q; every address must be tagged fixed or floating (FR-035)",
				network, i, kind)
			assert.NotEmpty(t, entry["addr"], "network %s entry %d has no address", network, i)
		}
	}
}

// createNetworkWithSubnet creates a network and a subnet on it, returning the network id.
func createNetworkWithSubnet(t *testing.T, token, name, cidr string) string {
	t.Helper()

	post := func(url, body string) map[string]any {
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
		require.NoError(t, err)
		req.Header.Set("X-Auth-Token", token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, resp.StatusCode,
			"POST %s -> %d: %s", url, resp.StatusCode, buf.String())

		var out map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
		return out
	}

	// The network endpoint the service catalog advertises, not an assumed localhost.
	network := contract.SetupNetworkV2Client(t)
	netResp := post(network.ServiceURL("networks"), `{"network":{"name":"`+name+`","admin_state_up":true}}`)
	netID := netResp["network"].(map[string]any)["id"].(string)

	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, network.ServiceURL("networks", netID), nil)
		req.Header.Set("X-Auth-Token", token)
		if r, err := http.DefaultClient.Do(req); err == nil {
			_ = r.Body.Close()
		}
	})

	post(network.ServiceURL("subnets"),
		`{"subnet":{"network_id":"`+netID+`","cidr":"`+cidr+`","ip_version":4,"enable_dhcp":true}}`)
	return netID
}

// createNamedServerOnNetwork boots a server attached to a network.
func createNamedServerOnNetwork(t *testing.T, token, name, networkID string) string {
	t.Helper()
	image := firstImageID(t, token)
	body := `{"server":{"name":"` + name + `","flavorRef":"m1.small","imageRef":"` + image +
		`","networks":[{"uuid":"` + networkID + `"}]}}`

	code, resp := createServerRaw(t, token, body)
	require.Contains(t, []int{http.StatusAccepted, http.StatusCreated, http.StatusOK}, code,
		"creating a server on %s: %s", networkID, resp)

	var out struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp), &out))
	require.NotEmpty(t, out.Server.ID)

	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, novaRootURL(t)+"/v2.1/servers/"+out.Server.ID, nil)
		req.Header.Set("X-Auth-Token", token)
		if r, err := http.DefaultClient.Do(req); err == nil {
			_ = r.Body.Close()
		}
	})
	return out.Server.ID
}

// awaitAddresses polls a server's address map until it is populated, because the boot
// path allocates ports after the create has already been answered.
func awaitAddresses(t *testing.T, token, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		last = serverAddresses(t, token, id)
		if len(last) > 0 {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last
}
