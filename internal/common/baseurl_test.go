package common

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func ginCtx(useTLS bool, host string) *gin.Context {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = host
	if useTLS {
		req.TLS = &tls.ConnectionState{}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	return c
}

// saveEnv restores an environment variable to its prior state.
func saveEnv(t *testing.T, k string) {
	t.Helper()
	v, had := os.LookupEnv(k)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(k, v)
		} else {
			_ = os.Unsetenv(k)
		}
	})
}

// TestBaseURL pins the precedence rule of SPEC-025 FR-002..FR-005: the host the
// client actually addressed wins, and LIGHTSTACK_ENDPOINT_HOST is only the fallback for a
// request that carries no usable host.
//
// This REVERSES the behavior this test previously asserted. The old cases passed the
// request host as the literal string "ignored:1" — the configured host outranked it
// unconditionally, which is precisely why keystoneauth discarded
// OS_*_ENDPOINT_OVERRIDE (cloud-storage#547 B3).
func TestBaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	saveEnv(t, "LIGHTSTACK_ENDPOINT_HOST")
	saveEnv(t, "LIGHTSTACK_ENDPOINT_SCHEME")

	tests := []struct {
		name       string
		host       string // LIGHTSTACK_ENDPOINT_HOST ("" = unset)
		scheme     string // LIGHTSTACK_ENDPOINT_SCHEME ("" = unset)
		useTLS     bool
		reqHost    string
		port       int
		want       string
		requiredBy string
	}{
		// --- FR-002: the request host outranks the configured host ---
		{
			name: "request host wins over configured host", host: "lightstack.example.com", scheme: "https",
			reqHost: "client.chose.me:9292", port: 9292,
			want: "https://client.chose.me:9292", requiredBy: "FR-002",
		},
		{
			name: "request host wins with default scheme", host: "lightstack.example.com",
			reqHost: "client.chose.me:9292", port: 9292,
			want: "http://client.chose.me:9292", requiredBy: "FR-002",
		},

		// --- FR-003: the configured host is the fallback, not the override ---
		{
			name: "configured host used when the request carries none", host: "lightstack.example.com", scheme: "https",
			reqHost: "", port: 9292,
			want: "https://lightstack.example.com:9292", requiredBy: "FR-003",
		},
		{
			name:    "no configured host and no request host still yields an absolute URL",
			reqHost: "", port: 8774,
			want: "http://localhost:8774", requiredBy: "FR-003, FR-007",
		},

		// --- FR-004: the port argument applies in BOTH branches ---
		// It names the port the TARGET service is reachable on, so it replaces
		// whatever port the source host carried. Lightstack is port-per-service: the chart's
		// own ingress template records that each service has its own port, so there is
		// no path-routed topology for this to break.
		{
			name:    "port applied to the request host when it carries none",
			reqHost: "lightstack.example.com", port: 8774,
			want: "http://lightstack.example.com:8774", requiredBy: "FR-004",
		},
		{
			name:    "port replaces the request host's own port for a cross-service link",
			reqHost: "lightstack.example.com:8774", port: 6080,
			want: "http://lightstack.example.com:6080", requiredBy: "FR-004, FR-009",
		},
		{
			name: "port applied to the configured host too", host: "lightstack.example.com",
			reqHost: "", port: 9696,
			want: "http://lightstack.example.com:9696", requiredBy: "FR-004",
		},
		{
			name: "configured host that already names a port does not gain a second one", host: "lightstack.example.com:1234",
			reqHost: "", port: 9696,
			want: "http://lightstack.example.com:9696", requiredBy: "FR-004",
		},

		// --- port 0 means "whatever the source said": preserved behavior ---
		{
			name: "port zero keeps the request host verbatim", useTLS: true,
			reqHost: "host:443", port: 0,
			want: "https://host:443", requiredBy: "A-006 (no unrequested change)",
		},

		// --- FR-005: scheme ---
		{
			name: "TLS request auto-detects https", useTLS: true,
			reqHost: "host", port: 8774,
			want: "https://host:8774", requiredBy: "FR-005",
		},
		{
			name: "configured scheme forces https on a plain request", scheme: "https",
			reqHost: "host:80", port: 0,
			want: "https://host:80", requiredBy: "FR-005",
		},
		{
			name: "TLS terminated ahead of the service still advertises https", scheme: "https",
			reqHost: "lightstack.example.com", port: 8774,
			want: "https://lightstack.example.com:8774", requiredBy: "FR-005",
		},

		// --- IPv6 must not be mangled ---
		{
			name:    "IPv6 literal with a port keeps its brackets",
			reqHost: "[::1]:8774", port: 9292,
			want: "http://[::1]:9292", requiredBy: "FR-007",
		},
		{
			name:    "IPv6 literal without a port gains one inside its brackets",
			reqHost: "[fe80::1]", port: 9292,
			want: "http://[fe80::1]:9292", requiredBy: "FR-007",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.host != "" {
				_ = os.Setenv("LIGHTSTACK_ENDPOINT_HOST", tc.host)
			} else {
				_ = os.Unsetenv("LIGHTSTACK_ENDPOINT_HOST")
			}
			if tc.scheme != "" {
				_ = os.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", tc.scheme)
			} else {
				_ = os.Unsetenv("LIGHTSTACK_ENDPOINT_SCHEME")
			}

			if got := BaseURL(ginCtx(tc.useTLS, tc.reqHost), tc.port); got != tc.want {
				t.Errorf("BaseURL() = %q, want %q (required by %s)", got, tc.want, tc.requiredBy)
			}
		})
	}
}

// TestBaseURLAlwaysAbsolute is the FR-007 guard: whatever the inputs, the result is
// parseable as an absolute URL with a scheme and a host. No caller may ever emit a
// relative or scheme-less self-link.
func TestBaseURLAlwaysAbsolute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	saveEnv(t, "LIGHTSTACK_ENDPOINT_HOST")
	saveEnv(t, "LIGHTSTACK_ENDPOINT_SCHEME")

	for _, envHost := range []string{"", "lightstack.example.com"} {
		for _, reqHost := range []string{"", "h", "h:1", "[::1]:1"} {
			for _, port := range []int{0, 8774} {
				if envHost != "" {
					_ = os.Setenv("LIGHTSTACK_ENDPOINT_HOST", envHost)
				} else {
					_ = os.Unsetenv("LIGHTSTACK_ENDPOINT_HOST")
				}
				got := BaseURL(ginCtx(false, reqHost), port)
				u, err := url.Parse(got)
				if err != nil {
					t.Fatalf("BaseURL(host=%q req=%q port=%d) = %q: not a URL: %v", envHost, reqHost, port, got, err)
				}
				if u.Scheme == "" || u.Host == "" {
					t.Errorf("BaseURL(host=%q req=%q port=%d) = %q: not absolute", envHost, reqHost, port, got)
				}
			}
		}
	}
}
