# Tasks: Integration Contract (Values Schema + Status Conventions)

**Input**: Design documents from `docs/specs/012-integration-contract/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: INCLUDED — the project constitution (Article III) makes test-first NON-NEGOTIABLE, and the
plan calls for schema-rejection tests RED before schema completion. Here "tests" are chart-level
(helm-unittest + `helm template`/`lint` assertions in `ci/run-tests.sh`), not Go tests.

**Organization**: By user story (US1 input/schema P1, US2 status+endpoint conventions P2, US3 published
contract P3). This is a documentation + schema + tests feature on `charts/lightstack/` — no Go/runtime/migration changes.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: US1 / US2 / US3 (omitted for Setup / Foundational / Polish)
- Exact file paths included.

## Path Conventions

All work is under `charts/lightstack/` (chart) and `docs/specs/012-integration-contract/` (design). No `src/`.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Establish the green baseline and test scaffolding before any change.

- [X] T001 [P] Confirm the helm-unittest plugin is installed and run `charts/lightstack/ci/run-tests.sh` on the unchanged chart; record the GREEN baseline (lint + template + unittest pass today).
- [X] T002 [P] Create the directory `charts/lightstack/ci/invalid-values/` to hold schema-rejection fixtures (helmignored — `ci/` is excluded from the package).

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The authoritative input key set both the schema (US1) and the drift gate (US3) depend on.

**⚠️ CRITICAL**: Complete before US1/US3 schema work.

- [X] T003 Reconcile the authoritative top-level input key inventory in `charts/lightstack/values.yaml` against `docs/specs/012-integration-contract/data-model.md` §1 (every group + leaf keys, and which are free-form maps); this list is the source of truth for both `values.schema.json` coverage and the drift gate.

**Checkpoint**: Key inventory agreed → schema + tests can proceed.

---

## Phase 3: User Story 1 - Reject invalid configuration before deploy (Priority: P1) 🎯 MVP

**Goal**: `values.schema.json` documents *and* structurally validates every input; invalid config is
rejected before any workload is created.

**Independent Test**: `helm template`/`install` fails on each invalid fixture (unknown key, bad enum,
bad storageMode, out-of-range, missing image, malformed DB) with a clear message; a complete valid
value set renders. (`contracts/input-contract.md` matrix.)

### Tests for User Story 1 (write FIRST — confirm RED) ⚠️

- [X] T004 [P] [US1] Author invalid-values fixtures in `charts/lightstack/ci/invalid-values/`: `unknown-key.yaml`, `bad-enum.yaml`, `bad-storagemode.yaml`, `out-of-range.yaml`, `missing-image.yaml`, `malformed-db.yaml`, plus a complete `valid-complete.yaml` (per `contracts/input-contract.md` §"Rejection test matrix").
- [X] T005 [US1] Extend `charts/lightstack/ci/run-tests.sh` with a rejection block: assert `helm template r charts/lightstack -f ci/invalid-values/<fixture>` FAILS for each invalid fixture and RENDERS for `valid-complete.yaml`; run it and confirm RED (today's schema doesn't yet reject unknown-key / bad-storagemode / malformed-db).
- [X] T006 [P] [US1] Add `charts/lightstack/tests/schema_test.yaml` (helm-unittest) asserting representative structural rejections (`failedTemplate` on unknown top-level key and bad `glance.storageMode`); confirm RED. If helm-unittest does not surface schema-validation errors, delete this file and rely on the T005 bash gate (note the decision in the test file's comment).

### Implementation for User Story 1 (make GREEN)

- [X] T007 [US1] In `charts/lightstack/values.schema.json`: add a `description` to every property and set `additionalProperties: false` at the top level.
- [X] T008 [US1] In `charts/lightstack/values.schema.json`: model the currently-bare/under-typed objects with `additionalProperties: false` (leaving free-form maps open) — `database` (url/existingSecret/existingSecretKey), `secrets` (jwtSecret/adminUser/adminPassword/existingSecret), `ingress`, `multiNode` (incl. `vxlanPort` port range + `minProductVersion` description), `realMode` (all fields), `cache` (redisUrl), `podSecurity`, `service` (type enum + open `annotations`), `endpointHost`, `logging`.
- [X] T009 [US1] In `charts/lightstack/values.schema.json`: add `storageMode` enums — `cinder` = stub/local/rbd/local,rbd; `glance` = stub/local/rbd/s3/local,rbd/local,s3/rbd,s3.
- [X] T010 [US1] In `charts/lightstack/values.schema.json`: document `image.tag` as the version input (typed string, no enum, defaults to `appVersion`) and `multiNode.minProductVersion` as the separate advisory-only floor (#52).
- [X] T011 [US1] Verify the render-time conditional guards still hold under the now-strict schema (jwtSecret/adminPassword/database.url required-unless-`existingSecret`; `teardown.kubectlImage` under `dataRetentionPolicy: delete`; multi-node+real rejects `local` glance/cinder) against `charts/lightstack/templates/_helpers.tpl`; extend a guard only if T012 reveals a gap. Reference existing `charts/lightstack/tests/gates_test.yaml`.
- [X] T012 [US1] Run `charts/lightstack/ci/run-tests.sh`: confirm all T005/T006 rejection cases are now GREEN and that `ci/test-values.yaml` still renders across stub, real-mode, and TLS permutations (no real key rejected by `additionalProperties`).

**Checkpoint**: US1 complete — the input contract validates and rejects, fail-fast, before execution.

---

## Phase 4: User Story 2 - Derive lifecycle state and endpoints (Priority: P2)

**Goal**: The status-derivation recipe and identity-anchored endpoint resolution are documented as
deterministic conventions.

**Independent Test**: An integrator, using only the documented recipe, classifies a release as
Provisioning/Ready/Failed from `helm status` + pod readiness (both topologies) and resolves endpoints
via the Keystone anchor + service catalog. (`contracts/status-contract.md`, `quickstart.md` §3–4.)

- [X] T013 [P] [US2] Create `charts/lightstack/CONTRACT.md` (with SPDX header) and write the **Status derivation** section: signals (`helm status -o json` `.info.status`, Deployment vs DaemonSet readiness fields, terminal pod states), the state-mapping table, the `--wait --timeout` readiness gate, and the Failed triggers. Source: `contracts/status-contract.md` + `data-model.md` §2.
- [X] T014 [US2] Add the **Endpoint resolution** section to `charts/lightstack/CONTRACT.md`: identity-anchored Keystone `/v3` (in-cluster Service + advertised host/scheme), service-catalog discovery for the other five, the `endpointHost` coupling, metadata exclusion, and the Flamingo API versions. Source: `contracts/status-contract.md` + `data-model.md` §3.
- [X] T015 [P] [US2] Update `charts/lightstack/templates/NOTES.txt`: mark the six in-cluster ports as non-authoritative debugging info, point the reader at the Keystone endpoint + `openstack catalog list`, and note that external consumers must set `endpointHost`.
- [X] T016 [US2] Add/extend a helm-unittest assertion in `charts/lightstack/tests/notes_test.yaml` that NOTES renders the Keystone endpoint and the non-authoritative caveat (RED → GREEN after T015).

**Checkpoint**: US2 complete — status + endpoint conventions are written and follow-able.

---

## Phase 5: User Story 3 - One published, versioned contract document (Priority: P3)

**Goal**: A single `CONTRACT.md` ties inputs → outputs, is linked from the README, ships in the
package, is version-matched, and is drift-guarded.

**Independent Test**: The packaged chart contains `CONTRACT.md`, the README links it, and its
inputs/status/endpoints match the actual schema + NOTES (drift gate green).

> Builds on US2 (which creates `CONTRACT.md`). Run US3 after US2.

- [X] T017 [US3] Add the **Inputs** section to `charts/lightstack/CONTRACT.md`: summarize the input groups (referencing `values.schema.json`), the `additionalProperties: false` strictness + open free-form maps, version = `image.tag`, and credential presence-only policy. Source: `contracts/input-contract.md`.
- [X] T018 [US3] Add the **Versioning & compatibility** section to `charts/lightstack/CONTRACT.md`: the contract rides `Chart.version` (SemVer) — MAJOR may break, MINOR/PATCH backward-compatible; `appVersion`/`image.tag` is the independent product-version axis.
- [X] T019 [P] [US3] Link `CONTRACT.md` from `charts/lightstack/README.md` under a clear "Integration contract (sci-in-a-box)" pointer.
- [X] T020 [US3] Add a packaging assertion to `charts/lightstack/ci/run-tests.sh`: `helm package` output (or `helm template` manifest set) includes `CONTRACT.md` — i.e. it is not excluded by `.helmignore` (`tar tzf <pkg>.tgz | grep CONTRACT.md`).
- [X] T021 [US3] Add the schema↔values **drift/coverage gate** to `charts/lightstack/ci/run-tests.sh`: fail if the set of top-level keys in `values.yaml` ≠ the set of properties in `values.schema.json` (catches a real key rejected by `additionalProperties:false` and any undocumented input). Source: `contracts/input-contract.md` coverage invariant.

**Checkpoint**: US3 complete — the contract is published, linked, shipping, and drift-guarded.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [X] T022 [P] Run the full `charts/lightstack/ci/run-tests.sh` (lint + template permutations + unittest + rejection fixtures + packaging + drift gate) → all GREEN.
- [X] T023 [P] Walk `docs/specs/012-integration-contract/quickstart.md` end-to-end (optionally on kind/k3s via `test/helm_singlenode_stub_test.sh`) to validate the US1→US2→US3 acceptance scenarios (SC-001..006).
- [X] T024 Tick the three acceptance-criteria boxes on cloud-storage#474 and add a "done" comment summarizing the schema + status conventions + published `CONTRACT.md`.
- [ ] T025 Run `/review-branch` and address findings before pushing.

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (P1)** → no deps.
- **Foundational (P2)** → after Setup; blocks US1/US3 schema/drift work.
- **US1 (P3 phase)** → after Foundational. Self-contained (schema + its tests).
- **US2 (P4 phase)** → after Foundational. Independent of US1 (docs vs schema).
- **US3 (P5 phase)** → after US2 (extends the `CONTRACT.md` US2 creates); independent of US1 except the Inputs section references the US1 schema.
- **Polish (P6)** → after the desired stories.

### Within each story

- US1: T004/T006 (tests, RED) → T005 (wire+RED) → T007→T008→T009→T010 (schema, sequential — same file) → T011 → T012 (GREEN).
- US2: T013 → T014 (same file, sequential); T015 → T016 (NOTES then its test) can run alongside T013/T014.
- US3: T017 → T018 (same file, sequential, after US2's CONTRACT.md); T019 [P]; T020 → T021 (same file `run-tests.sh`, sequential — and after T005 which also edits it).

### Parallel opportunities

- T001, T002 (Setup) in parallel.
- T004 and T006 (US1 test fixtures vs unittest spec — different files) in parallel.
- T015 (NOTES) parallel with T013/T014 (CONTRACT.md) — different files.
- T019 (README link) parallel with T017/T018.
- ⚠️ Sequential (same file): T007–T010 (`values.schema.json`); T013/T014/T017/T018 (`CONTRACT.md`); T005/T020/T021 (`ci/run-tests.sh`).

---

## Parallel Example: User Story 1

```bash
# RED first — different files, run together:
Task: "Author invalid-values fixtures in charts/lightstack/ci/invalid-values/ (T004)"
Task: "Add charts/lightstack/tests/schema_test.yaml helm-unittest rejection cases (T006)"
# Then schema edits are sequential (same file values.schema.json): T007 → T008 → T009 → T010
```

---

## Implementation Strategy

### MVP (User Story 1 only)

1. Setup (T001–T002) → Foundational (T003).
2. US1 (T004–T012): rejection tests RED → complete `values.schema.json` → GREEN.
3. **STOP & VALIDATE**: `helm install`/`upgrade` now rejects invalid config before execution (AC1). This alone is a shippable safety win.

### Incremental delivery

1. MVP = US1 (input contract validates).
2. + US2 (status + endpoint conventions documented) → an orchestrator can derive state + endpoints.
3. + US3 (single published, versioned, drift-guarded `CONTRACT.md`) → the contract is discoverable and shipped.
4. Polish: full gate green, quickstart walkthrough, close #474.

---

## Notes

- [P] = different files, no incomplete-task dependency.
- Contract-only feature: NO Go code, NO chart dependencies, NO DB migration.
- Verify RED before GREEN (T005/T006 before T007–T012).
- `CONTRACT.md` ships because `.helmignore` excludes only `/tests/` + `ci/` (not `*.md`) — add an SPDX header (T013).
- Commit per task or logical group; conventional-commit `feat(helm)/docs(helm)/test(helm)` scopes.
