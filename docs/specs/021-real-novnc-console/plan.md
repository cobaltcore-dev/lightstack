# Implementation Plan: a real console / noVNC (F1)

**Branch**: `feature/534` (feature id `021-real-novnc-console`) | **Date**: 2026-07-30 | **Spec**: [spec.md](./spec.md)
**Input**: `docs/specs/021-real-novnc-console/spec.md` — 30 functional requirements, 12 clarification decisions, no open forks.

## Summary

Make Horizon's Console tab show a running instance's real framebuffer, closing **F1** — the last open finding from the #482 verification pass.

The approach inverts the ticket's assumption. The ticket asks for "a real proxy that validates the token and connects to the guest VNC"; the clarification session established that **we do not write one**. The noVNC image already pinned in the verification stacks bundles websockify and its token plugins, and a command override turns it into exactly that proxy — verified end to end (research.md R5: a good token yields `101 Switching Protocols` and `connecting to: 127.0.0.1:5999`; a bad one is closed with no response and no guest contacted). So Lightstack's work reduces to four things:

1. **Read the real console port** back from libvirt per request (`DomainGetXMLDesc`, flags `0` so `autoport` is resolved), replacing `5900 + hash(id) % 1000`.
2. **Own a token lifecycle** — a database table as the store of record, projected into `/var/lib/lightstack/console-tokens/` as the `token: host:port` entries the proxy reads, revoked on every endpoint-moving operation and reaped on expiry.
3. **Return a working URL** at an operator-configured origin, in upstream's shape, and refuse honestly everywhere it cannot.
4. **Ship the proxy** in `charts/lightstack`, and relocate the dashboard chart out of its verification-only home.

The security posture is the part worth stating plainly: the guest console moves from listening on `0.0.0.0` to loopback only, so the token stops being decorative and becomes the sole path in.

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+)
**Primary Dependencies**: gin-gonic/gin (HTTP), jackc/pgx/v5 + pgxpool (PostgreSQL), digitalocean/go-libvirt (`DomainGetXMLDesc` — confirmed present in the pinned revision), `encoding/xml` (stdlib) for the graphics element. **No new Go modules** — the relay is not ours.
**External component**: `bonigarcia/novnc:1.3.0`, **unmodified**, command overridden to `python3 -m websockify --web /root/noVNC --token-plugin TokenFile --token-source <dir> 6080`. Ships noVNC **1.6.0** and websockify with its token plugins (the tag is the image's version, not noVNC's).
**Storage**: PostgreSQL — one new migration `065`: console-token table; drops the dead `instances.console_vnc_port` / `console_vnc_password`. Node-local: one new subdirectory `/var/lib/lightstack/console-tokens` under the existing `realMode.hostStoragePath` contract.
**Testing**: `go test` unit tests (XML parse, token lifecycle, authz, refusals); rewritten `test/console_test.sh` + `test/console_access_test.sh`; helm-unittest for chart wiring; host runbook for stub + real. **Not** `test/contract/nova` — stub-only (F15).
**Target Platform**: Linux for real mode (libvirt/KVM); stub mode everywhere including macOS.
**Project Type**: single Go binary + Helm charts (existing layout, no new project).
**Performance Goals**: console request within the API budget; the added libvirt XML read is one round trip on an already-open connection. Framebuffer visible within 10s of opening the tab (SC-001).
**Constraints**: fail-fast ≤1s on the libvirt read (Constitution III); synchronous issuance, no queues (II); stub mode must work with no libvirt (IV); project-scoped with an `admin` override (V).
**Scale/Scope**: single-node real mode only — multi-node console is out of scope for this feature and v1. Concurrent sessions: a handful; token rows are short-lived and reaped.

## Constitution Check

*GATE: evaluated before Phase 0 and re-evaluated after Phase 1 design. Both passes recorded.*

> ⚠️ **This repository contains two different constitutions, and both are checked below.**
> `.specify/memory/constitution.md` (139 lines, six Lightstack-specific principles) is what the planning tooling loads.
> `.claude/memory/constitution.md` (82 lines, the **Nine Articles**) is what `CLAUDE.md` designates as authoritative
> ("Immutable principles in `memory/constitution.md`"). An initial version of this plan checked only the first and
> missed the second — caught by the analysis pass. Both are evaluated here, and where they **conflict** the conflict
> is named rather than resolved silently. **Reconciling the two documents is a project-level task outside this
> feature**; a feature plan is the wrong place to pick a winner.

### A. Lightstack principles (`.specify/memory/constitution.md`)

| # | Principle | Verdict | Evidence / note |
|---|---|---|---|
| I | **Flamingo API compatibility** (non-negotiable) | ✅ PASS | Three surfaces implemented to the api-ref shapes captured in `contracts/nova-console-api.md`: `remote-consoles` (2.6+), the legacy `os-getVNCConsole` action, `os-console-auth-tokens/{token}` (2.31+). Upstream's `path=%3Ftoken%3D…` URL encoding is reproduced exactly and **verified** to work against our proxy. Two deliberate, documented choices where upstream offers no answer: the client page is `vnc_lite.html` (upstream's `vnc_auto.html` 404s on noVNC 1.6.0 — the URL is opaque to clients, so this is not a contract break), and stub mode returns 501 (no upstream analogue). |
| II | **Synchronous operations** | ✅ PASS | Issuance is one synchronous path: read the port from libvirt, insert the row, write the projection file, respond. No queue, no state machine. |
| III | **Fail-fast** | ✅ PASS | The XML readback is bounded at **1s** by an explicit deadline (T007a) — necessary because go-libvirt's generated calls take no `context`, so the bound has to be imposed by the caller. The dialer's 2s `WithLocalTimeout` applies **only at connect**, not per call, so it does not sit inside a console request; the connection is established once at startup. No retry loop. |
| IV | **Multi-mode architecture** | ✅ PASS | Stub mode refuses at request time (501) with no libvirt involvement; every new unit test runs without Linux. |
| V | **Project isolation** | ✅ PASS | Tokens are issued only for the caller's project, with an explicit `admin` override via the shared `common.HasRole` — precisely the "explicit admin role check" the principle allows. |
| VI | **Single binary deployment** | ⚠️ **DEVIATION** | The console proxy is a separate container. Justified in Complexity Tracking. |
| — | **Forbidden Patterns**: no async workers | ⚠️ **DEVIATION** | A periodic token reconciler is required for expiry to be enforceable at all. Justified in Complexity Tracking. |
| — | **Reversible migrations** | ✅ PASS | `065` up creates the table and drops two dead columns; down drops the table and restores both columns. |
| — | **Test-first** | ✅ PASS with a caveat | Unit tests and rewritten scripts precede implementation. The caveat is real: `test/contract/nova` is stub-only (F15), so no existing contract harness can gate real-mode console behaviour — the host runbook is the gate. |
| — | **No hardcoded credentials** | ✅ PASS | The feature *removes* a generated-and-stored VNC password rather than adding a secret. Token values are never logged in reusable form (FR-028). |

### B. The Nine Articles (`.claude/memory/constitution.md`, designated by CLAUDE.md)

| Art. | Principle | Verdict | Evidence / note |
|---|---|---|---|
| I | **Library-First** — every feature begins as a standalone library | ✅ PASS **after remediation** | An earlier draft claimed this Article conflicts with Lightstack Principle VI. **It does not** — libraries compile *into* the single binary, which is exactly what `pkg/hypervisor`, `pkg/networking`, `pkg/storage`, `internal/health` and `internal/teardown` already are. The token store therefore ships as a self-contained, independently testable package, **`internal/console`**, following the `internal/teardown` precedent; `internal/nova` keeps only thin handlers that call it. `pkg/paths.ConsoleTokenDir` and `pkg/hypervisor.GetConsoleEndpoint` are libraries already. |
| II | **CLI Interface** — every library exposes functionality via CLI | ✅ PASS **after remediation** | Also not in conflict with a single binary: `cmd/lightstack/main.go` already dispatches `cleanup` (`:48`) and `healthcheck` (`:55`) subcommands, each a thin wrapper over a library package, with text to stdout, errors to stderr and documented exit codes. `internal/console` gains the same treatment — **`lightstack console-tokens list|purge`** (T012/T013), with `--json` for structured output. This replaces hand-inspecting the token directory with `ls`/`cat`, so it improves the operator story rather than merely satisfying the Article. |
| III | **Test-First (NON-NEGOTIABLE)** | ✅ PASS | Every implementation task is preceded by a failing-test task in the same phase: T006→T007, T008→T009, T010→T011, T012→T013, T020/T021→T022, T036→T037. |
| IV | **Integration Testing** — real databases over mocks, contract tests mandatory | ✅ PASS | Token-store tests run against a **real throwaway PostgreSQL**, not mocks (the pattern established by #533), stated explicitly in the task descriptions. Contract tests exist as T012 and T036. |
| V | **Observability** — structured logging, clear errors | ✅ PASS | T041; every refusal names its reason (contracts/nova-console-api.md status matrix). |
| VI | **Versioning** — semantic versioning | ✅ PASS **after remediation** | Originally uncovered. Now T033 bumps `charts/lightstack`'s chart version and T034 sets `charts/horizon`'s version and appVersion deliberately. |
| VII | **Simplicity** — ≤3 projects, YAGNI, no future-proofing | ✅ PASS **after remediation** | The `console_type` column was future-proofing for a second console type that FR-025 explicitly refuses; **dropped**. No new project: charts are not projects and `charts/horizon` is a relocation, not a new codebase. |
| VIII | **Anti-Abstraction** — single model representation | ✅ PASS | No abstraction layer or interface is introduced; go-libvirt and gin are used directly. The token existing both as a row and as a projection file is not a second model — it is the literal wire format an external component requires, with the database declared the single source of truth (contracts/console-token-projection.md, invariant 1). |
| IX | **Integration-First** — contract tests before implementation | ✅ PASS with a caveat | T012 and T036 encode contracts/nova-console-api.md before the handlers exist. The caveat is honest: `test/contract/nova` is stub-only (F15), so the rewritten shell integration tests (T019, T028, T042) plus the host runbook carry real-mode verification. |

**Post-Phase-1 re-evaluation**: no new violations from the design artifacts. **Two** deviations total, both introduced by this feature and both argued in Complexity Tracking (Lightstack Principle VI, and the background-worker ban). Articles I and II were initially recorded as deviations on the grounds that they conflict with the single-binary principle; that was wrong — a library is compiled *into* the binary and a subcommand is *part of* it — so both are now satisfied rather than excused.

## Complexity Tracking

| Violation | Why needed | Simpler alternative rejected because |
|-----------|------------|--------------------------------------|
| **Principle VI** — a separate proxy container alongside the binary | The browser↔guest relay is a WebSocket-to-TCP proxy. The pinned noVNC image already contains a hardened, maintained one (websockify) plus the client assets; using it means **zero relay code and no new Go dependency**. The proxy is third-party infrastructure in the same class as PostgreSQL, memcached and Horizon — all already external — not a seventh Lightstack service. Lightstack's binary gains nothing and stays as it is. | Writing an in-process relay was evaluated in clarification and rejected: it means a new WebSocket dependency plus vendoring third-party client assets (MPL-2.0) into the binary, to reimplement a component we already ship. Principle VI forbids splitting *Lightstack's services* into microservices; it does not forbid depending on external infrastructure, which the deployment already does five times over. |
| **Forbidden Patterns** — a periodic token reconciler | websockify's token file format has **no expiry field** (verified, research.md R4), so an expired token stays live for exactly as long as its file exists. Deleting the file *is* the expiry mechanism. Lazy reaping at next-request would leave a token valid for hours after expiry, making FR-010 false. | Lazy-only reaping breaks the requirement (shown in R7). Pushing expiry into the proxy is impossible with any file-based plugin, and the HTTP-callback plugin that could do it needs a Python dependency absent from the image. A reconciler is not a job processor: no queue, no work items, only Lightstack reconciling its own projection to its own database — with in-repo precedent in the SPEC-009 real-mode network reconciler. |

**Honest consequence of the second deviation**: effective maximum token validity is `TTL + reconcile interval` (10m30s at the proposed defaults), not exactly 10m. Measurable and testable; recorded here rather than buried.

**Recommendation beyond this feature**: two authoritative constitutions is a defect in its own right — it made a CRITICAL gate finding possible, and it will do so again. Reconciling them (or designating one and deleting the other) deserves its own ticket.

## Project Structure

### Documentation (this feature)

```text
docs/specs/021-real-novnc-console/
├── spec.md                        # complete, 12 clarifications recorded
├── plan.md                        # this file
├── research.md                    # Phase 0 — R1..R10, all verified
├── data-model.md                  # Phase 1
├── quickstart.md                  # Phase 1
├── contracts/                     # Phase 1
│   ├── nova-console-api.md        # the three HTTP surfaces + status codes
│   ├── console-token-projection.md# the token-file directory contract
│   └── proxy-deployment.md        # the websockify invocation + placement contract
├── checklists/requirements.md     # spec quality — all items pass
└── tasks.md                       # Phase 2 — NOT created by this command
```

### Source code (repository root)

```text
internal/console/                  # NEW library (Article I) — the token store, self-contained
├── store.go                       #   Issue / Resolve / RevokeByInstance / RevokeExpired
├── projection.go                  #   the token-file writer + reaper
├── reconciler.go                  #   periodic + startup sweep
├── store_test.go                  #   lifecycle, expiry, fail-closed (real throwaway Postgres)
└── cli.go                         #   `lightstack console-tokens list|purge` (Article II)

internal/nova/
├── console.go                     # REWRITE: thin handlers — real URLs, honest refusals;
│                                  #   delete setupVNCConsole/generateConsoleToken/
│                                  #   generateVNCPassword/hashString
├── console_test.go                # NEW: refusals, authz, URL shape
├── handlers.go                    # route os-console-auth-tokens; revoke on
│                                  #   reboot/os-stop/os-start (:1548-1610) and DeleteServer (:1221)
└── advanced_actions.go            # revoke on suspend/resume (:20, :88)

pkg/hypervisor/
├── libvirt.go                     # NEW: GetConsoleEndpoint (DomainGetXMLDesc, flags 0) + stub branch
├── xml_template.go                # graphics: listen 127.0.0.1, sharePolicy='force-shared' (:163-168)
├── libvirt_test.go                # NEW: graphics XML parse cases
└── xml_template_test.go           # assert loopback + force-shared

pkg/paths/
├── paths.go                       # NEW: ConsoleTokenDir() beside ImagesDir/VolumesDir/…
└── paths_test.go                  # extend TestLayout — the node-local layout contract

internal/common/config.go          # NovaConfig: console_proxy_base_url, token TTL, reconcile interval
cmd/lightstack/main.go                    # wire the reconciler into bootstrap; dispatch the
                                   #   `console-tokens` subcommand beside cleanup (:48) / healthcheck (:55)
config/lightstack.yaml                    # nova console block

migrations/
├── 065_console_auth_tokens.up.sql # table + indexes; drop 2 dead instance columns
└── 065_console_auth_tokens.down.sql

charts/lightstack/
├── templates/console-proxy.yaml   # NEW: Deployment + Service (real mode, hostNetwork, node-pinned)
├── templates/_helpers.tpl         # console-tokens subdir (:177-193); config render (:115-118)
├── values.yaml                    # consoleProxy.* + nova console settings
├── values.schema.json             # MUST extend — additionalProperties: false
└── tests/                         # init_host_storage_test.yaml + console-proxy specs

charts/horizon/                    # RELOCATED from deployments/horizon-chart (novnc.yaml deleted)

deployments/docker-compose-horizon.yml  # retire the dead-placeholder novnc service
Makefile                           # chart paths (:105-107, :116-122)

test/
├── console_test.sh                # REWRITE — currently passes against the broken impl
├── console_access_test.sh         # REWRITE
└── ws0_console_test.sh            # must stay green (serial log)

docs/                              # 4 guides: make console_proxy_base_url real, drop /vnc_auto.html
docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md  # F1 → fixed, + headline lines
```

**Structure decision**: one new library package, `internal/console`, holding the token lifecycle — self-contained, independently testable, and exposed via an `lightstack console-tokens` subcommand, matching the `internal/teardown`+`cleanup` and `internal/health`+`healthcheck` pairs already in the tree (Articles I and II). Everything else extends existing packages (`internal/nova` for thin handlers, `pkg/hypervisor`, `pkg/paths`), plus one migration, one chart template and the chart relocation. No new project: Article VII's ≤3 limit is untouched, since `internal/console` is a package inside the existing binary, not a separate build.

## Phases

### Phase 0 — Outline & Research ✅ complete

Output: [research.md](./research.md). Ten items (R1–R10), each Decision / Rationale / Alternatives. Nothing marked NEEDS CLARIFICATION remains.

Three findings changed the design rather than merely confirming it:

- **R5** proved the whole mechanism works against the pinned image, including fail-closed behaviour on a bad token — so FR-011 is satisfied by the component, not by our code.
- **R7** found that websockify's token file has no expiry field, which turns reaping from hygiene into the actual expiry mechanism and forces the reconciler deviation.
- **R10** found that 14 of the 16 references to the chart's old path live in other features' spec directories, which we must not edit — narrowing FR-023e.

### Phase 1 — Design & Contracts ✅ complete

Outputs: [data-model.md](./data-model.md), [contracts/](./contracts/), [quickstart.md](./quickstart.md).

- **data-model.md** — the console-token entity (fields, indexes, state transitions, the projection's derived nature), and the two columns being dropped.
- **contracts/nova-console-api.md** — request/response bodies and the full status-code matrix for the three surfaces.
- **contracts/console-token-projection.md** — the file format, naming, permissions, and the invariant that the directory is derived and fail-closed.
- **contracts/proxy-deployment.md** — the exact websockify invocation, placement requirements, and what must be asserted by chart tests.
- **quickstart.md** — bring-up and verification for stub and real.

### Phase 2 — Tasks

Not produced by this command. `/cl.tasks` will derive a dependency-ordered `tasks.md`.

Suggested sequencing, so the ordering rationale isn't lost: **libvirt readback → token store + projection → API behaviour → config → charts → relocation → tests → docs/report**. The readback comes first because everything downstream needs a real endpoint to carry; the relocation comes late because it touches files other tasks also edit.

## Risks

| Risk | Mitigation |
|---|---|
| **Proxy cannot read the token files** (UID/mode mismatch — the pod drops ALL caps and may run non-root). Failure mode is "every token refused", indistinguishable from a token bug. | Decide mode/UID explicitly and assert it in a chart test; the quickstart includes a direct `?token=` probe that isolates proxy-side failure from Lightstack-side. |
| **`DomainGetXMLDesc` called with the wrong flags** returns the inactive definition, where the port is still `-1` — reproducing the original bug in a new form. | Flags `0` is called out in research.md R3 and in the unit tests, including an explicit "autoport not yet resolved" case. |
| **Proxy scheduled away from the Lightstack node** sees an empty directory and refuses everything. | Node pinning is a requirement (FR-023c), not a coincidence of single-node k3s; asserted in a chart test. |
| **`values.schema.json` has `additionalProperties: false`** — new values fail validation until the schema is extended. | Schema change lands in the same commit as the values; `make chart-test` catches it. |
| **The chart relocation breaks the SPEC-018 runbooks**, which we may not edit. | Deliberate and documented (R10): a pointer in this feature's runbook and a note in the compatibility report. |
| **No contract-test gate for real-mode console** (F15). | Host runbook is the gate; stated as a known limitation rather than papered over. |
