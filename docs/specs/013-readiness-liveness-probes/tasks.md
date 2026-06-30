# Tasks: Readiness/liveness endpoints driving k8s-native heartbeating

**Input**: Design documents from `docs/specs/013-readiness-liveness-probes/`
**Prerequisites**: plan.md, spec.md (11 clarifications), research.md, data-model.md, contracts/health-endpoints.md, quickstart.md

**Tests**: INCLUDED and test-first — the O3K constitution mandates Test-First Development (write tests, confirm RED, then implement GREEN). Each user-story phase lists its tests before its implementation.

**Organization**: tasks are grouped by user story (US1–US5, priority order) so each is independently implementable and testable.

## Implementation status (2026-06-30)

**33/35 done & verified locally.** Gates green: `go build ./...`, `go vet ./...`, `gofmt`, unit tests (`internal/health` + `cmd/o3k`, incl. `-race`; 14 pkgs, 0 fail), `make lint` (golangci-lint, 0 issues), `helm lint`, `helm unittest` (59/59), `charts/o3k/ci/run-tests.sh` (incl. schema rejection of `bad-probes.yaml`).

**2 open — require a live cluster/DB (cannot run in this environment):**
- **T032** — full gophercloud/`openstack` no-regression contract suite (needs a running o3k + DB; the suite dials `:35357`). Build/vet/unit/lint confirm no compile/route regression, and the new paths (`/readyz`,`/livez`,`/healthcheck`) don't shadow Keystone routes by construction.
- **T035** — optional host e2e on k3s (`test/readyz_test.sh` part 2: Pod-Ready gating, EndpointSlice removal, rollout drain).

Note: the `test/readyz_test.sh` script (T010/T019/T025) is delivered; its Part-1 endpoint checks run against any reachable o3k, Part-2 cluster checks are gated behind `RUN_K8S=1`.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: parallelizable — different file, no dependency on an incomplete task.
- **[USn]**: the user story the task serves (story phases only).
- All paths are repo-relative to `/home/fnk/code/sap/ora/o3k/feature/475/`.

## ⚠️ Same-file sequencing (read before parallelizing)

These files are touched by multiple stories and MUST be edited sequentially across phases (they are NOT [P] across stories, even though each within-story edit may be [P] against other files):
- `internal/health/health.go` — core + each handler + diagnosis
- `cmd/o3k/main.go` — readiness wiring, livez/healthcheck registration, drain, CLI
- `charts/o3k/templates/_helpers.tpl` — readiness, liveness, startup probes
- `charts/o3k/values.yaml` + `charts/o3k/values.schema.json` — per-probe config
- `charts/o3k/CONTRACT.md` — anchor + Pod-Ready note + exposure note
- `charts/o3k/tests/probes_test.yaml` — readiness/liveness (US3) then startup (US4)

---

## Phase 1: Setup

- [X] T001 Create the `internal/health` package skeleton in `internal/health/health.go` (package decl, doc comment, type stubs for `Checker`/`Check`) so `go build ./internal/health` succeeds.

---

## Phase 2: Foundational (Blocking Prerequisites)

**⚠️ CRITICAL**: the shared health engine — no user story can be implemented until this is done.

- [X] T002 [P] Write Checker-core unit tests (RED) in `internal/health/health_test.go`: per-check sub-second timeout is enforced (an injected slow check never exceeds the budget; the call never hangs — FR-003/SC-003); the `database` check passes/fails/times-out via an injected probe; `Ready()` returns not-ready while `started==false` and while `draining==true`; `Live()` is always true.
- [X] T003 Implement `Checker`, `Check`, the `database` check (wrapping `internal/database.HealthCheck`, `internal/database/db.go:92`), concurrent bounded-timeout aggregation, the `started`/`draining` atomics, and `Ready()`/`Live()` in `internal/health/health.go` — make T002 GREEN. (FR-003, FR-013; data-model.md)

**Checkpoint**: health engine ready; user stories can begin.

---

## Phase 3: User Story 1 - Honest readiness gates traffic (Priority: P1) 🎯 MVP

**Goal**: o3k is Ready only when the DB is reachable AND all HTTP listeners are bound; not-ready pulls it from Service traffic.

**Independent Test**: deploy with the DB down → `/readyz` 503 and the pod gets no traffic; bring the DB up → 200 and it serves; kill the DB on a running pod → flips to not-ready and leaves the Service.

### Tests (write first, confirm RED)

- [X] T004 [P] [US1] Write `/readyz` handler tests (RED) in `internal/health/readyz_test.go` (`net/http/httptest`): 503 when not-started (`starting`), 503 when the DB probe errors (`database unavailable`), 503 when the DB probe exceeds the sub-second timeout (returns within ~1s, never hangs), 200 `ok` when healthy; assert the 503 body carries only a short reason — no error text, connection string, latency, or stack trace (Q3).

### Implementation

- [X] T005 [US1] Implement the `GET /readyz` handler in `internal/health/health.go`: call `Checker.Ready` under a sub-second `context.WithTimeout`; return `200 "ok"` or `503 "<short reason>"`. (FR-001, FR-003, FR-010)
- [X] T006 [US1] Wire readiness in `cmd/o3k/main.go`: replace the seven `go srv.ListenAndServe()` calls with up-front `net.Listen("tcp", addr)` (fail-fast on bind error) + `go srv.Serve(ln)`; set `Checker.started=true` after all seven bind; register `GET /readyz` on the Keystone `gin.Engine` beside the existing root `/` (unauthenticated, outside the auth group). (FR-001, FR-004; research R8/R11)
- [X] T007 [P] [US1] In `charts/o3k/templates/_helpers.tpl` (`o3k.podSpec`), set `readinessProbe.httpGet` → `/readyz` on the `identity` port with a `timeoutSeconds` bound, rendered from `.Values.probes.readiness`. (FR-008)
- [X] T008 [P] [US1] Add a `probes.readiness` block (enabled/path/periodSeconds/timeoutSeconds/failureThreshold/successThreshold/initialDelaySeconds, with defaults so SC-002 ≈30s detection holds) to `charts/o3k/values.yaml` and draft-07 constraints to `charts/o3k/values.schema.json`. (FR-012)
- [X] T009 [P] [US1] Reconcile `charts/o3k/CONTRACT.md` (line ~91): readiness anchor `GET /v3` → `GET /readyz` (DB-gated; Pod `Ready` remains the consumed signal).
- [X] T010 [US1] Add `test/readyz_test.sh`: o3k up with DB → `/readyz` 200; stop the DB → 503 and the pod IP is removed from the Service `EndpointSlice`; restore → 200 and re-added. (FR-004, SC-001, SC-002)

**Checkpoint**: MVP — honest readiness gates traffic. STOP and validate.

---

## Phase 4: User Story 2 - Liveness recovers a wedged process without crash-looping (Priority: P2)

**Goal**: a wedged process is restarted; a DB outage is NOT (no crash loop).

**Independent Test**: wedge the process → restarted within ~60s; take the DB down for an extended period → process not restarted, `/livez` still 200.

### Tests (write first, confirm RED)

- [X] T011 [P] [US2] Write `/livez` tests (RED) in `internal/health/livez_test.go`: returns 200 unconditionally; with the DB probe forced to fail, `/livez` is still 200 (independent of the DB — FR-002).

### Implementation

- [X] T012 [US2] Implement the `GET /livez` handler (trivial 200 `ok`, no dependency calls) in `internal/health/health.go` and register it on the Keystone `gin.Engine` in `cmd/o3k/main.go`. (FR-002, FR-005)
- [X] T013 [P] [US2] In `charts/o3k/templates/_helpers.tpl`, set `livenessProbe.httpGet` → `/livez` on the `identity` port, rendered from `.Values.probes.liveness`. (FR-005, FR-008)
- [X] T014 [P] [US2] Add a `probes.liveness` block + defaults (so SC-004 ≈60s restart holds) to `charts/o3k/values.yaml` and draft-07 constraints to `charts/o3k/values.schema.json`. (FR-012)

**Checkpoint**: US1 + US2 work independently; liveness/readiness split is correct.

---

## Phase 5: User Story 3 - Operators consume the native health state (Priority: P2)

**Goal**: operators/automation consume the Pod `Ready` condition (no custom protocol); OpenStack tooling can consume `/healthcheck`.

**Independent Test**: `helm install --wait` / `kubectl wait --for=condition=Ready` blocks until serving and fails on timeout; `curl /healthcheck` behaves per oslo.

### Tests (write first, confirm RED)

- [X] T015 [P] [US3] Write `/healthcheck` tests (RED) in `internal/health/healthcheck_test.go`: `GET` and `HEAD`; `Accept: text/plain` → `OK`/`<reason>`; `Accept: application/json` → JSON body; `HEAD` → status only, no body; verdict mirrors `/readyz` (200/503). (FR-014)

### Implementation

- [X] T016 [US3] Implement the `GET|HEAD /healthcheck` handler in `internal/health/health.go` as an alias of readiness with `Accept`-based text/JSON negotiation (gin `Negotiate`), and register it on the Keystone `gin.Engine` in `cmd/o3k/main.go`. (FR-014)
- [X] T017 [P] [US3] Add `charts/o3k/tests/probes_test.yaml` (helm-unittest): assert `readinessProbe` and `livenessProbe` path/port for BOTH the single-node Deployment and the multi-node DaemonSet; assert defaults render and a values override applies. (FR-008)
- [X] T018 [P] [US3] Document native consumption in `charts/o3k/README.md` and `charts/o3k/templates/NOTES.txt` (`kubectl wait --for=condition=Ready`, `helm install --wait`, watching the Pod `Ready` condition) and the `/healthcheck` alias; add the Pod-`Ready`-is-the-signal note to `charts/o3k/CONTRACT.md`. (FR-007)
- [X] T019 [US3] Extend `test/readyz_test.sh`: `helm install --wait` / `kubectl wait --for=condition=Ready` blocks until serving and fails on timeout; the Pod `Ready` condition mirrors `/readyz`. (FR-007, SC-007)

**Checkpoint**: native Pod-`Ready` consumption + OpenStack `/healthcheck` both verified.

---

## Phase 6: User Story 4 - Resilient startup and zero-downtime rollout (Priority: P3)

**Goal**: slow DB connect at boot doesn't kill the pod; rolling updates drain with no dropped requests.

**Independent Test**: slow DB at boot → no restart within grace; DB never up at boot → CrashLoopBackOff; rollout under load → zero dropped requests.

### Tests (write first, confirm RED)

- [X] T020 [P] [US4] Write the drain test (RED) in `internal/health/drain_test.go`: setting `draining=true` makes `Ready()` return not-ready with reason `draining`, even when `started` and the DB probe are healthy. (FR-009)

### Implementation

- [X] T021 [US4] Implement graceful drain in `cmd/o3k/main.go`: on `SIGTERM`, set `Checker` draining first, sleep the configured drain delay (let the EndpointSlice controller deregister), then run the existing `srv.Shutdown(ctx)` sequence. (FR-009, research R9)
- [X] T022 [P] [US4] In `charts/o3k/templates/_helpers.tpl`, add `startupProbe.httpGet` → `/readyz` on `identity` from `.Values.probes.startup` (generous `failureThreshold × periodSeconds` ≈ 150s), plus `terminationGracePeriodSeconds` and an optional `lifecycle.preStop`. (FR-006, FR-009)
- [X] T023 [P] [US4] Add a `probes.startup` block + drain knobs + defaults (so SC-005 ≈150s grace holds) to `charts/o3k/values.yaml` and draft-07 constraints to `charts/o3k/values.schema.json`. (FR-012)
- [X] T024 [US4] Extend `charts/o3k/tests/probes_test.yaml`: assert `startupProbe` path/port and the boot-grace threshold for BOTH Deployment and DaemonSet. (FR-006, FR-008)
- [X] T025 [US4] Extend `test/readyz_test.sh`: slow DB at boot is not restarted within grace; DB never up at boot → CrashLoopBackOff; rolling update under client load → zero dropped requests. (SC-005, SC-006, Q6)

**Checkpoint**: slow boots tolerated; zero-downtime rollouts.

---

## Phase 7: User Story 5 - Diagnosing an unhealthy instance (Priority: P3)

**Goal**: the failing component is visible from the readiness signal; health is queryable from the CLI; transitions are logged.

**Independent Test**: force not-ready → the response names `database`; `o3k healthcheck` exits non-zero; a log line is emitted on each ready↔not-ready transition.

### Tests (write first, confirm RED)

- [X] T026 [P] [US5] Write diagnosis tests (RED) in `internal/health/diagnose_test.go`: `/readyz` and `/healthcheck` name the failing check (`database`); the optional verbose view lists check names only; a structured log line is emitted on each readiness transition (captured via the injected logger). (FR-010, FR-015)
- [X] T027 [P] [US5] Write CLI tests (RED) in `cmd/o3k/healthcheck_cli_test.go`: `o3k healthcheck --ready` / `--live` exits 0 when the target endpoint returns 200 and 1 when it returns 503 or is unreachable.

### Implementation

- [X] T028 [P] [US5] Implement the failing-check-name reason + optional verbose view, and emit the structured transition log (FR-015) on readiness-verdict change, in `internal/health/health.go`. (FR-010, FR-015)
- [X] T029 [P] [US5] Implement the `o3k healthcheck [--ready|--live] [--addr]` subcommand in `cmd/o3k/main.go` (dispatch before `flag.Parse`, mirroring `o3k cleanup`): GET `localhost:<identityPort>/readyz`|`/livez`, print the reason, exit 0/1. (FR-011, research R10)

**Checkpoint**: full diagnosis surface (reason + CLI + logs).

---

## Phase 8: Polish & Cross-Cutting Concerns

- [X] T030 [P] Document external-exposure guidance (Q8) — advise restricting `/readyz`/`/livez`/`/healthcheck` at the external LB/Ingress — in `charts/o3k/README.md` and `charts/o3k/CONTRACT.md`; document that there is NO o3k metrics endpoint (Q9) and that the scrapable signal is the Pod `Ready` condition via `kube-state-metrics`.
- [X] T031 [P] Add `charts/o3k/ci/invalid-values/bad-probes.yaml` (out-of-range / mistyped probe values) and ensure `charts/o3k/ci/run-tests.sh` exercises schema rejection of the `probes` block.
- [ ] T032 No-regression contract gate (Principle I): run the existing gophercloud/`openstack` contract + integration suite and confirm the new unauthenticated paths do not shadow Keystone routes (`/`, `/v3`) or any other service route.
- [X] T033 [P] Bump the chart `version`/`appVersion` (`charts/o3k/Chart.yaml`) and add a `CHANGELOG` entry for the readiness/liveness feature.
- [X] T034 Run the full gate suite: `make test` (`-race`), `make lint`, `make build`, `charts/o3k/ci/run-tests.sh`, `./test/readyz_test.sh` — all green.
- [ ] T035 (Optional) Host e2e on k3s per `quickstart.md`: liveness restart, readiness drain on rollout, startup grace, DB-down → NotReady + EndpointSlice removal.

---

## Dependencies & Execution Order

- **Setup (T001)** → **Foundational (T002–T003)** → user stories.
- **US1 (P1, MVP)**: T004→T005→T006; T007/T008/T009 [P] after T001 (chart-only); T010 after T005–T007.
- **US2 (P2)**: after Foundational. T011→T012; T013/T014 [P].
- **US3 (P2)**: after Foundational; T017 asserts US1+US2 probe wiring, so run it after T007 and T013. T015→T016; T018 [P]; T019 after T016.
- **US4 (P3)**: T020→T021; T022/T023 [P]; T024 extends T017 (run after it); T025 after T021/T022.
- **US5 (P3)**: T026/T027→T028/T029.
- **Polish (T030–T035)**: after the targeted stories; T032/T034 are gates (run last); T035 optional.

**Cross-story same-file edits are sequential** (see the warning block up top): `health.go` (T003→T005→T012→T016→T028), `main.go` (T006→T012→T016→T021→T029), `_helpers.tpl` (T007→T013→T022), `values.yaml`/`values.schema.json` (T008→T014→T023), `CONTRACT.md` (T009→T018→T030), `probes_test.yaml` (T017→T024). This limits cross-story parallelism on those files.

## Parallel Opportunities

- Foundational: T002 (test) is [P]-authored before T003.
- Within US1: T007, T008, T009 (chart/docs) run in parallel with the Go work (T004/T005), different files.
- Test files are per-concern (`readyz_test.go`, `livez_test.go`, `healthcheck_test.go`, `drain_test.go`, `diagnose_test.go`, `healthcheck_cli_test.go`) so all `[P]` test tasks across stories can be written in parallel.
- Polish: T030, T031, T033 are [P] (different files).

## Parallel Example: User Story 1

```bash
# After T001/T003, launch the chart + doc tasks in parallel with the readyz tests:
Task: T007 readinessProbe → /readyz in charts/o3k/templates/_helpers.tpl
Task: T008 probes.readiness in charts/o3k/values.yaml + values.schema.json
Task: T009 CONTRACT.md anchor /v3 → /readyz
Task: T004 /readyz tests in internal/health/readyz_test.go
```

## Implementation Strategy

- **MVP** = Phase 1 + Phase 2 + Phase 3 (US1). Honest readiness alone fixes the core defect (no more healthy-but-broken). STOP, validate, demo.
- **Increment**: add US2 (liveness/no-crash-loop) → US3 (native consumption + `/healthcheck`) → US4 (startup + drain) → US5 (diagnosis + CLI). Each is an independently testable, deployable increment.
- **Test-First (constitution)**: for every phase, author the RED tests first, confirm they fail, then implement to GREEN.

## Notes

- No new Go modules, no DB schema/migration. Reuse `database.HealthCheck`.
- Endpoints unauthenticated on the identity port only (Q2); coarse reason only (Q3); Tier-2 `/healthz` and an o3k metrics endpoint are out of scope (Q5/Q9).
- Commit after each task or logical group; verify tests fail before implementing.
