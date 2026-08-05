package common

import (
	"context"
	"errors"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// ResolveProjectRef resolves a project reference to the id of an existing project.
// It is the single shared implementation for every service that accepts a project in
// a URL path — Nova's and Cinder's `os-quota-sets/{id}`, Neutron's `quotas/{id}` —
// so they cannot disagree about what a valid target is.
//
// Upstream specifies these path segments as project UUIDs; O3K additionally accepts a
// project name, a pre-existing extension the contract suite relies on (it addresses
// quota-sets as "default").
//
// The project must exist. A syntactically valid UUID is not enough: upstream
// validates the target against Keystone and errors when it is unknown, explicitly so
// that quota rows are never stored against a project that does not exist. Returns
// false for an unknown reference AND for a malformed one, so callers answer 404
// rather than
//
//   - serving default quotas for a phantom project (a well-formed but unknown UUID
//     finds no rows, and the handler would fall back to its code defaults), or
//   - handing a non-UUID straight to a `uuid` column, which Postgres rejects with
//     "invalid input syntax for type uuid" — surfacing as a 500.
//
// Callers pass c.Request.Context(); this takes a plain context so the helper stays
// testable without a gin request.
// A database failure is reported as "not found" too, because the signature has no third
// state and every caller's next move is a 404 — but it is logged at Error, so an outage
// is not indistinguishable from a genuinely unknown project in the log. Only the
// no-rows case is silent, since that one is an ordinary answer.
func ResolveProjectRef(ctx context.Context, ref string) (string, bool) {
	query := "SELECT id FROM projects WHERE name = $1"
	if uuid.Validate(ref) == nil {
		query = "SELECT id FROM projects WHERE id = $1"
	}

	var resolved string
	if err := database.DB.QueryRow(ctx, query, ref).Scan(&resolved); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Error().Err(err).Str("ref", ref).
				Msg("failed to resolve project reference; answering not-found")
		}
		return "", false
	}
	return resolved, true
}

// QuotaTargetProject resolves which project a quota request acts on, from the `:id`
// path parameter the quota routes of all three services use — Nova's
// `os-quota-sets/:id`, Cinder's `quota-sets/:id`, Neutron's `quotas/:id`.
//
// A caller holding the `admin` role may address any project, by UUID or by name;
// everyone else is scoped to their own project, so a foreign id can never return
// another project's data (SPEC-020 FR-002a, Constitution Art. V). The bare and
// `default` forms always mean "my project" — `default` is the quota API's own alias
// for the caller's project, which is why this helper is named for quotas rather than
// presented as a general path-parameter resolver.
//
// Returns false when an admin names a project that does not exist, so the caller answers
// 404 (SC-007a). A non-admin's foreign id is ignored rather than resolved, so the
// endpoint cannot be used to probe which projects exist.
//
// It also returns false for an **unscoped** token. Keystone issues those (a token request
// with no project carries no project and no roles), and the fall-back branch would then
// hand the caller's empty project id to a `uuid`-typed column — "invalid input syntax for
// type uuid", surfacing as a 500 on what is really a malformed request.
func QuotaTargetProject(c *gin.Context) (string, bool) {
	requested := c.Param("id")
	if requested == "" || requested == "default" || !HasRole(c, RoleAdmin) {
		caller := c.GetString("project_id")
		return caller, caller != ""
	}
	return ResolveProjectRef(c.Request.Context(), requested)
}
