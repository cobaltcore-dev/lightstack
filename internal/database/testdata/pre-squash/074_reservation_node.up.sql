-- The node a volume or snapshot in `creating` was reserved by (SPEC-028 FR-033). A create
-- reserves its row before the backing store is touched; a node that stops mid-create
-- leaves the row behind, and at its next start sweeps exactly the rows it reserved —
-- never another node's. NULL for rows written before this migration, which no sweep
-- touches.
ALTER TABLE volumes ADD COLUMN reserved_by TEXT;
ALTER TABLE snapshots ADD COLUMN reserved_by TEXT;
