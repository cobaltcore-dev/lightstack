package neutron

import (
	"fmt"
	"net/http"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
)

// invalidAttribute is the API layer's refusal of an attribute its validator rejects: 400, upstream's
// "Invalid input for <attribute>. Reason: <reason>.".
func invalidAttribute(attribute, reason string) *common.OpenStackError {
	return common.NewBadRequestError("Invalid input for " + attribute + ". Reason: " + reason + ".")
}

// badRequest is neutron_lib's BadRequest: 400, its message upstream's "Bad <resource> request: <msg>.",
// the resource and msg as the upstream code raising it passes them.
func badRequest(resource, message string) *common.OpenStackError {
	return common.NewNeutronError(http.StatusBadRequest, "BadRequest", fmt.Sprintf("Bad %s request: %s.", resource, message))
}

// ErrorEnvelope marks every request to this service so its errors render in Neutron's envelope
// rather than the compute one shared by the other services (SPEC-024 / cloud-storage#546 B10).
//
// Register it on the ENGINE, as the first middleware — not on a route group. Neutron's error paths do
// not all run inside a group: the shared auth middleware aborts at engine level, and NoRoute and
// NoMethod are registered on the engine and never enter a group at all. A group-scoped marker would
// convert the handler responses and leave an unauthenticated call, an unrouted path and a wrong method
// answering in the compute envelope — a partial conversion that passes a wrapper check while still
// being wrong, which is exactly what FR-008 forbids. Going on first also puts it ahead of
// ErrorHandlingMiddleware, so a recovered panic renders through it too.
//
// It changes no call site: everything funnels through common.SendError, which reads the mark.
func ErrorEnvelope() gin.HandlerFunc {
	return func(c *gin.Context) {
		common.MarkNeutronErrors(c)
		c.Next()
	}
}
