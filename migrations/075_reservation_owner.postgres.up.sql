-- The process on a node that owns an in-flight block-storage reservation — the overlap fix
-- for the SPEC-028 FR-033 sweep. reserved_by names the node; reserved_owner names the
-- process on it. The startup sweep reclaims a creating row only when its owner has stopped
-- (its heartbeat in reservation_owners has gone stale), so a rolling update's surged pod, a
-- second stub replica, or a manually started second daemon never sweeps a live sibling's
-- create. NULL for rows written before this migration; the sweep leaves those alone, since
-- a pre-migration owner's liveness cannot be told.
ALTER TABLE volumes ADD COLUMN reserved_owner TEXT;
ALTER TABLE snapshots ADD COLUMN reserved_owner TEXT;

-- The live reservation owners, by node. Each running process upserts its row and heartbeats
-- last_heartbeat while it runs, then deletes the row on a clean stop; the sweep treats an
-- owner whose heartbeat is older than twice the heartbeat interval as stopped.
CREATE TABLE reservation_owners (
    owner_id TEXT PRIMARY KEY,
    node_name TEXT NOT NULL,
    last_heartbeat TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_reservation_owners_node ON reservation_owners (node_name, last_heartbeat);
