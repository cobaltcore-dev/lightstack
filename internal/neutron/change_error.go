package neutron

import (
	"errors"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// sendChangeError answers a request whose change failed: a refusal (*common.OpenStackError) as it is,
// anything else as 500 "failed to <operation> <resource>", logged with the request's path. A handler's
// failure ends here, directly or through a helper that first maps errors of its own
// (sendFloatingIPStoreError, sendRuleStoreError, sendPortChangeError).
func sendChangeError(c *gin.Context, err error, operation, resource string) {
	var refusal *common.OpenStackError
	if errors.As(err, &refusal) {
		common.SendError(c, refusal)
		return
	}
	log.Error().Err(err).Str("operation", operation+" "+resource).Str("path", c.Request.URL.Path).Msg("the change failed")
	common.SendError(c, common.NewInternalServerError("failed to "+operation+" "+resource))
}
