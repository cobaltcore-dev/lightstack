package middleware

import (
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/health"
)

var logger zerolog.Logger

// SensitiveFields lists fields that should be redacted from logs
var SensitiveFields = []string{
	"password",
	"token",
	"secret",
	"api_key",
	"auth_token",
	"x-auth-token",
	"x-subject-token",
	"authorization",
	"jwt",
	"credential",
}

// InitLogger initializes the global logger based on configuration
func InitLogger(config *common.LoggingConfig) {
	// Parse log level
	level, err := zerolog.ParseLevel(config.Level)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	// Configure output format
	if config.Format == "json" {
		logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
	} else {
		// Pretty console output for development
		output := zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: "2006-01-02 15:04:05",
		}
		logger = zerolog.New(output).With().Timestamp().Logger()
	}

	// Set as global logger
	log.Logger = logger
}

// CallerProjectKey holds the project of the authenticated CALLER, as distinct from
// "project_id", which handlers may rebind to the project being acted upon.
//
// Exported so the auth middleware sets it and the logging middleware reads it without
// either depending on a bare string literal in the other.
const CallerProjectKey = "caller_project_id"

// LoggingMiddleware logs HTTP requests with structured logging
func LoggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Kubernetes polls the health endpoints every few seconds; serve them
		// without flooding the access log (SPEC-013). The path list is owned by
		// the health package so it can't drift from what's actually registered.
		if slices.Contains(health.Paths, c.Request.URL.Path) {
			c.Next()
			return
		}

		// Generate request ID in OpenStack's shape: upstream request ids are
		// `req-<uuid>`, which is what X-Openstack-Request-Id carries and what
		// clients see echoed back from os-instance-actions. Generating it here
		// rather than at each use means the log line, the instance-action record
		// and any response header all carry the identical string, so an action
		// can be correlated with the request that caused it.
		requestID := "req-" + uuid.New().String()
		c.Set("request_id", requestID)

		// Start timer
		start := time.Now()

		// Log incoming request (user_id/project_id not yet available before auth middleware runs)
		logger.Debug().
			Str("request_id", requestID).
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Str("query", redactSensitiveQuery(c.Request.URL.RawQuery)).
			Str("remote_addr", c.ClientIP()).
			Str("user_agent", c.Request.UserAgent()).
			Msg("incoming request")

		// Process request
		c.Next()

		// Calculate duration
		duration := time.Since(start)

		// Extract user/project from context — populated by auth middleware during c.Next()
		userID, _ := c.Get("user_id")

		// Who ACTED, and who was acted upon.
		//
		// "project_id" is the EFFECTIVE project after any handler rebinding, so reading
		// it alone attributed an operator's cross-tenant action to the tenant acted upon
		// (cloud-storage#547 B15). CallerProjectKey is set by the auth middleware from
		// the token and is never rebound, so it is the caller.
		//
		// The target appears only when it DIFFERS (FR-046a): an ordinary same-project
		// line must stay byte-identical to what it was, and the existing caller field is
		// never renamed — it is the one operators already grep.
		callerProject, _ := c.Get(CallerProjectKey)
		effectiveProject, _ := c.Get("project_id")

		projectID := callerProject
		if projectID == nil {
			// No caller recorded: an unauthenticated path, or a test wiring that sets
			// only project_id. Fall back rather than lose the field entirely.
			projectID = effectiveProject
		}
		var targetProject any
		if callerProject != nil && effectiveProject != nil && callerProject != effectiveProject {
			targetProject = effectiveProject
		}

		// Determine log level based on status code
		statusCode := c.Writer.Status()
		logEvent := logger.Info()
		if statusCode >= 500 {
			logEvent = logger.Error()
		} else if statusCode >= 400 {
			logEvent = logger.Warn()
		}

		// Log response with performance metrics
		logEvent.
			Str("request_id", requestID).
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Int("status", statusCode).
			Int64("duration_ms", duration.Milliseconds()).
			Int("response_size", c.Writer.Size())

		if userID != nil {
			logEvent.Str("user_id", userID.(string))
		}
		if projectID != nil {
			logEvent.Str("project_id", projectID.(string))
		}
		if targetProject != nil {
			// Present only for a cross-project action, so "who touched tenant X" is
			// answerable from this line alone, with no stored record consulted (SC-012).
			logEvent.Str("target_project_id", targetProject.(string))
		}

		// Add slow request warning
		if duration > 1*time.Second {
			logEvent.Bool("slow_request", true)
		}

		logEvent.Msg("request completed")

		// Log errors if any occurred
		if len(c.Errors) > 0 {
			for _, err := range c.Errors {
				logger.Error().
					Str("request_id", requestID).
					Uint("type", uint(err.Type)).
					Err(err.Err).
					Str("path", c.Request.URL.Path).
					Str("method", c.Request.Method).
					Msg("request error")
			}
		}
	}
}

// redactSensitiveQuery removes sensitive values from query strings
func redactSensitiveQuery(query string) string {
	if query == "" {
		return ""
	}

	// Simple check for sensitive field names
	lowerQuery := strings.ToLower(query)
	for _, field := range SensitiveFields {
		if strings.Contains(lowerQuery, field) {
			return "[REDACTED]"
		}
	}

	return query
}

// GetLogger returns a logger with request context
func GetLogger(c *gin.Context) *zerolog.Logger {
	requestID, exists := c.Get("request_id")
	if !exists {
		return &logger
	}

	ctxLogger := logger.With().Str("request_id", requestID.(string))

	// Add user and project context if available
	if userID, exists := c.Get("user_id"); exists {
		ctxLogger = ctxLogger.Str("user_id", userID.(string))
	}
	if projectID, exists := c.Get("project_id"); exists {
		ctxLogger = ctxLogger.Str("project_id", projectID.(string))
	}

	loggerWithContext := ctxLogger.Logger()
	return &loggerWithContext
}

// LogOperationStart logs the start of an operation with context
func LogOperationStart(c *gin.Context, operation, resourceType, resourceID string) {
	logger := GetLogger(c)
	logger.Info().
		Str("operation", operation).
		Str("resource_type", resourceType).
		Str("resource_id", resourceID).
		Msg("operation started")
}

// LogOperationEnd logs the end of an operation with duration
func LogOperationEnd(c *gin.Context, operation, resourceType, resourceID string, duration time.Duration, err error) {
	logger := GetLogger(c)
	logEvent := logger.Info()

	if err != nil {
		logEvent = logger.Error().Err(err)
	}

	logEvent.
		Str("operation", operation).
		Str("resource_type", resourceType).
		Str("resource_id", resourceID).
		Dur("duration", duration).
		Msg("operation completed")
}

// LogExternalService logs calls to external services (libvirt, Ceph, S3)
func LogExternalService(c *gin.Context, service, operation string, duration time.Duration, err error) {
	logger := GetLogger(c)
	logEvent := logger.Info()

	if err != nil {
		logEvent = logger.Warn().Err(err)
	}

	logEvent.
		Str("external_service", service).
		Str("operation", operation).
		Dur("duration", duration).
		Msg("external service call")
}

// LogDatabaseQuery logs database operations (used by query logger)
func LogDatabaseQuery(c *gin.Context, query string, duration time.Duration, err error) {
	logger := GetLogger(c)
	logEvent := logger.Debug()

	if duration > 100*time.Millisecond {
		logEvent = logger.Warn().Bool("slow_query", true)
	}

	if err != nil {
		logEvent = logger.Error().Err(err)
	}

	logEvent.
		Str("query", query).
		Dur("duration", duration).
		Msg("database query")
}

// RecoveryMiddleware handles panics.
//
// It renders through common.SendError rather than gin's AbortWithStatus, which writes a status and
// no body at all. This is the only recover that runs: deferred recovers unwind innermost-first, and
// every service registers this middleware after ErrorHandlingMiddleware (cmd/lightstack/main.go), so
// its recover is the inner one and fires first — ErrorHandlingMiddleware's then sees nil. Keep it
// after that one if the chain changes. A panic therefore used to answer with a bodiless 500 on every
// service, and on Neutron it also escaped the NeutronError envelope that FR-008 requires on *every*
// failing path (branch review 2026-08-13, SRC-2).
//
// SendError picks the envelope from the request, so Neutron gets NeutronError and the other services
// get their existing compute-style body — each service keeps its own shape, and none of them keeps
// the empty one.
func RecoveryMiddleware() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		log.Error().
			Interface("panic", recovered).
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Msg("panic recovered")
		common.SendError(c, common.NewInternalServerError("internal server error"))
		c.Abort()
	})
}

// CORSMiddlewareWithConfig adds CORS headers using a configurable origin allowlist.
// Only origins present in allowedOrigins are echoed back; no wildcard is used.
// If allowedOrigins is empty or nil, it defaults to ["http://localhost"].
func CORSMiddlewareWithConfig(allowedOrigins []string) gin.HandlerFunc {
	if len(allowedOrigins) == 0 {
		allowedOrigins = []string{"http://localhost"}
	}

	// Build O(1) lookup map.
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		originSet[o] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if _, allowed := originSet[origin]; allowed {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
			c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Auth-Token, X-Subject-Token, OpenStack-API-Version, X-OpenStack-Nova-API-Version, Accept")
			c.Writer.Header().Set("Access-Control-Max-Age", "3600")
		}

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// CORSMiddleware adds CORS headers using the default origin allowlist.
// Deprecated: prefer CORSMiddlewareWithConfig with an explicit allowlist.
func CORSMiddleware() gin.HandlerFunc {
	return CORSMiddlewareWithConfig(nil)
}

func init() {
	// Use JSON binding by default
	binding.EnableDecoderUseNumber = true
}
