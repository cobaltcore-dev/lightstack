package common

import (
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// HandleDatabaseError converts database errors to appropriate OpenStack errors
func HandleDatabaseError(c *gin.Context, err error, resource string) {
	if errors.Is(err, pgx.ErrNoRows) {
		SendError(c, NewNotFoundError(resource))
		return
	}

	// Log the database error
	log.Error().
		Err(err).
		Str("resource", resource).
		Msg("Database error occurred")

	SendError(c, NewDatabaseError("query", resource, err))
}

// HandleDatabaseErrorWithOperation converts database errors with operation context
func HandleDatabaseErrorWithOperation(c *gin.Context, err error, operation, resourceType, resourceID string) {
	if errors.Is(err, pgx.ErrNoRows) {
		SendError(c, NewResourceNotFoundError(resourceType, resourceID))
		return
	}

	// Log the database error with context
	log.Error().
		Err(err).
		Str("operation", operation).
		Str("resource_type", resourceType).
		Str("resource_id", resourceID).
		Msg("Database error occurred")

	SendError(c, NewDatabaseError(operation, resourceType, err))
}

// HandleValidationError sends a bad request error with validation details
func HandleValidationError(c *gin.Context, field string, message string) {
	err := NewValidationError(field, message, "")
	SendError(c, err)
}

// HandleValidationErrorWithSuggestion sends a validation error with a helpful suggestion
func HandleValidationErrorWithSuggestion(c *gin.Context, field, issue, suggestion string) {
	err := NewValidationError(field, issue, suggestion)
	SendError(c, err)
}

// HandleBindingError handles JSON binding errors
func HandleBindingError(c *gin.Context, err error) {
	osErr := NewBadRequestErrorWithDetails(
		"Invalid request body",
		err.Error(),
	)
	SendError(c, osErr)
}

// AbortWithError sends an error response and aborts the request chain.
// Does NOT call c.Error() to avoid triggering the ErrorHandlingMiddleware a second time.
func AbortWithError(c *gin.Context, err *OpenStackError) {
	SendError(c, err)
	c.Abort()
}
