package neutron

import (
	"errors"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// An administrator reaches every project's Networking API resources, as upstream's admin_or_owner
// policy lets them (SPEC-033 round 2: FR-057, FR-058; research R50). One rule, in three shapes:
//
//   - A listing shows an administrator every project's resources and project_id/tenant_id narrow it
//     to the named projects; anyone else keeps the scope they had — their own project, and for
//     networks, subnets and some others also what is shared or external — narrowed by the same
//     filters (projectScope).
//   - A show, an update, a delete or an action on a resource addressed by id reaches any project's
//     for an administrator: the statement's project condition becomes "(column = $p OR $a)", $a
//     binding isAdmin (ownerOrAdmin). A resource addressed by name resolves only in the caller's own
//     project, administrators included, as Nova's targetInstance does for servers.
//   - A create may reference another project's resource by id, and the resource it creates belongs
//     to the project the request names, as SPEC-024's rule already had it for networks, or else to
//     the administrator's own (ownerOf).
//
// Upstream's listings have no other way to see what an unfiltered listing leaves out, so the old
// own-project scope for administrators could not be registered as a divergence (Article I, limit 4).

// isAdmin reports whether the caller holds the administrator role.
func isAdmin(c *gin.Context) bool { return common.HasRole(c, common.RoleAdmin) }

// callerScope is whose request it is: the project it acts in, and whether it is an administrator's, which
// reaches another project's resource by id (ownerOrAdmin).
type callerScope struct {
	project string
	admin   bool
}

// scopeOf is the request's caller scope.
func scopeOf(c *gin.Context) callerScope {
	return callerScope{project: c.GetString("project_id"), admin: isAdmin(c)}
}

// projectFilters are the listing's project_id and tenant_id values, merged, as upstream merges them.
func projectFilters(c *gin.Context) []string {
	return append(c.QueryArray("project_id"), c.QueryArray("tenant_id")...)
}

// projectScope narrows a listing on f to what the caller may see — every project for an
// administrator, memberScope for anyone else, given the placeholder of their project — and then to the
// projects the query names, on column. A member naming another project gets nothing, as upstream.
func projectScope(c *gin.Context, f *common.SQLFilter, column string, memberScope func(caller string) string) {
	if !isAdmin(c) {
		f.Cond(memberScope(f.Arg(c.GetString("project_id"))))
	}
	if projects := projectFilters(c); len(projects) > 0 {
		f.Cond(column + "::text = ANY(" + f.Arg(projects) + ")")
	}
}

// ownedBy is the scope of a resource only its own project sees.
func ownedBy(column string) func(caller string) string {
	return func(caller string) string { return column + " = " + caller }
}

// ownedOrShared is the scope of a resource its own project sees, and every project once it is shared.
func ownedOrShared(caller string) string {
	return "(project_id = " + caller + " OR shared = true)"
}

// ownerOrAdmin renders "(column = project OR admin)": the condition under which a request reaches a
// resource it addresses by id. project is the placeholder of the caller's project and admin the
// placeholder bound to isAdmin(c).
func ownerOrAdmin(column, project, admin string) string {
	return "(" + column + " = " + project + " OR " + admin + ")"
}

// scopeOrAdmin is ownerOrAdmin for a resource whose member scope is wider than its own project.
func scopeOrAdmin(memberScope, admin string) string {
	return "(" + memberScope + " OR " + admin + ")"
}

// ownerOf decides which project a resource an administrator creates belongs to, writing the refusal
// itself — so the caller only has to return. It is SPEC-024 B11's rule for networks, which mirrors
// upstream's populate_project_info and _validate_privileges, applied to every create (FR-058): one
// spelling populates the other, both present and different is 400; a project other than the caller's
// is 400 unless the caller is an administrator; a malformed or unknown project is 400.
func (svc *Service) ownerOf(c *gin.Context, projectID, tenantID *string) (string, bool) {
	caller := c.GetString("project_id")
	requested := projectID
	if requested == nil {
		requested = tenantID
	} else if tenantID != nil && *tenantID != *requested {
		common.SendError(c, common.NewBadRequestError("'project_id' and 'tenant_id' do not match"))
		return "", false
	}
	if requested == nil || *requested == "" || *requested == caller {
		return caller, true
	}
	if !isAdmin(c) {
		common.SendError(c, common.NewBadRequestError(
			"Specifying 'project_id' other than the authenticated project in request is not allowed for the current user."))
		return "", false
	}
	if uuid.Validate(*requested) != nil {
		common.SendError(c, common.NewBadRequestError("invalid project_id"))
		return "", false
	}
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(), "SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1)", *requested).Scan(&exists)
	if err != nil && !errors.Is(err, database.ErrNoRows) {
		log.Error().Err(err).Str("operation", "resolve_owner").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to resolve the project"))
		return "", false
	}
	if !exists {
		common.SendError(c, common.NewBadRequestError("invalid project_id"))
		return "", false
	}
	return *requested, true
}
