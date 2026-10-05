---
description: "Task list for SPEC-021 — a real console / noVNC (F1)"
---

# Tasks: a real console / noVNC (F1)

**Input**: Design documents from `docs/specs/021-real-novnc-console/`
**Prerequisites**: spec.md (35 live FRs, 12 clarifications; amended 2026-10-01 with FR-017a, FR-017b, SC-013 and 7 more answers), plan.md, research.md (R1–R18), data-model.md, contracts/ (3), quickstart.md

**Tests**: **INCLUDED** — Test-First is non-negotiable under both constitutions (Lightstack workflow section; Article III). Failing tests precede their implementation task in every phase.

**Traceability**: every task names the requirement(s) it satisfies. Requirements with no task, and tasks with no requirement, are both defects — the analysis pass found two uncovered requirements precisely because prose-only mapping hid them.

**Organization**: grouped by user story. See *Implementation Strategy* for the one place the spec overrides "US1 alone is shippable".

## Format: `[ID] [P?] [Story] Description`

- **[P]**: parallelizable — different files, no dependency on an incomplete task
- **[Story]**: US1–US4 from spec.md

## Path Conventions

Single Go binary + Helm charts, existing layout. Paths are repository-root-relative.

---

## Phase 1: Setup

- [X] T001 Re-verify the pinned proxy image contract and record its digest in `docs/specs/021-real-novnc-console/contracts/proxy-deployment.md`: `bonigarcia/novnc:1.3.0` still ships `token_plugins.py` with `TokenFile`, serves `vnc_lite.html` + `vnc.html`, has **no** `vnc_auto.html`, offers `--idle-timeout` and `--host-token`, and lacks `requests` / `redis` / `jwcrypto` — *(FR-023a)*
- [X] T002 [P] Add console settings to `NovaConfig` in `internal/common/config.go`: `console_proxy_base_url` (origin only), `console_token_ttl` (default `10m`), `console_reconcile_interval` (default `30s`) — struct fields, yaml tags and defaults only, no consumers yet — *(FR-010, FR-020)*

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: the endpoint readback, the token-store library, and its CLI. **No user story functions without these.**

- [X] T003 Create `migrations/065_console_auth_tokens.up.sql` and `.down.sql` per data-model.md: create `console_auth_tokens` (`token` PK, `instance_id` FK `ON DELETE CASCADE`, `project_id`, `host`, `port`, `created_at`, `expires_at` — **no `console_type`**, which was future-proofing for a type FR-025 refuses) with indexes on `(instance_id)` and `(expires_at)`; drop `instances.console_vnc_port` and `console_vnc_password`; down drops the table and restores both columns as nullable — *(FR-009, FR-014, FR-016)*
- [X] T004 [P] Add `ConsoleTokenDir()` returning `filepath.Join(Base, "console-tokens")` to `pkg/paths/paths.go`, beside `ImagesDir` / `VolumesDir` / `InstancesDir` / `CloudInitDir` / `DHCPDir` — *(FR-023b)*
- [X] T005 [P] Extend `TestLayout` in `pkg/paths/paths_test.go` to assert `ConsoleTokenDir() == "/var/lib/lightstack/console-tokens"` — *(FR-023b)*
- [X] T006 [P] Write failing graphics-XML parse tests in `pkg/hypervisor/libvirt_test.go`: resolved port, `port='-1'` (autoport unresolved), missing `<graphics>`, non-vnc graphics type, malformed XML — *(FR-003, FR-004)*
- [X] T007 Implement `GetConsoleEndpoint(ctx, vmUUID) (host string, port int, err error)` in `pkg/hypervisor/libvirt.go` via `DomainLookupByUUID` + `DomainGetXMLDesc(dom, 0)` — **flags `0`, not `DomainXMLInactive`**, or the port returns `-1` and reproduces the original bug; stub-mode branch returns a not-available error — *(FR-003, FR-004)*
- [X] T008 [P] Write a failing test in `pkg/hypervisor/libvirt_test.go` asserting the readback abandons a hung hypervisor within **1 second** — *(FR-007)*
- [X] T009 Bound the readback in `pkg/hypervisor/libvirt.go` with an explicit 1s deadline: go-libvirt's generated calls take **no `context`**, and the dialer's `WithLocalTimeout` applies only at connect, so run lookup+readback in a goroutine and `select` on a 1s `context.WithTimeout` — *(FR-007)*
- [X] T010 [P] Write failing token-store tests in `internal/console/store_test.go` **against a real throwaway PostgreSQL** (Article IV — real databases over mocks, the pattern established by #533): issue→resolve round trip, projection file is exactly `<token>: <host>:<port>`, issuance fails closed when the directory is unwritable — *(FR-008, FR-009, FR-023b)*
- [X] T011 Create the self-contained token-store library `internal/console` (Article I — following the `internal/teardown` and `internal/health` precedent, so it compiles into the one binary while staying independently testable): `store.go` with `Issue` (insert row **and** write the projection file, failing closed if the file cannot be written), `Resolve`, `RevokeByInstance`, `RevokeExpired`; `projection.go` for the file writer. `internal/nova` calls into this package and holds no token logic — *(FR-008, FR-009, FR-014, FR-016, FR-023b, Article I)*
- [X] T012 [P] Write failing CLI tests in `internal/console/cli_test.go`: `list` prints one line per live token and supports `--json`, `purge` removes expired entries (and by-instance when given one), usage errors exit `2` writing to stderr — *(Article II, Article V)*
- [X] T013 Add `internal/console/cli.go` and dispatch **`lightstack console-tokens list|purge`** from `cmd/lightstack/main.go`, beside the existing `cleanup` (line 48) and `healthcheck` (line 55) subcommands and following their conventions: text to stdout, `--json` for structured data, errors to stderr, documented exit codes. This is the operator interface to the token store, replacing hand-inspection with `ls`/`cat` — *(Article II, Article V, FR-028)*

**Checkpoint**: `go test ./pkg/hypervisor/... ./pkg/paths/... ./internal/console/...` green; migration applies **and** reverts; `lightstack console-tokens list` runs.

---

## Phase 3: User Story 1 — See and use a running instance's console (Priority: P1)

**Goal**: Horizon's Console tab renders a running instance's live framebuffer and accepts input.

**Independent test**: boot an instance in real mode, open its Console tab, see the cirros login prompt, type into it.

- [X] T014 [P] [US1] Write failing assertions in `pkg/hypervisor/xml_template_test.go`: graphics carries `listen='127.0.0.1'`, `sharePolicy='force-shared'`, and retains `autoport='yes'` — *(FR-017, FR-018a)*
- [X] T015 [US1] Change the graphics element in `pkg/hypervisor/xml_template.go` (lines 163-168) to `listen='127.0.0.1'` with a matching `<listen type='address' address='127.0.0.1'/>` and `sharePolicy='force-shared'` (valid values are `allow-exclusive` | `force-shared` | `ignore`) — *(FR-017, FR-018, FR-018a)*
- [X] T016 [P] [US1] Write failing HTTP-level contract tests in `internal/nova/console_test.go` for surfaces 1 and 2 of contracts/nova-console-api.md: `200` body shape, `url` matching `<origin>/vnc_lite.html?path=%3Ftoken%3D<hex>`, and both surfaces returning an equivalent URL for one instance — *(FR-001, FR-002, FR-005)*
- [X] T017 [US1] Rewrite the noVNC path of `GetRemoteConsole` in `internal/nova/console.go` as a thin handler: read the endpoint via `GetConsoleEndpoint` (T007), issue a token via `internal/console` (T011), return the URL — and delete `setupVNCConsole`, `generateConsoleToken`, `generateVNCPassword` and `hashString` (lines 171-222) — *(FR-001, FR-003, FR-004)*
- [X] T018 [US1] Route the legacy `os-getVNCConsole` action through the same code path in `internal/nova/console.go` (`GetVNCConsole`, `GetVNCConsoleAction`, `getVNCConsoleResponse`) so the two surfaces cannot diverge — *(FR-005)*
- [X] T019 [US1] Implement origin resolution in `internal/nova/console.go`: use `console_proxy_base_url` when set, else derive from `internal/common/baseurl.go`; the client page and `path=%3Ftoken%3D` query are appended in code and never configurable — *(FR-020, FR-021)*
- [X] T020 [US1] Add the stub-mode branch returning `501` naming stub mode, so stub never surfaces a raw `500` from T007's not-available error between this phase and Phase 6 — *(FR-027)*
- [X] T021 [P] [US1] Write helm-unittest specs under `charts/lightstack/tests/` asserting all nine items in contracts/proxy-deployment.md: the websockify command override (not the image entrypoint), `hostNetwork: true`, required `podAffinity` to the Lightstack pod, token-dir mount with a readable mode/UID, `console-tokens` in the initContainer list, `TokenFile` (not `ReadOnlyTokenFile`), a non-zero `--idle-timeout`, **absence of `--host-token`**, and a bumped chart version — *(FR-012, FR-019, FR-023a, FR-023c, FR-023d, Article VI)*
- [X] T022 [US1] Add `console-tokens` to the initContainer subdirectory list in `charts/lightstack/templates/_helpers.tpl` (lines 177-193) — must precede T023, which mounts it — *(FR-023b)*
- [X] T023 [US1] Add `charts/lightstack/templates/console-proxy.yaml`: real-mode-gated Deployment + Service running `python3 -m websockify --web /root/noVNC --token-plugin TokenFile --token-source /var/lib/lightstack/console-tokens --idle-timeout 300 6080`, with `hostNetwork: true`, required `podAffinity` to the Lightstack pod (`topologyKey: kubernetes.io/hostname`), a hostPath mount of `realMode.hostStoragePath`, and **no `--host-token`** — *(FR-012, FR-019, FR-023a, FR-023c, FR-023d)*
- [X] T024 [US1] Rewrite `test/console_test.sh` to assert a **resolvable** console URL — the current version asserts only that some URL string came back, which is why it passes against the broken console — *(FR-001)*

**Checkpoint**: framebuffer visible and interactive in the real harness.

---

## Phase 4: User Story 2 — Console access is scoped, time-bounded, unforgeable (Priority: P2)

**Goal**: a link opens exactly one instance, for an entitled caller, for a bounded window.

**Independent test**: request a console for instance A, then try B's console with A's link, a link past its lifetime, and an invented link. All refused; the legitimate one works.

- [X] T025 [P] [US2] Write failing tests in `internal/console/store_test.go` (real throwaway PostgreSQL): expired token refused, unknown token refused, a token for instance A cannot resolve to B, cross-project issuance yields `404`, an `admin` token may issue for any project, tokens gone after delete / reboot / suspend — *(FR-010, FR-012, FR-013, FR-014)*
- [X] T026 [P] [US2] Write a failing port-reuse test in `internal/console/store_test.go`: a token issued for A, then A stopped and its console port reassigned to B, must not resolve — the cross-tenant leak this story exists to close — *(FR-014)*
- [X] T027 [US2] Enforce expiry in `internal/console/store.go`: set `expires_at` from `console_token_ttl` at issue, refuse expired tokens in `Resolve`, and opportunistically reap that instance's expired entries on each issue — *(FR-010)*
- [X] T028 [US2] Add `internal/console/reconciler.go` — a periodic sweep at `console_reconcile_interval` plus one sweep at startup — and wire it into `cmd/lightstack/main.go` beside the existing server bootstrap. This is what actually enforces expiry: the relay performs no expiry check, so an unreaped file stays live — *(FR-010, FR-016)*
- [X] T029 [US2] Revoke on lifecycle transitions in `internal/nova/handlers.go`: `DeleteServer` (line 1221) and the `reboot` / `os-stop` / `os-start` action branches (lines 1548-1610) — *(FR-014)*
- [X] T030 [P] [US2] Revoke on suspend and resume in `internal/nova/advanced_actions.go` (`SuspendInstance` line 20, `ResumeInstance` line 88) — *(FR-014)*
- [X] T031 [US2] Enforce issuance authorization in `internal/nova/console.go`: project-scoped by default with an `admin` override via the shared `common.HasRole`, returning `404` (never revealing existence) for another project's instance — *(FR-013)*
- [X] T032 [US2] Implement `GET /v2.1/os-console-auth-tokens/{token}` in `internal/nova/console.go` per contracts/nova-console-api.md and register the route in `internal/nova/handlers.go` — compatibility surface only; the console must not depend on it — *(FR-015)*
- [X] T033 [P] [US2] Rewrite `test/console_access_test.sh` as the security probe covering all five SC-005 refusal cases — *(FR-011, FR-012, FR-013, SC-005)*
- [X] T034 [P] [US2] Add a restart-survival probe to `test/console_access_test.sh`: a link issued before a Lightstack restart still works after it, within its window — the sole justification for persisting tokens, and previously untested. In the same probe, assert the link resolves **twice** within its window, so reconnects and page refreshes are proven rather than assumed — *(FR-009, FR-010a, SC-007)*

**Checkpoint**: all five unauthorized-access attempts fail closed; a legitimate link survives a restart.

---

## Phase 5: User Story 3 — The operator decides where the console lives (Priority: P3)

**Goal**: the configured origin is what users get, in every documented topology.

**Independent test**: set the console origin to the deployment's external host, request a URL, confirm it carries that host and works from another machine.

- [X] T035 [US3] Add the nova console block to `config/lightstack.yaml` (near lines 35-39): `console_proxy_base_url`, `console_token_ttl`, `console_reconcile_interval` — *(FR-020, FR-022)*
- [X] T036 [US3] Add `consoleProxy.{enabled,image.{repository,tag,pullPolicy},service.{type,port,nodePort},idleTimeout,resources}` and `nova.consoleProxyBaseUrl` / `nova.consoleTokenTtl` / `nova.consoleReconcileInterval` to `charts/lightstack/values.yaml`, render them in `charts/lightstack/templates/_helpers.tpl` (lines 115-118), and **extend `charts/lightstack/values.schema.json`** — it sets `additionalProperties: false`, so new values fail validation until the schema changes in the same commit. Value names must match quickstart.md exactly — *(FR-020, FR-023a)*
- [X] T037 [US3] Bump the chart version in `charts/lightstack/Chart.yaml` (currently `0.2.0`) — a new chart component is a minor change under semantic versioning, and the chart is explicitly "bumped independently of the product" — *(Article VI)*
- [X] T038 [US3] Relocate `deployments/horizon-chart/` to `charts/horizon/`: rewrite `Chart.yaml` to drop the "verification infrastructure — NOT a product chart" claim and set a deliberate `version` and `appVersion`, and delete `templates/novnc.yaml` plus the `novnc` block in `values.yaml` (lines 60-70), both superseded by the product-chart proxy — must follow T023, or the harness has no console in between — *(FR-023e, Article VI)*
- [X] T039 [US3] Update the chart paths in `Makefile`: the lint/template targets (lines 105-107) and the `horizon-chart-sync` target (lines 116-122) — *(FR-023e)*
- [X] T040 [US3] Remove the `novnc` service from `deployments/docker-compose-horizon.yml` (lines 104-125) — stub mode refuses at request time, so the compose stack needs no proxy — *(FR-023a, FR-027)*
- [X] T041 [P] [US3] Make `console_proxy_base_url` real in the guides that already instruct operators to set it, and drop the `/vnc_auto.html` suffix from every example (that page 404s on noVNC 1.6.0): `docs/SINGLE_NODE_DEPLOYMENT.md` (lines 286, 917), `docs/SCALING.md` (line 618), `docs/HORIZON_INTEGRATION.md` (lines 250, 391), `deployments/README-KVM.md` (line 43), `docs/HORIZON_SETUP.md` (line 305) — *(FR-022)*
- [X] T042 [P] [US3] Document the console in the deployment guides: token lifetime, the reconcile interval and its effect on effective validity (`TTL + interval`), the idle timeout, the exposed proxy port, the shared token directory, the **`lightstack console-tokens`** subcommand as the supported way to inspect and purge tokens, and that **multi-node console is unsupported in v1** — *(FR-022, Article II)*

**Checkpoint**: a URL issued on the cluster opens from another machine; no `localhost` appears unless configured.

---

## Phase 6: User Story 4 — Honest answers for the consoles Lightstack does not serve (Priority: P4)

**Goal**: refusals instead of plausible URLs that lead nowhere.

**Independent test**: request each unsupported console type and get a clear refusal; request the serial log and still get guest output.

- [X] T043 [P] [US4] Write failing tests in `internal/nova/console_test.go` for the full status matrix in contracts/nova-console-api.md: `400` unsupported type, `409` not running / no graphical console, `501` stub mode, `404` absent or cross-project, `404` unknown token — *(FR-006, FR-025, FR-027)*
- [X] T044 [US4] Replace the fabricated URLs for unsupported types in `internal/nova/console.go` with `400` refusals naming the type — `xvpvnc` and `serial` in `GetRemoteConsole` (lines 72-77), plus `GetSerialConsoleAction`, `GetSPICEConsoleAction` and `GetRDPConsoleAction` (lines 276-384) — *(FR-025)*
- [X] T045 [US4] Refuse with `409` when the instance is not running or has no graphical console, in `internal/nova/console.go` — *(FR-006)*
- [X] T046 [P] [US4] Add a regression test asserting `os-getConsoleOutput` (`internal/nova/console.go` line 225) still returns the captured serial log unchanged — the documented fallback — *(FR-026)*
- [X] T047 [P] [US4] Add stub-mode assertions to `test/console_test.sh`: `501` and no URL returned, exercising the branch added in T020 — *(FR-027)*

**Checkpoint**: no endpoint returns a URL it cannot honour; the serial log is unregressed.

---

## Phase 7: Polish & Cross-Cutting Concerns

- [X] T048 Add structured logging for issued / validated / refused-with-reason / closed outcomes in `internal/nova/console.go` and `internal/console/store.go`, without recording token values in reusable form — *(FR-028, Article V)*
- [X] T049 [P] Generate `docs/specs/021-real-novnc-console/HOST_TESTING.md` (stub) and `HOST_TESTING_REAL_MODE.md` (real) from quickstart.md, covering SC-001 through SC-012 including the interactive-login and two-viewer checks, plus a note that the SPEC-018 runbooks still reference the chart's old path — *(FR-002, FR-018, SC-001, SC-002, SC-008)*
- [X] T050 [P] Update `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`: record F1 as fixed with evidence, and correct the headline lines still calling it the only open finding (12, 36, 50, 378, 414) — *(FR-030, SC-012)*
- [X] T051 Run the gates: `make fmt`, `make lint`, `make test`, `make chart-test`, migration up **and** down, and confirm the binary size is unchanged (no new Go dependency) — *(FR-029)*
      <br>*Done 2026-10-01 with T089, on the amended branch: `make fmt` no change, `make lint` 0 issues, `make test` green against a real Postgres (0 skips), `make chart-test` 112/112 + `charts/horizon` 3/3, migrations 71 → 64 → 71 (065 down restores the legacy columns), binary size unchanged, `go.mod`/`go.sum` untouched.*
- [X] T052 [P] Open the follow-up ticket for `charts/horizon` production hardening: values schema, ingress/TLS, secrets for the dashboard signing key and memcached, and the `local_settings` duplication the sync target papers over — *(out of scope, recorded)*
- [X] T053 [P] Open a follow-up ticket to reconcile the **two constitutions** (`.specify/memory/constitution.md` vs `.claude/memory/constitution.md`) — duplicate authoritative documents already caused one CRITICAL gate finding on this feature, and the tooling loads a different one than `CLAUDE.md` designates — *(project-level, not this feature)*
- [ ] T054 Branch review before the PR — *(quality gate: both constitutions' before-merge checklists)*

---

# Amendment 2026-10-01: guest sockets out of libvirt's per-domain directories

**Input**: spec.md, Session 2026-10-01 (7 answers; FR-009, FR-014a, FR-017, FR-017a, FR-017b, FR-023c, SC-005, SC-013), plan.md (amended), research.md R12–R18, data-model.md (`domain_id`, console socket directory), contracts/ (mounts, projection paths, pre-move refusal), quickstart.md.

**Why**: review comment r3987379050. The proxy mounted libvirt's per-domain directories, which also hold every guest's QMP `monitor.sock` and `master-key.aes`, and it keeps `DAC_OVERRIDE`. Test-first as above: each implementation task follows its failing test.

## Phase 8: Amendment foundations (blocking for Phases 9–11)

- [X] T055 Narrow the proxy's storage-root mount to `<state_path>/console-tokens` (hostPath `DirectoryOrCreate`, read-only) in `charts/lightstack/templates/console-proxy.yaml`; assert mount path, hostPath, type and a two-volume total in `charts/lightstack/tests/console_proxy_test.yaml`; fix the sentence in `docs/specs/021-real-novnc-console/HOST_TESTING_REAL_MODE.md` that described the old mount. RED→GREEN shown 2026-10-01, uncommitted — *(FR-023c, SC-013)*
- [X] T056 [P] Write failing assertions in `pkg/paths/paths_test.go` (`TestLayout`): `ConsoleSocketDir() == <base>/console-sockets` and `ConsoleSocketPath(id) == <base>/console-sockets/<id>.sock` — *(FR-017)*
- [X] T057 Add `ConsoleSocketDir()` and `ConsoleSocketPath(instanceID string)` to `pkg/paths/paths.go`, beside `ConsoleTokenDir()` — *(FR-017)*
- [X] T058 [P] Write failing tests in `internal/common/config_test.go`: with `nova.libvirt_mode: real`, a `state_path` of 51 characters is refused, with an error naming the 50-character limit and QEMU's 108-byte socket path; exactly 50 is accepted; stub mode accepts 51 — *(FR-017, research R13)*
- [X] T059 Implement the real-mode `state_path` length check in `internal/common/config.go` (next to `resolveStatePath`, once the libvirt mode is known) — *(FR-017)*
- [X] T060 [P] Write failing parse tests in `pkg/hypervisor/console_dir_test.go` (new): libvirt capabilities XML → QEMU's uid:gid from `<secmodel><model>dac</model><baselabel type='kvm'>+UID:+GID</baselabel>`, falling back to `type='qemu'`. A missing DAC secmodel, a missing label or a malformed `+u:+g` returns an error. Use libvirt's own example (`+77:+77`) as one fixture — *(FR-017a, research R15)*
- [X] T061 [P] Write failing tests in `pkg/hypervisor/console_dir_test.go`: `PrepareConsoleSocketDir` creates the directory with mode `0750` and the parsed owner (use the test process's own uid:gid, so no root is needed). An existing `0755` directory is corrected. On failure the manager reports not-ready with the cause — *(FR-017a)*
- [X] T062 Implement `pkg/hypervisor/console_dir.go`: read `ConnectGetCapabilities` under the same 1s goroutine+select bound as the readback, parse the DAC label, `MkdirAll`/`Chown`/`Chmod 0750`, and record readiness and the cause on the `VMManager` — *(FR-017a, research R15, Constitution III)*
- [X] T063 [P] Write failing tests in `pkg/hypervisor/libvirt_test.go` (libvirttest mock): while the directory is not ready, `CreateVM`, hard `RebootVM`, `StartVM` and `ResumeVM` return `ErrConsoleSocketDirNotReady` naming the directory, and no `DomainCreate` reaches libvirt — *(FR-017a, research R16)*
- [X] T064 Add the start guard before every `DomainCreate` in `pkg/hypervisor/libvirt.go` (`CreateVM` ~line 212, hard `RebootVM` ~371, `StartVM` ~465, `ResumeVM` ~703), returning the typed `ErrConsoleSocketDirNotReady` — *(FR-017a)*
- [X] T065 Call `PrepareConsoleSocketDir(paths.ConsoleSocketDir())` in real mode right after `InitHypervisor` succeeds in `cmd/lightstack/main.go`. Log the cause on failure and keep serving, because the binary runs every service — *(FR-017a)*
- [X] T066 [P] Write failing tests in `internal/nova/start_refusal_test.go`: `startRefusal` turns `ErrConsoleSocketDirNotReady` into a `500` naming the directory and keeps the generic message for any other error; `Service.PrepareConsoleSocketDir` with no hypervisor returns `errNoHypervisor`. Resume and create run after their 202 and already leave the instance `ERROR` — *(FR-017a)*
- [X] T067 Route the `os-start` and reboot error paths in `internal/nova/handlers.go` through `startRefusal`, so a not-ready directory is named in the `500`. In `internal/nova/advanced_actions.go`, log the cause of a failed background resume, which before marked the instance ERROR silently. The create path's ERROR handling already logs it — *(FR-017a)*
- [X] T068 Edit `migrations/065_console_auth_tokens.up.sql` in place: add `domain_id INTEGER NOT NULL CHECK (domain_id > 0)` to `console_auth_tokens`. Down still drops the table. Nothing is deployed; test databases are recreated — *(FR-009, FR-014a)*

**Checkpoint**: `go test ./pkg/paths/... ./pkg/hypervisor/... ./internal/common/... ./internal/nova/...` green. The migration applies and reverts on a fresh database.

## Phase 9: US1 (amended): the console works from the new socket location

- [X] T069 [P] [US1] Write failing assertions in `pkg/hypervisor/xml_template_test.go`: graphics is `<listen type='socket' socket='<ConsoleSocketPath(spec.UUID)>'/>`, still `sharePolicy='force-shared'`, and has no `port=` and no address listen — *(FR-017, FR-018a)*
- [X] T070 [US1] Render the explicit socket path in `GenerateVMXML` in `pkg/hypervisor/xml_template.go` (the graphics block ~line 179), derived from `spec.UUID` via `paths.ConsoleSocketPath`, and rewrite the comment above it: a fixed per-instance path, the run tracked by the domain ID — *(FR-017)*
- [X] T071 [P] [US1] Write failing tests in `pkg/hypervisor/console_endpoint_test.go`: `GetConsoleSocket` returns the socket and the domain ID from `DomainLookupByUUID`; a socket outside `ConsoleSocketDir()` returns `ErrConsoleLegacySocket` — *(FR-003, FR-014a, FR-017)*
- [X] T072 [US1] Change `GetConsoleSocket` in `pkg/hypervisor/console_endpoint.go` to return `(socket string, domainID int32, err error)`, refusing sockets outside `paths.ConsoleSocketDir()` with the new `ErrConsoleLegacySocket`. Update its doc comment, which still describes per-start paths — *(FR-003, FR-017)*

**Checkpoint**: a new domain's XML names its socket in `console-sockets`; the readback returns it with the domain ID.

## Phase 10: US2 (amended): restarts revoke, nothing outlives its purpose

- [X] T073 [P] [US2] Write failing tests in `internal/console/store_test.go` (real throwaway PostgreSQL): `Issue` stores `domain_id`. `RevokeStale` revokes a token whose guest serves the **same** socket under a **different** domain ID, keeps one whose socket and ID both match, and keeps tokens on a readback error — *(FR-014a)*
- [X] T074 [US2] In `internal/console/store.go`, make `Issue` take and persist the domain ID, make `CurrentSocket` return `(socket, domainID, err)`, and have `RevokeStale` compare both. Update the `RevokeByInstance`/`RevokeStale` doc comments that still argue from per-start paths, and keep the 2026-10-01 note on the issue/revoke window — *(FR-009, FR-014a)*
- [X] T075 [P] [US2] Write a failing test in `internal/console/cli_test.go`: `console-tokens list` shows each token's domain ID, in text and in `--json` — *(Article II, Article V)*
- [X] T076 [US2] Add the domain ID to `console-tokens list` in `internal/console/cli.go` — *(Article II, Article V)*
- [X] T077 [P] [US2] Write failing tests in `internal/nova/console_test.go`: a pre-move socket answers `500` "console misconfigured on this host"; issuance passes the readback's domain ID to the store; `CurrentConsoleSocket` returns it — *(FR-014a, FR-017)*
- [X] T078 [US2] Wire `internal/nova/console.go`: pass the domain ID from `GetConsoleSocket` to `Issue`, return it from `CurrentConsoleSocket`/`socketForSweep`, and map `ErrConsoleLegacySocket` like `ErrConsoleNotSocket` in `sendConsoleEndpointError`, logging the `docs/UPGRADING.md` pointer. Refresh the `revokeConsoleTokens` doc comment — *(FR-014a, FR-017)*
- [X] T079 [P] [US2] Write a failing test in `internal/teardown/teardown_test.go`: `purgeLocalStorage` removes `console-tokens` and `console-sockets` alongside the existing five trees — *(FR-017b)*
- [X] T080 [US2] Add `paths.ConsoleTokenDir()` and `paths.ConsoleSocketDir()` to `purgeLocalStorage` in `internal/teardown/teardown.go`, and update its doc comment — *(FR-017b)*
- [X] T081 [P] [US2] Extend `test/console_access_test.sh` (real mode): after `virsh destroy` + `virsh start` (outside the API), the pre-restart token is gone within one reconcile interval, and a fresh one works — *(FR-014a, SC-005)*

**Checkpoint**: a restart with or without the API revokes within one interval; uninstall leaves no console state.

## Phase 11: US3 (amended): every deployment artifact mounts only what it needs

- [X] T082 [P] [US3] Write failing chart tests:
  - `charts/lightstack/tests/console_proxy_test.yaml`: the proxy mounts `<root>/console-sockets` read-only (`DirectoryOrCreate`) at the same path; exactly two volumes; none under `/var/lib/libvirt`.
  - `charts/lightstack/tests/schema_test.yaml`: `consoleProxy.libvirtDomainDir` is now rejected.
  - `charts/lightstack/tests/init_host_storage_test.yaml`: the initContainer creates `console-sockets`.

  *(FR-017a, FR-023c, SC-013)*
- [X] T083 [US3] Make the chart changes:
  - `charts/lightstack/templates/console-proxy.yaml`: replace the `libvirt-domains` mount and volume with `console-sockets`; rewrite the `console-tokens` mount comment for FR-017b (only the uninstall purge removes it, and the proxy goes in the same uninstall).
  - `charts/lightstack/templates/_helpers.tpl`: add `console-sockets` to the initContainer's subdirectory list.
  - `charts/lightstack/values.yaml` and `values.schema.json`: drop `consoleProxy.libvirtDomainDir`.

  *(FR-017, FR-017a, FR-017b, FR-023c)*
- [X] T084 [P] [US3] Narrow the `novnc` service mounts to `$STORAGE_DIR/console-tokens` and `$STORAGE_DIR/console-sockets` (read-only, `DAC_OVERRIDE` kept) in `scripts/deploy-turnkey.sh` (~lines 784, 788) and `scripts/deploy-single-node.sh` (~695, 699), and mirror the snippet in `docs/SINGLE_NODE_DEPLOYMENT.md` — *(FR-023c, SC-013)*
- [X] T085 [P] [US3] Add the `/var/lib/lightstack/console-sockets:/var/lib/lightstack/console-sockets` mount to `deployments/docker-compose-kvm.yml` and a line in `deployments/README-KVM.md`'s Architecture list — *(FR-017a)*
- [X] T086 [P] [US3] Add a `docs/UPGRADING.md` entry:
  - domains defined before this change are refused as misconfigured (recreate the instance or redefine its domain);
  - migration `065` changed in place (recreate test databases);
  - `consoleProxy.libvirtDomainDir` is removed;
  - `state_path` is at most 50 characters in real mode.

  *(FR-017)*

**Checkpoint**: `make chart-test` green; `bash -n` on both scripts; no artifact mounts anything under `/var/lib/libvirt` into the proxy.

## Phase 12: Amendment polish & verification

- [X] T087 [P] Update `docs/specs/021-real-novnc-console/HOST_TESTING_REAL_MODE.md`:
  - R1: socket path in `console-sockets`, directory `libvirt-qemu:kvm 750`;
  - R7: the path stays the same, the domain ID changes, and the old token is revoked by the API action or within one interval for `virsh start`;
  - a mount check: the proxy has exactly `console-tokens` and `console-sockets`;
  - `state_path` over 50 characters is refused.

  Also update `HOST_TESTING.md` wherever a stub step names libvirt's domain directory. *(SC-005, SC-013)*
- [X] T088 [P] Update the saved reply to review comment r3987379050 (cl.fix-pr scratch, `/tmp/cl-fix-pr-cobaltcore-dev-lightstack-80.md` and its `~` copy) to describe what was done: tokens-only mount, sockets moved out of libvirt's directories, no follow-up ticket. Drop the `<ticket>` placeholder — *(review)*
- [X] T089 Run the gates from the `Makefile`: `make fmt`, `make lint`, `make test` with the test database, `make chart-test`, migration `065` up **and** down, `bash -n` on both deploy scripts, and the binary-size check (no new module) — *(FR-029)*
- [ ] T090 Real-mode host pass on k3s per `docs/specs/021-real-novnc-console/HOST_TESTING_REAL_MODE.md`: R1, R7, the mount check, quickstart security check 5b (`virsh destroy`/`virsh start`), and a guest boot proving QEMU can create its socket under AppArmor — *(SC-001, SC-005, SC-013, FR-017a)*
- [ ] T091 Stub-mode host pass per `docs/specs/021-real-novnc-console/HOST_TESTING.md`: S1–S11 unchanged (stub creates no sockets), and `state_path` length is not enforced in stub mode — *(FR-027, FR-017)*
- [X] T092 Refuse a stopped domain in `GetConsoleSocket` (`pkg/hypervisor/console_endpoint.go`). The fixed path puts the socket in the persistent definition too, which is what libvirt returns for a domain that is not running, so the ID (−1) is the signal: `ErrConsoleNotReady`, 409. Before, the readback answered `(path, -1)` and the insert failed the `domain_id > 0` constraint (500). Test: a `stopped` fake domain in `console_endpoint_test.go` (RED `err = <nil>`, then GREEN). Found while writing T087; research R14 — *(FR-006, FR-014a)*
- [X] T093 Remove the remaining per-start wording. Code comments: `internal/console/store.go`, `internal/nova/console.go`, `charts/lightstack/templates/{console-proxy.yaml,_helpers.tpl}` (the proxy comment still named the removed `libvirt-domains` mount), `test/console_access_test.sh`, and the console tests. Docs: `docs/blueprint.md`, `docs/REAL_LIBVIRT_MODE.md`, `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` and the `internal_access_path` example in `contracts/nova-console-api.md`. Every line came from this branch (`18b8917`) — *(FR-017)*

---

## Dependencies

```text
Phase 1 (Setup: T001-T002)
        │
        ▼
Phase 2 (Foundational: T003-T013)  ◄── blocks every story
        │
        ├──────────────► Phase 3  US1 (T014-T024)   MVP demo
        │                    │
        │                    ├────► Phase 4  US2 (T025-T034)  ships with US1
        │                    │
        │                    └────► Phase 5  US3 (T035-T042)  T038 needs T023
        │
        └──────────────► Phase 6  US4 (T043-T047)   independent
                             │
                             ▼
                        Phase 7  Polish (T048-T054)
```

**Hard ordering inside phases**

- T007 needs T006; T009 needs T008; T011 needs T004 + T010; T013 needs T011 + T012; T017 needs T007 + T011 (tests first throughout).
- **T022 before T023** — the initContainer must create the directory the proxy mounts.
- **T028 must land with T027** — expiry without the reconciler is unenforced, because the relay never checks it.
- **T038 must follow T023** — the product-chart proxy has to exist before the dashboard chart's noVNC pod is deleted, or the harness has no console at all in between.
- T041 and T042 need T035 + T036 — a setting must be real before it is documented; T042 also needs T013 for the subcommand it documents.
- T037 and T038 are the Article VI pair; T021's version assertion fails until T037 lands.

**Cross-story note**: US3's T036–T039 touch the same charts and Makefile as US1's T021–T023. Sequence US3 after US1 despite the stories being logically independent.

**Amendment (T055–T091)**

```text
Phase 8 foundations (T056-T068) ──► Phase 9 US1 (T069-T072) ──► Phase 10 US2 (T073-T081)
        │                                                              │
        └──────────────────────────► Phase 11 US3 (T082-T086) ◄────────┘
                                              │
                                              ▼
                                   Phase 12 polish (T087-T091)
```

- Test before implementation throughout: T056→T057, T058→T059, T060+T061→T062, T063→T064, T066→T067, T069→T070, T071→T072, T073→T074, T075→T076, T077→T078, T079→T080, T082→T083.
- **T057 before T062/T064/T070/T072/T080**: every path comes from `pkg/paths`.
- **T062 before T065**: the prepare step must exist before main calls it. **T064 before T067**: the handlers map the guard's error.
- **T068 before T073**: the store tests need the `domain_id` column.
- **T072 before T074 and T078**: the readback's new signature drives the store and the handler.
- **T083 lands with T070**: a domain naming a socket in `console-sockets` needs the proxy to mount it, or consoles break between the two commits.
- **T089 and T090 come last**: the host pass is the only gate for AppArmor and QEMU's ownership (Constitution IV/IX caveat F15).
- Each amendment task folds into its origin commit in the stack. Feat `18b8917` takes the paths, XML, readback, store, CLI, nova console, chart, scripts and docs. Nova fixes `d71d64d` takes the start-guard mapping in `handlers.go`/`advanced_actions.go`, if it lands next to that commit's reboot-type code. Check each fold with a merge-tree replay of the autosquash order before committing (the fixup-placement trap).

## Parallel Opportunities

**Phase 2**: T004, T005, T006, T008, T010 and T012 — six files, no interdependency.

**Phase 3**: T014 (xml_template_test) ∥ T016 (console_test) ∥ T021 (chart tests) — three test files in three packages.

**Phase 4**: T025 ∥ T026 (same file — sequence them), but T030 (advanced_actions.go) ∥ T033 ∥ T034 (shell) ∥ T031 (console.go) are independent.

**Phase 5**: T041 ∥ T042 — documentation only.

**Phase 6**: T043 ∥ T046 ∥ T047.

**Phase 7**: T049 ∥ T050 ∥ T052 ∥ T053.

**Phase 8**: T056 ∥ T058 ∥ T060 ∥ T061 ∥ T063 ∥ T066: tests in six files (T060 and T061 share `console_dir_test.go`, so write them together). T068 (SQL) ∥ any Go task.

**Phases 9–11**: T069 ∥ T071 ∥ T073 ∥ T075 ∥ T077 ∥ T079 ∥ T082: test files in different packages. T084 ∥ T085 ∥ T086: scripts, compose, docs.

**Phase 12**: T087 ∥ T088.

## Implementation Strategy

**MVP scope**: Phase 1 → Phase 2 → Phase 3 (US1) — 24 tasks producing a live, interactive framebuffer and closing F1's headline symptom.

**But US1 is not shippable alone.** The spec is explicit (US2: *"A working proxy without this is strictly worse than the broken console it replaces — it would relay any caller to any guest. It ships with US1, not after it."*) US1 is the demo increment; **US1 + US2 is the minimum mergeable increment.** Phase 3 leaves the guest on loopback and the token real, but with no expiry, no revocation and no authz — do not stop there.

**Then**: US3 makes it deployable anywhere and removes the documentation fiction; US4 removes the remaining fabricated URLs. Both are independent of each other.

**Four tasks worth extra care**, because their failure modes are silent or misleading:

- **T007** — the wrong `DomainGetXMLDesc` flags return the inactive definition with `port='-1'`: the original bug wearing a new hat. T006 exists for exactly this.
- **T021 / T023** — a proxy that cannot read the token directory (mode or UID) refuses *every* token, indistinguishable from a token bug. Asserted in a chart test rather than discovered during the host pass.
- **T023** — omitting `--idle-timeout` silently leaves it at `0` (disabled), so an abandoned tab holds a guest console forever. Adding `--host-token` would silently let a caller choose the target. One requirement is met by a flag's presence, the other by its absence; both are asserted.
- **T028** — without the reconciler, expiry is not enforced at all. It is not hygiene.

**Amendment strategy.** Foundations (Phase 8) first: they are where VM starts can break. A missing or wrongly owned `console-sockets` stops **every** guest, so T062–T067 must be green before T070 changes the XML. Then US1 → US2 → US3 as listed, and the host pass last. Four tasks need extra care:

- **T062**: ownership comes from libvirt's capabilities, and a wrong owner fails starts silently inside libvirt. That is why T063/T064 refuse first.
- **T070 + T083**: they ship together, or the proxy cannot see the sockets the new domains create.
- **T074**: the domain ID is what replaces the per-start path as the restart signal. Without it, `virsh start` leaves tokens alive until their TTL.
- **T090**: AppArmor and QEMU's write access can only be proven on the host.
