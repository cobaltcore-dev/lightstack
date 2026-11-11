-- Enforce server-group name uniqueness per project at the schema level.
--
-- Migration 031 intended this (UNIQUE(project_id, name)) but its CREATE TABLE
-- IF NOT EXISTS has been a no-op on every install: migration 017 had already
-- created the table without the constraint. Upstream nova answers 409 for a
-- duplicate (project, name); without the constraint the race between the
-- handler's EXISTS check and its INSERT could still produce a second row.
--
-- Existing duplicates (possible before the handler check existed) are folded
-- first — the earliest group per (project_id, name) keeps the name, its
-- members are merged into it, and the rest are deleted — so the constraint
-- can always be added.

-- Fold duplicates: merge members into the survivor (earliest created, then
-- lowest id for a deterministic tie-break), then drop the losers. Only
-- groups that actually have duplicates are touched; COALESCE keeps an
-- all-empty member set an empty array rather than NULL.
WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY project_id, name
               ORDER BY created_at, id
           ) AS rn,
           FIRST_VALUE(id) OVER (
               PARTITION BY project_id, name
               ORDER BY created_at, id
           ) AS survivor_id
    FROM server_groups
)
UPDATE server_groups survivor
SET members = COALESCE((
    SELECT array_agg(DISTINCT m ORDER BY m)
    FROM ranked r
    JOIN server_groups g ON g.id = r.id
    CROSS JOIN UNNEST(g.members) AS m
    WHERE r.survivor_id = survivor.id
), '{}')
WHERE survivor.id IN (SELECT survivor_id FROM ranked WHERE rn > 1);

DELETE FROM server_groups
WHERE id IN (
    SELECT id FROM (
        SELECT id,
               ROW_NUMBER() OVER (
                   PARTITION BY project_id, name
                   ORDER BY created_at, id
               ) AS rn
        FROM server_groups
    ) t
    WHERE t.rn > 1
);

ALTER TABLE server_groups
    ADD CONSTRAINT uq_server_groups_project_name UNIQUE (project_id, name);
