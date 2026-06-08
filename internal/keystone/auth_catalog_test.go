package keystone

import (
	"os"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/common"
)

func setSchemeEnv(t *testing.T, scheme string) {
	t.Helper()
	for _, k := range []string{"O3K_ENDPOINT_SCHEME", "O3K_ENDPOINT_HOST"} {
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
	os.Setenv("O3K_ENDPOINT_HOST", "o3k.example.com")
	if scheme == "" {
		os.Unsetenv("O3K_ENDPOINT_SCHEME")
	} else {
		os.Setenv("O3K_ENDPOINT_SCHEME", scheme)
	}
}

// TestHardcodedCatalogScheme: the fallback catalog honors the scheme on every
// URL, and the structure is identical between http and https except the scheme
// (FR-010/011/013, SC-006). No DB required.
func TestHardcodedCatalogScheme(t *testing.T) {
	setSchemeEnv(t, "")
	httpCat := buildHardcodedCatalog("proj1")
	setSchemeEnv(t, "https")
	httpsCat := buildHardcodedCatalog("proj1")

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
	cat := BuildServiceCatalog("proj1", nil)
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
