# Phase 1 Data Model: Nova server-detail read-back

**Feature**: `017-nova-server-detail-readback` | **Date**: 2026-07-17

Scope is deliberately small: **one new column**, and **new use** of an existing join table. No new
tables.

---

## Entity: Instance (`instances`) — 1 new column

| Column | Type | Null | Notes |
|---|---|---|---|
| *(existing columns unchanged)* | | | id, name, status, power_state, project_id, user_id, flavor_id, image_id, task_state, locked, admin_password_hash, timestamps, … |
| **`key_name`** | `VARCHAR(255)` | **YES** (default NULL) | **NEW** (migration `063`). The keypair name the instance was booted with. Nullable so pre-existing rows read back as `null`. |

- **Migration `063_nova_instance_key_name`** — up: `ALTER TABLE instances ADD COLUMN IF NOT EXISTS key_name VARCHAR(255);` · down: `ALTER TABLE instances DROP COLUMN IF EXISTS key_name;`
- **Model** (`internal/database/models.go:62-76`): add `KeyName *string \`json:"key_name"\`` if the
  struct is used on these paths. The handlers build `gin.H` maps directly, so they scan `key_name`
  into a `*string` (nil → JSON `null`).
- **Write**: `CreateServer` INSERT (`handlers.go:425-428`) includes `key_name` = `req.Server.KeyName`
  (empty string persisted as `NULL`).
- **Read**: `GetServer` (`handlers.go:992-1000` SELECT, `:1016-1035` map) and `ListServersDetail`
  (`:849-858` SELECT, `:885-912` map) add `key_name`.

---

## Relationship: Instance ↔ Security Group (`server_security_groups`) — existing table, new usage

Existing (migration `049_nova_server_actions.up.sql:9-14`) — **no schema change**:

```sql
server_security_groups (
    server_id UUID NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    security_group_id UUID NOT NULL,           -- NOTE: no FK to security_groups
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (server_id, security_group_id)
)
```

- **Write (NEW at create)**: for each supplied `security_groups[].name`, resolve to
  `security_groups.id` (scoped by `project_id`) and INSERT `(server_id, security_group_id)`. Also
  written by the existing `AddSecurityGroup`/`RemoveSecurityGroup` server actions (unchanged).
- **Read (NEW on detail)**: INNER JOIN to `security_groups` to get current names; orphaned rows
  (SG deleted; no FK) are dropped by the join. Empty result → synthesize `[{"name":"default"}]`.
- **Cascade**: deleting a server removes its rows (FK ON DELETE CASCADE). Deleting a security group
  does **not** (no FK) — handled by the INNER JOIN on read.

---

## Entity: Security Group (`security_groups`) — read + idempotent ensure-default

Existing (`001_initial_schema.up.sql:114-122`): `id`, `name`, `project_id`, `description`, timestamps,
`UNIQUE(project_id, name)`. Used for name↔id resolution (`WHERE name = $1 AND project_id = $2`).
The `default` group is seeded only for the seeded default project (`002_seed_data:54-61`); because
OpenStack guarantees *every* project has one, `ensureDefaultSecurityGroup(projectID)` idempotently
creates it — INSERT `security_groups` (`gen_random_uuid()`, `name='default'`) + default rules into
`security_group_rules` (`001:124-135`), mirroring `002_seed_data:54-69`, `ON CONFLICT … DO NOTHING`
— when a create references `default` in a project that lacks it (see research.md D3a). No schema
change.

## Entity: Keypair (`keypairs`) — read-only here

Existing (`001_initial_schema.up.sql`): `UNIQUE(user_id, name)`, user-owned. Used only for FR-002a
validation (`WHERE user_id = $1 AND name = $2`).

---

## Validation rules (create)

| Field | Rule | On violation |
|---|---|---|
| `key_name` (if non-empty) | MUST resolve in `keypairs` for the requesting **user** | `400 badRequest` "Invalid key_name provided." — no instance created (FR-002a) |
| `security_groups[].name` (each) | MUST resolve for the requesting **project**; the name `default` is ensured (auto-created) if absent | unknown **non-default** name → `400 badRequest` "Security group `<name>` not found." — no instance created (FR-005a) |

Both checks run **synchronously before** the `instances` INSERT (Constitution II/III).

## Read-back rules (detail)

| Field | Rule |
|---|---|
| `key_name` | Column value; `null` when unset |
| `security_groups` | INNER JOIN names → `[{"name": …}]`; if empty → `[{"name":"default"}]` |

Both `GetServer` (single query) and `ListServersDetail` (one batched `= ANY($1)` query) return the
fields identically.
