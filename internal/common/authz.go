package common

import (
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// RequireAdmin denies requests that do not carry the deployment-wide admin
// role. operation identifies the API operation in the error and refusal log.
func RequireAdmin(c *gin.Context, operation string) bool {
	if HasRole(c, RoleAdmin) {
		return true
	}
	log.Warn().
		Str("caller_project", c.GetString("project_id")).
		Str("caller_user", c.GetString("user_id")).
		Str("operation", operation).
		Msg("refused operator-only API operation")
	SendError(c, NewForbiddenError(
		"Policy doesn't allow "+operation+" to be performed. Admin role required."))
	return false
}
