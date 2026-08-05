package common

import (
	"slices"

	"github.com/gin-gonic/gin"
)

// RoleAdmin is the administrative role. It is the only role Lightstack gates behavior on,
// so it gets a constant while `member` and `reader` (also seeded by migration 002)
// do not — nothing checks them. Roles are not a closed set: the roles table is
// unconstrained and operators may define their own, so this is a name for the one
// role the code depends on, not an enumeration of what may exist.
const RoleAdmin = "admin"

// HasRole reports whether the authenticated request carries the given role in the
// JWT-derived "roles" slice that the auth middleware sets on the gin context. It is
// the single shared role-check helper for all services (Keystone, Glance, …), so the
// admin/role check is not re-implemented per package.
func HasRole(c *gin.Context, role string) bool {
	return slices.Contains(c.GetStringSlice("roles"), role)
}
