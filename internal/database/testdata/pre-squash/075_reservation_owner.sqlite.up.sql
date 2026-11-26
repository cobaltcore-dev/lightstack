-- See 075_reservation_owner.postgres.up.sql. SQLite stores the heartbeat as TEXT — the
-- backend's timestamp storage — in a STRICT table, matching the generated baseline.
ALTER TABLE volumes ADD COLUMN reserved_owner TEXT;
ALTER TABLE snapshots ADD COLUMN reserved_owner TEXT;

CREATE TABLE reservation_owners (
    owner_id TEXT PRIMARY KEY,
    node_name TEXT NOT NULL,
    last_heartbeat TEXT NOT NULL
) STRICT;
CREATE INDEX idx_reservation_owners_node ON reservation_owners (node_name, last_heartbeat);
