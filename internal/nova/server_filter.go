package nova

import (
	"fmt"
	"strings"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/gin-gonic/gin"
)

// serverFilter accumulates the predicates a server listing runs under, together
// with their arguments, so the page query and the marker lookup cannot disagree
// about scope — a disagreement that previously made a cross-tenant marker fail to
// resolve and silently return page 1.
//
// Placeholder numbers are always derived from len(args) as each value is
// appended, never counted by hand. The earlier form emitted a fixed
// " WHERE project_id = $n" and left callers to compensate: one rewrote the
// generated SQL with strings.Replace to add a table alias, another wrapped it in
// a closure to decide between WHERE and AND. Both were the caller working around
// what the helper could not say, and the string rewrite would have silently
// patched the wrong token the moment a second predicate appeared.
//
// This is the same shape keystone's listFilter uses for identity collections.
// The two are deliberately not yet merged: the shared core is ~17 lines, and
// extracting it needs accessors that exist only to satisfy embedding across a
// package boundary. Revisit when a third service grows a filter.
type serverFilter struct {
	conds []string
	args  []any
}

// arg appends a value and returns its placeholder, so a predicate written inline
// can never disagree with the argument list it reads from.
func (f *serverFilter) arg(v any) string {
	f.args = append(f.args, v)
	return fmt.Sprintf("$%d", len(f.args))
}

// eq appends `column = $n`. column carries its own table alias where the query
// needs one, so the caller never rewrites the rendered SQL to add it.
func (f *serverFilter) eq(column string, v any) {
	f.conds = append(f.conds, column+" = "+f.arg(v))
}

// beforeKeyset appends a keyset comparison over (created_at, id) — the exclusive
// "everything after the marker" predicate.
//
// The comparison is composite because the ORDER BY is: created_at alone is not
// unique, so a plain `<` can skip or repeat rows sharing a timestamp, which a
// cross-tenant listing makes far more likely (FR-005).
func (f *serverFilter) beforeKeyset(alias string, createdAt time.Time, id string) {
	f.args = append(f.args, createdAt, id)
	f.conds = append(f.conds, fmt.Sprintf("(%screated_at, %sid) < ($%d, $%d)",
		alias, alias, len(f.args)-1, len(f.args)))
}

// where renders the accumulated predicates, or "" when there are none.
func (f *serverFilter) where() string {
	if len(f.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(f.conds, " AND ")
}

// limitOffset appends the paging values and renders the clause that reads them,
// so the two cannot drift apart.
func (f *serverFilter) limitOffset(limit, offset int) string {
	f.args = append(f.args, limit, offset)
	return fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(f.args)-1, len(f.args))
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
	f.eq(alias+"project_id", c.GetString("project_id"))
	return f
}
