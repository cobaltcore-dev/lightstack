package middleware

import (
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/keystone"
	"github.com/gin-gonic/gin"
)

// AuthMiddleware validates OpenStack tokens
func AuthMiddleware(authService *keystone.AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip auth for version discovery endpoints
		if strings.HasSuffix(c.Request.URL.Path, "/v3") ||
			strings.HasSuffix(c.Request.URL.Path, "/v2.1") ||
			strings.HasSuffix(c.Request.URL.Path, "/v2.0") ||
			c.Request.URL.Path == "/" {
			c.Next()
			return
		}

		// Skip auth for token issuance endpoint
		if c.Request.Method == "POST" && strings.HasSuffix(c.Request.URL.Path, "/auth/tokens") {
			c.Next()
			return
		}

		// Get token from X-Auth-Token header
		token := c.GetHeader("X-Auth-Token")
		if token == "" {
			common.AbortWithError(c, common.NewUnauthorizedError("authentication required"))
			return
		}

		// Validate token
		claims, err := authService.ValidateToken(token)
		if err != nil {
			common.AbortWithError(c, common.NewUnauthorizedError("invalid or expired token"))
			return
		}

		// Store claims in context
		c.Set("user_id", claims.UserID)
		c.Set("user_name", claims.UserName)
		c.Set("project_id", claims.ProjectID)
		c.Set("roles", claims.Roles)

		// The CALLER's project, recorded separately and never rebound (SPEC-025
		// FR-045).
		//
		// "project_id" is the EFFECTIVE project a handler scopes its queries by, and
		// the compute action dispatcher deliberately rebinds it to the owning project
		// so an administrator's action lands in the right tenant (SPEC-023 FR-007).
		// The access log read that field after the handler had run, so a cross-tenant
		// action was attributed to the tenant acted UPON rather than the operator who
		// acted — the one field an operator greps when asking who touched a tenant's
		// server (cloud-storage#547 B15).
		//
		// Set here because this middleware is the authority on who the caller is, and
		// because the logging middleware runs before it: at the time logging's
		// pre-handler code executes, no identity exists yet.
		c.Set(CallerProjectKey, claims.ProjectID)

		c.Next()
	}
}
