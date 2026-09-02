# Phase 1 Data Model: SPEC-025 Follow-ups

**No new entities. No new columns.** One data migration, `073`, was added in review: a volume with a
pending transfer holds upstream's `awaiting-transfer` status, and the migration gives that status to the
volumes of transfers already pending (see the release notes).

Every one of the seven findings is behavioural: the tables that hold the answer already exist, and the
defect is that nothing reads them, that something reads them wrongly, or that a write is discarded
before it reaches them. This document therefore records **what changes about how existing data is
read and written**, which is the only part a reviewer needs to check.

---

## Reads that change

### `instances` — gains filter predicates (US1)

The listing query is unchanged in shape; predicates are appended to the accumulator it already
carries. Columns that become filterable, with the query parameter that reaches them:

| Parameter | Column | Notes |
|---|---|---|
| `name` | `name` | Regex match, per upstream (research R1) — not equality |
| `status` | `status` | Against the vocabulary the listing itself emits |
| `image` | `image_id` | UUID; `image_ref` is upstream's deprecated alias |
| `flavor` | `flavor_id` | UUID |
| `changes-since` | `updated_at` | Timestamp comparison |
| `user_id` | `user_id` | Administrator-only upstream |
| `project_id` / `tenant_id` | `project_id` | Narrows within scope only — never widens (FR-006a) |
| `key_name` | `key_name` | Migration 063 |
| `locked` | `locked` | Migration 015; boolean vocabulary per `common.ParseQueryBool` |
| `task_state` | `task_state` | Migration 005 |
| `power_state` | `power_state` | Integer |

**Not filterable, refused per FR-004**: `host`, `hostname`, `availability_zone`, `tags` — no column
exists for any of them. `ip` is expressible through `ports.fixed_ips` (JSONB) and its fate is tied to
US7's batched query (research R1).

**Invariant that must not break**: an unfiltered request produces the same SQL it produces today.
`common.SQLFilter.Where()` renders an empty string when no predicate was added, which is what
guarantees it.

### `ports` + `floating_ips` — one query instead of N (US7)

Same SQL body, same `LEFT JOIN floating_ips` with `ARRAY_AGG ... FILTER`, same `GROUP BY p.id`. Two
changes:

- `WHERE p.device_id = $1` becomes a match against the page's instance ids.
- `AND p.project_id = $2` — currently the *instance's own* project, passed per row — becomes
  `JOIN instances i ON i.id = p.device_id AND i.project_id = p.project_id`. This states the same
  constraint per row rather than per call, so a cross-tenant administrative listing stays correctly
  attributed. **Dropping this predicate would be a defect**: a port whose project disagreed with its
  instance's could contribute an address to the wrong server.
- The result set gains `device_id` so rows can be bucketed by server.

### `volumes`, `snapshots`, `volume_backups` — read on the write path (US3)

These are read today only by the quota *views*. They gain a reader on every path that adds to them,
inside a transaction that locks the project's quota row.

**One counting change (FR-026), and it is user-visible**: the `gigabytes` usage figure currently
reads `SUM(size_gb) FROM volumes`. It must become volume size **plus** snapshot size, matching
upstream, which describes the allowance as *"total gigabytes for volumes and snapshots"*.
`snapshots.size_gb` already exists and is `NOT NULL`, so no schema change is needed — the column has
simply never been summed.

Because `resolveCinderQuotaLimits` and `cinderQuotaUsage` are the single resolution behind all three
reporting surfaces (`quotas.go:131`, `quotas.go:219`, `volumes.go:2181`), correcting the count lands
in the quota-set view, the quota-set-detail view and the absolute-limits view at once — which is the
property FR-026a requires, and also why the change is visible in the dashboard's usage bars.

### `cinder_quotas` — read under a row lock (US3)

Unchanged in structure. The check performs `SELECT ... FOR UPDATE` on the project's row for the
resource being consumed, falling back to the package defaults when no row exists (FR-025). Locking is
what makes FR-023 hold: two concurrent creations against one remaining allowance serialise, and
exactly one succeeds.

**Note**: a project with no `cinder_quotas` row has no row to lock. The defaults path must still
serialise, or the concurrency requirement holds only for projects an administrator has touched.
This is the same case `nova.CheckQuota` handles by proceeding with the default limit inside the
transaction; whether that is sufficient serialisation for the no-row case is a task-level check, not
an assumption to inherit.

---

## Writes that change

### `images` — the patch actually applies (US2)

Columns reachable through the patch allowlist, unchanged: `name` (`NOT NULL`), `visibility`
(default `'private'`), `min_disk_gb` (default `0`), `min_ram_mb` (default `0`).

| Operation | Today | After |
|---|---|---|
| `replace` on an allowlisted path | Applied, errors logged and swallowed | Applied inside a transaction; a failure fails the request |
| `add` on an allowlisted path | **Silently discarded**, `200` returned | Applied — same write as `replace` (all four are scalars on one row) |
| `remove` on an allowlisted path | **Silently discarded**, `200` returned | **403**, naming the path (research R2 — all four are base properties with schema defaults, so none has an absent state) |
| Any operation on a non-allowlisted path | **Silently discarded**, `200` returned | **403** for a service-managed property, naming the path |
| An operation RFC 6902 does not define | **Silently discarded**, `200` returned | **400**, naming the operation |
| A write matching zero rows | `200` with the unchanged image | **404** — the affected-row count is checked (FR-015) |

**Atomicity (FR-014)**: the loop currently issues one `UPDATE` per operation and continues past
failures. It becomes one transaction: either every operation in the document applies or none does.

### `instances` — one handler shrinks (US5)

`RestoreInstance` keeps its route and loses its success branch: it answers 409 for any server it
reaches, and the dispatcher answers 404 for one that is gone. No column is written, no column is read,
and deletion semantics are untouched — the removed branch could never execute, since nothing in the
product has ever written the `SOFT_DELETED` status it gated on.

---

## State transitions

**None added.** Deferred delete was the one candidate for a new persisted state and was decided
against (FR-036), so no status value, retention window or reclaim sweep enters the model. The
`SOFT_DELETED` status is never written, and its one comparison goes with the handler's success branch.

---

## Cache

`service_catalog:<project_id>`, 24-hour TTL, Redis-backed, **disabled by default**
(`cache.enabled: false`).

The key gains the resolved host (FR-031a). This is not an optimisation: once endpoints derive from
the request host, a key that ignores the host lets one caller's client-supplied `Host` determine the
endpoints another caller in the same project is handed — and therefore where that caller sends its
token. The cache's own comment currently asserts the catalog is immutable, which the feature makes
false and must be corrected alongside the key.
