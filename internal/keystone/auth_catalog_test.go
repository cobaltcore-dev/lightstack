package keystone

import (
	"os"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/common"
)

func setSchemeEnv(t *testing.T, scheme string) {
	t.Helper()
	for _, k := range []string{"LIGHTSTACK_ENDPOINT_SCHEME", "LIGHTSTACK_ENDPOINT_HOST"} {
		v, had := os.LookupEnv(k)
		k := k
		t.Cleanup(func() {
			if had {
				os.Setenv(k, v)
			} else {
				os.Unsetenv(k)
			}
		})
	}
	os.Setenv("LIGHTSTACK_ENDPOINT_HOST", "lightstack.example.com")
	if scheme == "" {
		os.Unsetenv("LIGHTSTACK_ENDPOINT_SCHEME")
	} else {
		os.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", scheme)
	}
}

// TestHardcodedCatalogScheme: the fallback catalog honors the scheme on every
// URL, and the structure is identical between http and https except the scheme
// (FR-010/011/013, SC-006). No DB required.
func TestHardcodedCatalogScheme(t *testing.T) {
	setSchemeEnv(t, "")
	httpCat := buildHardcodedCatalog("proj1", "")
	setSchemeEnv(t, "https")
	httpsCat := buildHardcodedCatalog("proj1", "")

	for _, e := range httpsCat {
		for _, ep := range e.Endpoints {
			if !strings.HasPrefix(ep.URL, "https://") {
				t.Errorf("https config: %s/%s URL not https: %s", e.Type, ep.Interface, ep.URL)
			}
		}
	}
	for _, e := range httpCat {
		for _, ep := range e.Endpoints {
			if !strings.HasPrefix(ep.URL, "http://") {
				t.Errorf("default config: %s/%s URL not http: %s", e.Type, ep.Interface, ep.URL)
			}
		}
	}

	// Structure identical except the URL scheme.
	if len(httpCat) != len(httpsCat) {
		t.Fatalf("service count differs: http=%d https=%d", len(httpCat), len(httpsCat))
	}
	for i := range httpCat {
		if httpCat[i].Type != httpsCat[i].Type || httpCat[i].Name != httpsCat[i].Name {
			t.Fatalf("service[%d] differs: %+v vs %+v", i, httpCat[i], httpsCat[i])
		}
		if len(httpCat[i].Endpoints) != len(httpsCat[i].Endpoints) {
			t.Fatalf("endpoint count differs for %s", httpCat[i].Type)
		}
		for j := range httpCat[i].Endpoints {
			a, b := httpCat[i].Endpoints[j], httpsCat[i].Endpoints[j]
			if a.Interface != b.Interface || a.Region != b.Region {
				t.Errorf("endpoint meta differs: %+v vs %+v", a, b)
			}
			// URLs must match once the scheme is normalized away.
			if common.ForceScheme(a.URL, "http") != common.ForceScheme(b.URL, "http") {
				t.Errorf("endpoint URL differs beyond scheme: %s vs %s", a.URL, b.URL)
			}
		}
	}
}

// TestBuildServiceCatalogScheme: the DB-backed catalog applies the scheme to
// every advertised endpoint URL (FR-010). DB required.
func TestBuildServiceCatalogScheme(t *testing.T) {
	requireDB(t)
	setSchemeEnv(t, "https")
	cat := BuildServiceCatalog("proj1", nil, "")
	if len(cat) == 0 {
		t.Fatal("empty catalog")
	}
	for _, e := range cat {
		for _, ep := range e.Endpoints {
			if !strings.HasPrefix(ep.URL, "https://") {
				t.Errorf("%s/%s URL not https: %s", e.Type, ep.Interface, ep.URL)
			}
		}
	}
}

// TestBuildServiceCatalogS3EndpointFromConfig: the DB-backed catalog must advertise
// the resolved RGW endpoint (cfg.Glance.S3Endpoint, wired via SetGlanceS3Endpoint —
// the Helm chart populates it from YAML, NOT the env), never migration 062's seeded
// http://localhost:7480 placeholder (PR #73 r3558646680). DB required — the harness
// migrates 062, which seeds the s3 service+endpoint.
func TestBuildServiceCatalogS3EndpointFromConfig(t *testing.T) {
	requireDB(t)
	const rgw = "http://rgw.example.test:8080"
	t.Setenv("LIGHTSTACK_GLANCE_S3_ENDPOINT", "") // Helm-style: value comes from config, not env

	var got string
	for _, e := range BuildServiceCatalog("proj-s3", nil, rgw) { // nil cache => fresh; rgw = cfg value
		if e.Type != "s3" {
			continue
		}
		for _, ep := range e.Endpoints {
			if ep.Interface == "public" {
				got = ep.URL
			}
		}
	}
	if got != rgw {
		t.Fatalf("s3 catalog endpoint = %q, want %q (seeded localhost:7480 must be overridden by the config value)", got, rgw)
	}
}

// TestBuildServiceCatalogOmitsS3WhenUnconfigured: with RGW unconfigured, the
// DB-backed catalog must omit the `s3` service entirely — migration 062's seeded
// placeholder (http://localhost:7480) must never reach a client, on any service
// (PR #73 review r3710751586). DB required: the harness migrates 062, which seeds it.
func TestBuildServiceCatalogOmitsS3WhenUnconfigured(t *testing.T) {
	requireDB(t)
	t.Setenv("LIGHTSTACK_GLANCE_S3_ENDPOINT", "") // unconfigured deployment

	cat := BuildServiceCatalog("proj-unconf", nil, "") // "" = cfg.Glance.S3Endpoint unset
	if len(cat) == 0 {
		t.Fatal("empty catalog")
	}
	for _, e := range cat {
		if e.Type == "s3" {
			t.Errorf("unconfigured: catalog must omit the s3 service, got %+v", e)
		}
		for _, ep := range e.Endpoints {
			if strings.Contains(ep.URL, "localhost:7480") {
				t.Errorf("%s/%s advertises migration 062's placeholder: %s", e.Type, ep.Interface, ep.URL)
			}
		}
	}
}

// TestHardcodedCatalogS3FromConfig: the fallback catalog advertises `s3` only when
// the resolved config endpoint is set, and with that URL (no DB, no env).
func TestHardcodedCatalogS3FromConfig(t *testing.T) {
	s3URL := func(cat []CatalogEntry) (string, bool) {
		for _, e := range cat {
			if e.Type == "s3" && len(e.Endpoints) > 0 {
				return e.Endpoints[0].URL, true
			}
		}
		return "", false
	}

	if url, ok := s3URL(buildHardcodedCatalog("p", "http://rgw.example.test:8080")); !ok || url != "http://rgw.example.test:8080" {
		t.Fatalf("configured: fallback s3 endpoint = %q ok=%v; want the RGW URL", url, ok)
	}
	if _, ok := s3URL(buildHardcodedCatalog("p", "")); ok {
		t.Fatal("unconfigured: fallback must not advertise an s3 endpoint")
	}
}

// TestSubstituteURLTemplatesRewritesLocalhost: with a configured endpoint host,
// substitution rewrites a localhost-seeded URL (the cinder `volume` alias …015,
// migration 054) to that host — not just the `http://lightstack:` prefix (F5, FR-006/007).
// No DB required — substituteURLTemplates is pure.
func TestSubstituteURLTemplatesRewritesLocalhost(t *testing.T) {
	setSchemeEnv(t, "") // LIGHTSTACK_ENDPOINT_HOST=lightstack.example.com, scheme unset (http)

	got := substituteURLTemplates("http://localhost:8776/v3", "proj1")
	if strings.Contains(got, "localhost") {
		t.Errorf("localhost not rewritten: %q", got)
	}
	if !strings.Contains(got, "lightstack.example.com:8776") {
		t.Errorf("expected configured host, got %q", got)
	}

	// The existing http://lightstack: rewrite still works (no regression).
	if got := substituteURLTemplates("http://lightstack:9696", "p"); !strings.Contains(got, "lightstack.example.com:9696") {
		t.Errorf("lightstack prefix rewrite regressed: %q", got)
	}
}

// TestSubstituteURLTemplatesLocalhostUnsetNoop: with no endpoint host configured
// (dev/CI default), a localhost URL stays localhost — the fix must not break the
// local case (F5, FR edge case).
func TestSubstituteURLTemplatesLocalhostUnsetNoop(t *testing.T) {
	for _, k := range []string{"LIGHTSTACK_ENDPOINT_HOST", "LIGHTSTACK_ENDPOINT_SCHEME"} {
		v, had := os.LookupEnv(k)
		k := k
		t.Cleanup(func() {
			if had {
				os.Setenv(k, v)
			} else {
				os.Unsetenv(k)
			}
		})
		os.Unsetenv(k)
	}

	got := substituteURLTemplates("http://localhost:8776/v3", "p")
	if !strings.Contains(got, "localhost:8776") {
		t.Errorf("localhost must be preserved when host unset, got %q", got)
	}
}
