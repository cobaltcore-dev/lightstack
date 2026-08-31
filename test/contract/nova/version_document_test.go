package nova_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// novaRootURL is the compute service's origin, without a version path. Version
// documents live above the versioned prefix and are unauthenticated, so they are
// fetched directly rather than through a gophercloud service client.
func novaRootURL(t *testing.T) string {
	t.Helper()
	return strings.TrimSuffix(getEnvOrDefault("LIGHTSTACK_NOVA_URL", "http://localhost:8774"), "/")
}

// SPEC-025 US1 (cloud-storage#547 B4): the compute version document must carry a
// links array with a self href.
//
// Without it python-openstackclient aborts inside its own library — `openstack flavor
// list` dies with "list index out of range" — unless the caller happens to know it
// must pin OS_COMPUTE_API_VERSION. The version LIST at "/" has always had links; the
// version DETAIL at "/v2.1", which is what the client fetches during discovery, did
// not.
//
// Driven with a raw HTTP GET rather than gophercloud: this endpoint is unauthenticated
// and is read BEFORE a client has a token, which is exactly the situation the defect
// occurs in.
func TestNovaVersionDocumentHasSelfLink_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	base := novaRootURL(t)
	resp, err := http.Get(base + "/v2.1")
	require.NoError(t, err, "GET /v2.1")
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var doc struct {
		Version struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			Version    string `json:"version"`
			MinVersion string `json:"min_version"`
			Links      []struct {
				Rel  string `json:"rel"`
				HRef string `json:"href"`
			} `json:"links"`
		} `json:"version"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))

	assert.Equal(t, "v2.1", doc.Version.ID)
	require.NotEmpty(t, doc.Version.Links,
		"version.links is absent — this is what makes `openstack flavor list` abort inside the client (B4)")

	var self string
	for _, l := range doc.Version.Links {
		if l.Rel == "self" {
			self = l.HRef
		}
	}
	require.NotEmpty(t, self, `no link with rel "self"`)

	u, err := url.Parse(self)
	require.NoError(t, err, "self href %q does not parse", self)
	assert.NotEmpty(t, u.Scheme, "self href %q is scheme-less, so it is not resolvable", self)
	assert.NotEmpty(t, u.Host, "self href %q has no host, so it is relative", self)
	assert.True(t, strings.HasSuffix(u.Path, "/v2.1"),
		"self href %q should point at the version it describes", self)
}

// The version list at "/" already carried links; assert it still does and that its
// href obeys the same rule, so the two documents cannot drift apart.
func TestNovaVersionListSelfLinkIsAbsolute_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	resp, err := http.Get(novaRootURL(t) + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var doc struct {
		Versions []struct {
			ID    string `json:"id"`
			Links []struct {
				Rel  string `json:"rel"`
				HRef string `json:"href"`
			} `json:"links"`
		} `json:"versions"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	require.NotEmpty(t, doc.Versions)

	for _, v := range doc.Versions {
		require.NotEmpty(t, v.Links, "version %s has no links", v.ID)
		for _, l := range v.Links {
			u, err := url.Parse(l.HRef)
			require.NoError(t, err)
			assert.NotEmpty(t, u.Scheme, "version %s link %q is scheme-less", v.ID, l.HRef)
			assert.NotEmpty(t, u.Host, "version %s link %q is relative", v.ID, l.HRef)
		}
	}
}

// A self-link must name the host the CLIENT addressed, not one the deployment
// configures for itself (FR-002). This is the half of B3 a client can observe: when
// the advertised host is substituted, keystoneauth discards OS_*_ENDPOINT_OVERRIDE and
// then fails against a host the caller never named.
func TestNovaVersionDocumentEchoesRequestHost_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	req, err := http.NewRequest(http.MethodGet, novaRootURL(t)+"/v2.1", nil)
	require.NoError(t, err)
	// A name this deployment cannot have configured for itself. Go sends Host as
	// given, so the service sees exactly this.
	const addressedAs = "client.chose.me:8774"
	req.Host = addressedAs

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var doc struct {
		Version struct {
			Links []struct {
				Rel  string `json:"rel"`
				HRef string `json:"href"`
			} `json:"links"`
		} `json:"version"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))

	// Found first and required: a document whose self link is missing or renamed must fail
	// here, not pass with nothing compared. Upstream's carries exactly one, beside describedby.
	var self []string
	for _, l := range doc.Version.Links {
		if l.Rel == "self" {
			self = append(self, l.HRef)
		}
	}
	require.Len(t, self, 1, "the version document must carry exactly one self link: %+v", doc.Version.Links)

	u, err := url.Parse(self[0])
	require.NoError(t, err)
	assert.Equal(t, addressedAs, u.Host,
		"self href %q does not name the host the client addressed (%s)", self[0], addressedAs)
}
