package common

import (
	"fmt"
	"slices"
	"strings"
)

// Positional-argument bookkeeping for hand-built SQL.
//
// Every service builds queries by appending predicates or assignments and numbering the placeholders
// that read them. Written out longhand that is three statements per field — append the fragment,
// append the value, increment a counter — with the counter and the value list kept in step by hand:
//
//	updates = append(updates, fmt.Sprintf("name = $%d", argIdx))
//	args = append(args, *req.Name)
//	argIdx++
//
// Nothing checks that the number in the fragment matches the position of the value it reads. When
// they disagree the query either fails at the database with "there is no parameter $n" or, when the
// two values happen to share a type, silently reads the wrong one — a wrong column written on an
// UPDATE, a wrong row matched on a SELECT.
//
// SQLArgs removes the counter: a placeholder can only be obtained by appending the value it refers
// to, so the two cannot drift. Keystone's identity filters and Nova's server filter each grew their
// own copy of this; Neutron grew a third that still counted by hand. This is the shared core.
type SQLArgs struct {
	args []any
}

// Arg appends a value and returns the placeholder that reads it.
func (a *SQLArgs) Arg(v any) string {
	a.args = append(a.args, v)
	return fmt.Sprintf("$%d", len(a.args))
}

// Args returns the accumulated values, in placeholder order, ready to pass to the driver.
func (a *SQLArgs) Args() []any { return a.args }

// SQLFilter accumulates WHERE predicates alongside the arguments they read.
//
// Conditions are joined with AND. A filter with no conditions renders an empty string rather than a
// bare WHERE, so an unfiltered request produces exactly the query it produced before any filtering
// existed — the property that lets a filter be added to a listing without changing its unfiltered
// behaviour.
type SQLFilter struct {
	SQLArgs
	conds []string
}

// Cond appends an already-rendered predicate. Use it for anything Eq cannot express — a cast, a
// disjunction, a subquery — obtaining placeholders from Arg so the numbering still cannot drift.
func (f *SQLFilter) Cond(cond string) { f.conds = append(f.conds, cond) }

// Eq appends `column = $n`. column carries its own table alias where the query needs one, so a
// caller never rewrites the rendered SQL to insert it afterwards.
func (f *SQLFilter) Eq(column string, v any) { f.Cond(column + " = " + f.Arg(v)) }

// Where renders the accumulated predicates, or "" when there are none.
func (f *SQLFilter) Where() string {
	if len(f.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(f.conds, " AND ")
}

// Conds renders the predicates without the WHERE keyword, for a query that already has one.
func (f *SQLFilter) Conds() string { return strings.Join(f.conds, " AND ") }

// CloneArgs returns a filter carrying a COPY of this one's arguments and none of its conditions.
//
// For a derived query that reuses the original's WHERE but appends arguments of its own — a marker
// lookup wrapping the scoped listing, say, where the marker predicate sits outside the subquery.
// Appending to the original instead would leave a stray value in its argument list and the driver
// would reject the next statement built from it, so the copy is what keeps the two independent.
func (f *SQLFilter) CloneArgs() *SQLFilter {
	return &SQLFilter{SQLArgs: SQLArgs{args: slices.Clone(f.args)}}
}

// Len reports how many predicates have been added.
func (f *SQLFilter) Len() int { return len(f.conds) }

// SQLUpdate accumulates the SET assignments of an UPDATE alongside their arguments.
//
// Separate from SQLFilter because the two are joined differently — "," against " AND " — and because
// an UPDATE with no assignments must not run at all, which Empty reports.
type SQLUpdate struct {
	SQLArgs
	sets []string
}

// Set appends `column = $n`.
func (u *SQLUpdate) Set(column string, v any) { u.sets = append(u.sets, column+" = "+u.Arg(v)) }

// SetExpr appends an assignment that reads no argument, such as `updated_at = NOW()`.
func (u *SQLUpdate) SetExpr(assignment string) { u.sets = append(u.sets, assignment) }

// Assignments renders the SET list without the keyword.
func (u *SQLUpdate) Assignments() string { return strings.Join(u.sets, ", ") }

// Empty reports whether nothing would be assigned. An UPDATE built from an empty set is not a
// no-op — it is a syntax error — so callers check this before running the statement.
func (u *SQLUpdate) Empty() bool { return len(u.sets) == 0 }
