package common

import (
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// BaseURL returns the external base URL for self-links: the one rule every service's
// advertised addresses are derived from (SPEC-025 FR-002..FR-007a). No caller may
// assemble a self-link any other way — a relative path, a hardcoded host or a
// scheme-less concatenation is a defect, not a shortcut.
//
// # Host precedence: the request wins
//
// The host the client actually addressed takes precedence, and LIGHTSTACK_ENDPOINT_HOST is
// the FALLBACK used only when a request carries no usable host (FR-002, FR-003).
//
// This reverses the earlier behavior, in which the configured host won
// unconditionally and the request was never consulted. That is what made
// OS_*_ENDPOINT_OVERRIDE unusable: keystoneauth would resolve the catalog, fetch a
// version document, and be handed back the deployment's own hostname in place of the
// one the caller had explicitly named — so the override was silently discarded
// (cloud-storage#547 B3). Behind a proxy or ingress the request already carries the
// hostname the deployment wants advertised, so the configured value is only needed
// where there is none. There is deliberately no setting to restore the old order
// (FR-003a).
//
// # Port: names the TARGET service, and is applied in both branches
//
// defaultPort is the port the service this URL points AT is reachable on, so it
// replaces whatever port the source host carried. That matters because this helper
// serves cross-service links as well as self-links: compute advertises an image
// location on 9292 while itself being addressed on 8774.
//
// It was previously applied only in the configured-host branch and dropped otherwise,
// which is the second defect in this function (FR-004, FR-009). Applying it uniformly
// is correct for this product because Lightstack is port-per-service — the chart's own
// ingress template records that each service has its own port — so there is no
// path-routed topology for a port to break.
//
// Pass 0 to mean "whatever the source said", preserving any port the host carried.
//
// # Scheme
//
// LIGHTSTACK_ENDPOINT_SCHEME wins; otherwise https is auto-detected from a TLS request,
// which covers termination at a proxy ahead of the service (FR-005).
func BaseURL(c *gin.Context, defaultPort int) string {
	scheme := EndpointScheme()

	host := requestHost(c)
	if host == "" {
		host = os.Getenv("LIGHTSTACK_ENDPOINT_HOST")
	}
	if host == "" {
		// Last resort. A self-link must be absolute and parseable even for a
		// synthetic or malformed request (FR-007); an empty authority would emit
		// "http://", which no client can resolve.
		host = "localhost"
	}

	if scheme == "http" && c != nil && c.Request != nil && c.Request.TLS != nil {
		scheme = "https"
	}
	if defaultPort > 0 {
		host = hostWithPort(host, defaultPort)
	}
	return scheme + "://" + host
}

// requestHost reads the Host the client addressed, tolerating a nil context or
// request so a self-link can still be produced outside a live request.
func requestHost(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return c.Request.Host
}

// HostWithoutPort returns host with any port removed, in a form a port can be
// appended to: an IPv6 literal comes back bracketed, a name or IPv4 address bare.
//
// Handling both bracket forms matters: net.SplitHostPort strips the brackets from
// "[::1]:8774" but reports an error for a bare "[fe80::1]", so each case needs the
// brackets restored from a different starting point. Getting this wrong previously
// produced authorities like "lightstack.example.com:1234:9696" — two ports, no client can
// resolve it — whenever a configured host already named a port.
//
// Exported because the service catalog needs the same answer for a different reason
// (SPEC-025-followup FR-031): it substitutes the request host into endpoint URLs that
// supply their own ":port", so a request host arriving as "lightstack.example.com:35357"
// would produce exactly the two-port authority described above. One implementation,
// two callers, rather than the second caller re-deriving it and getting the IPv6 case
// wrong.
func HostWithoutPort(host string) string {
	h := host
	if stripped, _, err := net.SplitHostPort(h); err == nil {
		h = stripped
	} else {
		h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	}
	if strings.Contains(h, ":") {
		// A bare IPv6 literal: it must be bracketed before a port is appended.
		return "[" + h + "]"
	}
	return h
}

// hostWithPort replaces host's port with the given one.
func hostWithPort(host string, port int) string {
	return HostWithoutPort(host) + ":" + strconv.Itoa(port)
}
