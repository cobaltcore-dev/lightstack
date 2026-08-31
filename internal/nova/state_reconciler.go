package nova

import (
	"context"
	"errors"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/rs/zerolog/log"
)

// StateReconciler keeps the recorded run state of real-mode servers honest
// (SPEC-025 US4, FR-024b).
//
// This is a reconciler, not a job worker: no queue, no work items, no retries with
// backoff — the same shape as the real-mode network reconciler (SPEC-009), and
// permitted explicitly by Constitution v1.1.0.
//
// It exists for drift the product did NOT cause. Every action that changes a guest's
// run state now persists its own result (FR-024a), so a stop or a start needs no
// sweep. What needs one is a guest that crashed, or one removed out of band: nothing
// in the request path will ever hear about those, and until this ran the API went on
// reporting such a server as running (cloud-storage#547 B12).
//
// Reading a server deliberately does NOT contact the hypervisor (FR-024c) — a listing
// would then cost one hypervisor round trip per server returned. The price of that
// choice is that truth is bounded by this interval rather than instantaneous, which
// is why SC-006 states a window instead of claiming zero.
type StateReconciler struct {
	svc      *Service
	interval time.Duration
	// retention is how long a server's action records are kept after the server
	// itself is deleted (FR-042a). Zero disables reclamation entirely, which is what
	// a caller that only wants state reconciliation gets.
	retention time.Duration
}

// NewStateReconciler returns a reconciler sweeping at the given interval, clamping a
// non-positive value to the default so a deployment that configures 0 gets a sane
// period rather than a busy loop.
//
// The clamp is deliberately duplicated with LoadConfig's: a reconciler can be built
// without a loaded config — every test does — so the constructor cannot assume the value
// has already been sanitised. The CONSTANT is shared even though the check is not, which
// is what keeps both clamps agreeing with the 30s the chart schema publishes as contract.
func NewStateReconciler(svc *Service, interval time.Duration) *StateReconciler {
	if interval <= 0 {
		interval = common.DefaultStateReconcileInterval
	}
	return &StateReconciler{svc: svc, interval: interval}
}

// WithActionRetention enables reclamation of action records older than the given age,
// returning the receiver so it can be chained onto the constructor.
//
// A builder rather than a third constructor parameter because that is what the callers
// want: ten of the thirteen construction sites in this repo ask for no retention at all,
// and a third parameter would put a meaningless `0` at every one of them. The two sweeps
// are genuinely independent — sweep returns early without a hypervisor, reclaimActionHistory
// returns early without a retention age — so constructing with only the first is a real
// configuration, not a hypothetical one.
//
// The retention value alone would express the same optionality, since a non-positive age
// disables the sweep. That is true and is not the reason: the reason is that most callers
// should not have to mention a setting they have no opinion about.
func (r *StateReconciler) WithActionRetention(retention time.Duration) *StateReconciler {
	r.retention = retention
	return r
}

// Interval reports the EFFECTIVE period, after clamping — what should be logged at
// startup, since it may differ from what was configured.
func (r *StateReconciler) Interval() time.Duration { return r.interval }

// Run sweeps immediately, then on each tick until ctx is cancelled.
//
// The startup sweep matters on its own: if the process was down while a guest
// crashed, nothing else will ever notice, and the record would claim the server runs
// until someone tried an action on it.
func (r *StateReconciler) Run(ctx context.Context) {
	r.sweepAll(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("compute state reconciler stopped")
			return
		case <-ticker.C:
			r.sweepAll(ctx)
		}
	}
}

// sweepAll runs both sweeps this reconciler owns. They share a ticker and a lifecycle
// and neither depends on the other, so one failing must not skip the other.
func (r *StateReconciler) sweepAll(ctx context.Context) {
	r.sweep(ctx)
	r.reclaimActionHistory(ctx)
}

// sweep corrects every server whose recorded run state disagrees with the hypervisor.
//
// It began as a narrower thing — servers recorded ACTIVE with no guest behind them —
// and a host probe found the divergences that shape could not see. Both of the others
// have a guest that EXISTS, so the lookup succeeds; one of them is not recorded ACTIVE,
// so the row was never even selected. A sweep that only recognises one direction of
// drift leaves the other served indefinitely, which is the whole of FR-024.
//
// Only ACTIVE and SHUTOFF are judged. Every other status — BUILD, ERROR, SHELVED —
// says something about a server that the hypervisor has no opinion on, and rewriting it
// from a domain state would destroy information rather than correct it.
//
// Errors are logged and the loop continues: a transient database or hypervisor blip
// must not kill the goroutine that keeps the record honest.
func (r *StateReconciler) sweep(ctx context.Context) {
	if r.svc == nil || r.svc.vmManager == nil {
		return
	}

	candidates, err := r.readCandidates(ctx)
	if err != nil {
		log.Error().Err(err).Msg("state reconciler: listing servers failed")
		return
	}

	corrected := 0
	for _, cand := range candidates {
		if ctx.Err() != nil {
			return
		}

		wantStatus, wantPower, reason, ok := r.verdictFor(ctx, cand.status, cand.domainID)
		if !ok || wantStatus == cand.status {
			continue
		}

		applied, err := r.applyVerdict(ctx, cand, wantStatus, wantPower)
		if err != nil {
			log.Error().Err(err).Str("instance_id", cand.instanceID).
				Msg("state reconciler: correcting a diverged record failed")
			continue
		}
		if !applied {
			continue
		}
		corrected++
		log.Warn().
			Str("instance_id", cand.instanceID).
			Str("domain_id", cand.domainID).
			Str("recorded", cand.status).
			Str("corrected_to", wantStatus).
			Msg("state reconciler: " + reason)
	}

	if corrected > 0 {
		log.Info().Int("corrected", corrected).Int("checked", len(candidates)).
			Msg("state reconciler: diverged records corrected")
	}
}

// reconcileCandidate is a server as the sweep read it, before asking the hypervisor about it.
type reconcileCandidate struct {
	instanceID, status, domainID string
	updatedAt                    *time.Time
}

// readCandidates snapshots the servers the sweep judges.
//
// Collected first, then acted on: holding the rows open across hypervisor calls would keep a
// pooled connection busy for the length of the whole sweep.
func (r *StateReconciler) readCandidates(ctx context.Context) ([]reconcileCandidate, error) {
	rows, err := r.svc.pool.Query(ctx, `
		SELECT id::text, status, libvirt_domain_id, updated_at
		  FROM instances
		 WHERE status IN ('ACTIVE', 'SHUTOFF')
		   AND libvirt_domain_id IS NOT NULL
		   AND libvirt_domain_id <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []reconcileCandidate
	for rows.Next() {
		var cand reconcileCandidate
		if err := rows.Scan(&cand.instanceID, &cand.status, &cand.domainID, &cand.updatedAt); err != nil {
			log.Warn().Err(err).Msg("state reconciler: skipping an unreadable row")
			continue
		}
		candidates = append(candidates, cand)
	}
	return candidates, rows.Err()
}

// applyVerdict writes a verdict formed from cand, reporting whether it was written.
//
// Only if the row is as the sweep read it: same status, and not written since. A concurrent
// action that ran between the snapshot and this write wins rather than being overwritten by a
// verdict formed before it — including one that left the status as it was, which a guard on
// status alone let through: a server read ACTIVE with its guest down, started meanwhile and
// recorded ACTIVE again, was written SHUTOFF. Every action that records a run state bumps
// updated_at, so it is the row's revision. NULL-safe, so a row that never had one is still
// correctable. Upstream gets the same guarantee from a per-instance lock shared with
// stop_instance (nova/compute/manager.py _sync_power_states); a compare-and-set needs no lock
// held across a hypervisor call, and holds across processes.
func (r *StateReconciler) applyVerdict(ctx context.Context, cand reconcileCandidate, status string, power int) (bool, error) {
	tag, err := r.svc.pool.Exec(ctx,
		`UPDATE instances SET status = $1, power_state = $2, updated_at = $3
		  WHERE id = $4 AND status = $5 AND updated_at IS NOT DISTINCT FROM $6`,
		status, power, time.Now(), cand.instanceID, cand.status, cand.updatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// verdictFor decides what a candidate's row should say, reporting ok=false when the
// hypervisor gave no answer worth acting on.
//
// Split out because the three-way decision — up, down, or no usable answer — is the
// part that has to be got right, and inlining it in the loop is what let the original
// version conflate "the lookup succeeded" with "the record agrees".
func (r *StateReconciler) verdictFor(ctx context.Context, recorded, domainID string) (status string, power int, reason string, ok bool) {
	runState, err := r.svc.vmManager.GuestRunState(ctx, domainID)
	switch {
	case errors.Is(err, hypervisor.ErrDomainNotFound):
		// No guest at all. A record already saying SHUTOFF is consistent enough with
		// that — the server is off either way — so only an ACTIVE one is corrected.
		// Rewriting the stopped one would churn updated_at on every tick forever.
		if recorded != "ACTIVE" {
			return "", 0, "", false
		}
		return "SHUTOFF", 4, "server reported running but has no guest; record corrected", true

	case err != nil:
		// An unreachable hypervisor is not evidence about the guest. Correcting the
		// record on that basis would replace a stale answer with a wrong one.
		log.Warn().Err(err).Str("domain_id", domainID).
			Msg("state reconciler: could not read guest state; leaving the record alone")
		return "", 0, "", false

	case runState == hypervisor.RunStateUp:
		return "ACTIVE", 1, "guest is running; record corrected", true

	case runState == hypervisor.RunStateDown:
		return "SHUTOFF", 4, "guest is off; record corrected", true

	default:
		// RunStateUnknown: the domain answered with a state that carries no
		// information. Same treatment as an unreachable hypervisor.
		return "", 0, "", false
	}
}

// reclaimActionHistory removes action records older than the retention age
// (SPEC-025 FR-042a, FR-042c, FR-044).
//
// Deleting a server does NOT delete its history — upstream keeps it and so does Lightstack —
// so without this the table only grows, and the sibling scoping work multiplied the rate
// by recording every server action rather than creation alone.
//
// Two properties matter as much as the deletion itself:
//
//   - Records of a server that STILL EXISTS are never reclaimed, whatever their age. An
//     operator asking what was done to a live server must get the whole history, not
//     the recent part of it.
//   - The absence of a foreign key on instance_id is what makes the first property
//     require a NOT EXISTS rather than fall out of a cascade. That absence is
//     deliberate; see migration 024.
func (r *StateReconciler) reclaimActionHistory(ctx context.Context) {
	if r.retention <= 0 {
		return
	}

	cutoff := time.Now().Add(-r.retention)
	tag, err := r.svc.pool.Exec(ctx, `
		DELETE FROM instance_actions a
		 WHERE a.start_time < $1
		   AND NOT EXISTS (SELECT 1 FROM instances i WHERE i.id = a.instance_id)`,
		cutoff)
	if err != nil {
		log.Error().Err(err).Msg("state reconciler: reclaiming action history failed")
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		log.Info().Int64("reclaimed", n).Time("older_than", cutoff).
			Msg("state reconciler: action records of deleted servers reclaimed")
	}
}
