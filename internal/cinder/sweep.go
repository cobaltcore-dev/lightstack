package cinder

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
)

// SweepInterruptedReservations finishes off the creates this node was running when it
// stopped (FR-033, research R13). It runs at startup, before the listeners open.
//
// A volume or snapshot still in creating and reserved under this node's name has no
// create left to complete it: it goes to error, as upstream's volume service does with
// the resources it was working on (cinder/volume/manager.py, _do_cleanup), and its
// backing — whatever the create got to — is reclaimed. A reclaim that fails is logged and
// the sweep goes on; deleting the errored resource later reclaims it.
//
// Rows reserved under another name belong to another node of a multi-node PostgreSQL
// deployment, which may be creating them right now, and rows written before reservations
// carried a name have no owner to ask: neither is touched.
func (svc *Service) SweepInterruptedReservations(ctx context.Context) {
	now := time.Now()
	// Snapshots are reclaimed before their volumes: on RBD an image that still has a snapshot
	// cannot be removed, so the volume reclaim would fail if its snapshot's backing were there.
	snapshots := svc.sweepKind(ctx, "snapshot",
		`UPDATE snapshots SET status = 'error' WHERE status = 'creating' AND reserved_by = $1
		 RETURNING id::text, volume_id::text`,
		func(s sweptRow) error { return svc.cephClient.DeleteSnapshot(ctx, s.volumeID, s.id) })
	volumes := svc.sweepKind(ctx, "volume",
		`UPDATE volumes SET status = 'error', updated_at = $2 WHERE status = 'creating' AND reserved_by = $1
		 RETURNING id::text, id::text`,
		func(v sweptRow) error { return svc.cephClient.DeleteVolume(ctx, v.id) }, now)
	if volumes+snapshots > 0 {
		log.Info().Int("volumes", volumes).Int("snapshots", snapshots).Str("node", svc.nodeName).
			Msg("startup: interrupted block-storage creates set to error")
	}
}

// sweepKind runs one sweep statement, bound to this node's name and args, sets the matched rows to
// error, reclaims each one's backing, and returns how many it swept. A query failure or a reclaim
// failure is logged and does not stop the sweep; deleting the errored resource later reclaims it.
// kind names the resource in the log lines.
func (svc *Service) sweepKind(ctx context.Context, kind, query string, reclaim func(sweptRow) error, args ...any) int {
	rows, err := svc.sweep(ctx, query, args...)
	if err != nil {
		log.Error().Err(err).Str("node", svc.nodeName).Msgf("startup: sweeping interrupted %s creates failed", kind)
	}
	for _, r := range rows {
		if err := reclaim(r); err != nil {
			log.Warn().Err(err).Str(kind+"_id", r.id).Msgf("startup: could not reclaim the backing of an interrupted %s create", kind)
		}
	}
	return len(rows)
}

// sweptRow is one row the sweep set to error and the volume it belongs to — itself, for
// a volume.
type sweptRow struct{ id, volumeID string }

// sweep runs one of the sweep's UPDATE … RETURNING statements, bound to this node's name
// and args, and collects what it set to error.
func (svc *Service) sweep(ctx context.Context, query string, args ...any) ([]sweptRow, error) {
	rows, err := svc.pool.Query(ctx, query, append([]any{svc.nodeName}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sweptRow
	for rows.Next() {
		var r sweptRow
		if err := rows.Scan(&r.id, &r.volumeID); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
