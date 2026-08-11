package neutron

import (
	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/gin-gonic/gin"
)

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
