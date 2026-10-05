# Implementation Plan: a real console / noVNC (F1)

**Branch**: `feature/534` (feature id `021-real-novnc-console`) | **Date**: 2026-07-30, amended 2026-10-01 | **Spec**: [spec.md](./spec.md)
**Input**: `docs/specs/021-real-novnc-console/spec.md`. Planned 2026-07-30. Redesigned in the PR #80 review (2026-09-24: per-start unix-socket consoles, a libvirt-checked token sweep, `state_path`). Amended 2026-10-01 (seven clarifications): guest console sockets move out of libvirt's per-domain directories.

## Summary

Make Horizon's Console tab show a running instance's real framebuffer, closing **F1**, the last open finding from the #482 verification pass.

We do not write a proxy. The pinned noVNC image bundles websockify and its token plugins, and a command override turns it into exactly the token-validating relay the ticket asks for (research R5). Lightstack's part:

1. **Read the guest's console back from libvirt per request**: its unix socket path plus the domain ID of the run (`DomainLookupByUUID` + `DomainGetXMLDesc`, flags `0`), bounded at 1s.
2. **Own a token lifecycle.** A database table is the store of record. It is projected into `<state_path>/console-tokens/` as the `token: unix_socket:<path>` entries the proxy reads. Tokens are revoked on every endpoint-moving operation, swept against libvirt (a different socket or domain ID means revoke), and reaped on expiry.
3. **Return a working URL** at an operator-configured origin, in upstream's shape, and refuse honestly everywhere it cannot.
4. **Ship the proxy** in `charts/lightstack` and both deploy scripts, mounting only what it needs.

**Amendment, 2026-10-01** (review comment r3987379050). The proxy mounted libvirt's per-domain directories, which also hold every guest's QMP `monitor.sock` and `master-key.aes`, and the proxy keeps `DAC_OVERRIDE`. The changes:

- Guest sockets move to `<state_path>/console-sockets/<instance-uuid>.sock`. The directory is owned by QEMU's uid:gid, read from libvirt's capabilities, mode 0750.
- The proxy mounts only `console-tokens` and `console-sockets`, both read-only.
- With the path now fixed per instance, a restart is detected by the libvirt domain ID stored on each token.
- When the directory is not ready, guest starts are refused before libvirt is called. Domains defined before this change are refused as misconfigured.
- Uninstall purges both console directories.

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+)
**Primary Dependencies**: gin-gonic/gin, jackc/pgx/v5 + pgxpool, digitalocean/go-libvirt (`DomainGetXMLDesc`, `DomainLookupByUUID` → `Domain.ID`, `ConnectGetCapabilities`, all present in the pinned revision `v0.0.0-20260609165003-6254771e63a8`), `encoding/xml`. **No new Go modules.**
**External component**: `bonigarcia/novnc:1.3.0`, **unmodified**, pulled by digest (`sha256:a5be468d…`), with the command overridden to `python3 -m websockify --web /root/noVNC --token-plugin TokenFile --token-source <state_path>/console-tokens --idle-timeout <n> <port>`.
**Storage**: PostgreSQL, migration `065_console_auth_tokens`. It is edited in place: nothing is deployed, and test databases are recreated. This amendment adds `domain_id`. Node-local: `<state_path>/console-tokens/` (the projection) and `<state_path>/console-sockets/` (QEMU-owned, 0750, sockets only).
**Testing**:
- `go test` against a real throwaway PostgreSQL for the store.
- libvirt XML and capabilities parsing tests, and a start-guard test.
- helm-unittest for chart wiring.
- `test/console_test.sh` (stub) and `test/console_access_test.sh` (real).
- The real-mode host runbook (R1, R7 and the mount check re-run).
**Target Platform**: Linux with libvirt ≥ 2.0 and AppArmor for real mode (Ubuntu 22.04 ships libvirt 8.0, 24.04 ships 10.0). SELinux hosts are unverified. Stub mode runs everywhere, macOS included.
**Project Type**: single Go binary + Helm charts (existing layout).
**Performance Goals**: one extra libvirt round trip per console request (the lookup the readback already makes now also yields the domain ID); one capabilities read at startup. Framebuffer visible within 10s (SC-001).
**Constraints**:
- Fail-fast: ≤1s on every libvirt call made inside a request, and on the startup capabilities read (III).
- Synchronous issuance (II).
- Stub mode needs no libvirt (IV).
- Project-scoped, with an `admin` override (V).
- QEMU's 108-byte socket-path limit means `state_path` ≤ 50 characters in real mode.
**Scale/Scope**: single-node real mode; the multi-node chart deploys no proxy and still prepares `console-sockets` on every node (FR-017a).

## Constitution Check

*GATE: evaluated before Phase 0 and re-evaluated after Phase 1. Both constitutions are checked, because this repository has two (`.specify/memory/constitution.md`, which the tooling loads, and `.claude/memory/constitution.md`, which `CLAUDE.md` designates). Reconciling them remains a project-level task.*

### A. Lightstack principles (`.specify/memory/constitution.md`)

| # | Principle | Verdict | Evidence |
|---|---|---|---|
| I | Flamingo API compatibility | ✅ PASS | The three console surfaces keep upstream's shapes (contracts/nova-console-api.md). Protocol is validated, since upstream dispatches on it. The amendment adds no API shape: a refused start reuses today's answers for a hypervisor failure (202→ERROR, 500), and a pre-move domain gets the existing "console misconfigured" 500. |
| II | Synchronous operations | ✅ PASS | Issuance stays one synchronous path. The directory is prepared synchronously at startup. No queue. |
| III | Fail-fast | ✅ PASS | The readback keeps its 1s goroutine+select bound (go-libvirt calls take no context), and the startup capabilities read uses the same bound. Starts are refused immediately when the directory is not ready, rather than failing inside libvirt. |
| IV | Multi-mode | ✅ PASS | Stub mode creates no sockets and no directory; the `state_path` length check applies only in real mode. |
| V | Project isolation | ✅ PASS, strengthened | The proxy no longer reaches any guest's QMP monitor or key material (SC-013). The path is per instance, so no token names another guest's socket. |
| VI | Single binary | ⚠️ DEVIATION (unchanged) | The proxy is a separate container (Complexity Tracking). |
| — | No async workers | ⚠️ DEVIATION (unchanged) | The periodic token reconciler. The amendment only adds a domain-ID comparison to its existing sweep. |
| — | Reversible migrations | ✅ PASS | `065` down drops the table (with `domain_id`) and restores the two dropped instance columns. |
| — | Test-first | ✅ PASS | Every amendment task starts from a failing test (tasks.md). |
| — | No hardcoded credentials | ✅ PASS | Nothing added. |

### B. The Nine Articles (`.claude/memory/constitution.md`)

| Art. | Verdict | Evidence |
|---|---|---|
| I Library-First | ✅ PASS | Socket placement, capabilities parsing, directory preparation and the start guard live in `pkg/hypervisor` and `pkg/paths`; the domain-ID sweep lives in `internal/console`. Nova keeps thin handlers. |
| II CLI | ✅ PASS | `lightstack console-tokens list` gains the domain-ID column (text and `--json`), so the new state is inspectable. |
| III Test-First | ✅ PASS | RED before GREEN for each amendment task. |
| IV Integration Testing | ✅ PASS | Store tests on real PostgreSQL; the host runbook re-runs R1/R7 and the mount check. |
| V Observability | ✅ PASS | The not-ready directory, a refused start and a pre-move domain are each logged with the directory or the path named. |
| VI Versioning | ✅ PASS | `charts/lightstack` 0.3.0 is introduced by this unreleased branch, so the removed `consoleProxy.libvirtDomainDir` value needs no further bump. It is recorded in `docs/UPGRADING.md`. |
| VII Simplicity | ✅ PASS | No new package, setting or dependency. Owner taken from libvirt, not a new knob. |
| VIII Anti-Abstraction | ✅ PASS | go-libvirt used directly; no interface introduced. |
| IX Integration-First | ✅ PASS with the known caveat | `test/contract/nova` is stub-only (F15); real-mode behaviour is gated by the host runbook. |

**Post-Phase-1 re-evaluation**: no new violations. The amendment removes exposure rather than adding mechanism; the two existing deviations stand as argued below.

## Complexity Tracking

| Violation | Why needed | Simpler alternative rejected because |
|-----------|------------|--------------------------------------|
| **Principle VI**: a separate proxy container | The pinned noVNC image already contains a maintained relay (websockify) plus the client. Using it means zero relay code and no new Go dependency. It is third-party infrastructure like PostgreSQL and Horizon, not a seventh Lightstack service. | An in-process relay needs a new WebSocket dependency and vendored client assets (MPL-2.0) to reimplement a component we already ship. |
| **Forbidden Patterns**: a periodic token reconciler | websockify's token file has no expiry field, so deleting the file is the expiry mechanism. Lazy reaping would leave a token valid for hours past expiry. | Lazy-only reaping breaks FR-010. The reconciler holds no queue: it only reconciles Lightstack's projection with its own table and with libvirt (socket path and, from 2026-10-01, domain ID). |

**Consequence**: effective maximum token validity is `TTL + reconcile interval` (10m30s at defaults). After a host reboot, a token can coincide with its guest's new domain ID and survive until its TTL. It can only ever reach that same guest (spec, edge cases).

## Project Structure

### Documentation (this feature)

```text
docs/specs/021-real-novnc-console/
├── spec.md                         # 3 clarification sessions (19 answers in total)
├── plan.md                         # this file
├── research.md                     # R1–R11, plus R12–R18 for the 2026-10-01 amendment
├── data-model.md                   # console_auth_tokens incl. domain_id; console-sockets
├── quickstart.md                   # bring-up and probes, incl. the socket directory
├── contracts/
│   ├── nova-console-api.md         # HTTP surfaces + status matrix (pre-move domain row)
│   ├── console-token-projection.md # projection format; sockets under console-sockets
│   └── proxy-deployment.md         # invocation; mounts = console-tokens + console-sockets
├── HOST_TESTING.md, HOST_TESTING_REAL_MODE.md
└── tasks.md                        # updated by /cl.tasks
```

### Source code (amendment touch points marked ✱)

```text
pkg/paths/paths.go                  ✱ ConsoleSocketDir(), ConsoleSocketPath(id); TestLayout extended
pkg/hypervisor/
├── xml_template.go                 ✱ <listen type='socket' socket='<ConsoleSocketPath>'/> (was path-less)
├── console_endpoint.go             ✱ GetConsoleSocket → (socket, domainID); socket outside
│                                   #   console-sockets → ErrConsoleLegacySocket
├── console_dir.go                  ✱ NEW: capabilities → DAC baselabel (kvm, else qemu) → MkdirAll,
│                                   #   Chown, Chmod 0750; readiness recorded on the VMManager
└── libvirt.go                      ✱ start guard before every DomainCreate (CreateVM, hard
                                    #   RebootVM, StartVM, ResumeVM) → ErrConsoleSocketDirNotReady
internal/console/
├── store.go                        ✱ Issue(…, socket, domainID); RevokeStale compares both
├── cli.go                          ✱ list shows domain_id
└── reconciler.go                   #   (unchanged; CurrentSocket now returns the domain ID too)
internal/nova/
├── console.go                      ✱ pass domainID to Issue; map ErrConsoleLegacySocket → 500 misconfigured
└── handlers.go, advanced_actions.go ✱ start/reboot/resume error paths name the directory
internal/common/config.go           ✱ real mode: state_path ≤ 50 characters
internal/teardown/teardown.go       ✱ purge console-tokens + console-sockets
cmd/lightstack/main.go              ✱ prepare console-sockets right after InitHypervisor (real mode)
migrations/065_console_auth_tokens.{up,down}.sql  ✱ domain_id INTEGER NOT NULL (edited in place)

charts/lightstack/
├── templates/console-proxy.yaml    ✱ mount console-sockets (RO) instead of libvirt's domain dir;
│                                   #   console-tokens only (done, comment 4); comments per FR-017b
├── templates/_helpers.tpl          ✱ initContainer subdirs + console-sockets
├── values.yaml / values.schema.json ✱ drop consoleProxy.libvirtDomainDir
└── tests/                          ✱ mounts (2 volumes, both RO), initContainer list, schema
scripts/deploy-turnkey.sh, scripts/deploy-single-node.sh  ✱ novnc mounts console-tokens +
                                    #   console-sockets only (were all of $STORAGE_DIR + /var/lib/libvirt/qemu)
deployments/docker-compose-kvm.yml  ✱ mount /var/lib/lightstack/console-sockets (FR-017a)
deployments/README-KVM.md, docs/SINGLE_NODE_DEPLOYMENT.md  ✱ mirror the above
docs/UPGRADING.md                   ✱ pre-move domains; 065 changed; value removed; state_path ≤ 50
docs/specs/021-real-novnc-console/HOST_TESTING_REAL_MODE.md  ✱ R1, R7, mount check, virsh-start check
```

**Structure decision**: no new package. The amendment extends `pkg/paths` and `pkg/hypervisor` (placement, capabilities, start guard) and `internal/console` (the domain-ID sweep). Everything else is wiring: config, teardown, chart, scripts and docs.

## Phases

### Phase 0: Research ✅

[research.md](./research.md) R12–R18 hold the amendment's facts, each checked against source:
- explicit socket support (libvirt ≥ 2.0);
- `virt-aa-helper` adds the explicit path `rw`;
- QEMU unlinks before bind and removes the socket on an orderly close;
- the 108-byte limit;
- libvirt never prepares a socket directory the domain names;
- the DAC base label format;
- domain IDs are new on every start and continue from the highest after a libvirtd restart;
- the `DomainCreate` sites in the code.

Nothing remains NEEDS CLARIFICATION.

### Phase 1: Design & contracts ✅

[data-model.md](./data-model.md) (domain_id, console-sockets), [contracts/](./contracts/) (mounts, projection paths, the pre-move refusal), [quickstart.md](./quickstart.md) (socket-directory probes).

### Phase 2: Tasks

`/cl.tasks` appends the amendment's tasks. Suggested order:
1. `pkg/paths`
2. XML template
3. capabilities and directory preparation
4. start guard
5. readback with domain ID and the pre-move check
6. migration and store
7. config limit
8. teardown
9. chart
10. scripts and KVM compose
11. docs and UPGRADING
12. host pass

Each step is test-first.

## Risks

| Risk | Mitigation |
|---|---|
| **Every VM start fails** if `console-sockets` is missing, unmounted or wrongly owned. libvirt does not prepare it (R16). | Prepare it right after the libvirt connection; refuse starts with a clear error until it is ready (FR-017a). Every real-mode artifact mounts it, the KVM compose included. Covered by unit tests and the host pass. |
| The capabilities report no DAC base label (an unusual libvirt configuration). | Directory stays not-ready; starts refused, naming the cause. Fall back from `type='kvm'` to `type='qemu'`. |
| A path over 108 bytes from a long `state_path`. | Refused at startup in real mode (≤ 50 characters). |
| SELinux hosts. | Out of the supported set; recorded in the spec's assumptions. |
| Domains defined before the move on test hosts. | Refused as misconfigured; `docs/UPGRADING.md` gives the fix. |
| The proxy cannot read tokens (UID/mode). | Unchanged from the original plan: asserted by chart tests; the quickstart's direct `?token=` probe isolates it. |
| No real-mode contract gate (F15). | The host runbook is the gate. |
