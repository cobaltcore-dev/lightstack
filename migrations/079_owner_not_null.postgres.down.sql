-- Reverts 079: the ownership-label invariant goes back to conventional
-- (every writer binds the auth-context project) instead of structural.

ALTER TABLE volumes ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE ports ALTER COLUMN project_id DROP NOT NULL;
