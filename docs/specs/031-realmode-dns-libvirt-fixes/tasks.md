---

description: "Task list for SPEC-031 — real-mode cluster DNS, libvirt socket choice, retried console-dir preparation"
---

# Tasks: Real mode — cluster DNS under host networking, the libvirt socket, and a capabilities read that is never retried

**Feature**: `031-realmode-dns-libvirt-fixes` (cloud-storage#630)
**Input**: Design documents from `docs/specs/031-realmode-dns-libvirt-fixes/`
**Prerequisites**: plan.md, spec.md (required); research.md (R1–R13), data-model.md, contracts/ (4 files), quickstart.md (present). Constitution **v1.5.0** (ratified 2026-11-06) — the plan's gate is open.

**Tests**: INCLUDED, following the constitution's Development Workflow:
1. contract tests with OpenStack clients where an API outcome changes (T016);
2. the test strategy approved by the user before any test is written (T002);
3. every test seen failing first (RED) — a failing compile counts;
4. then the code.

The tests below implement the strategy approved in T002; a test that departs from it goes back to the user. A test marked **lock** passes before and after: it guards a behaviour this feature must not break.

**Organization**: Grouped by the spec's user stories. Priority order is US1, US2, US3, US6 (all P1), then US4 (P2), then US5 (P3). Among the P1s, **US6 comes before US3**: US3's background retry runs inside the connection loop US6 builds.

**Terms**: a *launch* is a *guest start* as the spec's Key Entities define it — create (image or volume boot), start, hard reboot, resume, self-heal.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel — a different file, no dependency on an incomplete task
- **[Story]**: US1–US6 for story-phase tasks; Setup, Foundational and Polish carry none
- Every task names an exact file path. Line numbers are hints from `a3075c0` — find the code by the function named.

## Path conventions

Single Go project at the repo root (`cmd/`, `internal/`, `pkg/`, `test/`), the Helm chart under `charts/lightstack/`, docs under `docs/`. No `src/` layout (plan.md § Project Structure).

**Test databases** (CLAUDE.md):
- PostgreSQL: `make db-up`, then `LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'`
- SQLite: `LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db'`
- Tests that read the seeded project can race across packages; use `go test -p 1` if they flake.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: a green baseline that SC-008's "the existing suites stay green" is measured against, and the approved test strategy.

- [X] T001 Record the baseline in `docs/specs/031-realmode-dns-libvirt-fixes/baseline-gates.md`. Run `make build`, `make lint`, `go test ./...` on PostgreSQL and on SQLite (both URLs above), `helm unittest charts/lightstack`, and `make test-contract` against a running stub Lightstack. Write down pass/fail/skip counts per package and any failure that predates this branch. Fix nothing here; a pre-existing failure is recorded, not hidden.
- [X] T002 Get the test strategy approved before any test is written (constitution Development Workflow, step 2). Review research.md R10 and every test task in this file with the user, and record the approval — date, and any change asked for — in `docs/specs/031-realmode-dns-libvirt-fixes/baseline-gates.md`. No later task starts before it.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: the `libvirt_uri` grammar (R3) and the per-attempt dialer (R4), which US2, US4, US5 and US6 all build on, plus the fake-libvirtd knobs US2, US3 and US6 test with.

**⚠️ CRITICAL**: no Go story work starts before T006 is green. US1 is chart-only and may run in parallel with this phase.

- [X] T003 [P] Write `TestParseLibvirtURI` in `pkg/hypervisor/libvirt_uri_test.go` (new): a table over `ParseLibvirtURI(string) (LibvirtTarget, error)`, per `contracts/libvirt-connection.md` C-URI-1/C-URI-2.
  - **Automatic** (empty socket): `""`, `qemu:///system`, `qemu+unix:///system`.
  - **Explicit**: `qemu:///system?socket=/run/x/s` and `qemu+unix:///system?socket=/run/x/s` → `/run/x/s`; `/run/libvirt/virtqemud-sock` → itself.
  - **Refused**, each error containing the value and the word `accepted`: `qemu:///session`, `qemu:///embed?root=/x`, `lxc:///system`, `qemu://host/system`, `qemu+tcp://host/system`, `qemu+ssh://u@h/system`, `qemu+tls://h/system`, `qemu+libssh2://h/system`, `qemu+ext:///system`, `relative/sock`, `qemu:///system?socket=relative`, `qemu:///system?socket=/a&socket=/b`, `qemu:///system?mode=direct`, `qemu:///system#frag`, `qemu:///system/`.

  RED: the function does not exist (FR-008, FR-009, FR-011).
- [X] T004 [P] Write `TestSocketDialer` in `pkg/hypervisor/libvirt_dialer_test.go` (new). Run real `net.Listen("unix", …)` listeners named `virtqemud-sock` / `libvirt-sock` in a `t.TempDir()` used as the run directory. Subtests:
  - only `virtqemud-sock` → `Dial` connects to it, and the dialer reports it as the socket dialed;
  - only `libvirt-sock` → that one;
  - both → `virtqemud-sock`;
  - neither → an error naming **both** paths;
  - neither, then `virtqemud-sock` created → the **next** `Dial` connects (the choice is per attempt);
  - `libvirt-sock` a symlink to `virtqemud-sock` → resolves;
  - an explicit target → dials exactly that path although both automatic sockets exist.

  RED: the dialer does not exist (FR-007, FR-010, FR-011).
- [X] T005 [P] Extend the fake libvirtd in `pkg/hypervisor/fake_libvirtd_test.go`. Add fields:
  - `uuid [16]byte` — used by `fakeDomain`, which must also answer `procDomainDefineXML` (it answers nothing today);
  - `capsDelay time.Duration` — applied once, to the first `ConnectGetCapabilities` reply across all connections;
  - `silentConns int` — the first N connections are read but never answered.

  Add two helpers:
  - `serveFakeOnSocket(t, path string, f *fakeLibvirtd)`, which listens on a real unix socket at `path`, serves each accepted connection with `f.serve`, and closes the listener in `t.Cleanup`;
  - `waitForWithin(t, what string, d time.Duration, cond func() bool)` beside `waitFor`. `waitFor`'s deadline is 2 s, too short for a 1.5 s first reply plus a retry; the new connection and retry tests use 10 s.

  Existing tests must still pass (`go test ./pkg/hypervisor/...`).
- [X] T006 Implement `pkg/hypervisor/libvirt_uri.go` (new). It makes T003 and T004 green.
  - **Parser**: `type LibvirtTarget struct{ Socket string }` (empty `Socket` = automatic) and `ParseLibvirtURI(uri string) (LibvirtTarget, error)`, parsed with `net/url`, exactly the accepted and refused forms of T003. One error format: `nova.libvirt_uri %q is not supported; accepted: qemu:///system or qemu+unix:///system (chooses virtqemud-sock, else libvirt-sock), either with ?socket=<absolute path>, or an absolute socket path`.
  - **Run directory**: a package variable `libvirtRunDir = "/var/run/libvirt"`, which tests override.
  - **Dialer**: an unexported `socketDialer` implementing go-libvirt's `socket.Dialer` (`Dial() (net.Conn, error)`). On every `Dial`, an explicit target dials its path; an automatic target `os.Stat`s `virtqemud-sock`, then `libvirt-sock` under the run directory, and dials the first that exists. `net.DialTimeout("unix", …, timeout)`. It records the socket dialed (mutex-guarded accessor) and keeps the last returned `net.Conn`, so a caller can close it to abandon a hung handshake (R5). Neither socket present → an error naming both.

**Checkpoint**: `go test ./pkg/hypervisor/...` green; the parser and dialer are ready.

---

## Phase 3: User Story 1 — Reach in-cluster services by name in real mode (Priority: P1) 🎯 MVP

**Goal**: host-networked Lightstack pods and the purge Job resolve `<service>.<namespace>.svc` (FR-001, FR-002, FR-003a).

**Independent Test**: `helm unittest charts/lightstack` passes the new DNS-policy suite. `helm template … --set realMode.enabled=true --set realMode.hostNetwork=true` shows `dnsPolicy: ClusterFirstWithHostNet` beside `hostNetwork: true` (quickstart §1). On k3s, a Service-name PostgreSQL URL becomes Ready (T044 a).

### Tests for User Story 1 (write first, confirm RED)

- [X] T007 [P] [US1] Write `charts/lightstack/tests/dns_policy_test.yaml` (new) for FR-001, FR-002 and FR-003, per `contracts/chart-render.md` C-DNS-1 and C-DNS-2. Set `image.repository`, `secrets.jwtSecret`, `secrets.adminPassword` and a PostgreSQL `database.url` as the other suites do.
  - With `realMode.enabled: true`, `realMode.hostNetwork: true`: `equal` `spec.template.spec.dnsPolicy` = `ClusterFirstWithHostNet` for `templates/deployment.yaml`, for `templates/daemonset.yaml` (`deploymentMode: multi-node`), and for `templates/teardown-purge-job.yaml` (`dataRetentionPolicy: delete`).
  - `notExists` `spec.template.spec.dnsPolicy` for stub mode with `realMode.hostNetwork: true` (Deployment), and for real mode with `realMode.hostNetwork: false` (Deployment, DaemonSet, purge Job).

  RED: the positive cases fail.

### Implementation for User Story 1

- [X] T008 [US1] FR-001 in `charts/lightstack/templates/_helpers.tpl`, `lightstack.podSpec` (~line 411): inside the existing `{{- if .Values.realMode.enabled }}` block that renders `hostNetwork`, render `dnsPolicy: ClusterFirstWithHostNet` when `.Values.realMode.hostNetwork` is true (R1).
- [X] T009 [P] [US1] FR-001 in `charts/lightstack/templates/teardown-purge-job.yaml` (~line 36): add `dnsPolicy: ClusterFirstWithHostNet` inside the existing `{{- if and .Values.realMode.enabled .Values.realMode.hostNetwork }}` block, after `hostNetwork: true`.
- [X] T010 [P] [US1] Document FR-003a in `charts/lightstack/values.yaml`, `charts/lightstack/values.schema.json` and `charts/lightstack/README.md`: say that host networking renders `dnsPolicy: ClusterFirstWithHostNet`, so the Lightstack pod and the purge Job resolve in-cluster Service names.
  - `values.yaml`: the `realMode.hostNetwork` comment (~line 140);
  - `values.schema.json`: the `realMode.hostNetwork` description (~line 226);
  - `README.md`: the `realMode.privileged` / `hostNetwork` row (~line 137).

**Checkpoint**: `helm unittest charts/lightstack` green; US1 shippable on its own.

---

## Phase 4: User Story 2 — Run real mode on a host with only libvirt's modular daemons (Priority: P1)

**Goal**: with default values, Lightstack dials `virtqemud-sock` where only the modular daemons run and `libvirt-sock` where `libvirtd` runs, logs which, and bounds each connection attempt at 1 s (FR-007, FR-010, FR-012; R5).

**Independent Test**: `go test ./pkg/hypervisor/... -run 'TestNewVMManager'` green. On the openSUSE host, a default chart install logs `/var/run/libvirt/virtqemud-sock` and boots a VM (T044 b); switched to `libvirtd`, it logs `libvirt-sock` (T044 e).

### Tests for User Story 2 (write first, confirm RED)

- [X] T011 [P] [US2] Write `pkg/hypervisor/libvirt_connect_test.go` (new). Override `libvirtRunDir` with a `t.TempDir()` (restore in `t.Cleanup`) and serve the fake with `serveFakeOnSocket` (T005).
  - `TestNewVMManagerSocketChoice`:
    - only `virtqemud-sock` → `NewVMManager("qemu:///system", "real")` connects and reports `virtqemud-sock` as dialed;
    - only `libvirt-sock` → connects to it (FR-012);
    - neither → the error names both paths.

    In the success cases, capture the zerolog global logger into a buffer (restore it after) and assert the INFO line names the socket (FR-010).
  - `TestNewVMManagerBoundsTheAttempt`: a fake with `silentConns: 1` on `virtqemud-sock` → `NewVMManager` returns an error naming the socket within 2 s (bound 1 s). Run it in a goroutine and fail after 5 s, because today it hangs (FR-020a, R5).

  RED: `NewVMManager` dials the fixed `/var/run/libvirt/libvirt-sock` and never times out the handshake.

### Implementation for User Story 2

- [X] T012 [US2] In `pkg/hypervisor/libvirt.go`, `connectLibvirt` (~lines 97-113):
  - parse `m.libvirtURI` with `ParseLibvirtURI` and build the T006 `socketDialer`;
  - add `dialWithin(d *socketDialer, timeout time.Duration) (*libvirt.Libvirt, error)` with `connectAttemptTimeout = time.Second`. It runs the existing `dial(d)` in a goroutine; on timeout it closes the dialer's last `net.Conn`, waits for the goroutine, and returns an error naming the socket;
  - `NewVMManager`'s one attempt uses it (C1 of the analysis, R5);
  - delete the verbatim-path fallback and its wrong "strip scheme" comment;
  - on success, log INFO naming the socket dialed; a failure's error names the socket or sockets tried (FR-010).

  `NewVMManager` keeps its contract — one synchronous attempt, error on failure — which `internal/teardown` relies on (FR-020a). Makes T011 green.

**Checkpoint**: T011 green; modular-daemon hosts connect with default values, and no connection attempt waits longer than 1 s.

---

## Phase 5: User Story 6 — Real mode never reports a server it did not create (Priority: P1)

**Goal**: an unreachable libvirt is an outage, never stub (Constitution IV v1.5.0).
- The server's hypervisor starts unconnected and dials in the background — each attempt ≤ 1 s, pauses 1 → 5 s.
- A create ends `ERROR` with a `fault`, never `ACTIVE`; every real-mode create that ends `ERROR` records a fault.

(FR-019, FR-020, FR-020a.)

**Independent Test**: `go test ./pkg/hypervisor/... -run 'TestStartVMManager'`, `go test ./internal/nova/... -run 'TestNoHypervisor|TestInitHypervisor'` (both backends) and `go test ./internal/teardown/...` green. Against a real-mode Lightstack with no libvirt socket (quickstart §2), the gated contract case T016 passes.

### Tests for User Story 6 (write first, confirm RED)

- [X] T013 [P] [US6] Write `pkg/hypervisor/libvirt_start_test.go` (new), with `libvirtRunDir` overridden, the fake on a real socket (T005), and `waitForWithin(…, 10*time.Second, …)`:
  - `TestStartVMManagerConnectsWhenSocketAppears`: no socket at start → `StartVMManager("qemu:///system", "real", <tempdir>/console-sockets)` returns at once with no error, and a call fails "not connected to libvirt"; then `serveFakeOnSocket(virtqemud-sock)` → the manager connects with no restart (FR-020; US6 scenarios 4–5).
  - `TestStartVMManagerAbandonsSilentHandshake`: `silentConns: 1` → the first attempt is abandoned after ≈1 s, its connection closed, and the second connects (R5).
  - `TestStartVMManagerCloseStopsDialing`: `Close` with no socket present → no dial afterwards (as `TestVMManagerCloseDoesNotReconnect`).

  RED: `StartVMManager` does not exist.
- [X] T014 [P] [US6] Write FR-020a's tests in `internal/teardown/teardown_test.go`:
  - `TestCleanupFailsAtOnceWithoutLibvirtSocket` (**lock**): a `common.Config` with `Nova.LibvirtMode: "real"` and `Nova.LibvirtURI: "<tempdir>/missing/libvirt-sock"` → `Run` returns an error naming that path in well under 1 s, and nothing dials afterwards.
  - `TestCleanupBoundsASilentDaemon`: a plain unix listener at the configured path that accepts and never answers → `Run` returns an error within 2 s. Guard with a 5 s timeout: RED today, because it hangs.
- [X] T015 [P] [US6] Write `internal/nova/no_hypervisor_test.go` (new; database-backed via `requireDB`, run on both backends, following `boot_failure_test.go`'s set-up) for FR-019. `internal/nova` reaches neither the fake libvirtd nor the run directory, so the unconnected manager comes from an **explicit socket path that does not exist**: `hypervisor.StartVMManager("<tempdir>/missing-sock", "real", "<tempdir>/console-sockets")`.
  - `TestNoHypervisorCreateEndsError`:
    - (a) with that manager, `paths.SetBase(t.TempDir())` (restored after; no `t.Parallel`), and a boot from an **existing volume** — seeded available, bootable and unattached in the test project, as `internal/nova/boot_from_volume_test.go` does — so the create reaches `CreateVM`. An image boot would instead stage the disk with `qemu-img` and prepare its directory first, and fail there with a different fault. → `POST /v2.1/servers` answers `202`; the server reaches `ERROR`, never `ACTIVE`, and `GET /v2.1/servers/{id}` carries `fault.code` 500 and `fault.message` "the hypervisor is not ready to start guests";
    - (b) real mode with no manager at all (`NewService(testPool, "", "real", nil)`, no `InitHypervisor`) → `ERROR` with the same fault, not `ACTIVE` (C-GUEST-1, R9).
  - `TestNoHypervisorActionAnswersGeneric500`: (c) `os-stop` on an instance seeded `ACTIVE` with a `libvirt_domain_id` — so the state precondition passes and no `409` masks the answer — with the unconnected manager → `500` whose message contains `failed to perform` (C-GUEST-2).
  - `TestCreateFault` (no database): `createFault` maps an error wrapping `hypervisor.ErrConsoleSocketDirNotReady` to "the hypervisor is not ready to start guests" and any other error to "the hypervisor could not create the guest" (C-GUEST-1).
  - `TestInitHypervisorRealModeAlwaysHasAManager`: `NewService(testPool, "<tempdir>/missing-sock", "real", nil).InitHypervisor("<tempdir>/cs")` → no error and a non-nil manager; with `qemu+tcp://h/system` → an error. This is the guarantee R9's other nil-manager branches rely on.

  RED: (b) reports `ACTIVE` with no fault today, and the rest does not compile.
- [X] T016 [P] [US6] Write the contract test `test/contract/nova/no_hypervisor_test.go` (new, package `nova_test`) — Development Workflow step 1, with an OpenStack client (FR-019).
  - `TestNoHypervisorCreateEndsErrorWithFault` skips unless `LIGHTSTACK_CONTRACT_NO_HYPERVISOR=1`, meaning the Lightstack under test runs real mode with an absent libvirt socket (quickstart §2).
  - It uses `contract.SkipIfLightstackNotRunning`, `contract.SetupComputeV2Client` and the image fixture in `test/contract/nova/image_fixture_test.go`, creates a server with gophercloud, and polls `servers.Get` for up to 60 s. It fails on `ACTIVE` and requires `ERROR` with a non-empty `Fault.Message`.

  RED against that server today: the server goes `ACTIVE`.

### Implementation for User Story 6

- [X] T017 [US6] In `pkg/hypervisor/libvirt.go`, add `StartVMManager(uri, mode, consoleDir string) (*VMManager, error)` and one connection loop that replaces `connect`, `keepConnected` and `redial` (~lines 129-194).
  - **`StartVMManager`, real mode**: parse the URI (errors only for refused forms), keep `m.conn` nil, start the loop, return.
  - **Each loop attempt**: `dialWithin` (T012, 1 s).
  - **Between failures**: pause `reconnectBackoffMin` → `reconnectBackoffMax`. Log the first failure at WARN, naming the sockets tried; later ones at DEBUG.
  - **On success**: `m.conn.Store`, log INFO with the socket, and call the post-connect hook. For now the hook makes one `PrepareConsoleSocketDir(ctx, consoleDir)` attempt per connection, as today's startup does; T025 makes it retry. Then wait on `Disconnected()` or `done`, and repeat.
  - **`NewVMManager`**: keeps its one synchronous, bounded attempt (T012), then runs the same loop already connected, so reconnects behave as today.
  - **Tests**: keep a seam taking a `socket.Dialer` and backoff bounds, and update `newConnectedManager`/`newConnectedManagerWith` in `pkg/hypervisor/fake_libvirtd_test.go` to it. `Close` keeps its `done`/`stopped`/`closeWait` semantics.

  Makes T013 green; `libvirt_reconnect_test.go` stays green.
- [X] T018 [US6] FR-019 in `internal/nova/handlers.go`:
  - `InitHypervisor()` (~line 200) becomes `InitHypervisor(consoleDir string) error` and calls `hypervisor.StartVMManager(svc.libvirtURI, svc.libvirtMode, consoleDir)`. Update the callers in `internal/nova/console_test.go`.
  - Add `createFault(err error) string`, curated as `allocationFault` is: `errors.Is(err, hypervisor.ErrConsoleSocketDirNotReady)` → "the hypervisor is not ready to start guests"; anything else → "the hypervisor could not create the guest". The detail stays in the log line the caller already writes.
  - Route **every** fault-less `ERROR` write in `CreateServer`'s goroutine through `svc.markInstanceError(instanceID, <reason>)`. That is the six inline `UPDATE … status = 'ERROR'` writes, at a3075c0:
    - panic recovery (~1100) → "the server could not be created";
    - boot-from-volume directory (~1234) → "the per-instance directory could not be prepared", the message ~1296 already uses;
    - invalid `imageRef` (~1264) → "the image reference is not a valid image ID";
    - image base (~1284) → "the image could not be made available on this host";
    - overlay staging (~1300) → "the root disk could not be staged";
    - `CreateVM` (~1330) → `createFault(err)`.
  - In the no-manager branch (~lines 1363-1371), add `else if svc.libvirtMode == "real"` → `svc.markInstanceError(instanceID, "the hypervisor is not ready to start guests")`. Only the stub path keeps the immediate `ACTIVE`.

  Makes T015 green, and T016 green against a real-mode server with no socket.
- [X] T019 [US6] In `cmd/lightstack/main.go` (~lines 152-166):
  - call `novaService.InitHypervisor(paths.ConsoleSocketDir())`;
  - in real mode an error is fatal (`log.Fatalf`): only a refused URI can cause one now (R9). Stub mode is unchanged;
  - remove the one-shot `novaService.PrepareConsoleSocketDir(...)` call and its ERROR line — the manager prepares after every connection (T017) and logs its own failures.

**Checkpoint**: T013–T016 green; with libvirt absent, `server create` ends `ERROR` with a fault.

---

## Phase 6: User Story 3 — The first guest boots on a cold, socket-activated libvirt (Priority: P1)

**Goal**:
- the preparation retries until it succeeds, after every connection, and on demand from a guest start;
- concurrent callers share one in-flight attempt;
- the first success is sticky;
- every guest start first makes the instance's directory QEMU's;
- real-mode readiness waits for the first preparation.

(FR-013–FR-018a.)

**Independent Test**: `go test ./pkg/hypervisor/... ./internal/nova/... ./cmd/lightstack/...` green, including the retry, single-flight, stickiness, launch-ownership and readiness tests. On the host, the forced cold path logs a failed first read, then success, and the first server boots `ACTIVE` with no restart (T044 c).

### Tests for User Story 3 (write first, confirm RED)

- [X] T020 [P] [US3] Extend `pkg/hypervisor/console_dir_test.go`, using `waitForWithin(…, 10*time.Second, …)` for every wait:
  - `TestPreparationRetriesUntilLibvirtAnswers`: fake with `capsDelay: 1500*time.Millisecond` and a DAC label for `otherOwner(t)`, under `StartVMManager` → not prepared after the first attempt; `ConsoleDirPrepared()` true within the deadline, with no restart; the directory owned by that uid:gid. Capture the log and assert one ERROR for the first failure and one INFO naming the directory and uid:gid on success (FR-013, FR-017a, FR-018).
  - `TestStartJoinsInFlightAttempt`: with `capsDelay` past the 1 s bound and a background attempt in flight, a concurrent `ensurePrepared` returns that attempt's result within 1.5 s — not after a second attempt of its own — and the fake records one `ConnectGetCapabilities` for both (FR-013, FR-016; Article III v1.5.0).
  - `TestPreparedSurvivesDisconnect`: after success, `pipeDialer.drop()` → `ConsoleDirPrepared()` stays true while unconnected (FR-015, FR-017).
  - `TestRedoAfterReconnect`: the label changes between connections → the owner is replaced; a redo whose caps read fails keeps the earlier owner and `prepared` (FR-015).
  - Adapt `TestStartsRefusedUntilConsoleDirReady` (~line 181): a start whose on-demand attempt fails is refused with `ErrConsoleSocketDirNotReady` naming the directory and the cause (FR-016).

  RED: `ConsoleDirPrepared` does not exist, and nothing retries or shares.
- [X] T021 [P] [US3] Write `pkg/hypervisor/launch_ownership_test.go` (new) for FR-014 and FR-018. Use `paths.SetBase(t.TempDir())` (restored after; no `t.Parallel`, since `pkg/paths` holds global state), a fake whose caps carry `otherOwner(t)`'s uid:gid and whose `uuid` is fixed, and a per-instance directory `InstanceDir(<uuid string>)` created beforehand with the test's own owner.
  - For each of `CreateVM` (domain from `DomainDefineXML`), `StartVM`, `RebootVM(…, hard=true)` and `ResumeVM` (each passed the same uuid): after the call the directory belongs to `otherOwner`, and the fake recorded `procDomainCreate`.
  - With the manager unprepared, the call's single on-demand attempt prepares it first (FR-013).

  RED: the ownership step does not exist.
- [X] T022 [P] [US3] Write `TestHypervisorReady` in `internal/nova/hypervisor_ready_test.go` (new, no database) for FR-017 and FR-018a. `internal/nova` cannot produce a *prepared* manager — the fake libvirtd is test-only in `pkg/hypervisor`, where T020 covers prepared and sticky — so this test covers only:
  - a real-mode service with no manager → `HypervisorReady(ctx)` returns an error;
  - a real-mode service whose manager comes from `hypervisor.StartVMManager("<tempdir>/missing-sock", "real", …)` → an error.

  RED: the method does not exist.
- [X] T023 [P] [US3] Write `TestReadinessChecks` in `cmd/lightstack/readiness_checks_test.go` (new). `readinessChecks(pool, novaService, "real")` includes a check named `hypervisor`; with `"stub"` it does not. A `health.Checker` built from a failing `hypervisor` check answers `/readyz` with `503` and reason `hypervisor unavailable`, and with the database failing too `database,hypervisor unavailable` (C-READY-3, FR-018a). RED: `readinessChecks` does not exist.

### Implementation for User Story 3

- [X] T024 [US3] Rework `pkg/hypervisor/console_dir.go` (R6).
  - **State**: replace the `consoleDir atomic.Pointer[consoleDirState]` usage with `prepared atomic.Bool` (sticky), the last failure's cause, the directory, and a `singleflight.Group` (`golang.org/x/sync/singleflight`, already in `go.mod`; run `go mod tidy` so it turns direct — no new module). Then check that the `go.mod`/`go.sum` diff is only that move, and revert anything else `go mod tidy` touched.
  - **`PrepareConsoleSocketDir`**: one attempt through `group.Do("prepare", …)`. A success sets `prepared`, replaces `qemuOwner` and chowns the directory as today; a failure records the cause and changes nothing in effect once prepared.
  - **`ensurePrepared(ctx)`**: `nil` if prepared; else `PrepareConsoleSocketDir`, which joins the attempt in flight or starts one, and returns `nil` or `ErrConsoleSocketDirNotReady` naming the directory and cause. Never a wait followed by a second attempt.
  - **Exports**: `ConsoleDirPrepared() bool`.
  - **Logging (FR-017a)**: first failure ERROR with the cause; success after a failure INFO with the directory and uid:gid; a redo after a reconnect DEBUG, or INFO when the owner changed; retries DEBUG.
  - Remove `consoleDirReady` once T026 no longer calls it.
- [X] T025 [US3] In `pkg/hypervisor/libvirt.go`, change the post-connect hook (T017) to retry the preparation — through the same single-flight call — until it succeeds, pausing 1 → 5 s between attempts, after **every** connection even when already prepared (FR-015). Stop when the connection drops (the next connection restarts it) or on `Close`. Makes T020's retry, join and redo tests green.
- [X] T026 [US3] In `pkg/hypervisor/libvirt.go`, in each of the four guest-start functions — `createVMReal` (~221), `hardRebootVMReal` (~370, no context: use `context.Background()`), `startVMReal` (~532) and `resumeVMReal` (~784):
  - replace `m.consoleDirReady()` with `m.ensurePrepared(ctx)`;
  - call `m.PrepareInstanceDir(<uuid>)` immediately before `conn.DomainCreate(domain)` — the defined domain's UUID in `createVMReal` (move the UUID formatting above `DomainCreate`; on failure undefine the domain, as the existing error path does), the `vmUUID` parameter elsewhere.

  In `pkg/hypervisor/disk.go`, rewrite `PrepareInstanceDir`'s comment (~lines 33-59): the "never gets used" premise no longer holds, and the step before every guest start makes the directory QEMU's (R7). Makes T021 green.
- [X] T027 [US3] In `internal/nova/handlers.go`, add `HypervisorReady(ctx context.Context) error`: `nil` once `svc.vmManager.ConsoleDirPrepared()`; an error otherwise, including real mode with a nil manager. Delete the `PrepareConsoleSocketDir` wrapper (~lines 209-217) if nothing calls it any more (grep the tests). Makes T022 green.
- [X] T028 [US3] In `cmd/lightstack/main.go`:
  - add `readinessChecks(pool database.DB, nova *nova.Service, libvirtMode string) []health.Check`, returning `health.DatabaseCheck(pool)` plus, when real, `health.Check{Name: "hypervisor", Probe: nova.HypervisorReady}`;
  - move `healthChecker := health.New(...)` from ~line 109 to after `InitHypervisor`, using it — nothing uses the checker in between (its uses: ~371, 415, 436).

  Makes T023 green.

**Checkpoint**: on a cold host the pod becomes Ready only once guests can start, and stays Ready through later libvirt outages.

---

## Phase 7: User Story 4 — Point the chart at a specific libvirt socket (Priority: P2)

**Goal**: a non-default `realMode.libvirtSocketPath` reaches the binary as `libvirt_uri`; the default render is unchanged; the schema refuses a relative path (FR-004, FR-005, FR-006, FR-008).

**Independent Test**: `helm unittest charts/lightstack` green with the new cases. `helm template … --set realMode.libvirtSocketPath=/run/libvirt/virtqemud-sock` renders `libvirt_uri: "/run/libvirt/virtqemud-sock"` in the config Secret (quickstart §1).

### Tests for User Story 4 (write first, confirm RED)

- [X] T029 [P] [US4] Add FR-004's cases to `charts/lightstack/tests/config_render_test.yaml`, per C-SOCK-1 and C-SOCK-2, `matchRegex` on `stringData["lightstack.yaml"]`:
  - real mode with `realMode.libvirtSocketPath: /run/libvirt/virtqemud-sock` → `libvirt_uri: "/run/libvirt/virtqemud-sock"`;
  - the default → `libvirt_uri: "qemu:///system"`;
  - set explicitly to `/var/run/libvirt/libvirt-sock` → `libvirt_uri: "qemu:///system"`.

  RED: the first case fails.
- [X] T030 [P] [US4] Add FR-006's cases to `charts/lightstack/tests/schema_test.yaml`, per C-SOCK-3: `realMode.libvirtSocketPath: libvirt-sock` and `realMode.libvirtSocketPath: ""` each → `failedTemplate` with an `errorPattern` matching `libvirtSocketPath|pattern|schema`. RED: both render today.
- [X] T031 [P] [US4] Add `TestExplicitSocketWins` to `pkg/hypervisor/libvirt_connect_test.go` (**lock** once T012 is in): with both automatic sockets served in the run dir, `NewVMManager("<tempdir>/custom-sock", "real")` and `NewVMManager("qemu+unix:///system?socket=<tempdir>/custom-sock", "real")` each dial the custom socket (US4 scenario 3, FR-008).

### Implementation for User Story 4

- [X] T032 [US4] FR-004 in `charts/lightstack/templates/_helpers.tpl`, `lightstack.config` (~line 253): render `libvirt_uri: {{ .Values.realMode.libvirtSocketPath | quote }}` when it differs from `/var/run/libvirt/libvirt-sock`, else `libvirt_uri: "qemu:///system"`. Keep the default literal in one place, e.g. a `lightstack.libvirtDefaultSocket` define (R2). Makes T029 green.
- [X] T033 [P] [US4] In `charts/lightstack/values.schema.json`, give `realMode.libvirtSocketPath` the property `"pattern": "^/.+"` (FR-006; makes T030 green). Then document FR-005 in its description there, in the `charts/lightstack/values.yaml` comment (~lines 141-143), and in the `charts/lightstack/README.md` row (~line 138):
  - a non-default value is also the socket dialed;
  - the default lets the binary choose `virtqemud-sock`, else `libvirt-sock`;
  - with `secrets.existingSecret` the operator's own config must name the same socket.

**Checkpoint**: a custom socket in the chart is the socket dialed by the pod and the purge Job.

---

## Phase 8: User Story 5 — A libvirt setting Lightstack cannot honour is refused at startup (Priority: P3)

**Goal**: an unsupported `nova.libvirt_uri` stops real mode at configuration load in every entry point that loads it; stub mode does not check it; the docs stop advertising forms that never worked (FR-009, FR-022).

**Independent Test**: `go test ./cmd/lightstack/... -run 'TestUnsupportedLibvirtURI|TestLoadConfigLibvirtURI'` green. `lightstack --config <real mode, qemu+tcp://h/system>` exits 1 naming the value and the accepted forms (quickstart §2).

### Tests for User Story 5 (write first, confirm RED)

- [X] T034 [P] [US5] Add `TestUnsupportedLibvirtURIRefused` to `cmd/lightstack/startup_refusal_test.go` (FR-009). Use the existing `run` helper with a SQLite URL in a temp dir and `extra: "nova:\n  libvirt_mode: real\n  libvirt_uri: \"qemu+tcp://h/system\"\n"`. For the `server` (exit 1), `cleanup` (2) and `console-tokens` (2) entry points, check the exit code, plus output containing `qemu+tcp://h/system` and `accepted`. Skip `lightstack-migrate`, which never reads libvirt. RED: the server starts and the run times out.
- [X] T035 [P] [US5] Add `TestLoadConfigLibvirtURI` to `cmd/lightstack/config_test.go`: `loadConfig` on a written config with `libvirt_mode: real` and `libvirt_uri: qemu:///session` returns an error naming the value; with `libvirt_mode: stub` the same value loads; real mode with `qemu+unix:///system?socket=/run/x/s` loads. RED: the first case loads.

### Implementation for User Story 5

- [X] T036 [US5] In `cmd/lightstack/main.go`, `loadConfig` (~lines 492-499): after `common.LoadConfig`, when `cfg.Nova.LibvirtMode == "real"`, call `hypervisor.ParseLibvirtURI(cfg.Nova.LibvirtURI)` and return its error unchanged (R3). Makes T034 and T035 green.
- [X] T037 [P] [US5] Update `docs/CONFIGURATION.md` and `docs/SCALING.md` (FR-022):
  - `docs/CONFIGURATION.md` (~lines 425-440): replace the "libvirt_uri options" list (`qemu:///session`, `qemu+tcp://`, `qemu+ssh://`) with the accepted forms and the automatic choice (`virtqemud-sock`, else `libvirt-sock`), and note that anything else stops real-mode startup.
  - `docs/SCALING.md` (~line 616): replace `qemu+tcp://compute1/system` with `qemu:///system` and a note that each node dials its own local libvirt.

**Checkpoint**: refused forms stop startup with one clear message.

---

## Phase 9: Polish & Cross-Cutting Concerns

**Purpose**: the remaining documentation (FR-022, FR-023, FR-024), the gates, a mutation check of the new tests, the host runbook, and host validation (FR-021).

- [X] T038 [P] FR-022: name `virtqemud-sock` beside `libvirt-sock` wherever an operator doc checks for the socket — `docs/TROUBLESHOOTING.md` (~196), `docs/REAL_LIBVIRT_MODE.md` (~102, ~577), `docs/REAL_MODE_TESTING.md` (~31, ~56, ~164), `docs/HORIZON_INTEGRATION.md` (~364).
- [X] T039 FR-023, in `charts/lightstack/CONTRACT.md`, `charts/lightstack/README.md`, `charts/lightstack/templates/NOTES.txt`, `charts/lightstack/values.yaml`, `charts/lightstack/values.schema.json` and `docs/blueprint.md`: add the real-mode readiness condition — `/readyz` also waits until the console socket directory has been prepared once, and a host where that never succeeds is restarted by the startup probe — to every place that lists the readiness inputs:
  - `CONTRACT.md` §2 (~lines 91-93);
  - `README.md`, the health table (~line 27);
  - `NOTES.txt` (~lines 22-23);
  - `values.yaml`, the `probes` comments (~lines 278-289);
  - `values.schema.json`, the probe descriptions (~lines 389, 394);
  - `docs/blueprint.md` (~lines 526-530).

  Runs after T010 and T033, which touch the same chart files.
- [X] T040 [P] FR-024: add a section to `docs/UPGRADING.md` with the four notes:
  - an unsupported `libvirt_uri` now stops real-mode startup;
  - a real-mode pod that cannot prepare the console socket directory is no longer Ready, and is restarted by its startup probe;
  - host-networked pods now resolve through the cluster DNS, so a name only the node's own resolver answers must be reachable through it;
  - the host workarounds can go: the `libvirt-sock` symlink, enabling `virtproxyd.socket`, and warming libvirt before start.
- [X] T041 Re-run T001's gates and append the results to `docs/specs/031-realmode-dns-libvirt-fixes/baseline-gates.md`, comparing with the baseline (SC-008):
  - `make fmt` clean, `make lint`, `go vet ./...`;
  - `go test ./...` on PostgreSQL and on SQLite;
  - `helm unittest charts/lightstack`;
  - `make test-contract` against a running stub Lightstack, once with a PostgreSQL and once with a SQLite `database.url`;
  - `LIGHTSTACK_CONTRACT_NO_HYPERVISOR=1 go test ./test/contract/nova/ -run NoHypervisor` against a real-mode Lightstack with no libvirt socket (quickstart §2).

  If `make lint` reports paths from a removed worktree, run `go tool golangci-lint cache clean` and repeat.
- [X] T042 Mutation-check the new tests and record the outcome in `docs/specs/031-realmode-dns-libvirt-fixes/baseline-gates.md`. Revert each fix **off the worktree** — a `git clone --shared` scratch clone or `go test -overlay`, never by editing tracked files in place — and confirm its test fails:
  - the `dnsPolicy` line (T007);
  - the per-attempt socket choice turned into a one-time choice (T004, T013);
  - `dialWithin`'s bound removed (T011, T013, T014);
  - the single-flight join replaced by a lock-and-retry (T020);
  - `prepared` cleared on a failed redo (T020);
  - the ownership step removed from one guest-start function at a time (T021);
  - the `ERROR` branch restored to `ACTIVE` and the `markInstanceError` call on `CreateVM` failure restored to the inline write (T015, T016);
  - the `loadConfig` check removed (T034, T035).
- [X] T043 Generate the host runbook `docs/specs/031-realmode-dns-libvirt-fixes/HOST_TESTING.md` with `/cl.hosttest docs/specs/031-realmode-dns-libvirt-fixes/`: prerequisites, setup, the FR-021 (a)–(e) steps as `quickstart.md` §3 and research R12 lay them out, and teardown, zsh-paste-safe.
- [ ] T044 Run host validation FR-021 (a)–(e) with the T043 runbook — k3s on the openSUSE host, `virtqemud` socket-activated, no `libvirtd`, `virtproxyd` or symlink — and record each step's outcome in `docs/specs/031-realmode-dns-libvirt-fixes/HOST_TESTING.md`. A cold step where the fast host answered within 1 s anyway is recorded as "retry not exercised". Restore the host's daemon setup afterwards.

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (T001 → T002)** → everything; T002, the approval, gates every later task.
- **Foundational (T003–T006)** → every Go story (US2, US6, US3, US4's T031, US5). T003, T004 and T005 run in parallel; T006 needs T003 and T004.
- **US1 (T007–T010)**: chart only; needs T002 alone, so it can run in parallel with Foundational.
- **US2 (T011–T012)**: needs T005 and T006.
- **US6 (T013–T019)**: needs US2's T012 — `dialWithin` and the `connectLibvirt` rewrite the loop builds on. T017 → T018 → T019.
- **US3 (T020–T028)**: needs US6 — the retry runs in its loop, and `StartVMManager` exists.
  - T024 → T025 → T026 (T025 and T026 share `libvirt.go`).
  - T024 → T027 → T028.
- **US4 (T029–T033)**: the chart tasks need only T002. T031 needs T012. T032 and T033 touch different files. T033 runs after T010, which edits the same `values.yaml`, schema and README.
- **US5 (T034–T037)**: needs T006. T036 needs T034 and T035 written.
- **Polish (T038–T044)**: after the stories.
  - T039 after T010 and T033.
  - T041 after all code; T042 after T041.
  - T043 before T044. T044 is last — it needs a built image and the host.

### Story completion order

US1 → US2 → US6 → US3 → US4 → US5, then Polish. US6 precedes US3 by dependency; both are P1.

### Within each story

Tests first, seen failing (RED) — or green from the start for a **lock** — then the implementation, in the order given. Each story ends at a checkpoint where its independent test passes.

---

## Parallel Examples

```bash
# Foundational — three test files at once:
Task: "T003 TestParseLibvirtURI in pkg/hypervisor/libvirt_uri_test.go"
Task: "T004 TestSocketDialer in pkg/hypervisor/libvirt_dialer_test.go"
Task: "T005 fake libvirtd knobs + serveFakeOnSocket + waitForWithin in pkg/hypervisor/fake_libvirtd_test.go"

# US1 alongside Foundational (chart vs Go), then its three edits at once after T007:
Task: "T008 dnsPolicy in charts/lightstack/templates/_helpers.tpl"
Task: "T009 dnsPolicy in charts/lightstack/templates/teardown-purge-job.yaml"
Task: "T010 FR-003a docs in values.yaml / values.schema.json / README.md"

# US6 tests together:
Task: "T013 pkg/hypervisor/libvirt_start_test.go"
Task: "T014 internal/teardown/teardown_test.go"
Task: "T015 internal/nova/no_hypervisor_test.go"
Task: "T016 test/contract/nova/no_hypervisor_test.go"

# US3 tests together:
Task: "T020 pkg/hypervisor/console_dir_test.go"
Task: "T021 pkg/hypervisor/launch_ownership_test.go"
Task: "T022 internal/nova/hypervisor_ready_test.go"
Task: "T023 cmd/lightstack/readiness_checks_test.go"
```

Never run these in parallel — the tasks share a file:

| File | Tasks |
|---|---|
| `pkg/hypervisor/libvirt.go` | T012, T017, T025, T026 |
| `pkg/hypervisor/libvirt_connect_test.go` | T011, T031 |
| `cmd/lightstack/main.go` | T019, T028, T036 |
| `internal/nova/handlers.go` | T018, T027 |
| the chart's `values.yaml` / `values.schema.json` / `README.md` | T010, T033, T039 |

---

## Implementation Strategy

### MVP first

**US1 (T001, T002, T007–T010)**: chart-only and shippable alone. It removes the cluster-IP workaround for an in-cluster database, RGW or Redis.

### Incremental delivery

1. **Foundational + US2** (T003–T006, T011–T012): modular-daemon hosts connect with default values, each attempt bounded at 1 s.
2. **US6** (T013–T019): no fake servers, a fault on every create `ERROR`; startup never waits on libvirt; reconnects as before.
3. **US3** (T020–T028): a cold libvirt no longer disables guest starts; guest starts fix directory ownership; readiness tells the truth.
4. **US4 + US5** (T029–T037): an explicit socket through the chart; refused URIs stop startup.
5. **Polish** (T038–T044): docs, gates, mutation check, runbook, host validation.

Each increment leaves the earlier stories' tests green.

---

## Notes

- 44 tasks: Setup 2, Foundational 4, US1 4, US2 2, US6 7, US3 9, US4 5, US5 4, Polish 7.
- The user commits. Leave every change in the working tree; never `git commit`.
- Mutation checks and experiments run off the worktree — a scratch clone or `go test -overlay` — because the working tree can be committed from at any moment.
- Error, fault and status contracts are pinned in `contracts/` — check a test's expectation there before inventing a message.
