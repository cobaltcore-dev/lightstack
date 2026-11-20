package common

import (
	"slices"

	"github.com/gin-gonic/gin"
)

// RoleManager is a project's manager, upstream's persona between member and administrator. Where
// upstream's policy gives an attribute to an administrator or the project's manager, Lightstack's
// does too: a floating IP's chosen address (branch review SRC-11). Migration 002 does not seed it;
// an operator who wants project managers creates the role and assigns it on the project.
const RoleManager = "manager"

// RoleAdmin is the administrative role. It and RoleManager are the roles Lightstack gates behavior
// on, so they get constants while `member` and `reader` (also seeded by migration 002)
// do not — nothing checks them. Roles are not a closed set: the roles table is
// unconstrained and operators may define their own, so each is a name for a
// role the code depends on, not an enumeration of what may exist.
const RoleAdmin = "admin"

// HasRole reports whether the authenticated request carries the given role in the
// JWT-derived "roles" slice that the auth middleware sets on the gin context. It is
// the single shared role-check helper for all services (Keystone, Glance, …), so the
// admin/role check is not re-implemented per package.
func HasRole(c *gin.Context, role string) bool {
	return slices.Contains(c.GetStringSlice("roles"), role)
}
