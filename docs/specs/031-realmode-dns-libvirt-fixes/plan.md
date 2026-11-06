# Implementation Plan: Real mode — cluster DNS under host networking, the libvirt socket, and a capabilities read that is never retried

**Branch**: `bugfix/630` (feature id: `031-realmode-dns-libvirt-fixes`) | **Date**: 2026-11-06 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `docs/specs/031-realmode-dns-libvirt-fixes/spec.md` (cloud-storage#630)

## Summary

Three real-mode defects, and the silent failure behind two of them. The fixes are small and local: one template condition, one rendered config line, one parser, one dialer, one connection loop that replaces today's reconnect loop, one retried preparation, one step before every guest launch, one readiness check. **No new Go module, no new chart value, no database schema change, no migration, no API shape change.** One constitution amendment, v1.5.0, was ratified on 2026-11-06 after the user chose it at the gate.

- **Cluster DNS (Problem 1, R1).** The shared pod spec and the purge Job render `dnsPolicy: ClusterFirstWithHostNet` beside `hostNetwork` when real mode runs with host networking, as the console proxy already does. Without host networking nothing is rendered.
- **The socket reaches the binary (Problem 2, R2).** The rendered config carries `libvirt_uri: "<path>"` when `realMode.libvirtSocketPath` is not the default; the default renders `qemu:///system` as today. The schema requires an absolute path.
- **The binary chooses the socket (Problem 2, R3–R4).**
  - `ParseLibvirtURI` accepts the system URI (automatic choice), the same with `?socket=<abs>`, or a bare absolute path.
  - Anything else stops real-mode startup at configuration load, in the server, `lightstack cleanup` and `console-tokens`.
  - The automatic choice is a dialer of our own (go-libvirt's `socket.Dialer` is one method). On every attempt it dials `virtqemud-sock` if present, else `libvirt-sock`, and logs which.
- **Connection (US6, R5).**
  - The server's hypervisor starts unconnected and returns at once. A background loop dials — each attempt bounded at 1 s, pauses of 1 → 5 s — runs the preparation on every connection, and watches for loss. It replaces `keepConnected`/`redial`.
  - Startup never waits on libvirt.
  - `lightstack cleanup` keeps one synchronous attempt, bounded at 1 s like the loop's, and fails closed (FR-020a).
- **Preparation (Problem 3, R6).**
  - Attempts are retried in the background after every connection until one succeeds, and once on demand per guest start while not yet prepared. Callers share one in-flight attempt (single-flight), and each is bounded at 1 s, so a guest start waits at most one bound.
  - The first success is sticky; later successes refresh QEMU's owner.
  - The log reports the first failure (ERROR), the success after it (INFO) and nothing louder than DEBUG in between.
- **Every launch (R7).** (*Launch* = a *guest start*, as the spec's Key Entities define it.) The four functions that reach `DomainCreate` — create (and self-heal), start, hard reboot, resume — replace their `consoleDirReady()` check with the on-demand attempt. They then make the instance's directory QEMU's immediately before `DomainCreate` (`PrepareInstanceDir`, idempotent; the domain UUID is the instance ID).
- **Readiness (R8).** A `hypervisor` check, real mode only, reads the sticky flag: `503 hypervisor unavailable` until the first preparation succeeds, then never again because of libvirt. The checker is created after the hypervisor starts.
- **Never fake (R9).**
  - Real mode always has a manager, so an unreachable libvirt fails like a lost connection: a create ends `ERROR` with a `fault`, an action gets the generic `500`.
  - `main.go` treats an `InitHypervisor` error as fatal in real mode.
  - The create's stub branch marks `ERROR` in real mode, and every real-mode create that ends `ERROR` records a `fault` through `markInstanceError`, with a curated message (spec Clarifications, analysis session).

Deferred from clarification and settled here: every connection attempt is bounded at 1 s — the server's in its loop and cleanup's single one (R5); concurrent preparation attempts share one in-flight call (R6); the readiness reason is `hypervisor unavailable` (R8). The analysis pass (2026-11-06) tightened the first two and added the `fault` (spec Clarifications).

## Technical Context

**Language/Version**: Go 1.27.3 (the module pins `go 1.26`; the constitution requires 1.26+). Helm 3.12+ chart templates (Go `text/template` + Sprig), JSON Schema draft-07. Markdown.
**Primary Dependencies**: `digitalocean/go-libvirt` at the pinned `v0.0.0-20260609165003-6254771e63a8` (`socket.Dialer`, `ConnectToURI`, `Disconnected()`); `gin`; `internal/health`; `golang.org/x/sync/singleflight` (already required in `go.mod`, indirect → direct); helm-unittest. **No new module.**
**Storage**: nothing new. No database schema change, no migration. Node-local ownership only: `<state_path>/console-sockets` (as today) and `<state_path>/instances/<id>` (now ensured before every launch).
**Testing**: `go test`, with database-backed tests on PostgreSQL and SQLite (`LIGHTSTACK_TEST_DB_URL`):
- the existing fake libvirtd, which speaks libvirt's wire protocol, for connection, retry and launch tests;
- real unix listeners in `t.TempDir()` for the socket choice;
- the built binaries in `cmd/lightstack/startup_refusal_test.go` for refusals;
- helm-unittest for the chart.

Host validation follows FR-021 (a)–(e) on k3s and the openSUSE host (R12, `quickstart.md`).
**Target Platform**: Linux real mode — k3s via the chart, Docker Compose, bare metal. Stub mode is unchanged and still runs on macOS.
**Project Type**: single Go project, plus the Helm chart and docs.
**Performance Goals**: none new. The readiness probe is an atomic load. Each libvirt attempt is ≤ 1 s, background pauses 1–5 s. A guest start adds at most one bounded attempt, and only while unprepared.
**Constraints**:
- Article III fail-fast: request-path attempts ≤ 1 s, and no request waits on a loop (v1.5.0 exception).
- Article IV: no runtime stub fallback in real mode (v1.5.0).
- Article I: no API shape change. A create still answers `202` and ends `ERROR`; actions keep SPEC-025's `500`.
- SPEC-013 readiness amended for the one-time preparation only.
- The default chart render changes only by `dnsPolicy` under host networking.

**Scale/Scope** (from the file reads in research):

| Area | Files |
|---|---|
| Go | `pkg/hypervisor/libvirt.go` (constructors, loop, dialer use, 4 launch functions); `pkg/hypervisor/console_dir.go` (state, attempts, logging); `pkg/hypervisor/disk.go` (comment); a new `pkg/hypervisor/libvirt_uri.go` (parser and dialer); `internal/nova/handlers.go` (`InitHypervisor`, the create branch, the `fault` on every create `ERROR`, `HypervisorReady`); `cmd/lightstack/main.go` (`loadConfig` check, startup order, fatal init, readiness check). `internal/teardown` keeps its code: `NewVMManager`'s contract is unchanged |
| Tests | `pkg/hypervisor` (URI table, dialer, connection loop, preparation retry, launch ownership; fake libvirtd knobs); `internal/nova` (create and action without a connection, the fault, the manager guarantee); `test/contract/nova/no_hypervisor_test.go` (gophercloud, gated); `cmd/lightstack` (readiness checks, startup refusals, `loadConfig`); `internal/teardown` (fail-fast on a missing and on a silent socket) |
| Chart | `templates/_helpers.tpl`, `templates/teardown-purge-job.yaml`, `templates/NOTES.txt`, `values.yaml`, `values.schema.json`, `README.md`, `CONTRACT.md`; tests: a new DNS-policy suite, `libvirt_socket_mount_test.yaml` or `config_render_test.yaml`, `schema_test.yaml` |
| Docs | `docs/CONFIGURATION.md`, `SCALING.md`, `TROUBLESHOOTING.md`, `REAL_LIBVIRT_MODE.md`, `REAL_MODE_TESTING.md`, `HORIZON_INTEGRATION.md`, `blueprint.md`, `UPGRADING.md`; `CLAUDE.md` (Mode Detection, with the amendment) |
| Governance | `.specify/memory/constitution.md` → v1.5.0, ratified 2026-11-06 |

No `NEEDS CLARIFICATION` remains: the spec's two Clarifications sessions settled scope, answers, ownership, validation, cleanup and docs, and research R1–R13 settle every design choice.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (below the table).*

| Principle (`.specify/memory/constitution.md` v1.4.0 at planning; v1.5.0 ratified 2026-11-06) | Verdict | Why |
|---|---|---|
| I. OpenStack Flamingo API compatibility | PASS | No endpoint, field or status is added or reshaped. A create against an unreachable libvirt answers `202` and ends `ERROR` with a `fault` — upstream's outcome when no compute can host the server — instead of a fake `ACTIVE`. Every real-mode create that ends `ERROR` now records the `fault` SPEC-025 added, which the libvirt failure path used to skip. Actions keep SPEC-025's deliberate generic `500`. Readiness is a Kubernetes endpoint; the oslo-style `/healthcheck` alias already answers `503` on a failed check. No entry for the Known Divergences register. |
| II. Synchronous operations | PASS | Server create is unchanged and stays asynchronous, as upstream's is: it answers `202 BUILD` and its goroutine does the libvirt work (`handlers.go:1090-1095`), so FR-019's `ERROR` is written there. Article II's VM-creation bullet yields to Article I under Governance rule 1 (compatibility first); this feature adds no new asynchrony. Every other operation completes before it answers. The loops accept no work from a request. A guest start makes one bounded attempt, or takes the result of the one already running — never both, never a wait for the loop. |
| III. Fail-fast | **PASS (v1.5.0)** — failed against v1.4.0 | "Never: retry loops, exponential backoff" is already contradicted on `main` by the reconnect loop of `6ed30f7`, recorded in no spec or plan. This feature extends that loop to the first connection and adds the preparation retry, which the clarified readiness gate requires (an on-demand-only retry deadlocks: no guest start reaches a not-Ready pod). Every attempt — the loop's, a guest start's, and `lightstack cleanup`'s single one — is bounded at 1 s, matching "libvirt connection failures: 1 second timeout". A guest start joins an attempt already running instead of following it with its own. Requests fail at once while the loop runs. The ratified exception (research R13) states those limits. |
| IV. Multi-mode architecture | **PASS (v1.5.0)** — failed against v1.4.0 | "Default to stub mode when dependencies unavailable" and "Mode detection: … or `svc.vmManager == nil`", read at runtime, are the fake-`ACTIVE` fallback FR-019 removes. The draft keeps stub as the default for an unconfigured mode and forbids runtime fallback from a configured real mode. Stub mode itself is unchanged and every test still runs without Linux. |
| V. Project isolation | PASS | No query or scoping change. |
| VI. Single binary | PASS | Nothing added outside the binary; no new process or service. |
| Technology constraints | PASS | No new module. `golang.org/x/sync`, already required in `go.mod`, becomes a direct dependency for `singleflight` (R6). |
| Forbidden patterns | PASS with the III exception | No queue, worker, state machine or service discovery. The two loops are the drafted exception's subject: no queue, no job records, no per-request state. |
| Test-first | PASS by plan | Each requirement's test is written first and seen failing (research R10). |
| Backwards compatibility | PASS | The default chart render changes only by `dnsPolicy`, and only under host networking. `docs/UPGRADING.md` states the behaviour changes (FR-024): an unsupported `libvirt_uri` now stops startup, an unpreparable real-mode pod is no longer Ready, host-networked names resolve through the cluster DNS, and the workarounds can go. No migration, nothing to reverse. |
| Security | PASS | No credential, token or scoping change. The readiness reason stays coarse (CONTRACT.md §2). The socket path is schema-checked as absolute. DNS queries go to the cluster DNS, as every other pod's do. |
| Quality gates before merge | PASS by plan | Both modes: stub unchanged and tested, real mode on the host (FR-021). Both database backends run the `internal/nova` and `cmd/lightstack` tests, though no SQL changes. No migration, so the up/down gate is N/A. |

**Gate decision (2026-11-06)**: the user chose to amend the constitution rather than justify the departures in this plan. Research R13 holds the v1.5.0 text. Phase 0 and Phase 1 proceeded, and v1.5.0 was ratified on 2026-11-06 (`.specify/memory/constitution.md`); **task generation may proceed.**

**Post-design re-check (after Phase 1)**: unchanged.
- The design adds no abstraction beyond the one dialer go-libvirt's own interface asks for, and the parser.
- The two loops collapse into one connection loop that runs the preparation, so the exception covers one place in the code.
- The contracts (`contracts/*.md`) add no API surface; they pin today's answers for the new states.
- No new divergence. Articles III and IV pass under v1.5.0, and nothing else needed amending.

## Project Structure

### Documentation (this feature)

```text
docs/specs/031-realmode-dns-libvirt-fixes/
├── spec.md                  # 2 clarification sessions (7 Q&As)
├── plan.md                  # this file
├── research.md              # Phase 0: R1–R13 (R13 = constitution v1.5.0 draft)
├── data-model.md            # Phase 1: configuration and in-process state (no DB entities)
├── quickstart.md            # Phase 1: gates, render and binary checks, host validation (a)–(e)
├── contracts/
│   ├── chart-render.md      # dnsPolicy, libvirt_uri rendering, schema, chart docs
│   ├── libvirt-connection.md# URI forms, refusals, socket choice, server loop, cleanup, preparation
│   ├── readiness.md         # /readyz in real mode
│   └── guest-operations.md  # create/actions while unreachable or unprepared; ownership before launch
├── checklists/requirements.md
└── tasks.md                 # Phase 2 (not created here)
```

### Source Code (repository root)

```text
charts/lightstack/
├── templates/_helpers.tpl            # podSpec: dnsPolicy; lightstack.config: libvirt_uri
├── templates/teardown-purge-job.yaml # dnsPolicy
├── templates/NOTES.txt               # readiness wording
├── values.yaml / values.schema.json  # comments, descriptions; libvirtSocketPath pattern
├── README.md / CONTRACT.md           # value rows, health table, §2 readiness
└── tests/                            # DNS-policy suite; libvirt_uri render; schema

pkg/hypervisor/
├── libvirt_uri.go        # NEW: ParseLibvirtURI, the per-attempt dialer
├── libvirt.go            # StartVMManager + connection loop (replaces keepConnected/redial);
│                         # NewVMManager one-shot; the 4 launch functions
├── console_dir.go        # sticky state, shared (single-flight) attempts, logging
├── disk.go               # PrepareInstanceDir comment
└── *_test.go             # URI table, dialer, loop, retry, launch ownership; fake libvirtd knobs

internal/nova/handlers.go # InitHypervisor(consoleDir) → StartVMManager; create branch; fault on every create ERROR; HypervisorReady
cmd/lightstack/main.go    # loadConfig check; hypervisor before health.New; fatal init; hypervisor check
internal/teardown/        # test only: cleanup fails fast on a missing or silent socket
test/contract/nova/no_hypervisor_test.go  # gophercloud, gated: create ends ERROR with a fault
docs/*.md, CLAUDE.md      # per FR-022/023/024 and the amendment
```

**Structure Decision**: the existing single-project layout. The one new file, `pkg/hypervisor/libvirt_uri.go`, holds the parser and the dialer so `libvirt.go` keeps its role. No new package. `internal/common` does not learn about libvirt (R3).

## Complexity Tracking

Both rows are resolved by the constitution amendment (v1.5.0, research R13), not by this table. They are listed because the departures are real, and the v1.2.0 rationale requires that they be visible.

| Violation | Why needed | Simpler alternative rejected because |
|---|---|---|
| Background retry with capped backoff (Article III) — the existing reconnect loop, extended to the first connection, plus the preparation retry | Readiness waits for the first preparation, and no guest start can reach a not-Ready pod to trigger one. A libvirtd restart must not leave real mode dead (`6ed30f7`). | Retrying only on guest start deadlocks under the readiness gate. Retrying from `/readyz` makes readiness call libvirt (FR-017). No retry at all is the defect. |
| No runtime fallback to stub (Article IV) | FR-019: a real-mode deployment must not report servers it did not create. | Keeping the fallback is the fake-`ACTIVE` defect. Failing startup on an unreachable libvirt would turn a socket-activated host's ordinary boot race into a crash loop, and contradicts the clarified retry. |
