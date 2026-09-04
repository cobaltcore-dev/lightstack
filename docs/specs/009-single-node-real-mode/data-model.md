# Phase 1 Data Model: Single-Node Real Mode (v1)

This feature is overwhelmingly **behavioral** (make existing entities work in real mode). The only persistent-schema change is one new column; most of the "model" is the **on-disk artifact layout** and **state transitions** that the reconciler and disk-staging logic depend on.

## Entities (existing tables — reused)

| Entity | Table | Key fields used | Change |
|--------|-------|-----------------|--------|
| **Server (instance)** | `instances` | `id`, `project_id`, `image_id` (nullable — supports volume-backed), `flavor_id`, `status`, `power_state`, `libvirt_domain_id`, `console_vnc_port` | none (no host column needed on a single node) |
| **Image** | `images` | `id`, `disk_format`, `rbd_pool`/`rbd_image`, `visibility`, `status` | `disk_format` reported as `raw` after ingest normalization (R2); delete guarded by in-use query (R3) |
| **Network / Subnet / Port** | `networks`, `subnets`, `ports` | `ports.mac_address`, `ports.fixed_ips` (JSONB), `ports.device_id`, `subnets.cidr/gateway_ip/enable_dhcp`, `networks.network_type` | none — read by the reconciler (R4/R5) |
| **Router** | `routers` | `external_gateway_info` (JSONB: `{network_id, enable_snat, external_fixed_ips}`) | external network auto-mapped to the node uplink (R10) |
| **Floating IP** | `floating_ips` | `floating_ip_address`, `fixed_ip_address`, `port_id`, `router_id`, `status` | none — DNAT activated in real mode (R10) |
| **Security group / rule** | `security_groups`, `security_group_rules`, `port_security_groups` | `direction`, `protocol`, `port_range_*`, `remote_ip_prefix` | none — enforced on north-south (R10) |
| **Metadata / cloud-init** | `instance_userdata`, `keypairs` | `instance_userdata.instance_id`/`userdata`, `keypairs.public_key` | `instance_userdata` write-path added (R9); table already exists |
| **Volume** | `volumes` | `id`, `size_gb`, `status`, `bootable`, `attached_to_instance_id`, `rbd_pool`/`rbd_image` | **+ `source_image_id`** (R3/R11) |

## Schema change — migration `060`

`060_add_volume_source_image.{up,down}.sql` (next after `059_qos_specs_multitenancy`):

```sql
-- up
ALTER TABLE volumes ADD COLUMN IF NOT EXISTS source_image_id UUID;
CREATE INDEX IF NOT EXISTS idx_volumes_source_image_id ON volumes(source_image_id);

-- down
DROP INDEX IF EXISTS idx_volumes_source_image_id;
ALTER TABLE volumes DROP COLUMN IF EXISTS source_image_id;
```

- **Purpose**: records the image a volume was created from. Set for **CoW (RBD) clones** (which pin the image) and left NULL for **independent local copies** (which don't). Drives both boot-from-volume (R11) and the image-in-use guard (R3).
- Reversible (up/down) per Constitution. No backfill needed (existing volumes were not created from images via this path).

> No migration is needed for `user_data` — the `instance_userdata` table already exists (migration 005); only the write-path is missing.

## On-disk artifact layout (node-local, persistent)

All under the chart's persistent hostPath `/var/lib/lightstack` (R6):

```text
/var/lib/lightstack/
├── images/        image-<image-id>.raw          # raw base (Glance; normalized on ingest, R2)
├── instances/     <instance-id>/disk.qcow2       # per-instance qcow2 overlay backing the raw base (R1)
├── cloud-init/    <instance-id>.iso              # config-drive ISO (genisoimage; attached as CDROM)
└── volumes/       volume-<volume-id>.qcow2        # local volumes  (MOVED from ~/.lightstack/volumes, R6)
```

- Overlay relationship: `instances/<id>/disk.qcow2` —(qcow2 backing, `-F raw`)→ `images/image-<imgid>.raw`. Deleting the image is blocked while the overlay exists (R3); the overlay dir is removed on VM delete.
- Persistence: every artifact above survives a pod restart (hostPath), satisfying FR-006a. The host libvirt domain definition also persists (it is defined, not transient), but **VM auto-resume after a full host reboot is out of scope** (spec).

## State transitions

**Instance power lifecycle** (WS-0, FR-001a) — maps to libvirt domain state:

```text
BUILD ──create──▶ ACTIVE ──stop──▶ SHUTOFF ──start──▶ ACTIVE
                    │  ▲──────reboot──────┘
                    └──delete──▶ (overlay + domain removed)
   any step failing host allocation ──▶ ERROR (clear message, R12 capacity)
```

**Volume** (WS-5):

```text
creating ──▶ available ──attach──▶ in-use ──detach──▶ available ──delete──▶ (gone)
   └─(from image: copy/CoW, set source_image_id, bootable=true)
boot-from-volume: available|new ──▶ in-use (root disk; delete_on_termination governs cleanup)
```

**Image** (WS-0): `queued → active`; `DELETE` rejected (409 "image in use") while any `instances.image_id` or `volumes.source_image_id` references it (R3).

## Reconciler read-set (WS-2, R4/R5)

On each pass the reconciler reads (idempotent rebuild, best-effort per resource):

- `networks` (+ `network_type`) → bridges
- `subnets` (`enable_dhcp`, `cidr`, `gateway_ip`, `dns_nameservers`) → dnsmasq config
- `ports` (`mac_address`, `fixed_ips`, `network_id`, `device_id`) → taps, bridge attach, **dnsmasq MAC→IP host reservations** (stable IP, R5)
- `security_groups` + `security_group_rules` + `port_security_groups` → iptables chains
- `routers` (`external_gateway_info`) + `floating_ips` → SNAT/DNAT (R10)
- per-namespace `169.254.169.254` metadata DNAT (R7)

No new tables are required for reconciliation — the existing schema already persists everything the kernel state is derived from.
