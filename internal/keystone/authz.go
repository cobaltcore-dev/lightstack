package keystone

import (
	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The identity management API is authentication-only by transport: every route
// under /v3 (except /auth/*) requires a valid token. What it historically did
// NOT require is authorization — the management surface (users, projects,
// roles, assignments, groups, domains, catalog, credentials) honored any
// authenticated caller, so a plain member could reset the admin's password,
// self-grant the admin role, or dump another tenant's S3 secret. These helpers
// put the same check the EC2-credentials API and ChangePassword already use
// (self-or-admin) plus the admin-only variant across that surface.
//
// Convention (cloud-storage#515, asserted by TestIdentityAuthzPrecedence):
// on a route with an authz check, the check runs in-handler BEFORE id
// validation, so a caller without permission gets 403 even for a malformed id.
// Such routes therefore sit on `mgmt` (no uuidParamMiddleware) and validate
// their path ids with denyIfMalformedID below, after the authz decision.

// denyIfMalformedID writes the Keystone-verbatim 404 and returns true when id
// is not a well-formed UUID. Call it after the authz check (see above).
func denyIfMalformedID(c *gin.Context, kind, id string) bool {
	if uuid.Validate(id) != nil {
		common.SendError(c, common.NewIdentityNotFoundError(kind, id))
		return true
	}
	return false
}

// sameUser compares UUID values, including the compact IDs used by RGW tokens.
// Invalid or missing caller IDs must never authorize self-service.
func sameUser(caller, target string) bool {
	callerID, err := uuid.Parse(caller)
	if err != nil {
		return false
	}
	targetID, err := uuid.Parse(target)
	return err == nil && callerID == targetID
}
