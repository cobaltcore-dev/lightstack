# Implementation Plan: Readiness/liveness endpoints driving k8s-native heartbeating

**Branch**: `013-readiness-liveness-probes` (git: `feature/475-…`) | **Date**: 2026-06-30 | **Spec**: [spec.md](./spec.md)
**Input**: cloud-storage#475 + 11 recorded clarifications (`## Clarifications`, Session 2026-06-30)

## Summary

Wire lightstack's already-present-but-unwired `database.HealthCheck` into honest, k8s-native health endpoints. Add a small `internal/health` package that serves, **unauthenticated on the identity listener (35357) only**:

- `GET /readyz` — DB-gated (pgx pool `Ping`, sub-second timeout) **AND** all-listeners-bound; `200`/`503` with a short curated reason.
- `GET /livez` — process-up, **no external deps**; `200`.
- `GET|HEAD /healthcheck` — OpenStack/oslo-compatible **alias of readiness**, content-negotiating text/JSON.

The chart repoints `livenessProbe → /livez`, `readinessProbe → /readyz`, adds `startupProbe → /readyz` (slow-DB grace), all configurable via `values.yaml` + draft-07 schema. On `SIGTERM` Lightstack flips readiness to not-ready and drains before shutting listeners. An `lightstack healthcheck` subcommand queries the endpoints locally. Downstream consumes the **Pod `Ready` condition** (no custom protocol); `CONTRACT.md` is reconciled from `/v3` to `/readyz`. Tier-2 rich `/healthz` is out of scope.

## Technical Context

**Language/Version**: Go 1.26
**Primary Dependencies**: Gin (HTTP), pgx/pgxpool (existing `database.HealthCheck`). **No new Go modules.** Chart: Helm 3.12+, JSON Schema draft-07, helm-unittest (existing harness) — no new chart deps.
**Storage**: External PostgreSQL — **no schema, no migration, no persisted data** (readiness is a live `Ping`).
**Testing**: `go test` unit (`-race`, `net/http/httptest`); helm-unittest (`charts/lightstack/tests/`); bash integration (`test/`); gophercloud/`openstack` for the no-regression contract gate (Principle I).
**Target Platform**: Linux (Kubernetes via the chart: single-node Deployment + multi-node DaemonSet). Stub mode runs on macOS for dev/unit tests.
**Project Type**: Single Go binary (`cmd/lightstack`) + Helm chart (`charts/lightstack`).
**Performance Goals**: health check returns within 1 s even when the DB is unresponsive (SC-003); sub-second component timeout (fail-fast, Principle III).
**Constraints**: thin signal only — DB ping + listeners-bound, no wide diagnostics (FR-013); endpoints unauthenticated; must work identically in stub and real mode.
**Scale/Scope**: small. ~1 new package, edits to `main.go`, the chart pod-spec helper + values + schema, docs, and four test surfaces.

## Constitution Check

*GATE: must pass before Phase 0 and re-checked after Phase 1. Governing file: `.specify/memory/constitution.md` (6 Lightstack principles).*

| Principle | Verdict | Note |
|-----------|---------|------|
| I. OpenStack Flamingo API Compatibility (NON-NEGOTIABLE) | **PASS** | New paths (`/readyz`, `/livez`, `/healthcheck`) are additive and do **not** shadow any Keystone route (`/`, `/v3`); existing clients/Horizon unaffected. `/healthcheck` *adds* oslo/Octavia compatibility. No microversioned API touched. |
| II. Synchronous Operations | **PASS** | The check is a synchronous in-request `Ping`; no queues/async. |
| III. Fail-Fast Design (<1 s) | **PASS** | Core requirement: sub-second `context.WithTimeout` on the DB ping; endpoint never hangs (FR-003, SC-003). |
| IV. Multi-Mode Architecture | **PASS** | DB is required in *both* stub and real mode (only libvirt/net/storage are stubbed), so readiness is mode-independent; handlers run on macOS stub for unit tests. |
| V. Project Isolation | **PASS (by design)** | Endpoints are unauthenticated by necessity (kubelet) but expose **no** project-scoped data — only coarse up/down + a generic reason (Q3). |
| VI. Single Binary Deployment | **PASS** | All endpoints live in the existing binary on existing listeners; no new process. |
| Forbidden patterns | **PASS** | No MQ/async/state-machine/service-discovery. The binary still runs standalone (endpoints curl-able without k8s); chart probes are an additive deployment-layer concern. |
| Test-First workflow | **PLANNED** | Unit + helm-unittest tests authored RED before implementation (tasks phase). |
| Quality gates (no secrets, stub+real) | **PASS** | No credentials; stub+real both exercised. |

**Note (citation reconciliation)**: the spec originally cited "Constitution Art. II — CLI Interface" / "Art. V — Observability" from the CLAUDE.md *Nine Articles* description, which does **not** match the on-disk governing constitution (6 Lightstack principles; the `memory/constitution.md` Nine-Articles file does not exist). The `lightstack healthcheck` CLI (FR-011) and transition logging (FR-015) stand on their own merits and on consistency with the existing `lightstack cleanup` subcommand; the FR-011 citation was corrected in the spec. No constitution amendment needed.

**No violations → Complexity Tracking empty.**

## Project Structure

### Documentation (this feature)

```text
docs/specs/013-readiness-liveness-probes/
├── plan.md              # this file
├── spec.md              # feature spec (+ 11 clarifications)
├── research.md          # Phase 0 — decisions + technical confirmations
├── data-model.md        # Phase 1 — health entities / Checker model
├── contracts/
│   └── health-endpoints.md   # Phase 1 — the /readyz, /livez, /healthcheck contract
├── quickstart.md        # Phase 1 — verification walkthrough
└── checklists/requirements.md
```

### Source Code (repository root)

```text
internal/health/                 # NEW — the only new package
├── health.go                    #   Checker (DI named checks), Ready/Live handlers, /healthcheck negotiation
└── health_test.go               #   unit tests (httptest): livez/readyz/healthcheck, timeout, not-started, drain

cmd/lightstack/main.go                  # MODIFIED
                                 #   - net.Listen() the 7 servers up-front → flip "ready" flag after all bound
                                 #   - register /readyz,/livez,/healthcheck on the Keystone engine (beside root "/")
                                 #   - SIGTERM: set not-ready + drain delay BEFORE Shutdown()
                                 #   - `lightstack healthcheck [--ready|--live]` subcommand dispatch (like `lightstack cleanup`)
internal/database/db.go          # UNCHANGED — reuse HealthCheck (FR-007/Q7)

charts/lightstack/templates/_helpers.tpl   # MODIFIED — lightstack.podSpec: liveness→/livez, readiness→/readyz, +startupProbe→/readyz (rendered from values)
charts/lightstack/values.yaml              # MODIFIED — `probes:` block (per-probe enable/path/period/timeout/thresholds) + drain knobs
charts/lightstack/values.schema.json       # MODIFIED — draft-07 constraints for the probes block
charts/lightstack/CONTRACT.md              # MODIFIED — readiness anchor /v3 → /readyz (Pod Ready stays the signal)
charts/lightstack/README.md, templates/NOTES.txt   # MODIFIED — operator health guidance + external-restriction note (Q8)

charts/lightstack/tests/probes_test.yaml   # NEW — helm-unittest: probe wiring (Deployment + DaemonSet), defaults, overrides
charts/lightstack/ci/invalid-values/*.yaml # NEW — bad probe values rejected by schema
test/readyz_test.sh                 # NEW — bash integration: DB up/down → 200/503, EndpointSlice removal
test/contract/ (existing harness)   # no-regression run: existing OpenStack contract tests still pass
```

**Structure Decision**: single-project Go layout (matches the repo). The feature is a thin library (`internal/health`) wired in `main.go`, plus chart and test changes — no new top-level projects (Principle VI / simplicity).

## Phase 0 — Research

See [research.md](./research.md). All spec `[NEEDS CLARIFICATION]` were resolved by the 11 clarifications; research consolidates the decisions and confirms the load-bearing technical facts (pgx `Ping` semantics, gin unauth-route + `Accept` negotiation, k8s startup/readiness/liveness semantics, oslo/Octavia precedent, `kube-state-metrics`, the all-listeners-bound + drain patterns).

## Phase 1 — Design & Contracts

- [data-model.md](./data-model.md) — the in-memory `Checker`/`Check`/readiness-state model (no DB).
- [contracts/health-endpoints.md](./contracts/health-endpoints.md) — the three endpoints' methods, status codes, bodies, negotiation, auth, and the Pod-Ready reconciliation of `CONTRACT.md`.
- [quickstart.md](./quickstart.md) — verification walkthrough (curl, `kubectl wait`, kill-DB drain, rolling-update).
- Agent context updated via `update-agent-context.sh claude`.

## Post-Design Constitution Re-check

**PASS (unchanged).** The design adds one small internal library and reuses an existing function; no new dependencies, no schema, no async, no new process, no OpenStack-API regression. The `/healthcheck` alias and the unauthenticated-but-coarse exposure keep Principles I and V satisfied. Ready for `/cl.tasks`.

## Complexity Tracking

No constitution violations — no entries.
