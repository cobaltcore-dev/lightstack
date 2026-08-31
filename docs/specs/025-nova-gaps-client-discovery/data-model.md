# Phase 1 Data Model: Nova Gaps & Client Discovery

**Feature**: `025-nova-gaps-client-discovery` | **Date**: 2026-08-31

Most of this feature changes **behaviour, not storage**. Two small reversible migrations are needed, both
driven by a requirement that could not be met with the columns that exist.

---

## Migrations

### `067_instance_fault` — makes a failed boot readable (FR-016, US3)

```sql
-- up
ALTER TABLE instances ADD COLUMN IF NOT EXISTS fault_code    INTEGER;
ALTER TABLE instances ADD COLUMN IF NOT EXISTS fault_message TEXT;
ALTER TABLE instances ADD COLUMN IF NOT EXISTS fault_created TIMESTAMP;

-- down
ALTER TABLE instances DROP COLUMN IF EXISTS fault_created;
ALTER TABLE instances DROP COLUMN IF EXISTS fault_message;
ALTER TABLE instances DROP COLUMN IF EXISTS fault_code;
```

All three nullable, no default. A server that never failed carries three NULLs and reports no `fault`
object at all — which is upstream's behaviour and means the change is invisible to every existing
response. See research.md R4 for why `instance_actions.message` was not reused instead.

### `068_instance_actions_time_index` — makes reclamation cheap (FR-042a, US7)

```sql
-- up
CREATE INDEX IF NOT EXISTS idx_instance_actions_start_time ON instance_actions(start_time);

-- down
DROP INDEX IF EXISTS idx_instance_actions_start_time;
```

The reclamation sweep is `DELETE FROM instance_actions WHERE start_time < $1`, run on a timer. The table
has indexes on `instance_id` and `request_id` (`migrations/024_nova_instance_actions.up.sql:15-16`) and
none on time, so without this the sweep is a full scan on a table whose entire problem is that it grows.

### Explicitly **not** added

- **No foreign key on `instance_actions.instance_id`.** Its absence is now *intentional* — the retention
  decision requires records to outlive the server, so a `REFERENCES ... ON DELETE CASCADE` would defeat
  the feature. This should be **commented in the migration or the model** so the next reader does not
  "fix" it. (FR-042)
- **No soft-delete state for instances.** US7 removes a filter on a status nothing writes; A12 records
  that introducing the status is a much larger change this feature was not given.
- **No new table for anything.** The reconciler is stateless; the retention sweep reads existing
  timestamps.

---

## Entities

### Instance (`instances`) — changed

| Field | Type | Change | Requirement |
|---|---|---|---|
| `status` | `VARCHAR(50)` | now written by the **real-mode** lifecycle path, which wrote nothing | FR-022, FR-023, FR-024a |
| `power_state` | `INTEGER` | same | FR-022, FR-023 |
| `libvirt_domain_id` | `VARCHAR(255)` | unchanged; may now be **re-written** by a self-heal, since re-defining yields a domain identifier | FR-025 |
| `fault_code` | `INTEGER` NULL | **new** | FR-016 |
| `fault_message` | `TEXT` NULL | **new** | FR-016 |
| `fault_created` | `TIMESTAMP` NULL | **new** | FR-016 |

**State transitions this feature adds or corrects**:

```
BUILD ──(pre-flight refusal)──> request refused, no row created          FR-017, FR-016a
BUILD ──(allocation fails post-accept)──> ERROR + fault_*                FR-015, FR-016
ACTIVE ──(os-stop succeeds)──> SHUTOFF, power_state=4   [was: no write]  FR-022
SHUTOFF ──(os-start succeeds)──> ACTIVE, power_state=1  [was: no write]  FR-023
SHUTOFF ──(os-start, guest absent)──> re-define + start ──> ACTIVE       FR-025
SHUTOFF ──(os-start, re-define fails)──> action-impossible + honest state FR-025c, FR-026b
ACTIVE ──(guest vanished, reconciler observes)──> corrected state         FR-024b
```

The invariant FR-018 states: **reported state and reported addresses must never contradict each other.**
`ACTIVE` with an empty address map for a server that requested a network is not a reachable state.

### Instance action record (`instance_actions`) — lifetime changed, shape unchanged

| Field | Role in this feature |
|---|---|
| `instance_id` | `UUID NOT NULL`, deliberately **without** `REFERENCES` — records outlive the server |
| `project_id` | `UUID NOT NULL` — becomes the **authorization basis** once the server row is gone (FR-043a). SPEC-023 fixed this to hold the *owning* project, which is what makes it usable |
| `start_time` | the reclamation predicate (FR-042a); newly indexed |
| `message` | carries the self-heal note for the triggering `os-start` (FR-025b) |

**Lifecycle**: created on every server action (SPEC-023 widened this from creation alone) → survives the
server's deletion → reclaimed when `start_time` is older than the retention age. Never reclaimed while
the server still exists, whatever the record's age (FR-042c).

### Floating IP (`floating_ips`) — read only, no change

Joined into the address map on `port_id`. Fields used: `port_id`, `floating_ip_address`,
`fixed_ip_address`. An index on `port_id` already exists
(`migrations/003_add_routers.up.sql:58`). Nothing is written.

### Volume (`volumes`) — read/write via the block-storage service, no schema change

US9 creates a row through the existing create-from-image path, which already sets `source_image_id`,
`bootable` and `volume_type` (`internal/cinder/volumes.go:472`). The root attachment is recorded in
`volume_attachments` at `/dev/vda` by the existing boot-from-volume path. `delete_on_termination` is
already honoured on server deletion (`internal/nova/handlers.go:1392-1400`).

### Identity resources (`projects`, `users`, `roles`, `groups`) — read only, no change

Responses gain a `links.self`; no stored field changes. Note that `projects.description` and
`domain_id` are nullable and are already rendered via `.String` to avoid changing the response shape —
adding a link must not disturb that (FR-013).

---

## Configuration

Two new `NovaConfig` fields, following the existing `Console*` style exactly (`time.Duration` with a
`Default*` constant applied by `LoadConfig` when the file omits it):

| Field | YAML key | Default | Constant | Requirement |
|---|---|---|---|---|
| `StateReconcileInterval` | `state_reconcile_interval` | `30s` | `DefaultStateReconcileInterval` | FR-024b, FR-024d |
| `InstanceActionRetention` | `instance_action_retention` | `2160h` (90 days) | `DefaultInstanceActionRetention` | FR-042a, FR-042b |

Both clamp a non-positive configured value to the default, as `NewNetworkReconciler` already does — a
deployment that sets `0` must not get a busy loop. Both surface as chart values and are documented in
`docs/CONFIGURATION.md`.

**They must arrive through the rendered `lightstack.yaml`**, not the environment: the binary honours only
`LIGHTSTACK_DB_URL`, `LIGHTSTACK_JWT_SECRET`, `LIGHTSTACK_ENDPOINT_HOST` and `LIGHTSTACK_ENDPOINT_SCHEME` as environment overrides.

---

## Derived values (no storage)

| Value | Derived from | Requirement |
|---|---|---|
| Self-link base | request host, else configured host; plus scheme and reachable port | FR-002..FR-007b |
| Boot-volume size when omitted | `flavors.disk_gb`, floored at `images.min_disk_gb` | FR-051, FR-051a |
| Guest definition for a self-heal | flavor row + instance ports + instance directory | FR-025a |
| Requested microversion | `OpenStack-API-Version`, else `X-OpenStack-Nova-API-Version`, else advertised current | FR-043 gate (R7) |
| Caller project for the access log | context project captured **before** the handler runs | FR-045 |
