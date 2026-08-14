package nova

import (
	"fmt"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/gin-gonic/gin"
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
	if common.QueryFlag(c, "all_tenants") && common.HasRole(c, common.RoleAdmin) {
		return f
	}
	f.Eq(alias+"project_id", c.GetString("project_id"))
	return f
}
