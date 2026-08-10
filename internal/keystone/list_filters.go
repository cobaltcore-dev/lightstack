package keystone

import (
	"fmt"
	"strings"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/gin-gonic/gin"
)

// Identity collection filtering (SPEC-023 US5, FR-024 – FR-027).
//
// Six collections — roles, users, projects, groups, services, endpoints — read
// NO query parameters at all and returned the whole collection whatever was
// asked for. Because the answer is a well-formed list rather than an error,
// nothing signalled that the filter had been discarded, so a caller taking the
// first entry silently got the wrong one.
//
// That is how the multi-tenancy suite came to grant the ADMINISTRATIVE role to
// both of its test users for as long as it did: it asked for the member role by
// name, got all three roles back, and took `.roles[0]` — which sorts to `admin`.
// Every isolation assertion in that suite was a tautology as a result.
//
// The shape here is the one #545 established on the domain collection: build the
// WHERE conditionally from the parameters that are present, leaving the
// unfiltered path byte-identical (FR-026).

// listFilter accumulates equality predicates and their arguments.
type listFilter struct {
	conds []string
	args  []any
}

// arg appends a value and returns its placeholder, so a predicate can never
// disagree with the argument list it reads from. Same shape as serverFilter.arg
// (internal/nova/authz.go): placeholder numbers are derived from len(args) as
// each value is appended, never counted by hand.
func (f *listFilter) arg(v any) string {
	f.args = append(f.args, v)
	return fmt.Sprintf("$%d", len(f.args))
}

// add appends `column = $n` for an already-resolved value.
func (f *listFilter) add(column string, v any) {
	f.conds = append(f.conds, column+" = "+f.arg(v))
}

// eq adds `column = $n` when the query parameter is present and non-empty.
// Absent parameters add nothing, so an unfiltered request produces exactly the
// query it produced before this feature.
func (f *listFilter) eq(c *gin.Context, param, column string) {
	if v := c.Query(param); v != "" {
		f.add(column, v)
	}
}

// eqBool adds a boolean predicate, accepting upstream's vocabulary — the same
// words common.QueryFlag accepts, so `?enabled=yes` behaves like
// `?all_tenants=yes` rather than being silently dropped. strconv.ParseBool is not
// enough for this: it rejects on/off/y/yes/no, all of which real clients send.
//
// An unrecognised value is ignored rather than treated as false, so a typo
// returns an unfiltered collection rather than a wrong subset. That is why this
// uses ParseQueryBool's two-value form instead of QueryFlag, which fails closed.
//
// An absent parameter needs no separate guard: ParseQueryBool rejects "" as
// unrecognised, so the empty case is already the ignored case.
func (f *listFilter) eqBool(c *gin.Context, param, column string) {
	if b, ok := common.ParseQueryBool(c.Query(param)); ok {
		f.add(column, b)
	}
}

// eqDomain adds a domain predicate, accepting the well-known `default` alias
// every OpenStack client sends. Without this a filter on the alias would match
// nothing and return an empty collection — technically honouring the filter, but
// wrong, and worse than the unfiltered answer it replaces.
//
// The alias resolves against the domains table rather than a hardcoded id,
// matching resolveDomainRef — the deployment's Default domain is whatever the
// database says it is. An absent parameter matches neither case and so adds
// nothing.
func (f *listFilter) eqDomain(c *gin.Context, param, column string) {
	switch v := c.Query(param); {
	case v == defaultDomainAlias:
		f.conds = append(f.conds, fmt.Sprintf(
			"%s = (SELECT id FROM domains WHERE name = %s)", column, f.arg(defaultDomainName)))
	case v != "":
		f.add(column, v)
	}
}

// where renders the accumulated predicates, or "" when none are present.
func (f *listFilter) where() string {
	if len(f.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(f.conds, " AND ")
}
