-- Merge existing duplicate groups before enforcing the same per-project
-- uniqueness as the PostgreSQL variant. SQLite stores members as JSON text.
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
), merged AS (
    SELECT r.survivor_id, json_group_array(DISTINCT member.value) AS members
    FROM ranked r
    JOIN server_groups g ON g.id = r.id
    JOIN json_each(CASE WHEN json_valid(g.members) THEN g.members ELSE '[]' END) AS member
    GROUP BY r.survivor_id
)
UPDATE server_groups
SET members = COALESCE((
    SELECT members FROM merged WHERE merged.survivor_id = server_groups.id
), '[]')
WHERE id IN (
    SELECT survivor_id FROM ranked GROUP BY survivor_id HAVING COUNT(*) > 1
);

WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY project_id, name
               ORDER BY created_at, id
           ) AS rn
    FROM server_groups
)
DELETE FROM server_groups WHERE id IN (SELECT id FROM ranked WHERE rn > 1);

CREATE UNIQUE INDEX uq_server_groups_project_name
    ON server_groups(project_id, name);
