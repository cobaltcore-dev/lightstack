-- Reverts 080: the ownership-label invariant for networks, routers and
-- floating_ips goes back to conventional (every writer binds the auth-context
-- project) instead of structural.

ALTER TABLE networks ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE routers ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE floating_ips ALTER COLUMN project_id DROP NOT NULL;
