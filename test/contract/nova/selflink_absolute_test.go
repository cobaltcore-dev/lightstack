package nova_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-025 FR-007: every self-link the product emits must be an absolute, resolvable
// URL. This walks real responses rather than asserting against a fixture, because the
// defect class it guards is precisely a link that looks fine in code and is
// unresolvable on the wire — a relative path, a missing scheme, or a hostname that
// exists only inside a compose file.
//
// The unit-level counterpart is TestNoHardcodedSelfLinkConstruction in
// internal/common, which catches the same class at the source rather than in a
// response.
func TestNovaSelfLinksAreAbsolute_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	token := issueToken(t)
	base := novaRootURL(t)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"flavor list", "/v2.1/flavors"},
		{"flavor list detail", "/v2.1/flavors/detail"},
		{"server list", "/v2.1/servers"},
		{"server list detail", "/v2.1/servers/detail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, base+tc.path, nil)
			require.NoError(t, err)
			req.Header.Set("X-Auth-Token", token)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", tc.path)

			var body any
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

			hrefs := collectHRefs(body)
			for _, h := range hrefs {
				u, err := url.Parse(h)
				if !assert.NoError(t, err, "href %q does not parse", h) {
					continue
				}
				assert.NotEmpty(t, u.Scheme, "href %q is scheme-less (GET %s)", h, tc.path)
				assert.NotEmpty(t, u.Host, "href %q is relative (GET %s)", h, tc.path)
				assert.NotEqual(t, "lightstack", u.Hostname(),
					"href %q names the compose-file hostname, unresolvable for any real client", h)
			}
		})
	}
}

// collectHRefs walks a decoded JSON body and returns every "href" string value,
// wherever it is nested. Written generically so a new link-bearing field is covered
// the day it appears rather than the day someone remembers to extend this test.
func collectHRefs(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "href" {
				if s, ok := val.(string); ok {
					out = append(out, s)
				}
				continue
			}
			// links may also be an object of {"self": "<url>"} rather than a list
			// of {"rel","href"} — the identity service uses that shape.
			if k == "links" {
				if m, ok := val.(map[string]any); ok {
					for _, s := range m {
						if str, ok := s.(string); ok {
							out = append(out, str)
						}
					}
					continue
				}
			}
			out = append(out, collectHRefs(val)...)
		}
	case []any:
		for _, item := range t {
			out = append(out, collectHRefs(item)...)
		}
	}
	return out
}

// issueToken returns a scoped token by reusing the same authenticated client every
// other test in this package builds. Hand-rolling the auth request duplicated a body
// shape that is easy to get subtly wrong — and did.
func issueToken(t *testing.T) string {
	t.Helper()
	client := setupNovaClient(t)
	require.NotEmpty(t, client.TokenID, "authenticated client carries no token")
	return client.TokenID
}
