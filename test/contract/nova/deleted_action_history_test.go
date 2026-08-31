package nova_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-025 US7 / FR-043 (cloud-storage#547 B7): a DELETED server's action history stays
// readable by its owner and by an administrator.
//
// The retention decision keeps the records past the server. The read path resolved the
// server first and answered *absent* once it was gone — to everyone, administrators
// included — so the records were kept and never served. Upstream exposes them from
// microversion 2.21, "restricted to administrative users or the owner of the server".

// getActionHistory fetches os-instance-actions with an optional microversion.
func getActionHistory(t *testing.T, token, id, microversion string) (int, []map[string]any) {
	t.Helper()
	status, body := getActionHistoryRaw(t, token, id, microversion)
	if status != http.StatusOK {
		return status, nil
	}
	var out struct {
		InstanceActions []map[string]any `json:"instanceActions"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	return status, out.InstanceActions
}

// getActionHistoryRaw is getActionHistory's request, answering with the status and the body as
// sent — what two answers must agree on to be indistinguishable.
func getActionHistoryRaw(t *testing.T, token, id, microversion string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		novaRootURL(t)+"/v2.1/servers/"+id+"/os-instance-actions", nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	if microversion != "" {
		req.Header.Set("OpenStack-API-Version", "compute "+microversion)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// deleteServer removes a server and returns once it is gone.
func deleteServer(t *testing.T, token, id string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, novaRootURL(t)+"/v2.1/servers/"+id, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Contains(t, []int{http.StatusNoContent, http.StatusAccepted, http.StatusOK}, resp.StatusCode)
}

// createAndActOn boots a server, performs one action so there is history, and deletes it.
func createAndActOn(t *testing.T, token, name string) string {
	t.Helper()
	id := createNamedServer(t, token, name)

	// One action beyond creation, so the history has something a reader would want.
	code, body := postAction(t, token, id, `{"lock": null}`)
	require.NotEqual(t, http.StatusInternalServerError, code, "seeding an action: %s", body)

	// The history must exist BEFORE deletion, or this test proves nothing about
	// retention.
	status, actions := getActionHistory(t, token, id, "2.21")
	require.Equal(t, http.StatusOK, status)
	require.NotEmpty(t, actions, "no action history recorded for a live server")

	deleteServer(t, token, id)
	return id
}

// T079 (FR-043, FR-043a): the owner still gets the history after deletion.
func TestNovaDeletedServerHistoryReadableByOwner_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)

	id := createAndActOn(t, token, "spec025-hist-owner")

	status, actions := getActionHistory(t, token, id, "2.21")
	assert.Equal(t, http.StatusOK, status,
		"a deleted server's history must still be readable by its owner (FR-043)")
	assert.NotEmpty(t, actions,
		"the history came back empty; retention keeps records that must also be served")

	// Every record must still name the owning project — that is what authorizes the
	// read now the server row is gone (FR-043a).
	for _, a := range actions {
		assert.NotEmpty(t, a["project_id"], "an action record carries no project; nothing could authorize it")
	}
}

// T081 (research.md R7): below microversion 2.21 the history of a deleted server is NOT
// served, because upstream does not serve it there. An absent header means "no
// negotiation" and gets the advertised current version.
func TestNovaDeletedServerHistoryIsMicroversionGated_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)

	id := createAndActOn(t, token, "spec025-hist-microversion")

	t.Run("2.1 does not get it", func(t *testing.T) {
		status, _ := getActionHistory(t, token, id, "2.1")
		assert.Equal(t, http.StatusNotFound, status,
			"a client that negotiated 2.1 must not receive a deleted server's history — "+
				"granting more than was negotiated is its own incompatibility")
	})

	t.Run("2.21 gets it", func(t *testing.T) {
		status, actions := getActionHistory(t, token, id, "2.21")
		assert.Equal(t, http.StatusOK, status)
		assert.NotEmpty(t, actions)
	})

	t.Run("no header is treated as the advertised current version", func(t *testing.T) {
		status, actions := getActionHistory(t, token, id, "")
		assert.Equal(t, http.StatusOK, status,
			"a client that does not negotiate gets the product's full behaviour, as every existing caller already does")
		assert.NotEmpty(t, actions)
	})

	t.Run("latest gets it", func(t *testing.T) {
		status, _ := getActionHistory(t, token, id, "latest")
		assert.Equal(t, http.StatusOK, status, "\"latest\" is upstream's spelling for the current version")
	})
}

// T080 (FR-043b): a caller who is neither owner nor administrator gets the same *absent*
// answer as for a server that never existed. Telling the two apart would confirm another
// project's server had existed.
func TestNovaDeletedServerHistoryHiddenFromOthers_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	adminProvider, _ := setupAdminClient(t)
	token := issueToken(t)

	id := createAndActOn(t, token, "spec025-hist-hidden")

	// A server id that never existed, for comparison.
	const neverExisted = "dddddddd-eeee-ffff-1111-222222222222"

	// The owner reads the deleted server's history; nobody reads one that never existed.
	ownerStatus, _ := getActionHistoryRaw(t, token, id, "2.21")
	assert.Equal(t, http.StatusOK, ownerStatus)
	absentStatus, _ := getActionHistoryRaw(t, token, neverExisted, "2.21")
	assert.Equal(t, http.StatusNotFound, absentStatus, "a server that never existed must be reported absent")

	// Another project — a member there, not an administrator, who may read any — must get
	// exactly the answer it gets for a server that never existed, status and body alike.
	other := memberOfAFreshProject(t, adminProvider)
	requireTokenLacksAdmin(t, other)
	deletedStatus, deletedBody := getActionHistoryRaw(t, other, id, "2.21")
	neverStatus, neverBody := getActionHistoryRaw(t, other, neverExisted, "2.21")
	assert.Equal(t, http.StatusNotFound, deletedStatus,
		"another project read a deleted server's history; body: %s", deletedBody)
	assert.Equal(t, neverStatus, deletedStatus,
		"another project's deleted server and one that never existed must answer alike")
	assert.Equal(t, strings.ReplaceAll(neverBody, neverExisted, "<id>"), strings.ReplaceAll(deletedBody, id, "<id>"),
		"another project's deleted server must be indistinguishable from one that never existed")
}
