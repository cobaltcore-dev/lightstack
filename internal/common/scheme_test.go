package common

import (
	"os"
	"testing"
)

func TestEndpointScheme(t *testing.T) {
	orig, had := os.LookupEnv("LIGHTSTACK_ENDPOINT_SCHEME")
	t.Cleanup(func() {
		if had {
			os.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", orig)
		} else {
			os.Unsetenv("LIGHTSTACK_ENDPOINT_SCHEME")
		}
	})

	cases := []struct{ in, want string }{
		{"http", "http"},
		{"https", "https"},
		{"HTTPS", "https"},     // case-insensitive
		{"  https  ", "https"}, // trimmed
		{"HtTp", "http"},
		{"ftp", "http"},        // unsupported → default http
		{"gopher://x", "http"}, // unsupported → default http
	}
	for _, c := range cases {
		os.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", c.in)
		if got := EndpointScheme(); got != c.want {
			t.Errorf("EndpointScheme(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	os.Unsetenv("LIGHTSTACK_ENDPOINT_SCHEME")
	if got := EndpointScheme(); got != "http" {
		t.Errorf("EndpointScheme(unset) = %q, want http", got)
	}
}

func TestForceScheme(t *testing.T) {
	cases := []struct{ url, scheme, want string }{
		{"http://lightstack:9292/v2", "https", "https://lightstack:9292/v2"},
		{"http://host:35357/v3", "http", "http://host:35357/v3"},
		{"https://host:8774", "http", "http://host:8774"},
		{"https://host:8774", "https", "https://host:8774"},
		{"ftp://x", "https", "ftp://x"}, // unrecognized leading scheme → unchanged
		{"relative/path", "https", "relative/path"},
	}
	for _, c := range cases {
		if got := ForceScheme(c.url, c.scheme); got != c.want {
			t.Errorf("ForceScheme(%q, %q) = %q, want %q", c.url, c.scheme, got, c.want)
		}
	}
}
