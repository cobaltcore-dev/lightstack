package common

import (
	"fmt"
	"os"

	"github.com/gin-gonic/gin"
)

// BaseURL returns the external base URL for self-links. The scheme honors
// O3K_ENDPOINT_SCHEME (SPEC-007 WS-3, FR-013a); when that is unset it still
// auto-detects https from a TLS request, preserving prior behavior.
func BaseURL(c *gin.Context, defaultPort int) string {
	scheme := EndpointScheme()
	if host := os.Getenv("O3K_ENDPOINT_HOST"); host != "" {
		return fmt.Sprintf("%s://%s:%d", scheme, host, defaultPort)
	}
	if scheme == "http" && c.Request.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, c.Request.Host)
}
