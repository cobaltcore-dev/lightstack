-- SPEC-033 round 2: the records the real-mode network layout renders from, and the API fields
-- that go with them.

-- A subnet's allocation pools (FR-030). pools_stored true: these rows are the pools, none
-- meaning none, as upstream's "allocation_pools": []. False or NULL: upstream's default pool,
-- computed from the CIDR and the gateway, which is how every subnet from before reads (FR-032a).
CREATE TABLE subnet_allocation_pools (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subnet_id UUID NOT NULL REFERENCES subnets(id) ON DELETE CASCADE,
    first_ip VARCHAR(64) NOT NULL,
    last_ip VARCHAR(64) NOT NULL
);
CREATE INDEX idx_subnet_allocation_pools_subnet ON subnet_allocation_pools (subnet_id);
ALTER TABLE subnets ADD COLUMN pools_stored BOOLEAN;

-- The node whose TAP a port is, reported as binding:host_id (FR-056); its chains live there.
ALTER TABLE ports ADD COLUMN binding_host_id TEXT;

-- The network's connection-tracking zone, assigned at creation, the lowest free from 4097.
-- NULL for networks from before; one is assigned the first time a port of theirs is rendered.
ALTER TABLE networks ADD COLUMN ct_zone INTEGER;
CREATE UNIQUE INDEX networks_ct_zone ON networks (ct_zone);

-- One floating IP of a network per fixed IP of a port (FR-029), as upstream's
-- uniq_floatingips0floatingnetworkid0fixedportid0fixedipaddress. An unassociated floating IP holds
-- NULLs there, which a unique index never takes for equal.
CREATE UNIQUE INDEX floating_ips_one_per_fixed_ip ON floating_ips (floating_network_id, port_id, fixed_ip_address);

-- Upstream's revision number (FR-050) and stateless groups (FR-051).
ALTER TABLE security_groups ADD COLUMN revision_number INTEGER NOT NULL DEFAULT 0;
ALTER TABLE security_groups ADD COLUMN stateful BOOLEAN NOT NULL DEFAULT true;

-- The seeded project's default admits its own members, as upstream's default does (FR-043).
-- Still no ingress rule open to every address (cloud-storage#528): the remote is the group.
INSERT INTO security_group_rules (id, security_group_id, direction, ethertype, remote_group_id)
SELECT '00000000-0000-0000-0000-000000000031'::uuid, id, 'ingress', 'IPv4', id
  FROM security_groups WHERE id = '00000000-0000-0000-0000-000000000030'
UNION ALL
SELECT '00000000-0000-0000-0000-000000000032'::uuid, id, 'ingress', 'IPv6', id
  FROM security_groups WHERE id = '00000000-0000-0000-0000-000000000030';

-- A router's gateway port has no project, as upstream's (_create_gw_port: "tenant_id": "",
-- intentionally not set; FR-038), so ports.project_id takes NULL again where 079 refused it.
-- Every reader scans it nullable.
ALTER TABLE ports ALTER COLUMN project_id DROP NOT NULL;
