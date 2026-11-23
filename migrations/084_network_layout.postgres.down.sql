-- The gateway ports go first: they are the rows without a project the up file allows.
DELETE FROM ports WHERE project_id IS NULL AND device_owner = 'network:router_gateway';
ALTER TABLE ports ALTER COLUMN project_id SET NOT NULL;
DELETE FROM security_group_rules
 WHERE id IN ('00000000-0000-0000-0000-000000000031', '00000000-0000-0000-0000-000000000032');
ALTER TABLE security_groups DROP COLUMN stateful;
ALTER TABLE security_groups DROP COLUMN revision_number;
DROP INDEX floating_ips_one_per_fixed_ip;
DROP INDEX networks_ct_zone;
ALTER TABLE networks DROP COLUMN ct_zone;
ALTER TABLE ports DROP COLUMN binding_host_id;
ALTER TABLE subnets DROP COLUMN pools_stored;
DROP TABLE subnet_allocation_pools;
