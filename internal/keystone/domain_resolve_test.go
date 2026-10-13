package keystone

import (
	"context"
	"errors"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// The default domain's stored id, seeded by migration 009. Tests assert against
// the row the resolver actually finds rather than this constant wherever they can,
// so they stay honest if a deployment's default domain carries a different id —
// which is the whole point of resolving by name (FR-003, research R2).
const seededDefaultDomainID = "00000000-0000-0000-0000-000000000100"

// TestResolveDomainRef covers the single shared resolver every creation path and the
// domain read path go through (FR-001, FR-003, FR-008). The outcome is tri-state
// because FR-002 needs "the client named a domain that does not exist" (a 400 on
// creation, a 404 on retrieval) to be distinguishable from "the database is
// unreachable" (a 500 for both); TestResolveDomainRefQueryFailure pins the third.
func TestResolveDomainRef(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	ctx := t.Context()

	// Resolve the default domain once, by name, so the expectations below describe
	// the row that is actually seeded rather than a hard-coded id.
	wantDefault, outcome := resolveDomainRef(ctx, testPool, "", enabledDomainsOnly)
	if outcome != domainResolved {
		t.Fatalf("resolveDomainRef(\"\") outcome = %v, want domainResolved", outcome)
	}
	if wantDefault == "" {
		t.Fatal("resolveDomainRef(\"\") returned an empty id")
	}
	if wantDefault != seededDefaultDomainID {
		t.Logf("note: default domain id is %s, not the migration-009 value %s", wantDefault, seededDefaultDomainID)
	}

	tests := []struct {
		name        string
		ref         string
		wantID      string
		wantOutcome domainOutcome
	}{
		{"empty ref resolves to the default domain", "", wantDefault, domainResolved},
		{"the well-known alias resolves to the same domain", "default", wantDefault, domainResolved},
		{"a uuid naming an enabled domain resolves to itself", wantDefault, wantDefault, domainResolved},
		{"a well-formed uuid naming no domain is not found", "11111111-1111-1111-1111-111111111111", "", domainNotFound},
		{"a non-identifier string is not found", "not-a-domain", "", domainNotFound},
		{"the alias is case-sensitive", "DEFAULT", "", domainNotFound},
		{"a domain name is not an accepted reference", "Default", "", domainNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOutcome := resolveDomainRef(ctx, testPool, tc.ref, enabledDomainsOnly)
			if gotOutcome != tc.wantOutcome {
				t.Fatalf("resolveDomainRef(%q) outcome = %v, want %v", tc.ref, gotOutcome, tc.wantOutcome)
			}
			if gotID != tc.wantID {
				t.Fatalf("resolveDomainRef(%q) id = %q, want %q", tc.ref, gotID, tc.wantID)
			}
		})
	}
}

// TestResolveDomainRefRejectsDisabledDomain proves the resolver honours `enabled`,
// which is what lets project and group creation refuse a disabled domain the same
// way user creation already does (FR-002).
func TestResolveDomainRefRejectsDisabledDomain(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	ctx := t.Context()

	id := insertTestDomain(t, "spec022-disabled", false)
	if gotID, outcome := resolveDomainRef(ctx, testPool, id, enabledDomainsOnly); outcome != domainNotFound || gotID != "" {
		t.Fatalf("resolveDomainRef(disabled uuid) = (%q, %v), want (\"\", domainNotFound)", gotID, outcome)
	}

	// The same id, once enabled, must resolve — otherwise the test above could pass
	// for the wrong reason (e.g. the row not existing at all).
	enableTestDomain(t, id)
	if gotID, outcome := resolveDomainRef(ctx, testPool, id, enabledDomainsOnly); outcome != domainResolved || gotID != id {
		t.Fatalf("resolveDomainRef(enabled uuid) = (%q, %v), want (%q, domainResolved)", gotID, outcome, id)
	}
}

// TestResolveDomainRefAnyDomainState pins the difference between the two callers'
// questions. Creation asks "is this a valid home for a new resource", so a disabled
// domain is not an answer; retrieval asks "does this domain exist", and its response
// reports `enabled` as a field — so answering 404 there would hide information the
// response is meant to carry. GetDomain's query never filtered on enabled, and
// resolving the alias must not silently change that.
func TestResolveDomainRefAnyDomainState(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	ctx := t.Context()
	id := insertTestDomain(t, "spec022-anystate", false)

	if gotID, outcome := resolveDomainRef(ctx, testPool, id, anyDomainState); outcome != domainResolved || gotID != id {
		t.Fatalf("resolveDomainRef(disabled, anyDomainState) = (%q, %v), want (%q, domainResolved)", gotID, outcome, id)
	}
	if gotID, outcome := resolveDomainRef(ctx, testPool, id, enabledDomainsOnly); outcome != domainNotFound || gotID != "" {
		t.Fatalf("resolveDomainRef(disabled, enabledDomainsOnly) = (%q, %v), want (\"\", domainNotFound)", gotID, outcome)
	}
	// A malformed ref is still refused without a query, whichever state is asked for.
	if gotID, outcome := resolveDomainRef(ctx, testPool, "not-a-uuid", anyDomainState); outcome != domainNotFound || gotID != "" {
		t.Fatalf("resolveDomainRef(malformed, anyDomainState) = (%q, %v), want (\"\", domainNotFound)", gotID, outcome)
	}
}

// TestResolveDomainRefMalformedNeedsNoDatabase runs without the testDBAvailable
// guard: a non-identifier ref must be answered from the ref alone. Handing such a
// string to a `uuid`-typed column is what produces the 22P02 cast error surfacing as
// the 500 this feature exists to remove, so the syntax guard has to precede the
// query. With no database configured this test would fail on a nil pool if the
// implementation queried first; with one configured it still pins the return value.
func TestResolveDomainRefMalformedNeedsNoDatabase(t *testing.T) {
	if id, outcome := resolveDomainRef(t.Context(), testPool, "definitely-not-a-uuid", enabledDomainsOnly); outcome != domainNotFound || id != "" {
		t.Fatalf("resolveDomainRef(malformed) = (%q, %v), want (\"\", domainNotFound)", id, outcome)
	}
}

// TestResolveDomainRefQueryFailure pins the outcome no other test reaches: a lookup
// that fails for any reason other than "no such row" is domainQueryFailed, never
// domainNotFound, so callers answer 500 rather than blaming the client (FR-002). It
// needs no database, and covers both query branches — by name for the omitted ref
// and the alias, by id for a UUID.
func TestResolveDomainRefQueryFailure(t *testing.T) {
	pool := unreachablePool(t)
	for _, ref := range []string{"", "default", seededDefaultDomainID} {
		if id, outcome := resolveDomainRef(t.Context(), pool, ref, enabledDomainsOnly); outcome != domainQueryFailed || id != "" {
			t.Errorf("resolveDomainRef(%q) with the database unreachable = (%q, %v), want (\"\", domainQueryFailed)", ref, id, outcome)
		}
	}
}

// unreachablePool returns a database whose every statement fails as a refused
// connection would, standing in for a database outage without needing a database.
func unreachablePool(t *testing.T) database.DB {
	t.Helper()
	return unreachableDB{}
}

var errUnreachable = errors.New("dial tcp 127.0.0.1:1: connect: connection refused")

type unreachableDB struct{}

func (unreachableDB) Query(context.Context, string, ...any) (database.Rows, error) {
	return failedRows{}, errUnreachable
}
func (unreachableDB) QueryRow(context.Context, string, ...any) database.Row { return failedRow{} }
func (unreachableDB) Exec(context.Context, string, ...any) (database.Result, error) {
	return failedResult{}, errUnreachable
}
func (unreachableDB) Begin(context.Context) (database.Tx, error) { return nil, errUnreachable }
func (unreachableDB) Ping(context.Context) error                 { return errUnreachable }
func (unreachableDB) Close()                                     {}
func (unreachableDB) Backend() database.Backend                  { return database.Postgres }

type failedRows struct{}

func (failedRows) Next() bool        { return false }
func (failedRows) Scan(...any) error { return errUnreachable }
func (failedRows) Err() error        { return errUnreachable }
func (failedRows) Close()            {}

type failedRow struct{}

func (failedRow) Scan(...any) error { return errUnreachable }

type failedResult struct{}

func (failedResult) RowsAffected() int64 { return 0 }

// insertTestDomain adds a domain row and removes it when the test ends.
func insertTestDomain(t *testing.T, name string, enabled bool) string {
	t.Helper()
	var id string
	err := testPool.QueryRow(t.Context(),
		`INSERT INTO domains (name, description, enabled) VALUES ($1, 'spec022 fixture', $2) RETURNING id`,
		name, enabled,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert test domain %q: %v", name, err)
	}
	t.Cleanup(func() {
		// context.Background(), not t.Context(): the test context is cancelled just
		// before Cleanup-registered functions run (testing.T.Context).
		if _, err := testPool.Exec(context.Background(), `DELETE FROM domains WHERE id = $1`, id); err != nil {
			t.Logf("cleanup domain %s: %v", id, err)
		}
	})
	return id
}

func enableTestDomain(t *testing.T, id string) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), `UPDATE domains SET enabled = true WHERE id = $1`, id); err != nil {
		t.Fatalf("enable test domain %s: %v", id, err)
	}
}
