package common

import (
	"slices"

	"github.com/gin-gonic/gin"
)

// HasRole reports whether the authenticated request carries the given role in the
// JWT-derived "roles" slice that the auth middleware sets on the gin context. It is
// the single shared role-check helper for all services (Keystone, Glance, …), so the
// admin/role check is not re-implemented per package.
func HasRole(c *gin.Context, role string) bool {
	return slices.Contains(c.GetStringSlice("roles"), role)
}
