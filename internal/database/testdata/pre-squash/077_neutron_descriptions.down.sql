-- Rollback the Neutron resource descriptions (SPEC-031).
ALTER TABLE networks DROP COLUMN description;
ALTER TABLE subnet_pools DROP COLUMN description;
