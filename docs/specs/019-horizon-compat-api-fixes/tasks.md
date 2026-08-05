---
description: "Task list for Horizon compat API/response fixes (F4, F5, F6, F9)"
---

# Tasks: Horizon compatibility — Cinder/Neutron/Glance API & response fixes (F4, F5, F6, F9)

**Input**: Design documents from `docs/specs/019-horizon-compat-api-fixes/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/

**Tests**: Test tasks ARE included and come FIRST in each story — required by Constitution Article III
(Test-First, NON-NEGOTIABLE), spec FR-017 (automated regression guard), and FR-019 (dual-mode verification).
Write each test, confirm it FAILS (RED), then implement to GREEN.

**Organization**: Tasks grouped by user story. The four findings touch four different services/files, so the
stories are fully independent and can be implemented in parallel.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks)
- **[Story]**: US1=F9, US2=F5, US3=F4, US4=F6 (maps to spec.md user stories)

## Path Conventions

Single Go project (existing repo). Sources under `internal/`, migrations under `migrations/`, tests alongside
sources as `*_test.go`, integration/runbooks under `test/` and the SPEC-018 spec dir.

---

## Phase 1: Setup

**Purpose**: Establish a clean baseline before changes (so each story's RED test is meaningful).

- [X] T001 Confirm baseline is green on `feature/532`: `make build` and `make test` succeed before any change (records the pre-fix reference; the new story tests are expected to FAIL against this baseline).

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core prerequisites shared by all user stories.

**None.** The four fixes are independent (different services and files: `internal/cinder/volumes.go`,
`internal/keystone/auth.go` + `migrations/`, `internal/neutron/network.go`, `internal/glance/images.go`) with
no shared new infrastructure. Proceed directly to the user stories — they may be done in any order or in
parallel.

**Checkpoint**: Baseline green ⇒ user-story work can begin (in parallel if staffed).

---

## Phase 3: User Story 1 - Volumes panel works when a volume exists (Priority: P1) 🎯 MVP — F9

**Goal**: `volume show` / `volume list --long` return the full upstream Cinder field set (non-null), so the
Horizon Volumes panel renders (no HTTP 500) once a volume exists.

**Independent Test**: Create a volume; `openstack volume show <id>` reports non-null `volume_type`
(`__DEFAULT__` when unset), `availability_zone`, `encrypted`, `user_id`, tenant/host ownership, `multiattach`,
`consumes_quota`, `shared_targets`, `metadata`; the Volumes panel loads. (contracts/cinder-volume-response.md)

### Tests for User Story 1 ⚠️ (write first, confirm RED)

- [X] T002 [P] [US1] Failing unit tests in `internal/cinder/volumes_test.go`: assert `GetVolume` and `ListVolumesDetail` responses contain every field in contracts/cinder-volume-response.md non-null; a volume created with `--type foo` reads back `volume_type=="foo"`; created with no type reads back `"__DEFAULT__"`; `GetDefaultVolumeType` returns `{"id":"__DEFAULT__","name":"__DEFAULT__"}`.

### Implementation for User Story 1

- [X] T003 [US1] Add a shared `serializeVolume(...) gin.H` helper in `internal/cinder/volumes.go` emitting the full field set with non-null defaults per contracts/cinder-volume-response.md (`availability_zone`=`nova`, `encrypted`/`multiattach`/`shared_targets`=false, `consumes_quota`=true, `os-vol-host-attr:host`=configured host, `os-vol-tenant-attr:tenant_id`=project_id, `metadata`={}).
- [X] T004 [US1] Wire `GetVolume` and `ListVolumesDetail` (`internal/cinder/volumes.go`) to `SELECT` `user_id` and `volume_type` and return `serializeVolume(...)` (replacing the inline `gin.H` maps).
- [X] T005 [US1] In `CreateVolume` (`internal/cinder/volumes.go`) bind `volume_type` into the INSERT (default `__DEFAULT__` when empty) and return `serializeVolume(...)`; change `GetDefaultVolumeType` to return `__DEFAULT__` (id + name).
- [X] T006 [US1] Run `go test ./internal/cinder/...` — T002 passes (GREEN); existing Cinder tests still pass.

**Checkpoint**: F9 fixed and independently testable — Volumes panel renders with a volume present.

---

## Phase 4: User Story 2 - Block-storage service is reachable from the dashboard (Priority: P1) — F5

**Goal**: No advertised catalog endpoint (the `volume` alias `…015`, `volumev3` `…013`, and s3 `…016`) points
at `localhost` in a non-`localhost` deployment.

**Independent Test**: With a non-`localhost` `LIGHTSTACK_ENDPOINT_HOST`, `openstack catalog show volume`/`volumev3`/`s3`
show only the configured host (no `localhost`); a volume op through the resolved endpoint succeeds; with the
host unset, endpoints remain `localhost` (dev regression). (contracts/keystone-catalog-endpoints.md)

### Tests for User Story 2 ⚠️ (write first, confirm RED)

- [X] T007 [P] [US2] Failing test in `internal/keystone/auth_test.go`: with `LIGHTSTACK_ENDPOINT_HOST` set to a non-`localhost` value, catalog resolution for the `volume`/`volumev3`/`s3` services yields no URL containing `localhost`; with it unset, URLs stay `localhost`.

### Implementation for User Story 2

- [X] T008 [P] [US2] Create `migrations/064_fix_cinder_volume_alias_localhost.up.sql` reseeding the `volume` alias endpoint (service `…015`) from `http://localhost:8776/v3` to `http://lightstack:8776/v3`. **Implementation note:** s3 (`…016`) is intentionally NOT reseeded — `BuildServiceCatalog` already overrides it from `cfg.Glance.S3Endpoint` (or omits it), so its seeded placeholder is never served; reseeding it to `lightstack` would be wrong (the real value is the external RGW URL).
- [X] T009 [P] [US2] Create `migrations/064_fix_cinder_volume_alias_localhost.down.sql` restoring the `…015` localhost URL (reversible; verified up→down→up on the test DB).
- [X] T010 [P] [US2] Extend `substituteURLTemplates` in `internal/keystone/auth.go` to also rewrite `http://localhost:` / `https://localhost:` → the configured host (defensive; no-op when the host is unset/localhost).
- [X] T011 [US2] Run `make migrate` then `go test ./internal/keystone/...` — T007 passes (GREEN); migration up/down verified reversible.

**Checkpoint**: F5 fixed — the `volume` alias and s3 endpoints resolve to the configured host.

---

## Phase 5: User Story 3 - Each network is listed once (Priority: P2) — F4

**Goal**: `GET /v2.0/networks` honors the `shared` / `router:external` / `project_id`/`tenant_id` filters so
Horizon's Project panel shows each network exactly once; every network carries `router:external`.

**Independent Test**: With one project-owned non-shared network, `?shared=true` excludes it, `?shared=false`
includes it, `?router:external=true` is empty, each network has `router:external=false`; the merged Horizon
sub-queries yield one row; unfiltered list unchanged. (contracts/neutron-network-list-filters.md)

### Tests for User Story 3 ⚠️ (write first, confirm RED)

- [X] T012 [P] [US3] Failing tests in `internal/neutron/network_test.go`: `ListNetworks` honors `shared` (true→only shared, false→exclude shared), `router:external=true`→empty, `tenant_id`/`project_id` narrows within the caller's scope (no cross-project widening); every network response includes `"router:external": false`; the three Horizon sub-queries de-duplicate to one row; unfiltered list returns each network once.

### Implementation for User Story 3

- [X] T013 [US3] In `ListNetworks` (`internal/neutron/network.go`) read and apply the `shared`, `router:external` (empty result when true — no external model), and `project_id`/`tenant_id` (narrowing, intersected with the caller's authorized scope) query filters, replacing the unconditional `WHERE (project_id=$1 OR shared=true)`.
- [X] T014 [US3] Emit `"router:external": false` in the network list, show (`GetNetwork`), and create (`CreateNetwork`) responses in `internal/neutron/network.go`.
- [X] T015 [US3] Run `go test ./internal/neutron/...` — T012 passes (GREEN); existing Neutron tests still pass.

**Checkpoint**: F4 fixed — no duplicate network rows; filters honored (unblocks #533 F7).

---

## Phase 6: User Story 4 - Deleting an image removes it (Priority: P3) — F6

**Goal**: Images retain their owner across all visibilities; delete is authorized as owner-or-admin (upstream
Glance) with correct 403/404 and rows-affected, so a deleted image actually disappears from `image list`.

**Independent Test**: Owner/admin create→delete→`image list` no longer shows it (stub and real); non-owner
non-admin delete of a public image → 403; delete of a nonexistent id → 404; in-use image → 409.
(contracts/glance-image-delete.md)

### Tests for User Story 4 ⚠️ (write first, confirm RED)

- [X] T016 [P] [US4] Failing tests in `internal/glance/images_test.go`: a public image created by a project records that project as owner; delete by the owner or an admin removes it (rows-affected>0, gone from list); delete by a non-owner non-admin of a public image → 403; delete of a missing id → 404; in-use image → 409.

### Implementation for User Story 4

- [X] T017 [US4] In `CreateImage` (`internal/glance/images.go`) bind `project_id` (owner) for images of every visibility — remove the `Valid: visibility == "private"` gating so public images retain their owner.
- [X] T018 [US4] Rewrite `DeleteImage` authz in `internal/glance/images.go`: `SELECT` owner+visibility by id (no row→404); not-visible (private, not owner, not admin)→404; visible-but-unauthorized (not owner, not admin)→403; keep the in-use guard (409); `DELETE FROM images WHERE id=$1` and check rows-affected (0→404). Add an `isAdmin(c)` helper using `c.GetStringSlice("roles")` containing `admin`.
- [X] T019 [US4] Run `go test ./internal/glance/...` — T016 passes (GREEN); existing Glance tests still pass.

**Checkpoint**: F6 fixed — image delete removes the image with correct authorization.

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: Gates, dual-mode host verification, and docs across all four fixes.

- [X] T020 [P] Run `make fmt`, `make lint`, and `go test ./... -race` — all clean; no regression in existing unit/integration suites (FR-018).
- [X] T021 Stub-mode host verification of F4/F5/F6/F9 via `deployments/docker-compose-horizon.yml`, following `docs/specs/018-horizon-dashboard-verification/HOST_TESTING.md` (Volumes panel, catalog hosts, network list once, image delete). *(Linux host; user-run.)* **Done 2026-07-24 (host `ora`) — all four PASS:** F9 `volume show` reads back `volume_type=__DEFAULT__` and `/v3/volumes/detail` is non-null throughout (`nova`, `false`, `false`, `true`, `false`, `user_id`, host, tenant, `metadata={}`); F5 no catalog URL contains `localhost`; F4 the network lists once, `router:external=true` is empty and the `shared` filter is honored; F6 delete removes the image, missing id → 404.
- [X] T022 Real-mode host verification of F4/F5/F6/F9 on k3s + `charts/lightstack` real mode + `deployments/horizon-chart/` with Ceph RGW/S3 via `deployments/rook/setup-rook-rgw.sh`, following `docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md`. *(Root k3s + minikube/Rook; user-run.)* **Done 2026-07-24 (host `ora`) — all four PASS** against node-local Cinder and RGW-backed Glance. F6 nuance: API/CLI delete removes the image, but Horizon's row **Delete** button still fails — that is the F8 write-UI cluster (#533), not a #532 regression; recorded in the report's Follow-ups.
- [X] T023 [P] Update `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` marking F4/F5/F6/F9 fixed, with evidence from T021/T022. **Done 2026-07-24:** dated update banner, ✅ FIXED tag on each of the four finding headers, Follow-ups rewritten (F8/#533 and F1/#534 stay open); `OPENSTACK_COMPATIBILITY_REPORT.md` got the superseded note. The Terraform report and `docs/blueprint.md` need no change — they reference only F1/console, still open.
- [X] T024 Branch review of the full diff (correctness + idiom) before opening the PR. **Done 2026-07-24, six findings applied:** DB-backed handler tests (`*_db_test.go` in cinder/neutron/glance), pure `buildNetworkListFilter` and `imageInUse` extracted, shared `common.HasRole` (keystone's `hasRole` delegates to it), `DeleteImage` documented and its orphaned comment removed, `volume_type` double-default annotated as intentional. Gates green: `gofmt`, `golangci-lint ./...` 0 issues, full suite + `-race`.
- [X] T025 PR #78 review follow-up (not one of F4/F5/F6/F9): scope the metadata service's SSH keys to the instance's persisted `key_name` at both call sites (`internal/metadata/service.go` `GetMetaDataJSON` + `publicKeysForInstance`), matching upstream Nova; DB-backed tests in `internal/metadata/service_db_test.go` (RED against the old user-wide query, GREEN after); host check added as step 2.5 in both runbooks.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (T001)**: no dependencies.
- **Foundational**: none (empty).
- **User Stories (US1–US4)**: each depends only on Setup. They are mutually independent (different files) and
  may run in parallel or in any order.
- **Polish (T020–T024)**: depends on all desired user stories being complete.

### Within Each User Story

- The test task (T002 / T007 / T012 / T016) is written and FAILS before implementation (Article III).
- US1: T003 (serializer) → T004, T005 (same file, sequential) → T006 (verify).
- US2: T008/T009/T010 are different files ([P]); T011 verifies after all.
- US3: T013 → T014 (same file, sequential) → T015 (verify).
- US4: T017 → T018 (same file, sequential) → T019 (verify).

### Parallel Opportunities

- All four stories can proceed at once (four developers / agents, four services).
- The four RED test tasks (T002, T007, T012, T016) are all [P] — different files.
- Within US2, T008/T009 (migration up/down) and T010 (auth.go) are [P].
- In Polish, T020 and T023 are [P]; T021/T022 (host passes) are sequential and user-gated.

---

## Parallel Example: all four stories at once

```bash
# After T001 (baseline green), start the four RED tests in parallel:
Task: "T002 volume response unit tests in internal/cinder/volumes_test.go"
Task: "T007 catalog-host test in internal/keystone/auth_test.go"
Task: "T012 network-filter tests in internal/neutron/network_test.go"
Task: "T016 image create/delete authz tests in internal/glance/images_test.go"
# then implement each story to GREEN independently.
```

---

## Implementation Strategy

### MVP (highest-value single fix)

US1 (F9) is the suggested MVP — a hard 500 crash on a read path. Complete T001 → T002–T006 → validate, and
the Volumes panel is usable. **But #532's Definition of Done is all four findings** (ticket acceptance:
"each finding fixed or closed with a documented reason").

### Incremental Delivery

Each story is an independent, shippable increment (F9 → F5 → F4 → F6, or any order). Land one, verify, repeat.
F4 (US3) is worth prioritizing early despite P2 because it unblocks #533 (F7). Do the dual-mode host
verification (T021/T022) once, after the code fixes land, since it exercises all four together.

### Definition of Done

All four unit-test tasks GREEN; `make fmt`/`lint`/`test -race` clean; both host passes (stub + real) observe
F4/F5/F6/F9 fixed; compatibility report updated; branch reviewed.

---

## Notes

- [P] = different files, no dependencies.
- [Story] label maps each task to its finding for traceability.
- Only one migration in the whole feature: `064` (F5). F9 reuses `volumes.volume_type` (mig `016`); F4/F6 are code-only.
- T021/T022 require a Linux host + cluster and are typically user-run (real mode needs real root; cf. SPEC-009/018 notes).
- Commit after each task or logical group; keep each finding's commit self-contained.
