# Phase 1 Data Model — Horizon compat fixes (F4, F5, F6, F9)

This feature is response-shape + behavior focused. **No new tables and only one migration** (`064`, F5). The
entities below already exist; the notes describe how each fix uses them.

---

## Volume (`volumes` table) — F9

Existing columns used (no new columns): `id`, `name`, `description` (mig `012`), `project_id`, `user_id`,
`size_gb`, `status`, `bootable`, `attached_to_instance_id`, `volume_type` (mig `016`), `source_image_id`
(mig `060`), `created_at`, `updated_at`.

**Change**: `CreateVolume` now **persists** `volume_type` (bind the request value, default `__DEFAULT__` when
empty — the column exists but was previously parsed and discarded). `GetVolume` / `ListVolumesDetail` now
`SELECT` `user_id` and `volume_type` (previously omitted from the SELECT).

**Serialized response shape** (single source: `serializeVolume`) — every field non-null:

| Field | Type | Source / default |
|---|---|---|
| `id`, `name`, `description`, `status`, `created_at`, `updated_at` | string | DB |
| `size` | int | DB `size_gb` |
| `bootable` | string | DB, formatted `"true"`/`"false"` (upstream shape) |
| `attachments` | array | DB `attached_to_instance_id` → `[{server_id, device}]` or `[]` |
| `metadata` | object | `{}` (or `volume_metadata` join) |
| `volume_type` | string | DB `volume_type`, else `__DEFAULT__` |
| `availability_zone` | string | `"nova"` |
| `encrypted` | bool | `false` |
| `multiattach` | bool | `false` |
| `consumes_quota` | bool | `true` |
| `shared_targets` | bool | `false` |
| `user_id` | string | DB `user_id` |
| `os-vol-tenant-attr:tenant_id` | string | DB `project_id` |
| `os-vol-host-attr:host` | string | stable non-null host (configured service/node host) |

Validation/consistency: the same serializer feeds show, list-detail, and create, so all three agree (FR-005).

Related endpoint: `GET /v3/types/default` returns `{"id":"__DEFAULT__","name":"__DEFAULT__", ...}` (was
`"default"`), so `volume show`'s `volume_type` and the default-type endpoint agree.

---

## Network (`networks` table) — F4

Existing columns: `id`, `name`, `project_id`, `admin_state_up`, `status`, `shared` (bool, default false,
mig `001`), `mtu`, `network_type`, `physical_network`, `created_at`, `updated_at`. **No `external`/`is_external`
column exists and none is added.**

**Change** (query behavior only, no schema change): `ListNetworks` reads and applies

- `shared` (bool) — filter on the `shared` column.
- `router:external` (bool) — no backing column ⇒ `true` yields an empty set; `false`/absent unaffected.
- `project_id` / `tenant_id` — narrowing predicate intersected with the caller's authorized scope.

**Serialized response shape**: unchanged fields plus a new constant attribute `"router:external": false` on
list, show, and create responses. (`shared` is already emitted from the DB value.)

State/transition: none.

---

## Image (`images` table) — F6

Existing columns: `id`, `name`, `project_id` (owner; `ON DELETE SET NULL`, mig `001`), `status`, `visibility`,
`properties` (mig `051`), `cached_at` (mig `046`), timestamps. **No soft-delete column; delete is a hard
`DELETE`.** No new column.

**Change** (write behavior, no schema change):

- `CreateImage`: bind `project_id = <caller project>` for images of **every** visibility (previously bound
  NULL unless `visibility == "private"`). This is the root-cause fix — public images retain their owner.
- `DeleteImage`: authorize by **owner-or-admin**; delete by `id`; verify rows-affected. Visibility governs
  read (see contract), ownership governs delete.

Legacy data: images created before this change with `project_id` NULL remain (admin-deletable via the
override). No backfill migration (original owner unrecoverable; not needed).

---

## Service catalog (`services` / `endpoints` tables) — F5

Existing rows (seeded by earlier migrations). **Migration `064` rewrites one endpoint URL** — the `volume`
alias (service `…015`) — to the `http://o3k:` convention used by all other endpoints, so runtime host
substitution then resolves it to the configured host:

| Service id | type/name | Before | After (migration `064`) |
|---|---|---|---|
| `…015` | `volume` / `cinder` (alias, from `054`) | `http://localhost:8776/v3` | `http://o3k:8776/v3` |

The s3 endpoint (service `…016`, from `062`) is **not** touched: `BuildServiceCatalog` overrides it from the
resolved `cfg.Glance.S3Endpoint` (or omits it when RGW isn't configured), so its seeded `localhost:7480`
placeholder is never served — reseeding it to `o3k` would be wrong (its real value is the external RGW URL).

Reversible: `064.down.sql` restores the `…015` `localhost` URL. No other catalog rows change. A defensive
change in `substituteURLTemplates` additionally normalizes any `localhost` host at runtime (research R2).

---

## Summary of persistence impact

| Fix | New table | New column | New migration | Write-path change |
|---|---|---|---|---|
| F9 | no | no | no | persist `volume_type` on create (existing column) |
| F5 | no | no | **`064`** (reseed the `…015` volume-alias URL) | no |
| F4 | no | no | no | no (read-path filters only) |
| F6 | no | no | no | bind image `project_id` for all visibilities on create |
