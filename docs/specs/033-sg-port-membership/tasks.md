---

description: "Task list for SPEC-033 — server security-group membership reaches the ports"
---

# Tasks: Server security-group membership reaches the ports

**Feature**: `033-sg-port-membership` (cloud-storage#579)
**Input**: Design documents from `docs/specs/033-sg-port-membership/`
**Prerequisites**: plan.md and spec.md (required); research.md (R1–R24), data-model.md, contracts/ (4 files) and quickstart.md (present). Constitution **v1.5.0**, ratified 2026-11-17 at the analysis gate and refined on 2026-11-18 at the second (research R19); the plan's gate is open. Register entry 5 and the two `CLAUDE.md` lines were added with the amendment, so no task carries them.

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
- [ ] T053 Generate the host runbook `docs/specs/033-sg-port-membership/HOST_TESTING.md` with `/cl.hosttest docs/specs/033-sg-port-membership/`. It covers prerequisites, setup, the FR-023 steps (a)–(h) as `quickstart.md` §2 and research R18 lay them out (one connection attempt per second in (d)), and teardown, all zsh-paste-safe. The prerequisites block checks, read-only, that iptables' `comment` match works: `iptables -m comment --help` succeeds, and with the legacy backend (`iptables -V`) `modinfo xt_comment` finds the module. Without it, every association answers `500`.
- [ ] T054 Run host validation (a)–(h) with T053's runbook on one real-mode node (libvirt, root, a routed floating IP, iptables' `comment` match, a second guest for (g) and (h)), and record each step's outcome in `docs/specs/033-sg-port-membership/HOST_TESTING.md`.
- [ ] T055 FR-021: once T054 step (f) has passed, record instance *Edit Security Groups* as verified in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, with the date. Verified means it pre-selects the instance's groups and changes its ports.
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
- **Polish (T049–T057)**: after the stories. T051 comes after all code; T052 after T051; T053 before T054; T055 after T054. T057 runs at the rebase onto a `main` that carries the other v1.5.0 amendment, before review, whenever that happens.

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
