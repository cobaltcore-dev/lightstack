package nova

import (
	"errors"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// targetInstance resolves the instance a server operation addresses and returns
// its id and the project that OWNS it.
//
// One rule, one implementation, shared by every server operation — the console
// included, which is why lookupConsoleInstance delegates here rather than
// carrying its own copy (SPEC-023 FR-001).
//
// Upstream Nova's default policy for server operations is admin_or_owner: an
// administrator may address any project's instance, everyone else only their
// own. Two properties of that rule are not obvious and are the reason this is a
// function rather than an inline branch:
//
//   - It returns the OWNING project, not a boolean. Callers need it to scope
//     their follow-up queries and to attribute the action to the right project
//     (FR-007) — recording an administrator's action against the administrator's
//     own project would leave the affected tenant's history silently empty.
//
//   - A NAME never spans projects, even for an administrator (FR-005c). O3K
//     accepts a name where upstream accepts only a UUID, so name matching is a
//     local extension. Two projects may own an instance called "web-1"; letting
//     an administrator's `openstack server delete web-1` reach across projects
//     would make destroying the wrong tenant's server a typo away. An
//     administrator addressing another project's instance must use its id.
//
// Returns pgx.ErrNoRows for both "absent" and "someone else's", so callers
// answer 404 either way: a 403 would confirm the instance exists.
//
// A malformed reference — neither a UUID nor a name that exists — is also
// pgx.ErrNoRows rather than a Postgres cast error, because the id comparison is
// done as text.
func targetInstance(c *gin.Context, ref string) (id, owner string, err error) {
	// An administrator addressing a well-formed id may reach any project. Every
	// other combination — a non-administrator, or a name — stays scoped to the
	// caller's own project.
	if common.HasRole(c, common.RoleAdmin) && uuid.Validate(ref) == nil {
		err = database.DB.QueryRow(c.Request.Context(),
			"SELECT id::text, project_id::text FROM instances WHERE id = $1",
			ref,
		).Scan(&id, &owner)
		return id, owner, err
	}

	callerProject := c.GetString("project_id")
	err = database.DB.QueryRow(c.Request.Context(),
		`SELECT id::text, project_id::text FROM instances
		 WHERE project_id = $2 AND ((id::text = $1) OR (name = $1))`,
		ref, callerProject,
	).Scan(&id, &owner)
	return id, owner, err
}

// resolveServerOr404 is targetInstance for the common case: a handler that needs
// the instance's id and owning project, and answers 404 for anything it cannot
// reach. It writes the error response itself and reports success via the bool,
// so call sites stay two lines.
//
// The 404 is deliberate and applies to both "absent" and "someone else's": a 403
// would confirm the instance exists. A database failure is reported as 500,
// because that is not an authorization answer and must not be mistaken for one.
func resolveServerOr404(c *gin.Context, ref string) (id, owner string, ok bool) {
	id, owner, err := targetInstance(c, ref)
	if errors.Is(err, pgx.ErrNoRows) {
		denyCrossTenant(c, ref)
		common.SendError(c, common.NewNotFoundError("instance"))
		return "", "", false
	}
	if err != nil {
		log.Error().Err(err).Str("instance_ref", ref).Msg("instance lookup failed")
		common.SendError(c, common.NewInternalServerError("failed to look up instance"))
		return "", "", false
	}
	return id, owner, true
}

// requireAdmin gates a surface that belongs to the DEPLOYMENT rather than to any
// project — host aggregates and the deployment-wide migration list (SPEC-023
// FR-008e, FR-008f). It reports whether the caller may proceed, sending the 403
// itself so call sites stay one line.
//
// These are deliberately NOT owner-or-administrator. Their tables carry no
// project_id, so there is no owner for the rule to fall back to; access is
// administrative or nothing. That missing column is also how they came to be
// ungated at all: every other handler in this package was written against a
// table with a project_id, so a scoping predicate was the obvious thing to
// write — here there was nothing obvious to write, and nothing was written.
//
// 403 rather than 404: unlike an instance, the existence of these surfaces is
// not a secret (they are in the API's own version document), so there is nothing
// to disclose by admitting they are there.
func requireAdmin(c *gin.Context, operation string) bool {
	if common.HasRole(c, common.RoleAdmin) {
		return true
	}
	log.Warn().
		Str("caller_project", c.GetString("project_id")).
		Str("caller_user", c.GetString("user_id")).
		Str("operation", operation).
		Msg("refused operator-only compute operation")
	common.SendError(c, common.NewForbiddenError(
		"Policy doesn't allow "+operation+" to be performed. Admin role required."))
	return false
}

// denyCrossTenant records a refused cross-tenant compute access, naming the
// parties (SPEC-023 FR-007b).
//
// A refused cross-tenant access is the event an operator goes looking for later
// — a misconfigured client, or a probe — and it cannot be answered from a log
// that recorded nothing. This mirrors what #545 established for refused
// cross-tenant network writes (internal/neutron/network.go).
//
// Only refusals are logged. A permitted access by the resource's own owner is
// ordinary traffic and logs nothing.
//
// The resource is always an instance: every caller reaches this through
// resolveServerOr404, including the migration and console paths. Add a kind
// parameter when a second kind actually exists, not before.
func denyCrossTenant(c *gin.Context, ref string) {
	log.Warn().
		Str("caller_project", c.GetString("project_id")).
		Str("caller_user", c.GetString("user_id")).
		Str("instance_ref", ref).
		Msg("refused cross-tenant compute access")
}

// recordInstanceAction writes an entry to the instance's action history.
//
// project MUST be the OWNING project, not the caller's (SPEC-023 FR-007). The
// read side selects by instance_id behind the instance ownership check, so a row
// carrying the owner is what surfaces in the affected tenant's own view of their
// server. Recording the administrator's project instead would leave that view
// silently empty for exactly the operations a tenant most wants to see — someone
// else deleting or resizing their server.
//
// A cross-project action is recognisable without a new column: the acting user
// holds no role in the project the row is recorded against.
//
// Before SPEC-023 only creation was ever recorded (a single insert site), so
// FR-007 would have been satisfied vacuously — the right project on a row that
// was never written (FR-007a).
//
// Failures are logged and swallowed: the history is an audit convenience, and
// losing an entry must not fail the operation that succeeded.
func recordInstanceAction(c *gin.Context, instanceID, project, action string) {
	if instanceID == "" || project == "" {
		return
	}
	_, err := database.DB.Exec(c.Request.Context(), `
		INSERT INTO instance_actions (instance_id, action, request_id, user_id, project_id, start_time, message)
		VALUES ($1, $2, $3, $4, $5, NOW(), $6)
	`, instanceID, action, requestID(c), nullableUUID(c.GetString("user_id")), project,
		"Action "+action+" requested")
	if err != nil {
		log.Debug().Err(err).Str("instance_id", instanceID).Str("action", action).
			Msg("failed to record instance action (non-critical)")
	}
}

// requestID returns the id the logging middleware assigned to this request, so an
// instance-action record can be correlated with the request log that produced it.
//
// It is a client-facing value, not bookkeeping: os-instance-actions returns it,
// and GET /servers/{id}/os-instance-actions/{request_id} routes on it. Both
// insert sites previously invented their own — in two different formats — which
// left the column's shape unreliable for the clients that look records up by it.
//
// The fallback covers contexts without the middleware (tests, and the health
// paths it skips).
func requestID(c *gin.Context) string {
	if id := c.GetString("request_id"); id != "" {
		return id
	}
	return "req-" + uuid.NewString()
}

// nullableUUID converts an empty string to nil so a missing user id stores NULL
// rather than failing the insert against a uuid-typed column.
func nullableUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// The recorded action name is no longer derived separately from the request
// body: ServerAction accepts exactly one key, reads the action name and the
// handler from that single key, and passes the name to recordInstanceAction. The
// name and the operation performed therefore cannot disagree by construction,
// which is what the old actionName helper could only assert in a comment.
