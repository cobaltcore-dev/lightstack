package common

import (
	"context"
	"errors"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// ResolveProjectRef resolves a project reference to the id of an existing project.
// It is the single shared implementation for every service that accepts a project in
// a URL path — Nova's and Cinder's `os-quota-sets/{id}`, Neutron's `quotas/{id}` —
// so they cannot disagree about what a valid target is.
//
// Upstream specifies these path segments as project UUIDs; Lightstack additionally accepts a
// project name, a pre-existing Cinder extension. The contract suite's `quota-sets/default`
// never reaches this lookup: QuotaTargetProject resolves `default` to the caller first.
//
// The project must exist. A syntactically valid UUID is not enough: upstream
// validates the target against Keystone and errors when it is unknown, explicitly so
// that quota rows are never stored against a project that does not exist. Returns
// a not-found error for an unknown reference AND for a malformed one, so callers answer 404
// rather than
//
//   - serving default quotas for a phantom project (a well-formed but unknown UUID
//     finds no rows, and the handler would fall back to its code defaults), or
//   - handing a non-UUID straight to a `uuid` column, which Postgres rejects with
//     "invalid input syntax for type uuid" — surfacing as a 500.
//
// Callers pass c.Request.Context() and their own service's pool; this takes a plain
// context so the helper stays testable without a gin request.
// A database failure is an internal error (500), never "not found": answering 404 during an
// outage would tell the client the project does not exist. It is logged at Error; the
// no-rows case is silent, since that one is an ordinary answer.
func ResolveProjectRef(ctx context.Context, pool database.DB, ref string) (string, *OpenStackError) {
	query := "SELECT id FROM projects WHERE name = $1"
	// Ids are stored canonical, and on SQLite they are compared as text, so a dash-free or
	// upper-case id — the form rgw_compat_tenant_ids puts in token payloads, which a client
	// sends back as the target — must be canonicalised here, not handed to the query as the
	// client wrote it. uuid.Parse accepts exactly the forms uuid.Validate did.
	if id, err := uuid.Parse(ref); err == nil {
		query = "SELECT id FROM projects WHERE id = $1"
		ref = id.String()
	}

	var resolved string
	if err := pool.QueryRow(ctx, query, ref).Scan(&resolved); err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return "", NewNotFoundError("project")
		}
		log.Error().Err(err).Str("ref", ref).Msg("failed to resolve project reference")
		return "", NewInternalServerError("failed to resolve project")
	}
	return resolved, nil
}

// QuotaTargetProject resolves which project a quota request acts on — reads and writes
// alike — from the `:id` path parameter the quota routes of all three services use:
// Nova's `os-quota-sets/:id`, Cinder's `quota-sets/:id`, Neutron's `quotas/:id`. A write
// that resolved its target any other way would let `default` name one project on GET
// and another on PUT.
//
// A caller holding the `admin` role may address any project, by UUID or by name.
// Everyone else may address only their own project. An explicit id naming another
// project is a 403, as upstream's quota policies give it: Nova's
// PROJECT_READER_OR_ADMIN, Cinder's SYSTEM_READER_OR_PROJECT_READER and Neutron's
// get_quota. The refusal comes before any lookup, so it says nothing about whether the
// project exists (SPEC-020 FR-002a, Constitution Art. V). The id is compared as a UUID,
// so the dash-free form that keystone.rgw_compat_tenant_ids puts in token payloads still
// names the caller's own project. The bare and `default` forms always mean "my project".
// `default` is Lightstack's own alias for the caller's project (upstream has none), which
// is why this helper is named for quotas rather than presented as a general
// path-parameter resolver.
//
// Returns a not-found error when an admin names a project that does not exist, so the
// caller answers 404 (SC-007a), and an internal error when the lookup itself fails.
//
// It also returns a not-found error for an **unscoped** token. Keystone issues those (a token request
// with no project carries no project and no roles), and the fall-back branch would then
// hand the caller's empty project id to a `uuid`-typed column — "invalid input syntax for
// type uuid", surfacing as a 500 on what is really a malformed request.
//
// pool is the calling service's pool. Only the admin branch queries it; every other
// branch resolves from the request alone.
func QuotaTargetProject(c *gin.Context, pool database.DB) (string, *OpenStackError) {
	requested := c.Param("id")
	caller := c.GetString("project_id")
	switch {
	case requested == "" || requested == "default":
		// the caller's own project, below
	case HasRole(c, RoleAdmin):
		return ResolveProjectRef(c.Request.Context(), pool, requested)
	case caller != "" && !sameProjectID(requested, caller):
		return "", NewForbiddenError("Policy doesn't allow reading another project's quota. Admin role required.")
	}
	if caller == "" {
		return "", NewNotFoundError("project")
	}
	return caller, nil
}

// sameProjectID reports whether a and b name the same project. They are compared as UUIDs,
// so the dashed and dash-free forms of one id match; anything that is not a UUID must be
// equal as given.
func sameProjectID(a, b string) bool {
	ua, errA := uuid.Parse(a)
	ub, errB := uuid.Parse(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ua == ub
}
