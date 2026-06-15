# Tasks: Teardown via Helm Pre-Delete Hooks (Clean, No State Leakage)

**Input**: Design documents from `docs/specs/009-helm-teardown/`
**Prerequisites**: plan.md, spec.md, research.md (R1–R9), data-model.md, contracts/ (cli-cleanup, teardown-hooks, values), quickstart.md
**Tracking issue**: [#53](https://github.com/cobaltcore-dev/o3k/issues/53)

**Tests**: INCLUDED and written **RED-first** — mandated by Constitution Article III (Test-First, NON-NEGOTIABLE) and research R8.

**Delivery scope**: the validated done-line is **single-node `dataRetentionPolicy: delete`** (US1+US2+US3, single-node). Per research R1 the o3k binary stays **Kubernetes-API-free** — a kubectl-image hook Job does scale/PVC, a privileged `o3k cleanup` Job does host/storage/DB purge. **Multi-node DaemonSet host-purge is [DEFERRED #52]** (FR-014, SC-007) — not authored or validated here; it needs functioning cross-node real mode.

## Format: `[ID] [P?] [Story] Description with file path`

- **[P]**: parallelizable (different files, no dependency on an incomplete task)
- **[Story]**: US1/US2/US3 (Setup/Foundational/Polish carry no story label)
- **[DEFERRED #52]**: ship/validate when issue #52 lands

---

## Phase 1: Setup (shared config surface)

**Purpose**: the teardown configuration surface every hook keys off. Backward-compatible — defaults reproduce SPEC-008 exactly.

- [X] T001 Add the `teardown.*` block (`kubectlImage`, `timeoutSeconds: 600`, `deletePVCs: true`) and expand the `dataRetentionPolicy` doc in `charts/o3k/values.yaml`; add their schema to `charts/o3k/values.schema.json` (FR-002/016/018; contracts/values.md)
- [X] T002 [P] Add teardown helpers to `charts/o3k/templates/_helpers.tpl`: name/label helpers (`o3k.fullname`-prefixed `-teardown*`), a kubectl-image helper, and the `dataRetentionPolicy == "delete" && teardown.kubectlImage == ""` → `fail` gate (FR-018; R1)

---

## Phase 2: Foundational (blocking prerequisites)

**Purpose**: the prerequisite product fix and the cleanup entrypoint that all purge work plugs into.

**⚠️ CRITICAL**: no user-story work begins until this phase is complete.

- [X] T003 [P] RED test: `pkg/storage/ceph_test.go` — assert the Cinder local-volume path resolves under the configured base (`<hostStoragePath>/volumes`), mirroring Glance's `<hostStoragePath>/images`; must FAIL against current code (FR-020; research R6)
- [X] T004 Fix `pkg/storage/ceph.go` (`NewCephClient`): local volumes under `<hostStoragePath>/volumes` (configurable base, `~/.o3k/volumes` fallback) — makes T003 pass (FR-020)
- [ ] T005 Add the `cleanup` subcommand to `cmd/o3k/main.go` (`os.Args[1] == "cleanup"` → run teardown and exit, before the server path) and scaffold `internal/teardown/teardown.go` (config load, DB-connect-or-fallback, exit-code contract 0/1/2, `--dry-run` flag) (FR-010; contracts/cli-cleanup.md)

**Checkpoint**: Cinder data lands on the mount; `o3k cleanup` is invokable. User stories can begin.

---

## Phase 3: User Story 1 - One-command clean teardown, nothing left behind (Priority: P1) 🎯 MVP

**Goal**: `helm uninstall` with `dataRetentionPolicy: delete` runs a blocking pre-delete hook that purges all release state (compute, network, node-local storage, PVCs, DB schema) before the release is removed.

**Independent Test**: single-node stub install → create resources → `helm uninstall` (delete) → uninstall **blocks** on the hook, then **zero** release-owned artifacts remain (no workloads/PVCs/domains/netns/local-storage; DB schema empty). (SC-001, SC-003; issue #53 criteria 1–2)

### Tests for User Story 1 (write FIRST — confirm RED) ⚠️

- [ ] T006 [P] [US1] `internal/teardown/teardown_test.go` (stub mode): enumeration maps DB records → the correct delete calls; dependency-safe order (compute → network → storage → DB-reset-last); idempotency (not-found = success); DB-unreachable host-prefix fallback (research R3/R9; data-model §2)
- [ ] T007 [P] [US1] `charts/o3k/tests/teardown_test.yaml` (helm-unittest): RBAC + both Jobs render **iff** `dataRetentionPolicy: delete` and are **absent** under `retain`; Role rules == the least-privilege set (R7); purge Job has `command:[o3k,cleanup]`, `activeDeadlineSeconds`, `backoffLimit:0`, real-mode host mounts; `kubectlImage:""` under `delete` → render fails (contracts/teardown-hooks.md)
- [ ] T008 [P] [US1] `test/helm_teardown_test.sh` (scaffold + US1 assertions): single-node stub install → create resources → `helm uninstall` (delete) → assert uninstall **blocks** on the hook, then zero residue (no workloads/PVCs/domains/netns/local-storage; DB schema empty) (SC-001/003)

### Implementation for User Story 1

- [ ] T009 [US1] `internal/teardown`: enumerate release state from the DB (`instances.libvirt_domain_id` → domains; `networks`/`routers` → `light-ns-`/`qrouter-` netns; `ports` → bridges/TAP; `volumes`/`images` → local files) + DB-unreachable host-prefix fallback (R3; data-model §2)
- [ ] T010 [US1] `internal/teardown`: compute purge — `VMManager.DeleteVM(uuid)` (destroy + undefine) per enumerated domain (FR-003)
- [ ] T011 [US1] `internal/teardown`: network purge — `DeleteNamespace`/`DeleteRouterNamespace` + bridges/TAP/iptables/VXLAN per enumerated id (FR-004)
- [ ] T012 [US1] `internal/teardown`: storage purge — delete node-local volume/image files under `hostStoragePath`; bounded orphan sweep within that tree (single-release per FR-003b) (FR-005/006; depends on T004)
- [ ] T013 [US1] `internal/teardown`: DB reset via `database.MigrateReset()` **last**; skip if DB unreachable (FR-013; C8/C9)
- [ ] T014 [US1] `internal/teardown`: fail-closed roll-up over required classes + idempotency + structured-JSON audit logging + exit codes — makes T006 pass (FR-008/012/016; R9)
- [ ] T015 [P] [US1] `charts/o3k/templates/teardown-rbac.yaml`: ServiceAccount + namespaced Role + RoleBinding (pre-delete, hook-weight `-20`, gated on `delete`); verbs = deployments get/list/patch, pods get/list, pvc get/list/delete (FR-011; R7)
- [ ] T016 [P] [US1] `charts/o3k/templates/teardown-quiesce-job.yaml`: kubectl Job (hook-weight `-10`) — scale Deployment→0, `wait --for=delete pod`, then delete release PVCs if `deletePVCs`; `backoffLimit:0` (FR-019/005; quiesce-first per C6)
- [ ] T017 [US1] `charts/o3k/templates/teardown-purge-job.yaml`: privileged `o3k cleanup` Job (hook-weight `0`) with real-mode host mounts + config Secret; `activeDeadlineSeconds: {{ .Values.teardown.timeoutSeconds }}`, `backoffLimit:0`, `restartPolicy:Never` — the blocking, fail-closed gate; makes T007 pass (FR-001/003/016)
- [ ] T018 [US1] `charts/o3k/templates/NOTES.txt`: add the teardown note + the `helm uninstall --no-hooks` escape hatch (C5/FR-016)

**Checkpoint**: single-node `delete` teardown works end-to-end and blocks until clean. MVP deliverable.

---

## Phase 4: User Story 2 - Immediate clean redeploy from a pristine baseline (Priority: P2)

**Goal**: an install issued immediately after a `delete` teardown (same release name/node/DB) comes up healthy with no naming collisions or residual-state errors.

**Independent Test**: run a `delete` teardown (US1), then immediately reinstall → identity check passes within the first-install budget; `openstack server list` etc. show no stale resources. (SC-002; issue #53 criterion 3)

> Implementation is delivered by US1's DB schema reset (T013) + the chart's release-scoped naming; this phase **validates** the pristine-baseline contract.

### Tests for User Story 2 (write FIRST — confirm RED) ⚠️

- [ ] T019 [US2] Extend `test/helm_teardown_test.sh`: after the US1 `delete` teardown, **immediately reinstall** the same release on the same node/DB → `helm test` identity check passes, no naming-collision error, no stale instances/networks/volumes via the OpenStack CLI (SC-002/009; depends on T008)

**Checkpoint**: teardown → immediate redeploy is pristine.

---

## Phase 5: User Story 3 - Explicit, safe retain-vs-delete choice (Priority: P3)

**Goal**: destruction is opt-in only; default `retain` reproduces SPEC-008 behavior; teardown is idempotent; the destructive semantics are documented and the stale SPEC-008/chart docs reconciled.

**Independent Test**: (a) `retain` uninstall = workloads/Service gone, data + runtime state intact, no teardown hooks ran (SC-004); (b) a second `delete` teardown against already-clean state exits success (SC-005, US3 test c); (c) the values reference documents the irreversible semantics + operator-owned boundaries.

### Tests for User Story 3 (write FIRST — confirm RED) ⚠️

- [ ] T020 [US3] Extend `test/helm_teardown_test.sh`: a `retain` uninstall removes only K8s workloads and leaves persisted/runtime state untouched, with **no** teardown hook executed (SC-004 no-regression to SPEC-008 SC-006); and a **second `delete` teardown against the already-clean release** exits success (SC-005 idempotency; depends on T008)

### Implementation for User Story 3

- [ ] T021 [P] [US3] Reconcile `charts/o3k/README.md`: rewrite the Uninstall section (supersede "clean node-local data manually when `delete`"); document the `delete` purge order, DB reset, irreversibility, operator-owned boundaries (external DB beyond this release / networked RBD/S3 not purged — C4/FR-015), and the `--no-hooks` escape; add the `teardown.*` rows to the values table (FR-018/SC-006)
- [ ] T022 [P] [US3] Reconcile the SPEC-008 docs to match what ships: `docs/specs/008-helm-charts/spec.md` (FR-014 — purge now also quiesces + resets the DB), `data-model.md` (uninstall/`dataRetentionPolicy` lifecycle), `tasks.md` (T027/T029 deferral notes → "done in #53"), `quickstart.md` (Uninstall: add the `delete` path) (FR-018)

**Checkpoint**: all three stories independently functional; safety + docs complete.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [ ] T023 [P] `helm lint` clean and `helm template` renders cleanly for both `retain` (no teardown resources) and `delete` (RBAC + 2 Jobs) on single-node values
- [ ] T024 [P] `go test ./internal/teardown/... ./pkg/storage/...` green; `go vet` + `golangci-lint` clean on the new/changed Go
- [ ] T025 Validate the `quickstart.md` runbook on a single-node cluster (kind/k3s): the §2 `delete` walkthrough, §4 zero-residue checks, §5 `--no-hooks` escape
- [ ] T026 [DEFERRED #52] Multi-node DaemonSet host-purge across N nodes (SC-007) — author the per-node mechanism (fan-out Job vs. per-instance terminating cleanup) and its e2e validation **when issue #52 lands**; NOT implemented in this feature (FR-014)

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (P1)** → **Foundational (P2)** → **US1 (P3)** → **US2 (P4)** / **US3 (P5)** → **Polish (P6)**.
- US2 and US3 depend on US1's engine + the shared `test/helm_teardown_test.sh` scaffold (T008); they are independently *testable* but build on US1.

### Within User Story 1

- Tests T006/T007/T008 written first (RED) before implementation.
- T009 (enumerate) precedes T010–T013 (per-class purges depend on enumerated ids).
- T012 (storage purge) depends on T004 (Cinder path fix).
- T014 (roll-up/idempotency/logging) after T010–T013.
- Chart templates T015/T016 are [P] (distinct files); T017 (purge Job) invokes `o3k cleanup` (T005/T014).

### Parallel opportunities

- Setup: T002 ∥ T001.
- Foundational: T003 ∥ start of T005 (different files); T004 follows T003.
- US1 tests: T006 ∥ T007 ∥ T008 (three different files).
- US1 chart templates: T015 ∥ T016 (∥ the Go purge tasks — different files).
- US3 docs: T021 ∥ T022.
- Polish: T023 ∥ T024.

## Parallel Example: User Story 1 tests (RED-first)

```bash
Task: "internal/teardown/teardown_test.go — enumeration/order/idempotency/DB-unreachable (T006)"
Task: "charts/o3k/tests/teardown_test.yaml — render-gating + RBAC + purge-Job specs (T007)"
Task: "test/helm_teardown_test.sh — blocking + zero-residue scenario (T008)"
```

## Implementation Strategy

### MVP first (single-node `delete`)

1. Setup (T001–T002) → Foundational (T003–T005) → **US1 (T006–T018)**.
2. **STOP and VALIDATE**: single-node `delete` teardown blocks and leaves zero residue (SC-001/003).
3. Then US2 (T019) proves pristine redeploy (SC-002), US3 (T020–T022) adds safety/idempotency/docs (SC-004/005/006).

### Notes

- All `delete`-path behavior is opt-in; `retain` (default) renders no teardown resources — verify SC-004 no-regression early (T007).
- FR-015 (networked RBD/S3 **not** auto-deleted) is enforced by *omission* — no task deletes backend data; it is documented in T021.
- Multi-node host purge is out of scope here (T026, [DEFERRED #52]); do not rely on `delete` for multi-node cleanup until #52 ships.
- Commit after each task or logical group; sign off commits (DCO).
