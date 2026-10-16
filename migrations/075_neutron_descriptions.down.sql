-- Rollback the Neutron resource descriptions (SPEC-028).
ALTER TABLE networks DROP COLUMN IF EXISTS description;
ALTER TABLE subnet_pools DROP COLUMN IF EXISTS description;
