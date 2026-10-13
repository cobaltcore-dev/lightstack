package cinder

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
)

// A reservation is owned by the process that made it, not merely by its node. Every process
// on a node shares the node name, so a sweep keyed on the node name alone reclaims a live
// sibling's in-flight creates — a rolling update's surged pod, a second stub replica and a
// manually started second daemon all share spec.nodeName. reserved_owner records the
// process; each live process heartbeats reservation_owners while it runs, and the sweep
// reclaims a creating row only once its owner has stopped — its heartbeat has gone stale
// (SPEC-028 FR-033).

const (
	// reservationHeartbeatInterval is how often a live owner refreshes its liveness.
	reservationHeartbeatInterval = 30 * time.Second
	// reservationOwnerStaleAfter is how old an owner's heartbeat must be before the sweep
	// treats it as stopped — twice the interval, so a single missed beat is not a stop.
	reservationOwnerStaleAfter = 2 * reservationHeartbeatInterval
	// reservationSweepInterval is how often the recovery pass re-runs after startup, so a
	// crashed owner skipped while its heartbeat was still fresh — a restart within the stale
	// window — is reclaimed once that heartbeat expires. The startup sweep alone would never
	// revisit it.
	reservationSweepInterval = reservationOwnerStaleAfter
)

// OwnerID is the per-process id this service records as the owner of the reservations it
// makes (reserved_owner); main registers and heartbeats it.
func (svc *Service) OwnerID() string { return svc.ownerID }

// RegisterReservationOwner records this process as a live reservation owner on its node. It
// must run before the startup sweep, so the sweep can tell this process's reservations from
// a stopped predecessor's. Its failure is fatal to the caller: without a live registration
// this process's own creates would look ownerless, and a sibling could sweep them.
func (svc *Service) RegisterReservationOwner(ctx context.Context) error {
	if _, err := svc.pool.Exec(ctx, `
		INSERT INTO reservation_owners (owner_id, node_name, last_heartbeat)
		VALUES ($1, $2, $3)
		ON CONFLICT (owner_id) DO UPDATE SET node_name = EXCLUDED.node_name, last_heartbeat = EXCLUDED.last_heartbeat
	`, svc.ownerID, svc.nodeName, time.Now()); err != nil {
		return fmt.Errorf("registering reservation owner %s: %w", svc.ownerID, err)
	}
	return nil
}

// StartReservationHeartbeat refreshes this owner's liveness every heartbeat interval until
// StopReservationHeartbeat or ctx ends, registering it again if it was deleted meanwhile.
// Run it in its own goroutine.
func (svc *Service) StartReservationHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(reservationHeartbeatInterval)
	defer ticker.Stop()
	defer close(svc.hbDone)
	for {
		select {
		case <-ticker.C:
			svc.heartbeat(ctx)
		case <-svc.hbStop:
			return
		case <-ctx.Done():
			return
		}
	}
}

// heartbeat refreshes this owner's liveness once. If the registration is gone — a process
// sharing this node found the heartbeat stale and deleted it — it registers again, as
// upstream's service recreates its own record when it finds it gone (cinder/service.py,
// report_state). Otherwise every later create of this process would look ownerless to that
// process's sweep until a restart.
func (svc *Service) heartbeat(ctx context.Context) {
	tag, err := svc.pool.Exec(ctx,
		`UPDATE reservation_owners SET last_heartbeat = $1 WHERE owner_id = $2`,
		time.Now(), svc.ownerID)
	if err != nil {
		log.Error().Err(err).Str("owner", svc.ownerID).Msg("reservation-owner heartbeat failed")
		return
	}
	if tag.RowsAffected() > 0 {
		return
	}
	log.Warn().Str("owner", svc.ownerID).Str("node", svc.nodeName).
		Msg("reservation-owner registration was gone: another process on this node took this one for stopped and may have set its in-flight creates to error; registering again")
	if err := svc.RegisterReservationOwner(ctx); err != nil {
		log.Error().Err(err).Str("owner", svc.ownerID).Msg("re-registering the reservation owner failed")
	}
}

// StopReservationHeartbeat stops the heartbeat and deregisters this owner, so a clean stop
// leaves no stale owner row for a successor to reason about.
func (svc *Service) StopReservationHeartbeat() {
	close(svc.hbStop)
	<-svc.hbDone
	if _, err := svc.pool.Exec(context.Background(),
		`DELETE FROM reservation_owners WHERE owner_id = $1`, svc.ownerID); err != nil {
		log.Warn().Err(err).Str("owner", svc.ownerID).Msg("failed to deregister reservation owner on shutdown")
	}
}

// RunReservationSweeper re-runs the interrupted-reservation sweep on a timer until ctx ends.
// The startup sweep runs once, before the listeners open, so a crashed owner skipped because
// its heartbeat had not yet expired — a restart within the stale window — would never be
// revisited; this pass reclaims it once it goes stale. It keeps the live-owner protection,
// so it only ever reclaims a stopped owner's creates and is safe to run while serving. Run
// it in its own goroutine.
func (svc *Service) RunReservationSweeper(ctx context.Context) {
	ticker := time.NewTicker(reservationSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			svc.SweepInterruptedReservations(ctx)
		case <-ctx.Done():
			return
		}
	}
}
