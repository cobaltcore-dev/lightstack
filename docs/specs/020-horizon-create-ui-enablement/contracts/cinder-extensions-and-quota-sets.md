# Contract — Cinder extensions & quota sets (F8)

**Service**: Block Storage (Cinder) v3, port 8776 | **Base**: catalog advertises `http://<host>:8776/v3` (no project suffix)
**Requirements**: FR-001, FR-002, FR-002a, FR-003, FR-004, FR-011, FR-012, FR-013

> **Gin routing constraint** *(corrected 2026-07-29)*: registration **order is irrelevant**. Gin's tree
> splices a static child ahead of an existing wildcard at the same level, so `/v3/groups/detail` wins over
> `/v3/groups/:id` whichever is registered first — verified in both orders against the vendored gin v1.12.0.
> The earlier claim that a flat `/v3/<static>` route must precede the `/v3/:project_id` group, repeated in
> comments at `volumes.go:63` and `:145`, was wrong; only **presence** matters.
>
> What *does* panic is reusing a **different param name** at a position already claimed by one —
> `:tenant_id` beside an existing `:id` gives *"':tenant_id' ... conflicts with existing wildcard ':id'"* —
> which is why every route here uses `:id`.

---

## C1. `GET /v3/extensions` — **NEW** (FR-011, FR-013)

Currently **absent (404)** — first suspected for the dead Create Volume modal, but **not** the blocker:
Horizon's volume-create form probes it through `cinder_az_supported`, which catches the failure, shows a
message and opens the form without an availability-zone choice. The modal's root was `GET /v3/groups/detail`
(FR-021). Cinder v3 upstream **does** serve this endpoint, so its absence is an Art. I gap in its own right.

**Response `200`**

```json
{"extensions": [
  {"alias": "os-availability-zone",
   "name": "AvailabilityZones",
   "description": "Describe Availability Zones.",
   "updated": "2013-06-27T00:00:00+00:00",
   "links": []}
]}
```

**Rules**
- Advertise **only** extensions Lightstack genuinely implements. `AvailabilityZones` is backed by the existing
  `GET /v3/os-availability-zone` (`internal/cinder/volumes.go:143`, project-scoped at `:268`). Any further alias is added only if the
  trace shows Horizon requires it **and** Lightstack implements it.
- Register at the flat path **and** in the `/v3/:project_id` group (mirroring how types/limits/quota-sets are
  registered twice).
- Touches no database → unit-testable without `LIGHTSTACK_TEST_DB_URL`.

---

## C2. `GET /v3/limits` and `GET /v3/{project_id}/limits` — absolute limits (existing, **source changes**)

**Response `200`** — `{"limits": {"rate": [], "absolute": { … }}}`

| Key | Source | Change |
|---|---|---|
| `maxTotalVolumes`, `maxTotalSnapshots`, `maxTotalVolumeGigabytes`, `maxTotalBackups`, `maxTotalBackupGigabytes` | **limits helper → `cinder_quotas` ⊕ defaults** | **was hardcoded** — `PUT /v3/quota-sets/{id}` had *no effect* on limits (G12) |
| `totalVolumesUsed`, `totalSnapshotsUsed`, `totalGigabytesUsed` | live query, `status != 'deleted'` | unchanged |
| `totalBackupsUsed`, `totalBackupGigabytesUsed` | live query on `volume_backups` (migration 022; no `deleted` status — rows are removed) | **was hardcoded `0`** → real (FR-004) |

**Rules**
- Limits resolve through the **single package helper** shared with C3/C4/C5 → C2 and C3 report identical
  limits (the FR-003 invariant).
- `GetLimitsNoProject` is byte-identical to `GetLimits` today (G13) — it **delegates** instead of duplicating.
- Both handlers currently read the token's `project_id` even on the `/v3/:project_id/limits` route; that stays
  the non-admin behavior and is now explicit (C6).

---

## C3. `GET /v3/quota-sets/{project_id}` — quota set (existing, **limits unchanged in value**)

**Response `200`** — `{"quota_set": {"id": "<project>", "volumes": 100, "snapshots": 100, "gigabytes": 10000, "backups": 100, "backup_gigabytes": 10000, "per_volume_gigabytes": 1000}}`

**Changes**: values now come from the shared helper (so C2 ⇄ C3 agree); the duplicated defaults map at
`internal/cinder/quotas.go:143` is removed; authorization per C6.

---

## C4. `GET /v3/quota-sets/{project_id}?usage=true` — **NEW** (FR-002, amended 2026-07-29)

Currently returns flat ints regardless of the parameter, so `cinder quota-show --usage` and
`cinderclient.quotas.get(project, usage=True)` get `volumes: 100` where they expect an object.

**This is a query parameter, not a `/detail` sub-resource.** `/detail` is Nova's spelling; the Block Storage
v3 API has no such path and documents `usage` instead ("Show project's quota usage information. Default is
`false`"). The earlier draft of this contract specified `/detail` by analogy with Nova — that was never
observed in the FR-014 trace (research.md G9 is a code-map inference, unlike G2/G5 which carry live `404`
evidence), and no client requests it. Any value `strconv.ParseBool` accepts turns the usage view on; anything
else is the documented default, `false`.

**Response `200`**

```json
{"quota_set": {
  "id": "<project_id>",
  "volumes":   {"in_use": 1, "limit": 100,   "reserved": 0},
  "snapshots": {"in_use": 0, "limit": 100,   "reserved": 0},
  "gigabytes": {"in_use": 8, "limit": 10000, "reserved": 0}
}}
```

Same resource set and limits as C3; `in_use` from the usage queries; `reserved` always `0` (no reservation
model — Art. II).

*(Upstream also emits per-volume-type keys such as `volumes___DEFAULT__`. Given SPEC-019 established
`__DEFAULT__` as Lightstack's default type name, these are a natural follow-on but are added **only** if the trace
shows Horizon needs them — otherwise out of scope.)*

---

## C5. `GET /v3/quota-sets/{project_id}/defaults` — **NEW** (FR-002)

Currently **404**. Same shape as C3, **code defaults only** (no `cinder_quotas` read, no usage).

---

## C6. Authorization (FR-002a) — applies to C3, C4, C5

**Cinder's quota-set endpoints have no authorization check whatsoever today** — any authenticated caller can
read, write, or delete any project's quota. This contract closes that.

| Caller | URL project id | Result |
|---|---|---|
| non-admin | own project | `200`, own quota |
| non-admin | **another project** | `403` on show and defaults, refused before any lookup |
| `admin` role | any project (name or UUID) | `200`, that project's quota |

Implemented with `common.HasRole(c, "admin")`. The existing name→UUID project lookup is retained for the
admin path.

---

## C8. Quota **write** authorization — `PUT` / `DELETE /v3/quota-sets/{project_id}` (FR-020)

**Today: no authorization check of any kind** — any authenticated caller can rewrite or reset **any**
project's quota. That is a cross-project write hole in code this feature already edits.

**Required**: both become **admin-only**, matching Nova's existing `UpdateQuotaSet` and upstream Cinder.

| Caller | Result |
|---|---|
| non-admin | **`403`** — quota unchanged |
| `admin` role | `200` / `204` as today |

This is a **deliberate behavior change** (an explicit exception to FR-016) and must be tested, not slipped in.
Error text follows the existing house style, e.g. `"Policy doesn't allow quota updates to be performed. Admin
role required."` (cf. `internal/nova/quotas.go`).

---

## C7. Create-Volume supporting metadata (verification only, no change planned)

| Endpoint | State |
|---|---|
| `GET /v3/types`, `/v3/types/{id}`, `/v3/types/default` | Exist; `default` reports `__DEFAULT__` (SPEC-019) |
| `GET /v3/os-availability-zone` | Registered at the flat path and — since T037 — in the `/v3/:project_id` group too, because that group's own `/extensions` advertises `os-availability-zone` |

## Test assertions (FR-015)

1. C1 returns `200` listing `AvailabilityZones` (fails today: 404). Runs without a DB.
2. C2 and C3 report the **same** limit for `volumes`, `snapshots`, `gigabytes`.
3. A `cinder_quotas` row override for `volumes` is reflected in **both** C2 and C3 (fails today: C2 ignores the table).
4. C4 returns `200` with `{in_use, limit, reserved}` per resource (fails today: flat ints), and **without**
   the parameter the response stays flat (FR-016). No `/detail` route is registered for block storage.
5. C5 returns `200` with defaults and no usage (fails today: 404).
6. C2's `totalBackupsUsed` reflects seeded backups, not `0`.
7. C6: a non-admin is not served another project's quota; an admin is.
8. Routes register without a gin panic (a startup smoke test covers registration; the panic risk is a conflicting
   parameter name at a position already claimed, not registration order).
9. C8: a non-admin `PUT /v3/quota-sets/{id}` returns **403** and leaves the quota unchanged (fails today: it
   succeeds); the same for `DELETE`; an admin still succeeds.
