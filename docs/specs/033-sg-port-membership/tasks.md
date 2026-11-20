---

description: "Task list for SPEC-033 — server security-group membership reaches the ports"
---

# Tasks: Server security-group membership reaches the ports

**Feature**: `033-sg-port-membership` (cloud-storage#579)
**Input**: Design documents from `docs/specs/033-sg-port-membership/`
**Prerequisites**: plan.md and spec.md (required); research.md (R1–R53), data-model.md, contracts/ (8 files) and quickstart.md (present). Round 2 (T058–T168) starts at Phase 11. Constitution **v1.5.0**, ratified 2026-11-17 at the analysis gate and refined on 2026-11-18 at the second (research R19); the plan's gate is open. Register entry 5 and the two `CLAUDE.md` lines were added with the amendment, so no task carries them.

**Tests**: INCLUDED. The spec requires them (FR-022), as does the constitution's Test-First Development:
1. contract tests with OpenStack clients where an answer changes;
2. the user approves the test strategy before any test is written (T002);
3. every test is seen failing first (RED); a failing compile counts;
4. then the code.

A test marked **lock** passes before and after: it guards behaviour this feature must not break.

**Organization**: by the spec's user stories. Priority order is US1 and US2 (P1); then US3, US4, US6 and US7 (P2); then US5 (P3). US2 needs US1's remove. US6 needs US1's refresh and undo. US7 needs US1's runner, node lock and dataplane seam (T013–T016). US5 has no tasks of its own: its two scenarios are red in T006 and T010, before the code that makes them pass.

**Terms**:
- *report*: a server's `security_groups` in show and detail.
- *a port's groups*: its `port_security_groups` rows.
- *rebuild*: applying a floating-IP filter.
- *converge*: make the node's filter, NAT and address for one floating IP match its record (R23).
- *node lock*: the Neutron service's one lock for the node's floating-IP state, acquired per address with a one-second deadline (R8).
- *seams*: the dataplane interface and the uplink function on the Neutron service (R9), the record read on the reconciler (R24), and the command runner on the `RouterManager` (R6).
- *the fake*: the in-memory implementation of the dataplane interface in `internal/neutron`'s tests (T009, extended in T036).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel: a different file, and no dependency on an incomplete task
- **[Story]**: US1–US7 for story-phase tasks; Setup, Foundational and Polish carry none
- Every task names an exact file path. Line numbers are hints from `d81f081`, the `main` commit this branch is based on; find the code by the function named.

## Path conventions

Single Go project at the repo root (`cmd/`, `internal/`, `pkg/`, `test/`), docs under `docs/`. No `src/` layout (plan.md § Project Structure).

**Test databases** (CLAUDE.md):
- PostgreSQL: `make db-up`, then `LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'`
- SQLite: `LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db'`
- Tests that read the seeded project can race across packages; use `go test -p 1` if they flake.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: a green baseline that SC-008 is measured against, and the approved test strategy.

- [X] T001 Record the baseline in `docs/specs/033-sg-port-membership/baseline-gates.md`. Run each of these and write down pass/fail/skip counts per package, plus any failure that predates this branch:
  - `make build`, `make lint`;
  - `go test ./...` on PostgreSQL and on SQLite (both URLs above);
  - `make test-contract` against a running stub Lightstack (Docker Compose), once with a PostgreSQL and once with a SQLite `database.url`;
  - `./test/security_groups_test.sh` and `./test/neutron_patch_test.sh`. Both delete groups, whose answers FR-008c changes.

  Fix nothing here. A failure that predates the branch is recorded, not fixed.
- [X] T002 Get the test strategy approved before any test is written (constitution, Test-First Development, step 2). Review research.md R16 and every test task in this file with the user. Record the approval in `docs/specs/033-sg-port-membership/baseline-gates.md`, with the date and any change asked for. No later task starts before it.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: the Neutron helpers every write path uses (R2), the report read from the ports (R10), which every story's tests read, and a Nova test fixture with the real Neutron service (R16).

**⚠️ CRITICAL**: no story work starts before T007 is green.

- [X] T003 [P] Write `internal/neutron/port_security_groups_db_test.go` (new, DB-backed: `requireDB(t)` and `testPool` from `network_db_test.go`). Subtests, each on a port in the seeded project:
  - `PortSecurityGroupIDs` returns the port's group ids; a port with none gives an empty slice, not an error.
  - `AddPortSecurityGroup(ctx, tx, port, group)` inserts the row and moves the port's `updated_at`.
  - `RemovePortSecurityGroup(ctx, tx, port, group)` deletes it, moves `updated_at` and reports `true`. A group the port does not carry reports `false`, with no error.
  - `SetPortSecurityGroups(ctx, tx, port, ids)` replaces the list, including with an empty one, and moves `updated_at`.
  - `AddPortSecurityGroup` and `SetPortSecurityGroups` naming the id of a group that no longer exists, as when it was deleted after it was resolved, return the exported not-found error, not a foreign-key error (R22).
  - A rolled-back transaction leaves the rows unchanged.
  - `ResolveSecurityGroup(ctx, q, ref, projectID)`:
    - resolves by name and by id;
    - an id match wins over a group named after that id;
    - another project's group, by name or by id, gives the exported not-found error;
    - a malformed id gives not-found, not a query error.
- [X] T004 Implement the T003 helpers in `internal/neutron/ports.go` (research R2): `PortSecurityGroupIDs`, `AddPortSecurityGroup`, `RemovePortSecurityGroup`, `SetPortSecurityGroups`, `ResolveSecurityGroup` and an exported not-found error.
  - The writers take a `database.Tx`; the readers take a querier such as `subnetQuerier` (`ports.go:668`), so they work on a transaction or the pool.
  - A foreign-key violation on the group (`database.IsForeignKeyViolation`) is returned as the exported not-found error, so every caller answers `404` for a group deleted after it was resolved.
  - `ResolveSecurityGroup` is today's create query (`internal/nova/handlers.go:881-887`): `(id::text = $1 OR name = $1) AND project_id = $2 ORDER BY (id::text = $1) DESC LIMIT 1`. Create switches to it in T029.

  This makes T003 green.
- [X] T005 [P] Write `internal/nova/network_fixture_test.go` (new, helpers only):
  - Factor the `X-Test-*` middleware out of `novaRouterOn` (`internal/nova/testrouter_test.go:31-45`) so the fixture reuses it.
  - `newNovaWithNetwork(t) (*gin.Engine, *Service, *neutron.Service)`: `NewService(testPool, "", "stub", nil)`, `neutron.NewService(testPool, "stub", nil)` set with `SetNeutronService`, Nova's routes behind the middleware.
  - `createTestNetwork(t, projectID) (networkID, subnetID string)`: a network and a /24 subnet, as `internal/nova/ip_allocation_concurrency_test.go:23-34` inserts them. A `t.Cleanup` deletes the ports, interface attachments, subnet and network.
  - `createTestGroup(t, projectID, name) string`, with cleanup.
  - `createTestServer(t, r, projectID, body) string` via `POST /v2.1/servers`. Stub mode has no boot goroutine, so the test allocates ports itself.
  - `allocateTestPort(t, net *neutron.Service, networkID, projectID, serverID string, groupIDs []string) string` via `AllocatePortForInstance`.
- [X] T006 [P] Rework `internal/nova/server_detail_secgroups_test.go` for the report read from the ports (FR-014, FR-015, FR-016; `contracts/server-report.md`). It is RED against `main`. Use the T005 fixture. Cases:
  - one port with `web` → show lists `web` only;
  - one port with `default` and `web` → both;
  - two ports, each in `default` → `default` twice;
  - no port → `security_groups` absent, in show and in detail;
  - a port with no group → absent;
  - a server shaped like v0.6.1's (ports without groups, no `server_security_groups` row) → absent. This is FR-015 and US5's first scenario;
  - a group with an empty name → its id;
  - a group deleted once no port carried it → not listed. This replaces `OrphanDropped`; a group a port carries can no longer be deleted (FR-008c);
  - detail over three servers with different ports → each lists its own entries.

  Keep the create-validation tests as **locks**: `UnknownSecurityGroup_400`, `EmptySecurityGroupName_400`, `NamelessSecurityGroupItem_Ignored`, `EnsureDefault_FreshProject`, `ImplicitDefault_FreshProject`, `EnsureDefaultSecurityGroup_SeedsOnlyWhenCreated`. Where they read the report, allocate a port first. These locks also guard T029's switch of create to the shared resolver.
- [X] T007 Implement the report in `internal/nova/advanced_actions.go` and `internal/nova/handlers.go` (research R10):
  - Replace `securityGroupNamesForServer` and `securityGroupNamesForServers` (`advanced_actions.go:668`, `:697`) with one port-derived query, one entry per row: `SELECT p.device_id::text, COALESCE(NULLIF(sg.name, ''), sg.id::text) FROM ports p JOIN port_security_groups psg ON psg.port_id = p.id JOIN security_groups sg ON sg.id = psg.security_group_id WHERE p.device_id = ANY($1::uuid[]) ORDER BY p.created_at, p.id, sg.name`.
  - `formatSecurityGroups` (`:729`) returns `nil` for none.
  - Detail (`handlers.go:1686-1696`) and show (`:1976-1983`) set `security_groups` only when it is not nil.

  This makes T006 green, and its locks stay green.

**Checkpoint**: the helpers exist, the report reads the ports, and Nova tests can build servers with ports.

---

## Phase 3: User Story 1 — Adding or removing a group changes what reaches the server (Priority: P1) 🎯 MVP

**Goal**: add and remove through the compute API reach every port and the floating-IP filter: atomically, with no window, never reverted, bounded at one second, and undone (compare-and-set) on failure. Covers FR-001–FR-004, FR-006, FR-007, FR-007a and FR-007b for the filter, FR-008 for membership, FR-010's re-assertion (US7 adds its removal), and US5's second scenario.

**Independent Test**: stub mode, add and remove a group on a server with a port; its ports and the report follow. Real mode: quickstart §2 (a), (d), (e).

### Tests for User Story 1 (write first, confirm RED)

- [X] T008 [P] [US1] Write `pkg/networking/ingress_filter_test.go` (new), per `contracts/floating-ip-dataplane.md` (the filter) and research R6–R7. Rework `pkg/networking/ingress_rule_test.go`'s cases into the canonical form and delete the old test.
  - **Pure functions.**
    - `RenderIngressFilter(chain, rules)` returns `-A` lines:
      - the conntrack line first: `-m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT`;
      - `-p tcp -m tcp --dport 22`, the range `--dport 80:90`, `min == max` as one port, `udp`;
      - `-p icmp -m icmp --icmp-type 8`;
      - protocol only: `-p tcp`;
      - a source CIDR before `-p` (`-s 10.0.0.0/8 -p tcp …`), a host source as `/32`, `0.0.0.0/0` dropping `-s`;
      - an empty rule as `-j ACCEPT` alone;
      - `-j DROP` last;
      - no rules: the conntrack line and `DROP` only.
    - `RestorePayload(chain, lines)` returns `*filter`, `:<chain> - [0:0]`, `-F <chain>`, the lines and `COMMIT`, each ending in a newline.
    - `ChainMatches(current, desired []string) bool`:
      - true for equal lines, and the `-N <chain>` line of `iptables -S` is ignored;
      - false for an extra rule, a missing rule, reordered rules, a missing `DROP`, or empty output (no chain).
  - **`EnsureIngressFilter` in real mode, with a fake command runner** (R6, FR-007b). Build a `RouterManager` in a real mode and replace its runner. Cases:
    - **Unchanged.** The runner's `iptables -S` returns exactly the rendering → no `iptables-restore` runs, and only the jump is checked.
    - **Changed or missing.** A changed chain, or a missing one → exactly one `iptables-restore`, carrying the payload on stdin, then the jump.
    - **Restore fails.** The restore's error is returned.
    - **Deadline.** Every command the runner sees carries a deadline of one second at most, the jump's check and append included.
    - **Blocking runner.** A runner that blocks until its context ends → `EnsureIngressFilter` returns an error within about one second.
  - **Stub mode.** No command runs.
- [X] T009 [P] [US1] Write `internal/neutron/fake_dataplane_test.go` (new, helpers only) and `internal/neutron/ingress_refresh_test.go` (new; DB-backed; stub mode with the service's dataplane replaced by the fake).
  - **The fake** implements T014's interface. It keeps, per fixed IP, the last rules applied; records every call in order; and can make any call block on a channel or return an error. T036 extends it.
  - Insert `floating_ips` rows for the ports directly. Cases:
    - **One floating IP.** `RefreshPortFilters(ctx, []string{port})` for a port with one floating IP calls the fake's `EnsureIngressFilter` once, with its fixed IP and the port's rules from the database (a `web` tcp/80 rule gives one rule).
    - **Other counts.** Two floating IPs give two calls; none gives no call.
    - **Emptied port.** A port whose groups were all removed gives one call with no rules: the reply-only filter (FR-007).
    - **Errors.** The fake's error is returned, naming the port and the address.
    - **Node lock (R8).**
      - The fake's call for address X blocks on a channel.
      - A second rebuild, of X or of another address Y, does not reach the fake until the first returns.
      - Change X's port groups in the database while the first call is blocked, then release it: the second rebuild of X carries the new rules.
    - **Lock deadline (FR-007b).** While a fake call stays blocked, a `RefreshPortFilters` returns an error after about one second, without calling the fake.
    - **No uplink.** With the service's uplink function failing, as on a real-mode node without one, `RefreshPortFilters` returns an error naming the port, without calling the fake.
    - **Reconciliation.** One pass (`reconcileNAT`) rebuilds the filter through the same locked path. While a blocked call holds the lock, the pass skips that address, and the next pass rebuilds it.
- [X] T010 [P] [US1] Write `internal/nova/security_group_actions_test.go` (new). Use the T005 fixture, with a test type embedding `*neutron.Service` that overrides `RefreshPortFilters` to record calls, optionally run a callback first, and optionally fail. Cases:
  - **Add.** One port in `default`: add `web` → `202`; the port lists `default` and `web`; the report lists both; `RefreshPortFilters` received the port.
  - **Two ports.** Add and remove change both.
  - **By id.** The same as by name.
  - **Unknown group.** An unknown name → `404`. Another project's group, by name and by id → `404`, and nothing changes.
  - **Administrator.** With `X-Test-Roles: admin`, acting on the tenant's server: the tenant's group by name → `202`. The administrator's own group by id → `404` (register entry 5).
  - **No port.** Add and remove → `404` ("could not be found as device id on any ports"), and nothing changes.
  - **Duplicate.** One of two ports already carries the group → add answers `400`, and neither port changes.
  - **Not carried.** Removing a group no port carries → `404`.
  - **Earlier release (US5, second scenario).** A server shaped like v0.6.1's: one port with no group, no `server_security_groups` row. Add `web` → `202`; the port carries `web`; the report names it. RED on `main`, where add writes only `server_security_groups`.
  - **Failure.** The wrapper fails `RefreshPortFilters`:
    - add → `500`, and the ports list what they listed before;
    - remove → `500`, and the group is back on every port it was removed from;
    - in both, a second `RefreshPortFilters` follows the undo.
  - **Compare-and-set (FR-008).** The wrapper's callback changes one port's list with `SetPortSecurityGroups` (another request), then fails:
    - add → `500`; that port keeps the other request's list, and the other ports are put back;
    - remove → the same.
  - Removing a server's last group is not asserted here; US2 changes it.
- [X] T011 [P] [US1] Rework `test/contract/nova/security_group_actions_test.go` (gophercloud, stub stack). Servers are created on a network with `createNetworkWithSubnet`, `createNamedServerOnNetwork` and `awaitAddresses` (`test/contract/nova/server_addresses_test.go`):
  - `TestNovaAddSecurityGroup_Contract`: `202`; every port from `ports.List(…, ports.ListOpts{DeviceID: server})` has the group's id in `SecurityGroups`; the server's `security_groups` names it.
  - `TestNovaAddSecurityGroupDuplicate_Contract`: on a server with a network, adding `default` → `400` (unchanged answer).
  - `TestNovaRemoveSecurityGroup_Contract`: the group leaves every port.
  - New cases: add by the group's id; add and remove on a server created without a network → `404`.
  - The group's cleanup runs after the server's, since a group a port carries can no longer be deleted (FR-008c).
  - `test/contract/nova/server_actions_test.go` has an older pair, `TestAddSecurityGroup_Contract` and `TestRemoveSecurityGroup_Contract`, that booted without a network. Both now answer `404` there, so they boot on a network through the same helpers. Their assertions are unchanged; T020 handles the remove's guard.
  - `TestNovaActionsByName_Contract` (`test/contract/nova/action_by_name_test.go`) sends both actions to a server addressed by name and reads any `404` as the name not resolving. Its server boots on a network, so the actions find a port.
  - `TestNovaServerReadBack_SecurityGroups_Contract` (`test/contract/server_readback_test.go`) read `default` back from a server without a network. It boots on a network and waits for its port (found by T051).
- [X] T012 [P] [US1] Delete `test/nova_server_actions_test.sh`. No Makefile target, workflow or script ran it; it stopped at its server create for want of an image named `test-image`, and it never failed on a wrong answer. The contract tests send all eight of its actions, and T011 checks every port's groups after add and remove.

### Implementation for User Story 1

- [X] T013 [US1] In `pkg/networking/northsouth.go` (research R6–R7):
  - Add `RenderIngressFilter`, `RestorePayload` and `ChainMatches`.
  - Add a command runner to `RouterManager`: a function field taking a context, the command, its arguments and its stdin. Its default runs `exec.CommandContext`. Every `iptables` and `iptables-restore` call of the filter path, the jump's check and append included, goes through it under a fresh one-second context.
  - Rewrite `EnsureIngressFilter` (`:157`):
    1. In stub mode, return `nil`.
    2. Read `iptables -S <chain>`; a missing chain reads as empty.
    3. If `ChainMatches`, only ensure the `FORWARD` jump.
    4. Otherwise run `iptables-restore --noflush --wait 1` with `RestorePayload` on stdin. On an error, return it: the old chain stays whole. Then ensure the jump.
  - Delete the flush-and-append sequence and `ingressRuleSpec` (`:210`), which the renderer replaces. `RemoveIngressFilter` is unchanged here; T042 rewrites it.

  This makes T008 green.
- [X] T014 [US1] In `internal/neutron/network.go`, add three `Service` fields, all set in `NewService`:
  - `nsLock`, a one-slot channel: the node lock (R8). A helper acquires it with a one-second deadline and returns a release function, or a timeout error.
  - `dataplane`, an unexported interface holding `EnsureIngressFilter(fixedIP, uplink string, rules []networking.IngressRule) error`, set to `routerManager`. T043 adds the NAT, removal and listing methods.
  - `uplink func() (string, error)`: `networking.DefaultUplinkInterface` in real mode, and a function returning `""` in stub mode (research R9).
- [X] T015 [US1] In `internal/neutron/floatingip.go`:
  - `ensureIngressFilter` (`:680`) acquires the node lock (one-second deadline). A timeout is an error naming the address. Inside the lock it reads `portIngressRules` and calls `dataplane.EnsureIngressFilter`.
  - Add `RefreshPortFilters(ctx, portIDs []string) error`. Per port, it reads `SELECT fixed_ip_address FROM floating_ips WHERE port_id = $1 AND fixed_ip_address IS NOT NULL`, then calls `ensureIngressFilter` for each address. The uplink comes from `svc.uplink()`. Its error is a failed change naming the port: it never skips the work (FR-008). It tries every address and returns `errors.Join` of the failures, each naming its port and address.
  - The floating-IP handlers keep calling `ensureIngressFilter` as they do; T043 rewrites them.

  This makes T009 green, with T014 and T016.
- [X] T016 [US1] In `internal/neutron/reconciler.go` (`:133-136`), the per-floating-IP filter step goes through the locked `ensureIngressFilter`. A lock timeout skips that address for this pass and is logged at debug, because the holder is changing it. Unchanged filters are left alone by `EnsureIngressFilter` itself (T013).
  - The pass takes its uplink from `svc.uplink()` (T014) instead of `networking.DefaultUplinkInterface()` (`:122`). Stub-mode tests, T009's reconciliation case among them, then no longer depend on the test machine having a default route. T048 keeps this when it makes the pass converge both ways.
- [X] T017 [US1] In `internal/nova/handlers.go` (`:61`), `NeutronService` gains `RefreshPortFilters(ctx context.Context, portIDs []string) error`. Its doc comment says: the serving node only; stub mode never fails; an error means the old filters still stand. `fakeNeutron` (`internal/nova/boot_failure_test.go:46`) implements it, recording its calls and returning a configurable error.
- [X] T018 [US1] Rewrite `AddSecurityGroup` (`:741`) and `RemoveSecurityGroup` (`:829`) in `internal/nova/advanced_actions.go`, per research R4–R5 and `contracts/compute-security-groups.md`:
  1. In one `database.WithTx`:
     - lock with `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`, then `SELECT id FROM ports WHERE device_id = $1 ORDER BY id FOR UPDATE`. No instance row means the server was deleted meanwhile: `404`;
     - resolve the group with `neutron.ResolveSecurityGroup(ctx, tx, name, projectID)`;
     - no port → `404`, "instance_id `<id>` could not be found as device id on any ports";
     - add: if any port carries the group → `400`; else `neutron.AddPortSecurityGroup` on each port. Its not-found error (a group deleted meanwhile) → `404`;
     - remove: `neutron.RemovePortSecurityGroup` on each port; if none removed one → `404`;
     - keep today's last-group refusal, recomputed on the ports (the server's ports carry one distinct group in total → `400`). US2 removes it.
     - Before committing, record each changed port's resulting list (`PortSecurityGroupIDs`).
  2. After the commit, call `svc.neutronSvc.RefreshPortFilters(ctx, ports)`; a nil `neutronSvc` skips it. On an error:
     - a second transaction takes the same locks in the same order, then undoes this request's change on each port whose current list still equals the recorded one. Add deletes the group's row again; remove inserts it again. A port that differs keeps its list;
     - call `RefreshPortFilters` again and log its error;
     - log the first error, naming the port and address;
     - answer `500`.
  3. Drop every read and write of `server_security_groups` and the two stale comments (`:824`, `:928`). Answer `202`.

  This makes T010 and T011 green.

**Checkpoint**: T008–T011 are green, and the locks still pass.

---

## Phase 4: User Story 2 — Removing the last group closes the server, as upstream (Priority: P1)

**Goal**: FR-005: removing a server's last group answers `202`, and its ports carry no group.

**Independent Test**: remove a server's only group: `202`, its ports list no group, and the server reports none. Real mode: quickstart §2 (c).

### Tests for User Story 2 (write first, confirm RED)

- [X] T019 [P] [US2] In `internal/nova/security_group_actions_test.go`:
  - a server whose only group is `web`, on one port: remove `web` → `202`; the port lists no group; show has no `security_groups`; `RefreshPortFilters` received the port;
  - then add `db` → the port carries `db`, and the report names it;
  - two ports, both carrying only `web`: remove → both empty.
- [X] T020 [P] [US2] In `test/contract/nova/security_group_actions_test.go`:
  - add `TestNovaRemoveLastSecurityGroup_Contract`: a server on a network with only `default`; remove it → `202`; `ports.Get(...).SecurityGroups` is empty; show has no `security_groups`;
  - add `TestNovaReplaceOnlySecurityGroup_Contract`, in Terraform's order: only `a`, remove `a`, then add `b` → both `202`, and the port carries only `b`;
  - delete the guard's mentions from `TestNovaRemoveSecurityGroup_Contract`'s doc comment (`:115-116`) and from `test/contract/nova/server_actions_test.go:387`.

### Implementation for User Story 2

- [X] T021 [US2] Delete the last-group refusal from `RemoveSecurityGroup` in `internal/nova/advanced_actions.go` (T018, step 1). This makes T019 and T020 green. The reply-only filter for an emptied port is already covered by T009.

**Checkpoint**: US1 and US2 together complete the P1 scope.

---

## Phase 5: User Story 3 — An interface attached later is filtered as upstream (Priority: P2)

**Goal**: FR-013: a port attached on a network gets `default`; a port attached by id keeps its groups.

**Independent Test**: attach on a network and attach by id to a server in `web`; read each port and the server.

### Tests for User Story 3 (write first, confirm RED)

- [X] T022 [P] [US3] Write `internal/nova/interface_attach_groups_test.go` (new; T005 fixture). Start from a server with one port in `web`:
  - `POST /v2.1/servers/{id}/os-interface` with the `net_id` of a second network → `200`. The new port lists `default` only, in a fresh project too (the group is created). The report lists `web` and `default`.
  - With the `port_id` of a free port carrying `db` → the port still lists `db`, and the report includes `db`.
  - Then remove `web` → `202`; only the first port changes.
  - `DELETE /v2.1/servers/{id}/os-interface/<db port>` → the report no longer lists `db`, and the port keeps it.
- [X] T023 [P] [US3] Write `test/contract/nova/interface_attach_groups_test.go` (new). Start from a server on one network, then attach an interface on a second network (a raw `POST /servers/{id}/os-interface`):
  - the new port's `SecurityGroups` is `default`'s id alone;
  - the server's `security_groups` names `default`.

### Implementation for User Story 3

- [X] T024 [US3] In `internal/nova/interface_attach.go`, on the `net_id` path:
  - call `neutron.EnsureDefaultSecurityGroup(ctx, svc.pool, projectID)` before the transaction (`:206`);
  - inside the transaction, after the port insert (`:228`), call `neutron.AddPortSecurityGroup(ctx, tx, portID, defaultID)`.

  The `port_id` path is unchanged. This makes T022 and T023 green.

---

## Phase 6: User Story 4 — Create and boot keep their behaviour (Priority: P2)

**Goal**: FR-011 and FR-012: a change made between a boot's ports reaches the later ports; create and the implicit default are unchanged; `server_security_groups` is no longer written (R1); create resolves groups through the shared function (R2).

**Independent Test**: create with and without groups and read the port; change the groups between the first and second port of a two-network boot.

### Tests for User Story 4 (write first, confirm RED)

- [X] T025 [P] [US4] In `internal/neutron/port_security_groups_db_test.go`, test `AllocatePortForInstance(…, securityGroupIDs, copyGroupsFrom, admin)`:
  - **First port.** It carries the given groups; none gives `default` (**lock**, under the new signature).
  - **A later port.** With `copyGroupsFrom` set to the first port, it carries the first port's current groups. After `AddPortSecurityGroup` on the first port in between, the later port carries the added group too (FR-011).
  - **Instance lock.** The allocation locks the instance row (PostgreSQL only; skip on SQLite, where immediate transactions serialise anyway):
    - a transaction holds `SELECT … FROM instances WHERE id = $1 FOR UPDATE` and adds a group to the first port;
    - the allocation runs in a goroutine meanwhile;
    - the holder commits after a short wait;
    - the copy includes the group.
- [X] T026 [P] [US4] Write `internal/nova/boot_groups_test.go` (new), with `newBootService` (`internal/nova/boot_failure_test.go`) and a recording fake. Cases:
  - **Two networks.** A create with `security_groups: web`: the first `AllocatePortForInstance` call gets `web`'s id and an empty copy-from; the second gets no groups and the first port's id.
  - **No groups.** A create without groups: the first call gets `default`'s id (**lock**).
  - **No stored copy.** After a create, `server_security_groups` has no row for the server (R1).
- [X] T027 [P] [US4] Add `TestNovaCreateWithSecurityGroupReachesPort_Contract` to `test/contract/nova/security_group_actions_test.go`. Create on a network:
  - with a non-default group → the port lists it and not `default` (**lock**: on `main` since cloud-storage#524);
  - without groups → `default` only (**lock**).

### Implementation for User Story 4

- [X] T028 [US4] Change `AllocatePortForInstance` in `internal/neutron/ports.go` (`:1596`) and every caller in the same step, so every package keeps compiling:
  - Add `copyGroupsFrom string` after `securityGroupIDs`.
  - Inside the transaction, before `LockSubnet`, run `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`. A missing row is not an error: callers outside a boot allocate for ids without one.
  - When `copyGroupsFrom != ""`, use that port's groups from `PortSecurityGroupIDs(ctx, tx, …)`; an empty list stays empty. Otherwise behave as today.
  - Update `NeutronService` (`internal/nova/handlers.go:62`).
  - Update the production call (`handlers.go:1146`), passing `""` for now; T029 adds the copy-from.
  - Update `fakeNeutron` (`internal/nova/boot_failure_test.go:60`), `internal/nova/ip_allocation_concurrency_test.go:59` and the T005 fixture.

  This makes T025 green.
- [X] T029 [US4] In `internal/nova/handlers.go`:
  - In the boot loop (`:1141-1185`), pass `sgIDs` and `""` for the first network, then `nil` and the first allocation's `PortInfo.ID` for each later network.
  - Create's transaction (`:1026-1048`) stops inserting `server_security_groups`; update its comment (`:1019-1024`).
  - Replace create's inline group resolution (`:855-905`, the query at `:881-887`) with `neutron.ResolveSecurityGroup`. The `default` fallback and the `400` answers stay as they are, so there is one resolver for create, add and remove (R2).

  This makes T026 green. T027 and T006's locks stay green.
  - `internal/nova/security_group_ref_test.go`'s `TestCreateServerResolvesSecurityGroupByIDOrName` read the resolved group from `server_security_groups`, which create no longer writes. It boots on a network with `newBootService` and reads the group from the first allocation instead.

---

## Phase 7: User Story 5 — Servers from earlier releases report the truth (Priority: P3)

**Goal**: FR-015. A server from v0.6.1 or earlier reports no group, and a group added to it reaches its ports.

**Independent Test**: a server and ports shaped like v0.6.1's.

No tasks of its own. Both scenarios are covered where they first fail:
- the first (no `security_groups` key) in T006, red before T007;
- the second (an added group reaches the ports) in T010, red before T018.

**Checkpoint**: green once T007 and T018 are in.

---

## Phase 8: User Story 6 — The Networking API and Horizon change a port's groups (Priority: P2)

**Goal**: FR-017, FR-018, FR-019 and FR-019a: `security_groups` on port create and update, the group listing's `id` and `name`, and Horizon's *Edit Security Groups*.

**Independent Test**: `PUT /v2.0/ports/{id}` with `security_groups`: the port and its server report the new list; Horizon's sequence through the API.

### Tests for User Story 6 (write first, confirm RED)

- [X] T030 [P] [US6] Write `internal/neutron/port_api_security_groups_test.go` (new; DB-backed). Use the Neutron router behind stub auth, as the package's other handler tests do, with the service's dataplane replaced by the fake (T009). Cases:
  - **`PUT /v2.0/ports/{id}`, valid bodies.**
    - `{"port":{"security_groups":["<web>"]}}` → `200`; the port lists `web` only; `updated_at` moved.
    - `[]` → no group; `null` → no group.
    - Absent, with a `name` → the groups are unchanged and the name changes. `name` and `security_groups` together → both apply.
    - A body with only `security_groups` → `200`, no longer `400 no updates provided`.
  - **Errors, in this order:**
    - `["x"]` → `400` "is not a valid UUID";
    - `[a, a]` → `400` "Duplicate items";
    - a missing port with a valid body → `404`;
    - another project's group → `404` `SecurityGroupNotFound`, administrators included.

    Nothing changes in any of them.
  - **A port with a floating IP.**
    - The fake receives the new rules.
    - When it fails → `500`, and the list is back.
    - When the fake's failing call first writes a different list for the port (another request), that list stays.
    - While another change on the node holds the lock (a fake call blocked in a goroutine), the `PUT` answers `500` after about one second and the port's list is back, as T009 does for `RefreshPortFilters`.
  - **`POST /v2.0/ports`.** `[]` → no group; `null` → no group; absent → `default`. Malformed, repeated or foreign ids → `400`, `400`, `404`.
- [X] T031 [P] [US6] Write `internal/neutron/security_group_list_filter_test.go` (new). Groups `a`, `b`, `c` in one project and `x` in another:
  - `?id=a` → `[a]`; `?id=a&id=b` → `a` and `b`;
  - `?name=b` → `[b]`; `?id=a&name=b` → `[]`;
  - `?id=<x>` → `[]`; `?id=not-a-uuid` → `200 []`;
  - no query → `a`, `b`, `c` (**lock**).
- [X] T032 [P] [US6] Write `test/contract/neutron/port_security_groups_test.go` (new; gophercloud). Cases:
  - **Port update and create.** `ports.Update` with `SecurityGroups: &[]string{web}`, then with `&[]string{}`. `ports.Create` with `&[]string{}` → no group.
  - **Listing.** `groups.List` with `ListOpts{Name: …}` → exactly one group. A raw `GET /v2.0/security-groups?id=a&id=b` → exactly those two.
  - **Horizon's *Edit Security Groups* sequence**, on a server with a network:
    1. list its ports by device id;
    2. list the ids found there with a repeated `?id=` → exactly the server's groups;
    3. update each port with a chosen list → the server's `security_groups` reports it.

### Implementation for User Story 6

- [X] T033 [US6] In `internal/neutron/ports.go`, `CreatePort` (`:38`) and `UpdatePort` (`:563`) read `security_groups` as `json.RawMessage` (research R12).
  - **Validation, in this order:**
    1. each entry is a UUID;
    2. no entry is repeated;
    3. for an update, the port exists in the caller's project, else `404`. The update transaction's `SELECT … FOR UPDATE` of the port is this check;
    4. each group is the port's project's, else `404` with `NewResourceNotFoundError("security group", id)`. For an update it is read in the same transaction, after the port's lock (constitution v1.5.0, limit 2). No lock is needed: a group's project never changes, and the foreign key re-checks its existence at the write, which T004 maps to the same not-found error.

    Steps 1 and 2 read only the body and run before any database access.
  - **`CreatePort`.** `[]` or `null` → no rows; absent → `default`, as today.
  - **`UpdatePort`.**
    - In one transaction: lock the port (`SELECT … FOR UPDATE`), read the old list, call `SetPortSecurityGroups`, and apply the other fields. `u.Empty()` (`:599`) counts `security_groups` as an update.
    - After the commit, call `RefreshPortFilters([port])`.
    - On a failure: a second transaction locks the port again and restores the old list only if the port still carries the list just written. Then rebuild again and answer `500`.
    - Otherwise answer with `GetPort`, as today.

  This makes T030 green.
- [X] T034 [US6] In `internal/neutron/ports.go`, `ListSecurityGroups` (`:961`) and `listSecurityGroupRows` (`:926`) build the `WHERE` with `common.SQLFilter`: the project, then `id::text = ANY(…)` over `c.QueryArray("id")`, then `name = …`. This makes T031 green, and T032 with T033.

**Checkpoint**: US6 is independently green.

---

## Phase 9: User Story 7 — Floating-IP and rule changes are in force when the API answers (Priority: P2)

**Goal**: FR-007a and FR-007b for the NAT; FR-008 for rules and floating IPs; FR-008a, FR-008b, FR-008c, FR-008d and FR-008e; FR-010's removal half; SC-011, SC-013, SC-014 and SC-015. Research R20–R25, `contracts/floating-ip-dataplane.md` and `contracts/network-api.md`.

**Independent Test**: stub mode with the fake. Associate, move, disassociate and delete a floating IP, and add and delete a rule: the fake follows before each answer. A failing fake gives `500` with the records unchanged. A reconciliation pass removes NAT and filters that no record holds. Deleting a group in use gives `409`. Real mode: quickstart §2 (g) and (h).

### Tests for User Story 7 (write first, confirm RED)

- [X] T035 [P] [US7] Write `pkg/networking/floating_ip_nat_test.go` (new), per research R23.
  - **Pure functions.**
    - The NAT parser reads `iptables -t nat -S PREROUTING` and `-S OUTPUT` output. It returns every rule of the shape (research R23), with its floating and fixed address and whether it is marked: `-d <F>/32 -m comment --comment lightstack-fip -j DNAT --to-destination <X>`, and the same without the mark, as earlier releases wrote it. A port-forwarding rule for the same address (with `-p` and `--dport`), and any other DNAT, are not returned.
    - The nat payload for associating F with X, given the current rules:
      - none → two `-A` lines (one per hook);
      - marked to Y → `-D` both, `-A` both;
      - unmarked to X → `-D` both, `-A` both (marked);
      - marked to X already → no payload.
    - The payload for a withdrawal is `-D` of every rule for F, marked or unmarked.
    - The filter's removal payload is `-D FORWARD <jump>` for each jump found, then `-F` and `-X` of the chain.
    - The node listing returns every rule of the shape, marked and unmarked, as floating→fixed pairs with the mark noted. It also returns every `LS-NSI-*` chain as its fixed IP, and the uplink's addresses.
  - **Real mode with a fake command runner.**
    - `EnsureFloatingIP`: each hook is read once; one nat restore when something differs and none when it matches. It runs `ip addr add` only when the address is missing, and only after the restore. It reports the fixed IPs the rules it deleted pointed at.
    - `WithdrawFloatingIP`: `ip addr del` first, when the address is present, then the nat restore. It reports the fixed IPs the deleted rules pointed at. Nothing present → no command, no error, nothing reported.
    - `RemoveIngressFilter`: one restore; a missing chain → no command and no error.
    - Every error is returned. Every command carries a deadline of one second at most. A blocking runner fails the call within about one second.
  - **Stub mode.** No command runs, and the listing is empty.
- [X] T036 [P] [US7] Extend the fake in `internal/neutron/fake_dataplane_test.go` with the methods T043 adds. It keeps, per address, its rules of the shape, marked or not, and the uplink's addresses. The NAT methods report the targets they replaced, and the listing returns all of it. Then write `internal/neutron/floating_ip_dataplane_test.go` (new; DB-backed; stub mode with the fake). Cases:
  - **Create on a port** → `201`. The fake saw X's filter, then F's NAT to X, then F's address, in that order. Create without a port → no call.
  - **Move** F from X to Y → `200`. The fake holds Y's filter and F's NAT to Y only, and X's filter is gone. X's filter stays when a second floating IP is still on X, and also when the fake holds an unmarked rule of the shape, with no record, pointing at X.
  - **Disassociate** → `200`. The fake removed F's address, then F's NAT, then X's filter. A description-only update → no call.
  - **Delete** an associated floating IP that has a port forwarding → `204`. One transaction deleted the row and the port forwarding. The fake removed F's address, then F's NAT, then X's filter. Delete an unassociated one → `204`, no call.
  - **Failure**, for each case above with the fake failing its filter, NAT or address call → `500`:
    - after a create, no row is left;
    - after a move or a disassociation, the old association is in the row;
    - after a delete, the row and its port forwarding are back, every column as it was;
    - in every case the fake was converged again from the restored record.
  - **Restoring a delete, as the foreign keys would have.** The fake's failing call first changes the database through the pool (another request):
    - it deletes the port → the floating IP comes back unassociated, and its port forwarding does not (its internal port is gone);
    - it allocates the address to a new floating IP → the deleted one stays deleted, and the new one stays.
  - **No uplink.** With the service's uplink function failing: a create answers `500` and leaves no row; an update answers `500` with the association back; a delete answers `500` with the floating IP back.
  - **A deleted port (research R25).** `DELETE /v2.0/ports/{id}` on a port with an associated floating IP → `204`. In the same transaction, the floating IP lost its port, fixed IP and router, and its status is `DOWN`. The fake removed its address, NAT and filter before the answer.
  - **The port's lock comes first.** PostgreSQL only; immediate transactions serialise this on SQLite anyway.
    1. A test transaction locks the port `FOR UPDATE`.
    2. `DELETE /v2.0/ports/{id}` runs in a goroutine meanwhile.
    3. The test transaction associates a second floating IP with the port and commits after a short wait.

    The deletion, unblocked, disassociates that floating IP too: no port, fixed IP or router, status `DOWN`. If the deletion read the floating IPs before taking the port's lock, it would miss the second one, which would keep a stale fixed IP and `ACTIVE`, with only `port_id` cleared by the foreign key.
  - **A failed boot's release (research R25).** `ReleaseInstancePorts`, which runs only when a boot fails (`internal/nova/handlers.go:1131-1135`), for a server whose port has an associated floating IP → the same. A server's deletion is Nova's and is tested in T041.
  - **A failed removal on deletion.** With the fake failing its NAT call, the port deletion still answers `204`, and the floating IP stays disassociated. The failure is logged naming the floating IP, and the next reconciliation pass removes the NAT.
  - **Compare-and-set.** The fake's failing call first changes the row through the pool (another request). After a create its description changes, and the row stays with it. After a move it is moved again, and that association stays.
  - **Lock deadline.** While another change holds the node lock (a fake call blocked in a goroutine), a `PUT` answers `500` after about one second, and the association is back.
- [X] T037 [P] [US7] Write `internal/neutron/security_group_rule_refresh_test.go` (new; DB-backed; the fake). Three ports: one carries `web` and has a floating IP, one carries `web` without one, one carries `db` with one. Cases:
  - **Add** a tcp/8080 rule to `web` → `201`. Only the first port's fixed IP is rebuilt, and its rules include tcp/8080.
  - **Delete** it → `204`. Rebuilt again, without it.
  - **Failure on add** → `500`. The rule row is gone, and the filter was rebuilt again without it.
  - **Failure on delete** → `500`. The rule row is back with every column as it was (id, `created_at` included), and the filter was rebuilt again with it.
  - **No uplink.** With the service's uplink function failing, an add answers `500` and the rule is gone; a delete answers `500` and the rule is back.
- [X] T038 [P] [US7] Write `internal/neutron/security_group_delete_test.go` (new; DB-backed). Cases:
  - A group a port carries → `409`, type `SecurityGroupInUse`. The group, its rules and the port's list are unchanged.
  - An id of no group → `404`. Another project's group → `404`, administrators included (register entry 5).
  - An unused group → `204`. Its rules are gone, and a rule of another group that names it as its remote group is gone too (**lock**: the foreign key cascades).
  - After the port that carried a group drops it, the delete → `204`.
- [X] T039 [P] [US7] Write `internal/neutron/reconcile_dataplane_test.go` (new; DB-backed; the fake with its listing). One pass (`reconcileNAT`) per case:
  - **Adds.** F is associated with X in the records and the fake is empty → X's filter, F's NAT, F's address, in that order.
  - **Removes.** The fake holds a marked NAT for G to Y, G's address and Y's filter, and no record holds G → G's address, then G's NAT, then Y's filter, are removed.
  - **Orphan filter.** The fake holds a filter for Z that no record associates and no rule reaches → removed.
  - **Earlier release.**
    - An unmarked rule for an associated F → replaced by the marked one.
    - An unmarked rule for an address no record holds, pointing at Z, whose filter no record associates → the rule and Z's filter both stay.
  - **Missing address.** F is associated, its NAT matches, and its address is missing from the fake's uplink → the address is added back.
  - **Failed read.** With `floating_ips` made unavailable (`dbtest.Unavailable`, `internal/database/dbtest/dbtest.go:258`, both backends), the fake holding a marked NAT and a filter → the pass removes nothing.
  - **Partial read.** The reconciler's record-read function returns one association and `complete = false`. The fake also holds a marked NAT with no record → nothing is removed, and the association that was read is ensured.
  - **An unassociated floating IP's leftovers.** A floating IP has a row but no association. The fake holds an unmarked rule of the shape for it, pointing at Z, and its address. Its rule and its address are removed, because its row makes them Lightstack's. Z's filter goes too, unless something else still reaches Z.
  - **No uplink.** With the service's uplink function failing, the pass makes no call and logs an error.
  - **Unchanged.** The fake already matches → no write call, reads only.
  - **Lock.** A request's blocked call holds the node lock → the pass skips that address, finishes the others, and the next pass converges it.
  - **Fresh read.** The record changes between the pass's listing and its step for that address → the step acts on the new record.
- [X] T040 [P] [US7] Write `test/contract/neutron/security_group_delete_test.go` (new; gophercloud, stub stack). Create a group, and a port on a network carrying it (`ports.Create` with `SecurityGroups`):
  - `groups.Delete` → `409`;
  - delete the port, then `groups.Delete` → `204`;
  - `groups.Delete` again → `404`.

- [X] T041 [P] [US7] Write `internal/nova/server_delete_floating_ips_test.go` (new; the T005 fixture, with a test type embedding `*neutron.Service` that records `ConvergeFloatingIPs` and can fail it, as T010's does `RefreshPortFilters`). Insert the `floating_ips` rows directly. Cases (research R25):
  - **A deleted server.** `DELETE /v2.0/servers/{id}` for a server whose boot port has an associated floating IP → `204`. In the deleting transaction the floating IP lost its port, fixed IP and router, and its status is `DOWN`. `ConvergeFloatingIPs` received its address before the answer. RED on `main`, where only the foreign key clears `port_id`.
  - **A port attached by id survives.** A floating IP on a tenant port the server had attached by id stays associated: the deletion detaches that port and does not delete it (`handlers.go:2160-2170`).
  - **A failed convergence.** With `ConvergeFloatingIPs` failing → still `204`; the floating IP stays disassociated, and the failure is logged naming it.
  - **`forceDelete` (FR-008e).** `POST /v2.1/servers/{id}/action` with `{"forceDelete": null}`, on a server with a boot port carrying an associated floating IP and an attached volume. The boot port is deleted, the floating IP disassociated and converged, and the volume released (`available`, no attachment), exactly as the delete case. The answer is as today. RED on `main`, where `forceDelete` deletes only the instance row.
  - **A concurrent deletion.** Two deletions of one server, `DELETE` and `forceDelete` in either order: one deletes, and the other answers `404` once the first has committed.
  - **Lock order.** PostgreSQL only.
    1. A test transaction holds `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`.
    2. `DELETE /v2.0/servers/{id}` runs in a goroutine meanwhile.
    3. A second test transaction locks one of the server's ports `FOR UPDATE` under a short `lock_timeout` (`//sqlcheck:postgres-only`). It succeeds, because the deletion takes the instance first.
    4. Both test transactions commit, and the deletion completes.

### Implementation for User Story 7

- [X] T042 [US7] In `pkg/networking/northsouth.go` (research R23). Everything goes through T013's runner, one second per command.
  - The mark `lightstack-fip`, the parser for rules of the shape (marked or not), and the nat and filter-removal payloads.
  - `EnsureFloatingIP(floatingIP, fixedIP, uplink) ([]string, error)` rewritten:
    1. enable forwarding as today;
    2. read each hook once;
    3. run one `iptables-restore --noflush --wait 1` if anything differs;
    4. add the address if missing.

    It reports the fixed IPs the rules it deleted pointed at.
  - `WithdrawFloatingIP(floatingIP, uplink) ([]string, error)` (no fixed IP): the address first, then one restore deleting every rule of the address's. It reports the fixed IPs they pointed at, and returns its errors.
  - `RemoveIngressFilter(fixedIP)` (no uplink): one restore. It returns its errors.
  - `NorthSouthState(uplink)`: every rule of the shape with its floating and fixed address and its mark, the `LS-NSI-*` fixed IPs, and the uplink's addresses.
  - Stub mode does nothing in any of them.
  - Update the callers whose signatures changed: `internal/neutron/teardown.go:67-68`, and the floating-IP handlers until T043 rewrites them.

  This makes T035 green.
- [X] T043 [US7] In `internal/neutron/network.go` and `internal/neutron/floatingip.go` (research R20, R23):
  - `dataplane` gains `EnsureFloatingIP`, `WithdrawFloatingIP`, `RemoveIngressFilter` and `NorthSouthState`.
  - Add `convergeFloatingIP(ctx, floatingIP string) error`. It runs under the node lock, with the uplink from `svc.uplink()`; that function's error is a failed change.
    1. Read the record: the fixed IP and port it is associated with, or none.
    2. If associated with X: the port's rules, then `EnsureIngressFilter(X)`, then `EnsureFloatingIP(F, X)`. Otherwise `WithdrawFloatingIP(F)`.
    3. For each fixed IP that step 2 reported, call `RemoveIngressFilter` unless a record still associates a floating IP with it, or `NorthSouthState` shows a rule of the shape, marked or not, still reaching it.

    Errors name the floating IP and the address.
  - `CreateFloatingIP` (`:160`):
    - One transaction does the checks and the insert, in this order:
      1. it locks the floating network with `SELECT … FROM networks WHERE id = $1 FOR UPDATE` and checks that it is external, through `requireExternalNetwork` given a querier;
      2. it reads the port, the caller's, with its fixed IP;
      3. it looks the router up as today, then locks it by id in a query of its own, `SELECT id FROM routers WHERE id = $1 AND external_gateway_info IS NOT NULL FOR UPDATE`;
      4. it inserts the row.
    - The address is chosen before the transaction, as today. Only bare trailing `FOR UPDATE` belongs to the shared SQL subset (`internal/database/doc.go:45`); `FOR SHARE` and `FOR UPDATE OF` do not. The port needs no lock: its project never changes, and the foreign key re-checks its existence at the insert.
    - Nothing is programmed before the commit. With a port, converge afterwards.
    - On a failure, a second transaction deletes the row if `port_id`, `fixed_ip_address` and `description` still hold what the insert wrote. Then converge again and answer `500`.
  - `UpdateFloatingIP` (`:409`):
    - One transaction locks, in this order: the new port, `FOR UPDATE`, with the checks of today (the caller's, its fixed IP); its router, found as today and then locked by id, as in the create; and only then the floating IP's row, `FOR UPDATE`. Port deletion takes the port before the floating IPs that name it, so this order cannot deadlock with it. The transaction then reads the old association, writes the new one and commits. A disassociation locks only the row. The best-effort withdrawal of the old association (`:443-451`) goes; convergence removes it.
    - When `port_id` was sent, converge.
    - On a failure, a second transaction locks the row and restores the old `port_id`, `fixed_ip_address`, `router_id` and `status` if the row still holds the new ones. Then converge again and answer `500`.
  - `DeleteFloatingIP` (`:573`):
    - One transaction locks the row (`FOR UPDATE`), reads it and its `port_forwardings` rows in full, and deletes the row; the port forwardings go with it. With an association, converge afterwards.
    - On a failure, a second transaction restores the row and the port forwardings. It applies the foreign keys' delete actions to whatever other requests deleted meanwhile:
      - no project or network → nothing is restored;
      - no port → the floating IP is restored unassociated;
      - no router → it is restored without one;
      - a port forwarding whose internal port is gone stays deleted;
      - the address allocated again → nothing is restored.

      Then converge again and answer `500`.
  - In the first transaction of all three, a foreign-key violation answers `404`, never `500`. In an undo transaction the request already answers `500`, and rows deleted meanwhile are handled by the foreign keys' delete actions, as above.

  This makes T036 green, except its deletion cases (T046).
- [X] T044 [US7] In `internal/neutron/ports.go` (research R21), `CreateSecurityGroupRule` (`:1179`) and `DeleteSecurityGroupRule` (`:1421`). Their LS-SG step stays where it is, and so does the ownership check before it.
  - An add inserts the rule in a transaction that first reads the group, and the remote group when named, both the caller's (`404` otherwise). No row lock: a group's project never changes, and the foreign keys re-check that both still exist at the insert.
  - A delete reads the whole rule row `FOR UPDATE`, scoped to the caller's project, and deletes it in one transaction.
  - After the commit, read the fixed IP and port of every floating IP whose port carries the group (`floating_ips` joined to `port_security_groups`). Call `ensureIngressFilter` for each, with the uplink from `svc.uplink()`; its error is a failed change.
  - On a failure: a second transaction deletes the added rule if it is still there, or re-inserts the deleted rule with every column if its group exists. Then rebuild the same addresses again, log the first error and answer `500`.
  - In the first transaction, a foreign-key violation answers `404`, never `500`.
  - Delete `refreshIngressFiltersForSG` (`floatingip.go:691`).

  This makes T037 green.
- [X] T045 [US7] In `internal/neutron/ports.go`, `DeleteSecurityGroup` (`:1059`, research R22):
  - One transaction: `SELECT … FROM security_groups WHERE id = $1 AND project_id = $2 FOR UPDATE`. No row → `404` (`NewNotFoundError("security group")`). Any `port_security_groups` row → `409` with `common.NewResourceInUseError("security group", "delete security group", …)`, naming the group. Otherwise delete it and commit.
  - Move the LS-SG chain removal after the commit. Its failure is still only logged, a recorded departure that follow-up 2 lists at its new place (T056).
  - In `internal/common/neutron_errors.go`, `neutronInUseTypes` gains `"security group": "SecurityGroupInUse"`.

  This makes T038 and T040 green.
- [X] T046 [US7] In `internal/neutron/ports.go`, `internal/neutron/floatingip.go` and `internal/nova/handlers.go` (`NeutronService`), the floating IPs of a deleted port (research R25):
  - Add `DisassociatePortFloatingIPs(ctx, tx database.Tx, portIDs []string) ([]string, error)`, a package-level helper on the caller's transaction (R2's pattern). It reads the floating IPs whose `port_id = ANY($1)` and clears their `port_id`, `fixed_ip_address` and `router_id`. It also sets `status` to `DOWN`: upstream's port deletion clears the same three columns, and its L3 agent reports `DOWN` afterwards. It returns their addresses.
  - `DeletePort` (`:516`): its single `DELETE` (`:551`) becomes a `database.WithTx`. That transaction locks the port `FOR UPDATE` first, then calls the helper, then deletes the port.
  - `ReleaseInstancePorts` (`:1860`), which a failed boot calls: one transaction locks the instance row, then the server's ports in id order, `FOR UPDATE`, as membership changes do. Then it calls the helper and deletes the ports (`:1891`). No instance row means the server was deleted meanwhile; the release carries on with whatever ports remain.
  - Both, after the commit: `convergeFloatingIP` for each address. A failure is logged naming the floating IP, and the deletion stands, because the port's own teardown has already run without an undo. Reconciliation removes what is left within one pass. This is a recorded departure (follow-up 2).
  - Add `ConvergeFloatingIPs(ctx, floatingIPs []string) error` to the Neutron service in `internal/neutron/floatingip.go`, beside `convergeFloatingIP`, joining its errors. Add it to Nova's `NeutronService` (`internal/nova/handlers.go:61`). `fakeNeutron` (`internal/nova/boot_failure_test.go:46`) records its calls and returns a configurable error.
  - `teardownPortHost` and the DHCP refresh are unchanged.

  This makes T036's deletion cases green.
- [X] T047 [US7] In `internal/nova/handlers.go`, `DeleteServer` (`:2021`) and the `forceDelete` action (`ForceDeleteInstanceAction`, `:3720`), research R25 and FR-008e:
  - Extract `DeleteServer`'s deletion into one function, called by both: console tokens, libvirt domain, the transaction below, the convergence and the file cleanup after it. `ForceDeleteInstanceAction` stops deleting only the instance row, and keeps its own answer.
  - The transaction locks the instance row first, `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`. No row means a concurrent deletion won: `404`, as the exactly-once check answers today (`:2201`). Then it locks the server's ports in id order, `FOR UPDATE`, before it touches volumes or ports. Today it changes volumes and ports before the instance row, the reverse of T018.
  - Before the boot ports' `DELETE` (`:2175`), it calls `neutron.DisassociatePortFloatingIPs` on its transaction for those ports. A tenant port it only detaches (`:2166`) keeps its floating IP, because the port still exists.
  - After the commit, and before the answer, it calls `svc.neutronSvc.ConvergeFloatingIPs` for the returned addresses. A failure is logged naming the floating IP, and the deletion still answers `204`.

  This makes T041 green.
- [X] T048 [US7] In `internal/neutron/reconciler.go`, `reconcileNAT`'s floating-IP part converges both ways (research R24):
  1. Take the uplink from `svc.uplink()`. On its error, log an error and skip the floating-IP part.
  2. `dataplane.NorthSouthState(uplink)` first.
  3. Then every floating IP's row, associated or not, with its association if it has one. The read goes through a function field on the reconciler that defaults to the query. It returns them and whether the read was complete, as `readNetworkIDs` does.
  4. Then one address at a time:
     - `convergeFloatingIP` for every floating IP that has a row or a marked rule. An unassociated one is withdrawn, its unmarked rules included. One with only a marked rule and no row is converged only after a complete read. One with only unmarked rules and no row is left alone.
     - `RemoveIngressFilter`, under the node lock, for each listed fixed IP that no association names and no listed rule of the shape reaches, marked or not, after a complete read only.
  - A lock timeout skips the address.
  - Converge only where the listing differs from the rows: a rule missing, extra or pointing elsewhere, or the address absent or present when it should not be. An unassociated floating IP with no rule and no address has nothing to do. The filter's own `iptables -S` comparison stays.
  - Egress SNAT is unchanged.

  This makes T039 green.

**Checkpoint**: every story is independently green.

---

## Phase 10: Polish & Cross-Cutting Concerns

**Purpose**: the remaining documentation (FR-020, FR-021, R14), the gates, a mutation check, the host runbook, host validation (FR-023), the follow-up tickets, and the constitution's renumbering at the rebase.

- [X] T049 [P] FR-020: add a section "Security-group membership reaches the ports" to `docs/UPGRADING.md`, with a before/after table:
  - last-group removal: `400` → `202`;
  - add and remove accept an id;
  - add and remove change the ports, and so the firewall;
  - add and remove on a server with no port: `202` → `404`;
  - a server reports its ports' groups: no invented `default`; servers from v0.6.1 and earlier report none; one entry per port;
  - a new interface's port gets `default`;
  - `PUT /v2.0/ports/{id}` applies `security_groups`;
  - `POST /v2.0/ports` with `[]` or `null` gives no group;
  - `GET /v2.0/security-groups` honours `id` and `name`;
  - deleting a group a port carries: `204` → `409`; deleting a missing group: `204` → `404`;
  - deleting a port or a server disassociates its floating IPs completely: no fixed IP or router, status `DOWN`, where only the port used to be cleared;
  - `forceDelete` deletes a server as a delete does: its boot ports go and its volumes are released, where they used to be left behind;
  - a membership, rule or floating-IP change whose firewall or NAT the node cannot apply, or that takes longer than one second: success → `500`, and nothing changes.

  Add a paragraph on the dataplane. A floating-IP filter is replaced atomically, before its NAT, and every command is bounded at one second. The NAT is marked as Lightstack's, and reconciliation now removes floating-IP state no record holds on every node. Rules from earlier releases are replaced for floating IPs that still exist; those of floating IPs deleted before the upgrade stay. Give the command that lists them, `iptables -t nat -S PREROUTING | grep -- '-j DNAT' | grep -v lightstack-fip` (and the same for `OUTPUT`), and say they are the operator's to remove (research R17).
- [X] T050 [P] R14: in `docs/EBPF_STATUS.md`, say that a membership change does not re-program a port's eBPF rule set, and change the `internal/neutron/network.go:40` references to `:41`.
  - Done with `:91`, where the call now is: the node lock and the dataplane interface are declared before `NewService`. The doc's mention of `refreshIngressFiltersForSG`, which T044 deleted, now names `rebuildGroupFilters`.
- [X] T051 Re-run T001's gates and append the results to `docs/specs/033-sg-port-membership/baseline-gates.md`, compared with the baseline (SC-007, SC-008):
  - `make fmt` clean, `make lint`, `go vet ./...`;
  - `go test ./...` on PostgreSQL and on SQLite;
  - `make test-contract` against a running stub Lightstack, once on each backend;
  - `./test/security_groups_test.sh` and `./test/neutron_patch_test.sh`.

  If `make lint` reports paths from a removed worktree, run `go tool golangci-lint cache clean` and repeat.
- [X] T052 Mutation-check the new tests and record the outcome in `docs/specs/033-sg-port-membership/baseline-gates.md`. Revert each fix **off the worktree** (a `git clone --shared` scratch clone or `go test -overlay`, never by editing tracked files in place) and confirm its test fails:
  - the report back to `server_security_groups` (T006);
  - `ChainMatches` always true, and the payload's `-F` dropped (T008);
  - `EnsureIngressFilter` restoring regardless of `ChainMatches` (T008);
  - the runner's one-second context removed (T008, T035);
  - the node lock removed, and its deadline removed (T009);
  - the undo removed, and its compare-and-set removed (T010 for add and remove; T030 for `PUT`; T036 for floating IPs; T037 for rules);
  - the `404` for a server with no port removed (T010);
  - add writing `server_security_groups` only (T010, the earlier-release case);
  - the foreign-key violation passed through as an error (T003);
  - copy-from ignored (T025, T026);
  - the `net_id` port without `default` (T022);
  - the listing's filters ignored (T031);
  - `null` treated as absent (T030);
  - the last-group refusal restored (T019);
  - the address added before the NAT, the NAT before the filter, and the filter removed before the NAT (T035, T036, T039);
  - the mark dropped from the NAT (T035, T039);
  - create programming before its insert (T036, failure on create);
  - the rule rebuild skipped (T037);
  - the in-use check removed (T038, T040);
  - the reconciler removing nothing, removing after a failed or partial read, and removing unmarked rules without a record (T039);
  - a filter removed while an unmarked rule of the shape still reaches it (T036 move, T039);
  - the floating address left out of the pass's comparison (T039, missing address);
  - a missing uplink skipped instead of failing the change (T009, T036, T037, T039);
  - a failed delete's restore leaving out the port forwardings, or not applying a deleted port's `SET NULL` (T036);
  - a port or server deletion leaving its floating IP associated, or not converging it (T036, deletion cases; T041);
  - a port deletion reading the floating IPs before it locks the port, and a server deletion locking ports before the instance (T036, T041);
  - `forceDelete` deleting only the instance row again (T041);
  - the pass reading only associated floating IPs (T039, an unassociated floating IP's leftovers).
- [ ] T053 **Superseded by T165 (round 2 retires the floating-IP filter this run tests).** Generate the host runbook `docs/specs/033-sg-port-membership/HOST_TESTING.md` with `/cl.hosttest docs/specs/033-sg-port-membership/`. It covers prerequisites, setup, the FR-023 steps (a)–(h) as `quickstart.md` §2 and research R18 lay them out (one connection attempt per second in (d)), and teardown, all zsh-paste-safe. The prerequisites block checks, read-only, that iptables' `comment` match works: `iptables -m comment --help` succeeds, and with the legacy backend (`iptables -V`) `modinfo xt_comment` finds the module. Without it, every association answers `500`.
- [ ] T054 **Superseded by T166–T167.** Run host validation (a)–(h) with T053's runbook on one real-mode node (libvirt, root, a routed floating IP, iptables' `comment` match, a second guest for (g) and (h)), and record each step's outcome in `docs/specs/033-sg-port-membership/HOST_TESTING.md`.
- [ ] T055 **Moved to T168.** FR-021: once T054 step (f) has passed, record instance *Edit Security Groups* as verified in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, with the date. Verified means it pre-selects the instance's groups and changes its ports.
- [X] T056 [P] Draft four follow-up tickets in `docs/specs/033-sg-port-membership/follow-ups.md` (new), for the user to file; do not file them (research R17).
  1. **The security-group listing's other filters** (`description`, `project_id`/`tenant_id`, `tags`, `shared`, `stateful`, `revision_number`). They are ignored today, a silent divergence. Honour those the data model can answer, and register or refuse the rest. Refusing all of them would break Horizon, which sends `tenant_id` (research R13).
  2. **Request paths that log a failed node-local step and answer success.** These are the departures named in the constitution's v1.5.0 migration plan, each with its location at `d81f081`:
     - `CreateAutoAllocatedTopology` (`auto_allocated_topology.go:119`);
     - network teardown (`network.go:1167`, `:1185`);
     - `CreateSubnet` (`network.go:1515`), and the subnet delete and gateway-change paths (`:1884`, `:1890`, `:1943`, `:1951`);
     - `DeleteRouter` (`router.go:445`), `AddRouterInterface` (`:541`, `:558`), `RemoveRouterInterface` (`:626`), and gateway set and clear (`:692`, `:799`);
     - port forwarding create, update and delete (`port_forwarding.go:220`, `:361`, `:383`, `:447`);
     - `CreatePort`'s eBPF and FDB steps (`ports.go:235`, `:276`);
     - `DeletePort` and `ReleaseInstancePorts` teardown (`ports.go:546`, `:1887`, `:1899`; `teardownPortHost` `:1919`, `:1928`, `:1934`, `:1938`). This feature disassociates their floating IPs in the deleting transaction and removes the NAT after it (T046). A failed removal is logged and left to reconciliation, because the rest of the teardown has no undo;
     - `AllocatePortForInstance`'s FDB and DHCP steps (`ports.go:1726`, `:1735`);
     - the LS-SG chain writes in `DeleteSecurityGroup` (`:1066`, which this feature moves after the commit, T045), `CreateSecurityGroupRule` (`:1263`) and `DeleteSecurityGroupRule` (`:1462`). Nothing jumps to these chains, so an undone rule add can leave its rule in one, without effect. Removing the chains, or wiring them, is this ticket's decision;
     - `DeleteFloatingIP` (`floatingip.go:573`), which takes the floating IP's port forwardings with it through the foreign key (migration 050) and leaves their NAT.

     Also network creation's undo (`network.go:631-637`), which deletes the row unconditionally rather than compare-and-set. Each path is brought in line with Article II as amended, or the constitution is amended for it.
  3. **Deleting `default` as a non-administrator.** Upstream refuses it with `409` (`SecurityGroupCannotRemoveDefault`) even when no port carries it. `test/security_groups_test.sh` Test 11 already expects a refusal.
  4. **Floating IPs beyond their dataplane.** Two creates can race for one address. A port whose network has no router stores an association (upstream: `404`). Several floating IPs can reach one fixed IP (upstream refuses within one external network).
- [ ] T057 Rebase onto `main` once it carries the other constitution amendment numbered v1.5.0, which lands first, and before review. Renumber this amendment to v1.6.0. Name no other branch or pull request in anything committed.
  - **Constitution.** Resolve `.specify/memory/constitution.md` so both amendments stand:
    - The Sync Impact Report describes this amendment only: "Version change: 1.5.0 → 1.6.0".
    - Every other hit of `grep -n '1\.5\.0' .specify/memory/constitution.md` that belongs to this amendment becomes 1.6.0: "added in", "scope stated in", the log heading.
    - The Forbidden Patterns "No async workers" bullet names both exemptions: the reconciliation loops of this amendment, and the other amendment's re-establishment of a dependency in the background (Article III).
    - The log lists v1.6.0 above v1.5.0.
    - The footer reads `**Version**: 1.6.0` and the rebase date.
  - **`CLAUDE.md`.** Renumber only this feature's own citation, the database paragraph's "(constitution v1.5.0, Article II)". The other amendment's "Constitution IV, v1.5.0" citations stay as they are. Keep both features' Active Technologies lines.
  - **The spec directory.** Every hit of `grep -rn 'v1\.5\.0' docs/specs/033-sg-port-membership/` becomes v1.6.0, the checklist notes included.
  - **The Constitution Check.** Re-run `plan.md`'s table against the merged Articles III and IV, and record the result. This analysis found no conflict: the design never falls back to stub at runtime (a missing uplink is a failed change, T015, T043), and the background-reconnect exception concerns another loop.
  - **The ticket.** Its body and checklist cite v1.5.0 too.
  - **Check.** Run both `grep` commands afterwards. Outside the constitution, expect no hit in the spec directory, and in `CLAUDE.md` only the other amendment's two. Inside it, expect only the other amendment's.

---

# Round 2 (2026-11-20): the follow-ups and the real-mode network layout

**Input**: spec Sessions 2026-11-19 and 2026-11-20, and both round-2 analyses (US8–US12; FR-019a–b, FR-024–FR-058); research R26–R53; data-model.md round 2; contracts `network-api-round2.md`, `node-apply.md`, `router-namespace-dataplane.md`, `port-firewall.md`; quickstart §3–5. Constitution v1.5.0 refined 2026-11-20 (R42) is **already in place** (done at the plan gate), so no task carries it; register entry 6 was added there and withdrawn by the third analysis (R53). Locations are hints from `dcda565`.

**Organization**: US8–US11 are the spec's stories for findings 1, 3 and 4. FR-033–FR-056's router API and layout work (finding 2, the router namespace, and the answers the round-2 analysis turned into requirements) has no user story in the spec; its phases (16 and 18–24) carry no story label and name their requirements instead. Order: setup and foundation (migration, seams, errors, node name), then the API (US8, US9, US11, the router API, US10), then the layout (node, units, firewall, deletions, reconciliation and teardown, the node), then polish.

**Terms (round 2)**:
- *unit*: a router, a network's DHCP, or a node's set of bound ports, rendered from the records and converged, or converged to *absent* before a deletion (R28, R29, R32, R34).
- *node call*: the authenticated, one-second, stateless call naming a unit (R27; `contracts/node-apply.md`).
- *the fakes*: in-memory `routerDataplane`, `dhcpDataplane`, `portFirewall` and `nodeCaller` in `internal/neutron`'s tests, holding state and able to record, block or fail calls (R41, T065).

---

## Phase 11: Round 2 setup

- [ ] T058 Record the round-2 baseline in `docs/specs/033-sg-port-membership/baseline-gates.md`, section "Round 2 baseline", at `dcda565` plus the uncommitted spec and constitution changes: build, lint, unit tests on PostgreSQL and SQLite (per package, `-count=1 -p 1`), contract tests on both backends, `helm unittest charts/lightstack`, and `./test/security_groups_test.sh`. Fix nothing; record failures that predate round 2.
- [ ] T059 Get round 2's test strategy approved before any round-2 test is written (constitution, Test-First Development, step 2): review research R43 and every round-2 test task below with the user, and record the approval, date and any change in `baseline-gates.md`. No later round-2 task starts before it.

---

## Phase 12: Round 2 foundational (blocks every round-2 story)

### Tests (write first, confirm RED)

- [ ] T060 [P] Migration 076 test in `internal/database/migration_076_test.go`: on both backends, after migrating up, `subnet_allocation_pools` exists with `id`, `subnet_id`, `first_ip`, `last_ip` and its cascade from `subnets`; `subnets.pools_stored`, `ports.binding_host_id`, `networks.ct_zone`, `security_groups.revision_number` (default 0) and `security_groups.stateful` (default 1) exist; `networks.ct_zone` has a unique index; the seeded project's `default` has the two ingress rules whose `remote_group_id` is its own id. Down removes all of it, and the two rules. `TestSchemaParity` and `TestMigrationsRoundTrip` must also pass with 076. Migration 077 too: on both backends `security_groups` accepts two groups with one name in a project, a second `default` in a project is refused by the partial unique index, and on SQLite every `port_security_groups` and `security_group_rules` row survives the rebuild.
- [ ] T061 [P] `internal/database/migrate_notx_test.go` (new), R53: a SQLite migration file whose first line is `-- lightstack:no-transaction` runs outside a transaction with foreign keys off, and foreign keys are on again afterwards; a file that leaves a foreign-key violation fails with `foreign_key_check`'s rows named; PostgreSQL ignores the marker; files without it behave as today (lock).
- [ ] T062 [P] Explicit Neutron error type in `internal/common/neutron_errors_test.go`: an error carrying an explicit Neutron type (`SecurityGroupCannotRemoveDefault`) renders that type even with resource "security group" and code 409, where today the resource map wins (`SecurityGroupInUse`). Errors without one render as today (lock).
- [ ] T063 [P] Node name in `internal/compute/node_registry_test.go` (new): the registry registers and heartbeats under `common.NodeName()` (`internal/common/nodename.go:9`, which already returns `LIGHTSTACK_NODE_NAME` or the hostname), not `os.Hostname()` (R26).
- [ ] T064 [P] `neutron.network_node` in `internal/common/config_test.go`: parsed from YAML and from its environment override; with VXLAN on and the key empty, start-up validation fails naming the key; with VXLAN off the node is its own network node whatever the key says.

### Implementation

- [ ] T065 Seams and fakes (R41): in `internal/neutron/units.go` (new) declare `routerDataplane`, `dhcpDataplane`, `portFirewall` and `nodeCaller`, and the unit lock type (per key, one-slot channel, acquired within the node call's shared 900 ms deadline, R27); add the fields to `Service` in `internal/neutron/network.go`, stub implementations that apply nothing, and in `internal/neutron/fake_units_test.go` (new) the four stateful fakes with `failWith`, `blockFirst`, `writes` and `seed*` helpers in the style of `fake_dataplane_test.go`. The fake `nodeCaller` routes a call to another in-process `Service` by node name, so two-node tests run in one process.
- [ ] T066 Migration 076 (makes T060 green): `migrations/076_network_layout.postgres.up.sql`, `076_network_layout.postgres.down.sql`, `076_network_layout.sqlite.up.sql` and `076_network_layout.sqlite.down.sql`, per `migrations/README.md`'s naming: the new table (a pair, since it creates a table); `ALTER TABLE … ADD COLUMN` for `subnets.pools_stored` (integer), `ports.binding_host_id` (text) and `networks.ct_zone` (integer); a unique index on `networks(ct_zone)`; `security_groups.revision_number` (integer, default 0) and `security_groups.stateful` (integer, default 1); the seeded `default`'s two member rules (`002`'s ids). No table that others reference is rebuilt on SQLite. And migration 077 (`077_security_group_names.postgres.up.sql`/`.down.sql`, `077_security_group_names.sqlite.up.sql`/`.down.sql`, the SQLite up file with the `-- lightstack:no-transaction` marker): drop the unique name constraint (PostgreSQL by its looked-up name; SQLite by rebuilding the table), and create the partial unique index for `default` (R53).
- [ ] T067 The migrator change (makes T061 green): `internal/database`'s migration runner honours the `-- lightstack:no-transaction` marker on SQLite (foreign keys off, run, `PRAGMA foreign_key_check`, foreign keys on); document it in `migrations/README.md`, replacing "comes with a change to the migrator" with the rule and its check.
- [ ] T068 [P] Explicit Neutron type (makes T062 green) in `internal/common/errors.go` (a field and a constructor) and `internal/common/neutron_errors.go` (`neutronType` honours it first).
- [ ] T069 [P] Node name (makes T063 green) in `internal/compute/node_registry.go`: register and heartbeat under `common.NodeName()`.
- [ ] T070 [P] `neutron.network_node` (makes T064 green) in `internal/common/config.go` (`NeutronConfig`, the env override, validation called from `cmd/lightstack/main.go`'s `checkDatabaseConfig` neighbourhood), `config/lightstack.yaml` (commented example), and the chart: `charts/lightstack/values.yaml`, `values.schema.json`, `templates/_helpers.tpl` neutron block, plus a case in `charts/lightstack/tests/config_render_test.yaml`.

**Checkpoint**: migration 076 applies on both backends; the seams and fakes compile; nothing user-visible changed.

---

## Phase 13: User Story 8 — A client lists groups by any attribute (Priority: P3)

**Goal**: every attribute filter applies as upstream's (FR-019a, FR-019b; R38).
**Independent Test**: contract — each filter against a project whose groups differ in it.

### Tests for User Story 8 (write first, confirm RED)

- [ ] T071 [P] [US8] Extend `internal/neutron/security_group_list_filter_test.go`: `description` (repeated values OR'd), `project_id`/`tenant_id` (own → all; another project as a member → empty; another project as an administrator → that project's groups), `shared=false`/`true`, `stateful=true`/`false` against stored values, `revision_number` against stored values, `tags`, `tags-any` (empty), `not-tags`, `not-tags-any` (all), `shared=maybe` → `400`, a repeated `name`. And every group in list, show, create and update bodies carries its `stateful` and `revision_number`, `shared: false`, `tags: []`.
- [ ] T072 [P] [US8] Contract `test/contract/neutron/security_group_filters_test.go` (new): the same through gophercloud's `groups.List` with `ListOpts` and raw queries for the parameters gophercloud lacks; `revision_number` and `stateful` filters against groups created stateless and updated.

- [ ] T073 [P] [US8] `internal/neutron/security_group_revision_test.go` (new), FR-050: a created group (with its starting rules) has `revision_number` 0; each update of the group adds one; each rule added or deleted adds one; a rule add undone on a failed apply leaves the revision where it was (compare-and-set, with the rule); `?revision_number=` lists by the stored value; `PUT` and `DELETE` with `If-Match: revision_number=<n>` and a different revision answer `412 RevisionNumberConstraintFailed`, nothing changed, and with the current one succeed; `GET /v2.0/extensions` lists `stateful-security-group`.
- [ ] T074 [P] [US8] `internal/neutron/security_group_stateless_test.go` (new), FR-051: `POST` with `stateful: false` creates a stateless group, reported so; changing `stateful` while a port carries the group → `409 SecurityGroupInUse`, unchanged; while none does → `200`; a port create or update that would carry stateful and stateless groups → `409 SecurityGroupConflict`, nothing changed.
- [ ] T075 [P] [US8] `internal/nova/security_group_stateless_test.go` (new), FR-051: `addSecurityGroup` of a stateless group to a server whose ports carry stateful ones → `409`, nothing changed; `addSecurityGroup` that leaves every port single-kind (a server whose ports already differ, each staying single-kind) succeeds; interface attach of a port of the other kind succeeds, the port keeping its groups (FR-013); a boot whose requested groups mix both kinds ends `ERROR` with a fault, as upstream's failed port creation does.

### Implementation for User Story 8

- [ ] T076 [US8] In `internal/neutron/ports.go` `securityGroupListFilter`: each parameter with `= ANY($n)` over its values, `project_id` and `tenant_id` merged (an administrator's replace the own-project scope with the named projects; anyone else's narrow within it), `stateful` and `revision_number` on their columns, the constant answers for `shared` and the tag filters, strict booleans; and the four fields in `ListSecurityGroups`, `GetSecurityGroup`, `CreateSecurityGroup` and `UpdateSecurityGroup` responses.

- [ ] T077 [US8] `internal/neutron/ports.go` and `internal/neutron/security_group_default.go`: `revision_number` bumped in the transactions of `UpdateSecurityGroup`, `CreateSecurityGroupRule`, `DeleteSecurityGroupRule` and their undos; `stateful` accepted on create, refused on update while in use; the mixing check inside the shared membership helpers (`SetPortSecurityGroups`, `AddPortSecurityGroup`), so the port API, Nova's add, attach and the boot's allocation all refuse it; `internal/nova/advanced_actions.go` (`addSecurityGroup`) maps it to `409` (the check is per port; interface attach has none), and the boot's allocation failure in `internal/nova/handlers.go` to the fault; undos put `revision_number` back compare-and-set; `UpdateSecurityGroup` and `DeleteSecurityGroup` honour `If-Match`; `ListExtensions` (`internal/neutron/network.go`) adds `stateful-security-group`.

**Checkpoint**: US8 green on both backends.

---

## Phase 14: User Story 9 — `default` stays the project's default (Priority: P2)

**Goal**: upstream's refusals for `default`, duplicate names as upstream with `409 NoUniqueMatch` on an ambiguous name, upstream's starting rules (FR-024–FR-026, FR-043; R39, R53).
**Independent Test**: delete, rename and create against `default` as a tenant and as an administrator; `test/security_groups_test.sh` Test 11.

### Tests for User Story 9 (write first, confirm RED)

- [ ] T078 [P] [US9] `internal/neutron/security_group_default_test.go` (new): non-administrator deletes unused `default` → `409 SecurityGroupCannotRemoveDefault`, group and rules unchanged; administrator → `204`, and the next `EnsureDefaultSecurityGroup` makes a new one with four rules; in-use `default` → `409 SecurityGroupInUse` for both; `PUT` `default` with `name: "web"` → `409 SecurityGroupCannotUpdateDefault`, with `name: "Default"` → `409 SecurityGroupDefaultAlreadyExists`, with only `description` → `200`; `POST` `default`/`DEFAULT` → `409 SecurityGroupDefaultAlreadyExists`; `PUT` of another group with `name: "default"` → `409 SecurityGroupDefaultAlreadyExists`; a duplicate name on create and on rename succeeds, as upstream (FR-026); `GET /v2.0/security-groups/{name}` with a name two groups share answers `404`, and `?name=` lists both.
- [ ] T079 [P] [US9] `internal/neutron/security_group_starting_rules_test.go` (new): a created group has exactly two egress rules (IPv4, IPv6, no remote); `EnsureDefaultSecurityGroup` creates four (plus ingress IPv4 and IPv6 with `remote_group_id` = the group); no rule has neither remote while being ingress (cloud-storage#528 lock).
- [ ] T080 [P] [US9] Contract `test/contract/neutron/security_group_default_test.go` (new): the refusals with their Neutron types through gophercloud, as a member and as an administrator.
- [ ] T081 [P] [US9] In `test/security_groups_test.sh`, rename the `GROUPS` variable to `SG_LIST` (`:40`, `:43`, `:209`), so Test 1 finds `default` and Test 11 runs; Test 11 expects the `409`. Run it against `main`'s binary: Test 11 fails red (it deletes `default` with `204`).
- [ ] T082 [P] [US9] `internal/nova/security_group_ambiguous_name_test.go` (new), FR-026: with two groups named `web` in the project, server create naming `web`, `addSecurityGroup` and `removeSecurityGroup` by name answer `409` with upstream Nova's `NoUniqueMatch` message and change nothing; by either group's id they succeed.

### Implementation for User Story 9

- [ ] T083 [US9] In `internal/neutron/ports.go`: `DeleteSecurityGroup` checks in-use first, then `default` and not administrator (`common.HasRole`); `CreateSecurityGroup` and `UpdateSecurityGroup` check the name against `default` case-insensitively, `UpdateSecurityGroup` refuses a `name` on `default`; neither refuses a duplicate name any more; `CreateSecurityGroup` inserts the two egress rules in its transaction (it gains one). Use T068's explicit types.
- [ ] T084 [US9] In `internal/neutron/security_group_default.go`: `EnsureDefaultSecurityGroup` seeds four rules; update its comment (the member rules are not open to every address).
- [ ] T085 [US9] `internal/neutron/ports.go` (`ResolveSecurityGroup`: an ambiguity error when a name matches several groups; `GetSecurityGroup`: `404` for an ambiguous name), `internal/neutron/security_group_default.go` (`EnsureDefaultSecurityGroup`'s conflict target becomes `(project_id) WHERE name = 'default'`), and `internal/nova/handlers.go` and `internal/nova/advanced_actions.go` (the ambiguity → `409` `NoUniqueMatch`) — makes T082 and T078's name cases green (R53).

**Checkpoint**: US9 green; Test 11 passes.

---

## Phase 15: User Story 11 — Operators choose the floating range with allocation pools (Priority: P2)

**Goal**: stored pools, upstream's validation, one allocator for every automatic address (FR-030–FR-032a; R30). Before US10, whose allocation uses it.
**Independent Test**: subnet create and update with pools, refusals, and the addresses ports get.

### Tests for User Story 11 (write first, confirm RED)

- [ ] T086 [P] [US11] `internal/neutron/subnet_pools_test.go` (new), FR-030 and FR-052: create with pools reports them on create, show and list; absent → upstream's default (gateway removed, a split range when the gateway is inside); `[]` → no pool; `pools_stored` false → default (a subnet as migration 076 leaves it); the five refusals in upstream's order (`InvalidAllocationPool` for malformed, start after end and wrong version; `OutOfBoundsAllocationPool`; `OverlappingAllocationPools`; `GatewayConflictWithAllocationPools` on create, on a pools update and on a gateway update); an update replaces the pools, leaves allocated ports alone, and the next allocation comes from the new pools; `DeleteSubnet` with a non-DHCP port on it → `409 SubnetInUse`.
- [ ] T087 [P] [US11] `internal/neutron/allocation_test.go` (new), FR-032 and FR-053: the allocator never hands out an address outside the pools, the gateway, a port's fixed IP or a floating IP of the network; a port gets a pool address; a requested address outside the pools but inside the subnet is accepted; the network and broadcast addresses are refused; exhaustion is reported as such. Concurrent `AllocatePortForInstance` on one subnet still gets distinct addresses (lock); a floating-IP create, a router gateway and a port on the same external subnet allocated concurrently get distinct addresses (PostgreSQL; every owner takes the subnet lock, FR-032).
- [ ] T088 [P] [US11] Contract `test/contract/neutron/subnet_pools_test.go` (new): gophercloud `subnets.Create` with `AllocationPools`, the read-back, and the `409` for a pool holding the gateway.
- [ ] T089 [P] [US11] `internal/nova/interface_attach_ip_test.go` (new), FR-053: a requested `fixed_ip` equal to the subnet's network or broadcast address → `400 InvalidIpForSubnet` (today accepted).

### Implementation for User Story 11

- [ ] T090 [US11] `internal/neutron/allocation.go` (new): pool reading (rows when `pools_stored`, else the default), `generatePools` (upstream's `generate_pools`), validation, and `allocateAddress(ctx, tx, subnet, policy)` with the policies of data-model.md (random, lowest), excluding the gateway, ports' fixed IPs and the network's floating IPs, always under the subnet's `LockSubnet`, whatever the owner. Replace `allocateIP`, `AllocateFreeIP` and `IPAvailable`'s range logic in `internal/neutron/ports.go` with it; `IPAvailable` refuses the network and broadcast addresses.
- [ ] T091 [US11] `internal/neutron/network.go`: `CreateSubnetRequest` and `UpdateSubnet` accept `allocation_pools`; store rows and `pools_stored` in the subnet's transaction; `validateSubnetGateway` and the gateway update check against the pools; every response reports the stored or default pools (replaces `subnetAllocationPools`); `DeleteSubnet` checks in-use in its transaction.
- [ ] T092 [US11] `internal/nova/interface_attach.go`: use `neutron.IPAvailable`'s new refusal (makes T089 green).

**Checkpoint**: US11 green on both backends.

---

## Phase 16: Router API — gateway ports, interfaces, FR-039 (FR-038, FR-039, FR-045; R31)

### Tests (write first, confirm RED)

- [ ] T093 [P] `internal/neutron/router_ports_test.go` (new): gateway set creates one `network:router_gateway` port with no project and a pool address, reported in `external_gateway_info.external_fixed_ips` with its subnet; a requested address by an administrator is used; by a member → `403`; outside the network's subnets → `400 InvalidIpForNetwork`; equal to a subnet's gateway → `400`; taken → `409 IpAddressAlreadyAllocated`; a changed requested address updates the port; clear deletes it. `AddRouterInterface` stores port and interface in one transaction (a failing second insert leaves neither); a second interface on the same subnet → `400` with no new port; a subnet without a gateway → `400` (today `500`) (FR-055). The gateway port is listed for administrators only.
- [ ] T094 [P] `internal/neutron/router_floating_ip_refusals_test.go` (new): removing an interface whose subnet a floating IP of this router reaches → `409 RouterInterfaceInUseByFloatingIP`; clearing or changing the gateway while a floating IP uses the router → `409 RouterExternalGatewayInUseByFloatingIp`; nothing changed in either case; with no such floating IP both succeed.
- [ ] T095 [P] Contract `test/contract/neutron/router_gateway_test.go` (new): `external_fixed_ips` read back through gophercloud `routers.Get`, and FR-039's two `409`s.

### Implementation

- [ ] T096 `internal/neutron/router.go`: the gateway port (create, update, clear) in the router's transaction, `external_fixed_ips` rendered from it, the administrator rule and upstream's errors; `AddRouterInterface` in one transaction with upstream's `400`s and the subnet lookup scoped to the caller; FR-039's checks in the transactions that remove an interface or clear or change a gateway. The SNAT source becomes the gateway port's address (used by Phase 19).

---

## Phase 17: User Story 10 — Floating IPs are allocated and associated as upstream (Priority: P2)

**Goal**: no allocation race, upstream's router lookup and refusals (FR-027–FR-029, FR-031; R36).
**Independent Test**: concurrent creates; association with and without a joining router; a second floating IP on one fixed IP.

### Tests for User Story 10 (write first, confirm RED)

- [ ] T097 [P] [US10] `internal/neutron/floating_ip_allocation_test.go` (new): ten goroutines create on one external network at once → ten `201`, ten distinct addresses, all inside the pools (both backends; on SQLite the writers serialise anyway, the test still pins the answers); two external networks with overlapping subnets created concurrently → no `500`; a network with no IPv4 subnet → `400` and no row; pools taken → `400 ExternalIpAddressExhausted`; `subnet_id` and `floating_ip_address` honoured; a taken `floating_ip_address` → `409 IpAddressAlreadyAllocated` (FR-054).
- [ ] T098 [P] [US10] `internal/neutron/floating_ip_router_test.go` (new): create with a port, and update to a port, whose subnet no router joins to the floating network → `404 ExternalGatewayForFloatingIPNotFound`, nothing stored (today `201` and `400`); a router joining the subnet to another external network → `404`; two joining routers → the one whose interface is the subnet's gateway; a second floating IP of the same network on the same fixed IP → `409 FloatingIPPortAlreadyAssociated` (create and update), one of another network accepted; a requested `fixed_ip_address` that is not the port's → `400` (FR-054).
- [ ] T099 [P] [US10] Contract `test/contract/neutron/floating_ip_association_test.go` (new): the `404` and `409` through gophercloud `floatingips.Create`/`Update`, and ten parallel creates.

### Implementation for User Story 10

- [ ] T100 [US10] `internal/neutron/floatingip.go`: `CreateFloatingIP` takes the allocation advisory lock first in its transaction (`pg_advisory_xact_lock` on a named constant; SQLite's registered no-op), chooses the address there through T090's allocator (lowest, across the network's IPv4 subnets), honours `subnet_id` and `floating_ip_address`, refuses no subnet (`400`) and exhaustion (`400 ExternalIpAddressExhausted`); remove the pre-transaction `allocateFloatingIP` and the `192.0.2.0/24` fallback.
- [ ] T101 [US10] `internal/neutron/floatingip.go`: replace `lockGatewayRouter` with upstream's lookup (an interface port on the fixed IP's subnet, a gateway port on the floating network, the gateway-interface preference), `404` when none, on create and update; the FR-029 check in the same transaction; `portFixedIP` verifies a requested fixed IP is the port's.

**Checkpoint**: US10 green on both backends. Phases 11–17a are the API increment: no layout change; the gateway port's address is stored and reported, and egress keeps today's host SNAT until Phase 19 (R51).

---

## Phase 17a: User Story 12 — An administrator sees and manages every project's networking resources (Priority: P3)

**Goal**: every Networking API listing, show and write as upstream's for administrators (FR-057, FR-058; R50).
**Independent Test**: contract — as an administrator, another project's resource in each listing, by project filter and by id; as a member, nothing changes.

### Tests for User Story 12 (write first, confirm RED)

- [ ] T102 [P] [US12] `internal/neutron/admin_listing_test.go` (new), table-driven over the sixteen listings of R50: an administrator of project A sees project B's resource unfiltered; `?project_id=B` and `?tenant_id=B` list only B's; a show by id of B's resource answers `200`; `GetSecurityGroup` by B's group's name answers `404`; a member of A sees what they see today (own project; networks also shared and external) and gets `404` showing B's resource (lock).
- [ ] T103 [P] [US12] Contract `test/contract/neutron/admin_listing_test.go` (new): the same through gophercloud as the seeded administrator and as a member of a second project, for networks, ports, routers, floating IPs and security groups.
- [ ] T104 [P] [US12] `internal/neutron/admin_write_test.go` (new), table-driven over every update, delete and action handler of R50 and the creates' references by id: an administrator of project A succeeds on project B's resource by id, a member of A gets `404` (lock); a resource the administrator creates belongs to the named project or to A; an administrator putting B's security group on A's port by id succeeds, as upstream (FR-018).
- [ ] T105 [P] [US12] Change round 1's pins of the old id rule to upstream's, FR-004 and FR-018 (R52): `internal/nova/security_group_actions_test.go`, `internal/neutron/port_api_security_groups_test.go` and `test/contract/nova/security_group_actions_test.go` — an administrator naming another project's group **by id** in `addSecurityGroup`, server create or port create/update now succeeds; **by name** it still resolves in the server's project (entry 5); a member naming another project's group by id or name still gets `404`. Red against the current code.

### Implementation for User Story 12

- [ ] T106 [US12] `internal/neutron/list_scope.go` (new): `projectScope(c, f, column)` per R50, and its use in all sixteen listings in `internal/neutron/network.go`, `ports.go`, `router.go`, `floatingip.go`, `port_forwarding.go` and the QoS, RBAC, trunk, address-scope, subnet-pool, IP-availability and metering handlers; the shows by id skip the project condition for an administrator, a name branch keeps it. The same helper decides every write's target (update, delete, action, and a create's references by id) for an administrator, per R50; the membership helpers follow T107.
- [ ] T107 [US12] `internal/neutron/ports.go` (`ResolveSecurityGroup`, `requireGroupsOfProject`) and their callers in `internal/nova/handlers.go` and `internal/nova/advanced_actions.go`: an administrator's id matches any project's group; names unchanged (makes T105 green; R52).

**Checkpoint**: US12 green on both backends.

---

## Phase 18: The network node and the node call (FR-033, FR-034; R26, R27)

### Tests (write first, confirm RED)

- [ ] T108 [P] `internal/neutron/node_call_test.go` (new): a node token (audience `lightstack-node`, `node`, `exp`, `body_sha256`) is accepted by `/internal/v1/converge`; expired, wrong audience, wrong node, tampered body → `401`; a user token → `401`; a node token on any `/v2.0` route → `401`; a unit the target does not hold (not the network node; a port bound elsewhere) → `409`; a target that blocks → the caller fails within one second, and the target itself gives up within 900 ms of receipt, lock wait included; `absent` for a port no longer bound to the target is accepted; a call to self bypasses HTTP; the body carries only unit, ids and `absent`.
- [ ] T109 [P] `internal/neutron/network_node_test.go` (new): which node holds which unit (routers and DHCP the network node; ports their `binding_host_id`), the caller resolving a node's address from the registry's tunnel IP and the Neutron port, and a missing or inactive node failing the step.

### Implementation

- [ ] T110 `internal/neutron/node_call.go` (new): the token's minting and checking with `keystone.jwt_secret`, signing inline with `golang-jwt` as `internal/keystone/auth.go:397` does (keystone exports no helper; no new module), the HTTP client with a one-second timeout, the `/internal/v1/converge` handler (unit lock, read, render, apply, all within one 900 ms deadline from receipt), routing in `internal/neutron/network.go`'s route setup outside `/v2.0`, and the real `nodeCaller` over the registry. `internal/middleware/auth.go` refuses tokens with the node audience.
- [ ] T111 `cmd/lightstack/main.go`: pass the network node name, the node name, the registry (nil without VXLAN) and the Neutron port to `neutron.NewService`; refuse to start with VXLAN on and no network node (T064).

---

## Phase 19: The router unit — namespace, legs, NAT, port forwarding (FR-033, FR-035, FR-037, FR-038, FR-049; R28, R37)

### Tests (write first, confirm RED)

- [ ] T112 [P] `pkg/networking/router_namespace_test.go` (new), pure renderers: for a router with a gateway, two interfaces, two floating IPs and a port forwarding, the namespace's nat and mangle `iptables-restore` payloads equal `contracts/router-namespace-dataplane.md`'s rules (chain names, `lightstack` comments, SNAT exception, hairpin mark, `ls-pf-<id[:8]>`); devices and addresses (`qg-`/`lg-`, `qr-`/`lr-`, `/32` floating addresses, default route); host routes and proxy entries; SNAT off drops the two SNAT rules; a port-forwarding range as upstream.
- [ ] T113 [P] `pkg/networking/router_namespace_apply_test.go` (new), with the fake command runner: adding applies namespace, legs, addresses, one restore per table, then floating addresses, then host routes and proxy entries; removing the reverse; an unchanged router runs reads only; every command gets at most one second; a failing or blocking command fails the call within about a second; stub mode runs nothing.
- [ ] T114 [P] `internal/neutron/router_unit_test.go` (new), against the fakes: a floating-IP create, update, move, disassociate and delete, a port forwarding's create, update and delete, an interface add and remove, and a gateway set, change and clear each converge their router on the network node before the answer (through the fake node caller when served by another node); a failing router unit gives `500` and the compare-and-set undo of R20 (FR-008) or the re-apply of FR-040; a floating IP deleted with port forwardings removes their rules with it.
- [ ] T115 [P] Rework `internal/neutron/floating_ip_dataplane_test.go` and `internal/neutron/ingress_refresh_test.go`: their floating-IP NAT and filter assertions move to the router unit (T114) and the port unit (T127); keep only what still holds, or delete the file, and say which in this task's note.

### Implementation

- [ ] T116 `pkg/networking/router_namespace.go` (new): the renderers and the real-mode converge through `rm.run` (bounded), per the contract. It adds the new code beside the old and removes nothing; T118 removes the old functions once nothing calls them.
- [ ] T117 `internal/neutron/units.go`: the router unit — read a router's records (router, gateway port, interfaces with their ports and subnets, floating IPs, port forwardings), render, converge through `routerDataplane` under the router lock; and `convergeRouter(ctx, id, absent)` routed through the node caller to the network node.
- [ ] T118 Switch every caller to the router unit and remove the old functions in the same task, so the build never breaks: `internal/neutron/reconciler.go` (its floating-IP and egress passes; the router-unit pass itself is T147), `internal/neutron/teardown.go` (the old calls go; the new purge is the teardown task), `internal/neutron/network.go` (the NAT methods of `floatingIPDataplane`; its filter methods, the `dataplane` field and `lockNode` stay until T132, which still has `RefreshPortFilters` and `removeOrphanFilter` using them), `internal/neutron/floatingip.go` (create, update, delete, `ConvergeFloatingIPs`, replacing `convergeFloatingIP*` and the node lock), `internal/neutron/port_forwarding.go` (create, update, delete; checks in their transactions: the floating IP's router, the internal port's project and address, duplicates), `internal/neutron/router.go` (interfaces, gateway; replacing `configureExternalGateway`, `withdrawEgressSNAT`, the namespace calls), `internal/neutron/ports.go` (`convergeDisassociated`), `internal/nova/handlers.go` (`ConvergeFloatingIPs` after server deletion). Remove `EnsureFloatingIP`, `WithdrawFloatingIP`, `EnsureEgressSNAT`, `RemoveEgressSNAT` and the host-namespace parts of `NorthSouthState` from `pkg/networking/northsouth.go`, and `CreateRouterNamespace`, `AttachInterfaceToRouter`, `DetachInterfaceFromRouter`, `AddPortForwarding`, `RemovePortForwarding` and the unused `SetDefaultGateway`/`AddRoute`/`DeleteRoute` from `pkg/networking/router.go`, with their tests.

---

## Phase 20: The DHCP unit and metadata (FR-037; R29)

### Tests (write first, confirm RED)

- [ ] T119 [P] `pkg/networking/dhcp_namespace_test.go` (new): `dnsmasq`'s rendered configuration for a network with two DHCP subnets: static ranges per subnet with tags, a host entry per port, option 3 the gateway, option 121 `169.254.169.254/32,<DHCP address>` and `0.0.0.0/0,<gateway>`, DNS; a gatewayless subnet gets the metadata route only; the namespace and device plan (`qdhcp-<N>`, `tap<port[:11]>`, the addresses and `169.254.169.254/32`); real-mode apply through the fake runner, bounded, stub mode inert.
- [ ] T120 [P] `internal/neutron/dhcp_unit_test.go` (new), against the fakes, the DHCP port's host end `ld-<port[:11]>`: the network's first DHCP subnet creates a `network:dhcp` port with the lowest free pool address and the network's project; a second subnet adds an address; the last one removed deletes the port; subnet create, update and delete, and port create and delete (reservations), converge the DHCP unit on the network node before the answer; a failing unit gives `500` and the undo (create, update) or re-apply (delete).
- [ ] T121 [P] `internal/metadata/network_identity_test.go` (new): the handler resolves an instance by network and source address; two networks with the same address resolve to their own instances; the existing loopback and trusted-header rules stay (lock).

### Implementation

- [ ] T122 `pkg/networking/dhcp_namespace.go` (new): renderers and real-mode converge, beside the old code; it removes nothing (T123 does) (namespace, veth to the bridge, addresses, `dnsmasq` started and stopped inside the namespace, its files under the state path).
- [ ] T123 `internal/neutron/units.go`, `internal/neutron/network.go`, `internal/neutron/ports.go` (`AllocatePortForInstance`, `ReleaseInstancePorts`) and `internal/neutron/reconciler.go` (`reconcileSubnets`): the DHCP unit and its triggers (subnet create, update, delete; port create, delete; `AllocatePortForInstance`, `ReleaseInstancePorts`), the DHCP port's lifecycle, under the network's lock, through the node caller. Replace `applyDHCPForNetwork`'s callers. In the same task, retire `pkg/networking/metadata_redirect.go`'s per-bridge DNAT and the bridge addressing in `pkg/networking/dhcp.go`, with every caller, so the build never breaks.
- [ ] T124 `internal/metadata/service.go` and a new `internal/metadata/netns_listener.go`: a listener per `qdhcp-*` namespace on `169.254.169.254:80`, opened by entering the namespace on a locked OS thread, serving the handler with its network; identity by network and source address. The DHCP unit opens and closes it through an interface Neutron receives from `main.go`.

---

## Phase 21: The port firewall (FR-041–FR-043, FR-047; R32, R33)

### Tests (write first, confirm RED)

- [ ] T125 [P] `pkg/networking/port_firewall_test.go` (new), pure: for ports with groups, rules, a remote group with members, IPv4 and IPv6, the rendered `iptables-restore` and `ip6tables-restore` payloads equal `contracts/port-firewall.md` (dispatch, `ls-i`/`ls-o`/`ls-s` chains, DHCP and neighbour-discovery allowances, spoof per fixed IP and MAC, the rule block with `RELATED,ESTABLISHED`, de-duplicated `RETURN`s, `INVALID` drop, fallback; remote groups expanded excluding the port's own addresses; trusted ports accepted; raw zone rules; a port with stateless groups gets `CT --notrack` and no `RELATED,ESTABLISHED`/`INVALID` rules (FR-051)); trusted ports accepted by `lr-`/`ld-` device name; a port with no group renders the block without rules; the payload removes a deleted port's chains and dispatch.
- [ ] T126 [P] `pkg/networking/port_firewall_apply_test.go` (new), fake runner: compares with `-S` per chain and restores only on a difference, one commit per family, bounded at one second; stub mode inert.
- [ ] T127 [P] `internal/neutron/port_unit_test.go` (new), against the fakes, two services and the fake node caller: a membership change, a rule add and delete, a change to a remote group's members, and a port's binding change each converge the affected ports' chains on their bound nodes before the answer, one call per node; a failure gives `500` and the existing compare-and-set undo (FR-008); an unbound port has no chains; a port whose binding moves sends `absent` to the old node and presence to the new one in the same request; the same converges happen in `ebpf` mode (FR-047); `CreatePort` no longer calls the eBPF manager (FR-045); the network's `ct_zone` is assigned at creation, lowest free from 4097.
- [ ] T128 [P] `pkg/hypervisor/xml_template_test.go` (extend): the interface element carries `<target dev='tap<port id[:11]>'/>`.
- [ ] T129 [P] `internal/nova/port_binding_test.go` (new), FR-041 and FR-056: boot sets the ports' `binding_host_id` to the node name and converges their chains before `DomainCreate` (the fake `NeutronService` records the order); a failing converge puts the server in `ERROR` with a fault and starts no domain; interface attach binds and converges before `AttachDevice`, and answers `500` and unbinds on failure; detach clears the binding and converges the old node `absent`; `GET /v2.0/ports/{id}` reports `binding:host_id` to an administrator only.
- [ ] T130 [P] Rework the round-1 filter tests: `internal/neutron/security_group_rule_refresh_test.go`, `internal/nova/security_group_actions_test.go`'s failure cases and `pkg/networking/ingress_filter_test.go` assert the port unit instead of the floating-IP filter (FR-042); `reconcile_dataplane_test.go`'s filter cases move to T146. Note per file what changed.

### Implementation

- [ ] T131 `pkg/networking/port_firewall.go` (new): renderers and real-mode converge through `rm.run`, beside the old code; it removes nothing (T132 does).
- [ ] T132 `internal/neutron/units.go`: the port unit — read each port's groups, rules, remote members, addresses, MAC and network zone; group by `binding_host_id`; one node call per node. `RefreshPortFilters` and `rebuildGroupFilters` converge port units; `portIngressRules` and `floatingFixedIPs` go. Remove the LS-SG chain calls from `CreateSecurityGroup`, `DeleteSecurityGroup`, `CreateSecurityGroupRule`, `DeleteSecurityGroupRule`. In the same task, update every remaining caller (`internal/neutron/floatingip.go`, `internal/neutron/teardown.go`, `internal/neutron/reconciler.go`) and delete `RenderIngressFilter`, `EnsureIngressFilter`, `RemoveIngressFilter` and the `LS-NSI` helpers from `pkg/networking/northsouth.go`, and the `LS-SG-*` chain code in `pkg/networking/security_groups.go`, so the build never breaks. Remove what is left of `floatingIPDataplane`, the `dataplane` field, `lockNode` and the old fake in `internal/neutron/network.go` and its tests.
- [ ] T133 `internal/neutron/network.go`: assign `ct_zone` in `CreateNetwork`'s transaction. `internal/neutron/ports.go`: report `binding:host_id` (administrators), accept it from Nova's calls; stop creating `tap-<port8>` in `CreatePort` and `AllocatePortForInstance` (libvirt owns the TAP), and remove `CreatePort`'s eBPF step with it (`ports.go:215-241`; FR-045, FR-047).
- [ ] T134 `pkg/hypervisor/xml_template.go` and `internal/nova` (boot, attach, detach): the TAP name in the domain XML; `binding_host_id` set and cleared, and the chains converged, through `NeutronService` (new method `BindPort(ctx, portID, node string) error`, converging before it returns; faked in `boot_failure_test.go`'s `fakeNeutron`), before `DomainCreate`/`AttachDevice`, with T129's failure handling.

---

## Phase 22: Deletions and the remaining Article II paths (FR-040, FR-045; R34, R40)

### Tests (write first, confirm RED)

- [ ] T135 [P] `internal/neutron/deletion_order_test.go` (new), against the fakes: for a port, a router interface, a router, a gateway clear, a subnet, a network and a port forwarding — the refusal checks run first; the affected units converge to absent before the record goes; a failing unit → `500`, the record kept, the units converged from it again; a refusal in the final transaction (a dependant added in between) → that refusal and the units re-applied; the fake records the order.
- [ ] T136 [P] `internal/neutron/network_create_undo_test.go` (new): a failing bridge on create removes the row compare-and-set on `updated_at`; a row updated in between stays; a row another request built on (a subnet added) stays, with its subnet.
- [ ] T137 [P] `internal/neutron/port_fdb_test.go` (new), FR-045 and R51: with VXLAN on, a port create writes its FDB row in the port's own transaction; a failing FDB insert answers `500` and leaves no port; a pre-076 network without `ct_zone` gets the lowest free zone the first time one of its ports is rendered.
- [ ] T138 [P] `internal/nova/server_delete_ports_test.go` (new): server deletion runs its ports' deletion through FR-040 after libvirt; a failing port unit → `500`, the server and its ports kept, a second delete succeeds; `forceDelete` the same.
- [ ] T139 [P] `internal/neutron/auto_allocated_topology_unit_test.go` (new): a failing unit during auto-allocation answers `500` and leaves no half topology.

### Implementation

- [ ] T140 `internal/neutron/ports.go` (`DeletePort`, `ReleaseInstancePorts`, `teardownPortHost` reduced to the units), `internal/neutron/router.go` (`DeleteRouter`, `RemoveRouterInterface`, gateway clear), `internal/neutron/network.go` (`DeleteSubnet`, `DeleteNetwork`, `teardownNetworkHost`, network create's undo: one `DELETE … WHERE id AND updated_at AND NOT EXISTS (dependants)` statement), `internal/neutron/port_forwarding.go` (delete), `internal/neutron/auto_allocated_topology.go`: FR-040's order through the units, and FR-008's undo on every create and update path named in R40. Port creation's FDB row moves into the port's transaction (`CreatePort`, `AllocatePortForInstance`), and the null-`ct_zone` assignment of R51 goes into the port unit's read (makes T137 green).
- [ ] T141 `internal/nova/handlers.go` `deleteServer`: the ports' deletion through Neutron's FR-040 path after libvirt; a failure answers `500` and keeps the instance.

---

## Phase 23: Reconciliation by role (FR-010 restated, FR-033, FR-042, FR-044; R26, R28–R33)

### Tests (write first, confirm RED)

- [ ] T142 [P] `internal/neutron/reconcile_units_test.go` (new): on the network node a pass converges every router and DHCP unit with a record, restarting a network's `dnsmasq` whose process is gone (after a restart), and removes namespaces, legs, proxy entries and listeners of units without one, only after a complete read; a matching unit is left untouched; on another node a pass removes any `qrouter-*`/`qdhcp-*` it finds; every node converges its bound ports' chains and removes `ls-` chains no bound port holds, after a complete read.
- [ ] T143 [P] `internal/neutron/reconcile_retired_test.go` (new): the retired state of FR-044 is removed where recognised — `LS-NSI-*` chains and jumps, marked host floating-IP NAT, `/32` uplink addresses of floating IPs with a record, bridge addresses, per-bridge metadata DNAT, host `dnsmasq`, `LS-SG-*` chains — and nothing unmarked without a record.
- [ ] T144 [P] `internal/neutron/vxlan_coordinator_test.go` (new): FDB entries whose row is gone are removed; a deleted network's VXLAN device is removed (`DeleteVXLAN` gains its caller).
- [ ] T145 [P] `internal/neutron/teardown_test.go` and `internal/teardown/teardown_test.go` (extend), R48: the purge converges every router and DHCP unit to absent and removes every `ls-` port chain and dispatch on the node, with a fake runner; the database-less fallback removes `qrouter-*` and `qdhcp-*` namespaces and `ls-` chains by prefix; it never touches what it does not recognise.
- [ ] T146 [P] Rework `internal/neutron/reconcile_dataplane_test.go`: its floating-IP cases become router-unit cases in T142; delete what no longer applies, and note it.

### Implementation

- [ ] T147 `internal/neutron/reconciler.go`: roles (network node or not), the units' passes with the complete-read rule, the retired-state removal, and the FDB/VXLAN removal in `internal/neutron/vxlan_coordinator.go`. `reconcileNAT`, `reconcileFloatingIPs`, `removeOrphanFilter`, `reconcileSubnets`' host DHCP and the egress SNAT pass go.

- [ ] T148 `internal/neutron/teardown.go` (`PurgeRealModeNetworkState`) and `internal/teardown/teardown.go` (the fallback), R48: purge the new layout as T145 pins it.

---

## Phase 24: The node — namespaces across restarts, bridge netfilter, deployment (FR-036, FR-046; R35)

### Tests (write first, confirm RED)

- [ ] T149 [P] `pkg/networking/nodesettings_test.go` (new): the check runs `modprobe br_netfilter` through the runner, writes `1` to both `/proc/sys/net/bridge/bridge-nf-call-*` files under a test root, reads them back, and reports which failed; stub mode runs nothing.
- [ ] T150 [P] `internal/health/health_test.go` (extend): a `bridge-netfilter` check failing makes `/readyz` `503` naming it; passing makes it ready.
- [ ] T151 [P] Chart tests `charts/lightstack/tests/netns_mount_test.yaml` (new) and `teardown_test.yaml` (extend): in real mode the pod and the teardown job mount `hostPath` `/run/netns` at `/run/netns` with `mountPropagation: Bidirectional` and `/lib/modules` read-only; stub mode mounts neither.

### Implementation

- [ ] T152 `pkg/networking/nodesettings.go` (new) and `cmd/lightstack/main.go`: the start-up check after `neutron.NewService` in real mode, its result as a `health.Check` (`internal/health/health.go` gains `AddCheck` or the checker moves after the check).
- [ ] T153 [P] `charts/lightstack/templates/_helpers.tpl` (pod spec volumes and mounts), `templates/teardown-purge-job.yaml`, and `deployments/docker-compose-kvm.yml` (`/run/netns:/run/netns:rshared`, `/lib/modules:/lib/modules:ro`).

---

## Phase 25: Round 2 polish

- [ ] T154 [P] `docs/UPGRADING.md`: a section for round 2 — the clean break (run `lightstack cleanup` and start fresh; no conversion), and a before/after table of every answer FR-020's round-2 bullets list (FR-019a–b, FR-024–FR-058, administrators' listings, shows and writes included).
- [ ] T155 [P] `docs/EBPF_STATUS.md`: the port chains enforce in both modes; Neutron's TAP is gone, so eBPF's attach point is too; the decision is the user's.
- [ ] T156 [P] `docs/NETWORKING_MODES.md`, `docs/VXLAN_IMPLEMENTATION.md`: the network node, namespaces, the port firewall, the node call.
- [ ] T157 [P] `docs/CONFIGURATION.md`: `neutron.network_node`; `docs/TROUBLESHOOTING.md`: remove the manual `/var/run/netns` step; `docs/SCALING.md`: the bridge sysctls are set by Lightstack; FR-035's underlay note (allow the floating range where the underlay delivers only assigned addresses).
- [ ] T158 [P] `charts/lightstack/README.md`: the mounts, multi-node's network node, the underlay note.
- [ ] T159 [P] `docs/specs/033-sg-port-membership/contracts/network-api.md` and `server-report.md`: point to `network-api-round2.md` where round 2 changes them. (`compute-security-groups.md` was restated during the round-2 analysis.)
- [ ] T160 Re-run T058's gates and append "Round 2 results" to `baseline-gates.md`, compared with T058 (build, lint, vet, gofmt, unit on both backends, contract on both backends, `helm unittest charts/lightstack`, `./test/security_groups_test.sh` with Test 11 passing).
- [ ] T161 Mutation-check round 2 off the worktree (`go test -overlay` or a scratch clone) and record it in `baseline-gates.md`: each listing filter ignored (T071); each `default` refusal removed (T078); the ambiguity check removed (T082); the migrator's foreign-key check removed (T061); starting rules not inserted (T079); pools ignored by the allocator, the gateway or floating IPs not excluded, each validation removed (T086, T087); the allocation lock removed (T097); the router lookup's floating-network check removed, the gateway-interface preference removed, FR-029's check removed (T098); gateway port or one-transaction interface reverted (T093); FR-039's checks removed (T094); the node token's audience, expiry, node or digest check removed (T108); the router unit's add or remove order swapped, the SNAT exception or hairpin rule dropped (T112, T113); the DHCP static range made dynamic, option 121's metadata route dropped (T119); metadata identity by address only (T121); the spoof chain, `INVALID` drop, remote-group expansion or zone rules dropped (T125); the TAP name dropped (T128); a deletion's record deleted before its units (T135); the re-apply on failure removed (T135); the complete-read rule removed from a removal (T142); `br_netfilter` failure not failing readiness (T150); the administrator branch of `projectScope` removed, and a show's administrator bypass removed (T102); a write's administrator target removed (T104); an administrator's foreign group id refused again (T105).
- [ ] T162 [P] Update `docs/specs/033-sg-port-membership/follow-ups.md` only if round 2 leaves something out; otherwise leave it as it is.
- [ ] T163 [P] The ticket's checklist (cloud-storage#579): draft the round-2 items in `docs/specs/033-sg-port-membership/ticket-checklist-round2.md` for the user to post or approve; do not post.
- [ ] T164 [P] Update `CLAUDE.md`'s Neutron real-mode paragraph and its Project Isolation bullet ("Listings never widen implicitly" is Compute's `all_tenants` rule; Networking API listings show administrators every project, FR-057) ("Project Isolation" and "Neutron real mode") for the network node, namespaces and the port firewall, replacing statements the layout change makes false.
- [ ] T165 Generate the round-2 host runbook `docs/specs/033-sg-port-membership/HOST_TESTING.md` with `/cl.hosttest docs/specs/033-sg-port-membership/`: two runs per quickstart §5 and research R45 — (i) the chart on one Kubernetes node, opening with FR-023's checks as restated in §5 (one connection attempt per second included), (ii) two nodes with a shared PostgreSQL and VXLAN — each with read-only prerequisites (`br_netfilter` loadable, `ip netns`, `iptables -m physdev --help`, `-m comment`, `ip6tables`), setup, steps with desired outcomes, and teardown; zsh-paste-safe. Replaces T053.
- [ ] T166 Run (i) on a Kubernetes node, its Horizon *Edit Security Groups* step included, and record each step's outcome in `HOST_TESTING.md`. Replaces T054.
- [ ] T167 Run (ii) on two hosts and record each step's outcome in `HOST_TESTING.md`.
- [ ] T168 FR-021: once (i)'s Horizon step has passed, record instance *Edit Security Groups* as verified in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, with the date. Replaces T055.

T057 (the rebase and renumbering to v1.6.0) stays last, after T168.


---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (T001 → T002)** comes before everything. T002, the approval, gates every later task.
- **Foundational (T003–T007)** comes before every story. T003, T005 and T006 run in parallel; T004 needs T003; T007 needs T005 and T006.
- **US1 (T008–T018)**: tests T008–T011 run in parallel. T013 needs T008. T014 → T015 → T016. T017 comes before T018. T018 needs T004 and T015.
- **US2 (T019–T021)**: needs T018. T019 and T020 run in parallel, then T021.
- **US3 (T022–T024)**: needs Foundational, and T018 for its remove case. T024 needs T004.
- **US4 (T025–T029)**: needs Foundational. T028 changes a signature every caller uses; run it after T017 and T018 have settled the interface. T029 needs T028.
- **US5**: no tasks. Covered by T006 and T010.
- **US6 (T030–T034)**: needs T004 and US1's T014–T015 (refresh, lock, seam). T033 → T034, which share `ports.go`.
- **US7 (T035–T048)**: needs US1's T013–T016. Tests T035–T041 run in parallel; T036 extends T009's fake. T042 needs T035. T043 needs T042. T044, T045 and T046 come after US6's T033–T034, which share `ports.go`. T046 needs T043. T047 needs T046 and comes after US4's T028–T029, which share `internal/nova/handlers.go`. T048 needs T043.
- **Polish (T049–T057)**: after the stories; T057 waits for `main` to carry the other amendment, and runs last, after T168, before review. T051 comes after all code; T052 after T051; T053 before T054; T055 after T054. T057 runs at the rebase onto a `main` that carries the other v1.5.0 amendment, before review, whenever that happens.

### Story completion order

US1 → US2 → US3 → US4 → US6 → US7, then Polish. US5 completes with US1.

### Within each story

Tests first, seen failing (RED), or passing from the start for a **lock**. Then the implementation, in the order given. Each story ends at a checkpoint where its independent test passes.

---

## Parallel Examples

```bash
# Foundational — three test files at once:
Task: "T003 internal/neutron/port_security_groups_db_test.go"
Task: "T005 internal/nova/network_fixture_test.go"
Task: "T006 internal/nova/server_detail_secgroups_test.go"

# US1 tests together:
Task: "T008 pkg/networking/ingress_filter_test.go"
Task: "T009 internal/neutron/fake_dataplane_test.go + ingress_refresh_test.go"
Task: "T010 internal/nova/security_group_actions_test.go"
Task: "T011 test/contract/nova/security_group_actions_test.go"

# US6 tests together:
Task: "T030 internal/neutron/port_api_security_groups_test.go"
Task: "T031 internal/neutron/security_group_list_filter_test.go"
Task: "T032 test/contract/neutron/port_security_groups_test.go"

# US7 tests together:
Task: "T035 pkg/networking/floating_ip_nat_test.go"
Task: "T036 internal/neutron/floating_ip_dataplane_test.go"
Task: "T037 internal/neutron/security_group_rule_refresh_test.go"
Task: "T038 internal/neutron/security_group_delete_test.go"
Task: "T039 internal/neutron/reconcile_dataplane_test.go"
Task: "T040 test/contract/neutron/security_group_delete_test.go"
Task: "T041 internal/nova/server_delete_floating_ips_test.go"

# Polish docs together:
Task: "T049 docs/UPGRADING.md"
Task: "T050 docs/EBPF_STATUS.md"
Task: "T056 docs/specs/033-sg-port-membership/follow-ups.md"
```

Never run these in parallel, because the tasks share a file:

| File | Tasks |
|---|---|
| `internal/neutron/ports.go` | T004, T028, T033, T034, T044, T045, T046 |
| `internal/neutron/floatingip.go` | T015, T042 (callers), T043, T044, T046 |
| `internal/neutron/network.go` | T014, T043 |
| `internal/neutron/reconciler.go` | T016, T048 |
| `internal/neutron/fake_dataplane_test.go` | T009, T036 |
| `pkg/networking/northsouth.go` | T013, T042 |
| `internal/nova/advanced_actions.go` | T007, T018, T021 |
| `internal/nova/handlers.go` | T007, T017, T028, T029, T046, T047 |
| `internal/nova/security_group_actions_test.go` | T010, T019 |
| `test/contract/nova/security_group_actions_test.go` | T011, T020, T027 |
| `internal/neutron/port_security_groups_db_test.go` | T003, T025 |
| `internal/nova/boot_failure_test.go` | T017, T028, T046 |
| `internal/nova/network_fixture_test.go` | T005, T028 |

---

## Implementation Strategy

### MVP first

**Foundational + US1 (T001–T018)**: add and remove through the compute API reach every port and the firewall:
- atomically, with no window, never reverted;
- bounded at one second;
- undone, compare-and-set, on a failed rebuild;
- reported from the ports.

That is the defect the ticket is about. It also delivers US5.

### Incremental delivery

1. **US2** (T019–T021): the last group, as upstream; Terraform can replace a server's only group.
2. **US3** (T022–T024): interfaces attached later get `default`.
3. **US4** (T025–T029): changes made mid-boot reach later ports; `server_security_groups` is retired; create uses the shared resolver.
4. **US6** (T030–T034): Horizon's *Edit Security Groups*, `port set`, Terraform's port and data source.
5. **US7** (T035–T048): floating-IP and rule changes in force when answered or undone; a group in use cannot be deleted; a deleted port or server takes its floating IPs' association with it; reconciliation removes what no record holds. Until it lands, floating-IP and rule changes still log a failed firewall step and continue, which v1.5.0 does not permit. The feature is not complete without it.
6. **Polish** (T049–T057): docs, gates, mutation check, runbook, host validation, follow-up drafts, and the constitution's renumbering at the rebase.

Each increment leaves the earlier stories' tests green.

---

## Notes

- 57 tasks: Setup 2, Foundational 5, US1 11, US2 3, US3 3, US4 5, US5 0 (covered by T006 and T010), US6 5, US7 14, Polish 9.
- Nothing here commits. The user commits.
- `server_security_groups` stays in the schema, unused (R1). No task drops it.

---

## Round 2: dependencies and execution order

- **T058 → T059** before any round-2 test. **Phase 12** (T060–T070) blocks every later round-2 phase.
- **US8** (Phase 13) and **US9** (Phase 14) depend only on Phase 12, and can run in parallel with each other.
- **US11** (Phase 15) before the router API (Phase 16), which allocates gateway addresses, and before **US10** (Phase 17), whose allocation uses T090's allocator and whose router lookup needs Phase 16's gateway ports. FR-039's checks (Phase 16) read floating IPs' `router_id` as stored today.
- **US12** (Phase 17a) after Phase 12; it touches every listing handler, so it runs after US10's edits to `floatingip.go` and before Phase 18.
- **Phase 18** (node call) after Phase 12; **Phases 19–21** (router, DHCP, port units) after Phase 18. Phase 19 after Phases 16 and 17. Phases 20 and 21 can run in parallel with 19 except where files overlap (`internal/neutron/units.go`: sequential).
- **Phase 22** (deletions) after Phases 19–21. **Phase 23** (reconciliation) after Phase 22. **Phase 24** (node) after Phase 20 (its `main.go` change follows T124's); its chart work (T151, T153) any time after Phase 12.
- **Phase 25** after everything; T165 → T166 → T167 → T168 → T057.

**Parallel within round 2**: every `[P]` test task of a phase together (different files); US8 ∥ US9; T068 ∥ T069 ∥ T070; T112 ∥ T113 ∥ T119 ∥ T125 ∥ T126 (pure `pkg/networking` tests); T154–T159 docs together.

**Shared files (implementation tasks touching them run in this order, never together)**:

| File | Tasks, in order |
|---|---|
| `internal/neutron/ports.go` | T076 → T077 → T083 → T085 → T090 → T106 → T107 → T118 → T123 → T132 → T133 → T140 |
| `internal/neutron/network.go` | T065 → T077 → T091 → T106 → T110 → T118 → T123 → T133 → T140 |
| `internal/neutron/floatingip.go` | T100 → T101 → T106 → T118 → T132 |
| `internal/neutron/router.go` | T096 → T106 → T118 → T140 |
| `internal/neutron/port_forwarding.go` | T106 → T118 → T140 |
| `internal/neutron/units.go` | T065 → T117 → T123 → T132 → T140 |
| `internal/neutron/reconciler.go` | T118 → T123 → T132 → T147 |
| `internal/neutron/teardown.go` | T118 → T132 → T148 |
| `pkg/networking/northsouth.go` | T118 → T132 |
| `cmd/lightstack/main.go` | T070 → T111 → T124 → T152 |
| `internal/nova/handlers.go` | T077 → T085 → T107 → T118 → T134 → T141 |


**Strategy**: round 2's first useful increment is Phases 11–17a — the four API findings, the router API and administrators' access, no layout change, deployable on its own. Phases 18–24 change the real-mode layout together; they are one increment, since a router namespace without the port firewall or DHCP namespace would leave the node half on each layout.
