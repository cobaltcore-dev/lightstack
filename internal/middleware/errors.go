package middleware

import (
	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/gin-gonic/gin"
)

// ErrorHandlingMiddleware catches panics and converts them to proper error responses
func ErrorHandlingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// Log the panic
				logger.Error().
					Interface("error", err).
					Str("request_id", getRequestID(c)).
					Str("path", c.Request.URL.Path).
					Msg("Panic recovered in error handling middleware")

				// Only send if response not yet written
				if !c.Writer.Written() {
					osErr := common.NewInternalServerError("internal server error")
					common.SendError(c, osErr)
				}
			}
		}()

		c.Next()

		// Check if there are any errors set by handlers, but only if no response has been written yet.
		// SendError writes a response directly, so c.Errors populated by non-SendError paths
		// (e.g. ShouldBindJSON failures that called c.Error) are handled here.
		if len(c.Errors) > 0 && !c.Writer.Written() {
			lastErr := c.Errors.Last()

			if osErr, ok := lastErr.Err.(*common.OpenStackError); ok {
				common.SendError(c, osErr)
				return
			}

			osErr := common.NewInternalServerError(lastErr.Error())
			common.SendError(c, osErr)
		}
	}
}

// getRequestID extracts request ID from context
func getRequestID(c *gin.Context) string {
	if reqID, exists := c.Get("request_id"); exists {
		return reqID.(string)
	}
	return "unknown"
}

// NotFoundHandler returns a 404 error for undefined routes
func NotFoundHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		osErr := common.NewNotFoundError("Resource")
		common.SendError(c, osErr)
	}
}

// MethodNotAllowedHandler returns a 405 error for wrong HTTP methods
func MethodNotAllowedHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		osErr := common.NewMethodNotAllowedError(c.Request.Method)
		common.SendError(c, osErr)
	}
}
