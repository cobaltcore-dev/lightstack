package cinder

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Unit-level half of US3, separated from the HTTP-level tests deliberately: it names
// package-internal symbols (withVolumeQuota, QuotaExceededError), so it cannot compile
// against a tree without the implementation. Keeping it apart lets the route-level tests
// be run against the UNMODIFIED code to prove they fail — which is the only way "confirm
// RED" means anything for a story whose implementation was written first.

// TestQuotaDenialIsRecorded is T059: FR-029a. A denial is the one refusal in this feature
// that is logged — actionable by whoever configured the limit, and invisible to them
// otherwise.
func TestQuotaDenialIsRecorded(t *testing.T) {
	requireDB(t)
	seedQuotaProject(t)
	setQuotaLimit(t, "volumes", 0)

	// The error carries the numbers an operator needs; sendQuotaError is what puts them in
	// the log, and this asserts they are all present to be logged.
	//
	// The write closure also asserts it is never reached: a refusal must not perform the
	// write it was asked to authorise. That is free here, and it is the property the whole
	// helper exists for.
	wrote := false
	err := withVolumeQuota(context.Background(), testPool, quotaProject, 1, func(pgx.Tx) error {
		wrote = true
		return nil
	})
	if err == nil {
		t.Fatal("a zero limit accepted a create")
	}
	if wrote {
		t.Error("the write ran despite the quota refusal; the check must gate it, not precede it")
	}
	var qe *QuotaExceededError
	if !errors.As(err, &qe) {
		t.Fatalf("error %v is not a QuotaExceededError; the denial cannot be reported without it", err)
	}
	if qe.Resource == "" || qe.Limit != 0 || qe.Requested != 1 {
		t.Errorf("denial = %+v; want resource, limit 0 and requested 1 for the operator to act on", qe)
	}
	if qe.QuotaResource() != qe.Resource {
		t.Errorf("QuotaResource() = %q, Resource = %q; compute reads the former without importing this package", qe.QuotaResource(), qe.Resource)
	}
}
