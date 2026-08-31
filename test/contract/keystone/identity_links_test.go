package keystone_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/gophercloud/gophercloud"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-025 US2 (cloud-storage#547 B16): identity resources must carry links.self.
//
// python-openstackclient 6.x does an unconditional `_info.pop('links')` before
// rendering, so a response without the field aborts the command with a bare 'links'
// and exit 1. `project show`, `user show` and `role show` are all affected. A 10.x
// client tolerates the omission, which is why whether Lightstack appears functional depends
// on which client happens to be first on PATH — and why these tests assert the
// RESPONSE SHAPE rather than shelling out to a client whose version we cannot pin.
//
// It bites this project's own runbooks too: SPEC-023's real-mode fixtures read
// identifiers from these very commands, and on a 6.x client they come back empty
// rather than failing, so every later step silently tests the wrong thing.

// getRaw fetches a URL with the client's token and decodes it into a generic map, so
// fields gophercloud's typed structs discard are still visible. `links` is exactly
// such a field.
func getRaw(t *testing.T, client *gophercloud.ServiceClient, url string) map[string]any {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s -> %s", url, string(body))

	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out), "decoding %s", url)
	return out
}

// assertSelfLink checks the one thing the 6.x client requires and the one thing
// FR-007 requires: the field is present, and it is an absolute URL.
func assertSelfLink(t *testing.T, obj map[string]any, what string) {
	t.Helper()

	rawLinks, ok := obj["links"]
	require.True(t, ok, "%s has no links field — this is what aborts the 6.x client (B16)", what)

	links, ok := rawLinks.(map[string]any)
	require.True(t, ok, "%s links is not an object: %#v", what, rawLinks)

	self, ok := links["self"].(string)
	require.True(t, ok, "%s links has no self: %#v", what, links)
	require.NotEmpty(t, self, "%s links.self is empty", what)

	u, err := url.Parse(self)
	require.NoError(t, err, "%s links.self %q does not parse", what, self)
	assert.NotEmpty(t, u.Scheme, "%s links.self %q is scheme-less — not a resolvable URL (FR-007)", what, self)
	assert.NotEmpty(t, u.Host, "%s links.self %q is relative", what, self)
}

// T023 (FR-010, FR-012): the single-resource reads.
func TestKeystoneSingleResourceLinks_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := setupKeystoneClient(t)

	t.Run("project", func(t *testing.T) {
		list := getRaw(t, client, client.ServiceURL("projects"))
		projects := list["projects"].([]any)
		require.NotEmpty(t, projects, "no projects to read")
		id := projects[0].(map[string]any)["id"].(string)

		got := getRaw(t, client, client.ServiceURL("projects", id))
		assertSelfLink(t, got["project"].(map[string]any), "GET /v3/projects/{id}")
	})

	t.Run("user", func(t *testing.T) {
		list := getRaw(t, client, client.ServiceURL("users"))
		users := list["users"].([]any)
		require.NotEmpty(t, users, "no users to read")
		id := users[0].(map[string]any)["id"].(string)

		got := getRaw(t, client, client.ServiceURL("users", id))
		assertSelfLink(t, got["user"].(map[string]any), "GET /v3/users/{id}")
	})

	t.Run("role", func(t *testing.T) {
		list := getRaw(t, client, client.ServiceURL("roles"))
		roles := list["roles"].([]any)
		require.NotEmpty(t, roles, "no roles to read")
		id := roles[0].(map[string]any)["id"].(string)

		got := getRaw(t, client, client.ServiceURL("roles", id))
		assertSelfLink(t, got["role"].(map[string]any), "GET /v3/roles/{id}")
	})
}

// T024 (FR-011): the COLLECTION entries.
//
// This is the path that actually breaks `<resource> show <name>`: the client resolves
// a name by listing first, then pops links from the object it found there. A link on
// the single-resource read alone does not save it — which is why `role show <name>`
// failed even though GetRole already emitted one.
func TestKeystoneCollectionEntryLinks_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := setupKeystoneClient(t)

	for _, tc := range []struct{ name, path, key string }{
		{"projects", "projects", "projects"},
		{"users", "users", "users"},
		{"roles", "roles", "roles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := getRaw(t, client, client.ServiceURL(tc.path))
			entries, ok := list[tc.key].([]any)
			require.True(t, ok, "%s is not a list: %#v", tc.key, list[tc.key])
			require.NotEmpty(t, entries, "no %s returned", tc.key)

			for i, e := range entries {
				obj, ok := e.(map[string]any)
				require.True(t, ok, "entry %d is not an object", i)
				assertSelfLink(t, obj, "GET /v3/"+tc.path+" entry "+tc.name)
			}
		})
	}
}

// FR-013: adding a link must not disturb anything else. The nullable fields are named
// explicitly because they are rendered through sql.NullString and would change shape
// for every client if that rendering were touched.
func TestKeystoneLinksDoNotDisturbOtherFields_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := setupKeystoneClient(t)

	list := getRaw(t, client, client.ServiceURL("projects"))
	projects := list["projects"].([]any)
	require.NotEmpty(t, projects)

	for _, p := range projects {
		obj := p.(map[string]any)
		for _, field := range []string{"id", "name", "enabled"} {
			assert.Contains(t, obj, field, "project entry lost the %q field", field)
		}
		// Nullable columns must still render as plain JSON strings, never as
		// {"String":"","Valid":false} — the shape a raw sql.NullString would emit.
		for _, field := range []string{"description", "domain_id"} {
			if v, ok := obj[field]; ok {
				_, isString := v.(string)
				assert.True(t, isString || v == nil,
					"project %q is %#v; nullable fields must render as a string or null", field, v)
			}
		}
	}
}
