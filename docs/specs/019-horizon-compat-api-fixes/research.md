# Phase 0 Research — Horizon compat fixes (F4, F5, F6, F9)

All ambiguities were resolved in the spec's **Clarifications** and **Assumptions**; this file records the
resulting design decisions, each grounded in the code investigation and the upstream OpenStack docs. No
`NEEDS CLARIFICATION` markers remain.

Sources: code root-causes (four Explore passes, 2026-07-24); upstream docs via Context7 —
`/openstack/glance` (policy), `/websites/openstack_api-ref` (Cinder v3 volume), `/openstack/neutron`
(network list / external networks); the SPEC-018 report `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`.

---

## R1 — F9: Cinder volume response completeness

**Decision.** Introduce a single `serializeVolume(...)` helper in `internal/cinder/volumes.go` and use it for
`GetVolume`, `ListVolumesDetail`, and `CreateVolume`. It emits the full upstream Cinder v3 volume field set,
every field non-null:

| Field | Value source |
|---|---|
| `id`, `name`, `description`, `status`, `size`, `created_at`, `updated_at`, `attachments`, `bootable` (string `"true"`/`"false"`) | existing DB columns (unchanged) |
| `metadata` | `{}` (or joined from `volume_metadata`; empty object is sufficient for rendering) |
| `volume_type` | the volume's stored type, else `__DEFAULT__` |
| `availability_zone` | `"nova"` |
| `encrypted` | `false` |
| `multiattach` | `false` |
| `consumes_quota` | `true` |
| `shared_targets` | `false` |
| `user_id` | DB `volumes.user_id` (now SELECTed) |
| `os-vol-tenant-attr:tenant_id` | the volume's `project_id` |
| `os-vol-host-attr:host` | a stable non-null host string (the configured service/node host, e.g. `o3k`) |

**`volume_type` persistence.** `CreateVolume` currently parses `volume_type` from the request and discards it;
the INSERT must bind it (defaulting to `__DEFAULT__` when empty), and `GetVolume`/`ListVolumesDetail` must
`SELECT` it back. `GetDefaultVolumeType` (`GET /v3/types/default`) is changed to return `__DEFAULT__` so the
two agree (spec Q2).

**Rationale.** The 500 is a `TemplateSyntaxError` in Horizon's volume-row template dereferencing fields that
O3K omits (they become `None`). Emitting the upstream field set with non-null defaults is the direct fix and
matches Constitution Art. I. A shared serializer removes the current per-handler drift (three hand-built
`gin.H` maps) and keeps show/list/create consistent (FR-005). `__DEFAULT__` is upstream Cinder's canonical
default-type name (spec Q2), giving CLI/Terraform parity.

**Alternatives considered.** (a) Patch only `GetVolume`/`ListVolumesDetail` inline — rejected: perpetuates the
drift that caused the omission. (b) Add DB columns for `availability_zone`/`encrypted`/… — rejected: O3K does
not model those, constant defaults suffice, and the spec puts data-model redesign out of scope. (c) Default
type `"default"` (O3K's current `types/default`) or `"ceph-rbd"` — rejected per spec Q2 in favor of
`__DEFAULT__`.

---

## R2 — F5: service-catalog endpoint host

**Decision.** Primary fix = **migration `064_fix_cinder_volume_alias_localhost`** that rewrites the one
truly-broken endpoint to the `http://o3k:` convention every other endpoint already uses:

- service `…015` (`volume`/`cinder` alias, seeded by `054`) `http://localhost:8776/v3` → `http://o3k:8776/v3`

After the reseed, the **existing** runtime substitution (`substituteURLTemplates`, `internal/keystone/auth.go`)
rewrites `http://o3k:` → the configured `O3K_ENDPOINT_HOST` exactly as it does for every other endpoint.
**Plus** a small defensive change: make `substituteURLTemplates` also rewrite `http://localhost:` /
`https://localhost:` → the configured host, so any future `localhost`-seeded endpoint cannot recur this bug
(a no-op when the host is unset, since it defaults to `localhost`).

**Implementation finding (s3 `…016`).** The S3/object-store endpoint is **not** reseeded. `BuildServiceCatalog`
already special-cases `s3`: it serves the resolved `cfg.Glance.S3Endpoint` and omits the service when RGW isn't
configured (`auth.go` ~L607-632), so migration 062's `localhost:7480` seed is never advertised. Its real value
is the external RGW URL (not `o3k:port`), so reseeding it would be wrong. F5's s3 scope (spec Q3) is therefore
satisfied by **verification** — the existing `TestBuildServiceCatalogS3EndpointFromConfig` already proves the s3
endpoint resolves to the config value, never localhost.

**Rationale.** The root cause is a *data* inconsistency: `054` and `062` seeded `localhost` while `052` had
already normalized all earlier endpoints to `o3k`, and the one-shot `052` (keyed to service ids `…010`–`…014`)
could never touch `…015`/`…016`. Reseeding to `o3k` makes the catalog uniform and needs no logic change; the
defensive substitution is belt-and-suspenders. This satisfies FR-006/006a/007/008 and Art. I. The S3 endpoint
is now in scope and verified in the real (RGW) pass (spec Q3).

**Alternatives considered.** (a) Runtime substitution only (rewrite `localhost` but leave the seeds) —
rejected as primary: leaves the DB inconsistent with every other endpoint and relies solely on runtime
rewriting; kept only as the defensive layer. (b) Rewrite *any* host to the configured host unconditionally —
rejected: too broad, could clobber a deliberately distinct host; matching the established `o3k` convention is
safer.

---

## R3 — F4: Neutron network list filters

**Decision.** In `ListNetworks` (`internal/neutron/network.go`) honor the three filters Horizon's
`network_list_for_tenant` sends, so its sub-queries are disjoint:

- `shared` — `shared=true` → only `shared=true` rows; `shared=false` → exclude shared rows.
- `router:external` — read the query param; since O3K models no external networks, `router:external=true`
  returns an **empty** set. Every network response (list/show/create) emits `"router:external": false`.
- `project_id` / `tenant_id` — apply as a **narrowing** predicate intersected with the caller's authorized
  scope: a non-admin can never see beyond their own project + shared (Art. V); the param cannot widen scope.

De-duplication then follows for free: Horizon issues `tenant_id=<self>&shared=false` (own non-shared),
`shared=true` (shared), and `router:external=true` (external → empty) — three disjoint sets, so each network
renders once. Unfiltered `network list` is unchanged.

**Rationale.** The dup is caused by the hardcoded `WHERE (project_id=$1 OR shared=true)` returning the same
superset for every sub-query. Honoring the filters is the minimal conformant fix (FR-009…013). O3K has no
`external`/`is_external` column and external networks are an admin/provider concept O3K doesn't implement, so
treating all networks as non-external is correct and avoids a schema change (spec F4 Assumption). Confirmed via
Context7 `/openstack/neutron`: external networks are created with `--external` (usually also `--share`) and
surface `router:external`; absent that capability, an empty external set is the honest answer.

**Alternatives considered.** (a) Add an `external` column + admin create-flag — rejected: a full
external/provider-network model is out of scope and unnecessary to stop the dup. (b) De-dup inside a single
unfiltered query — rejected: Horizon drives the sub-queries; the API must honor the filters it is sent, which
is also what python-openstackclient/Terraform expect.

---

## R4 — F6: Glance image delete authorization

**Decision.** Adopt upstream Glance ownership semantics (spec Q1, confirmed against `/openstack/glance` policy
docs: default `delete_image` = `not_protected_and_is_owner`, with the admin override):

1. **Preserve the owner on create.** `CreateImage` binds `project_id` for images of **every** visibility
   (today it binds NULL unless `visibility == "private"`). Visibility stays independent and governs read access.
2. **Authorize delete as owner-or-admin.** In `DeleteImage`:
   - `SELECT owner (project_id), visibility WHERE id=$1`; **no row → 404**.
   - *Visible?* `isAdmin || owner == caller.project || visibility == 'public'`; if not visible → **404**
     (don't leak existence of a private image the caller can't see).
   - *Authorized to delete?* `isAdmin || owner == caller.project`; if visible but not authorized (e.g. a public
     image the caller doesn't own) → **403**.
   - keep the existing in-use guard (instances/volumes referencing the image → 409).
   - `DELETE FROM images WHERE id=$1`; **check rows-affected → 0 ⇒ 404** (race guard).
3. **Admin detection** reuses the established idiom: `c.GetStringSlice("roles")` contains `"admin"` (JWT roles
   set by `middleware/auth.go`; cf. `keystone/handlers.go:146` `hasRole`, `cinder/volumes.go:943`). Add a small
   local `isAdmin(c)` helper in the glance package (or reuse a shared one) rather than importing keystone.

**Rationale.** The no-op is a WHERE mismatch: public images are stored ownerless (`project_id` NULL) so
`… AND project_id=$2` matches nothing, yet the discarded rows-affected count lets the handler return 204. The
upstream-faithful fix removes the mismatch at its source (keep the owner) and applies the real Glance
authorization (owner-or-admin), giving correct 403/404 semantics (FR-014…016, Art. I & V). Works identically in
stub and real (RGW) modes because the DB row — the list's source of truth — is always removed regardless of the
best-effort backend object delete.

**Legacy rows.** Public images created before this change keep `project_id` NULL; they remain deletable by an
**admin** via the override (the common operator case). No backfill migration is attempted (the original owner
is unrecoverable), and none is needed.

**Alternatives considered.** (a) "Anyone who can see it may delete it" (align delete to the visibility check) —
rejected after the Q1 clarification: diverges from upstream, where visibility ≠ delete rights. (b) Admin-only
for all deletes — rejected: over-restrictive vs. upstream default (owners may delete their own images). (c)
Model `protected` images — out of scope (O3K has no `protected` column).

---

## Cross-cutting decisions

- **Testing (Art., Test-First).** Each fix gets a Go unit test asserting the corrected behavior (RED before,
  GREEN after) in the existing CI unit-tests job: F9 field/default presence on show + list-detail; F4 filter
  disjointness + no-dup + `router:external` present; F6 owner-or-admin authz + 403/404 + rows-affected; F5
  catalog resolution yields no `localhost` for `volume`/`volumev3`/`s3`. Manual stub + real runbook passes per
  SPEC-018 close the loop (FR-019).
- **No new dependencies; one migration.** Only migration `064` (F5). F9 reuses `volumes.volume_type`
  (migration `016`); F4/F6 are code-only. Migration ships reversible up/down (Art., Backwards-Compat).
- **No regression.** All other response fields and flows for the four services stay unchanged (FR-018);
  existing unit/integration suites must stay green.
