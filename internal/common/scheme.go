package common

import (
	"os"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
)

// endpointSchemeEnv selects the scheme advertised in the Keystone service
// catalog and in resource self-links (SPEC-007 WS-3). Valid values are "http"
// (the default) and "https".
const endpointSchemeEnv = "O3K_ENDPOINT_SCHEME"

var invalidSchemeWarnOnce sync.Once

// EndpointScheme returns the advertised URL scheme ("http" or "https") from the
// O3K_ENDPOINT_SCHEME environment variable. It defaults to "http" when unset or
// empty, parses case-insensitively and trims surrounding whitespace, and on any
// unsupported value logs a one-time WARN and falls back to "http"
// (FR-009/FR-011/FR-014).
func EndpointScheme() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(endpointSchemeEnv))) {
	case "", "http":
		return "http"
	case "https":
		return "https"
	default:
		invalidSchemeWarnOnce.Do(func() {
			log.Warn().
				Str("o3k_endpoint_scheme", os.Getenv(endpointSchemeEnv)).
				Msg("unsupported O3K_ENDPOINT_SCHEME; falling back to http (valid values: http, https)")
		})
		return "http"
	}
}

// ForceScheme rewrites a leading "http://" or "https://" in url to scheme://.
// A url without a recognized leading scheme is returned unchanged.
func ForceScheme(url, scheme string) string {
	switch {
	case strings.HasPrefix(url, "http://"):
		return scheme + "://" + url[len("http://"):]
	case strings.HasPrefix(url, "https://"):
		return scheme + "://" + url[len("https://"):]
	default:
		return url
	}
}
