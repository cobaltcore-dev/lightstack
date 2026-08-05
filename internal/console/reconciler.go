package console

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// Reconciler keeps the projection directory truthful.
//
// # Why this exists at all
//
// websockify's TokenFile plugin has no notion of expiry — it resolves whatever
// entry it finds. So a console token stops working when O3K deletes its file,
// not when its expires_at passes. Reaping is the enforcement mechanism for
// FR-010, not housekeeping, and it cannot be left to the next console request:
// between a token expiring and the next request (possibly hours) the token
// would stay live.
//
// This is a reconciler, not a job worker: no queue, no work items, no retries
// with backoff. It compares O3K's own projection against O3K's own table, in
// the same spirit as the real-mode network reconciler from SPEC-009 WS-2.
//
// Consequence worth knowing: effective maximum token validity is
// TTL + Interval, not TTL exactly.
type Reconciler struct {
	store    *Store
	interval time.Duration
}

// NewReconciler returns a Reconciler sweeping at the given interval.
func NewReconciler(store *Store, interval time.Duration) *Reconciler {
	return &Reconciler{store: store, interval: interval}
}

// Run performs one immediate sweep and then repeats until ctx is cancelled.
//
// The startup sweep matters on its own: an unclean shutdown can leave entries
// whose rows are gone (an instance deleted while O3K was down cascades the row
// away but not the file), and those would otherwise stay resolvable forever.
func (r *Reconciler) Run(ctx context.Context) {
	r.sweep(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("console-token reconciler stopped")
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

// sweep reaps expired tokens and orphaned projection entries.
//
// Errors are logged and the loop continues: a transient database blip must not
// kill the goroutine that enforces token expiry.
func (r *Reconciler) sweep(ctx context.Context) {
	n, err := r.store.RevokeExpired(ctx)
	if err != nil {
		log.Error().Err(err).Msg("console-token reconciler: reaping expired tokens failed")
	} else if n > 0 {
		log.Info().Int("reaped", n).Msg("console-token reconciler: expired tokens removed")
	}

	if err := r.store.SweepOrphans(ctx); err != nil {
		log.Error().Err(err).Msg("console-token reconciler: sweeping orphaned projections failed")
	}
}
