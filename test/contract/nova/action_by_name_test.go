package nova_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-025 US5 (cloud-storage#547 B14): a server action addressed by NAME must behave
// like every other server operation.
//
// Lightstack deliberately accepts a name wherever upstream accepts only a UUID, and most of the
// compute surface honours it — the dispatcher resolves `id::text = $1 OR name = $1`, and
// so does DELETE /servers/{ref}. The action sub-handlers did not: they read the raw path
// parameter straight into `WHERE id = $1` against a uuid column, so a name failed the
// cast, missed the not-found branch and landed on a generic 500.
//
// This MUST be driven over HTTP directly. python-openstackclient resolves names to
// UUIDs client-side before the request leaves the machine, so no CLI-driven suite can
// reproduce it — which is why it went unnoticed.

// postAction sends a server action for a reference (id or name) and returns the status.
func postAction(t *testing.T, token, ref, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		novaRootURL(t)+"/v2.1/servers/"+ref+"/action", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// createNamedServer boots a server and returns its id, registering cleanup.
func createNamedServer(t *testing.T, token, name string) string {
	t.Helper()
	image := firstImageID(t, token)
	body := `{"server":{"name":"` + name + `","flavorRef":"m1.small","imageRef":"` + image + `"}}`

	code, resp := createServerRaw(t, token, body)
	require.Contains(t, []int{http.StatusAccepted, http.StatusCreated, http.StatusOK}, code,
		"creating a server for the by-name test: %s", resp)

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

// T065 (FR-029, FR-030, FR-033): every action the dispatcher dispatches, addressed by
// name, must behave as it would by id — and in particular must never be a 500.
func TestNovaActionsByName_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)

	const name = "spec025-byname"
	createNamedServer(t, token, name)

	// Every entry in the serverActions dispatch table, plus the lifecycle tail. Bodies
	// are the minimal valid form for each.
	actions := []struct {
		label string
		body  string
	}{
		{"suspend", `{"suspend": null}`},
		{"resume", `{"resume": null}`},
		{"shelve", `{"shelve": null}`},
		{"shelveOffload", `{"shelveOffload": null}`},
		{"unshelve", `{"unshelve": null}`},
		{"pause", `{"pause": null}`},
		{"unpause", `{"unpause": null}`},
		{"lock", `{"lock": null}`},
		{"unlock", `{"unlock": null}`},
		{"os-stop", `{"os-stop": null}`},
		{"os-start", `{"os-start": null}`},
		{"reboot", `{"reboot": {"type": "SOFT"}}`},
		{"os-resetNetwork", `{"os-resetNetwork": null}`},
		{"changePassword", `{"changePassword": {"adminPass": "sekrit-1234"}}`},
		{"addSecurityGroup", `{"addSecurityGroup": {"name": "default"}}`},
		{"removeSecurityGroup", `{"removeSecurityGroup": {"name": "default"}}`},
		{"confirmResize", `{"confirmResize": null}`},
		{"revertResize", `{"revertResize": null}`},
		{"restore", `{"restore": null}`},
		{"migrate", `{"migrate": null}`},
		{"createBackup", `{"createBackup": {"name": "b", "backup_type": "daily", "rotation": 1}}`},
		{"createImage", `{"createImage": {"name": "spec025-snap"}}`},
		{"resize", `{"resize": {"flavorRef": "m1.small"}}`},
		{"os-resetState", `{"os-resetState": {"state": "active"}}`},
		{"os-migrateLive", `{"os-migrateLive": {"host": null}}`},
		{"evacuate", `{"evacuate": {}}`},
	}

	for _, a := range actions {
		t.Run(a.label, func(t *testing.T) {
			code, body := postAction(t, token, name, a.body)

			// The contract is narrow and deliberate: whatever the action decides, a
			// NAME must not be the reason it fails internally. A 500 here means the
			// raw reference reached a uuid-typed query.
			assert.NotEqual(t, http.StatusInternalServerError, code,
				"%s by name returned 500 — the unresolved name reached the query. body: %s", a.label, body)

			// And it must not be reported as absent: this server exists under this
			// name in this project.
			assert.NotEqual(t, http.StatusNotFound, code,
				"%s by name reported the server as absent, though it exists. body: %s", a.label, body)
		})
	}
}

// T066 (FR-030, FR-032): an unknown name is absent, never a server error.
func TestNovaActionUnknownNameIsAbsent_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)

	for _, ref := range []string{
		"spec025-no-such-server",
		"not-a-uuid-at-all",
		"11111111-2222-3333-4444-555555555555", // well-formed, absent
	} {
		t.Run(ref, func(t *testing.T) {
			code, body := postAction(t, token, ref, `{"suspend": null}`)
			assert.Equal(t, http.StatusNotFound, code,
				"an action on an unreachable reference must report it absent; body: %s", body)
			assert.NotEqual(t, http.StatusInternalServerError, code,
				"an unknown reference must never be a server error (FR-030)")
		})
	}
}
