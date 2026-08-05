---
description: "Task list for SPEC-021 — a real console / noVNC (F1)"
---

# Tasks: a real console / noVNC (F1)

**Input**: Design documents from `docs/specs/021-real-novnc-console/`
**Prerequisites**: spec.md (35 live FRs, 12 clarifications), plan.md, research.md (R1–R11), data-model.md, contracts/ (3), quickstart.md

**Tests**: **INCLUDED** — Test-First is non-negotiable under both constitutions (O3K workflow section; Article III). Failing tests precede their implementation task in every phase.

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
- [X] T005 [P] Extend `TestLayout` in `pkg/paths/paths_test.go` to assert `ConsoleTokenDir() == "/var/lib/o3k/console-tokens"` — *(FR-023b)*
- [X] T006 [P] Write failing graphics-XML parse tests in `pkg/hypervisor/libvirt_test.go`: resolved port, `port='-1'` (autoport unresolved), missing `<graphics>`, non-vnc graphics type, malformed XML — *(FR-003, FR-004)*
- [X] T007 Implement `GetConsoleEndpoint(ctx, vmUUID) (host string, port int, err error)` in `pkg/hypervisor/libvirt.go` via `DomainLookupByUUID` + `DomainGetXMLDesc(dom, 0)` — **flags `0`, not `DomainXMLInactive`**, or the port returns `-1` and reproduces the original bug; stub-mode branch returns a not-available error — *(FR-003, FR-004)*
- [X] T008 [P] Write a failing test in `pkg/hypervisor/libvirt_test.go` asserting the readback abandons a hung hypervisor within **1 second** — *(FR-007)*
- [X] T009 Bound the readback in `pkg/hypervisor/libvirt.go` with an explicit 1s deadline: go-libvirt's generated calls take **no `context`**, and the dialer's `WithLocalTimeout` applies only at connect, so run lookup+readback in a goroutine and `select` on a 1s `context.WithTimeout` — *(FR-007)*
- [X] T010 [P] Write failing token-store tests in `internal/console/store_test.go` **against a real throwaway PostgreSQL** (Article IV — real databases over mocks, the pattern established by #533): issue→resolve round trip, projection file is exactly `<token>: <host>:<port>`, issuance fails closed when the directory is unwritable — *(FR-008, FR-009, FR-023b)*
- [X] T011 Create the self-contained token-store library `internal/console` (Article I — following the `internal/teardown` and `internal/health` precedent, so it compiles into the one binary while staying independently testable): `store.go` with `Issue` (insert row **and** write the projection file, failing closed if the file cannot be written), `Resolve`, `RevokeByInstance`, `RevokeExpired`; `projection.go` for the file writer. `internal/nova` calls into this package and holds no token logic — *(FR-008, FR-009, FR-014, FR-016, FR-023b, Article I)*
- [X] T012 [P] Write failing CLI tests in `internal/console/cli_test.go`: `list` prints one line per live token and supports `--json`, `purge` removes expired entries (and by-instance when given one), usage errors exit `2` writing to stderr — *(Article II, Article V)*
- [X] T013 Add `internal/console/cli.go` and dispatch **`o3k console-tokens list|purge`** from `cmd/o3k/main.go`, beside the existing `cleanup` (line 48) and `healthcheck` (line 55) subcommands and following their conventions: text to stdout, `--json` for structured data, errors to stderr, documented exit codes. This is the operator interface to the token store, replacing hand-inspection with `ls`/`cat` — *(Article II, Article V, FR-028)*

**Checkpoint**: `go test ./pkg/hypervisor/... ./pkg/paths/... ./internal/console/...` green; migration applies **and** reverts; `o3k console-tokens list` runs.

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
- [X] T021 [P] [US1] Write helm-unittest specs under `charts/o3k/tests/` asserting all nine items in contracts/proxy-deployment.md: the websockify command override (not the image entrypoint), `hostNetwork: true`, required `podAffinity` to the O3K pod, token-dir mount with a readable mode/UID, `console-tokens` in the initContainer list, `TokenFile` (not `ReadOnlyTokenFile`), a non-zero `--idle-timeout`, **absence of `--host-token`**, and a bumped chart version — *(FR-012, FR-019, FR-023a, FR-023c, FR-023d, Article VI)*
- [X] T022 [US1] Add `console-tokens` to the initContainer subdirectory list in `charts/o3k/templates/_helpers.tpl` (lines 177-193) — must precede T023, which mounts it — *(FR-023b)*
- [X] T023 [US1] Add `charts/o3k/templates/console-proxy.yaml`: real-mode-gated Deployment + Service running `python3 -m websockify --web /root/noVNC --token-plugin TokenFile --token-source /var/lib/o3k/console-tokens --idle-timeout 300 6080`, with `hostNetwork: true`, required `podAffinity` to the O3K pod (`topologyKey: kubernetes.io/hostname`), a hostPath mount of `realMode.hostStoragePath`, and **no `--host-token`** — *(FR-012, FR-019, FR-023a, FR-023c, FR-023d)*
- [X] T024 [US1] Rewrite `test/console_test.sh` to assert a **resolvable** console URL — the current version asserts only that some URL string came back, which is why it passes against the broken console — *(FR-001)*

**Checkpoint**: framebuffer visible and interactive in the real harness.

---

## Phase 4: User Story 2 — Console access is scoped, time-bounded, unforgeable (Priority: P2)

**Goal**: a link opens exactly one instance, for an entitled caller, for a bounded window.

**Independent test**: request a console for instance A, then try B's console with A's link, a link past its lifetime, and an invented link. All refused; the legitimate one works.

- [X] T025 [P] [US2] Write failing tests in `internal/console/store_test.go` (real throwaway PostgreSQL): expired token refused, unknown token refused, a token for instance A cannot resolve to B, cross-project issuance yields `404`, an `admin` token may issue for any project, tokens gone after delete / reboot / suspend — *(FR-010, FR-012, FR-013, FR-014)*
- [X] T026 [P] [US2] Write a failing port-reuse test in `internal/console/store_test.go`: a token issued for A, then A stopped and its console port reassigned to B, must not resolve — the cross-tenant leak this story exists to close — *(FR-014)*
- [X] T027 [US2] Enforce expiry in `internal/console/store.go`: set `expires_at` from `console_token_ttl` at issue, refuse expired tokens in `Resolve`, and opportunistically reap that instance's expired entries on each issue — *(FR-010)*
- [X] T028 [US2] Add `internal/console/reconciler.go` — a periodic sweep at `console_reconcile_interval` plus one sweep at startup — and wire it into `cmd/o3k/main.go` beside the existing server bootstrap. This is what actually enforces expiry: the relay performs no expiry check, so an unreaped file stays live — *(FR-010, FR-016)*
- [X] T029 [US2] Revoke on lifecycle transitions in `internal/nova/handlers.go`: `DeleteServer` (line 1221) and the `reboot` / `os-stop` / `os-start` action branches (lines 1548-1610) — *(FR-014)*
- [X] T030 [P] [US2] Revoke on suspend and resume in `internal/nova/advanced_actions.go` (`SuspendInstance` line 20, `ResumeInstance` line 88) — *(FR-014)*
- [X] T031 [US2] Enforce issuance authorization in `internal/nova/console.go`: project-scoped by default with an `admin` override via the shared `common.HasRole`, returning `404` (never revealing existence) for another project's instance — *(FR-013)*
- [X] T032 [US2] Implement `GET /v2.1/os-console-auth-tokens/{token}` in `internal/nova/console.go` per contracts/nova-console-api.md and register the route in `internal/nova/handlers.go` — compatibility surface only; the console must not depend on it — *(FR-015)*
- [X] T033 [P] [US2] Rewrite `test/console_access_test.sh` as the security probe covering all five SC-005 refusal cases — *(FR-011, FR-012, FR-013, SC-005)*
- [X] T034 [P] [US2] Add a restart-survival probe to `test/console_access_test.sh`: a link issued before an O3K restart still works after it, within its window — the sole justification for persisting tokens, and previously untested. In the same probe, assert the link resolves **twice** within its window, so reconnects and page refreshes are proven rather than assumed — *(FR-009, FR-010a, SC-007)*

**Checkpoint**: all five unauthorized-access attempts fail closed; a legitimate link survives a restart.

---

## Phase 5: User Story 3 — The operator decides where the console lives (Priority: P3)

**Goal**: the configured origin is what users get, in every documented topology.

**Independent test**: set the console origin to the deployment's external host, request a URL, confirm it carries that host and works from another machine.

- [X] T035 [US3] Add the nova console block to `config/o3k.yaml` (near lines 35-39): `console_proxy_base_url`, `console_token_ttl`, `console_reconcile_interval` — *(FR-020, FR-022)*
- [X] T036 [US3] Add `consoleProxy.{enabled,image.{repository,tag,pullPolicy},service.{type,port,nodePort},idleTimeout,resources}` and `nova.consoleProxyBaseUrl` / `nova.consoleTokenTtl` / `nova.consoleReconcileInterval` to `charts/o3k/values.yaml`, render them in `charts/o3k/templates/_helpers.tpl` (lines 115-118), and **extend `charts/o3k/values.schema.json`** — it sets `additionalProperties: false`, so new values fail validation until the schema changes in the same commit. Value names must match quickstart.md exactly — *(FR-020, FR-023a)*
- [X] T037 [US3] Bump the chart version in `charts/o3k/Chart.yaml` (currently `0.2.0`) — a new chart component is a minor change under semantic versioning, and the chart is explicitly "bumped independently of the product" — *(Article VI)*
- [X] T038 [US3] Relocate `deployments/horizon-chart/` to `charts/horizon/`: rewrite `Chart.yaml` to drop the "verification infrastructure — NOT a product chart" claim and set a deliberate `version` and `appVersion`, and delete `templates/novnc.yaml` plus the `novnc` block in `values.yaml` (lines 60-70), both superseded by the product-chart proxy — must follow T023, or the harness has no console in between — *(FR-023e, Article VI)*
- [X] T039 [US3] Update the chart paths in `Makefile`: the lint/template targets (lines 105-107) and the `horizon-chart-sync` target (lines 116-122) — *(FR-023e)*
- [X] T040 [US3] Remove the `novnc` service from `deployments/docker-compose-horizon.yml` (lines 104-125) — stub mode refuses at request time, so the compose stack needs no proxy — *(FR-023a, FR-027)*
- [X] T041 [P] [US3] Make `console_proxy_base_url` real in the guides that already instruct operators to set it, and drop the `/vnc_auto.html` suffix from every example (that page 404s on noVNC 1.6.0): `docs/SINGLE_NODE_DEPLOYMENT.md` (lines 286, 917), `docs/SCALING.md` (line 618), `docs/HORIZON_INTEGRATION.md` (lines 250, 391), `deployments/README-KVM.md` (line 43), `docs/HORIZON_SETUP.md` (line 305) — *(FR-022)*
- [X] T042 [P] [US3] Document the console in the deployment guides: token lifetime, the reconcile interval and its effect on effective validity (`TTL + interval`), the idle timeout, the exposed proxy port, the shared token directory, the **`o3k console-tokens`** subcommand as the supported way to inspect and purge tokens, and that **multi-node console is unsupported in v1** — *(FR-022, Article II)*

**Checkpoint**: a URL issued on the cluster opens from another machine; no `localhost` appears unless configured.

---

## Phase 6: User Story 4 — Honest answers for the consoles O3K does not serve (Priority: P4)

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
- [ ] T051 Run the gates: `make fmt`, `make lint`, `make test`, `make chart-test`, migration up **and** down, and confirm the binary size is unchanged (no new Go dependency) — *(FR-029)*
      <br>*Partially done: `gofmt` clean, `go build` clean, unit suite green against a real Postgres (all packages), `helm lint charts/o3k` + all 75 helm-unittest specs green. Still to run: `make lint` (golangci-lint), `make chart-test` end-to-end including `charts/horizon`, migration down, binary-size check.*
- [X] T052 [P] Open the follow-up ticket for `charts/horizon` production hardening: values schema, ingress/TLS, secrets for the dashboard signing key and memcached, and the `local_settings` duplication the sync target papers over — *(out of scope, recorded)*
- [X] T053 [P] Open a follow-up ticket to reconcile the **two constitutions** (`.specify/memory/constitution.md` vs `.claude/memory/constitution.md`) — duplicate authoritative documents already caused one CRITICAL gate finding on this feature, and the tooling loads a different one than `CLAUDE.md` designates — *(project-level, not this feature)*
- [ ] T054 Branch review before the PR — *(quality gate: both constitutions' before-merge checklists)*

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

## Parallel Opportunities

**Phase 2**: T004, T005, T006, T008, T010 and T012 — six files, no interdependency.

**Phase 3**: T014 (xml_template_test) ∥ T016 (console_test) ∥ T021 (chart tests) — three test files in three packages.

**Phase 4**: T025 ∥ T026 (same file — sequence them), but T030 (advanced_actions.go) ∥ T033 ∥ T034 (shell) ∥ T031 (console.go) are independent.

**Phase 5**: T041 ∥ T042 — documentation only.

**Phase 6**: T043 ∥ T046 ∥ T047.

**Phase 7**: T049 ∥ T050 ∥ T052 ∥ T053.

## Implementation Strategy

**MVP scope**: Phase 1 → Phase 2 → Phase 3 (US1) — 24 tasks producing a live, interactive framebuffer and closing F1's headline symptom.

**But US1 is not shippable alone.** The spec is explicit (US2: *"A working proxy without this is strictly worse than the broken console it replaces — it would relay any caller to any guest. It ships with US1, not after it."*) US1 is the demo increment; **US1 + US2 is the minimum mergeable increment.** Phase 3 leaves the guest on loopback and the token real, but with no expiry, no revocation and no authz — do not stop there.

**Then**: US3 makes it deployable anywhere and removes the documentation fiction; US4 removes the remaining fabricated URLs. Both are independent of each other.

**Four tasks worth extra care**, because their failure modes are silent or misleading:

- **T007** — the wrong `DomainGetXMLDesc` flags return the inactive definition with `port='-1'`: the original bug wearing a new hat. T006 exists for exactly this.
- **T021 / T023** — a proxy that cannot read the token directory (mode or UID) refuses *every* token, indistinguishable from a token bug. Asserted in a chart test rather than discovered during the host pass.
- **T023** — omitting `--idle-timeout` silently leaves it at `0` (disabled), so an abandoned tab holds a guest console forever. Adding `--host-token` would silently let a caller choose the target. One requirement is met by a flag's presence, the other by its absence; both are asserted.
- **T028** — without the reconciler, expiry is not enforced at all. It is not hygiene.
