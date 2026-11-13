-- Defense in depth for the ownership labels the cross-project listings and
-- shows render: volumes.project_id and ports.project_id are scanned into
-- plain strings, so a NULL would 500 the show and silently drop the row from
-- the listing. No writer can produce NULL today (every INSERT binds the
-- auth-context project, and an empty string fails the uuid cast), which is
-- exactly why the constraint belongs in the schema: it makes the invariant
-- structural instead of conventional (round 9).

ALTER TABLE volumes ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE ports ALTER COLUMN project_id SET NOT NULL;
