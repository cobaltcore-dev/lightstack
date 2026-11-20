-- See 084_network_layout.postgres.up.sql. SQLite spells ids TEXT and booleans INTEGER, in a
-- STRICT table, as the generated baseline does.
CREATE TABLE "subnet_allocation_pools" (
    "id" TEXT NOT NULL DEFAULT (gen_random_uuid()),
    "subnet_id" TEXT NOT NULL,
    "first_ip" TEXT NOT NULL CHECK (length("first_ip") <= 64),
    "last_ip" TEXT NOT NULL CHECK (length("last_ip") <= 64),
    PRIMARY KEY ("id"),
    FOREIGN KEY (subnet_id) REFERENCES subnets(id) ON DELETE CASCADE
) STRICT;
CREATE INDEX "idx_subnet_allocation_pools_subnet" ON "subnet_allocation_pools" (subnet_id);
ALTER TABLE subnets ADD COLUMN pools_stored INTEGER;

ALTER TABLE ports ADD COLUMN binding_host_id TEXT;

ALTER TABLE networks ADD COLUMN ct_zone INTEGER;
CREATE UNIQUE INDEX networks_ct_zone ON networks (ct_zone);

ALTER TABLE security_groups ADD COLUMN revision_number INTEGER NOT NULL DEFAULT 0;
ALTER TABLE security_groups ADD COLUMN stateful INTEGER NOT NULL DEFAULT 1;

INSERT INTO security_group_rules (id, security_group_id, direction, ethertype, remote_group_id)
SELECT '00000000-0000-0000-0000-000000000031', id, 'ingress', 'IPv4', id
  FROM security_groups WHERE id = '00000000-0000-0000-0000-000000000030'
UNION ALL
SELECT '00000000-0000-0000-0000-000000000032', id, 'ingress', 'IPv6', id
  FROM security_groups WHERE id = '00000000-0000-0000-0000-000000000030';

-- See 084_network_layout.postgres.up.sql: a router's gateway port has no project, so 079's guards
-- on ports.project_id go.
DROP TRIGGER ports_project_id_not_null_insert;
DROP TRIGGER ports_project_id_not_null_update;
