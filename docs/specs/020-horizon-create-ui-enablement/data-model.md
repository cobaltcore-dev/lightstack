# Phase 1 Data Model — Horizon create/write UI enablement (F2, F3, F7, F8)

**Feature**: SPEC-020 | **Date**: 2026-07-27 | **Plan**: [plan.md](./plan.md)

> **No schema change and no migration.** Every entity below already exists in the database or is a pure
> response projection. This document records the *shape* each entity must present to Horizon and where its
> values come from.

---

## 1. Quota limit (persisted)

The per-project hard limit for one resource. Two independent tables, one per service; **Neutron has none**.

| Table | Migration | Columns | Notes |
|---|---|---|---|
| `quotas` (Nova/shared) | `007` | `id UUID PK`, `project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE`, `resource VARCHAR(50)`, `hard_limit INTEGER`, `created_at`, `updated_at`, `UNIQUE(project_id, resource)` | Seeds 13 rows for the default project `…0002` at the upstream values |
| `cinder_quotas` | `039` | `project_id UUID NOT NULL` (no FK), `resource VARCHAR(255)`, `"limit" INTEGER`, `created_at`, `updated_at`, `PK(project_id, resource)` | No seed rows |
| *(Neutron)* | — | — | **No table.** Limits are code defaults; usage is counted live. Adding a table is out of scope. |

**Validation / rules**
- `project_id` must be a **UUID** (`quotas` enforces an FK to `projects`), so a project *name* in a URL must be
  resolved before use. Cinder's existing name→UUID lookup is retained for the admin path.
- A resource with **no row** falls back to the code default — the row set is sparse by design.
- **Resolution rule (new, FR-003):** effective limit = `code default` ⊕ `table row` (row wins). This is
  computed by **one helper per service package**, replacing the four duplicated Nova maps and two Cinder maps.

**Default limit values (single source per service)**

| Service | Resources | Values |
|---|---|---|
| Nova | `instances`, `cores`, `ram` | **10 / 20 / 51200** (upstream; the shadowing 100/200/512000 map is deleted — FR-003) |
| Nova | `volumes`, `gigabytes`, `snapshots`, `networks`, `subnets`, `ports`, `routers`, `floatingip`, `security_groups`, `security_group_rules` | unchanged (10 / 1000 / 10 / 10 / 10 / 50 / 10 / 10 / 10 / 100) |
| Nova | `keypairs`, `server_groups`, `server_group_members` | 100 / 10 / 10 — currently only in the limits map; folded into the shared set so `maxTotalKeypairs` etc. survive |
| Cinder | `volumes`, `snapshots`, `gigabytes`, `backups`, `backup_gigabytes`, `per_volume_gigabytes` | unchanged (100 / 100 / 10000 / 100 / 10000 / 1000) |
| Neutron | `network`, `subnet`, `port`, `router`, `floatingip`, `security_group`, `security_group_rule` | unchanged (100 / 100 / 500 / 10 / 50 / 100 / 500) |

---

## 2. Quota usage (derived, never stored)

Live counts/sums for the target project, computed per request from the owning tables.

| Resource | Source | Notes |
|---|---|---|
| `instances`, `cores`, `ram` | `instances` ⋈ `flavors` | **Must apply `status != 'DELETED'`** — `GetLimits` does, `GetQuotaSet` does not; they are aligned |
| `volumes`, `gigabytes` | `volumes` | Cinder additionally filters `status != 'deleted'` |
| `snapshots` | `snapshots` | Nova's quota-set lists `snapshots` as a limit but has **no** usage query — added |
| `networks`, `subnets`, `ports`, `routers`, `floating_ips`, `security_groups` | respective tables, `WHERE project_id = $1` | |
| `security_group_rules` | `security_group_rules` via the project's groups | Currently hardcoded `0` in Neutron's detail response |
| `floatingip`, `security_groups`, `server_groups` (Nova limits) | `floating_ips`, `security_groups`, `server_groups` | **Currently hardcoded `0`** (`handlers.go:2010-2012`) — populated per FR-004 |
| `backups`, `backup_gigabytes` (Cinder) | `backups` | Currently hardcoded `0`; populated for consistency |

**Rule**: a usage query that errors degrades to `0` and is logged (the established idiom) — the limits
response must still ship, because a failure here is exactly what blanks Horizon's widget.

---

## 3. Quota response projections

The same underlying data, rendered in the four shapes OpenStack clients request. Full field lists live in
[`contracts/`](./contracts/).

| Projection | Shape | Used by |
|---|---|---|
| **Absolute limits** (`/limits`) | `{limits:{rate:[], absolute:{maxTotal*…, total*Used…}}}` — flat `max`/`total` key pairs | Horizon Overview; Launch/Create action gating |
| **Quota set** (`/os-quota-sets/{id}`, `/v3/quota-sets/{id}`) | `{quota_set:{id, <resource>: <limit>, …}}` | CLI, admin views |
| **Quota set — detail** (`…/detail`) | `{quota_set:{id, <resource>:{in_use, limit, reserved}, …}}` | quota-aware clients |
| **Quota set — defaults** (`…/defaults`) | same as quota set, code defaults only (no table rows, no usage) | CLI, Horizon |
| **Network quota** (`/v2.0/quotas/{id}`) | `{quota:{<resource>: <limit>, …, project_id, tenant_id}}` | Horizon Create-Network gating |
| **Network quota — details** (`…/details[.json]`) | `{quota:{<resource>:{limit, used, reserved}, …}}` | Horizon network usage (single deterministic call) |
| **Network quota — default** (`…/default`) | `{quota:{<resource>: <limit>, …}}` | Horizon / neutronclient |

**Invariant (FR-003, testable):** for any resource present in more than one projection, the **limit value is
identical**. This is the direct G1/G12 regression guard.

**Authorization invariant (FR-002a):** the *target project* of any of these is the caller's own
`project_id` unless the caller holds the `admin` role, in which case the URL id is honored. Resolved via the
existing shared helper `common.HasRole(c, "admin")`.

---

## 4. Capability advertisement (response only)

What each service claims to support; Horizon gates controls on these.

| Service | Today | Change |
|---|---|---|
| **Neutron** `/v2.0/extensions` | 6 aliases: `security-group`, `router`, `port-security`, `binding`, `provider`, `quotas` | **+ `quota_details`** — routes the network-usage read onto one endpoint O3K controls instead of Horizon's multi-call fallback. Entries also gain the upstream `links: []` field. Only genuinely-supported aliases are advertised. |
| **Cinder** `/v3/extensions` | **absent (404)** — prime suspect for the dead Create Volume modal | **New endpoint**, advertising only what O3K implements (at minimum `AvailabilityZones`, matching the existing `/v3/os-availability-zone`) |
| **Nova** `/extensions` | absent | **Unchanged — deliberately.** Upstream removed the extensions API at microversion 2.36; adding it would diverge from Flamingo (Art. I) |

**Entry shape**: `{alias, name, description, updated, links: []}`.

---

## 5. Selectable network (response projection)

A Neutron network as the launch wizard consumes it.

| Field | Today | Change |
|---|---|---|
| `id`, `name`, `status`, `admin_state_up`, `shared`, `mtu`, `created_at`, `updated_at`, `router:external` | present | unchanged |
| `tenant_id` | present | unchanged (kept for compatibility) |
| **`project_id`** | **absent** | **added** alongside `tenant_id` (FR-007) |
| `subnets` | list of subnet **ids** | unchanged in shape — but see the integrity rule below |

Built inline in three places that must stay in agreement: `CreateNetwork`, `ListNetworks`, `GetNetwork`
(`internal/neutron/network.go:462`, `:637`, `:709`).

**Subnet-reference integrity rule (new, FR-007 — the F7 guard).** Every id in a network's `subnets` array
**must be resolvable** from the subnet listing the *same caller* can read. Horizon expands those ids against a
separate subnet list and substitutes the resolved object per id; an unresolvable id becomes a null entry and
the wizard row fails to render. Today the two paths use different scopes:

- `networkSubnetIDs` — `SELECT id FROM subnets WHERE network_id = $1` (**no project predicate**, unpaginated)
- `ListSubnets` — `WHERE (s.project_id = $1 OR n.shared = true)` **plus LIMIT/OFFSET pagination**

These are aligned so the two cannot disagree. *(Also to confirm during the trace: `ListSubnets`' SELECT does
not fetch `project_id`, so subnet responses may omit `tenant_id`/`project_id`.)*

---

## 6. Volume type & availability zone (existing, verification only)

Metadata the Create Volume dialog loads while constructing itself.

| Entity | Endpoint | State |
|---|---|---|
| Volume type | `GET /v3/types`, `/v3/types/{id}`, `/v3/types/default` | Exists; `default` reports `__DEFAULT__` after SPEC-019. No change planned — confirm it resolves on Horizon's path |
| Availability zone | `GET /v3/os-availability-zone` | Exists, but registered **only** at the flat path, not in the `/v3/:project_id` group (unlike types/limits/quota-sets). Works with the current catalog (`http://o3k:8776/v3`); the asymmetry is confirmed in the trace rather than assumed broken |

---

## State transitions

**None.** Every entity in this feature is either a persisted limit read as-is, a value derived per request, or
a response projection. No lifecycle, no state machine (Art. II).
