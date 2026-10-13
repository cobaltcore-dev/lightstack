package nova

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// serverFilter accumulates the predicates a server listing runs under, together
// with their arguments, so the page query and the marker lookup cannot disagree
// about scope — a disagreement that previously made a cross-tenant marker fail to
// resolve and silently return page 1.
//
// The accumulation is common.SQLFilter, shared with Keystone's identity filters
// and Neutron's listings. The earlier form emitted a fixed
// " WHERE project_id = $n" and left callers to compensate: one rewrote the
// generated SQL with strings.Replace to add a table alias, another wrapped it in
// a closure to decide between WHERE and AND. Both were the caller working around
// what the helper could not say, and the string rewrite would have silently
// patched the wrong token the moment a second predicate appeared.
//
// Only the two methods below are peculiar to compute: a composite keyset over the
// listing's sort key, and the paging clause.
type serverFilter struct {
	common.SQLFilter
}

// beforeKeyset appends a keyset comparison over (created_at, id) — the exclusive
// "everything after the marker" predicate.
//
// The comparison is composite because the ORDER BY is: created_at alone is not
// unique, so a plain `<` can skip or repeat rows sharing a timestamp, which a
// cross-tenant listing makes far more likely (FR-005).
func (f *serverFilter) beforeKeyset(alias string, createdAt time.Time, id string) {
	f.Cond(fmt.Sprintf("(%screated_at, %sid) < (%s, %s)",
		alias, alias, f.Arg(createdAt), f.Arg(id)))
}

// limitOffset appends the paging values and renders the clause that reads them,
// so the two cannot drift apart.
func (f *serverFilter) limitOffset(limit, offset int) string {
	return fmt.Sprintf(" LIMIT %s OFFSET %s", f.Arg(limit), f.Arg(offset))
}

// newServerFilter starts a filter scoped to the caller's project, unless the
// caller is an administrator who explicitly asked for a cross-tenant view.
//
// Widening is opt-in and administrator-only (SPEC-023 FR-005). Holding the admin
// role is NOT enough on its own: an operator's ordinary "my servers" view must
// keep showing their own servers, or every existing admin client silently starts
// seeing the whole deployment. Upstream's `all_tenants` is the opt-in, and
// common.QueryFlag parses it upstream's way — the valueless `?all_tenants` form
// Horizon sends included.
//
// A non-administrator passing all_tenants is IGNORED, not refused (FR-005b).
// Refusing would tell them the parameter exists and is gated; ignoring makes the
// answer identical to the same request without it, which is also what upstream
// does with administrator-only query parameters.
//
// alias is the table alias the query uses ("" when it has none), so the column
// is correct at the point it is written rather than patched afterwards.
func newServerFilter(c *gin.Context, alias string) *serverFilter {
	f := &serverFilter{}
	project, all := listScope(c)
	if all {
		return f
	}
	// An unscoped token names no project, so it owns no server. Bound as "" the
	// empty project would fail the uuid cast and the listing would answer 500.
	if project == "" {
		f.Cond("FALSE")
		return f
	}
	f.Eq(alias+"project_id", project)
	return f
}

// listScope is the project a server listing is confined to: every project for an
// administrator who opted in with all_tenants (all), otherwise the caller's own — ""
// for an unscoped token, which owns no server.
func listScope(c *gin.Context) (project string, all bool) {
	if common.QueryFlag(c, "all_tenants") && common.HasRole(c, common.RoleAdmin) {
		return "", true
	}
	return c.GetString("project_id"), false
}

// Server-listing query filters (SPEC-025-followup US1, FR-001 – FR-009).
//
// The listing accepted upstream's filter parameters and applied none of them, so
// `openstack server list --name web` returned the whole project and the caller read the
// first row as the match. Nothing signalled it: the answer is a well-formed page of other
// servers. This is the third time the shape has appeared in this product — the usage
// report and six identity collections had it before — so the accumulation here is the one
// keystone/list_filters.go established, on the common.SQLFilter this type already embeds.
//
// Two properties are load-bearing and cheap to lose:
//
//   - An ABSENT parameter appends nothing, so an unfiltered listing renders exactly the
//     query it rendered before this existed (FR-007).
//   - A filter only ever ANDs onto the scope predicate, so it can only narrow. There is no
//     path by which a filter replaces or widens scope (FR-006); the administrator-only
//     cross-tenant opt-in remains the only way to see past one's own project.

// refusedListFilters names the parameters upstream supports and this product cannot,
// because it records none of these attributes for a server (FR-004).
//
// `ip` is NOT here: FR-003b's condition is met — US7's batched ports query exists, so the
// address data is reachable and the filter is honoured below rather than refused. It was
// refused in the interim precisely so it could never be *ignored*, which is the one answer
// FR-004 rules out for a filter in either state.
//
// Refusing is a deliberate divergence from upstream, which honours `host`/`hostname` for
// administrators and SILENTLY IGNORES them for everyone else. Both upstream answers are
// silent, and a silent answer is what this feature exists to remove: an empty list reads
// as a fact about the deployment, an ignored filter hands back somebody else's servers.
// Recorded as divergence 4 in the constitution's Known Divergences register.
var refusedListFilters = []string{"host", "hostname", "availability_zone", "tags"}

// honouredListFilters are the filters EVERY caller may use, named here so the refusal
// message can be built from them instead of repeating a hand-written copy.
//
// The copy had already drifted: `ip` spent the first part of this feature in
// refusedListFilters and the rest of it honoured, and the message went on advertising the
// list from before the move — so the error whose job is to tell a caller what it CAN
// filter on was telling it `ip` was unavailable.
var honouredListFilters = []string{"name", "status", "image", "flavor", "changes-since", "ip"}

// adminListFilters are honoured only for a caller holding the admin role.
//
// They are named in a refusal ONLY to such a caller. applyAdminFilters ignores rather than
// refuses them for everyone else, precisely so their existence is not advertised —
// "refusing advertises that the parameter exists and is gated" — and a message listing
// them to every tenant would undo that decision from a few lines away.
var adminListFilters = []string{"project_id", "tenant_id", "user_id", "key_name", "task_state", "locked", "power_state"}

// supportedListFilters is what a refusal should name for this caller: the common set, plus
// the administrator-only ones when the caller can actually use them.
func supportedListFilters(c *gin.Context) []string {
	if !common.HasRole(c, common.RoleAdmin) {
		return honouredListFilters
	}
	all := make([]string, 0, len(honouredListFilters)+len(adminListFilters))
	all = append(all, honouredListFilters...)
	return append(all, adminListFilters...)
}

// applyQueryFilters appends a predicate for every filter parameter present, and returns a
// client error for a parameter naming an attribute the product cannot record.
//
// alias is the table alias the query uses ("" or "i."), passed rather than patched in
// afterwards for the same reason newServerFilter takes it.
//
// The ORDER of the four steps is load-bearing, not cosmetic:
//
//   - refusals run first, so a request naming an unrecordable attribute is answered
//     before any predicate is built and the caller is told about the filter rather than
//     handed a page that silently ignored it;
//   - the value-validating filters run before the administrator-only ones, so a malformed
//     pattern or timestamp is reported even to a caller whose later parameters would be
//     ignored — a client error should not depend on the caller's role.
//
// Predicates themselves are ANDed, so their relative order does not affect the result;
// only the placeholder numbering follows the call sequence, and args are appended with
// their placeholders.
func (f *serverFilter) applyQueryFilters(c *gin.Context, db database.DB, alias string) *common.OpenStackError {
	if c == nil || c.Request == nil {
		return nil
	}
	if err := f.refuseUnrecordable(c); err != nil {
		return err
	}
	if err := f.applyRegexFilters(c, db, alias); err != nil {
		return err
	}
	if err := f.applyScalarFilters(c, alias); err != nil {
		return err
	}
	return f.applyAdminFilters(c, alias)
}

// refuseUnrecordable answers a request that filters on an attribute this deployment does
// not record. Nothing is appended: the request is refused, not narrowed.
func (f *serverFilter) refuseUnrecordable(c *gin.Context) *common.OpenStackError {
	for _, name := range refusedListFilters {
		if c.Query(name) != "" {
			return common.NewBadRequestErrorWithDetails(
				fmt.Sprintf("Unsupported filter: %s", name),
				fmt.Sprintf("This deployment does not record %q for a server, so it cannot filter on it. Supported filters: %s.",
					name, strings.Join(supportedListFilters(c), ", ")),
			)
		}
	}
	return nil
}

// applyRegexFilters handles the two filters upstream matches as regular expressions.
//
// Both are validated in Go first: an invalid pattern is a client error, where an unguarded
// one becomes `invalid regular expression` from the driver and surfaces as a 500 (FR-008).
// Go's RE2 and PostgreSQL's POSIX syntax are not identical, so a pattern valid in one and
// not the other is refused rather than run — the conservative direction. `name` is checked
// first, so a request with two bad patterns is told about the same one it always was.
func (f *serverFilter) applyRegexFilters(c *gin.Context, db database.DB, alias string) *common.OpenStackError {
	// name is a REGULAR EXPRESSION over the display name, which is upstream's semantics
	// ("exact and regex matching") — not equality and not a substring match (FR-003a). A
	// substring match would return rows a regex would not, which is the same class of
	// wrong-but-plausible answer being fixed here.
	//
	// The database matches it, as upstream's does (nova/db/main/api.py): PostgreSQL with
	// its own regular expressions, SQLite with the application's. An escape the two
	// dialects read differently follows the backend.
	if v := c.Query("name"); v != "" {
		if _, err := regexp.Compile(v); err != nil {
			return common.NewInvalidValueError("name", v, "a valid regular expression")
		}
		f.Cond(alias + "name ~ " + f.Arg(v))
	}

	// ip matches a server's FIXED addresses, which live in ports.fixed_ips (FR-003b).
	// Matched as a regular expression as upstream does, so `?ip=^10\.0\.` narrows to a
	// subnet — and, as upstream does, here rather than in the database
	// (nova/compute/api.py): the ports in the listing's scope are read, each address is
	// matched in Go, and the servers holding a match become an id predicate.
	if v := c.Query("ip"); v != "" {
		re, err := regexp.Compile(v)
		if err != nil {
			return common.NewInvalidValueError("ip", v, "a valid regular expression")
		}
		ids, err := serversWithFixedIP(c.Request.Context(), db, c, re)
		if err != nil {
			log.Error().Err(err).Str("operation", "list_servers").Msg("resolving the ip filter failed")
			return common.NewInternalServerError("failed to list servers")
		}
		f.Cond(alias + "id = ANY(" + f.Arg(ids) + "::uuid[])")
	}
	return nil
}

// serversWithFixedIP returns the servers in the listing's scope with a port holding a
// fixed address re matches.
//
// A port counts for a server only when the two belong to the same project — the pairing
// instanceAddressMap states as a join condition, for the same reason it gives:
// `ports.device_id` has no foreign key (migration 001) and CreatePort takes it from the
// request body unvalidated, so a port whose project disagrees with its instance's is
// reachable. Without the pairing `?ip=X` returned a server whose own address map did not
// contain X. Two readers of one relation have to select on the same basis or the listing
// and the detail view disagree, which is what US7 exists to prevent.
func serversWithFixedIP(ctx context.Context, db database.DB, c *gin.Context, re *regexp.Regexp) ([]string, error) {
	project, all := listScope(c)
	if !all && project == "" {
		return []string{}, nil
	}
	rows, err := portFixedIPs(ctx, db, project, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return serversMatching(rows, re)
}

// portFixedIPs queries each server id with its port's fixed_ips, scoped to project unless all is
// set. A server's port counts only when the two share a project (ports.device_id is unvalidated).
func portFixedIPs(ctx context.Context, db database.DB, project string, all bool) (database.Rows, error) {
	q := `SELECT i.id::text, p.fixed_ips
		FROM instances i
		JOIN ports p ON p.device_id = i.id AND p.project_id = i.project_id`
	var args []any
	if !all {
		q += ` WHERE i.project_id = $1`
		args = append(args, project)
	}
	return db.Query(ctx, q, args...)
}

// serversMatching returns the distinct server ids holding a fixed address that matches re.
func serversMatching(rows database.Rows, re *regexp.Regexp) ([]string, error) {
	ids := []string{}
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		var fixedIPs common.FixedIPs
		if err := rows.Scan(&id, &fixedIPs); err != nil {
			return nil, err
		}
		for _, ip := range fixedIPs {
			if !seen[id] && re.MatchString(ip.IPAddress) {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, rows.Err()
}

// applyScalarFilters handles the filters that compare a single column to a single value.
func (f *serverFilter) applyScalarFilters(c *gin.Context, alias string) *common.OpenStackError {
	// status compares case-insensitively against the vocabulary the listing itself emits,
	// so `--status active` behaves like `--status ACTIVE` rather than silently matching
	// nothing.
	if v := c.Query("status"); v != "" {
		f.Cond("UPPER(" + alias + "status) = UPPER(" + f.Arg(v) + ")")
	}

	// image and flavor are uuid columns compared as TEXT deliberately: an id column
	// compared against a non-UUID string makes PostgreSQL raise `invalid input syntax for
	// type uuid`, which reaches the client as a 500. Casting means a malformed id simply
	// matches nothing, which is the answer upstream gives for an id that does not exist.
	if v := c.Query("image"); v != "" {
		f.Cond(alias + "image_id::text = " + f.Arg(v))
	}
	if v := c.Query("flavor"); v != "" {
		f.Cond(alias + "flavor_id::text = " + f.Arg(v))
	}

	// changes-since selects servers updated at or after the given time, as upstream does.
	// A malformed timestamp is refused: this is an honoured filter, so a value that cannot
	// be applied must not be silently dropped (FR-008).
	if v := c.Query("changes-since"); v != "" {
		ts, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return common.NewInvalidValueError("changes-since", v, "an RFC 3339 timestamp")
		}
		f.Cond(alias + "updated_at >= " + f.Arg(ts))
	}
	return nil
}

// applyAdminFilters handles project_id/tenant_id, user_id, key_name, task_state, locked
// and power_state.
//
// A non-administrator passing one is IGNORED rather than refused — the same treatment
// all_tenants gets, and for the same reason: refusing advertises that the parameter exists
// and is gated, where ignoring makes the answer identical to the same request without it.
//
// Each ANDs onto whatever scope is already in place, which is what makes FR-006a fall out
// for free: an administrator naming another project WITHOUT the cross-tenant opt-in gets
// `project_id = own AND project_id = other`, an empty collection — the correct answer to a
// request for another project's servers made without asking to see other projects. With
// the opt-in, scope adds nothing and the same predicate narrows the cross-tenant view to
// that one project.
func (f *serverFilter) applyAdminFilters(c *gin.Context, alias string) *common.OpenStackError {
	if !common.HasRole(c, common.RoleAdmin) {
		return nil
	}

	if v := firstQuery(c, "project_id", "tenant_id"); v != "" {
		f.Cond(alias + "project_id::text = " + f.Arg(v))
	}
	if v := c.Query("user_id"); v != "" {
		f.Cond(alias + "user_id::text = " + f.Arg(v))
	}
	if v := c.Query("key_name"); v != "" {
		f.Eq(alias+"key_name", v)
	}
	if v := c.Query("task_state"); v != "" {
		f.Eq(alias+"task_state", v)
	}
	if b, ok := common.ParseQueryBool(c.Query("locked")); ok {
		f.Eq(alias+"locked", b)
	}
	if v := c.Query("power_state"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return common.NewInvalidValueError("power_state", v, "an integer")
		}
		f.Eq(alias+"power_state", n)
	}
	return nil
}

// firstQuery returns the first of the given parameters that is present and non-empty.
// Upstream accepts both project_id and tenant_id for the same filter.
func firstQuery(c *gin.Context, names ...string) string {
	for _, n := range names {
		if v := c.Query(n); v != "" {
			return v
		}
	}
	return ""
}
