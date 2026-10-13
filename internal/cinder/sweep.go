package cinder

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// SweepInterruptedReservations finishes off the creates a stopped process on this node was
// running (FR-033, research R13). It runs at startup, before the listeners open, and then
// periodically (RunReservationSweeper).
//
// A volume or snapshot still in creating whose owner has stopped — its heartbeat in
// reservation_owners has gone stale — has no create left to complete it: it goes to error,
// as upstream's volume service does with the resources it was working on
// (cinder/volume/manager.py, _do_cleanup). Like upstream, the sweep leaves the backing where
// it is, and deleting the errored volume or snapshot reclaims it. A stale heartbeat cannot
// tell a stopped owner from a live one that only missed beats — a database outage, a paused
// process — and may still be building that backing; such a create fails when it finds its row
// in error, and keeps the backing for that row (abandonVolume, abandonSnapshot).
//
// A create whose owner is still heartbeating is left alone, so a second process sharing
// this node name — a rolling update's surged pod, a second stub replica, a manually started
// second daemon — never sweeps a live sibling's in-flight create. Rows reserved by another
// node, or written before reservations carried an owner (reserved_owner NULL), are left
// alone too: another node's are its own to sweep, and a NULL owner's liveness cannot be
// told, so the row is kept rather than risk failing a live create.
//
// This process's own creates are never swept, whatever its heartbeat says: the process
// running the sweep is alive, so a missed beat — a database outage, a paused process, a
// clock step — must not fail its own in-flight creates. Each create finalises or abandons
// its own row, and one left behind is swept by a successor once this process has stopped —
// as upstream's volume service cleans up only its own previous run, at start
// (cinder/manager.py, CleanableManager.init_host).
func (svc *Service) SweepInterruptedReservations(ctx context.Context) {
	now := time.Now()
	staleCutoff := now.Add(-reservationOwnerStaleAfter)
	snapshots := svc.sweepKind(ctx, "snapshot",
		`UPDATE snapshots SET status = 'error'
		 WHERE status = 'creating' AND reserved_by = $1 AND reserved_owner IS NOT NULL AND reserved_owner <> $3
		   AND reserved_owner NOT IN (SELECT owner_id FROM reservation_owners WHERE last_heartbeat > $2)`,
		staleCutoff, svc.ownerID)
	volumes := svc.sweepKind(ctx, "volume",
		`UPDATE volumes SET status = 'error', updated_at = $2
		 WHERE status = 'creating' AND reserved_by = $1 AND reserved_owner IS NOT NULL AND reserved_owner <> $4
		   AND reserved_owner NOT IN (SELECT owner_id FROM reservation_owners WHERE last_heartbeat > $3)`,
		now, staleCutoff, svc.ownerID)
	if volumes+snapshots > 0 {
		log.Info().Int64("volumes", volumes).Int64("snapshots", snapshots).Str("node", svc.nodeName).
			Msg("reservation sweep: interrupted block-storage creates set to error")
	}
	// Forget the owners the sweep just found stopped, so the table does not grow across
	// crashes. A live owner (fresh heartbeat) is kept, and so is this process's own
	// registration, whatever its heartbeat says.
	if _, err := svc.pool.Exec(ctx,
		`DELETE FROM reservation_owners WHERE node_name = $1 AND last_heartbeat <= $2 AND owner_id <> $3`,
		svc.nodeName, staleCutoff, svc.ownerID); err != nil {
		log.Warn().Err(err).Str("node", svc.nodeName).Msg("reservation sweep: pruning stopped reservation owners failed")
	}
}

// sweepKind runs one sweep statement, bound to this node's name and args, and returns how many
// rows it set to error. A failure is logged and does not stop the sweep; kind names the
// resource in the log line.
func (svc *Service) sweepKind(ctx context.Context, kind, query string, args ...any) int64 {
	tag, err := svc.pool.Exec(ctx, query, append([]any{svc.nodeName}, args...)...)
	if err != nil {
		log.Error().Err(err).Str("node", svc.nodeName).Msgf("reservation sweep: sweeping interrupted %s creates failed", kind)
		return 0
	}
	return tag.RowsAffected()
}
