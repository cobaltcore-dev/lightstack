-- networks.description and subnet_pools.description — upstream Neutron's
-- standard resource annotation (SPEC-028).
--
-- Neither table had the column, so `network set --description` answered
-- 400 "no updates provided" (the update handler binds name/admin_state_up/
-- router:external only) and `subnet pool set --description` did the same.
-- Existing rows carry no description; NULL reads back as the empty string,
-- exactly what the CLI displayed for them before.
ALTER TABLE networks ADD COLUMN IF NOT EXISTS description TEXT;
ALTER TABLE subnet_pools ADD COLUMN IF NOT EXISTS description TEXT;
