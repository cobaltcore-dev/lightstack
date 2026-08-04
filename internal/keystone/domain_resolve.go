package keystone

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// defaultDomainAlias is the reference every OpenStack client sends for the Default
// domain, because upstream Keystone's Default domain literally carries `default` as
// its id. Lightstack cannot store that: the identity id columns are Postgres `uuid`, so the
// alias has to be resolved rather than persisted — which is why this file exists and
// why there is no simpler fix (SPEC-022 A1).
//
// Lightstack already emits the alias in its own token and S3-signature responses, so
// refusing it on input was self-inconsistent (cloud-storage#545 B5/P1).
const defaultDomainAlias = "default"

// defaultDomainName is what the row actually carries — the two halves of the mapping,
// named side by side, because the difference is exactly one letter's case and a reader
// who misses it will misread the resolver. Migrations 009 and 037 both seed a domain of
// this name (with different ids), which is why resolution goes by name; see
// resolveDomainRef.
const defaultDomainName = "Default"

// domainOutcome is why resolveDomainRef did or did not produce an id.
type domainOutcome int

const (
	// domainResolved: the reference names an existing domain.
	domainResolved domainOutcome = iota
	// domainNotFound: the reference is malformed, or names no domain the caller asked
	// for. The caller's answer is a client error — the request said something wrong.
	domainNotFound
	// domainQueryFailed: the lookup itself failed. The caller's answer is a server
	// error — the request may well have been fine.
	domainQueryFailed
)

// domainState says whether a disabled domain counts as resolved. The two callers ask
// genuinely different questions, and collapsing them would be a behaviour change in
// one direction or the other, so the choice is explicit at every call site.
type domainState int

const (
	// enabledDomainsOnly: the domain is about to be used as the home of a new
	// resource, so a disabled one is not a valid answer. Creating a project, a user or
	// a group.
	enabledDomainsOnly domainState = iota
	// anyDomainState: the caller only needs to know the domain exists. Retrieval
	// reports `enabled` as a field, so hiding a disabled domain behind a 404 would
	// lose information the response is supposed to carry.
	anyDomainState
)

// resolveDomainRef resolves a domain reference from a request to the id of an existing,
// enabled domain. It is the single shared implementation for every path that accepts
// one — creating a project, a user or a group, and retrieving a domain by id — so those
// paths cannot disagree about what a valid domain is (FR-008). It mirrors
// common.ResolveProjectRef, the equivalent for project references in URL paths.
//
// Three reference forms all name the same thing (FR-001, FR-003):
//
//   - "" (omitted) and the `default` alias resolve the deployment's Default domain
//     **by name**, not by a hard-coded id. That matters: migrations 009 and 037 both
//     seed a domain named `Default` but with *different* ids (…0100 and …0001), and
//     009 wins only by running first. Resolving by name makes the outcome independent
//     of migration ordering, and lets a deployment whose Default domain carries some
//     other id keep working.
//   - a UUID resolves that domain directly.
//   - anything else is not found, and deliberately without a query: handing a
//     non-UUID to a `uuid`-typed column is what raises Postgres 22P02 and surfaces as
//     the generic 500 this feature exists to remove.
//
// Like common.ResolveProjectRef, this separates an unknown reference from a failed
// lookup: a client naming a domain that does not exist is a client error, while an
// unreachable database is a 500 (FR-002). It returns a three-state outcome rather than
// a ready-made error because the callers disagree on the client-error status —
// creation answers 400, retrieval 404. CreateUser already made that distinction by
// hand; this makes it available to every caller.
//
// `state` decides whether a disabled domain resolves; see domainState. The Default
// domain is always matched by name regardless, so a deployment that disables its
// Default domain fails creation for the right reason rather than resolving to nothing.
func resolveDomainRef(ctx context.Context, pool *pgxpool.Pool, ref string, state domainState) (string, domainOutcome) {
	enabledOnly := ""
	if state == enabledDomainsOnly {
		enabledOnly = " AND enabled = true"
	}

	// Both forms bind exactly one parameter — the name for the alias, the id otherwise.
	// The name is bound rather than inlined because the lookup this consolidated did the
	// same (CreateUser, pre-SPEC-022) and because a literal in the string is one more
	// thing to notice when reading the query.
	query := "SELECT id FROM domains WHERE name = $1" + enabledOnly
	arg := any(defaultDomainName)

	if ref != "" && ref != defaultDomainAlias {
		if uuid.Validate(ref) != nil {
			// Not an id and not the alias: nothing it could name. Answer without a
			// query so the malformed value never reaches a uuid column.
			return "", domainNotFound
		}
		query = "SELECT id FROM domains WHERE id = $1" + enabledOnly
		arg = ref
	}

	var id string
	if err := pool.QueryRow(ctx, query, arg).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domainNotFound
		}
		log.Error().Err(err).Str("ref", ref).Msg("failed to resolve domain reference")
		return "", domainQueryFailed
	}
	return id, domainResolved
}
