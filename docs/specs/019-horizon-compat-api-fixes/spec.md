# Feature Specification: Horizon compatibility — Cinder/Neutron/Glance API & response fixes (F4, F5, F6, F9)

**Feature Branch**: `feature/532` (feature id: `019-horizon-compat-api-fixes`)
**Created**: 2026-07-24
**Status**: Draft
**Input**: cobaltcore-dev/cloud-storage#532 — "lightstack — Horizon compat: API/response fixes (F4, F5, F6, F9)" (sub-issue of #531, step 1 of 3; parent epic #423). Discrete backend/response fixes grounded in the SPEC-018 verification report [`docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`](../../compatibility/HORIZON_COMPATIBILITY_REPORT.md).

## Context

The SPEC-018 Horizon verification (feature/482) found Lightstack is a drop-in for the OpenStack **API/CLI**, but the Horizon **dashboard** is not usable for resource management. It logged nine findings (F1–F9). This feature fixes the four that are self-contained backend/response defects, independently verifiable, and don't depend on the create/write-UI investigation:

- **F5** (major) — the block-storage service catalog advertises an unreachable `localhost` endpoint.
- **F9** (major) — the Volumes panel returns HTTP 500 once any volume exists.
- **F4** (medium) — a single network is listed twice in the Project panel. *(Does double duty: the deduplicated network list is consumed by the launch-instance wizard fixed in the next ticket, #533.)*
- **F6** (medium) — image delete silently does nothing.

The create/write-UI cluster (F2, F3, F7, F8) and the noVNC console (F1) are **out of scope** here — they are ticket #533 and #534 respectively.

## Clarifications

### Session 2026-07-24

- Q: Which environments must each fix be verified in? → A: **Both** stub mode and real mode, for every one of the four findings, reusing the existing setup scripts (no new harness). **Stub** — Horizon + Lightstack (stub) via `deployments/docker-compose-horizon.yml`, per the SPEC-018 stub runbook (`docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md`). **Real** — `charts/lightstack` in real mode + `deployments/horizon-chart/` on **k3s** (helm deploy + Horizon), with Ceph **RGW/S3** provided by **Rook on minikube** (`deployments/rook/setup-rook-rgw.sh`), per the SPEC-018 real runbook (`docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md`). Each finding's panel/behavior must be observed fixed in both modes.
- Q: F6 — who may delete an image (matching OpenStack)? → A: Confirmed against the Glance policy docs — upstream authorizes deletion by **owner-or-admin** (default `delete_image` = `not_protected_and_is_owner`, plus the admin override). *Visibility* (public) governs who can see/boot an image, **not** who can delete it, and a public image **keeps its owner**. Adopt that: record the owning project on create for **all** visibilities (stop storing public images ownerless — the root cause), authorize delete as owner-or-admin (admin via the token role), return **403** for a non-owner non-admin and **404** for a missing image. Lightstack does not model `protected` images, so protected-blocking is out of scope.
- Q: F9 — default `volume_type` value when a volume has no explicit type? → A: **`__DEFAULT__`** (upstream Cinder's canonical default-type name); also align Lightstack's `GET /v3/types/default` to report `__DEFAULT__`, so `volume show` and the types endpoint agree.
- Q: F5 — scope of the identical `localhost` bug on the S3/RGW catalog endpoint (service `…016`, migration `062`)? → A: **Include and verify it** — the S3/object-store endpoint MUST also advertise the configured host (not `localhost`), gated as a requirement and verified in the real (RGW) pass (no longer out of scope).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Volumes panel works when a volume exists (Priority: P1)

An operator opens the Horizon **Project → Volumes** panel in a deployment that already has one or more
volumes. Today the panel returns an HTTP 500 (`TemplateSyntaxError`): Lightstack's Cinder volume response leaves
about ten fields that Horizon's volume table renders unset, so they arrive as `None` and the row template
crashes. The empty panel renders, but the moment any volume exists the whole page is dead.

**Why this priority**: This is a hard crash on a read/list path — the most severe kind of failure. A single
volume makes the entire Volumes panel unusable in the dashboard, even though the underlying Cinder API works
via the CLI. Highest user-facing impact of the four.

**Independent Test**: Create a volume via the CLI, then load Project → Volumes in Horizon (and run
`openstack volume show <id>`). The panel loads and lists the volume; the show response reports non-null
values for volume type, availability zone, encryption and ownership fields.

**Acceptance Scenarios**:

1. **Given** at least one volume exists, **When** the Volumes panel is loaded, **Then** it returns successfully (no HTTP 500) and lists the volume(s).
2. **Given** a volume, **When** `openstack volume show <id>` is run, **Then** the response includes non-null `volume_type`, `availability_zone`, `encrypted`, `user_id`, tenant/host ownership, `multiattach`, `consumes_quota`, `shared_targets`, and `metadata` fields, in the shape upstream Cinder returns.
3. **Given** a volume created with an explicit volume type, **When** it is read back, **Then** the response reports that type; **and given** a volume created with no type, **Then** it reports `__DEFAULT__` (never null).
4. **Given** no volumes exist, **When** the Volumes panel is loaded, **Then** it still renders (empty), unchanged from today.

---

### User Story 2 - Block-storage service is reachable from the dashboard (Priority: P1)

An OpenStack client (Horizon or the CLI) resolves the block-storage service from the identity catalog and
must reach a working Cinder endpoint. Today the catalog advertises a second, aliased volume endpoint (the
`volume` service type that upstream Horizon also resolves) hardcoded to `http://localhost:8776/...`. In any
deployment where the reachable host is not `localhost` (docker-compose, Kubernetes, a real host), that
endpoint is unreachable, so a client that picks it cannot talk to Cinder at all.

**Why this priority**: A catalog endpoint that points at `localhost` is unreachable for every non-local
deployment — a correctness defect in the service catalog, the contract every OpenStack client depends on. It
is a small, high-confidence fix.

**Independent Test**: In a deployment configured with a non-`localhost` endpoint host, resolve the
block-storage service (`openstack catalog show volume` and `openstack catalog show volumev3`) and confirm
every advertised URL uses the configured host, then perform a volume operation through it.

**Acceptance Scenarios**:

1. **Given** a deployment configured with a non-`localhost` endpoint host, **When** the block-storage service is resolved from the catalog, **Then** every advertised endpoint URL (including the `volume` alias) uses the configured host, not `localhost`.
2. **Given** the resolved endpoint, **When** a client performs a volume list/show through it, **Then** the request reaches Cinder and succeeds.
3. **Given** the default local configuration (no endpoint host set), **When** the catalog is resolved, **Then** endpoints continue to work as before (no regression for the local/dev case).
4. **Given** the same catalog-host fix, **When** the S3/object-store service (`…016`) is resolved in a non-`localhost` (real/RGW) deployment, **Then** its advertised endpoint also uses the configured host, not `localhost`.

---

### User Story 3 - Each network is listed once (Priority: P2)

A project user opens **Project → Network → Networks**. Horizon builds this panel by merging the results of
several filtered list queries (the project's own networks, shared networks, external networks). Lightstack's Neutron
ignores those filters and returns the same superset for every sub-query, so a single network is rendered
twice. Unfiltered `openstack network list` shows it once, so the API list itself is fine — the filters are
the problem.

**Why this priority**: A confusing/incorrect list is a medium defect on its own, but it is prioritized early
because the deduplicated, filter-honoring network list is what the launch-instance wizard consumes in the
next ticket (#533, F7) — fixing it here unblocks that work.

**Independent Test**: With one network present, query the list endpoint with the `shared`, `router:external`,
and `tenant_id`/`project_id` filters individually and confirm each returns the correct disjoint subset; then
load Project → Networks and confirm the network appears exactly once.

**Acceptance Scenarios**:

1. **Given** one project-owned, non-shared network, **When** the list is queried with `shared=true`, **Then** that network is **not** returned; **When** queried with `shared=false` (or the caller's `tenant_id`), **Then** it **is** returned.
2. **Given** the list is queried with `router:external=true`, **When** the deployment models no external networks, **Then** the result is empty; **and** every network response includes the `router:external` attribute.
3. **Given** the Project → Networks panel, **When** it is loaded, **Then** each network appears exactly once (no duplicate rows).
4. **Given** an unfiltered `network list`, **When** it is run, **Then** each network is returned exactly once (unchanged).
5. **Given** a project user supplies another project's id as a `tenant_id` filter, **When** the list is queried, **Then** they still do not see networks outside their authorized scope (the filter narrows within scope; it does not widen it).

---

### User Story 4 - Deleting an image removes it (Priority: P3)

An operator deletes an image (`openstack image delete <id>` or the Horizon Images panel). Today the call
returns success but the image stays in `image list` — the delete is a no-op. Root cause: Lightstack stores public
images **ownerless** (`project_id` NULL), yet the delete only matches rows scoped to the caller's project, so
it matches zero rows and still reports success. Upstream Glance instead keeps every image's owner and
authorizes deletion by **owner-or-admin** — visibility controls who can *see* an image, ownership controls
who can *delete* it.

**Why this priority**: Images cannot be removed via the CLI or dashboard — a real functional gap — but it is a
single self-contained fix with no downstream dependents, so it is sequenced last of the four.

**Independent Test**: Create an image, delete it as its owner (or an admin), and list images — the image is
gone. Delete the same id again → not-found. Attempt a delete as a non-owner non-admin → forbidden, and the
image remains.

**Acceptance Scenarios**:

1. **Given** an image owned by the caller's project (or the caller is an admin), **When** it is deleted, **Then** it no longer appears in `image list` (CLI or dashboard).
2. **Given** a public image created by a project (its owner retained) and then deleted by that owner or an admin, **When** the images are listed, **Then** the deleted image does not reappear.
3. **Given** a delete attempted by a user who is neither the image's owner nor an admin, **When** it runs, **Then** it is rejected (HTTP 403) and the image remains — not a silent success.
4. **Given** an image id that does not exist (or was already deleted), **When** a delete is attempted, **Then** the response is a not-found error (HTTP 404), not a success.
5. **Given** the fix, **When** it runs in both stub and real (RGW/S3) storage modes, **Then** the image is removed from the listing in both.

---

### Edge Cases

- **Volumes panel with zero volumes**: still renders (the crash only occurs once a row exists) — must remain working.
- **Volume with no type / no attachments**: reports `__DEFAULT__` (non-null) and an empty attachments list; still renders.
- **Local/dev deployment (endpoint host unset)**: the catalog fix must not break the case where `localhost` is the correct, reachable host.
- **S3/object-store endpoint host**: the same catalog-host fix applies to the S3/RGW endpoint (`…016`); in a non-`localhost` deployment it must resolve to the configured host, verified in the real (RGW) pass.
- **Network that is both project-owned and shared**: must still appear exactly once after the filters are honored (the sub-queries must be disjoint or de-duplicated).
- **`router:external=true` when no external networks are modeled**: returns an empty set (not the full list), and responses still carry `router:external: false`.
- **Image delete of a nonexistent id**: returns not-found (404), matching upstream Glance, rather than a false success.
- **Delete by a non-owner non-admin**: rejected with 403 (matching upstream Glance's `delete_image` policy); the image is not removed and the caller gets an error, not a false success.
- **Legacy ownerless public images** (created before the owner-preservation fix, or whose owning project was later removed): deletable by an **admin** via the admin override; a non-admin cannot delete an image it does not own.
- **Real (RGW/S3) mode image delete**: the database row (the source of truth for `image list`) is removed regardless of whether the backend object delete succeeds.

## Requirements *(mandatory)*

### Functional Requirements

**F9 — Volume response completeness (US1)**

- **FR-001**: The single-volume detail response (`volume show`) and the list-with-details response (`volume list --long`) MUST include every field Horizon's volume table renders, each present and non-null with a type-correct value matching upstream Cinder's volume schema — at minimum: `volume_type`, `availability_zone`, `encrypted`, `user_id`, the tenant-attr and host-attr ownership fields, `multiattach`, `consumes_quota`, `shared_targets`, and `metadata` — in addition to the fields already returned (`bootable`, `status`, `size`, `created_at`, `updated_at`, `attachments`).
- **FR-002**: The Volumes dashboard panel MUST render without an HTTP 500 whether or not any volume exists.
- **FR-003**: A `volume_type` supplied at volume creation MUST be persisted and read back on show/list-detail; when none is supplied, the response MUST report `__DEFAULT__` (upstream Cinder's canonical default-type name). Lightstack's `GET /v3/types/default` MUST also report `__DEFAULT__`, so `volume show` and the types endpoint agree. *(Today the requested type is parsed at create and discarded.)*
- **FR-004**: Attributes Lightstack does not model (availability zone, encryption, multiattach, backend host, shared targets, quota consumption) MUST be reported with upstream-compatible, non-null default values rather than omitted or null.
- **FR-005**: The `volume show`, `volume list --long`, and volume-create responses MUST report these fields consistently with one another.

**F5 — Service-catalog endpoint host (US2)**

- **FR-006**: Every block-storage endpoint advertised by the identity catalog — including the `volume` service-type alias — MUST resolve to the configured, reachable host in any deployment where a non-`localhost` host is configured; no advertised endpoint may be pinned to `localhost`.
- **FR-006a**: The S3/object-store catalog endpoint (service `…016`, migration `062`) MUST NOT advertise `localhost` in any non-`localhost` deployment. Unlike the block-storage endpoints, it is satisfied *not* by host substitution but by `BuildServiceCatalog` serving the resolved external RGW address (`cfg.Glance.S3Endpoint`) in place of the seeded value, and omitting the service when RGW is unconfigured — the RGW is an external service, so its address is unrelated to `LIGHTSTACK_ENDPOINT_HOST` and migration `064` deliberately leaves `…016` untouched. This is a gated requirement and MUST be verified in the real (RGW) pass.
- **FR-007**: Catalog host substitution MUST apply to all seeded endpoints uniformly, so no endpoint escapes host rewriting because of the literal host it happened to be seeded with. *(Today runtime substitution rewrites only the `http://lightstack:` / `https://lightstack:` prefixes, so a `localhost`-seeded endpoint is never corrected.)*
- **FR-008**: An unmodified OpenStack client resolving the `volume` or `volumev3` service from the catalog MUST reach a working Cinder endpoint and complete a volume operation through it.

**F4 — Network list filters (US3)**

- **FR-009**: `GET /v2.0/networks` MUST honor the `shared` filter: `shared=true` returns only shared networks and `shared=false` excludes shared networks.
- **FR-010**: `GET /v2.0/networks` MUST honor the `router:external` filter, and every network response (list, show, create) MUST include the `router:external` attribute.
- **FR-011**: `GET /v2.0/networks` MUST honor the `project_id`/`tenant_id` filter as a narrowing constraint within the caller's authorized scope — it MUST NOT let a caller enumerate networks outside that scope.
- **FR-012**: When the Project → Networks panel is built from Horizon's several filtered sub-queries, each network MUST appear exactly once (the sub-query results MUST be disjoint or otherwise de-duplicate to a single row per network).
- **FR-013**: Unfiltered `network list` MUST continue to return each network exactly once (no regression to the existing list behavior, ordering, or pagination).

**F6 — Image delete (US4)**

- **FR-014**: Image creation MUST record the owning project for images of **every** visibility — public images MUST retain their owner rather than being stored ownerless — matching upstream Glance, where *visibility* governs who can see/boot an image and *ownership* governs who can delete it. *(Today public images are stored with a NULL owner, the root cause of the no-op delete.)*
- **FR-014a**: Deleting an image MUST be authorized as **owner-or-admin** — the caller's project owns the image, or the caller holds the admin role — matching upstream Glance's default `delete_image` policy (`not_protected_and_is_owner`, plus the admin override). A delete by a non-owner non-admin MUST be rejected with HTTP 403 and MUST NOT be a silent success.
- **FR-015**: A delete that matches no existing image (wrong id, or already deleted) MUST return not-found (HTTP 404); the handler MUST verify a row was actually removed (rows-affected) rather than assuming success. *(The existing codebase already checks rows-affected in comparable handlers.)*
- **FR-016**: After an authorized delete, the image MUST NOT reappear in `image list` (CLI or dashboard) in **both** stub and real (RGW/S3) storage modes; the database row — the source of truth for the listing — MUST be removed regardless of whether the backend object delete succeeds.

**Cross-cutting**

- **FR-017**: Each of the four fixes MUST be covered by an automated test that fails before the fix and passes after, runnable in the existing CI test job, so the defect cannot silently regress.
- **FR-018**: All other existing API behavior and response fields for the affected services (Cinder, the Neutron network list, the identity catalog, Glance) MUST remain unchanged; the existing unit and integration suites MUST continue to pass.
- **FR-019**: Each of the four fixes MUST be verified in **both** operating modes, reusing the existing setup scripts (no new harness), and each corresponding finding's panel/behavior MUST be observed fixed in both:
  - **Stub mode** — Horizon + Lightstack (stub) via `deployments/docker-compose-horizon.yml`, following the SPEC-018 stub runbook (`docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md`).
  - **Real mode** — `charts/lightstack` in real mode + `deployments/horizon-chart/` on k3s (helm deploy + Horizon), with Ceph RGW/S3 provided by Rook on minikube (`deployments/rook/setup-rook-rgw.sh`), following the SPEC-018 real runbook (`docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md`).

### Key Entities *(include if feature involves data)*

- **Volume**: a block-storage volume; its detail/list responses gain the full set of upstream Cinder fields (type, availability zone, encryption, ownership, multiattach, quota, shared-targets, metadata), and it retains the volume type it was created with.
- **Service-catalog endpoint**: an advertised service URL (service type + region + interface); the block-storage `volume` alias (`…015`) and the S3/object-store endpoint (`…016`) must advertise the configured host, not `localhost`.
- **Network**: a Neutron network; list responses become filterable by `shared`, `router:external`, and project scope, and carry the `router:external` attribute.
- **Image**: a Glance image; it retains its owning project across all visibilities (public images are not stored ownerless), and deletion — authorized by owner-or-admin — actually removes it (or returns 404 for missing / 403 for unauthorized).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With at least one volume present, the Horizon Volumes panel loads successfully (HTTP 200, no error) and lists the volume — where today it returns HTTP 500.
- **SC-002**: `openstack volume show <id>` returns non-null values for volume type, availability zone, encryption, and ownership fields, matching the upstream Cinder response shape.
- **SC-003**: In a non-`localhost` deployment, resolving the block-storage service **and the S3/object-store service** from the catalog yields only reachable endpoint URLs (no `localhost`), and a volume operation succeeds through the resolved block-storage endpoint.
- **SC-004**: In the Project → Networks panel a single network appears exactly once (not twice), and each of the `shared` / `router:external` / project filters returns the correct disjoint subset when queried directly.
- **SC-005**: An owner (or admin) `openstack image delete <id>` followed by `openstack image list` shows the image is gone (stub and real modes); deleting a nonexistent id returns 404; a delete by a non-owner non-admin returns 403 and leaves the image in place.
- **SC-006**: The four findings are observed fixed in **both** the stub pass (`docker-compose-horizon.yml` Horizon) and the real pass (k3s + `charts/lightstack` real mode + `horizon-chart` + Rook/Ceph RGW on minikube), re-run from the SPEC-018 runbooks; the existing unit/integration test suites still pass (no regression).

## Assumptions

- **F9 defaults follow upstream Cinder.** Unmodeled fields take upstream-compatible non-null defaults (availability zone `nova`, `encrypted` false, `multiattach` false, `consumes_quota` true, `shared_targets` false, a non-null backend-host string, `metadata` `{}`). `volume_type` is persisted on create (the column already exists) and defaulted to `__DEFAULT__` (upstream Cinder's canonical name) when unset, with Lightstack's `GET /v3/types/default` endpoint aligned to report `__DEFAULT__` too so the two agree. Because the volume responses are currently hand-built per handler with no shared serializer, a single shared serialization path is preferred to keep show/list/create consistent (implementation detail for planning).
- **F5 fix is host-general.** The fix may reseed the aliased endpoint with the configured host and/or make runtime host substitution rewrite any seeded host (not only the `http://lightstack:` prefix). A host-general runtime fix corrects the `volume` alias (`…015`) and prevents this class of bug from recurring; the S3/RGW endpoint (`…016`) is in scope but reached differently — `BuildServiceCatalog` replaces its URL with the configured external RGW address before substitution matters, so it is neither reseeded nor host-substituted (see FR-006a); the report's real pass showed the primary `volumev3` endpoint already resolves cleanly, so this specifically repairs the `volume` alias that upstream Horizon also resolves (plus the object-store endpoint, per the F5 clarification).
- **F4 external networks are not modeled.** Lightstack has no external/provider-network concept and no backing column for it, so the minimal conformant fix treats every network as non-external: `router:external=true` returns an empty set and responses report `router:external: false`. Honoring `project_id`/`tenant_id` narrows within the caller's existing authorization scope and never widens it. A full external-network model is out of scope.
- **F6 follows upstream Glance ownership semantics.** Confirmed against the Glance policy docs: the default `delete_image` policy is `not_protected_and_is_owner`, so deletion is authorized by the image's **owner-or-admin**; *visibility* (public) controls read access, not deletion, and a public image retains its `owner`. The fix therefore (a) records the owner on create for all visibilities — stopping the ownerless-public-image storage that is the root cause — and (b) authorizes delete as owner-or-admin (admin via the token role), returning 403 for others and 404 for missing, with a rows-affected check. Lightstack does not model `protected` images, so protected-blocking is out of scope.
- **Reference material**: `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` (findings F4/F5/F6/F9) and the SPEC-018 runbooks (`docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md`, `HOST_TESTING_REAL_MODE.md`).
- **Verify-then-fix lineage**: this branch is based on feature/482 (SPEC-018), which produced the report; the four findings were spun off as #532 exactly as SPEC-016 spun off #524.

## Out of Scope

- **F1** (noVNC console) — ticket #534.
- **F2, F3, F7, F8** (the create/write-UI cluster: limits widget, missing Create-Network button, launch-wizard network step, dead create modals) — ticket #533.
- A full external/provider-network model in Neutron (only the `router:external` filter/attribute behavior needed to de-duplicate the list is in scope).
- Modeling `protected` images (upstream blocks deleting a protected image); Lightstack does not model `protected`, so every owned image is deletable by its owner or an admin.
- Redesigning the Cinder volume data model beyond persisting the `volume_type` needed for read-back.
- Any automated browser test suite (verification remains manual/runbook-driven, per SPEC-018).

## Dependencies

- Based on **feature/482 / SPEC-018**, which produced `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`.
- **F4 unblocks F7** in the follow-on create/write-UI ticket **#533** (the launch-instance wizard consumes the deduplicated, filter-honoring network list).
- Part of the **#531** umbrella (step 1 of 3); siblings **#533** (create/write UI) and **#534** (console).
