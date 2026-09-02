package keystone

import (
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
)

// SPEC-025-followup US4: the catalog names the host the client addressed (FR-030 – FR-034).
//
// substituteURLTemplates rewrote every endpoint host from LIGHTSTACK_ENDPOINT_HOST, defaulting to
// "localhost", and never consulted the host the client used. When the configured host
// names something the client cannot resolve, EVERY command fails at catalog lookup — one
// step before the version documents and self-links SPEC-025 fixed are ever fetched. That
// is why this is the earlier of the two failures, and why fixing self-links alone left the
// fix unreachable in exactly the deployments that needed it.

// TestCatalogEchoesRequestHost is T079: FR-031. The addressed host wins over the configured
// one, and each endpoint keeps its own port.
func TestCatalogEchoesRequestHost(t *testing.T) {
	requireDB(t)
	t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "lightstack.internal")

	cat := BuildServiceCatalog(testPool, "proj1", "lightstack.example.com", nil, "")
	if len(cat) == 0 {
		t.Fatal("empty catalog")
	}
	for _, entry := range cat {
		if entry.Type == "s3" {
			continue // configured out of band; covered by TestCatalogLeavesObjectStorageAlone
		}
		for _, ep := range entry.Endpoints {
			u, err := url.Parse(ep.URL)
			if err != nil {
				t.Fatalf("%s endpoint %q is unparseable: %v", entry.Type, ep.URL, err)
			}
			if u.Hostname() != "lightstack.example.com" {
				t.Errorf("%s endpoint = %q; want the addressed host lightstack.example.com, not the configured lightstack.internal", entry.Type, ep.URL)
			}
			// Each service keeps its OWN port, from the seeded endpoint rows.
			if u.Port() == "" {
				t.Errorf("%s endpoint = %q; the endpoint's own port was lost", entry.Type, ep.URL)
			}
		}
	}
}

// TestCatalogNeverDoublesThePort is T080: the failure a naive substitution produces. A
// request host arriving with a port must not yield "host:35357:8774" — an authority
// common.hostWithPort's own comment records having emitted before.
func TestCatalogNeverDoublesThePort(t *testing.T) {
	requireDB(t)
	t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "lightstack.internal")

	for _, requestHost := range []string{"lightstack.example.com:35357", "[::1]:35357", "10.0.0.5:35357"} {
		for _, entry := range BuildServiceCatalog(testPool, "proj1", requestHost, nil, "") {
			if entry.Type == "s3" {
				continue
			}
			for _, ep := range entry.Endpoints {
				u, err := url.Parse(ep.URL)
				if err != nil {
					t.Errorf("request host %q produced unparseable %q: %v", requestHost, ep.URL, err)
					continue
				}
				if _, _, err := net.SplitHostPort(u.Host); err != nil {
					t.Errorf("request host %q produced authority %q, which is not a single host:port: %v", requestHost, u.Host, err)
				}
				if strings.Count(u.Host, ":") > 1 && !strings.HasPrefix(u.Host, "[") {
					t.Errorf("request host %q produced %q — two ports", requestHost, u.Host)
				}
			}
		}
	}
}

// TestCatalogFallsBackWhenNoHost is T082: FR-032. A request carrying no usable host still
// yields absolute, parseable endpoints — the configured host, then localhost.
func TestCatalogFallsBackWhenNoHost(t *testing.T) {
	requireDB(t)
	t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "lightstack.configured")
	for _, entry := range BuildServiceCatalog(testPool, "proj1", "", nil, "") {
		if entry.Type == "s3" {
			continue
		}
		for _, ep := range entry.Endpoints {
			u, err := url.Parse(ep.URL)
			if err != nil || u.Hostname() == "" {
				t.Fatalf("%s endpoint %q is not absolute and parseable: %v", entry.Type, ep.URL, err)
			}
			if u.Hostname() != "lightstack.configured" {
				t.Errorf("%s endpoint = %q; with no request host the configured one applies", entry.Type, ep.URL)
			}
		}
	}

	// And with neither, localhost — so an endpoint is never "http://" with no authority.
	if err := os.Unsetenv("LIGHTSTACK_ENDPOINT_HOST"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	for _, entry := range BuildServiceCatalog(testPool, "proj1", "", nil, "") {
		if entry.Type == "s3" {
			continue
		}
		for _, ep := range entry.Endpoints {
			u, err := url.Parse(ep.URL)
			if err != nil || u.Hostname() == "" {
				t.Fatalf("%s endpoint %q lost its authority entirely: %v", entry.Type, ep.URL, err)
			}
		}
	}
}

// TestCatalogLeavesObjectStorageAlone is T083: FR-033. The object-storage endpoint names an
// external service configured out of band; the host rule must not touch it.
func TestCatalogLeavesObjectStorageAlone(t *testing.T) {
	requireDB(t)
	t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "lightstack.internal")
	const rgw = "http://rgw.example.test:8080"

	for _, entry := range BuildServiceCatalog(testPool, "proj-s3", "lightstack.example.com", nil, rgw) {
		if entry.Type != "s3" {
			continue
		}
		for _, ep := range entry.Endpoints {
			if ep.URL != rgw {
				t.Errorf("s3 endpoint = %q; want the configured %q served verbatim", ep.URL, rgw)
			}
		}
	}
}

// TestCatalogHostPrecedence pins catalogHost directly — the ordering is the requirement,
// and asserting it here keeps it readable when the endpoint plumbing changes.
func TestCatalogHostPrecedence(t *testing.T) {
	t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "configured.example")
	if got := catalogHost("addressed.example:35357"); got != "addressed.example" {
		t.Errorf("catalogHost(addressed) = %q; the request host must win, with its port stripped", got)
	}
	if got := catalogHost(""); got != "configured.example" {
		t.Errorf("catalogHost(\"\") = %q; want the configured host as fallback", got)
	}
	if err := os.Unsetenv("LIGHTSTACK_ENDPOINT_HOST"); err != nil {
		t.Fatalf("unsetenv: %v", err)
	}
	if got := catalogHost(""); got != "localhost" {
		t.Errorf("catalogHost with nothing configured = %q; want localhost so a URL is always parseable", got)
	}
}

// TestCatalogCacheKeyIncludesHost is T081: FR-031a, and the security half of this story.
//
// The request host is CLIENT-SUPPLIED. Keyed on the project alone, the 24-hour catalog
// cache would let one caller's Host header decide the endpoints another caller in the same
// project receives — and therefore where that caller sends its token. This asserts the two
// hosts produce different keys; the cache itself is off by default, which bounds the
// exposure without removing the requirement.
func TestCatalogCacheKeyIncludesHost(t *testing.T) {
	requireDB(t)
	t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "lightstack.internal")

	first := BuildServiceCatalog(testPool, "shared-proj", "alice.example.com", nil, "")
	second := BuildServiceCatalog(testPool, "shared-proj", "bob.example.com", nil, "")

	hostOf := func(cat []CatalogEntry) string {
		for _, e := range cat {
			if e.Type == "s3" {
				continue
			}
			for _, ep := range e.Endpoints {
				if u, err := url.Parse(ep.URL); err == nil {
					return u.Hostname()
				}
			}
		}
		return ""
	}
	if h := hostOf(first); h != "alice.example.com" {
		t.Errorf("first caller got %q; want its own host", h)
	}
	if h := hostOf(second); h != "bob.example.com" {
		t.Errorf("second caller got %q; want its own host, NOT the first caller's — a shared cache key is a token-exfiltration path", h)
	}
}
