package console

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The reconciler is what ENFORCES token expiry (FR-010): websockify's TokenFile
// plugin resolves whatever entry it finds and never checks a clock, so a token dies
// when its projection file is removed, not when its expires_at passes. Store.
// RevokeExpired is tested directly in store_test.go; these cover what the
// reconciler adds on top of it.
//
// Not covered, deliberately: the claim that a transient database error is logged and
// the loop survives. sweep reaches for the package-global database.DB, so testing it
// would mean introducing an interface seam that exists only for the test.

// The startup sweep is the one with no second chance. An instance deleted while O3K
// was down cascades its rows away but not their projection files, and without a
// sweep before the first tick those entries stay resolvable for a whole interval —
// or forever, if the process is restarted often enough.
func TestReconcilerSweepsOnStartupBeforeTheFirstTick(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, "127.0.0.1", 5901)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	backdate(t, ctx, tok)

	// An hour-long interval proves the reap came from the startup sweep and not
	// from a tick: no ticker fires within this test.
	NewReconciler(s, time.Hour).sweep(ctx)

	if _, err := os.Stat(filepath.Join(s.dir, tok)); !os.IsNotExist(err) {
		t.Error("startup sweep left an expired projection entry — expiry is not enforced")
	}
	if _, err := s.Resolve(ctx, tok); err == nil {
		t.Error("expired token still resolves after the startup sweep")
	}
}

// A projection file whose row is gone is a live console for an instance that may no
// longer exist. The FK cascade removes rows behind O3K's back, so only the orphan
// sweep can catch the files.
func TestReconcilerSweepReapsOrphanedProjections(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, "127.0.0.1", 5901)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	deleteInstance(t, ctx, inst) // cascades the row away, leaves the file

	NewReconciler(s, time.Hour).sweep(ctx)

	if _, err := os.Stat(filepath.Join(s.dir, tok)); !os.IsNotExist(err) {
		t.Error("sweep left a projection entry with no surviving row — the proxy would still resolve it")
	}
}

// Run must return when its context is cancelled, so shutdown does not leave a
// goroutine sweeping against a closing pool.
//
// Needs a database despite testing only control flow: Run's first act is an
// unconditional startup sweep, which queries. Without requireDB this panics on a nil
// pool rather than skipping.
func TestReconcilerRunStopsOnContextCancel(t *testing.T) {
	requireDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		NewReconciler(NewStore(t.TempDir(), time.Minute), 10*time.Millisecond).Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
