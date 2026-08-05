package nova_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Console contract tests (SPEC-021 / cloud-storage#534, finding F1).
//
// These were rewritten with SPEC-021. The previous versions asserted the behaviour
// F1 was filed against: 200 plus a URL for every console type, including the three
// O3K cannot serve and including stub mode, where no guest exists. That contract was
// a fiction — the URLs pointed at a hardcoded localhost:6080 with a token nothing had
// recorded — so encoding it made the suite green while the console was unusable.
//
// The contract now is:
//
//	novnc  -> 200 + a resolvable URL in real mode; 501 in stub mode (no hypervisor,
//	          so no guest to display)
//	others -> 400, mode-independent: xvpvnc, serial, spice-html5 and rdp-html5 are
//	          refused rather than answered with a URL that leads nowhere
//
// A refusal must never carry a URL. That is the specific regression these guard: a
// plausible link is worse than an honest error, because it moves the failure into the
// browser where it looks like a networking problem.
//
// Set O3K_MODE=real when running against a libvirt deployment, matching
// test/console_test.sh. Default is stub.

// o3kMode reports the deployment mode under test.
func o3kMode() string {
	if m := os.Getenv("O3K_MODE"); m != "" {
		return m
	}
	return "stub"
}

// newConsoleTestServer creates a throwaway instance and registers its cleanup.
func newConsoleTestServer(t *testing.T, client *gophercloud.ServiceClient, name string) *servers.Server {
	t.Helper()
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      name,
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  "00000000-0000-0000-0000-000000000001",
	}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = servers.Delete(client, server.ID).ExtractErr() })
	return server
}

// postServerAction sends one action payload and returns the status and raw body.
// Six near-identical request blocks used to be inlined per test; the shape of the
// request is not what any of these tests are about.
func postServerAction(t *testing.T, client *gophercloud.ServiceClient, path string, payload map[string]any) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, respBody
}

// assertRefusalCarriesNoURL is the assertion that matters on every refusal path.
func assertRefusalCarriesNoURL(t *testing.T, body []byte) {
	t.Helper()
	s := string(body)
	assert.NotContains(t, s, "http://", "a refusal must not contain a URL: %s", s)
	assert.NotContains(t, s, "ws://", "a refusal must not contain a URL: %s", s)
}

// assertConsoleURLShape pins the URL O3K issues: <origin>/vnc_lite.html?path=…token…
// The page name is load-bearing — the deployed noVNC is 1.6.0, which has no
// vnc_auto.html, the page four operator guides recommended for years.
func assertConsoleURLShape(t *testing.T, url string) {
	t.Helper()
	assert.NotEmpty(t, url, "real mode must return a console URL")
	assert.Contains(t, url, "/vnc_lite.html?path=", "unexpected console URL shape: %s", url)
	assert.NotContains(t, url, "vnc_auto.html", "vnc_auto.html does not exist in noVNC 1.6.0")
	assert.NotContains(t, url, "localhost:6080", "the console origin must be configured, not hardcoded")
}

// TestNovaGetConsoleOutput_Contract tests POST /v2.1/servers/:id/action (os-getConsoleOutput).
//
// Deliberately unchanged by SPEC-021: the serial log is the documented fallback that
// kept working throughout the F1 outage, and breaking it would be a regression
// (FR-026).
func TestNovaGetConsoleOutput_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := setupNovaClient(t)
	server := newConsoleTestServer(t, client, "test-console-output")

	code, body := postServerAction(t, client, client.ServiceURL("servers", server.ID, "action"),
		map[string]any{"os-getConsoleOutput": map[string]any{"length": 50}})

	assert.Equal(t, http.StatusOK, code)

	var result map[string]any
	require.NoError(t, json.Unmarshal(body, &result))
	assert.NotNil(t, result["output"])
}

// TestNovaGetVNCConsole_Contract tests the legacy os-getVNCConsole action.
//
// It routes through the same issuance as remote-consoles (FR-005), so the two must
// agree: two code paths returning two different URLs for one instance is how
// consoles rot.
func TestNovaGetVNCConsole_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := setupNovaClient(t)
	server := newConsoleTestServer(t, client, "test-vnc-console")

	code, body := postServerAction(t, client, client.ServiceURL("servers", server.ID, "action"),
		map[string]any{"os-getVNCConsole": map[string]any{"type": "novnc"}})

	if o3kMode() == "stub" {
		// No hypervisor means no guest. Saying so beats a link that fails in the
		// browser, which is exactly what F1 looked like to a user.
		require.Equal(t, http.StatusNotImplemented, code, "stub mode must refuse: %s", body)
		assert.Contains(t, string(body), "notImplemented")
		assert.Contains(t, strings.ToLower(string(body)), "stub", "the refusal should name stub mode: %s", body)
		assertRefusalCarriesNoURL(t, body)
		return
	}

	// Real mode: 409 is legitimate when the instance is not running yet, but a 200
	// must carry a properly shaped URL.
	require.Contains(t, []int{http.StatusOK, http.StatusConflict}, code, "unexpected status: %s", body)
	if code != http.StatusOK {
		assertRefusalCarriesNoURL(t, body)
		return
	}

	var result struct {
		Console struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"console"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, "novnc", result.Console.Type)
	assertConsoleURLShape(t, result.Console.URL)
}

// TestNovaCreateRemoteConsole_Contract tests POST /v2.1/servers/:id/remote-consoles
// (microversion 2.6+).
func TestNovaCreateRemoteConsole_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := setupNovaClient(t)
	server := newConsoleTestServer(t, client, "test-remote-console")

	body, err := json.Marshal(map[string]any{
		"remote_console": map[string]any{"protocol": "vnc", "type": "novnc"},
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost,
		client.ServiceURL("servers", server.ID, "remote-consoles"), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenStack-API-Version", "compute 2.6")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	if o3kMode() == "stub" {
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode, "stub mode must refuse: %s", respBody)
		assertRefusalCarriesNoURL(t, respBody)
		return
	}

	require.Contains(t, []int{http.StatusOK, http.StatusConflict}, resp.StatusCode,
		"unexpected status: %s", respBody)
	if resp.StatusCode != http.StatusOK {
		assertRefusalCarriesNoURL(t, respBody)
		return
	}

	var result struct {
		RemoteConsole struct {
			Protocol string `json:"protocol"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"remote_console"`
	}
	require.NoError(t, json.Unmarshal(respBody, &result))
	assert.Equal(t, "vnc", result.RemoteConsole.Protocol)
	assert.Equal(t, "novnc", result.RemoteConsole.Type)
	assertConsoleURLShape(t, result.RemoteConsole.URL)
}

// TestNovaUnsupportedConsoleTypesRefused_Contract covers every console type O3K does
// not serve (FR-025).
//
// Each of these previously returned 200 with a fabricated URL — SPICE even carried a
// made-up port 6082, RDP 6084, neither of which anything listened on. Refusing is
// mode-independent: no deployment of O3K serves them.
func TestNovaUnsupportedConsoleTypesRefused_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := setupNovaClient(t)
	server := newConsoleTestServer(t, client, "test-unsupported-consoles")
	action := client.ServiceURL("servers", server.ID, "action")

	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"os-getSerialConsole", map[string]any{"os-getSerialConsole": map[string]any{"type": "serial"}}},
		{"os-getSPICEConsole", map[string]any{"os-getSPICEConsole": map[string]any{"type": "spice-html5"}}},
		{"os-getRDPConsole", map[string]any{"os-getRDPConsole": map[string]any{"type": "rdp-html5"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postServerAction(t, client, action, tc.payload)
			assert.Equal(t, http.StatusBadRequest, code, "%s must be refused: %s", tc.name, body)
			assert.Contains(t, string(body), "badRequest")
			assertRefusalCarriesNoURL(t, body)
		})
	}

	// The modern endpoint refuses unsupported types too, not just the legacy actions.
	t.Run("remote-consoles xvpvnc", func(t *testing.T) {
		code, body := postServerAction(t, client,
			client.ServiceURL("servers", server.ID, "remote-consoles"),
			map[string]any{"remote_console": map[string]any{"protocol": "vnc", "type": "xvpvnc"}})
		assert.Equal(t, http.StatusBadRequest, code, "xvpvnc must be refused: %s", body)
		assertRefusalCarriesNoURL(t, body)
	})
}

// TestNovaConsoleAuthTokenUnknown_Contract covers GET
// /v2.1/os-console-auth-tokens/{token} (microversion 2.31+), served for API
// compatibility only — the proxy resolves tokens from its projection directory and
// never asks O3K.
func TestNovaConsoleAuthTokenUnknown_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)
	client := setupNovaClient(t)

	req, err := http.NewRequest(http.MethodGet,
		client.ServiceURL("os-console-auth-tokens", "deadbeefdeadbeefdeadbeefdeadbeef"), nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Unknown, expired and instance-gone are deliberately indistinguishable.
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
