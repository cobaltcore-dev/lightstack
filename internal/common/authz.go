package common

import "github.com/gin-gonic/gin"

// HasRole reports whether the authenticated caller carries the given role.
//
// The match is exact: HasRole(c, "admin") is true only for a literal "admin"
// entry. This is deliberately narrower than middleware.RequireRole, which treats
// "admin" as a wildcard satisfying any required role — that widening is right for
// route-level gating, but wrong for an in-handler predicate asking "is this caller
// specifically an admin?". The two are different tools; neither replaces the other.
//
// A request whose context carries no roles (unauthenticated, or a middleware that
// never populated them) has no role, so this returns false — callers that must
// distinguish "no roles" from "wrong role" should check the context themselves.
func HasRole(c *gin.Context, role string) bool {
	for _, r := range c.GetStringSlice("roles") {
		if r == role {
			return true
		}
	}
	return false
}
