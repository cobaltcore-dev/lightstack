-- Extends 077's hardening to the remaining tables whose project_id is scanned
-- into a plain string and rendered as the row's tenant_id by the cross-project
-- listings and shows (networks, routers, floating_ips — round 10). Every writer
-- binds the auth-context project (an empty string fails the uuid cast), so the
-- constraint makes an existing invariant structural, exactly as 077 did for
-- volumes and ports.

ALTER TABLE networks ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE routers ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE floating_ips ALTER COLUMN project_id SET NOT NULL;
