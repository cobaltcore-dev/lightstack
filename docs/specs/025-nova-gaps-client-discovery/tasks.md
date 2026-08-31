# Tasks: Nova Gaps & Client Discovery

**Input**: Design documents from `docs/specs/025-nova-gaps-client-discovery/`
**Prerequisites**: plan.md, spec.md (80 FRs, 9 stories), research.md (R1–R12), data-model.md, contracts/ (7)

**Revision**: updated 2026-08-31 after the `/cl.analyze` cross-artifact pass — seven verification tasks
added (FR-009, FR-019, FR-024c, FR-025c, FR-039, FR-046, FR-055), two "confirm" tasks promoted to assertions
(FR-028, FR-039), FR-052's predicate corrected, and all tasks renumbered. 126 tasks.

**Tests**: **REQUIRED, not optional.** The project Constitution mandates Test-First Development
("Write contract tests first … Confirm RED … Implement") and CLAUDE.md marks it NON-NEGOTIABLE. Every
story therefore opens with failing tests.

**Organization**: grouped by user story. Story order follows spec priority with two plan-mandated
adjustments: **US4 before US3** (US3's pre-flight reads the network list that US4's `VMSpec` extraction
exposes, and US3's fault write is US4's `FR-025c` failure path), and **US9 after US3/US4** (it depends
on US3's failure semantics and touches the boot path US4 refactors).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel — different files, no dependency on an incomplete task
- **[Story]**: US1–US9 per spec.md

## Path Conventions

Existing single-binary layout. No new top-level directories. Go sources under `internal/` and `pkg/`,
migrations in `migrations/`, contract tests in `test/contract/<service>/`, integration scripts in `test/`.

---

## Phase 1: Setup

**Purpose**: establish a trustworthy baseline before anything changes.

- [X] T001 Verify the shared PostgreSQL baseline before trusting any red run: check `quotas` for project `default` is non-zero and the project list is intact, per the known shared-container hazard, and record the values in `docs/specs/025-nova-gaps-client-discovery/BASELINE.md`
- [X] T002 Capture the pre-change suite baseline into `docs/specs/025-nova-gaps-client-discovery/BASELINE.md`: `go test ./...`, `go test ./test/contract/nova/... ./test/contract/keystone/...`, and `make lint`, so later failures are attributable
- [X] T003 [P] Confirm both stub and real-mode stacks come up per quickstart.md and record which stories each can exercise in `BASELINE.md`

**Checkpoint**: a known-good baseline exists and red results are attributable.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: the shared self-link rule and the shared configuration surface. Both are single files that
many stories read; landing them first means one round of expectation churn instead of nine.

**⚠️ CRITICAL**: no user story work begins until this phase completes.

- [X] T004 Rewrite `internal/common/baseurl_test.go` for the new precedence (FR-002, FR-003): replace the cases at `:43` and `:49` that pass the request host as the literal `"ignored:1"` and assert the configured host wins; add a case asserting `defaultPort` is applied when `LIGHTSTACK_ENDPOINT_HOST` is unset (FR-004), a case for a request carrying no host (FR-003), and a case for TLS terminated ahead of the service (FR-005). **Confirm RED.**
- [X] T005 Implement the precedence reversal and port fix in `internal/common/baseurl.go`: prefer `c.Request.Host`, fall back to `LIGHTSTACK_ENDPOINT_HOST`, and apply `defaultPort` in both branches (FR-002, FR-003, FR-004, FR-005). No opt-out setting (FR-003a). This is also what makes a client-supplied endpoint override reach the host the client named (FR-006). **T004 must be GREEN after this.**
- [X] T006 Sweep `internal/` and `test/` for test expectations broken by T005 and update them — the change alters every advertised self-link, so unrelated suites may assert the old host; fix expectations, never the helper
- [X] T007 Add `StateReconcileInterval` (`state_reconcile_interval`, default 30s) and `InstanceActionRetention` (`instance_action_retention`, default 2160h) to `NovaConfig` in `internal/common/config.go` with `DefaultStateReconcileInterval` / `DefaultInstanceActionRetention` constants applied by `LoadConfig`, matching the existing `ConsoleTokenTTL` / `ConsoleReconcileInterval` style (FR-024d, FR-042b). Both settings live here so US4 and US7 never edit this file concurrently
- [X] T008 [P] Add unit coverage in `internal/common/config_test.go` asserting each new setting falls back to its default when the file omits it and that a non-positive configured value is clamped rather than accepted (FR-024d, FR-042b)
- [X] T009 [P] Expose both settings as chart values in `charts/lightstack/values.yaml` and `charts/lightstack/values.schema.json`, rendered into the Secret-backed `lightstack.yaml` — the binary honours only `LIGHTSTACK_DB_URL`, `LIGHTSTACK_JWT_SECRET`, `LIGHTSTACK_ENDPOINT_HOST` and `LIGHTSTACK_ENDPOINT_SCHEME` as environment overrides, so these cannot be passed as env

**Checkpoint**: one self-link rule and one configuration surface. User stories can now proceed.

---

## Phase 3: User Story 1 — Use a stock client with no configuration (Priority: P1) 🎯 MVP

**Goal**: a stock client works out of the box — `flavor list` succeeds with no version pinned, and a
per-service endpoint override is honoured.

**Independent Test**: with no compute API version pinned, list flavors — they list. Set an
`OS_*_ENDPOINT_OVERRIDE` and run a command — the override is the host contacted.

### Tests for User Story 1

- [X] T010 [P] [US1] Contract test in `test/contract/nova/version_document_test.go` asserting `GET /v2.1` carries `version.links` with a `rel: self` whose `href` is absolute and matches the requested host (FR-001, FR-007). **Confirm RED.**
- [X] T011 [P] [US1] Contract test in `test/contract/nova/selflink_absolute_test.go` asserting every self-link in a server, flavor and image response is absolute and carries a scheme (FR-007). **Confirm RED.**
- [X] T012 [P] [US1] Unit test in `internal/neutron/network_test.go` asserting the version document's `links[].href` is absolute, not the relative `"/v2.0"` (FR-007a, FR-007b). **Confirm RED.**
- [X] T013 [P] [US1] Unit test in `internal/cinder/volumes_test.go` asserting the v3 version document's `href` is derived from the request, not the hardcoded `http://lightstack:8776/v3/` (FR-007a, FR-007b). **Confirm RED.**
- [X] T014 [P] [US1] Unit test in `internal/nova/console_test.go` pinning that the console link's origin never advertises the API's own port 8774, under both an unset and a set `LIGHTSTACK_ENDPOINT_HOST` (FR-009, SPEC-021 FR-021). **Must be written before the console change and must pass before and after it** — this is a regression guard on a shipped feature, not a new behaviour
- [X] T015 [P] [US1] Exhaustive sweep test in `internal/common/selflink_sweep_test.go` (or an equivalent repository check) asserting **no** emitter in `internal/` produces a self-link that is relative, scheme-less, or built from a hardcoded host — so SC-003's "0 responses" is a measured claim rather than a sample. Exclude `internal/metadata/service.go:264-315`, whose `links` field is cloud-init interface data, not a self-link (FR-007, FR-007a, SC-003). **Confirm RED.**

### Implementation for User Story 1

- [X] T016 [US1] Add the `links` array with a `rel: self` href to `GetVersion` in `internal/nova/handlers.go:287`, using `common.BaseURL(c, 8774)` exactly as `ListVersions:279` already does (FR-001, FR-008)
- [X] T017 [P] [US1] Replace the relative `"/v2.0"` self-link in `internal/neutron/network.go:255` with a `common.BaseURL`-derived absolute href (FR-007a, FR-007b)
- [X] T018 [P] [US1] Replace the hardcoded `http://lightstack:8776/v3/` self-link in `internal/cinder/volumes.go:2253` with a `common.BaseURL`-derived href (FR-007a, FR-007b)
- [X] T019 [P] [US1] Replace the hardcoded `"http://" + c.Request.Host` bases in `internal/placement/placement.go:39` and `:60` with `common.BaseURL` (FR-007a)
- [X] T020 [P] [US1] Replace the scheme-less `c.Request.Host + "/v2.1/servers/…"` self-link in `internal/nova/server_update.go:105` with `common.BaseURL` (FR-007, FR-007a)
- [X] T021 [US1] Re-assess `deriveConsoleOrigin` in `internal/nova/console.go:299` now that `defaultPort` is honoured: remove it if the shared rule subsumes it, otherwise keep it with an updated comment explaining what still differs. Either way the console origin must never advertise the API's own port 8774 (FR-009)
- [X] T022 [US1] Integration coverage in `test/nova_gaps_test.sh`: `openstack flavor list` with `OS_COMPUTE_API_VERSION` unset, and one `OS_*_ENDPOINT_OVERRIDE` proven to be the host contacted against a host reachable under two names (FR-006, FR-008)

**Checkpoint**: US1 delivers independently — a stock client works with no special configuration.

---

## Phase 4: User Story 2 — Read back a project, user or role (Priority: P1)

**Goal**: `project show`, `user show` and `role show` by name succeed on the 6.6.0 client, and this
project's own runbook fixtures stop degrading to empty strings.

**Independent Test**: on the older client, run all three `show` commands by name — each prints the
resource. Fetch each resource directly and confirm an absolute `links.self`.

### Tests for User Story 2

- [X] T023 [P] [US2] Contract test in `test/contract/keystone/identity_links_test.go` asserting a single-resource read of a project, user and role each carries an absolute `links.self` (FR-010, FR-012). **Confirm RED.**
- [X] T024 [P] [US2] Contract test in `test/contract/keystone/identity_links_test.go` asserting the **collection** entries carry the same `links.self` — this is the path a name-based `show` actually takes (FR-011). **Confirm RED.**
- [X] T025 [P] [US2] Unit test in `internal/keystone/handlers_test.go` asserting no other field of any identity response changes, including nullable `description` and `domain_id` rendering (FR-013). **Confirm RED against the link additions only.**

### Implementation for User Story 2

- [X] T026 [US2] Add `links.self` to `GetProject` (`internal/keystone/handlers.go:495`) and `GetUser` (`:421`) via `common.BaseURL(c, 35357)` (FR-010, FR-012, FR-014)
- [X] T027 [US2] Add `links.self` to the collection entries in `internal/keystone/handlers.go`: `ListUsers:404`, `ListProjects:477`, `ListRoles:706`, `ListAuthProjects:354`, `ListUserProjectRoles:926`, `GetUserProjects:1393` (FR-011, FR-014)
- [X] T028 [P] [US2] Fix the five scheme-less `c.Request.Host + "/v3/…"` links in `internal/keystone/handlers.go:588, 656, 739, 778, 825` to use `common.BaseURL` (FR-007, FR-012)
- [X] T029 [P] [US2] Fix the four scheme-less links in `internal/keystone/groups.go:47, 121, 156, 221` the same way (FR-007, FR-012)
- [X] T030 [US2] Integration coverage in `test/nova_gaps_test.sh`: with the 6.6.0 client on `PATH` (assert via `which -a openstack`), `project show`, `user show` and `role show` by name all exit 0 (FR-014)
- [X] T031 [US2] Re-run the fixture block of `docs/specs/023-tenant-scoping-authz/HOST_TESTING_REAL_MODE.md` and confirm `PROJ_A`, `PROJ_B`, `USER_A` and `USER_B` are non-empty — the silent degradation is why this defect reaches the project's own validation (FR-014)

**Checkpoint**: US1 and US2 both work independently.

---

## Phase 5: User Story 4 — Stop a server and start it again (Priority: P1)

**Goal**: stop and start both work, the record never claims a server runs when it does not, and a start
against an absent guest re-creates it.

**Independent Test**: on a real host, stop and start a booted server — both succeed and the reported
state matches the hypervisor after each. Remove the guest out of band, then start — it succeeds.

**Sequenced before US3** per plan: the `VMSpec` extraction here is what US3's pre-flight reads.

### Tests for User Story 4

- [X] T032 [P] [US4] Unit test in `internal/nova/lifecycle_test.go` asserting the **real-mode** branch persists `SHUTOFF` / `power_state=4` after a successful stop (FR-022, FR-024a). **Confirm RED — the real branch writes nothing today.**
- [X] T033 [P] [US4] Unit test in `internal/nova/lifecycle_test.go` asserting a successful start persists `ACTIVE` / `power_state=1` (FR-023, FR-024a). **Confirm RED.**
- [X] T034 [P] [US4] Unit test in `internal/nova/lifecycle_test.go` asserting the three failure outcomes map to three distinct statuses — absent target, action impossible, internal fault — for **every** action that reaches the hypervisor, `reboot` included, not only stop and start (FR-026, FR-026a, FR-026b, FR-027). **Confirm RED — every libvirt failure is a flat 500 today.**
- [X] T035 [P] [US4] Unit test in `internal/nova/selfheal_test.go` asserting a start against an absent guest re-creates it from stored state and succeeds, that the rebuilt definition carries the flavor's processors, memory and disk plus the instance's recorded ports, and that the repair is recorded under the existing `os-start` action name with no new action introduced (FR-025, FR-025a, FR-025d). **Confirm RED.**
- [X] T036 [P] [US4] Unit test in `internal/nova/state_reconciler_test.go` asserting a server whose guest is absent stops being reported as running after one reconciliation pass, and that a live server is untouched (FR-024b). **Confirm RED.**
- [X] T037 [P] [US4] Unit test in `internal/nova/lifecycle_test.go` asserting a server read and a server **listing** issue **zero** hypervisor calls, and that the count does not grow with the number of servers returned — the feature's only performance requirement, currently unverified (FR-024c). **Confirm RED or by mutation.**
- [X] T038 [P] [US4] Unit test in `internal/nova/selfheal_test.go` asserting that when re-creation itself fails the response is *action impossible* rather than an internal fault, and the reported state ends up consistent with reality — including when re-creation exceeds its time budget rather than failing outright (FR-025c, FR-025e, FR-026b). **Confirm RED.**
- [X] T039 [P] [US4] Unit test in `internal/nova/lifecycle_test.go` asserting stub mode is unchanged — transitions recorded, no hypervisor contacted, self-heal a no-op. An assertion rather than an eyeball, because Constitution Article IV makes stub mode mandatory (FR-028). **Should pass — regression guard.**

### Implementation for User Story 4

- [X] T040 [US4] Extract the `hypervisor.VMSpec` assembly out of the VM-creation goroutine closure at `internal/nova/handlers.go:830-838` into a reusable function that builds a spec from stored state alone — flavor row, `ports WHERE device_id`, the per-instance overlay path and the instance's config-drive ISO (FR-025a, research.md R3). This extraction is the prerequisite for the self-heal and is the actual work of this story
- [X] T041 [US4] Persist the state change in the real-mode branch of `lifecycleAction` in `internal/nova/handlers.go:1741-1756` for stop, start and reboot — today only the stub branch writes `status` and `power_state` (FR-022, FR-023, FR-024a)
- [X] T042 [US4] Distinguish an absent hypervisor guest from an internal fault in `pkg/hypervisor/libvirt.go` `stopVMReal:270` / `startVMReal:316` so the caller can tell them apart — a typed or sentinel error, not a wrapped string the outer layer cannot inspect (FR-026)
- [X] T043 [US4] Map the outcomes to upstream's statuses in `internal/nova/handlers.go` `lifecycleAction`: `common.NewNotFoundError` for an absent target, `common.NewOperationConflictError` for an action impossible in the current state, `common.NewInternalServerError` only for a genuine fault. Introduce no status upstream does not emit for a lifecycle action (FR-026a, FR-027, research.md R10)
- [X] T044 [US4] Implement the self-heal in the `os-start` path of `internal/nova/handlers.go` `lifecycleAction`: on an absent guest, rebuild the spec via T040, re-define and start through the existing `CreateVM`, and persist the resulting domain identifier. One attempt inside the existing 10-second context — no retry, no backoff; exceeding the budget is a failure under FR-025c, not a held-open request (FR-025, FR-025a, FR-025e)
- [X] T045 [US4] In `internal/nova/handlers.go` `lifecycleAction`, on a self-heal that cannot complete answer *action impossible* rather than an internal fault, and leave the reported state consistent with reality (FR-025c, FR-026b)
- [X] T046 [US4] In `internal/nova/handlers.go`, record the self-heal on the `instance_actions` row for the triggering `os-start` via its existing `message` column, plus a warning-level log. Introduce no new action name and no new API surface (FR-025b, FR-025d)
- [X] T047 [US4] Create `internal/nova/state_reconciler.go` following `internal/neutron/reconciler.go:31,44,67,70` and `internal/console/reconciler.go:33,42,62`: constructor taking the interval and clamping non-positive values, `Start(ctx)`, an effective-interval accessor for logging, and `Stop()` (FR-024b, research.md R6)
- [X] T048 [US4] Implement the reconciliation sweep in `internal/nova/state_reconciler.go`: for real-mode instances reported as running, compare against the hypervisor and correct the record, so a server is never reported as running when the hypervisor has no corresponding guest. Reading a server must **not** contact the hypervisor — the sweep is the only place that does (FR-024, FR-024b, FR-024c)
- [X] T049 [US4] Wire the reconciler into `cmd/lightstack/main.go` beside the existing two — start as a goroutine when the mode is real, log the effective interval, and stop it on shutdown (FR-024b)
- [X] T050 [US4] Real-mode host validation, recorded in `docs/specs/025-nova-gaps-client-discovery/HOST_TESTING_REAL_MODE.md`: ten stop/start cycles with `server show` and `virsh list --all` agreeing after each; then remove the guest out of band and confirm the next start succeeds with the repair on the action record (FR-022, FR-023, FR-025, FR-025b)
- [X] T051 [US4] Run the R2 diagnostic during the host pass and record the outcome in `BASELINE.md`: log the libvirt URI and `DomainGetState` around the stop, call `DomainLookupByUUID` immediately after, and compare `virsh -c <the URI Lightstack actually used>` rather than the host default. The root cause of the original disappearance is still open; the fix does not depend on it, but the answer should be written down
- [X] T060 [US4] **Found by the T052 contract run on the real-mode host, 2026-09-02.** `suspend`, `resume` and `shelve` scanned the nullable `instances.libvirt_domain_id` into a plain `string`, so pgx refused any row where it was NULL — which is every server until a guest is defined, and every stub-mode server forever. The action answered `500 database error`, and the failing contract test (`action_by_name_test.go`) made it read as a name-resolution defect when the name had resolved correctly. Add a `guestDomain(*string)` helper in `internal/nova/advanced_actions.go` and scan through it, matching what `lifecycleAction` already does with `any`; pin it with a fixture that ASSERTS the column is NULL so the test cannot stop covering the case
- [X] T052 [US4] Confirm `test/contract/nova` no longer hangs against a stack where stop/start has been exercised, which is the suspected cause of the sibling harness ticket's hang

### T053–T057 — found by the T050 host pass, 2026-09-01

The R1 probe failed, and not in the shape T042–T048 anticipated. The record was being written from the
action's *intent* rather than its *outcome*, so all three of these were live at once: an optimistic stop,
a refusal that reported a fault and latched, and a sweep that recognised only one direction of drift.

- [X] T053 [US4] Add `waitForShutoff` and `RunStateOf` to `pkg/hypervisor/runstate.go`, tests first: poll a state-reader until the guest is down or a grace period expires, treating `SHUTDOWN` as still-up, an absent domain as down, and an unreadable state as no evidence. Three-valued `RunState` so "could not tell" cannot be mistaken for "off" (FR-022a)
- [X] T054 [US4] Rework `stopVMReal` in `pkg/hypervisor/libvirt.go` to ask, wait, then insist — `DomainShutdown`, poll via T053, `DomainDestroy` once the grace period expires — and return early when the guest is already down. Add `shutdown_timeout` to `NovaConfig`, the chart and `docs/CONFIGURATION.md`, defaulting to upstream's 60s, and widen the lifecycle request context past it (FR-022a, FR-022b)
- [X] T055 [US4] In `internal/nova/handlers.go` `lifecycleAction`, refuse `os-stop` on a guest already down and `os-start` on one already up with `common.NewOperationConflictError`, persisting the OBSERVED state before answering. Check the hypervisor rather than the record — a record that is already wrong must not decide whether a client may act — and let an unreadable state fall through to the action so a blip is not a refusal and an absent guest still reaches the self-heal (FR-026c)
- [X] T056 [US4] Widen the sweep in `internal/nova/state_reconciler.go` from "guest missing" to "guest disagrees": select `ACTIVE` and `SHUTOFF` rows, compare the observed run state, and correct in either direction. Judge no other status — the hypervisor has no opinion on `BUILD`, `ERROR` or `SHELVED`. Guard the update on the status the row was read with (FR-024e)
- [X] T057 [US4] Fix `getVMStateReal`'s power state: it returned libvirt's own enum, where `SHUTOFF` is 5, but a client reads nova's `power_state`, where a stopped server is 4. Latent until T056 made the sweep consume it
- [X] T059 [US4] **Found by the second R1 run, same day.** With the grace period at upstream's 60s and `createNovaServer`'s `WriteTimeout` also 60s, every forced power-off reached its response write at or past the deadline, so net/http closed the connection unanswered — the stop worked and could not say so, and the client's retry got a truthful `409`. Define `HTTPWriteTimeout`, `LifecycleResponseSlack` and `MaxShutdownTimeout` together in `internal/common/config.go`, clamp `shutdown_timeout` to the ceiling in both `LoadConfig` and `guestShutdownGrace`, drop nova's duplicate copies of both numbers, and lower the default to 30s — gunicorn's and most ingresses' default read timeout. Test the invariant as arithmetic on the constants, which is what would have caught it before the host did (FR-022b)
- [X] T058 [US4] Host-verify T053–T059 through `HOST_TESTING_REAL_MODE.md` R13, including a re-run of R1. `stopVMReal` needs a real libvirt connection, so the shutdown/wait/force *wiring* is not reachable by unit test — only its policy is. R13a and R13e are the steps nothing else covers

**Checkpoint**: stop/start is trustworthy and the record cannot silently disagree with the hypervisor.

---

## Phase 6: User Story 3 — A server never reports healthy without its network (Priority: P1)

**Goal**: a requested network that cannot be attached never yields a server reporting itself as running,
and the reason is readable from the server itself.

**Independent Test**: request an unknown or unusable network — refused before anything is created.
Induce an allocation failure — the server lands in a failed state with a readable reason.

### Tests for User Story 3

- [X] T053 [P] [US3] Contract test in `test/contract/nova/boot_network_failure_test.go` asserting a create naming a nonexistent network is refused with a bad-request naming the network, and no instance row is created (FR-017, FR-016a). **Confirm RED.**
- [X] T054 [P] [US3] Contract test in `test/contract/nova/boot_network_failure_test.go` asserting a create naming another project's network is refused the same way (FR-017). **Confirm RED.**
- [X] T055 [P] [US3] Unit test in `internal/nova/boot_failure_test.go` asserting a post-accept allocation failure yields `ERROR` with a populated `fault` object, and never `ACTIVE` with an empty address map (FR-015, FR-016, FR-018). **Confirm RED — the boot continues today.**
- [X] T056 [P] [US3] Unit test in `internal/nova/boot_failure_test.go` asserting a partial failure across several requested networks is treated as a total failure (FR-020), and that a server requesting no network is unaffected (FR-021). **Confirm RED.**
- [X] T057 [P] [US3] Unit test in `internal/nova/boot_failure_test.go` asserting that after a failed boot nothing is left unowned and the failed server is still **deletable by its owner** — an undeletable `ERROR` server is the failure mode this requirement exists to prevent (FR-019). **Confirm RED.**

### Implementation for User Story 3

- [X] T058 [US3] Create migration `migrations/067_instance_fault.up.sql` and `.down.sql` adding nullable `instances.fault_code`, `fault_message` and `fault_created`, per data-model.md (FR-016)
- [X] T059 [US3] Pre-flight the requested networks in `CreateServer` (`internal/nova/handlers.go:394`) before the instance row is inserted, alongside the existing flavor, quota, keypair and security-group validation. Each requested network must exist and be usable by the caller — reuse the network service's attach rule, the one the port allocation applies (owner or shared; an unshared external network for administrators only, as upstream), do not write a second one (FR-017, FR-016a)
- [X] T060 [US3] Replace the fail-open at `internal/nova/handlers.go:664` (`// Continue with empty networks rather than failing`) with a transition to `ERROR` that records the reason (FR-015, FR-016, FR-020)
- [X] T061 [US3] Extend `svc.markInstanceError` (`internal/nova/handlers.go:385`) to persist a fault code, message and timestamp — it records no reason today (FR-016)
- [X] T062 [US3] Emit upstream's `fault` object from the server-detail builder in `internal/nova/handlers.go` when when the server is in `ERROR` and a fault is recorded, omitting `details` deliberately (FR-016, research.md R4)
- [X] T063 [US3] In `internal/nova/handlers.go` `CreateServer`, release or attribute anything created before the failure, and confirm the failed server remains deletable by its owner (FR-019)
- [ ] T064 [US3] Real-mode host validation, recorded in `docs/specs/025-nova-gaps-client-discovery/HOST_TESTING_REAL_MODE.md`: induce a port-allocation failure, confirm the server never reports `ACTIVE`, and confirm `server show` surfaces the reason (FR-015, FR-016, FR-018)

**Checkpoint**: a boot either gets its network or says why not.

---

## Phase 7: User Story 5 — Address an action by name (Priority: P2)

**Goal**: every action on the server-action endpoint accepts the same reference forms as the rest of the
compute surface, answering *absent* rather than a server error for an unknown name.

**Independent Test**: drive each action by name directly, bypassing the client's own name resolution —
each succeeds or reports the resource absent; none returns a server error.

### Tests for User Story 5

- [X] T065 [P] [US5] Contract test in `test/contract/nova/action_by_name_test.go` driving **every** action in the `serverActions` table by name over the API directly, asserting none returns a server error (FR-029, FR-030, FR-033). **Confirm RED.** A CLI-driven test cannot reproduce this — the client resolves names before the request leaves the machine
- [X] T066 [P] [US5] Contract test in `test/contract/nova/action_by_name_test.go` asserting an unknown name reports *absent*, and that a name matching a server in another project also reports *absent* for a non-administrator (FR-030, FR-032). **Confirm RED.**

### Implementation for User Story 5

- [X] T067 [US5] Change the signatures of the 17 handlers in `internal/nova/advanced_actions.go` from `func(c *gin.Context)` to `func(c *gin.Context, instanceID, projectID string)` and delete their `c.Param("id")` reads: `SuspendInstance:20`, `ResumeInstance:92`, `ShelveInstance:163`, `UnshelveInstance:236`, `ResizeInstance:283`, `ConfirmResizeInstance:407`, `RevertResizeInstance:434`, `EvacuateInstance:486`, `MigrateInstance:521`, `LiveMigrateInstance:609`, `AddSecurityGroup:780`, `RemoveSecurityGroup:868`, `ChangePassword:974`, `RestoreInstance:1053`, `CreateBackupAction:1096`, `ResetStateAction:1236`, `ResetNetworkAction:1301` (FR-029, FR-031, FR-033)
- [X] T068 [US5] Update the corresponding `serverActions` entries in `internal/nova/handlers.go:1502-1535` from `_, _` to `id, proj`, passing through the values the dispatcher already resolved. **Add no lookups** — resolving inside a handler would resolve twice and reintroduce the divergence the shared resolver removed (FR-031, research.md R5)
- [X] T069 [US5] Leave the dispatcher's resolve-and-rebind at `internal/nova/handlers.go:1615-1621` untouched, and confirm the administrative override by identifier still behaves identically (FR-032)
- [X] T070 [US5] Confirm by grep over `internal/nova/` that no handler in T067's list is reachable by any route other than the action dispatcher, so no call site needs its own resolution (FR-031)

**Checkpoint**: name addressing behaves the same everywhere on the compute surface.

---

## Phase 8: User Story 6 — See a floating IP on its server (Priority: P2)

**Goal**: an associated floating IP appears in the server's address map, tagged as floating.

**Independent Test**: associate a floating IP, then read the server — both addresses appear under the
network, each labelled with its kind.

### Tests for User Story 6

- [X] T071 [P] [US6] Unit test in `internal/nova/addresses_test.go` asserting the address map carries both a fixed and a floating entry under the same network, each tagged from the address rather than a constant (FR-034, FR-035). **Confirm RED — the type is hardcoded `"fixed"`.**
- [X] T072 [P] [US6] Unit test in `internal/nova/addresses_test.go` asserting a server with no floating address reports today's shape unchanged (FR-037), an unassociated floating address appears on no server (FR-038), and two floating addresses on one port both appear (FR-034). **Confirm RED where applicable.**
- [X] T073 [P] [US6] Contract test in `test/contract/nova/server_addresses_test.go` asserting the single-server read and the server listing report identical addresses (FR-036). **Confirm RED.**
- [X] T074 [P] [US6] Unit test in `internal/nova/addresses_test.go` asserting an **administrator reading another project's server** sees that server's floating addresses. `getInstanceAddresses` takes the instance's **owning** project deliberately — its own comment documents that passing the caller's project returns an empty map — so the new join must widen with its parent (FR-039). **Confirm RED.**

### Implementation for User Story 6

- [X] T075 [US6] Extend the query in `getInstanceAddresses` (`internal/nova/handlers.go:1158`) to join `floating_ips` on `port_id`, selecting `floating_ip_address` (FR-034). The existing index on `floating_ips(port_id)` covers this
- [X] T076 [US6] Replace the hardcoded `"OS-EXT-IPS:type": "fixed"` at `internal/nova/handlers.go:1191` with the per-entry kind (FR-035)
- [ ] T077 [US6] Real-mode host validation, recorded in `docs/specs/025-nova-gaps-client-discovery/HOST_TESTING_REAL_MODE.md`, with a router, an external network and an associated floating IP: `server show`, `server list` and the dashboard's instance list all report both addresses (FR-034, FR-036)

**Checkpoint**: the result of associating a floating IP is visible where users look for it.

---

## Phase 9: User Story 7 — Trust the usage numbers and the action history (Priority: P2)

**Goal**: usage counts every server the project owns, and a deleted server's action history survives,
is readable by its owner and administrators, and is bounded by age.

**Independent Test**: compare reported usage against the project's servers — they agree. Create and
delete a server, then read its history as the owner, as an administrator, and as another project.

### Tests for User Story 7

- [X] T078 [P] [US7] Contract test in `test/contract/nova/limits_usage_test.go` asserting reported instance, core and memory usage matches the project's actual servers and agrees with the quota views (FR-040, FR-041). **Confirm RED by mutation if it passes today** — the filter is dead, so this may be GREEN until something writes that status
- [X] T079 [P] [US7] Contract test in `test/contract/nova/deleted_action_history_test.go` asserting a deleted server's action history is returned to its owner and to an administrator, authorized from the ownership recorded with each action rather than from the vanished server row (FR-043, FR-043a). **Confirm RED — the read resolves the instance first and answers absent to everyone.**
- [X] T080 [P] [US7] Contract test in `test/contract/nova/deleted_action_history_test.go` asserting another project receives the same *absent* answer and cannot distinguish a deleted server from one that never existed (FR-043b), and that the answer is consistent whether or not the server still exists (FR-043c). **Confirm RED.**
- [X] T081 [P] [US7] Contract test in `test/contract/nova/deleted_action_history_test.go` asserting a request below microversion 2.21 does **not** receive a deleted server's history, and that an absent header is treated as the advertised current version (research.md R7). **Confirm RED.**
- [X] T082 [P] [US7] Unit test in `internal/nova/retention_test.go` asserting records older than the retention age are reclaimed, that a live server's old records are not (FR-042a, FR-042c), and that deleting a server leaves its history intact (FR-042). **Confirm RED.**

### Implementation for User Story 7

- [X] T083 [US7] Create migration `migrations/068_instance_actions_time_index.up.sql` and `.down.sql` adding an index on `instance_actions(start_time)`, per data-model.md (FR-042a)
- [X] T084 [US7] Drop `AND i.status != 'DELETED'` from the usage query in `GetLimits` (`internal/nova/handlers.go:2164`) — the only remaining occurrence, and nothing writes that status (FR-040, FR-041)
- [X] T085 [US7] Comment the deliberate absence of `REFERENCES` on `instance_actions.instance_id` in `migrations/024_nova_instance_actions.up.sql` or the model, so the retention decision is not later "fixed" into a cascade (FR-042)
- [X] T086 [US7] Add a minimal requested-microversion reader in `internal/nova/` — `OpenStack-API-Version`, else `X-OpenStack-Nova-API-Version`, else the advertised current — and document that this is the product's first microversion gate, with generalisation explicitly out of scope (research.md R7)
- [X] T087 [US7] Change `ListInstanceActions` (`internal/nova/diagnostics.go:68`) so a deleted server's history is served to the owning project and to administrators, authorizing from `instance_actions.project_id` once the server row is gone, gated on microversion 2.21 (FR-043, FR-043a, FR-043c)
- [X] T088 [US7] In `internal/nova/diagnostics.go`, ensure a caller who is neither owner nor administrator receives the same *absent* answer given today, revealing nothing about whether the server existed (FR-043b)
- [X] T089 [US7] Implement the retention sweep in `internal/nova/state_reconciler.go` on the object created in T047: `DELETE FROM instance_actions WHERE start_time < now() - retention`, never touching the history of a server that still exists whatever the record's age, and never interfering with serving requests (FR-042a, FR-042c, FR-044)
- [X] T090 [US7] Add a test pinning that all three delete paths leave the history alone — `DeleteServer` (`internal/nova/handlers.go:1433`) and `ForceDeleteInstanceAction` (`:2683`, `:2716`) — i.e. that no change was needed there (FR-042)
- [X] T091 [US7] Measure with a short retention age configured in `config/lightstack.yaml`: post-deletion records present, over-age records gone within one interval, a live server's old records untouched (FR-044)

**Checkpoint**: the numbers mean what they say and the audit trail is both kept and bounded.

---

## Phase 10: User Story 9 — Boot from a new volume (Priority: P2)

**Goal**: the dashboard's default launch path creates the volume it says it will, at the requested size,
honouring delete-on-termination.

**Independent Test**: launch requesting a new volume from an image with an explicit size — the server
boots from a bootable volume of that size, attached as its root disk.

**Sequenced after US3 and US4**: FR-055 routes a volume-creation failure through US3's failure path, and
this touches the boot path US4 refactored.

### Tests for User Story 9

- [X] T092 [P] [US9] Contract test in `test/contract/nova/boot_from_new_volume_test.go` asserting a root `source_type=image, destination_type=volume` mapping creates a bootable volume from the image and the server boots from it (FR-049). **Confirm RED — the image is booted directly and no volume is created.**
- [X] T093 [P] [US9] Contract test in `test/contract/nova/boot_from_new_volume_test.go` asserting a requested `volume_size` is honoured (FR-050), an omitted one yields the flavor's disk floored at the image minimum and never zero (FR-051, FR-051a), and a size below **either** the image's `min_disk_gb` **or** its actual `size_bytes` — whichever is larger — is refused before any volume exists, for both requested and defaulted sizes (FR-052, FR-052a). **Confirm RED — `volume_size` is not parsed at all.**
- [X] T094 [P] [US9] Contract test in `test/contract/nova/boot_from_new_volume_test.go` asserting `delete_on_termination` is honoured in both directions on server deletion (FR-053). **Confirm RED.**
- [X] T095 [P] [US9] Contract test in `test/contract/nova/boot_from_new_volume_test.go` asserting an explicit `imageRef`, and an existing boot volume, behave exactly as today (FR-056). **Should pass — this is the regression guard.**
- [X] T096 [P] [US9] Contract test in `test/contract/nova/boot_from_new_volume_test.go` asserting that when the volume cannot be created the boot **fails visibly** with a fault, and never falls back to booting the image directly — the silent-downgrade shape this whole feature exists to remove (FR-055). **Confirm RED.**

### Implementation for User Story 9

- [X] T097 [US9] Add `VolumeSize` to the `BlockDeviceMappingV2` struct in `internal/nova/handlers.go:314-320` — the field is absent today, so the attribute cannot be honoured (FR-050)
- [X] T098 [US9] Add a narrow `CinderService` interface to `internal/nova/handlers.go` with one method to create a bootable volume from an image, mirroring the existing `NeutronService` precedent, and add the field to the `Service` struct (research.md R8)
- [X] T099 [US9] Implement the interface over the block-storage service's existing create-from-image path (`internal/cinder/volumes.go:443-472`, which already sets `source_image_id` and `bootable`) — reuse it, do not reimplement volume creation (FR-049, FR-054)
- [X] T100 [US9] Wire the implementation into the compute service in `cmd/lightstack/main.go` alongside the existing Neutron wiring (research.md R8)
- [X] T101 [US9] For a root image-to-volume mapping, create the volume and boot from it instead of adopting the image reference in `bootImageRefFromBDM` (`internal/nova/handlers.go:370`), attaching it as the root device through the existing boot-from-volume path at `:716-780` (FR-049, FR-054)
- [X] T102 [US9] Apply the size rule in `internal/nova/handlers.go` `CreateServer`: requested size honoured; omitted size defaults to the flavor's `disk_gb` floored at the image's `min_disk_gb`; a size below either the image's `min_disk_gb` or its actual `size_bytes` — whichever is larger — refused before a volume is created, whether requested or defaulted (FR-050, FR-051, FR-051a, FR-052, FR-052a)
- [X] T103 [US9] In `internal/nova/handlers.go`, route a volume-creation failure through US3's failure path rather than falling back to an image boot (FR-055)
- [X] T104 [US9] Confirm `delete_on_termination` works in both directions — `DeleteServer` already handles it for attached volumes at `internal/nova/handlers.go:1392-1400` (FR-053)
- [X] T105 [US9] Define and verify stub-mode behaviour for `internal/nova/handlers.go` + `internal/cinder/volumes.go`: the volume row and attachment are created with no backing file, matching the block-storage service's existing stub behaviour
- [X] T106 [US9] Replace the `LIMITATION:` comment at `internal/nova/handlers.go:364-369`, which documents the downgrade this story removes
- [ ] T107 [US9] Real-mode host validation in `docs/specs/025-nova-gaps-client-discovery/HOST_TESTING_REAL_MODE.md`, following SPEC-020's runbook: the dashboard's default launch yields a bootable volume of the requested size attached as root, and deletion matches the toggle (FR-049, FR-053, FR-054)

**Checkpoint**: the dashboard's default launch path delivers what it promises.

---

## Phase 11: User Story 8 — See who acted on a tenant's server (Priority: P3)

**Goal**: the access log names the operator who acted, not the tenant acted upon.

**Independent Test**: as an administrator, act on another project's server, then read the service log —
the line identifies the caller's own project, and the target remains identifiable.

### Tests for User Story 8

- [X] T108 [P] [US8] Unit test in `internal/middleware/logging_test.go` asserting the completed-request line carries the **caller's** project for a request whose handler rebinds the context project, and that `user_id` is unchanged (FR-045, FR-047). **Confirm RED.**
- [X] T109 [P] [US8] Unit test in `internal/middleware/logging_test.go` asserting an ordinary same-project request logs exactly as today — no extra field appears (FR-047, FR-046a). **Should pass — regression guard.**
- [X] T110 [P] [US8] Unit test in `internal/middleware/logging_test.go` asserting a cross-project action's log line carries **both** the caller's project and the target project, so the question "who touched tenant X" is answerable from the line alone with no stored record consulted (FR-046, SC-012). **Confirm RED.**

### Implementation for User Story 8

- [X] T111 [US8] Capture the caller's project in `internal/middleware/logging.go` **before** `c.Next()` and log that as the caller — it is read at `:101`, after the handler, which is why it picks up the rebind (FR-045)
- [X] T112 [US8] In `internal/middleware/logging.go`, emit the project acted upon on the **same log line** as a distinct field alongside the caller's, populated only when the two differ — detected by comparing the project captured before `c.Next()` with the one present after. Never replace or rename the existing caller field (FR-046, FR-046a)
- [X] T113 [US8] Verify the rebind at `internal/nova/handlers.go:1619` is untouched and the cross-project override still works, with the action record still attributed to the owning tenant — removing the rebind would break the override (FR-048)
- [X] T114 [US8] Validation in `test/nova_gaps_test.sh` (stub) or the real-mode runbook: an administrator acts on another project's server; the log names the operator's project and the action record names the owner (FR-045, FR-046, FR-048)

**Checkpoint**: all nine stories independently functional.

---

## Phase 12: Polish & Cross-Cutting Concerns

- [X] T115 [P] Document the `LIGHTSTACK_ENDPOINT_HOST` precedence change in `docs/CONFIGURATION.md` — from "always wins" to "fallback when the request carries no host", with no opt-out, flagged as a behavioural change for existing deployments including the ingress case (FR-003a)
- [X] T116 [P] Document both new settings in `docs/CONFIGURATION.md` with their defaults and their effect, noting they must arrive through the rendered `lightstack.yaml` rather than the environment (FR-024d, FR-042b)
- [X] T117 [P] Add chart tests under `charts/lightstack/tests/` covering the two new values and their schema constraints
- [X] T118 [P] File the follow-up tickets recorded as out of scope: volume creation is unmetered (`CheckQuota` has zero hits in `internal/cinder/`); `RestoreInstance` gates on `SOFT_DELETED`, a status nothing writes; general microversion negotiation; the per-instance address query in server listings
- [X] T119 Verify both migrations apply and roll back cleanly with `make migrate` and the corresponding down step
- [X] T120 Run the full gates: `make fmt`, `make lint`, `go test ./...`, and both contract suites, comparing against the T002 baseline
- [X] T121 Run the complete quickstart.md validation end to end in stub mode
- [ ] T122 Run the complete quickstart.md validation in real mode on k3s for US3, US4, US6 and US9 — one stack, one router, one external network, one floating IP
- [X] T123 Write the real-mode runbook `docs/specs/025-nova-gaps-client-discovery/HOST_TESTING_REAL_MODE.md`, zsh-copy-paste-safe, covering every story that needs a host
- [X] T124 Record the outcome of the R2 root-cause diagnostic and any Constitution deviation actually taken in `BASELINE.md`
- [X] T125 Confirm every check written for this feature can actually fail — mutate each fix in turn and verify the corresponding test goes red
- [ ] T126 Branch review before merge

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: no dependencies
- **Foundational (Phase 2)**: depends on Setup — **BLOCKS every user story.** T005 changes every
  advertised self-link in the product, so landing it first means one round of expectation churn
  instead of nine; T007 puts both configuration settings in one file so US4 and US7 never edit it
  concurrently
- **US1 (Phase 3)**, **US2 (Phase 4)**: depend only on Foundational
- **US4 (Phase 5)**: depends only on Foundational
- **US3 (Phase 6)**: depends on **US4's T040** (the `VMSpec` extraction exposes the
  requested-network read) and supplies the fault machinery US4's T045 needs — land T040 first, then
  US3, then close US4's T045
- **US5 (Phase 7)**, **US6 (Phase 8)**, **US7 (Phase 9)**: depend only on Foundational; US7's T089
  depends on **US4's T047** (the reconciler object)
- **US9 (Phase 10)**: depends on **US3** (FR-055 routes failures through it) and touches the boot path US4 refactored
- **US8 (Phase 11)**: depends only on Foundational; verification of FR-048 is easiest after US5 exists
- **Polish (Phase 12)**: depends on all desired stories

### The one genuine cross-story coupling

US3 and US4 interlock and cannot be fully separated: US4's T040 unblocks US3, and US3's T061 unblocks
US4's T045. Land in the order **T040 → US3 → T045**. Everything else is independent.

### Within Each User Story

- Tests written and **failing** before implementation (Constitution, non-negotiable)
- Migration before the code that reads the new column
- Shared helper before its callers
- Host validation last

### Parallel Opportunities

- T008 and T009 in Phase 2 after T007
- All of US1's tests (T010–T015) together; then T017–T020 together — four different files, no shared state
- US2's T028 and T029 together
- All of US4's tests (T032–T039) together — eight files-worth of assertions, all of which must fail first
- All of US5's, US6's, US7's and US9's tests together within their phases
- **US2, US5, US6 and US8 are mutually independent** and independent of the US3/US4 pair — the best
  parallel block in the feature
- Polish T115–T118 together

---

## Parallel Example: User Story 4

```bash
# All eight tests first — must all fail (or be proven by mutation) before any implementation:
Task: "Real-mode stop persists SHUTOFF in internal/nova/lifecycle_test.go"
Task: "Real-mode start persists ACTIVE in internal/nova/lifecycle_test.go"
Task: "Three failure outcomes map to three statuses, reboot included, in internal/nova/lifecycle_test.go"
Task: "Self-heal rebuilds from stored state in internal/nova/selfheal_test.go"
Task: "Reconciler corrects an absent guest in internal/nova/state_reconciler_test.go"
Task: "Zero hypervisor calls on a server read and listing in internal/nova/lifecycle_test.go"
Task: "Failed self-heal reports action-impossible in internal/nova/selfheal_test.go"
Task: "Stub mode unchanged in internal/nova/lifecycle_test.go"
```

## Parallel Example: the independent block

```bash
# Once Phase 2 is done, these four stories share no files:
Task: "US2 — identity self-links in internal/keystone/"
Task: "US5 — 17 handler signatures in internal/nova/advanced_actions.go"
Task: "US6 — floating IPs in getInstanceAddresses"
Task: "US8 — caller project in internal/middleware/logging.go"
```

---

## Implementation Strategy

### MVP (Foundational + US1)

1. Phase 1 Setup — trustworthy baseline
2. Phase 2 Foundational — one self-link rule, one configuration surface
3. Phase 3 US1 — the compute version document and the product-wide link sweep
4. **STOP and VALIDATE**: a stock client lists flavors with nothing pinned, and an endpoint override is honoured

That is the smallest increment a user can feel: the client stops crashing.

### Incremental Delivery

1. Foundational + US1 → a stock client works (MVP)
2. + US2 → identity read-back works on the older client, and the project's own runbooks stop degrading
3. + US4 → stop/start trustworthy, record cannot lie
4. + US3 → a boot never claims health it does not have
5. + US5, US6, US7, US8 → fidelity and audit gaps closed, independently deliverable in any order
6. + US9 → the dashboard's default launch path delivers a volume

### Suggested Parallel Team Split

- **Developer A**: Foundational, then US1, then US2 — the self-link thread end to end
- **Developer B**: US4, then US3, then US9 — the compute-state thread, which owns the only real coupling
- **Developer C**: US5, US6, US7, US8 — four independent surfaces

---

## Notes

- `[P]` = different files, no dependency on an incomplete task
- Every task cites the FRs it satisfies; all 80 are covered across the phases, and 78 of the 80 have
  a test or verification task (the 3 without are a documentation requirement, a parent goal, and a
  negative invariant)
- **RED before GREEN is non-negotiable** here, and T125 exists because the sibling feature's host pass
  found three checks that could not fail
- The `/cl.analyze` pass found eight requirements with an implementation task but no verification task.
  All eight now have one — the pattern to avoid is a task whose verb is "confirm", which reads as done
  without asserting anything
- Two migrations, both reversible, each landing with the story that needs it — never up front, so a
  partial merge leaves no unused column
- Commit after each task or logical group
