---

description: "Task list for SPEC-033 — server security-group membership reaches the ports"
---

# Tasks: Server security-group membership reaches the ports

**Feature**: `033-sg-port-membership` (cloud-storage#579)
**Input**: Design documents from `docs/specs/033-sg-port-membership/`
**Prerequisites**: plan.md and spec.md (required); research.md (R1–R53), data-model.md, contracts/ (8 files) and
quickstart.md (present). Round 2 (T058–T168) starts at Phase 11. Constitution **v1.6.0**, ratified 2026-11-17 at the
analysis gate and refined on 2026-11-18 at the second (research R19); the plan's gate is open. Register entry 5 and the
two `CLAUDE.md` lines were added with the amendment, so no task carries them.

**Tests**: INCLUDED. The spec requires them (FR-022), as does the constitution's Test-First Development:
1. contract tests with OpenStack clients where an answer changes;
2. the user approves the test strategy before any test is written (T002);
3. every test is seen failing first (RED); a failing compile counts;
4. then the code.

A test marked **lock** passes before and after: it guards behaviour this feature must not break.

**Organization**: by the spec's user stories. Priority order is US1 and US2 (P1); then US3, US4, US6 and US7 (P2); then
US5 (P3). US2 needs US1's remove. US6 needs US1's refresh and undo. US7 needs US1's runner, node lock and dataplane seam
(T013–T016). US5 has no tasks of its own: its two scenarios are red in T006 and T010, before the code that makes them
pass.

**Terms**:
- *report*: a server's `security_groups` in show and detail.
- *a port's groups*: its `port_security_groups` rows.
- *rebuild*: applying a floating-IP filter.
- *converge*: make the node's filter, NAT and address for one floating IP match its record (R23).
- *node lock*: the Neutron service's one lock for the node's floating-IP state, acquired per address with a one-second
  deadline (R8).
- *seams*: the dataplane interface and the uplink function on the Neutron service (R9), the record read on the
  reconciler (R24), and the command runner on the `RouterManager` (R6).
- *the fake*: the in-memory implementation of the dataplane interface in `internal/neutron`'s tests (T009, extended in
  T036).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel: a different file, and no dependency on an incomplete task
- **[Story]**: US1–US7 for story-phase tasks; Setup, Foundational and Polish carry none
- Every task names an exact file path. Line numbers are hints from `d81f081`, the `main` commit this branch is based on;
  find the code by the function named.

## Path conventions

Single Go project at the repo root (`cmd/`, `internal/`, `pkg/`, `test/`), docs under `docs/`. No `src/` layout (plan.md
§ Project Structure).

**Test databases** (CLAUDE.md):
- PostgreSQL: `make db-up`, then
  `LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'`
- SQLite: `LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db'`
- Tests that read the seeded project can race across packages; use `go test -p 1` if they flake.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: a green baseline that SC-008 is measured against, and the approved test strategy.

- [X] T001 Record the baseline in `docs/specs/033-sg-port-membership/baseline-gates.md`. Run each of these and write
      down pass/fail/skip counts per package, plus any failure that predates this branch:
  - `make build`, `make lint`;
  - `go test ./...` on PostgreSQL and on SQLite (both URLs above);
  - `make test-contract` against a running stub Lightstack (Docker Compose), once with a PostgreSQL and once with a
    SQLite `database.url`;
  - `./test/security_groups_test.sh` and `./test/neutron_patch_test.sh`. Both delete groups, whose answers FR-008c
    changes.

  Fix nothing here. A failure that predates the branch is recorded, not fixed.
- [X] T002 Get the test strategy approved before any test is written (constitution, Test-First Development, step 2).
      Review research.md R16 and every test task in this file with the user. Record the approval in
      `docs/specs/033-sg-port-membership/baseline-gates.md`, with the date and any change asked for. No later task
      starts before it.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: the Neutron helpers every write path uses (R2), the report read from the ports (R10), which every story's
tests read, and a Nova test fixture with the real Neutron service (R16).

**⚠️ CRITICAL**: no story work starts before T007 is green.

- [X] T003 [P] Write `internal/neutron/port_security_groups_db_test.go` (new, DB-backed: `requireDB(t)` and `testPool`
      from `network_db_test.go`). Subtests, each on a port in the seeded project:
  - `PortSecurityGroupIDs` returns the port's group ids; a port with none gives an empty slice, not an error.
  - `AddPortSecurityGroup(ctx, tx, port, group)` inserts the row and moves the port's `updated_at`.
  - `RemovePortSecurityGroup(ctx, tx, port, group)` deletes it, moves `updated_at` and reports `true`. A group the port
    does not carry reports `false`, with no error.
  - `SetPortSecurityGroups(ctx, tx, port, ids)` replaces the list, including with an empty one, and moves `updated_at`.
  - `AddPortSecurityGroup` and `SetPortSecurityGroups` naming the id of a group that no longer exists, as when it was
    deleted after it was resolved, return the exported not-found error, not a foreign-key error (R22).
  - A rolled-back transaction leaves the rows unchanged.
  - `ResolveSecurityGroup(ctx, q, ref, projectID)`:
    - resolves by name and by id;
    - an id match wins over a group named after that id;
    - another project's group, by name or by id, gives the exported not-found error;
    - a malformed id gives not-found, not a query error.
- [X] T004 Implement the T003 helpers in `internal/neutron/ports.go` (research R2): `PortSecurityGroupIDs`,
      `AddPortSecurityGroup`, `RemovePortSecurityGroup`, `SetPortSecurityGroups`, `ResolveSecurityGroup` and an exported
      not-found error.
  - The writers take a `database.Tx`; the readers take a querier such as `subnetQuerier` (`ports.go:668`), so they work
    on a transaction or the pool.
  - A foreign-key violation on the group (`database.IsForeignKeyViolation`) is returned as the exported not-found error,
    so every caller answers `404` for a group deleted after it was resolved.
  - `ResolveSecurityGroup` is today's create query (`internal/nova/handlers.go:881-887`): `(id::text = $1 OR name = $1)
    AND project_id = $2 ORDER BY (id::text = $1) DESC LIMIT 1`. Create switches to it in T029.

  This makes T003 green.
- [X] T005 [P] Write `internal/nova/network_fixture_test.go` (new, helpers only):
  - Factor the `X-Test-*` middleware out of `novaRouterOn` (`internal/nova/testrouter_test.go:31-45`) so the fixture
    reuses it.
  - `newNovaWithNetwork(t) (*gin.Engine, *Service, *neutron.Service)`: `NewService(testPool, "", "stub", nil)`,
    `neutron.NewService(testPool, "stub", nil)` set with `SetNeutronService`, Nova's routes behind the middleware.
  - `createTestNetwork(t, projectID) (networkID, subnetID string)`: a network and a /24 subnet, as
    `internal/nova/ip_allocation_concurrency_test.go:23-34` inserts them. A `t.Cleanup` deletes the ports, interface
    attachments, subnet and network.
  - `createTestGroup(t, projectID, name) string`, with cleanup.
  - `createTestServer(t, r, projectID, body) string` via `POST /v2.1/servers`. Stub mode has no boot goroutine, so the
    test allocates ports itself.
  - `allocateTestPort(t, net *neutron.Service, networkID, projectID, serverID string, groupIDs []string) string` via
    `AllocatePortForInstance`.
- [X] T006 [P] Rework `internal/nova/server_detail_secgroups_test.go` for the report read from the ports (FR-014,
      FR-015, FR-016; `contracts/server-report.md`). It is RED against `main`. Use the T005 fixture. Cases:
  - one port with `web` → show lists `web` only;
  - one port with `default` and `web` → both;
  - two ports, each in `default` → `default` twice;
  - no port → `security_groups` absent, in show and in detail;
  - a port with no group → absent;
  - a server shaped like v0.6.1's (ports without groups, no `server_security_groups` row) → absent. This is FR-015 and
    US5's first scenario;
  - a group with an empty name → its id;
  - a group deleted once no port carried it → not listed. This replaces `OrphanDropped`; a group a port carries can no
    longer be deleted (FR-008c);
  - detail over three servers with different ports → each lists its own entries.

  Keep the create-validation tests as **locks**: `UnknownSecurityGroup_400`, `EmptySecurityGroupName_400`,
  `NamelessSecurityGroupItem_Ignored`, `EnsureDefault_FreshProject`, `ImplicitDefault_FreshProject`,
  `EnsureDefaultSecurityGroup_SeedsOnlyWhenCreated`. Where they read the report, allocate a port first. These locks also
  guard T029's switch of create to the shared resolver.
- [X] T007 Implement the report in `internal/nova/advanced_actions.go` and `internal/nova/handlers.go` (research R10):
  - Replace `securityGroupNamesForServer` and `securityGroupNamesForServers` (`advanced_actions.go:668`, `:697`) with
    one port-derived query, one entry per row: `SELECT p.device_id::text, COALESCE(NULLIF(sg.name, ''), sg.id::text)
    FROM ports p JOIN port_security_groups psg ON psg.port_id = p.id JOIN security_groups sg ON sg.id =
    psg.security_group_id WHERE p.device_id = ANY($1::uuid[]) ORDER BY p.created_at, p.id, sg.name`.
  - `formatSecurityGroups` (`:729`) returns `nil` for none.
  - Detail (`handlers.go:1686-1696`) and show (`:1976-1983`) set `security_groups` only when it is not nil.

  This makes T006 green, and its locks stay green.

**Checkpoint**: the helpers exist, the report reads the ports, and Nova tests can build servers with ports.

---

## Phase 3: User Story 1 — Adding or removing a group changes what reaches the server (Priority: P1) 🎯 MVP

**Goal**: add and remove through the compute API reach every port and the floating-IP filter: atomically, with no
window, never reverted, bounded at one second, and undone (compare-and-set) on failure. Covers FR-001–FR-004, FR-006,
FR-007, FR-007a and FR-007b for the filter, FR-008 for membership, FR-010's re-assertion (US7 adds its removal), and
US5's second scenario.

**Independent Test**: stub mode, add and remove a group on a server with a port; its ports and the report follow. Real
mode: quickstart §2 (a), (d), (e).

### Tests for User Story 1 (write first, confirm RED)

- [X] T008 [P] [US1] Write `pkg/networking/ingress_filter_test.go` (new), per `contracts/floating-ip-dataplane.md` (the
      filter) and research R6–R7. Rework `pkg/networking/ingress_rule_test.go`'s cases into the canonical form and
      delete the old test.
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
    - `RestorePayload(chain, lines)` returns `*filter`, `:<chain> - [0:0]`, `-F <chain>`, the lines and `COMMIT`, each
      ending in a newline.
    - `ChainMatches(current, desired []string) bool`:
      - true for equal lines, and the `-N <chain>` line of `iptables -S` is ignored;
      - false for an extra rule, a missing rule, reordered rules, a missing `DROP`, or empty output (no chain).
  - **`EnsureIngressFilter` in real mode, with a fake command runner** (R6, FR-007b). Build a `RouterManager` in a real
    mode and replace its runner. Cases:
    - **Unchanged.** The runner's `iptables -S` returns exactly the rendering → no `iptables-restore` runs, and only the
      jump is checked.
    - **Changed or missing.** A changed chain, or a missing one → exactly one `iptables-restore`, carrying the payload
      on stdin, then the jump.
    - **Restore fails.** The restore's error is returned.
    - **Deadline.** Every command the runner sees carries a deadline of one second at most, the jump's check and append
      included.
    - **Blocking runner.** A runner that blocks until its context ends → `EnsureIngressFilter` returns an error within
      about one second.
  - **Stub mode.** No command runs.
- [X] T009 [P] [US1] Write `internal/neutron/fake_dataplane_test.go` (new, helpers only) and
      `internal/neutron/ingress_refresh_test.go` (new; DB-backed; stub mode with the service's dataplane replaced by the
      fake).
  - **The fake** implements T014's interface. It keeps, per fixed IP, the last rules applied; records every call in
    order; and can make any call block on a channel or return an error. T036 extends it.
  - Insert `floating_ips` rows for the ports directly. Cases:
    - **One floating IP.** `RefreshPortFilters(ctx, []string{port})` for a port with one floating IP calls the fake's
      `EnsureIngressFilter` once, with its fixed IP and the port's rules from the database (a `web` tcp/80 rule gives
      one rule).
    - **Other counts.** Two floating IPs give two calls; none gives no call.
    - **Emptied port.** A port whose groups were all removed gives one call with no rules: the reply-only filter
      (FR-007).
    - **Errors.** The fake's error is returned, naming the port and the address.
    - **Node lock (R8).**
      - The fake's call for address X blocks on a channel.
      - A second rebuild, of X or of another address Y, does not reach the fake until the first returns.
      - Change X's port groups in the database while the first call is blocked, then release it: the second rebuild of X
        carries the new rules.
    - **Lock deadline (FR-007b).** While a fake call stays blocked, a `RefreshPortFilters` returns an error after about
      one second, without calling the fake.
    - **No uplink.** With the service's uplink function failing, as on a real-mode node without one,
      `RefreshPortFilters` returns an error naming the port, without calling the fake.
    - **Reconciliation.** One pass (`reconcileNAT`) rebuilds the filter through the same locked path. While a blocked
      call holds the lock, the pass skips that address, and the next pass rebuilds it.
- [X] T010 [P] [US1] Write `internal/nova/security_group_actions_test.go` (new). Use the T005 fixture, with a test type
      embedding `*neutron.Service` that overrides `RefreshPortFilters` to record calls, optionally run a callback first,
      and optionally fail. Cases:
  - **Add.** One port in `default`: add `web` → `202`; the port lists `default` and `web`; the report lists both;
    `RefreshPortFilters` received the port.
  - **Two ports.** Add and remove change both.
  - **By id.** The same as by name.
  - **Unknown group.** An unknown name → `404`. Another project's group, by name and by id → `404`, and nothing changes.
  - **Administrator.** With `X-Test-Roles: admin`, acting on the tenant's server: the tenant's group by name → `202`.
    The administrator's own group by id → `404` (register entry 5).
  - **No port.** Add and remove → `404` ("could not be found as device id on any ports"), and nothing changes.
  - **Duplicate.** One of two ports already carries the group → add answers `400`, and neither port changes.
  - **Not carried.** Removing a group no port carries → `404`.
  - **Earlier release (US5, second scenario).** A server shaped like v0.6.1's: one port with no group, no
    `server_security_groups` row. Add `web` → `202`; the port carries `web`; the report names it. RED on `main`, where
    add writes only `server_security_groups`.
  - **Failure.** The wrapper fails `RefreshPortFilters`:
    - add → `500`, and the ports list what they listed before;
    - remove → `500`, and the group is back on every port it was removed from;
    - in both, a second `RefreshPortFilters` follows the undo.
  - **Compare-and-set (FR-008).** The wrapper's callback changes one port's list with `SetPortSecurityGroups` (another
    request), then fails:
    - add → `500`; that port keeps the other request's list, and the other ports are put back;
    - remove → the same.
  - Removing a server's last group is not asserted here; US2 changes it.
- [X] T011 [P] [US1] Rework `test/contract/nova/security_group_actions_test.go` (gophercloud, stub stack). Servers are
      created on a network with `createNetworkWithSubnet`, `createNamedServerOnNetwork` and `awaitAddresses`
      (`test/contract/nova/server_addresses_test.go`):
  - `TestNovaAddSecurityGroup_Contract`: `202`; every port from `ports.List(…, ports.ListOpts{DeviceID: server})` has
    the group's id in `SecurityGroups`; the server's `security_groups` names it.
  - `TestNovaAddSecurityGroupDuplicate_Contract`: on a server with a network, adding `default` → `400` (unchanged
    answer).
  - `TestNovaRemoveSecurityGroup_Contract`: the group leaves every port.
  - New cases: add by the group's id; add and remove on a server created without a network → `404`.
  - The group's cleanup runs after the server's, since a group a port carries can no longer be deleted (FR-008c).
  - `test/contract/nova/server_actions_test.go` has an older pair, `TestAddSecurityGroup_Contract` and
    `TestRemoveSecurityGroup_Contract`, that booted without a network. Both now answer `404` there, so they boot on a
    network through the same helpers. Their assertions are unchanged; T020 handles the remove's guard.
  - `TestNovaActionsByName_Contract` (`test/contract/nova/action_by_name_test.go`) sends both actions to a server
    addressed by name and reads any `404` as the name not resolving. Its server boots on a network, so the actions find
    a port.
  - `TestNovaServerReadBack_SecurityGroups_Contract` (`test/contract/server_readback_test.go`) read `default` back from
    a server without a network. It boots on a network and waits for its port (found by T051).
- [X] T012 [P] [US1] Delete `test/nova_server_actions_test.sh`. No Makefile target, workflow or script ran it; it
      stopped at its server create for want of an image named `test-image`, and it never failed on a wrong answer. The
      contract tests send all eight of its actions, and T011 checks every port's groups after add and remove.

### Implementation for User Story 1

- [X] T013 [US1] In `pkg/networking/northsouth.go` (research R6–R7):
  - Add `RenderIngressFilter`, `RestorePayload` and `ChainMatches`.
  - Add a command runner to `RouterManager`: a function field taking a context, the command, its arguments and its
    stdin. Its default runs `exec.CommandContext`. Every `iptables` and `iptables-restore` call of the filter path, the
    jump's check and append included, goes through it under a fresh one-second context.
  - Rewrite `EnsureIngressFilter` (`:157`):
    1. In stub mode, return `nil`.
    2. Read `iptables -S <chain>`; a missing chain reads as empty.
    3. If `ChainMatches`, only ensure the `FORWARD` jump.
    4. Otherwise run `iptables-restore --noflush --wait 1` with `RestorePayload` on stdin. On an error, return it: the
       old chain stays whole. Then ensure the jump.
  - Delete the flush-and-append sequence and `ingressRuleSpec` (`:210`), which the renderer replaces.
    `RemoveIngressFilter` is unchanged here; T042 rewrites it.

  This makes T008 green.
- [X] T014 [US1] In `internal/neutron/network.go`, add three `Service` fields, all set in `NewService`:
  - `nsLock`, a one-slot channel: the node lock (R8). A helper acquires it with a one-second deadline and returns a
    release function, or a timeout error.
  - `dataplane`, an unexported interface holding `EnsureIngressFilter(fixedIP, uplink string, rules
    []networking.IngressRule) error`, set to `routerManager`. T043 adds the NAT, removal and listing methods.
  - `uplink func() (string, error)`: `networking.DefaultUplinkInterface` in real mode, and a function returning `""` in
    stub mode (research R9).
- [X] T015 [US1] In `internal/neutron/floatingip.go`:
  - `ensureIngressFilter` (`:680`) acquires the node lock (one-second deadline). A timeout is an error naming the
    address. Inside the lock it reads `portIngressRules` and calls `dataplane.EnsureIngressFilter`.
  - Add `RefreshPortFilters(ctx, portIDs []string) error`. Per port, it reads `SELECT fixed_ip_address FROM floating_ips
    WHERE port_id = $1 AND fixed_ip_address IS NOT NULL`, then calls `ensureIngressFilter` for each address. The uplink
    comes from `svc.uplink()`. Its error is a failed change naming the port: it never skips the work (FR-008). It tries
    every address and returns `errors.Join` of the failures, each naming its port and address.
  - The floating-IP handlers keep calling `ensureIngressFilter` as they do; T043 rewrites them.

  This makes T009 green, with T014 and T016.
- [X] T016 [US1] In `internal/neutron/reconciler.go` (`:133-136`), the per-floating-IP filter step goes through the
      locked `ensureIngressFilter`. A lock timeout skips that address for this pass and is logged at debug, because the
      holder is changing it. Unchanged filters are left alone by `EnsureIngressFilter` itself (T013).
  - The pass takes its uplink from `svc.uplink()` (T014) instead of `networking.DefaultUplinkInterface()` (`:122`).
    Stub-mode tests, T009's reconciliation case among them, then no longer depend on the test machine having a default
    route. T048 keeps this when it makes the pass converge both ways.
- [X] T017 [US1] In `internal/nova/handlers.go` (`:61`), `NeutronService` gains `RefreshPortFilters(ctx context.Context,
      portIDs []string) error`. Its doc comment says: the serving node only; stub mode never fails; an error means the
      old filters still stand. `fakeNeutron` (`internal/nova/boot_failure_test.go:46`) implements it, recording its
      calls and returning a configurable error.
- [X] T018 [US1] Rewrite `AddSecurityGroup` (`:741`) and `RemoveSecurityGroup` (`:829`) in
      `internal/nova/advanced_actions.go`, per research R4–R5 and `contracts/compute-security-groups.md`:
  1. In one `database.WithTx`:
     - lock with `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`, then `SELECT id FROM ports WHERE device_id = $1
       ORDER BY id FOR UPDATE`. No instance row means the server was deleted meanwhile: `404`;
     - resolve the group with `neutron.ResolveSecurityGroup(ctx, tx, name, projectID)`;
     - no port → `404`, "instance_id `<id>` could not be found as device id on any ports";
     - add: if any port carries the group → `400`; else `neutron.AddPortSecurityGroup` on each port. Its not-found error
       (a group deleted meanwhile) → `404`;
     - remove: `neutron.RemovePortSecurityGroup` on each port; if none removed one → `404`;
     - keep today's last-group refusal, recomputed on the ports (the server's ports carry one distinct group in total →
       `400`). US2 removes it.
     - Before committing, record each changed port's resulting list (`PortSecurityGroupIDs`).
  2. After the commit, call `svc.neutronSvc.RefreshPortFilters(ctx, ports)`; a nil `neutronSvc` skips it. On an error:
     - a second transaction takes the same locks in the same order, then undoes this request's change on each port whose
       current list still equals the recorded one. Add deletes the group's row again; remove inserts it again. A port
       that differs keeps its list;
     - call `RefreshPortFilters` again and log its error;
     - log the first error, naming the port and address;
     - answer `500`.
  3. Drop every read and write of `server_security_groups` and the two stale comments (`:824`, `:928`). Answer `202`.

  This makes T010 and T011 green.

**Checkpoint**: T008–T011 are green, and the locks still pass.

---

## Phase 4: User Story 2 — Removing the last group closes the server, as upstream (Priority: P1)

**Goal**: FR-005: removing a server's last group answers `202`, and its ports carry no group.

**Independent Test**: remove a server's only group: `202`, its ports list no group, and the server reports none. Real
mode: quickstart §2 (c).

### Tests for User Story 2 (write first, confirm RED)

- [X] T019 [P] [US2] In `internal/nova/security_group_actions_test.go`:
  - a server whose only group is `web`, on one port: remove `web` → `202`; the port lists no group; show has no
    `security_groups`; `RefreshPortFilters` received the port;
  - then add `db` → the port carries `db`, and the report names it;
  - two ports, both carrying only `web`: remove → both empty.
- [X] T020 [P] [US2] In `test/contract/nova/security_group_actions_test.go`:
  - add `TestNovaRemoveLastSecurityGroup_Contract`: a server on a network with only `default`; remove it → `202`;
    `ports.Get(...).SecurityGroups` is empty; show has no `security_groups`;
  - add `TestNovaReplaceOnlySecurityGroup_Contract`, in Terraform's order: only `a`, remove `a`, then add `b` → both
    `202`, and the port carries only `b`;
  - delete the guard's mentions from `TestNovaRemoveSecurityGroup_Contract`'s doc comment (`:115-116`) and from
    `test/contract/nova/server_actions_test.go:387`.

### Implementation for User Story 2

- [X] T021 [US2] Delete the last-group refusal from `RemoveSecurityGroup` in `internal/nova/advanced_actions.go` (T018,
      step 1). This makes T019 and T020 green. The reply-only filter for an emptied port is already covered by T009.

**Checkpoint**: US1 and US2 together complete the P1 scope.

---

## Phase 5: User Story 3 — An interface attached later is filtered as upstream (Priority: P2)

**Goal**: FR-013: a port attached on a network gets `default`; a port attached by id keeps its groups.

**Independent Test**: attach on a network and attach by id to a server in `web`; read each port and the server.

### Tests for User Story 3 (write first, confirm RED)

- [X] T022 [P] [US3] Write `internal/nova/interface_attach_groups_test.go` (new; T005 fixture). Start from a server with
      one port in `web`:
  - `POST /v2.1/servers/{id}/os-interface` with the `net_id` of a second network → `200`. The new port lists `default`
    only, in a fresh project too (the group is created). The report lists `web` and `default`.
  - With the `port_id` of a free port carrying `db` → the port still lists `db`, and the report includes `db`.
  - Then remove `web` → `202`; only the first port changes.
  - `DELETE /v2.1/servers/{id}/os-interface/<db port>` → the report no longer lists `db`, and the port keeps it.
- [X] T023 [P] [US3] Write `test/contract/nova/interface_attach_groups_test.go` (new). Start from a server on one
      network, then attach an interface on a second network (a raw `POST /servers/{id}/os-interface`):
  - the new port's `SecurityGroups` is `default`'s id alone;
  - the server's `security_groups` names `default`.

### Implementation for User Story 3

- [X] T024 [US3] In `internal/nova/interface_attach.go`, on the `net_id` path:
  - call `neutron.EnsureDefaultSecurityGroup(ctx, svc.pool, projectID)` before the transaction (`:206`);
  - inside the transaction, after the port insert (`:228`), call `neutron.AddPortSecurityGroup(ctx, tx, portID,
    defaultID)`.

  The `port_id` path is unchanged. This makes T022 and T023 green.

---

## Phase 6: User Story 4 — Create and boot keep their behaviour (Priority: P2)

**Goal**: FR-011 and FR-012: a change made between a boot's ports reaches the later ports; create and the implicit
default are unchanged; `server_security_groups` is no longer written (R1); create resolves groups through the shared
function (R2).

**Independent Test**: create with and without groups and read the port; change the groups between the first and second
port of a two-network boot.

### Tests for User Story 4 (write first, confirm RED)

- [X] T025 [P] [US4] In `internal/neutron/port_security_groups_db_test.go`, test `AllocatePortForInstance(…,
      securityGroupIDs, copyGroupsFrom, admin)`:
  - **First port.** It carries the given groups; none gives `default` (**lock**, under the new signature).
  - **A later port.** With `copyGroupsFrom` set to the first port, it carries the first port's current groups. After
    `AddPortSecurityGroup` on the first port in between, the later port carries the added group too (FR-011).
  - **Instance lock.** The allocation locks the instance row (PostgreSQL only; skip on SQLite, where immediate
    transactions serialise anyway):
    - a transaction holds `SELECT … FROM instances WHERE id = $1 FOR UPDATE` and adds a group to the first port;
    - the allocation runs in a goroutine meanwhile;
    - the holder commits after a short wait;
    - the copy includes the group.
- [X] T026 [P] [US4] Write `internal/nova/boot_groups_test.go` (new), with `newBootService`
      (`internal/nova/boot_failure_test.go`) and a recording fake. Cases:
  - **Two networks.** A create with `security_groups: web`: the first `AllocatePortForInstance` call gets `web`'s id and
    an empty copy-from; the second gets no groups and the first port's id.
  - **No groups.** A create without groups: the first call gets `default`'s id (**lock**).
  - **No stored copy.** After a create, `server_security_groups` has no row for the server (R1).
- [X] T027 [P] [US4] Add `TestNovaCreateWithSecurityGroupReachesPort_Contract` to
      `test/contract/nova/security_group_actions_test.go`. Create on a network:
  - with a non-default group → the port lists it and not `default` (**lock**: on `main` since cloud-storage#524);
  - without groups → `default` only (**lock**).

### Implementation for User Story 4

- [X] T028 [US4] Change `AllocatePortForInstance` in `internal/neutron/ports.go` (`:1596`) and every caller in the same
      step, so every package keeps compiling:
  - Add `copyGroupsFrom string` after `securityGroupIDs`.
  - Inside the transaction, before `LockSubnet`, run `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`. A missing row
    is not an error: callers outside a boot allocate for ids without one.
  - When `copyGroupsFrom != ""`, use that port's groups from `PortSecurityGroupIDs(ctx, tx, …)`; an empty list stays
    empty. Otherwise behave as today.
  - Update `NeutronService` (`internal/nova/handlers.go:62`).
  - Update the production call (`handlers.go:1146`), passing `""` for now; T029 adds the copy-from.
  - Update `fakeNeutron` (`internal/nova/boot_failure_test.go:60`), `internal/nova/ip_allocation_concurrency_test.go:59`
    and the T005 fixture.

  This makes T025 green.
- [X] T029 [US4] In `internal/nova/handlers.go`:
  - In the boot loop (`:1141-1185`), pass `sgIDs` and `""` for the first network, then `nil` and the first allocation's
    `PortInfo.ID` for each later network.
  - Create's transaction (`:1026-1048`) stops inserting `server_security_groups`; update its comment (`:1019-1024`).
  - Replace create's inline group resolution (`:855-905`, the query at `:881-887`) with `neutron.ResolveSecurityGroup`.
    The `default` fallback and the `400` answers stay as they are, so there is one resolver for create, add and remove
    (R2).

  This makes T026 green. T027 and T006's locks stay green.
  - `internal/nova/security_group_ref_test.go`'s `TestCreateServerResolvesSecurityGroupByIDOrName` read the resolved
    group from `server_security_groups`, which create no longer writes. It boots on a network with `newBootService` and
    reads the group from the first allocation instead.

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

**Goal**: FR-017, FR-018, FR-019 and FR-019a: `security_groups` on port create and update, the group listing's `id` and
`name`, and Horizon's *Edit Security Groups*.

**Independent Test**: `PUT /v2.0/ports/{id}` with `security_groups`: the port and its server report the new list;
Horizon's sequence through the API.

### Tests for User Story 6 (write first, confirm RED)

- [X] T030 [P] [US6] Write `internal/neutron/port_api_security_groups_test.go` (new; DB-backed). Use the Neutron router
      behind stub auth, as the package's other handler tests do, with the service's dataplane replaced by the fake
      (T009). Cases:
  - **`PUT /v2.0/ports/{id}`, valid bodies.**
    - `{"port":{"security_groups":["<web>"]}}` → `200`; the port lists `web` only; `updated_at` moved.
    - `[]` → no group; `null` → no group.
    - Absent, with a `name` → the groups are unchanged and the name changes. `name` and `security_groups` together →
      both apply.
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
    - While another change on the node holds the lock (a fake call blocked in a goroutine), the `PUT` answers `500`
      after about one second and the port's list is back, as T009 does for `RefreshPortFilters`.
  - **`POST /v2.0/ports`.** `[]` → no group; `null` → no group; absent → `default`. Malformed, repeated or foreign ids →
    `400`, `400`, `404`.
- [X] T031 [P] [US6] Write `internal/neutron/security_group_list_filter_test.go` (new). Groups `a`, `b`, `c` in one
      project and `x` in another:
  - `?id=a` → `[a]`; `?id=a&id=b` → `a` and `b`;
  - `?name=b` → `[b]`; `?id=a&name=b` → `[]`;
  - `?id=<x>` → `[]`; `?id=not-a-uuid` → `200 []`;
  - no query → `a`, `b`, `c` (**lock**).
- [X] T032 [P] [US6] Write `test/contract/neutron/port_security_groups_test.go` (new; gophercloud). Cases:
  - **Port update and create.** `ports.Update` with `SecurityGroups: &[]string{web}`, then with `&[]string{}`.
    `ports.Create` with `&[]string{}` → no group.
  - **Listing.** `groups.List` with `ListOpts{Name: …}` → exactly one group. A raw `GET /v2.0/security-groups?id=a&id=b`
    → exactly those two.
  - **Horizon's *Edit Security Groups* sequence**, on a server with a network:
    1. list its ports by device id;
    2. list the ids found there with a repeated `?id=` → exactly the server's groups;
    3. update each port with a chosen list → the server's `security_groups` reports it.

### Implementation for User Story 6

- [X] T033 [US6] In `internal/neutron/ports.go`, `CreatePort` (`:38`) and `UpdatePort` (`:563`) read `security_groups`
      as `json.RawMessage` (research R12).
  - **Validation, in this order:**
    1. each entry is a UUID;
    2. no entry is repeated;
    3. for an update, the port exists in the caller's project, else `404`. The update transaction's `SELECT … FOR
       UPDATE` of the port is this check;
    4. each group is the port's project's, else `404` with `NewResourceNotFoundError("security group", id)`. For an
       update it is read in the same transaction, after the port's lock (constitution v1.6.0, limit 2). No lock is
       needed: a group's project never changes, and the foreign key re-checks its existence at the write, which T004
       maps to the same not-found error.

    Steps 1 and 2 read only the body and run before any database access.
  - **`CreatePort`.** `[]` or `null` → no rows; absent → `default`, as today.
  - **`UpdatePort`.**
    - In one transaction: lock the port (`SELECT … FOR UPDATE`), read the old list, call `SetPortSecurityGroups`, and
      apply the other fields. `u.Empty()` (`:599`) counts `security_groups` as an update.
    - After the commit, call `RefreshPortFilters([port])`.
    - On a failure: a second transaction locks the port again and restores the old list only if the port still carries
      the list just written. Then rebuild again and answer `500`.
    - Otherwise answer with `GetPort`, as today.

  This makes T030 green.
- [X] T034 [US6] In `internal/neutron/ports.go`, `ListSecurityGroups` (`:961`) and `listSecurityGroupRows` (`:926`)
      build the `WHERE` with `common.SQLFilter`: the project, then `id::text = ANY(…)` over `c.QueryArray("id")`, then
      `name = …`. This makes T031 green, and T032 with T033.

**Checkpoint**: US6 is independently green.

---

## Phase 9: User Story 7 — Floating-IP and rule changes are in force when the API answers (Priority: P2)

**Goal**: FR-007a and FR-007b for the NAT; FR-008 for rules and floating IPs; FR-008a, FR-008b, FR-008c, FR-008d and
FR-008e; FR-010's removal half; SC-011, SC-013, SC-014 and SC-015. Research R20–R25,
`contracts/floating-ip-dataplane.md` and `contracts/network-api.md`.

**Independent Test**: stub mode with the fake. Associate, move, disassociate and delete a floating IP, and add and
delete a rule: the fake follows before each answer. A failing fake gives `500` with the records unchanged. A
reconciliation pass removes NAT and filters that no record holds. Deleting a group in use gives `409`. Real mode:
quickstart §2 (g) and (h).

### Tests for User Story 7 (write first, confirm RED)

- [X] T035 [P] [US7] Write `pkg/networking/floating_ip_nat_test.go` (new), per research R23.
  - **Pure functions.**
    - The NAT parser reads `iptables -t nat -S PREROUTING` and `-S OUTPUT` output. It returns every rule of the shape
      (research R23), with its floating and fixed address and whether it is marked: `-d <F>/32 -m comment --comment
      lightstack-fip -j DNAT --to-destination <X>`, and the same without the mark, as earlier releases wrote it. A
      port-forwarding rule for the same address (with `-p` and `--dport`), and any other DNAT, are not returned.
    - The nat payload for associating F with X, given the current rules:
      - none → two `-A` lines (one per hook);
      - marked to Y → `-D` both, `-A` both;
      - unmarked to X → `-D` both, `-A` both (marked);
      - marked to X already → no payload.
    - The payload for a withdrawal is `-D` of every rule for F, marked or unmarked.
    - The filter's removal payload is `-D FORWARD <jump>` for each jump found, then `-F` and `-X` of the chain.
    - The node listing returns every rule of the shape, marked and unmarked, as floating→fixed pairs with the mark
      noted. It also returns every `LS-NSI-*` chain as its fixed IP, and the uplink's addresses.
  - **Real mode with a fake command runner.**
    - `EnsureFloatingIP`: each hook is read once; one nat restore when something differs and none when it matches. It
      runs `ip addr add` only when the address is missing, and only after the restore. It reports the fixed IPs the
      rules it deleted pointed at.
    - `WithdrawFloatingIP`: `ip addr del` first, when the address is present, then the nat restore. It reports the fixed
      IPs the deleted rules pointed at. Nothing present → no command, no error, nothing reported.
    - `RemoveIngressFilter`: one restore; a missing chain → no command and no error.
    - Every error is returned. Every command carries a deadline of one second at most. A blocking runner fails the call
      within about one second.
  - **Stub mode.** No command runs, and the listing is empty.
- [X] T036 [P] [US7] Extend the fake in `internal/neutron/fake_dataplane_test.go` with the methods T043 adds. It keeps,
      per address, its rules of the shape, marked or not, and the uplink's addresses. The NAT methods report the targets
      they replaced, and the listing returns all of it. Then write `internal/neutron/floating_ip_dataplane_test.go`
      (new; DB-backed; stub mode with the fake). Cases:
  - **Create on a port** → `201`. The fake saw X's filter, then F's NAT to X, then F's address, in that order. Create
    without a port → no call.
  - **Move** F from X to Y → `200`. The fake holds Y's filter and F's NAT to Y only, and X's filter is gone. X's filter
    stays when a second floating IP is still on X, and also when the fake holds an unmarked rule of the shape, with no
    record, pointing at X.
  - **Disassociate** → `200`. The fake removed F's address, then F's NAT, then X's filter. A description-only update →
    no call.
  - **Delete** an associated floating IP that has a port forwarding → `204`. One transaction deleted the row and the
    port forwarding. The fake removed F's address, then F's NAT, then X's filter. Delete an unassociated one → `204`, no
    call.
  - **Failure**, for each case above with the fake failing its filter, NAT or address call → `500`:
    - after a create, no row is left;
    - after a move or a disassociation, the old association is in the row;
    - after a delete, the row and its port forwarding are back, every column as it was;
    - in every case the fake was converged again from the restored record.
  - **Restoring a delete, as the foreign keys would have.** The fake's failing call first changes the database through
    the pool (another request):
    - it deletes the port → the floating IP comes back unassociated, and its port forwarding does not (its internal port
      is gone);
    - it allocates the address to a new floating IP → the deleted one stays deleted, and the new one stays.
  - **No uplink.** With the service's uplink function failing: a create answers `500` and leaves no row; an update
    answers `500` with the association back; a delete answers `500` with the floating IP back.
  - **A deleted port (research R25).** `DELETE /v2.0/ports/{id}` on a port with an associated floating IP → `204`. In
    the same transaction, the floating IP lost its port, fixed IP and router, and its status is `DOWN`. The fake removed
    its address, NAT and filter before the answer.
  - **The port's lock comes first.** PostgreSQL only; immediate transactions serialise this on SQLite anyway.
    1. A test transaction locks the port `FOR UPDATE`.
    2. `DELETE /v2.0/ports/{id}` runs in a goroutine meanwhile.
    3. The test transaction associates a second floating IP with the port and commits after a short wait.

    The deletion, unblocked, disassociates that floating IP too: no port, fixed IP or router, status `DOWN`. If the
    deletion read the floating IPs before taking the port's lock, it would miss the second one, which would keep a stale
    fixed IP and `ACTIVE`, with only `port_id` cleared by the foreign key.
  - **A failed boot's release (research R25).** `ReleaseInstancePorts`, which runs only when a boot fails
    (`internal/nova/handlers.go:1131-1135`), for a server whose port has an associated floating IP → the same. A
    server's deletion is Nova's and is tested in T041.
  - **A failed removal on deletion.** With the fake failing its NAT call, the port deletion still answers `204`, and the
    floating IP stays disassociated. The failure is logged naming the floating IP, and the next reconciliation pass
    removes the NAT.
  - **Compare-and-set.** The fake's failing call first changes the row through the pool (another request). After a
    create its description changes, and the row stays with it. After a move it is moved again, and that association
    stays.
  - **Lock deadline.** While another change holds the node lock (a fake call blocked in a goroutine), a `PUT` answers
    `500` after about one second, and the association is back.
- [X] T037 [P] [US7] Write `internal/neutron/security_group_rule_refresh_test.go` (new; DB-backed; the fake). Three
      ports: one carries `web` and has a floating IP, one carries `web` without one, one carries `db` with one. Cases:
  - **Add** a tcp/8080 rule to `web` → `201`. Only the first port's fixed IP is rebuilt, and its rules include tcp/8080.
  - **Delete** it → `204`. Rebuilt again, without it.
  - **Failure on add** → `500`. The rule row is gone, and the filter was rebuilt again without it.
  - **Failure on delete** → `500`. The rule row is back with every column as it was (id, `created_at` included), and the
    filter was rebuilt again with it.
  - **No uplink.** With the service's uplink function failing, an add answers `500` and the rule is gone; a delete
    answers `500` and the rule is back.
- [X] T038 [P] [US7] Write `internal/neutron/security_group_delete_test.go` (new; DB-backed). Cases:
  - A group a port carries → `409`, type `SecurityGroupInUse`. The group, its rules and the port's list are unchanged.
  - An id of no group → `404`. Another project's group → `404`, administrators included (register entry 5).
  - An unused group → `204`. Its rules are gone, and a rule of another group that names it as its remote group is gone
    too (**lock**: the foreign key cascades).
  - After the port that carried a group drops it, the delete → `204`.
- [X] T039 [P] [US7] Write `internal/neutron/reconcile_dataplane_test.go` (new; DB-backed; the fake with its listing).
      One pass (`reconcileNAT`) per case:
  - **Adds.** F is associated with X in the records and the fake is empty → X's filter, F's NAT, F's address, in that
    order.
  - **Removes.** The fake holds a marked NAT for G to Y, G's address and Y's filter, and no record holds G → G's
    address, then G's NAT, then Y's filter, are removed.
  - **Orphan filter.** The fake holds a filter for Z that no record associates and no rule reaches → removed.
  - **Earlier release.**
    - An unmarked rule for an associated F → replaced by the marked one.
    - An unmarked rule for an address no record holds, pointing at Z, whose filter no record associates → the rule and
      Z's filter both stay.
  - **Missing address.** F is associated, its NAT matches, and its address is missing from the fake's uplink → the
    address is added back.
  - **Failed read.** With `floating_ips` made unavailable (`dbtest.Unavailable`,
    `internal/database/dbtest/dbtest.go:258`, both backends), the fake holding a marked NAT and a filter → the pass
    removes nothing.
  - **Partial read.** The reconciler's record-read function returns one association and `complete = false`. The fake
    also holds a marked NAT with no record → nothing is removed, and the association that was read is ensured.
  - **An unassociated floating IP's leftovers.** A floating IP has a row but no association. The fake holds an unmarked
    rule of the shape for it, pointing at Z, and its address. Its rule and its address are removed, because its row
    makes them Lightstack's. Z's filter goes too, unless something else still reaches Z.
  - **No uplink.** With the service's uplink function failing, the pass makes no call and logs an error.
  - **Unchanged.** The fake already matches → no write call, reads only.
  - **Lock.** A request's blocked call holds the node lock → the pass skips that address, finishes the others, and the
    next pass converges it.
  - **Fresh read.** The record changes between the pass's listing and its step for that address → the step acts on the
    new record.
- [X] T040 [P] [US7] Write `test/contract/neutron/security_group_delete_test.go` (new; gophercloud, stub stack). Create
      a group, and a port on a network carrying it (`ports.Create` with `SecurityGroups`):
  - `groups.Delete` → `409`;
  - delete the port, then `groups.Delete` → `204`;
  - `groups.Delete` again → `404`.

- [X] T041 [P] [US7] Write `internal/nova/server_delete_floating_ips_test.go` (new; the T005 fixture, with a test type
      embedding `*neutron.Service` that records `ConvergeFloatingIPs` and can fail it, as T010's does
      `RefreshPortFilters`). Insert the `floating_ips` rows directly. Cases (research R25):
  - **A deleted server.** `DELETE /v2.0/servers/{id}` for a server whose boot port has an associated floating IP →
    `204`. In the deleting transaction the floating IP lost its port, fixed IP and router, and its status is `DOWN`.
    `ConvergeFloatingIPs` received its address before the answer. RED on `main`, where only the foreign key clears
    `port_id`.
  - **A port attached by id survives.** A floating IP on a tenant port the server had attached by id stays associated:
    the deletion detaches that port and does not delete it (`handlers.go:2160-2170`).
  - **A failed convergence.** With `ConvergeFloatingIPs` failing → still `204`; the floating IP stays disassociated, and
    the failure is logged naming it.
  - **`forceDelete` (FR-008e).** `POST /v2.1/servers/{id}/action` with `{"forceDelete": null}`, on a server with a boot
    port carrying an associated floating IP and an attached volume. The boot port is deleted, the floating IP
    disassociated and converged, and the volume released (`available`, no attachment), exactly as the delete case. The
    answer is as today. RED on `main`, where `forceDelete` deletes only the instance row.
  - **A concurrent deletion.** Two deletions of one server, `DELETE` and `forceDelete` in either order: one deletes, and
    the other answers `404` once the first has committed.
  - **Lock order.** PostgreSQL only.
    1. A test transaction holds `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`.
    2. `DELETE /v2.0/servers/{id}` runs in a goroutine meanwhile.
    3. A second test transaction locks one of the server's ports `FOR UPDATE` under a short `lock_timeout`
       (`//sqlcheck:postgres-only`). It succeeds, because the deletion takes the instance first.
    4. Both test transactions commit, and the deletion completes.

### Implementation for User Story 7

- [X] T042 [US7] In `pkg/networking/northsouth.go` (research R23). Everything goes through T013's runner, one second per
      command.
  - The mark `lightstack-fip`, the parser for rules of the shape (marked or not), and the nat and filter-removal
    payloads.
  - `EnsureFloatingIP(floatingIP, fixedIP, uplink) ([]string, error)` rewritten:
    1. enable forwarding as today;
    2. read each hook once;
    3. run one `iptables-restore --noflush --wait 1` if anything differs;
    4. add the address if missing.

    It reports the fixed IPs the rules it deleted pointed at.
  - `WithdrawFloatingIP(floatingIP, uplink) ([]string, error)` (no fixed IP): the address first, then one restore
    deleting every rule of the address's. It reports the fixed IPs they pointed at, and returns its errors.
  - `RemoveIngressFilter(fixedIP)` (no uplink): one restore. It returns its errors.
  - `NorthSouthState(uplink)`: every rule of the shape with its floating and fixed address and its mark, the `LS-NSI-*`
    fixed IPs, and the uplink's addresses.
  - Stub mode does nothing in any of them.
  - Update the callers whose signatures changed: `internal/neutron/teardown.go:67-68`, and the floating-IP handlers
    until T043 rewrites them.

  This makes T035 green.
- [X] T043 [US7] In `internal/neutron/network.go` and `internal/neutron/floatingip.go` (research R20, R23):
  - `dataplane` gains `EnsureFloatingIP`, `WithdrawFloatingIP`, `RemoveIngressFilter` and `NorthSouthState`.
  - Add `convergeFloatingIP(ctx, floatingIP string) error`. It runs under the node lock, with the uplink from
    `svc.uplink()`; that function's error is a failed change.
    1. Read the record: the fixed IP and port it is associated with, or none.
    2. If associated with X: the port's rules, then `EnsureIngressFilter(X)`, then `EnsureFloatingIP(F, X)`. Otherwise
       `WithdrawFloatingIP(F)`.
    3. For each fixed IP that step 2 reported, call `RemoveIngressFilter` unless a record still associates a floating IP
       with it, or `NorthSouthState` shows a rule of the shape, marked or not, still reaching it.

    Errors name the floating IP and the address.
  - `CreateFloatingIP` (`:160`):
    - One transaction does the checks and the insert, in this order:
      1. it locks the floating network with `SELECT … FROM networks WHERE id = $1 FOR UPDATE` and checks that it is
         external, through `requireExternalNetwork` given a querier;
      2. it reads the port, the caller's, with its fixed IP;
      3. it looks the router up as today, then locks it by id in a query of its own, `SELECT id FROM routers WHERE id =
         $1 AND external_gateway_info IS NOT NULL FOR UPDATE`;
      4. it inserts the row.
    - The address is chosen before the transaction, as today. Only bare trailing `FOR UPDATE` belongs to the shared SQL
      subset (`internal/database/doc.go:45`); `FOR SHARE` and `FOR UPDATE OF` do not. The port needs no lock: its
      project never changes, and the foreign key re-checks its existence at the insert.
    - Nothing is programmed before the commit. With a port, converge afterwards.
    - On a failure, a second transaction deletes the row if `port_id`, `fixed_ip_address` and `description` still hold
      what the insert wrote. Then converge again and answer `500`.
  - `UpdateFloatingIP` (`:409`):
    - One transaction locks, in this order: the new port, `FOR UPDATE`, with the checks of today (the caller's, its
      fixed IP); its router, found as today and then locked by id, as in the create; and only then the floating IP's
      row, `FOR UPDATE`. Port deletion takes the port before the floating IPs that name it, so this order cannot
      deadlock with it. The transaction then reads the old association, writes the new one and commits. A disassociation
      locks only the row. The best-effort withdrawal of the old association (`:443-451`) goes; convergence removes it.
    - When `port_id` was sent, converge.
    - On a failure, a second transaction locks the row and restores the old `port_id`, `fixed_ip_address`, `router_id`
      and `status` if the row still holds the new ones. Then converge again and answer `500`.
  - `DeleteFloatingIP` (`:573`):
    - One transaction locks the row (`FOR UPDATE`), reads it and its `port_forwardings` rows in full, and deletes the
      row; the port forwardings go with it. With an association, converge afterwards.
    - On a failure, a second transaction restores the row and the port forwardings. It applies the foreign keys' delete
      actions to whatever other requests deleted meanwhile:
      - no project or network → nothing is restored;
      - no port → the floating IP is restored unassociated;
      - no router → it is restored without one;
      - a port forwarding whose internal port is gone stays deleted;
      - the address allocated again → nothing is restored.

      Then converge again and answer `500`.
  - In the first transaction of all three, a foreign-key violation answers `404`, never `500`. In an undo transaction
    the request already answers `500`, and rows deleted meanwhile are handled by the foreign keys' delete actions, as
    above.

  This makes T036 green, except its deletion cases (T046).
- [X] T044 [US7] In `internal/neutron/ports.go` (research R21), `CreateSecurityGroupRule` (`:1179`) and
      `DeleteSecurityGroupRule` (`:1421`). Their LS-SG step stays where it is, and so does the ownership check before
      it.
  - An add inserts the rule in a transaction that first reads the group, and the remote group when named, both the
    caller's (`404` otherwise). No row lock: a group's project never changes, and the foreign keys re-check that both
    still exist at the insert.
  - A delete reads the whole rule row `FOR UPDATE`, scoped to the caller's project, and deletes it in one transaction.
  - After the commit, read the fixed IP and port of every floating IP whose port carries the group (`floating_ips`
    joined to `port_security_groups`). Call `ensureIngressFilter` for each, with the uplink from `svc.uplink()`; its
    error is a failed change.
  - On a failure: a second transaction deletes the added rule if it is still there, or re-inserts the deleted rule with
    every column if its group exists. Then rebuild the same addresses again, log the first error and answer `500`.
  - In the first transaction, a foreign-key violation answers `404`, never `500`.
  - Delete `refreshIngressFiltersForSG` (`floatingip.go:691`).

  This makes T037 green.
- [X] T045 [US7] In `internal/neutron/ports.go`, `DeleteSecurityGroup` (`:1059`, research R22):
  - One transaction: `SELECT … FROM security_groups WHERE id = $1 AND project_id = $2 FOR UPDATE`. No row → `404`
    (`NewNotFoundError("security group")`). Any `port_security_groups` row → `409` with
    `common.NewResourceInUseError("security group", "delete security group", …)`, naming the group. Otherwise delete it
    and commit.
  - Move the LS-SG chain removal after the commit. Its failure is still only logged, a recorded departure that follow-up
    2 lists at its new place (T056).
  - In `internal/common/neutron_errors.go`, `neutronInUseTypes` gains `"security group": "SecurityGroupInUse"`.

  This makes T038 and T040 green.
- [X] T046 [US7] In `internal/neutron/ports.go`, `internal/neutron/floatingip.go` and `internal/nova/handlers.go`
      (`NeutronService`), the floating IPs of a deleted port (research R25):
  - Add `DisassociatePortFloatingIPs(ctx, tx database.Tx, portIDs []string) ([]string, error)`, a package-level helper
    on the caller's transaction (R2's pattern). It reads the floating IPs whose `port_id = ANY($1)` and clears their
    `port_id`, `fixed_ip_address` and `router_id`. It also sets `status` to `DOWN`: upstream's port deletion clears the
    same three columns, and its L3 agent reports `DOWN` afterwards. It returns their addresses.
  - `DeletePort` (`:516`): its single `DELETE` (`:551`) becomes a `database.WithTx`. That transaction locks the port
    `FOR UPDATE` first, then calls the helper, then deletes the port.
  - `ReleaseInstancePorts` (`:1860`), which a failed boot calls: one transaction locks the instance row, then the
    server's ports in id order, `FOR UPDATE`, as membership changes do. Then it calls the helper and deletes the ports
    (`:1891`). No instance row means the server was deleted meanwhile; the release carries on with whatever ports
    remain.
  - Both, after the commit: `convergeFloatingIP` for each address. A failure is logged naming the floating IP, and the
    deletion stands, because the port's own teardown has already run without an undo. Reconciliation removes what is
    left within one pass. This is a recorded departure (follow-up 2).
  - Add `ConvergeFloatingIPs(ctx, floatingIPs []string) error` to the Neutron service in
    `internal/neutron/floatingip.go`, beside `convergeFloatingIP`, joining its errors. Add it to Nova's `NeutronService`
    (`internal/nova/handlers.go:61`). `fakeNeutron` (`internal/nova/boot_failure_test.go:46`) records its calls and
    returns a configurable error.
  - `teardownPortHost` and the DHCP refresh are unchanged.

  This makes T036's deletion cases green.
- [X] T047 [US7] In `internal/nova/handlers.go`, `DeleteServer` (`:2021`) and the `forceDelete` action
      (`ForceDeleteInstanceAction`, `:3720`), research R25 and FR-008e:
  - Extract `DeleteServer`'s deletion into one function, called by both: console tokens, libvirt domain, the transaction
    below, the convergence and the file cleanup after it. `ForceDeleteInstanceAction` stops deleting only the instance
    row, and keeps its own answer.
  - The transaction locks the instance row first, `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`. No row means a
    concurrent deletion won: `404`, as the exactly-once check answers today (`:2201`). Then it locks the server's ports
    in id order, `FOR UPDATE`, before it touches volumes or ports. Today it changes volumes and ports before the
    instance row, the reverse of T018.
  - Before the boot ports' `DELETE` (`:2175`), it calls `neutron.DisassociatePortFloatingIPs` on its transaction for
    those ports. A tenant port it only detaches (`:2166`) keeps its floating IP, because the port still exists.
  - After the commit, and before the answer, it calls `svc.neutronSvc.ConvergeFloatingIPs` for the returned addresses. A
    failure is logged naming the floating IP, and the deletion still answers `204`.

  This makes T041 green.
- [X] T048 [US7] In `internal/neutron/reconciler.go`, `reconcileNAT`'s floating-IP part converges both ways (research
      R24):
  1. Take the uplink from `svc.uplink()`. On its error, log an error and skip the floating-IP part.
  2. `dataplane.NorthSouthState(uplink)` first.
  3. Then every floating IP's row, associated or not, with its association if it has one. The read goes through a
     function field on the reconciler that defaults to the query. It returns them and whether the read was complete, as
     `readNetworkIDs` does.
  4. Then one address at a time:
     - `convergeFloatingIP` for every floating IP that has a row or a marked rule. An unassociated one is withdrawn, its
       unmarked rules included. One with only a marked rule and no row is converged only after a complete read. One with
       only unmarked rules and no row is left alone.
     - `RemoveIngressFilter`, under the node lock, for each listed fixed IP that no association names and no listed rule
       of the shape reaches, marked or not, after a complete read only.
  - A lock timeout skips the address.
  - Converge only where the listing differs from the rows: a rule missing, extra or pointing elsewhere, or the address
    absent or present when it should not be. An unassociated floating IP with no rule and no address has nothing to do.
    The filter's own `iptables -S` comparison stays.
  - Egress SNAT is unchanged.

  This makes T039 green.

**Checkpoint**: every story is independently green.

---

## Phase 10: Polish & Cross-Cutting Concerns

**Purpose**: the remaining documentation (FR-020, FR-021, R14), the gates, a mutation check, the host runbook, host
validation (FR-023), the follow-up tickets, and the constitution's renumbering at the rebase.

- [X] T049 [P] FR-020: add a section "Security-group membership reaches the ports" to `docs/UPGRADING.md`, with a
      before/after table:
  - last-group removal: `400` → `202`;
  - add and remove accept an id;
  - add and remove change the ports, and so the firewall;
  - add and remove on a server with no port: `202` → `404`;
  - a server reports its ports' groups: no invented `default`; servers from v0.6.1 and earlier report none; one entry
    per port;
  - a new interface's port gets `default`;
  - `PUT /v2.0/ports/{id}` applies `security_groups`;
  - `POST /v2.0/ports` with `[]` or `null` gives no group;
  - `GET /v2.0/security-groups` honours `id` and `name`;
  - deleting a group a port carries: `204` → `409`; deleting a missing group: `204` → `404`;
  - deleting a port or a server disassociates its floating IPs completely: no fixed IP or router, status `DOWN`, where
    only the port used to be cleared;
  - `forceDelete` deletes a server as a delete does: its boot ports go and its volumes are released, where they used to
    be left behind;
  - a membership, rule or floating-IP change whose firewall or NAT the node cannot apply, or that takes longer than one
    second: success → `500`, and nothing changes.

  Add a paragraph on the dataplane. A floating-IP filter is replaced atomically, before its NAT, and every command is
  bounded at one second. The NAT is marked as Lightstack's, and reconciliation now removes floating-IP state no record
  holds on every node. Rules from earlier releases are replaced for floating IPs that still exist; those of floating IPs
  deleted before the upgrade stay. Give the command that lists them, `iptables -t nat -S PREROUTING | grep -- '-j DNAT' | grep -v lightstack-fip` (and the same for `OUTPUT`), and say they are the operator's to remove (research R17).
- [X] T050 [P] R14: in `docs/EBPF_STATUS.md`, say that a membership change does not re-program a port's eBPF rule set,
      and change the `internal/neutron/network.go:40` references to `:41`.
  - Done with `:91`, where the call now is: the node lock and the dataplane interface are declared before `NewService`.
    The doc's mention of `refreshIngressFiltersForSG`, which T044 deleted, now names `rebuildGroupFilters`.
- [X] T051 Re-run T001's gates and append the results to `docs/specs/033-sg-port-membership/baseline-gates.md`, compared
      with the baseline (SC-007, SC-008):
  - `make fmt` clean, `make lint`, `go vet ./...`;
  - `go test ./...` on PostgreSQL and on SQLite;
  - `make test-contract` against a running stub Lightstack, once on each backend;
  - `./test/security_groups_test.sh` and `./test/neutron_patch_test.sh`.

  If `make lint` reports paths from a removed worktree, run `go tool golangci-lint cache clean` and repeat.
- [X] T052 Mutation-check the new tests and record the outcome in `docs/specs/033-sg-port-membership/baseline-gates.md`.
      Revert each fix **off the worktree** (a `git clone --shared` scratch clone or `go test -overlay`, never by editing
      tracked files in place) and confirm its test fails:
  - the report back to `server_security_groups` (T006);
  - `ChainMatches` always true, and the payload's `-F` dropped (T008);
  - `EnsureIngressFilter` restoring regardless of `ChainMatches` (T008);
  - the runner's one-second context removed (T008, T035);
  - the node lock removed, and its deadline removed (T009);
  - the undo removed, and its compare-and-set removed (T010 for add and remove; T030 for `PUT`; T036 for floating IPs;
    T037 for rules);
  - the `404` for a server with no port removed (T010);
  - add writing `server_security_groups` only (T010, the earlier-release case);
  - the foreign-key violation passed through as an error (T003);
  - copy-from ignored (T025, T026);
  - the `net_id` port without `default` (T022);
  - the listing's filters ignored (T031);
  - `null` treated as absent (T030);
  - the last-group refusal restored (T019);
  - the address added before the NAT, the NAT before the filter, and the filter removed before the NAT (T035, T036,
    T039);
  - the mark dropped from the NAT (T035, T039);
  - create programming before its insert (T036, failure on create);
  - the rule rebuild skipped (T037);
  - the in-use check removed (T038, T040);
  - the reconciler removing nothing, removing after a failed or partial read, and removing unmarked rules without a
    record (T039);
  - a filter removed while an unmarked rule of the shape still reaches it (T036 move, T039);
  - the floating address left out of the pass's comparison (T039, missing address);
  - a missing uplink skipped instead of failing the change (T009, T036, T037, T039);
  - a failed delete's restore leaving out the port forwardings, or not applying a deleted port's `SET NULL` (T036);
  - a port or server deletion leaving its floating IP associated, or not converging it (T036, deletion cases; T041);
  - a port deletion reading the floating IPs before it locks the port, and a server deletion locking ports before the
    instance (T036, T041);
  - `forceDelete` deleting only the instance row again (T041);
  - the pass reading only associated floating IPs (T039, an unassociated floating IP's leftovers).
- [ ] T053 **Superseded by T165 (round 2 retires the floating-IP filter this run tests).** Generate the host runbook
      `docs/specs/033-sg-port-membership/HOST_TESTING.md` with `/cl.hosttest docs/specs/033-sg-port-membership/`. It
      covers prerequisites, setup, the FR-023 steps (a)–(h) as `quickstart.md` §2 and research R18 lay them out (one
      connection attempt per second in (d)), and teardown, all zsh-paste-safe. The prerequisites block checks,
      read-only, that iptables' `comment` match works: `iptables -m comment --help` succeeds, and with the legacy
      backend (`iptables -V`) `modinfo xt_comment` finds the module. Without it, every association answers `500`.
- [ ] T054 **Superseded by T166.** Run host validation (a)–(h) with T053's runbook on one real-mode node (libvirt, root,
      a routed floating IP, iptables' `comment` match, a second guest for (g) and (h)), and record each step's outcome
      in `docs/specs/033-sg-port-membership/HOST_TESTING.md`.
- [ ] T055 **Moved to T168.** FR-021: once T054 step (f) has passed, record instance *Edit Security Groups* as verified
      in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, with the date. Verified means it pre-selects the
      instance's groups and changes its ports.
- [X] T056 [P] Draft four follow-up tickets in `docs/specs/033-sg-port-membership/follow-ups.md` (new), for the user to
      file; do not file them (research R17).
  1. **The security-group listing's other filters** (`description`, `project_id`/`tenant_id`, `tags`, `shared`,
     `stateful`, `revision_number`). They are ignored today, a silent divergence. Honour those the data model can
     answer, and register or refuse the rest. Refusing all of them would break Horizon, which sends `tenant_id`
     (research R13).
  2. **Request paths that log a failed node-local step and answer success.** These are the departures named in the
     constitution's v1.6.0 migration plan, each with its location at `d81f081`:
     - `CreateAutoAllocatedTopology` (`auto_allocated_topology.go:119`);
     - network teardown (`network.go:1167`, `:1185`);
     - `CreateSubnet` (`network.go:1515`), and the subnet delete and gateway-change paths (`:1884`, `:1890`, `:1943`,
       `:1951`);
     - `DeleteRouter` (`router.go:445`), `AddRouterInterface` (`:541`, `:558`), `RemoveRouterInterface` (`:626`), and
       gateway set and clear (`:692`, `:799`);
     - port forwarding create, update and delete (`port_forwarding.go:220`, `:361`, `:383`, `:447`);
     - `CreatePort`'s eBPF and FDB steps (`ports.go:235`, `:276`);
     - `DeletePort` and `ReleaseInstancePorts` teardown (`ports.go:546`, `:1887`, `:1899`; `teardownPortHost` `:1919`,
       `:1928`, `:1934`, `:1938`). This feature disassociates their floating IPs in the deleting transaction and removes
       the NAT after it (T046). A failed removal is logged and left to reconciliation, because the rest of the teardown
       has no undo;
     - `AllocatePortForInstance`'s FDB and DHCP steps (`ports.go:1726`, `:1735`);
     - the LS-SG chain writes in `DeleteSecurityGroup` (`:1066`, which this feature moves after the commit, T045),
       `CreateSecurityGroupRule` (`:1263`) and `DeleteSecurityGroupRule` (`:1462`). Nothing jumps to these chains, so an
       undone rule add can leave its rule in one, without effect. Removing the chains, or wiring them, is this ticket's
       decision;
     - `DeleteFloatingIP` (`floatingip.go:573`), which takes the floating IP's port forwardings with it through the
       foreign key (migration 050) and leaves their NAT.

     Also network creation's undo (`network.go:631-637`), which deletes the row unconditionally rather than
     compare-and-set. Each path is brought in line with Article II as amended, or the constitution is amended for it.
  3. **Deleting `default` as a non-administrator.** Upstream refuses it with `409` (`SecurityGroupCannotRemoveDefault`)
     even when no port carries it. `test/security_groups_test.sh` Test 11 already expects a refusal.
  4. **Floating IPs beyond their dataplane.** Two creates can race for one address. A port whose network has no router
     stores an association (upstream: `404`). Several floating IPs can reach one fixed IP (upstream refuses within one
     external network).
- [X] T057 Rebase onto `main` once it carries the other constitution amendment numbered v1.5.0, which lands first, and
      before review. Renumber this amendment to v1.6.0. Name no other branch or pull request in anything committed.
      *Noted 2026-11-23 for the amendment: the constitution's technology list names `github.com/coreos/go-iptables`,
      which round 2 no longer imports (`go mod tidy` dropped it); the line becomes iptables through `iptables-restore`.*
  - **Constitution.** Resolve `.specify/memory/constitution.md` so both amendments stand:
    - The Sync Impact Report describes this amendment only: "Version change: 1.5.0 → 1.6.0".
    - Every other hit of `grep -n '1\.5\.0' .specify/memory/constitution.md` that belongs to this amendment becomes
      1.6.0: "added in", "scope stated in", the log heading.
    - The Forbidden Patterns "No async workers" bullet names both exemptions: the reconciliation loops of this
      amendment, and the other amendment's re-establishment of a dependency in the background (Article III).
    - The log lists v1.6.0 above v1.5.0.
    - The footer reads `**Version**: 1.6.0` and the rebase date.
  - **`CLAUDE.md`.** Renumber only this feature's own citation, the database paragraph's "(constitution v1.5.0, Article
    II)". The other amendment's "Constitution IV, v1.5.0" citations stay as they are. Keep both features' Active
    Technologies lines.
  - **The spec directory.** Every hit of `grep -rn 'v1\.5\.0' docs/specs/033-sg-port-membership/` becomes v1.6.0, the
    checklist notes included.
  - **The Constitution Check.** Re-run `plan.md`'s table against the merged Articles III and IV, and record the result.
    This analysis found no conflict: the design never falls back to stub at runtime (a missing uplink is a failed
    change, T015, T043), and the background-reconnect exception concerns another loop.
  - **The ticket.** Its body and checklist cite v1.5.0 too.
  - **Migrations (flagged 2026-11-20).** `main` is expected to have squashed its migrations into a baseline by the time
    of the rebase, with new schema going into the next release's migration. Migrations 076 and 077 cannot stay numbered
    files then. Move their schema to where `main`'s rules put new schema, then decide what else goes by where it landed:
    - **In the baseline**, `security_groups` is created without the unique name and no table needs rebuilding. Delete
      the `-- lightstack:foreign-keys-off` marker and everything that serves it: `internal/database/migrate_notx.go`,
      `migrate_notx_test.go`, the driver hook in `internal/database/migrate.go`, and the marker's section of
      `migrations/README.md` (T061, T067). Every database then starts with 076's columns, so the code for rows from
      before 076 goes too: subnets without stored pools (`internal/neutron/allocation.go`, the gateway change in
      `network.go`, the "from before migration 076" case in `subnet_allocation_pools_test.go`), and R51's zone
      assignment for networks without one, if a later phase has written it.
    - **On top of a baseline that still has the unique name**, the SQLite rebuild stays. The marker must then be the
      first line of the file the migrator actually runs, so check it against how `main` assembles that file.
    - **Either way**, `internal/database/migration_076_test.go` (T060) cannot run as written: it migrates to versions
      75, 76 and 77. Its checks of the steps themselves go with the numbered files: 076's down, and 077's "every
      referencing row survives", which exists for the rebuild. Its checks of what the schema means move to a test that
      migrates to the newest version: pools deleted with their subnet, `binding_host_id`, one network per zone, a
      group's revision and statefulness defaults, the seeded `default`'s member rules, repeatable names with one
      `default` per project. Keep `binding_host_id` and `ct_zone` above all: nothing else reads them before Phase 18.
      Reword every comment that cites a migration number too: `grep -rn 'migration 07[67]' internal/`.
  - **Check.** Run both `grep` commands afterwards. Outside the constitution, expect no hit in the spec directory, and
    in `CLAUDE.md` only the other amendment's two. Inside it, expect only the other amendment's.
  - *Done 2026-11-27, onto `main` at the CLI conformance environment (SPEC-031).* Constitution: v1.6.0, both
    amendments standing (done at the first rebase, 2026-11-26; `main`'s file has not changed since). `CLAUDE.md`: the
    database paragraph cites v1.6.0; the other amendment's two v1.5.0 citations stay; the table count is 76, counted
    on a scratch SQLite file at 085 without `schema_migrations` (73 at 075, 75 at `main`'s 083). Spec directory: every
    citation of this amendment says v1.6.0. Eleven hits keep v1.5.0 on purpose, because they name the other
    amendment or record the collision as it was decided: this task's own text and the ordering note under the phase
    list, the Clarifications answer (spec.md), checklist iterations 5 and 6, plan.md's settled-collision line and R42's
    heading. Constitution Check: re-run, one Article III conflict, fixed (T312; plan.md, "Re-check at the rebase").
    Migrations: `main` has not squashed; it took 076–083, so this feature's 076 and 077 became **084** and **085**,
    numbered files on top of it. 084 also lets a port have no project again, which `main`'s 079 had forbidden: a
    router's gateway port has none, as upstream's. The SQLite rebuild stays; the marker is the first line of
    `085_security_group_names.sqlite.up.sql`, which the per-backend view runs in place of the plain file, and
    `TestMigration085`'s "every referencing row survives" passes on SQLite. `migration_076_test.go` is
    `migration_084_test.go`, migrating to 83, 84 and 85; every citation in `internal/` and the spec directory says
    084/085, except this task's text and T232's count. `main`'s subnet `enable_dhcp` update follows the DHCP unit:
    flipping it moves the network's DHCP port onto or off the subnet, is applied on the network node before the
    answer, and is undone, compare-and-set, when it cannot be (`TestDHCPFollowsASubnetsDHCPFlag`, "a DHCP flag change
    is undone"). The ticket's citations go with T311.

---

# Round 2 (2026-11-20): the follow-ups and the real-mode network layout

**Input**: spec Sessions 2026-11-19 and 2026-11-20, and both round-2 analyses (US8–US12; FR-019a–b, FR-024–FR-058);
research R26–R53; data-model.md round 2; contracts `network-api-round2.md`, `node-apply.md`,
`router-namespace-dataplane.md`, `port-firewall.md`; quickstart §3–5. Constitution v1.6.0 refined 2026-11-20 (R42) is
**already in place** (done at the plan gate), so no task carries it; register entry 6 was added there and withdrawn by
the third analysis (R53). Locations are hints from `dcda565`.

**Organization**: US8–US11 are the spec's stories for findings 1, 3 and 4. FR-033–FR-056's router API and layout work
(finding 2, the router namespace, and the answers the round-2 analysis turned into requirements) has no user story in
the spec; its phases (16 and 18–24) carry no story label and name their requirements instead. Order: setup and
foundation (migration, seams, errors, node name), then the API (US8, US9, US11, the router API, US10), then the layout
(node, units, firewall, deletions, reconciliation and teardown, the node), then polish.

**Terms (round 2)**:
- *unit*: a router, a network's DHCP, or a node's set of bound ports, rendered from the records and converged, or
  converged to *absent* before a deletion (R28, R29, R32, R34).
- *node call*: the authenticated, one-second, stateless call naming a unit (R27; `contracts/node-apply.md`).
- *the fakes*: in-memory `routerDataplane`, `dhcpDataplane`, `portFirewall` and `nodeCaller` in `internal/neutron`'s
  tests, holding state and able to record, block or fail calls (R41, T065).

---

## Phase 11: Round 2 setup

- [X] T058 Record the round-2 baseline in `docs/specs/033-sg-port-membership/baseline-gates.md`, section "Round 2
      baseline", at `dcda565` plus the uncommitted spec and constitution changes: build, lint, unit tests on PostgreSQL
      and SQLite (per package, `-count=1 -p 1`), contract tests on both backends, `helm unittest charts/lightstack`, and
      `./test/security_groups_test.sh`. Fix nothing; record failures that predate round 2.
- [X] T059 Get round 2's test strategy approved before any round-2 test is written (constitution, Test-First
      Development, step 2): review research R43 and every round-2 test task below with the user, and record the
      approval, date and any change in `baseline-gates.md`. No later round-2 task starts before it.

---

## Phase 12: Round 2 foundational (blocks every round-2 story)

### Tests (write first, confirm RED)

- [X] T060 [P] Migration 084 test in `internal/database/migration_084_test.go`: on both backends, after migrating up,
      `subnet_allocation_pools` exists with `id`, `subnet_id`, `first_ip`, `last_ip` and its cascade from `subnets`;
      `subnets.pools_stored`, `ports.binding_host_id`, `networks.ct_zone`, `security_groups.revision_number` (default 0)
      and `security_groups.stateful` (default 1) exist; `networks.ct_zone` has a unique index; the seeded project's
      `default` has the two ingress rules whose `remote_group_id` is its own id. Down removes all of it, and the two
      rules. `TestSchemaParity` and `TestMigrationsRoundTrip` must also pass with 084. Migration 085 too: on both
      backends `security_groups` accepts two groups with one name in a project, a second `default` in a project is
      refused by the partial unique index, and on SQLite every `port_security_groups` and `security_group_rules` row
      survives the rebuild.
- [X] T061 [P] `internal/database/migrate_notx_test.go` (new), R53: a SQLite migration file whose first line is `--
      lightstack:foreign-keys-off` runs with foreign keys off, turned off outside the transaction it then runs in, and
      foreign keys are on again afterwards; a file that leaves a foreign-key violation fails with `foreign_key_check`'s
      rows named; PostgreSQL ignores the marker; files without it behave as today (lock).
- [X] T062 [P] Explicit Neutron error type in `internal/common/neutron_errors_test.go`: an error carrying an explicit
      Neutron type (`SecurityGroupCannotRemoveDefault`) renders that type even with resource "security group" and code
      409, where today the resource map wins (`SecurityGroupInUse`). Errors without one render as today (lock).
- [X] T063 [P] Node name in `internal/compute/node_registry_test.go` (new): the registry registers and heartbeats under
      `common.NodeName()` (`internal/common/nodename.go:9`, which already returns `LIGHTSTACK_NODE_NAME` or the
      hostname), not `os.Hostname()` (R26).
- [X] T064 [P] `neutron.network_node` in `internal/common/config_test.go`: parsed from YAML and from its environment
      override; with VXLAN on and the key empty, start-up validation fails naming the key; with VXLAN off the node is
      its own network node whatever the key says.

### Implementation

- [X] T065 **Moved to the start of Phase 18 (2026-11-20, during implementation).** *Done in Phase 18 for what the node
      call uses: the unit request and its wire form, the unit locks, and the `nodeCaller` seam with its real
      implementation (`internal/neutron/units.go`, `node_call.go`). The three dataplane seams come with their units,
      `routerDataplane` with T117, `dhcpDataplane` with T123 and `portFirewall` with T132, and each fake with the first
      test that uses it, the fake node caller with T114: their state types are the renderers', and an unused test helper
      fails lint.* Nothing in Phases 13–17a uses these seams, and their state types come from the renderers of Phases
      19–21. Declared here, they would be guesses, and the API increment the user reviews at the Phase 17a checkpoint
      would carry them unused. Seams and fakes (R41): in `internal/neutron/units.go` (new) declare `routerDataplane`,
      `dhcpDataplane`, `portFirewall` and `nodeCaller`, and the unit lock type (per key, one-slot channel, acquired
      within the node call's shared 900 ms deadline, R27); add the fields to `Service` in `internal/neutron/network.go`,
      stub implementations that apply nothing, and in `internal/neutron/fake_units_test.go` (new) the four stateful
      fakes with `failWith`, `blockFirst`, `writes` and `seed*` helpers in the style of `fake_dataplane_test.go`. The
      fake `nodeCaller` routes a call to another in-process `Service` by node name, so two-node tests run in one
      process.
- [X] T066 Migration 084 (makes T060 green): `migrations/084_network_layout.postgres.up.sql`,
      `084_network_layout.postgres.down.sql`, `084_network_layout.sqlite.up.sql` and
      `084_network_layout.sqlite.down.sql`, per `migrations/README.md`'s naming: the new table (a pair, since it creates
      a table); `ALTER TABLE … ADD COLUMN` for `subnets.pools_stored` (integer), `ports.binding_host_id` (text) and
      `networks.ct_zone` (integer); a unique index on `networks(ct_zone)`; `security_groups.revision_number` (integer,
      default 0) and `security_groups.stateful` (integer, default 1); the seeded `default`'s two member rules (`002`'s
      ids). No table that others reference is rebuilt on SQLite. And migration 085
      (`085_security_group_names.postgres.up.sql`/`.down.sql`, `085_security_group_names.sqlite.up.sql`/`.down.sql`, the
      SQLite up file with the `-- lightstack:foreign-keys-off` marker): drop the unique name constraint (PostgreSQL by
      its looked-up name; SQLite by rebuilding the table), and create the partial unique index for `default` (R53).
- [X] T067 The migrator change (makes T061 green): `internal/database`'s migration runner honours the `--
      lightstack:foreign-keys-off` marker on SQLite (foreign keys off, run, `PRAGMA foreign_key_check`, foreign keys
      on); document it in `migrations/README.md`, replacing "comes with a change to the migrator" with the rule and its
      check.
- [X] T068 [P] Explicit Neutron type (makes T062 green) in `internal/common/errors.go` (a field and a constructor) and
      `internal/common/neutron_errors.go` (`neutronType` honours it first).
- [X] T069 [P] Node name (makes T063 green) in `internal/compute/node_registry.go`: register and heartbeat under
      `common.NodeName()`.
- [X] T070 [P] `neutron.network_node` (makes T064 green) in `internal/common/config.go` (`NeutronConfig`, the env
      override, validation called from `cmd/lightstack/main.go`'s `checkDatabaseConfig` neighbourhood),
      `config/lightstack.yaml` (commented example), and the chart: `charts/lightstack/values.yaml`,
      `values.schema.json`, `templates/_helpers.tpl` neutron block, plus a case in
      `charts/lightstack/tests/config_render_test.yaml`.

**Checkpoint**: migration 084 applies on both backends; the seams and fakes compile; nothing user-visible changed.

---

## Phase 13: User Story 8 — A client lists groups by any attribute (Priority: P3)

**Goal**: every attribute filter applies as upstream's (FR-019a, FR-019b; R38).
**Independent Test**: contract — each filter against a project whose groups differ in it.

### Tests for User Story 8 (write first, confirm RED)

- [X] T071 [P] [US8] Extend `internal/neutron/security_group_list_filter_test.go`: `description` (repeated values OR'd),
      `project_id`/`tenant_id` (own → all; another project as a member → empty; another project as an administrator →
      that project's groups), `shared=false`/`true`, `stateful=true`/`false` against stored values, `revision_number`
      against stored values, `tags`, `tags-any` (empty), `not-tags`, `not-tags-any` (all), `shared=maybe` → `400`, a
      repeated `name`. And every group in list, show, create and update bodies carries its `stateful` and
      `revision_number`, `shared: false`, `tags: []`.
- [X] T072 [P] [US8] Contract `test/contract/neutron/security_group_filters_test.go` (new): the same through
      gophercloud's `groups.List` with `ListOpts` and raw queries for the parameters gophercloud lacks;
      `revision_number` and `stateful` filters against groups created stateless and updated.

- [X] T073 [P] [US8] `internal/neutron/security_group_revision_test.go` (new), FR-050: a created group (with its
      starting rules) has `revision_number` 0; each update of the group adds one; each rule added or deleted adds one; a
      rule add undone on a failed apply leaves the revision where it was (compare-and-set, with the rule);
      `?revision_number=` lists by the stored value; `PUT` and `DELETE` with `If-Match: revision_number=<n>` and a
      different revision answer `412 RevisionNumberConstraintFailed`, nothing changed, and with the current one succeed;
      `GET /v2.0/extensions` lists `stateful-security-group`.
- [X] T074 [P] [US8] `internal/neutron/security_group_stateless_test.go` (new), FR-051: `POST` with `stateful: false`
      creates a stateless group, reported so; changing `stateful` while a port carries the group → `409
      SecurityGroupInUse`, unchanged; while none does → `200`; a port create or update that would carry stateful and
      stateless groups → `409 SecurityGroupConflict`, nothing changed.
- [X] T075 [P] [US8] `internal/nova/security_group_stateless_test.go` (new), FR-051: `addSecurityGroup` of a stateless
      group to a server whose ports carry stateful ones → `409`, nothing changed; `addSecurityGroup` that leaves every
      port single-kind (a server whose ports already differ, each staying single-kind) succeeds; interface attach of a
      port of the other kind succeeds, the port keeping its groups (FR-013); a boot whose requested groups mix both
      kinds ends `ERROR` with a fault, as upstream's failed port creation does.

### Implementation for User Story 8

- [X] T076 [US8] In `internal/neutron/ports.go` `securityGroupListFilter`: each parameter with `= ANY($n)` over its
      values, `project_id` and `tenant_id` merged (an administrator's replace the own-project scope with the named
      projects; anyone else's narrow within it), `stateful` and `revision_number` on their columns, the constant answers
      for `shared` and the tag filters, strict booleans; and the four fields in `ListSecurityGroups`,
      `GetSecurityGroup`, `CreateSecurityGroup` and `UpdateSecurityGroup` responses.

- [X] T077 [US8] `internal/neutron/ports.go` and `internal/neutron/security_group_default.go`: `revision_number` bumped
      in the transactions of `UpdateSecurityGroup`, `CreateSecurityGroupRule`, `DeleteSecurityGroupRule` and their
      undos; `stateful` accepted on create, refused on update while in use; the mixing check inside the shared
      membership helpers (`SetPortSecurityGroups`, `AddPortSecurityGroup`), so the port API, Nova's add, attach and the
      boot's allocation all refuse it; `internal/nova/advanced_actions.go` (`addSecurityGroup`) maps it to `409` (the
      check is per port; interface attach has none), and the boot's allocation failure in `internal/nova/handlers.go` to
      the fault; undos put `revision_number` back compare-and-set; `UpdateSecurityGroup` and `DeleteSecurityGroup`
      honour `If-Match`; `ListExtensions` (`internal/neutron/network.go`) adds `stateful-security-group`.

**Checkpoint**: US8 green on both backends.

---

## Phase 14: User Story 9 — `default` stays the project's default (Priority: P2)

**Goal**: upstream's refusals for `default`, duplicate names as upstream with `409 NoUniqueMatch` on an ambiguous name,
upstream's starting rules (FR-024–FR-026, FR-043; R39, R53).
**Independent Test**: delete, rename and create against `default` as a tenant and as an administrator;
`test/security_groups_test.sh` Test 11.

### Tests for User Story 9 (write first, confirm RED)

- [X] T078 [P] [US9] `internal/neutron/security_group_default_test.go` (new): non-administrator deletes unused `default`
      → `409 SecurityGroupCannotRemoveDefault`, group and rules unchanged; administrator → `204`, and the next
      `EnsureDefaultSecurityGroup` makes a new one with four rules; in-use `default` → `409 SecurityGroupInUse` for
      both; `PUT` `default` with `name: "web"` → `409 SecurityGroupCannotUpdateDefault`, with `name: "Default"` → `409
      SecurityGroupDefaultAlreadyExists`, with only `description` → `200`; `POST` `default`/`DEFAULT` → `409
      SecurityGroupDefaultAlreadyExists`; `PUT` of another group with `name: "default"` → `409
      SecurityGroupDefaultAlreadyExists`; a duplicate name on create and on rename succeeds, as upstream (FR-026); `GET
      /v2.0/security-groups/{name}` with a name two groups share answers `404`, and `?name=` lists both.
- [X] T079 [P] [US9] `internal/neutron/security_group_starting_rules_test.go` (new): a created group has exactly two
      egress rules (IPv4, IPv6, no remote); `EnsureDefaultSecurityGroup` creates four (plus ingress IPv4 and IPv6 with
      `remote_group_id` = the group); no rule has neither remote while being ingress (cloud-storage#528 lock).
- [X] T080 [P] [US9] Contract `test/contract/neutron/security_group_default_test.go` (new): the refusals with their
      Neutron types through gophercloud, as a member and as an administrator.
- [X] T081 [P] [US9] In `test/security_groups_test.sh`, rename the `GROUPS` variable to `SG_LIST` (`:40`, `:43`,
      `:209`), so Test 1 finds `default` and Test 11 runs; Test 11 expects the `409`. Run it against `main`'s binary:
      Test 11 fails red (it deletes `default` with `204`).
  - *Done differently (2026-11-20):* the script's token is the seeded administrator's, and FR-024 lets an administrator
    delete an unused `default` (`204`, as upstream). As written, Test 11 would have failed, and deleted the seeded
    `default`. Test 11 now creates a member user on the same project, deletes `default` with that user's token, expects
    `409`, and removes the user on exit. Against `main`'s binary it fails red with `204`.
- [X] T082 [P] [US9] `internal/nova/security_group_ambiguous_name_test.go` (new), FR-026: with two groups named `web` in
      the project, server create naming `web`, `addSecurityGroup` and `removeSecurityGroup` by name answer `409` with
      upstream Nova's `NoUniqueMatch` message and change nothing; by either group's id they succeed.

### Implementation for User Story 9

- [X] T083 [US9] In `internal/neutron/ports.go`: `DeleteSecurityGroup` checks in-use first, then `default` and not
      administrator (`common.HasRole`); `CreateSecurityGroup` and `UpdateSecurityGroup` check the name against `default`
      case-insensitively, `UpdateSecurityGroup` refuses a `name` on `default`; neither refuses a duplicate name any
      more; `CreateSecurityGroup` inserts the two egress rules in its transaction (it gains one). Use T068's explicit
      types.
- [X] T084 [US9] In `internal/neutron/security_group_default.go`: `EnsureDefaultSecurityGroup` seeds four rules; update
      its comment (the member rules are not open to every address).
- [X] T085 [US9] `internal/neutron/ports.go` (`ResolveSecurityGroup`: an ambiguity error when a name matches several
      groups; `GetSecurityGroup`: `404` for an ambiguous name), `internal/neutron/security_group_default.go`
      (`EnsureDefaultSecurityGroup`'s conflict target becomes `(project_id) WHERE name = 'default'`; *done with T066*,
      because migration 085 removes the constraint the old target named, and every caller of
      `EnsureDefaultSecurityGroup` failed until it changed), and `internal/nova/handlers.go` and
      `internal/nova/advanced_actions.go` (the ambiguity → `409` `NoUniqueMatch`) — makes T082 and T078's name cases
      green (R53).

**Checkpoint**: US9 green; Test 11 passes.

---

## Phase 15: User Story 11 — Operators choose the floating range with allocation pools (Priority: P2)

**Goal**: stored pools, upstream's validation, one allocator for every automatic address (FR-030–FR-032a; R30). Before
US10, whose allocation uses it.
**Independent Test**: subnet create and update with pools, refusals, and the addresses ports get.

### Tests for User Story 11 (write first, confirm RED)

- [X] T086 [P] [US11] `internal/neutron/subnet_allocation_pools_test.go` (new; *named for allocation pools, since
      `subnet_pools.go` is the subnet-pool resource*), FR-030 and FR-052: create with pools reports them on create, show
      and list; absent → upstream's default (gateway removed, a split range when the gateway is inside); `[]` → no pool;
      `pools_stored` false → default (a subnet as migration 084 leaves it); the five refusals in upstream's order
      (`InvalidAllocationPool` for malformed, start after end and wrong version; `OutOfBoundsAllocationPool`;
      `OverlappingAllocationPools`; `GatewayConflictWithAllocationPools` on create, on a pools update and on a gateway
      update); an update replaces the pools, leaves allocated ports alone, and the next allocation comes from the new
      pools; `DeleteSubnet` with a non-DHCP port on it → `409 SubnetInUse`.
- [X] T087 [P] [US11] `internal/neutron/allocation_test.go` (new), FR-032 and FR-053: the allocator never hands out an
      address outside the pools, the gateway, a port's fixed IP or a floating IP of the network; a port gets a pool
      address; a requested address outside the pools but inside the subnet is accepted; the network and broadcast
      addresses are refused; exhaustion is reported as such. Concurrent `AllocatePortForInstance` on one subnet still
      gets distinct addresses (lock); a floating-IP create, a router gateway and a port on the same external subnet
      allocated concurrently get distinct addresses (PostgreSQL; every owner takes the subnet lock, FR-032). *The
      mixed-owner case moved to T097: floating IPs and router gateways allocate through T090's allocator only after T096
      and T100.*
- [X] T088 [P] [US11] Contract `test/contract/neutron/subnet_allocation_pools_test.go` (new; *`subnet_pools_test.go`
      already tests the subnet-pool resource*): gophercloud `subnets.Create` with `AllocationPools`, the read-back, and
      the `409` for a pool holding the gateway.
- [X] T089 [P] [US11] `internal/nova/interface_attach_ip_test.go` (new), FR-053: a requested `fixed_ip` equal to the
      subnet's network or broadcast address → `400 InvalidIpForSubnet` (today accepted).

### Implementation for User Story 11

- [X] T090 [US11] `internal/neutron/allocation.go` (new): pool reading (rows when `pools_stored`, else the default),
      `generatePools` (upstream's `generate_pools`), validation, and `allocateAddress(ctx, tx, subnet, policy)` with the
      policies of data-model.md (random, lowest), excluding the gateway, ports' fixed IPs and the network's floating
      IPs, always under the subnet's `LockSubnet`, whatever the owner. Replace `allocateIP`, `AllocateFreeIP` and
      `IPAvailable`'s range logic in `internal/neutron/ports.go` with it; `IPAvailable` refuses the network and
      broadcast addresses.
- [X] T091 [US11] `internal/neutron/network.go`: `CreateSubnetRequest` and `UpdateSubnet` accept `allocation_pools`;
      store rows and `pools_stored` in the subnet's transaction; `validateSubnetGateway` and the gateway update check
      against the pools; every response reports the stored or default pools (replaces `subnetAllocationPools`);
      `DeleteSubnet` checks in-use in its transaction.
- [X] T092 [US11] `internal/nova/interface_attach.go`: use `neutron.IPAvailable`'s new refusal (makes T089 green).

**Checkpoint**: US11 green on both backends.

---

## Phase 16: Router API — gateway ports, interfaces, FR-039 (FR-038, FR-039, FR-045; R31)

### Tests (write first, confirm RED)

- [X] T093 [P] `internal/neutron/router_ports_test.go` (new): gateway set creates one `network:router_gateway` port with
      no project and a pool address, reported in `external_gateway_info.external_fixed_ips` with its subnet; a requested
      address by an administrator is used; by a member → `403`; outside the network's subnets → `400
      InvalidIpForNetwork`; equal to a subnet's gateway → `400`; taken → `409 IpAddressAlreadyAllocated`; a changed
      requested address updates the port; clear deletes it. `AddRouterInterface` stores port and interface in one
      transaction (a failing second insert leaves neither); a second interface on the same subnet → `400` with no new
      port; a subnet without a gateway → `400` (today `500`) (FR-055). The gateway port is listed for administrators
      only. *The members' half is green with T096; the administrators' half waits for T106, which is when an
      administrator's port listing stops being scoped to their own project (the gateway port has no project). Green with
      T106.*
- [X] T094 [P] `internal/neutron/router_floating_ip_refusals_test.go` (new): removing an interface whose subnet a
      floating IP of this router reaches → `409 RouterInterfaceInUseByFloatingIP`; clearing or changing the gateway
      while a floating IP uses the router → `409 RouterExternalGatewayInUseByFloatingIp`; nothing changed in either
      case; with no such floating IP both succeed.
- [X] T095 [P] Contract `test/contract/neutron/router_gateway_test.go` (new): `external_fixed_ips` read back through
      gophercloud `routers.Get`, and FR-039's two `409`s.

### Implementation

- [X] T096 `internal/neutron/router.go`: the gateway port (create, update, clear) in the router's transaction,
      `external_fixed_ips` rendered from it, the administrator rule and upstream's errors; `AddRouterInterface` in one
      transaction with upstream's `400`s and the subnet lookup scoped to the caller; FR-039's checks in the transactions
      that remove an interface or clear or change a gateway. The SNAT source becomes the gateway port's address (used by
      Phase 19). *Done as: the gateway port's addresses are stored and reported; nothing reads them for SNAT before
      Phase 19 (R51). An external network with no IPv4 subnet gives the gateway no address, as upstream's
      `_create_router_gw_port` allows; subnets that are all exhausted answer `409 IpAddressGenerationFailure`.*

---

## Phase 17: User Story 10 — Floating IPs are allocated and associated as upstream (Priority: P2)

**Goal**: no allocation race, upstream's router lookup and refusals (FR-027–FR-029, FR-031; R36).
**Independent Test**: concurrent creates; association with and without a joining router; a second floating IP on one
fixed IP.

### Tests for User Story 10 (write first, confirm RED)

- [X] T097 [P] [US10] `internal/neutron/floating_ip_allocation_test.go` (new): ten goroutines create on one external
      network at once → ten `201`, ten distinct addresses, all inside the pools (both backends; on SQLite the writers
      serialise anyway, the test still pins the answers); two external networks with overlapping subnets created
      concurrently → no `500`; a network with no IPv4 subnet → `400` and no row; pools taken → `400
      ExternalIpAddressExhausted`; `subnet_id` and `floating_ip_address` honoured; a taken `floating_ip_address` → `409
      IpAddressAlreadyAllocated` (FR-054). *Also the mixed-owner case moved from T087
      (`TestMixedOwnersOnOneExternalSubnet`): a floating IP, a router gateway and a port allocated at once on one
      external subnet. It found a real deadlock: the create had locked the floating network `FOR UPDATE` before the
      subnets, against the key-share lock a port's or gateway's insert takes on the network while holding a subnet's
      lock. The create now checks the network first, unlocked, and takes its row lock last, after the subnets. Since
      `floating_ip_address` is unique across networks, and one network node holds every floating address (FR-033), the
      allocator skips every floating address, of any network.*
- [X] T098 [P] [US10] `internal/neutron/floating_ip_router_test.go` (new): create with a port, and update to a port,
      whose subnet no router joins to the floating network → `404 ExternalGatewayForFloatingIPNotFound`, nothing stored
      (today `201` and `400`); a router joining the subnet to another external network → `404`; two joining routers →
      the one whose interface is the subnet's gateway; a second floating IP of the same network on the same fixed IP →
      `409 FloatingIPPortAlreadyAssociated` (create and update), one of another network accepted; a requested
      `fixed_ip_address` that is not the port's → `400` (FR-054).
- [X] T099 [P] [US10] Contract `test/contract/neutron/floating_ip_association_test.go` (new): the `404` and `409`
      through gophercloud `floatingips.Create`/`Update`, and ten parallel creates.

### Implementation for User Story 10

- [X] T100 [US10] `internal/neutron/floatingip.go`: `CreateFloatingIP` takes the allocation advisory lock first in its
      transaction (`pg_advisory_xact_lock` on a named constant; SQLite's registered no-op), chooses the address there
      through T090's allocator (lowest, across the network's IPv4 subnets), honours `subnet_id` and
      `floating_ip_address`, refuses no subnet (`400`) and exhaustion (`400 ExternalIpAddressExhausted`); remove the
      pre-transaction `allocateFloatingIP` and the `192.0.2.0/24` fallback.
- [X] T101 [US10] `internal/neutron/floatingip.go`: replace `lockGatewayRouter` with upstream's lookup (an interface
      port on the fixed IP's subnet, a gateway port on the floating network, the gateway-interface preference), `404`
      when none, on create and update; the FR-029 check in the same transaction; `portFixedIP` verifies a requested
      fixed IP is the port's.

**Checkpoint**: US10 green on both backends. Phases 11–17a are the API increment: no layout change; the gateway port's
address is stored and reported, and egress keeps today's host SNAT until Phase 19 (R51).

---

## Phase 17a: User Story 12 — An administrator sees and manages every project's networking resources (Priority: P3)

**Goal**: every Networking API listing, show and write as upstream's for administrators (FR-057, FR-058; R50).
**Independent Test**: contract — as an administrator, another project's resource in each listing, by project filter and
by id; as a member, nothing changes.

### Tests for User Story 12 (write first, confirm RED)

- [X] T102 [P] [US12] `internal/neutron/admin_listing_test.go` (new), table-driven over the sixteen listings of R50: an
      administrator of project A sees project B's resource unfiltered; `?project_id=B` and `?tenant_id=B` list only B's;
      a show by id of B's resource answers `200`; `GetSecurityGroup` by B's group's name answers `404`; a member of A
      sees what they see today (own project; networks also shared and external) and gets `404` showing B's resource
      (lock). *Seen RED before T106. It also found an administrator's unfiltered network listing answering `500`: with
      no condition left, `ListNetworks` rendered an empty `WHERE`; it now takes the filter's own clause.*
- [X] T103 [P] [US12] Contract `test/contract/neutron/admin_listing_test.go` (new): the same through gophercloud as the
      seeded administrator and as a member of a second project, for networks, ports, routers, floating IPs and security
      groups. *RED against a binary built with the administrator branches of `list_scope.go` turned off (`go build
      -overlay`), green against the real one.*
- [X] T104 [P] [US12] `internal/neutron/admin_write_test.go` (new), table-driven over every update, delete and action
      handler of R50 and the creates' references by id: an administrator of project A succeeds on project B's resource
      by id, a member of A gets `404` (lock); a resource the administrator creates belongs to the named project or to A;
      an administrator putting B's security group on A's port by id succeeds, as upstream (FR-018). *44 writes and 12
      creates. With the administrator branches off, 54 of the 56 fail; the two that pass are `UpdateNetwork` and a
      subnet on another project's network, whose administrator exemptions SPEC-024 and SPEC-022 already had. The RBAC
      cases use a project id as `target_tenant`: upstream's `*` answers `500` on PostgreSQL, where the column is a UUID
      (pre-existing, not this feature's).*
- [X] T105 [P] [US12] Change round 1's pins of the old id rule to upstream's, FR-004 and FR-018 (R52):
      `internal/nova/security_group_actions_test.go`, `internal/neutron/port_api_security_groups_test.go` and
      `test/contract/nova/security_group_actions_test.go` — an administrator naming another project's group **by id** in
      `addSecurityGroup`, server create or port create/update now succeeds; **by name** it still resolves in the
      server's project (entry 5); a member naming another project's group by id or name still gets `404`. Red against
      the current code. *Server create excepted (2026-11-20, during implementation): upstream's boot matches the
      requested groups against the instance's project's (`nova/network/neutron.py`, `_process_security_groups`,
      `search_opts = {'tenant_id': instance.project_id}`), so another project's group never reaches a booted server
      there, as spec.md's own account of upstream says. The test pins server create's `400` for an administrator's
      foreign id instead. The clarification of 2026-11-20 lists server create; raised at the Phase 17a checkpoint. The
      Nova contract case boots a server, so like the other boot cases it skips when no active image exists.*

### Implementation for User Story 12

- [X] T106 [US12] `internal/neutron/list_scope.go` (new): `projectScope(c, f, column)` per R50, and its use in all
      sixteen listings in `internal/neutron/network.go`, `ports.go`, `router.go`, `floatingip.go`, `port_forwarding.go`
      and the QoS, RBAC, trunk, address-scope, subnet-pool, IP-availability and metering handlers; the shows by id skip
      the project condition for an administrator, a name branch keeps it. The same helper decides every write's target
      (update, delete, action, and a create's references by id) for an administrator, per R50; the membership helpers
      follow T107. *Also taken by the rule: the agent-scheduler lookups (DHCP agents of a network, L3 agents of a
      router) and `externalNetworkRefusal`, so another project's internal network is "not external" to an administrator,
      as upstream's, rather than unknown. A router interface's port belongs to the router's project and a port
      forwarding to its floating IP's, as upstream's. Subnet pools and network IP availability rendered the caller's
      project as the resource's; they render the row's now. Older tests that used the administrator's header to test a
      member's isolation now use a member's (`security_group_rule_authz_test.go`, `security_group_shape_test.go`'s
      listing, `router_gateway_test.go`'s disclosure test, which also pins an administrator's `400` for another
      project's private network), and `security_group_delete_test.go` pins upstream's `204` for an administrator.
      `test/security_groups_test.sh` narrows its listing to the token's project: unnarrowed, its Test 1 passed on other
      projects' `default` groups and Test 11 read several project ids.*
- [X] T107 [US12] `internal/neutron/ports.go` (`ResolveSecurityGroup`, `requireGroupsOfProject`) and their callers in
      `internal/nova/handlers.go` and `internal/nova/advanced_actions.go`: an administrator's id matches any project's
      group; names unchanged (makes T105 green; R52). *Server create passes `false`; see T105.*

**Checkpoint**: US12 green on both backends.

---

## Phase 18: The network node and the node call (FR-033, FR-034; R26, R27)

### Tests (write first, confirm RED)

- [X] T108 [P] `internal/neutron/node_call_test.go` (new): a node token (audience `lightstack-node`, `node`, `exp`,
      `body_sha256`) is accepted by `/internal/v1/converge`; expired, wrong audience, wrong node, tampered body → `401`;
      a user token → `401`; a node token on any `/v2.0` route → `401`; a unit the target does not hold (not the network
      node; a port bound elsewhere) → `409`; a target that blocks → the caller fails within one second, and the target
      itself gives up within 900 ms of receipt, lock wait included; `absent` for a port no longer bound to the target is
      accepted; a call to self bypasses HTTP; the body carries only unit, ids and `absent`.
- [X] T109 [P] `internal/neutron/network_node_test.go` (new): which node holds which unit (routers and DHCP the network
      node; ports their `binding_host_id`), the caller resolving a node's address from the registry's tunnel IP and the
      Neutron port, and a missing or inactive node failing the step.

### Implementation

- [X] T110 `internal/neutron/node_call.go` (new): the token's minting and checking with `keystone.jwt_secret`, signing
      inline with `golang-jwt` as `internal/keystone/auth.go:397` does (keystone exports no helper; no new module), the
      HTTP client with a one-second timeout, the `/internal/v1/converge` handler (unit lock, read, render, apply, all
      within one 900 ms deadline from receipt), routing in `internal/neutron/network.go`'s route setup outside `/v2.0`,
      and the real `nodeCaller` over the registry. `internal/middleware/auth.go` refuses tokens with the node audience.
      *Keystone's `ValidateToken` refuses the node audience, which covers every service's user authentication and
      Keystone's own token validation; `AuthMiddleware` lets `/internal/` through to the handler, which checks the node
      token itself. A node token also lives at most ten seconds by its own claims (`exp - iat`), checked on receipt.*
- [X] T111 `cmd/lightstack/main.go`: pass the network node name, the node name, the registry (nil without VXLAN) and the
      Neutron port to `neutron.NewService`; refuse to start with VXLAN on and no network node (T064). *Through
      `SetNodes`, not the constructor's arguments: its twenty callers in tests are single nodes, which is the default.
      The refusal was already T070's.*

---

## Phase 19: The router unit — namespace, legs, NAT, port forwarding (FR-033, FR-035, FR-037, FR-038, FR-049; R28, R37)

### Tests (write first, confirm RED)

- [X] T112 [P] `pkg/networking/router_namespace_test.go` (new), pure renderers: for a router with a gateway, two
      interfaces, two floating IPs and a port forwarding, the namespace's nat and mangle `iptables-restore` payloads
      equal `contracts/router-namespace-dataplane.md`'s rules (chain names, `lightstack` comments, SNAT exception,
      hairpin mark, `ls-pf-<id[:8]>`); devices and addresses (`qg-`/`lg-`, `qr-`/`lr-`, `/32` floating addresses,
      default route); host routes and proxy entries; SNAT off drops the two SNAT rules; a port-forwarding range as
      upstream. *SNAT off drops only `-o qg-… -j SNAT`, as upstream's `_add_snat_rules`: the exception for traffic
      between the router's own networks and the hairpin rule stay whatever `enable_snat` says, and the contract, which
      marked the exception "(SNAT on)", is corrected. The jumps from the built-in chains and `ls-l3-snat -j
      ls-l3-float-snat` are in the contract now. No range case: Lightstack's port forwardings carry single ports, with
      no `external_port_range` column or attribute, so there is no range to render.*
- [X] T113 [P] `pkg/networking/router_namespace_apply_test.go` (new), with the fake command runner: adding applies
      namespace, legs, addresses, one restore per table, then floating addresses, then host routes and proxy entries;
      removing the reverse; an unchanged router runs reads only; every command gets at most one second; a failing or
      blocking command fails the call within about a second; stub mode runs nothing. *The fake command runner moved to
      `pkg/networking/fake_runner_test.go`: `ingress_filter_test.go`, where it lived, goes with T132.*
- [X] T114 [P] `internal/neutron/router_unit_test.go` (new), against the fakes: a floating-IP create, update, move,
      disassociate and delete, a port forwarding's create, update and delete, an interface add and remove, and a gateway
      set, change and clear each converge their router on the network node before the answer (through the fake node
      caller when served by another node); a failing router unit gives `500` and the compare-and-set undo of R20
      (FR-008) or the re-apply of FR-040; a floating IP deleted with port forwardings removes their rules with it. *Two
      services on one database, the API node and the network node, with the fake node caller between them
      (`router_unit_test.go`, `fake_units_test.go`).*
- [X] T115 [P] Rework `internal/neutron/floating_ip_dataplane_test.go` and `internal/neutron/ingress_refresh_test.go`:
      their floating-IP NAT and filter assertions move to the router unit (T114) and the port unit (T127); keep only
      what still holds, or delete the file, and say which in this task's note. *`floating_ip_dataplane_test.go` kept and
      reworked: its NAT checks became checks of the router's converged state, and one event log orders the filter and
      the router; the case of an unmarked host rule keeping a filter went with host NAT. `reconcile_dataplane_test.go`
      is cut to the filter cases now, ahead of T146, since its NAT pass went with T118; `ingress_refresh_test.go`'s pass
      test is renamed. `router_snat_db_test.go` is deleted, and `router_ownership_test.go`'s egress-SNAT test removed:
      per-range host SNAT is gone.*

### Implementation

- [X] T116 `pkg/networking/router_namespace.go` (new): the renderers and the real-mode converge through `rm.run`
      (bounded), per the contract. It adds the new code beside the old and removes nothing; T118 removes the old
      functions once nothing calls them.
- [X] T117 `internal/neutron/units.go`: the router unit — read a router's records (router, gateway port, interfaces with
      their ports and subnets, floating IPs, port forwardings), render, converge through `routerDataplane` under the
      router lock; and `convergeRouter(ctx, id, absent)` routed through the node caller to the network node. *The router
      unit's request names the router's records to read as gone (`absent` as a list, as the ports unit's), so a deletion
      converges the router without one interface, port forwarding or gateway before that record goes (FR-040).
      `contracts/node-apply.md` updated.*
- [X] T118 Switch every caller to the router unit and remove the old functions in the same task, so the build never
      breaks: `internal/neutron/reconciler.go` (its floating-IP and egress passes; the router-unit pass itself is T147),
      `internal/neutron/teardown.go` (the old calls go; the new purge is the teardown task),
      `internal/neutron/network.go` (the NAT methods of `floatingIPDataplane`; its filter methods, the `dataplane` field
      and `lockNode` stay until T132, which still has `RefreshPortFilters` and `removeOrphanFilter` using them),
      `internal/neutron/floatingip.go` (create, update, delete, `ConvergeFloatingIPs`, replacing `convergeFloatingIP*`
      and the node lock), `internal/neutron/port_forwarding.go` (create, update, delete; checks in their transactions:
      the floating IP's router, the internal port's project and address, duplicates), `internal/neutron/router.go`
      (interfaces, gateway; replacing `configureExternalGateway`, `withdrawEgressSNAT`, the namespace calls),
      `internal/neutron/ports.go` (`convergeDisassociated`), `internal/nova/handlers.go` (`ConvergeFloatingIPs` after
      server deletion). Remove `EnsureFloatingIP`, `WithdrawFloatingIP`, `EnsureEgressSNAT`, `RemoveEgressSNAT` and the
      host-namespace parts of `NorthSouthState` from `pkg/networking/northsouth.go`, and `CreateRouterNamespace`,
      `AttachInterfaceToRouter`, `DetachInterfaceFromRouter`, `AddPortForwarding`, `RemovePortForwarding` and the unused
      `SetDefaultGateway`/`AddRoute`/`DeleteRoute` from `pkg/networking/router.go`, with their tests. *FR-040's order
      for router deletion, interface removal, gateway clear and port-forwarding deletion is in place now; T140 keeps
      them. Router create and gateway set or change are undone compare-and-set (FR-045). The fixed IPs' filters stay
      until T132: a floating-IP change converges its routers and the filters of the fixed IPs it reached and reaches
      (`applyFloatingIPChange`). `DisassociatePortFloatingIPs` returns the associations it took away, so their routers
      can be converged. `DeleteRouterNamespace` stays for the teardown's fallback until T148, now deleting exactly
      `qrouter-<suffix>`; it truncated to eleven characters, which missed the full-id namespaces. A port forwarding's
      update converges only when it moves and its floating IP has a router; `update_statement_test.go`'s fixture gives
      its port the address the update moves to.*

---

## Phase 20: The DHCP unit and metadata (FR-037; R29)

### Tests (write first, confirm RED)

- [X] T119 [P] `pkg/networking/dhcp_namespace_test.go` (new): `dnsmasq`'s rendered configuration for a network with two
      DHCP subnets: static ranges per subnet with tags, a host entry per port, option 3 the gateway, option 121
      `169.254.169.254/32,<DHCP address>` and `0.0.0.0/0,<gateway>`, DNS; a gatewayless subnet gets the metadata route
      only; the namespace and device plan (`qdhcp-<N>`, `tap<port[:11]>`, the addresses and `169.254.169.254/32`);
      real-mode apply through the fake runner, bounded, stub mode inert. *Hosts and subnets are rendered in a fixed
      order (subnet id, then MAC), so an unchanged network renders the same bytes and dnsmasq is left alone; a change
      restarts it and keeps the lease file. A `dnsmasq` an earlier process left on the same configuration is stopped
      first (`pkill -f <conf>`).*
- [X] T120 [P] `internal/neutron/dhcp_unit_test.go` (new), against the fakes, the DHCP port's host end `ld-<port[:11]>`:
      the network's first DHCP subnet creates a `network:dhcp` port with the lowest free pool address and the network's
      project; a second subnet adds an address; the last one removed deletes the port; subnet create, update and delete,
      and port create and delete (reservations), converge the DHCP unit on the network node before the answer; a failing
      unit gives `500` and the undo (create, update) or re-apply (delete). *Written RED 2026-11-20 against two services,
      the fake node caller, a fake DHCP and fake metadata listeners (the router fake became a generic holder both
      share). Beyond the list: the network's delete converges its DHCP away first (FR-040), since its DHCP port now
      exists and `networkInUse` must pass it by, and a listener that cannot be opened fails the unit. The DHCP port's
      `device_id` is the network's id, not `dhcp<network id>`: `ports.device_id` is a `uuid` column on PostgreSQL (R29
      notes it).*
- [X] T121 [P] `internal/metadata/network_identity_test.go` (new): the handler resolves an instance by network and
      source address; two networks with the same address resolve to their own instances; the existing loopback and
      trusted-header rules stay (lock). *Written RED 2026-11-20: `ServeNetwork(handler, network)` carries the network
      into the request; the per-network cases fail without it, the header-rule lock passes.*

### Implementation

- [X] T122 `pkg/networking/dhcp_namespace.go` (new): renderers and real-mode converge, beside the old code; it removes
      nothing (T123 does) (namespace, veth to the bridge, addresses, `dnsmasq` started and stopped inside the namespace,
      its files under the state path). *The bounded command helper became package-level (`runCommand`), shared with the
      router, and `hostEnd` knows a DHCP namespace's `tap`→`ld-`. The address reader kept IPv4 link-local addresses out
      as if the kernel had added them; only IPv6 ones are, and 169.254.169.254 is Lightstack's. `MetadataIP` moved here
      in T123.*
- [X] T123 `internal/neutron/units.go`, `internal/neutron/network.go`, `internal/neutron/ports.go`
      (`AllocatePortForInstance`, `ReleaseInstancePorts`) and `internal/neutron/reconciler.go` (`reconcileSubnets`): the
      DHCP unit and its triggers (subnet create, update, delete; port create, delete; `AllocatePortForInstance`,
      `ReleaseInstancePorts`), the DHCP port's lifecycle, under the network's lock, through the node caller. Replace
      `applyDHCPForNetwork`'s callers. In the same task, retire `pkg/networking/metadata_redirect.go`'s per-bridge DNAT
      and the bridge addressing in `pkg/networking/dhcp.go`, with every caller, so the build never breaks. *Done
      2026-11-20. The unit and the DHCP port's lifecycle are `internal/neutron/dhcp_unit.go`. Triggers: subnet create
      and gateway update, undone compare-and-set on failure; subnet, port, network and auto-allocated-topology delete
      and `ReleaseInstancePorts`, converged away first and re-applied on failure (FR-040); port create and
      `AllocatePortForInstance`, undone compare-and-set on the `updated_at` the create left (`SetPortSecurityGroups`
      touches the port, so `updated_at = created_at` never holds there). Lock order, after a deadlock found in review: a
      request never waits for an existing subnet's lock while it holds the network's. A subnet create locks the network,
      inserts, and allocates on its own subnet only; a deletion locks its subnet, then the network, and allocates
      nothing; only the reconciliation pass allocates on the other subnets, locking them in creation order before the
      network, as a floating-IP create does. `reconcileSubnets` realigns each network's DHCP port, making it again when
      it was deleted through the API (as upstream's agent has a new one made when it resyncs), and converges the unit on
      the network node; on every node it removes the earlier layout's metadata DNAT. Retired: `MetadataRedirect.Ensure`
      (the removal side stays for FR-044), the host dnsmasq of `dhcp.go`, `EnsureBridgeIP`/`RemoveBridgeIP`,
      `applyDHCPForNetwork`, `dhcpReservations`, `dhcpRange`, `bridgeAddrFor`, `desiredRedirectsNow`, both
      `reconcileNetworkDHCPAfter*`, `decrementIP`. `networkInUse` passes the DHCP port by, as upstream's auto-deleted
      owners. Old tests: `dhcp_options_test.go`, `dhcp_range_test.go`, `bridge_addr_test.go`,
      `subnet_delete_dhcp_test.go` and `TestDesiredRedirectsNow…` deleted (T119 and T120 pin what still holds);
      `TestApplyDHCPForNetworkTellsUnreadFromAbsent` became `TestReadDHCPStateTellsUnreadFromAbsent`; the
      network-delete, topology-delete and prune tests hold a fake DHCP; `TestSubnetAllocationPools`' single-address
      subnet goes without DHCP, whose port would take the address, as upstream's does. Added
      `TestReconcilerMakesTheDHCPPortAgain`. Found here: `CreatePort` ignored `fixed_ips` altogether, so FR-053's port
      half was unmet, and the port-forwarding contract test failed once Phase 19's internal-address check (upstream's)
      ran. Fixed: the router gateway's requested-address resolver is shared (`requestedAddresses`), pinned by
      `TestPortCreateRequestedFixedIPs`. Left open: the DHCP port's VXLAN FDB entry must name the network node's VTEP
      (T137/T140); IPv6 subnets get no DHCP address, as before.*
- [X] T124 `internal/metadata/service.go` and a new `internal/metadata/netns_listener.go`: a listener per `qdhcp-*`
      namespace on `169.254.169.254:80`, opened by entering the namespace on a locked OS thread, serving the handler
      with its network; identity by network and source address. The DHCP unit opens and closes it through an interface
      Neutron receives from `main.go`. *Done 2026-11-20. `ServeNetwork` carries the network, and `instanceHolding`
      matches on it (`internal/metadata/service.go`). `Listeners` (`internal/metadata/netns_listener.go`) keeps one
      listener per network, opens a new one when the namespace was replaced (its device and inode changed), and closes
      it before the namespace goes. The namespace entry is `pkg/networking/netns_listen_linux.go` (`ListenInNamespace`,
      `NamespaceID`): a fresh goroutine on a locked thread, ended rather than unlocked when it cannot leave; `!linux`
      refuses. `main.go` builds one metadata engine for the host listener and the namespace listeners, and gives Neutron
      the listeners in real networking mode before the reconciler starts. `TestListeners…` pin the bookkeeping on a
      loopback socket; the namespace itself is the host runs' (T166, T167).*

---

## Phase 21: The port firewall (FR-041–FR-043, FR-047; R32, R33)

### Tests (write first, confirm RED)

- [X] T125 [P] `pkg/networking/port_firewall_test.go` (new), pure: for ports with groups, rules, a remote group with
      members, IPv4 and IPv6, the rendered `iptables-restore` and `ip6tables-restore` payloads equal
      `contracts/port-firewall.md` (dispatch, `ls-i`/`ls-o`/`ls-s` chains, DHCP and neighbour-discovery allowances,
      spoof per fixed IP and MAC, the rule block with `RELATED,ESTABLISHED`, de-duplicated `RETURN`s, `INVALID` drop,
      fallback; remote groups expanded excluding the port's own addresses; trusted ports accepted; raw zone rules; a
      port with stateless groups gets `CT --notrack` and no `RELATED,ESTABLISHED`/`INVALID` rules (FR-051)); trusted
      ports accepted by `lr-`/`ld-` device name; a port with no group renders the block without rules; the payload
      removes a deleted port's chains and dispatch. *Done 2026-11-23. The trusted legs are accepted by wildcard (`lr+`,
      `ld+`), as the router's `lg+` forward chain, so a router or DHCP port needs no binding and no trigger; the zones
      live in `ls-ct`, jumped first from raw `PREROUTING`, so a restore replaces them without touching anyone else's
      rules (contracts/port-firewall.md says both). IPv6 has upstream's ICMPv6 and DHCPv6 allowances, but no router
      advertisement is let in, as no Lightstack router sends one; a port without an IPv6 address sends no IPv6 but its
      duplicate address detection, where upstream would let it through unchecked. ICMP type 0 is a type, not "any" (the
      rule's ports are pointers). Traffic from a port to the host, which INPUT brings, reaches its egress chain through
      a dispatch rule without `--physdev-is-bridged`. Added in Phase 22: a TAP with no bound port (its port deleted
      while its guest runs; no event detaches it) is dropped by `tap+` rules after every port's dispatch, so it is not
      let through unfiltered (contracts/port-firewall.md).*
- [X] T126 [P] `pkg/networking/port_firewall_apply_test.go` (new), fake runner: compares with `-S` per chain and
      restores only on a difference, one commit per family, bounded at one second; stub mode inert. *Done 2026-11-23.
      The node reads each table once per family (`iptables-save -t filter`, `-t raw`) and compares chain by chain; a
      node holding its firewall gets no restore.*
- [X] T127 [P] `internal/neutron/port_unit_test.go` (new), against the fakes, two services and the fake node caller: a
      membership change, a rule add and delete, a change to a remote group's members, and a port's binding change each
      converge the affected ports' chains on their bound nodes before the answer, one call per node; a failure gives
      `500` and the existing compare-and-set undo (FR-008); an unbound port has no chains; a port whose binding moves
      sends `absent` to the old node and presence to the new one in the same request; the same converges happen in
      `ebpf` mode (FR-047); `CreatePort` no longer calls the eBPF manager (FR-045); the network's `ct_zone` is assigned
      at creation, lowest free from 4097. *Done 2026-11-23, RED first (behaviourally, with the seams inert). Three
      services on one database and the fake node caller; every host-touching seam is the test's, the bridges included,
      since the ports unit makes its ports' bridges. It also pins `binding:host_id` for administrators only, T129's last
      point, which is Neutron's.*
- [X] T128 [P] `pkg/hypervisor/xml_template_test.go` (extend): the interface element carries `<target dev='tap<port
      id[:11]>'/>`. *Done 2026-11-23. Nova passes the device (`NetworkConfig.Device`, from `networking.PortDevice`):
      Neutron owns the TAP's name, the hypervisor renders it.*
- [X] T129 [P] `internal/nova/port_binding_test.go` (new), FR-041 and FR-056: boot sets the ports' `binding_host_id` to
      the node name and converges their chains before `DomainCreate` (the fake `NeutronService` records the order); a
      failing converge puts the server in `ERROR` with a fault and starts no domain; interface attach binds and
      converges before `AttachDevice`, and answers `500` and unbinds on failure; detach clears the binding and converges
      the old node `absent`; `GET /v2.0/ports/{id}` reports `binding:host_id` to an administrator only. *Done
      2026-11-23. The boot cases stage a boot that reaches the domain (qemu-img, a redirected state root, a raw base),
      as the boot failure tests' success path does, and skip without qemu-img; the order is pinned by the instance
      having no domain yet when BindPort runs. `binding:host_id` is pinned in T127. Real mode does not hot-plug an
      attached interface (the attach only records it, a gap that predates this work), so there is no AttachDevice for
      the binding to precede.*
- [X] T130 [P] Rework the round-1 filter tests: `internal/neutron/security_group_rule_refresh_test.go`,
      `internal/nova/security_group_actions_test.go`'s failure cases and `pkg/networking/ingress_filter_test.go` assert
      the port unit instead of the floating-IP filter (FR-042); `reconcile_dataplane_test.go`'s filter cases move to
      T146. Note per file what changed. *Done 2026-11-23, per file: `pkg/networking/ingress_filter_test.go` and
      `floating_ip_nat_test.go` deleted with the LS-NSI filter (T125, T126 pin the port firewall);
      `internal/neutron/ingress_refresh_test.go` deleted (T127 pins `RefreshPortFilters`' new meaning) and
      `fake_dataplane_test.go` with its seam; `reconcile_dataplane_test.go` deleted with `reconcileFilters` (T146 brings
      the units' reconciliation tests); `security_group_rule_refresh_test.go` and `security_group_revision_test.go`'s
      undo cases onto the fake firewall with bound ports, the no-uplink case gone, as the port firewall needs no uplink;
      `port_api_security_groups_test.go`'s floating-IP cases are now a bound port's, its busy case holding the ports
      unit's lock; `floating_ip_dataplane_test.go` keeps its routers only, its "old filter stays" case gone and its busy
      case holding the router's unit lock; `port_xdp_failclosed_test.go` deleted with `CreatePort`'s XDP step (FR-045),
      which T127's ebpf case replaces; `internal/nova/security_group_actions_test.go`'s recorder passes the groups on,
      its failure cases unchanged.*

### Implementation

- [X] T131 `pkg/networking/port_firewall.go` (new): renderers and real-mode converge through `rm.run`, beside the old
      code; it removes nothing (T132 does). *Done 2026-11-23: `pkg/networking/port_firewall.go`
      (`FirewallManager.ConvergePorts`, `PortFirewallRules`, `firewallPayload`). `canonicalSource` and `portSpec` moved
      there from `northsouth.go`.*
- [X] T132 `internal/neutron/units.go`: the port unit — read each port's groups, rules, remote members, addresses, MAC
      and network zone; group by `binding_host_id`; one node call per node. `RefreshPortFilters` and
      `rebuildGroupFilters` converge port units; `portIngressRules` and `floatingFixedIPs` go. Remove the LS-SG chain
      calls from `CreateSecurityGroup`, `DeleteSecurityGroup`, `CreateSecurityGroupRule`, `DeleteSecurityGroupRule`. In
      the same task, update every remaining caller (`internal/neutron/floatingip.go`, `internal/neutron/teardown.go`,
      `internal/neutron/reconciler.go`) and delete `RenderIngressFilter`, `EnsureIngressFilter`, `RemoveIngressFilter`
      and the `LS-NSI` helpers from `pkg/networking/northsouth.go`, and the `LS-SG-*` chain code in
      `pkg/networking/security_groups.go`, so the build never breaks. Remove what is left of `floatingIPDataplane`, the
      `dataplane` field, `lockNode` and the old fake in `internal/neutron/network.go` and its tests. *Done 2026-11-23.
      The ports unit is `internal/neutron/port_unit.go`. A node converges every port bound to it from the records, in
      one restore per family; ports named absent are read as gone, from the remote-group expansions too. `convergePorts`
      finds the affected ports from a `portChange` (ports, groups whose rules changed, groups whose members changed,
      absent ports) and calls each node once. `RefreshPortFilters(ctx, ports, groups)` takes the groups whose members
      changed: the previous and the new for a port update, the added or removed one for Nova's actions.
      `rebuildGroupFilters` became `convergeGroupRules`. Floating IPs converge their routers only. Deleted: the LS-NSI
      code of `northsouth.go`, the LS-SG code of `security_groups.go` (its eBPF half stays, unloaded),
      `floatingIPDataplane`, the `dataplane` field, `lockNode`, the reconciler's filter pass (the ports' pass is T147),
      and the teardown's filter and chain loops (the teardown converges the node's ports to none).*
- [X] T133 `internal/neutron/network.go`: assign `ct_zone` in `CreateNetwork`'s transaction.
      `internal/neutron/ports.go`: report `binding:host_id` (administrators), accept it from Nova's calls; stop creating
      `tap-<port8>` in `CreatePort` and `AllocatePortForInstance` (libvirt owns the TAP), and remove `CreatePort`'s eBPF
      step with it (`ports.go:215-241`; FR-045, FR-047). *Done 2026-11-23. `ct_zone` is computed in the network's
      insert, the lowest free from 4097, retried when another create took it; the auto-allocated topology's network gets
      one too. `binding:host_id` is reported to administrators, in the list and the show. `CreatePort` and
      `AllocatePortForInstance` make no device and no bridge: the bound node's ports unit makes its ports' bridges
      before libvirt needs them. A port create converges the ports whose rules expand its groups, which it joins with
      its addresses, and is undone compare-and-set when that fails. With the eBPF step gone, the Neutron service holds
      neither the eBPF nor the TAP manager, and `teardownPortHost` keeps only the VXLAN entry.*
- [X] T134 `pkg/hypervisor/xml_template.go` and `internal/nova` (boot, attach, detach): the TAP name in the domain XML;
      `binding_host_id` set and cleared, and the chains converged, through `NeutronService` (new method `BindPort(ctx,
      portID, node string) error`, converging before it returns; faked in `boot_failure_test.go`'s `fakeNeutron`),
      before `DomainCreate`/`AttachDevice`, with T129's failure handling. *Done 2026-11-23. Nova's service knows its
      node, as Neutron's does (`common.NodeName()`). An instance records no node, so a guest's ports are bound to the
      node its boot ran on, which is where it runs; a node without a name fails the boot. Boot binds each port right
      before the domain is created; attach binds after storing its records and undoes them all on failure; detach
      unbinds before its records go. Found, not fixed here: an interface attach creates its port in Nova's own
      transaction, so neither the network's DHCP reservation nor the remote-group members converge for it.*

---

## Phase 22: Deletions and the remaining Article II paths (FR-040, FR-045; R34, R40)

### Tests (write first, confirm RED)

- [X] T135 [P] `internal/neutron/deletion_order_test.go` (new), against the fakes: for a port, a router interface, a
      router, a gateway clear, a subnet, a network and a port forwarding — the refusal checks run first; the affected
      units converge to absent before the record goes; a failing unit → `500`, the record kept, the units converged from
      it again; a refusal in the final transaction (a dependant added in between) → that refusal and the units
      re-applied; the fake records the order. *Done 2026-11-23. Three nodes (API, network, compute) with every unit
      faked; each case's hook checks the record still exists when its unit converges without it, and adds a dependant
      meanwhile where a final refusal can happen (a port on the subnet or the network, an interface on the router, a
      floating IP through the gateway). Part of what it pins Phases 19 and 20 had already done (the router's deletions,
      a subnet's DHCP); T161's mutations cover the new paths.*
- [X] T136 [P] `internal/neutron/network_create_undo_test.go` (new): a failing bridge on create removes the row
      compare-and-set on `updated_at`; a row updated in between stays; a row another request built on (a subnet added)
      stays, with its subnet. *Done 2026-11-23, with a bridge seam (`bridgeDataplane`; the fake makes the create fail
      and, through its hook, changes or builds on the network meanwhile).*
- [X] T137 [P] `internal/neutron/port_fdb_test.go` (new), FR-045 and R51: with VXLAN on, a port create writes its FDB
      row in the port's own transaction; a failing FDB insert answers `500` and leaves no port; a pre-084 network
      without `ct_zone` gets the lowest free zone the first time one of its ports is rendered. *Done 2026-11-23, with an
      FDB seam (`fdbStore`; the VXLAN coordinator in production). The zone of an older network is assigned on its first
      port's convergence, here through `BindPort`.*
- [X] T138 [P] `internal/nova/server_delete_ports_test.go` (new): server deletion runs its ports' deletion through
      FR-040 after libvirt; a failing port unit → `500`, the server and its ports kept, a second delete succeeds;
      `forceDelete` the same. *Done 2026-11-23: the fake's `WithdrawPorts` checks the domain is gone when it runs; a
      failing withdrawal answers 500 and keeps the server and its ports, and a second deletion finishes. `forceDelete`
      answers 204, as it did. `floating_ip_dataplane_test.go`'s port-deletion failure case now pins FR-040's order (a
      router that cannot be converged keeps the port and its association, 500) instead of R25's departure (the deletion
      stood), and `TestPortUnitBinding` the old node of a moved binding converging from the records.*
- [X] T139 [P] `internal/neutron/auto_allocated_topology_unit_test.go` (new): a failing unit during auto-allocation
      answers `500` and leaves no half topology. *Done 2026-11-23: a failing bridge or DHCP leaves no network, subnet or
      DHCP port; a successful one gives the topology its DHCP.*

### Implementation

- [X] T140 `internal/neutron/ports.go` (`DeletePort`, `ReleaseInstancePorts`, `teardownPortHost` reduced to the units),
      `internal/neutron/router.go` (`DeleteRouter`, `RemoveRouterInterface`, gateway clear),
      `internal/neutron/network.go` (`DeleteSubnet`, `DeleteNetwork`, `teardownNetworkHost`, network create's undo: one
      `DELETE … WHERE id AND updated_at AND NOT EXISTS (dependants)` statement), `internal/neutron/port_forwarding.go`
      (delete), `internal/neutron/auto_allocated_topology.go`: FR-040's order through the units, and FR-008's undo on
      every create and update path named in R40. Port creation's FDB row moves into the port's transaction
      (`CreatePort`, `AllocatePortForInstance`), and the null-`ct_zone` assignment of R51 goes into the port unit's read
      (makes T137 green). *Done 2026-11-23. `WithdrawPorts` (`internal/neutron/port_unit.go`) reads what deleting ports
      touches — the routers of their floating IPs, their networks' DHCP, their groups — converges all of it as if the
      ports were gone, and returns the restore; port deletion and `ReleaseInstancePorts` use it, and `teardownPortHost`
      went (the forwarding row cascades with its port). A network's deletion locks it and checks it is unused again in
      its final transaction, so a port added while its DHCP converged is refused, not cascaded away. Network create's
      undo is one compare-and-set statement (`undoNetworkCreate`), shared with the auto-allocated topology, now built
      from its parts: one transaction for the network with its zone, the subnet and its DHCP port, then the bridge and
      the DHCP unit. The forwarding row is written in the port's transaction (`fdbStore`), naming this node's tunnel
      address: the node a server's ports are allocated on is the node it boots on. An older network's zone is assigned
      in the port unit's convergence (R51). Found here and fixed: a binding that moved sent the port as absent to its
      old node, which read it as gone from the remote-group expansions too, though it stays a member; the old node now
      converges from the records, and a ports request may name no port (contracts/node-apply.md). The DHCP and router
      units make the bridges they join on the network node, where nothing else made them before the reconciliation
      pass.*
- [X] T141 `internal/nova/handlers.go` `deleteServer`: the ports' deletion through Neutron's FR-040 path after libvirt;
      a failure answers `500` and keeps the instance. *Done 2026-11-23. After the domain, before the transaction: the
      ports attached by id are unbound (`BindPort`), the boot ports withdrawn (`WithdrawPorts`); a failure answers 500
      and keeps the server, and the transaction's failure restores. A domain already gone counts as deleted, so a second
      deletion finishes. The tenant ports' detach clears their binding.*

---

## Phase 23: Reconciliation by role (FR-010 restated, FR-033, FR-042, FR-044; R26, R28–R33)

### Tests (write first, confirm RED)

- [X] T142 [P] `internal/neutron/reconcile_units_test.go` (new): on the network node a pass converges every router and
      DHCP unit with a record, restarting a network's `dnsmasq` whose process is gone (after a restart), and removes
      namespaces, legs, proxy entries and listeners of units without one, only after a complete read; a matching unit is
      left untouched; on another node a pass removes any `qrouter-*`/`qdhcp-*` it finds; every node converges its bound
      ports' chains and removes `ls-` chains no bound port holds, after a complete read. *Done 2026-11-23. Pinned
      against the fakes, which now list what they hold (`HeldRouters`, `HeldNetworks`, the listeners' `Serving`): on the
      network node a pass converges every router and network DHCP with a record and converges the ones it holds or
      serves without one, which reads them as absent; a partial or failed read removes nothing and still converges what
      it read; another node removes every `qrouter-`/`qdhcp-` it holds and closes their listeners, converging no router
      to a state; every node converges its bound ports, which removes the chains of ports not bound to it. Each test's
      passes read only its own records through the reconciler's `readIDs` seam: a pass over the shared test database
      would converge, and write the DHCP ports of, other tests' networks. The restarted dnsmasq is pinned where it
      happens, `TestConvergeDHCP_AfterARestartStartsDNSMasqAgain` (green at once: Phase 20's `ensureDNSMasq` does it).
      Proxy entries: a router's own go with its namespace (`ConvergeRouter` to absent removes those its leg routes); an
      entry whose namespace went out of band has no route left to tie it to a router, so the pass removes an entry no
      `lg-` leg routes when a record makes its address Lightstack's — a floating IP's, a gateway port's, or one in an
      external network's IPv4 pools (`RemoveStrayProxies`, `TestRemoveStrayProxies`,
      `TestReconcileNetworkNodeRemovesStrayProxies`). On every node, not only the network node: a former network node
      would otherwise keep answering for the addresses (FR-033). On the network node it runs before the routers
      converge, so a router that kept a stray entry it was just given gets it back in the same pass.*
- [X] T143 [P] `internal/neutron/reconcile_retired_test.go` (new): the retired state of FR-044 is removed where
      recognised — `LS-NSI-*` chains and jumps, marked host floating-IP NAT, `/32` uplink addresses of floating IPs with
      a record, bridge addresses, per-bridge metadata DNAT, host `dnsmasq`, `LS-SG-*` chains — and nothing unmarked
      without a record. *Done 2026-11-23. What needs no record is recognised on the node, `pkg/networking/retired.go`
      (`RetiredState.Remove`, `TestRemoveRetired_*` with the fake runner and a fake `/proc`): dnsmasq in Lightstack's
      own network namespace on a configuration in its DHCP directory (one in a `qdhcp-` namespace is left), every
      address on a `br-<8>` bridge but the IPv6 link-local, `lightstack-fip`-marked DNAT, the per-bridge metadata DNAT,
      the `LS-NSI-`/`LS-SG-` chains with every jump to them. With a record: a `/32` floating address on the uplink, an
      unmarked DNAT of R23's shape, and — from data-model.md's retired list, not T143's — the egress `MASQUERADE` of a
      subnet out the uplink; also Neutron's `tap-<8>` TAPs. Everything else stays, unmarked rules without a record
      included. The pass (`internal/neutron/reconcile_retired_test.go`) hands every node every floating IP's address and
      every subnet's CIDR; a failed read hands fewer. `MetadataRedirect` went (its rule parser stays);
      `TestStaleMetadataRules` and `TestMetadataRedirect_StubModeTouchesNothing` went with it.*
- [X] T144 [P] `internal/neutron/vxlan_coordinator_test.go` (new): FDB entries whose row is gone are removed; a deleted
      network's VXLAN device is removed (`DeleteVXLAN` gains its caller). *Done 2026-11-23. The coordinator reads
      through seams (`vxlanDataplane`, `activeNodes`, `readNetworks`, `readRows`) and converges both ways: an entry it
      wrote (`ListFDBEntries` now lists only permanent entries; learned ones age out) that no row of an active node
      holds is removed, the inactive node's included, and a device of no VXLAN network goes — neither after a partial or
      failed read. A deleted network's id survives nowhere, its VNI and forwarding rows cascading with it, so the device
      goes by its name: `DeleteVXLANDevice`, which `DeleteVXLAN` now calls. Found, not fixed (pre-existing, outside the
      plan, blocks T167): a VXLAN device has no flooding destination — no `00:00:00:00:00:00` entry per remote node, no
      group — so broadcast never crosses nodes. Round 1 never needed it, every node serving DHCP and holding the gateway
      itself; with DHCP and the routers on the network node only, a guest on another node gets no lease and cannot reach
      its gateway. Also open: the DHCP port's forwarding row names the node that served the request, not the network
      node (T123's note).*
- [X] T145 [P] `internal/neutron/teardown_test.go` and `internal/teardown/teardown_test.go` (extend), R48: the purge
      converges every router and DHCP unit to absent and removes every `ls-` port chain and dispatch on the node, with a
      fake runner; the database-less fallback removes `qrouter-*` and `qdhcp-*` namespaces and `ls-` chains by prefix;
      it never touches what it does not recognise. *Done 2026-11-23. The purge's test runs a real-mode service on fakes:
      every router and network DHCP the records name or the node holds converges to absent, the whole port firewall goes
      once (`RemoveAll`), the retired state once, then every bridge, records' and listed, and the project namespaces.
      `RemoveAll` (`TestRemoveAllChains`) removes every port chain, the fixed chains, `ls-ct`, `ls-l3-fwd` and the
      retired chains with every jump to them, both families, one restore each, and leaves a chain it does not recognise.
      The fallback (`prefixPurge`, `TestPrefixPurge_*`) recognises `light-ns-<any>`, and `qrouter-`/`qdhcp-` followed by
      a lower-case UUID, converging the latter to absent so their proxy entries, host routes and dnsmasq go too; a
      failure fails the purge and the rest still goes.*
- [X] T146 [P] Rework `internal/neutron/reconcile_dataplane_test.go`: its floating-IP cases become router-unit cases in
      T142; delete what no longer applies, and note it. *Done 2026-11-23. Nothing left to rework:
      `reconcile_dataplane_test.go` was deleted with the filters in T130, and its floating-IP cases live on as
      router-unit cases (`TestReconcileNetworkNodeConvergesEveryUnitWithARecord`).*

### Implementation

- [X] T147 `internal/neutron/reconciler.go`: roles (network node or not), the units' passes with the complete-read rule,
      the retired-state removal, and the FDB/VXLAN removal in `internal/neutron/vxlan_coordinator.go`. `reconcileNAT`,
      `reconcileFloatingIPs`, `removeOrphanFilter`, `reconcileSubnets`' host DHCP and the egress SNAT pass go. *Done
      2026-11-23. `reconcileNAT`, `reconcileFloatingIPs`, `removeOrphanFilter`, the host DHCP and the egress SNAT pass
      had gone with T118, T123 and T132. A pass: bridges; the retired state; on the network node DHCP, the stray proxy
      entries, then the routers; on any other node the routers and DHCP it holds, then the stray entries; the bound
      ports; the grandfathered gateways' report. Every removal by "no record" lists the node first and needs a complete
      read (`readIDs`). Each unit converges within the node call's deadline, as before. The coordinator's pass lists its
      devices before it reads.*

- [X] T148 `internal/neutron/teardown.go` (`PurgeRealModeNetworkState`) and `internal/teardown/teardown.go` (the
      fallback), R48: purge the new layout as T145 pins it. *Done 2026-11-23. `PurgeRealModeNetworkState`: the port
      firewall whole, the retired state, then DHCP and routers by records and by listing, the bridges by records and
      listing, the project namespaces. Teardown leaves the VXLAN devices, as it did before (pre-existing; teardown makes
      no VXLAN manager). `DeleteRouterNamespace` went: the fallback converges instead.*

---

## Phase 24: The node — namespaces across restarts, bridge netfilter, deployment (FR-036, FR-046; R35)

### Tests (write first, confirm RED)

- [X] T149 [P] `pkg/networking/nodesettings_test.go` (new): the check runs `modprobe br_netfilter` through the runner,
      writes `1` to both `/proc/sys/net/bridge/bridge-nf-call-*` files under a test root, reads them back, and reports
      which failed; stub mode runs nothing. *Done 2026-11-23. `NodeSettings`: `Ensure` at start, `Check` for readiness,
      which reads the two settings live so readiness follows them. A failed `modprobe` counts only while the settings do
      not hold: a module built in, or loaded by the host, needs none. Also pinned: a setting the kernel refuses is named
      alone; one turned off later fails the check.*
- [X] T150 [P] `internal/health/health_test.go` (extend): a `bridge-netfilter` check failing makes `/readyz` `503`
      naming it; passing makes it ready. *Done 2026-11-23. `TestAddCheck_GatesReadiness`: `503` naming
      `bridge-netfilter`, never its error text, as the checker's other reasons; `200` once it holds.*
- [X] T151 [P] Chart tests `charts/lightstack/tests/netns_mount_test.yaml` (new) and `teardown_test.yaml` (extend): in
      real mode the pod and the teardown job mount `hostPath` `/run/netns` at `/run/netns` with `mountPropagation:
      Bidirectional` and `/lib/modules` read-only; stub mode mounts neither. *Done 2026-11-23. `netns_mount_test.yaml`
      (the Deployment, the multi-node DaemonSet, stub mode) and two purge-Job cases in `teardown_test.yaml`.
      `/lib/modules` is `type: Directory`, so a host without it is named at the pod's start rather than given an empty
      directory. Bidirectional propagation is allowed to privileged containers only, and `realMode.privileged` defaults
      to `false`, so real mode without it now fails to render (`netns_mount_test.yaml`'s last case) instead of producing
      a pod the API server refuses; the suites that enable real mode set `realMode.privileged: true` beside it (44
      places in eight files). `test/compose_kvm_state_test.sh` pins the Compose mounts with their propagation.*

### Implementation

- [X] T152 `pkg/networking/nodesettings.go` (new) and `cmd/lightstack/main.go`: the start-up check after
      `neutron.NewService` in real mode, its result as a `health.Check` (`internal/health/health.go` gains `AddCheck` or
      the checker moves after the check). *Done 2026-11-23. `pkg/networking/nodesettings.go`; `health.Checker.AddCheck`,
      its checks now guarded by a mutex since `Ready` may run meanwhile. `main.go` runs the check after
      `neutron.NewService` in real mode and adds it before the reconciler starts.*
- [X] T153 [P] `charts/lightstack/templates/_helpers.tpl` (pod spec volumes and mounts),
      `templates/teardown-purge-job.yaml`, and `deployments/docker-compose-kvm.yml` (`/run/netns:/run/netns:rshared`,
      `/lib/modules:/lib/modules:ro`). *Done 2026-11-23. The pod spec and the purge Job mount `host-netns`
      (`/run/netns`, `DirectoryOrCreate`, `Bidirectional`) and `kernel-modules` (`/lib/modules`, read-only) in real
      mode; the chart's gates refuse real mode without `realMode.privileged` (a values change for UPGRADING.md, T154).
      Compose: `/run/netns:/run/netns:rshared` and `/lib/modules:/lib/modules:ro`. Podman, unlike Docker, does not
      create a missing bind source: the runbook makes `/run/netns` first (T165).*

---

## Phase 25: Round 2 polish

- [X] T154 [P] `docs/UPGRADING.md`: a section for round 2 — the clean break (run `lightstack cleanup` and start fresh;
      no conversion), and a before/after table of every answer FR-020's round-2 bullets list (FR-019a–b, FR-024–FR-058,
      administrators' listings, shows and writes included). *Done 2026-11-23. Round 1's table now names the ports'
      chains where it named the floating-IP filters, and its failure row covers every real-mode change, deletions
      included. New: *The Networking API* (every round-2 answer, before and after, each checked against `d81f081`), *The
      real-mode layout: a clean break* (the layout, `lightstack cleanup` first, what reconciliation removes of the
      earlier layout and the command for what it leaves, the underlay note), *Kubernetes and Compose*
      (`realMode.privileged` now required, the mounts, Podman's missing bind source, `neutron.network_node`).*
- [X] T155 [P] `docs/EBPF_STATUS.md`: the port chains enforce in both modes; Neutron's TAP is gone, so eBPF's attach
      point is too; the decision is the user's. *Done 2026-11-23. Status: not wired; `ebpf` mode enforces through the
      ports' chains. Nothing calls `NewSecurityGroupManager` any more; the decision is left open, as asked.*
- [X] T156 [P] `docs/NETWORKING_MODES.md`, `docs/VXLAN_IMPLEMENTATION.md`: the network node, namespaces, the port
      firewall, the node call. *Done 2026-11-23. `NETWORKING_MODES.md`: the architecture, iptables mode's mechanics and
      eBPF mode's, the node call, what a node needs. `VXLAN_IMPLEMENTATION.md`: a section on the network node with the
      flooding gap of T144's note, `network_node` in the configuration and both nodes' examples (node 2's example had
      two `neutron:` keys; merged), the forwarding rows written in the port's transaction and removed both ways.*
- [X] T157 [P] `docs/CONFIGURATION.md`: `neutron.network_node`; `docs/TROUBLESHOOTING.md`: remove the manual
      `/var/run/netns` step; `docs/SCALING.md`: the bridge sysctls are set by Lightstack; FR-035's underlay note (allow
      the floating range where the underlay delivers only assigned addresses). *Done 2026-11-23. `CONFIGURATION.md`:
      `network_node`, the network node, the modes. `TROUBLESHOOTING.md`: the manual `/var/run/netns` step and the
      `light-ns-` commands went; namespaces lost at a restart and `bridge-netfilter unavailable` are issues of their
      own. `SCALING.md`: the network node and the underlay note; the bridge sysctls left `sysctl.conf`, as Lightstack
      sets them.*
- [X] T158 [P] `charts/lightstack/README.md`: the mounts, multi-node's network node, the underlay note. *Done
      2026-11-23. `neutron.networkNode`, `realMode.privileged` required, the mounts, the underlay note, the network node
      in multi-node.*
- [X] T159 [P] `docs/specs/033-sg-port-membership/contracts/network-api.md` and `server-report.md`: point to
      `network-api-round2.md` where round 2 changes them. (`compute-security-groups.md` was restated during the round-2
      analysis.) *Done 2026-11-23. `network-api.md`'s round-2 note lists every contract that holds over it — the
      filters' retirement, the router namespace and the node call included; `server-report.md` notes repeated names.*
- [X] T160 Re-run T058's gates and append "Round 2 results" to `baseline-gates.md`, compared with T058 (build, lint,
      vet, gofmt, unit on both backends, contract on both backends, `helm unittest charts/lightstack`,
      `./test/security_groups_test.sh` with Test 11 passing). *Done 2026-11-23. Green on the recreated database,
      `baseline-gates.md` (Round 2 results): unit 2538 and 2499 passed, contract 419 and 392, 25 helm suites,
      `security_groups_test.sh` 13 of 13 with Test 11 passing.*
- [X] T161 Mutation-check round 2 off the worktree (`go test -overlay` or a scratch clone) and record it in
      `baseline-gates.md`: each listing filter ignored (T071); each `default` refusal removed (T078); the ambiguity
      check removed (T082); the migrator's foreign-key check removed (T061); starting rules not inserted (T079); pools
      ignored by the allocator, the gateway or floating IPs not excluded, each validation removed (T086, T087); the
      allocation lock removed (T097); the router lookup's floating-network check removed, the gateway-interface
      preference removed, FR-029's check removed (T098); gateway port or one-transaction interface reverted (T093);
      FR-039's checks removed (T094); the node token's audience, expiry, node or digest check removed (T108); the router
      unit's add or remove order swapped, the SNAT exception or hairpin rule dropped (T112, T113); the DHCP static range
      made dynamic, option 121's metadata route dropped (T119); metadata identity by address only (T121); the spoof
      chain, `INVALID` drop, remote-group expansion or zone rules dropped (T125); the TAP name dropped (T128); a
      deletion's record deleted before its units (T135); the re-apply on failure removed (T135); the complete-read rule
      removed from a removal (T142); `br_netfilter` failure not failing readiness (T150); the administrator branch of
      `projectScope` removed, and a show's administrator bypass removed (T102); a write's administrator target removed
      (T104); an administrator's foreign group id refused again (T105). *Done 2026-11-23. 57 reversions plus Phase 23's
      19, all caught, recorded in `baseline-gates.md` (Round 2 mutation check). Two pins were missing and were added: a
      node token without `exp` (or `iat`) refused, and a port deletion whose final transaction fails re-applying its
      units (`failingStatement` in `deletion_order_test.go`).*
- [X] T162 [P] Update `docs/specs/033-sg-port-membership/follow-ups.md` only if round 2 leaves something out; otherwise
      leave it as it is. *Done 2026-11-23. Round 2 leaves eight findings out; `follow-ups.md` §5 drafts them, VXLAN's
      missing flooding first, as it blocks multi-node.*
- [X] T163 [P] The ticket's checklist (cloud-storage#579): draft the round-2 items in
      `docs/specs/033-sg-port-membership/ticket-checklist-round2.md` for the user to post or approve; do not post. *Done
      2026-11-23. `ticket-checklist-round2.md`: round 2's acceptance criteria and tasks to append to the `## Checklist`
      comment, ticked where implemented and tested, open where the host runs are needed. Round 1's items keep their
      wording, since the weekly summaries match items by text. Not posted.*
- [X] T164 [P] Update `CLAUDE.md`'s Neutron real-mode paragraph and its Project Isolation bullet ("Listings never widen
      implicitly" is Compute's `all_tenants` rule; Networking API listings show administrators every project, FR-057)
      ("Project Isolation" and "Neutron real mode") for the network node, namespaces and the port firewall, replacing
      statements the layout change makes false. *Done 2026-11-23. Project Isolation: the network node, the namespaces,
      the port firewall, the TAP as libvirt's; Compute's `all_tenants` rule told apart from the Networking API's
      administrators' listings. Neutron real mode: the requirements, the command runner, eBPF not wired. Also removed:
      `TAPDeviceManager` (no caller since Neutron's TAP went) with its two tests and the docs that named it, and
      `github.com/coreos/go-iptables` from `go.mod` (`go mod tidy`; nothing imports it since round 1's group chains
      went).*
- [X] T165 Generate the round-2 host runbook `docs/specs/033-sg-port-membership/HOST_TESTING.md` with `/cl.hosttest
      docs/specs/033-sg-port-membership/`: the single-node run per quickstart §5 and research R45, the chart on one
      Kubernetes node, opening with FR-023's checks as restated in §5 (one connection attempt per second included), with
      read-only prerequisites, setup, steps with desired outcomes, and teardown; zsh-paste-safe. Replaces T053. *Written
      2026-11-23 and re-grounded the same day: `HOST_TESTING.md`, 22 blocks, all `zsh -n` clean, no paste traps; shells
      T1–T3; the chart on k3s with Horizon, floating IPs from an unused pool on the host's own LAN, 16 test steps
      (2.1–2.16). A two-node run was written too, then dropped with T167.*
- [X] T166 Run `HOST_TESTING.md` on a Kubernetes node, its Horizon *Edit Security Groups* step included, and record each
      step's outcome. Replaces T054. *Done 2026-11-23 on an openSUSE Tumbleweed VM (k3s v1.37.2, nested KVM, modular
      libvirt 12.9 with `virtproxyd.socket`; floating pool `192.168.122.240`–`249` on the VM's LAN): §0 to §3 passed,
      every step 2.1–2.16 as its Expect line. SC-010: 843 attempts, 838 `timeout`, 5 `unreachable` (2.10's window), none
      open or refused. FR-010: both planted rules gone within one pass. FR-036: namespaces kept across the pod restart,
      the ssh stream unbroken. 2.15 ran on `ls579-vm2`: exact pre-selection, one `PUT` on its port, 8080 closed at once.
      A fresh pod's reconciliation also pruned an earlier attempt's bridges (FR-010). The outcomes are recorded here:
      `/cl.hosttest` keeps runbooks free of results.*
- [ ] T167 **Dropped 2026-11-23 (user): two nodes are not supported yet, so a two-node host run is not useful; it waits
      for multi-node support.** Was: a run on two hosts, recorded in the host-testing runbook.
- [X] T168 FR-021: once the runbook's Horizon step (2.15) has passed, record instance *Edit Security Groups* as verified
      in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, with the date. Replaces T055. *Done 2026-11-23: workflow
      7 in the report's results table, and a dated update note.*

## Phase 26: Branch review (`2026-11-23-review.md`)

The review's findings, one task per finding the user accepts, in the order they are discussed: by severity, then the
review's order. Each: tests first with RED confirmed, then the fix, mutation-checked off the worktree.

- [X] T169 [SRC-1] FR-059: a rule's `remote_ip_prefix` and `protocol` were stored as sent and written into the port
      firewall's `iptables-restore` input, so a newline wrote commands that ran as root on every node binding a port
      with the group. `CreateSecurityGroupRule` checks and normalises a rule as upstream does
      (`internal/neutron/security_group_rule_check.go`); `pkg/networking/port_firewall.go` writes a rule only as it
      parses it, protocols by upstream's number (`pkg/networking/protocols.go`), and leaves out what it cannot express
      exactly. Tests: `internal/neutron/security_group_rule_check_test.go`, `pkg/networking/port_firewall_rule_test.go`.
      *Done 2026-11-24. RED: 24 of 24 refusals, 11 of 14 normalisations, 15 of 15 left-out rules and 6 of 10 protocol
      renderings failed. GREEN: every unit package on both backends (no contract test creates a rule); 28 of 28
      mutations caught (`go test -overlay`); lint clean. Port ranges included (Clarifications 2026-11-24). Found on the
      way: upstream's `ipip` is protocol 4, and iptables reads the name as 94; a name now renders as upstream's number
      for it.*

- [X] T170 [SRC-2] A router's ports were found by `device_owner` and `device_id`, which any member sets on a port of
      their own. A port claiming to be a router's gateway became it: the network node routed its address to the router
      and answered ARP for it on the uplink, whatever address the member chose (the LAN's own gateway, say); the API
      reported it as the router's gateway, at an address only an administrator may choose (FR-038), and clearing the
      gateway deleted it. A floating IP found its router through ports claiming an interface or a gateway.
      `gatewayPortsOf` now reads only the port the router code writes, the one with no project
      (`internal/neutron/router_gateway.go`); `routerForFloatingIP` reads interfaces from `router_interfaces` and the
      gateway the same way (`internal/neutron/floatingip_association.go`). Tests:
      `internal/neutron/router_forged_ports_test.go`, `internal/neutron/router_unit_forged_test.go`. *Done 2026-11-24.
      RED: every new test failed for its reason. GREEN: every unit package on both backends; 4 of 4 mutations caught
      (`go test -overlay`), one of them only after a case was added: a router with a real interface elsewhere. The
      review's two other claims did not hold: a router owner does not unlock a subnet's gateway address (`routerPort`
      admits only an IPv6 subnet's anycast address), and the interface legs already came from `router_interfaces`.
      Refusing `network:*` owners to members stays SRC-7's. Duplicate rules (found with T169) went to `follow-ups.md`
      §6.*

- [X] T171 [SRC-3] Adding a router interface took any subnet the caller can read, a shared network's included, and gave
      the router's port the subnet's gateway address whoever held it; on the network node a member's router then
      answered for another project's gateway, or became it. FR-055 as amended: in `AddRouterInterface`'s transaction,
      `checkInterfaceSubnet` (`internal/neutron/router_interface_check.go`) makes upstream's checks in upstream's order
      — a gateway; for a non-administrator, a subnet of their own project (400, upstream's message); no port of the
      router on the subnet, its gateway's included, and no interface's subnet overlapping it, both read from the
      router's records (400); and, under the subnet's lock, no port holding the gateway address (409). `usedAddresses`'
      port scan became `portAddresses` (`internal/neutron/allocation.go`). Tests:
      `internal/neutron/router_interface_refusals_test.go`. *Done 2026-11-24. RED: the five new cases failed (four adds
      answered 200; a member's port claiming an interface blocked the router's own add); a sixth case, added when its
      mutation survived, pins the existing refusal of a subnet the router already has. GREEN: every unit package on both
      backends (the first PostgreSQL run hit the seeded project's known `/servers/detail` race in `internal/nova`, green
      alone and on a re-run); 7 of 7 mutations caught (`go test -overlay`), the allocator's lost port scan among them.
      Lock order: the router, then the subnet, as every router and floating-IP path already takes them. The overlap
      refusal was not in the review (Clarifications 2026-11-24).*

- [X] T172 [SRC-4] A subnet's `dns_nameservers` were stored as sent and written into the dnsmasq configuration of the
      network's DHCP, which starts as root on the network node: a newline started a directive of the subnet owner's
      choosing (`dhcp-leasefile=`, `conf-file=`, `dhcp-script=`). Pre-existing: `main`'s template had the same flaw.
      FR-060: `CreateSubnet` checks them as upstream does before anything is read (`internal/neutron/subnet_dns.go`);
      `RenderDNSMasq` writes a gateway or DNS server only as an IPv4 address and logs what it leaves out
      (`pkg/networking/dhcp_namespace.go`). Tests: `internal/neutron/subnet_dns_test.go`,
      `TestRenderDNSMasqWritesOnlyAddresses`. *Done 2026-11-24. RED: the eight refusals answered 201, and the renderer
      wrote both injected directives. GREEN: every unit package on both backends; 8 of 8 mutations caught (`go test
      -overlay`). Upstream's dot-count check is left out: it guards netaddr's short IPv4 forms, which netip refuses
      already. Two gaps went to `follow-ups.md` §6: subnet update ignores `dns_nameservers`, and a subnet overlapping
      another of its network is accepted.*

- [X] T173 [SRC-5] A guest revived by `os-start`'s self-heal got interfaces without a named TAP (`storedNetworkConfigs`
      left `Device` empty), so libvirt named them `vnetN`, which neither a port's chains nor the `tap+` drop match: the
      guest's traffic went unfiltered to the host's FORWARD chain. The revival did not bind the ports either. FR-041
      restored: `storedNetworkConfigs` names each TAP for its port, as boot does (`internal/nova/selfheal.go`);
      `GenerateVMXML` writes a `<target dev>` for every interface, its port's TAP when none is named
      (`pkg/hypervisor/xml_template.go`); `reviveGuest` binds the guest's ports before the domain is made, and a port
      that cannot be bound refuses the start (`409`) as any self-heal that cannot complete. Tests:
      `TestGenerateVMXMLNamesEveryInterfacesTAP`, `TestStoredNetworkConfigsRebuildsEachInterface` (the name),
      `TestSelfHealBindsTheGuestsPorts`. *Done 2026-11-24. RED: all three failed (no target, no name, no binding, a
      failed binding answered 202). GREEN: every unit package on both backends; 4 of 4 mutations caught (`go test
      -overlay`). Both safeguards at the user's choice (Clarifications 2026-11-24).*

- [X] T174 [SRC-6] A member read every project's ports on a shared network — names, MACs, addresses, the servers they
      belong to and their security groups — through the port listing, its marker and a show, which share
      `portReadScope`. Pre-existing: `main` admitted `n.shared = true`; round 2 only took gateway ports out. FR-061:
      `portReadScope` is upstream's `get_port`, the port's project or the network's (`internal/neutron/ports.go`).
      Tests: `internal/neutron/port_read_scope_test.go`; `TestRouterGatewayPort`'s visibility case now puts the gateway
      on an external network of another project, as an administrator's is, and pins that its owner sees the port. *Done
      2026-11-24. RED: the member listed and showed the other projects' ports, and paged from one. GREEN: every unit
      package on both backends; 2 of 2 mutations caught (`go test -overlay`).*

- [X] T175 [MD-1] `docs/UPGRADING.md` told operators to reset each node with `lightstack cleanup`, "which removes its
      guests and its host networking". It also removes every storage tree — images, volumes, instance disks, cloud-init
      and DHCP files, the console directories — and, with a SQLite `database.url`, the database file; and it misses the
      earlier layout's router namespaces, `qrouter-<id[:11]>`, which neither it nor reconciliation recognises. The
      section now says what the reset removes, asks for a backup first (`lightstack-migrate backup` for SQLite, whose
      default target survives the reset; `pg_dump`; image and volume data copied off by hand), has PostgreSQL's records
      deleted through the earlier release's API first, offers `--dry-run`, and gives the command for the old namespaces.
      *Done 2026-11-24. Docs only; the command's filter tried on sample `ip netns list` output (only the 11-character
      names match) and `zsh -n` clean.*

- [X] T176 [HELM-1] A multi-node release rendered `vxlan_enabled: true` (the default) and `network_node: ""` (the
      default), which Lightstack refuses at start, before anything else (FR-033): every pod crash-looped.
      `lightstack.validate` now refuses that render, last of its checks, naming `neutron.networkNode` and what to put in
      it (`charts/lightstack/templates/_helpers.tpl`); the multi-node suites name a network node, `multinode_test.yaml`
      pins the refusal and the render with VXLAN off. `values.yaml`, the chart README and `docs/UPGRADING.md` say so;
      the UPGRADING bullet now names the chart's key beside the configuration's (MD-11's first half). *Done 2026-11-24.
      RED: the render did not fail. GREEN: `helm unittest charts/lightstack`, 25 suites, 179 tests; 3 of 3 mutations on
      a scratch copy of the chart caught (no guard, no `trim`, VXLAN ignored). The refusal is tested through
      `templates/service.yaml`, the one template that includes `lightstack.validate`.*

- [X] T177 [HELM-2] `realMode.privileged` had one accepted value: `privileged:` renders only in real mode, and real mode
      refused to render unless it was `true` (T153), so the key was a second flag every real-mode install had to set. It
      is gone: real mode renders `privileged: true` for the workload and the purge Job (`_helpers.tpl`,
      `teardown-purge-job.yaml`), as research R35 assumed; the check, the value, its schema entry and 47 test lines
      went, and `ci/run-tests.sh`, the chart README, `docs/UPGRADING.md` and this runbook (`HOST_TESTING.md`'s `helm`
      command and its troubleshooting entry) no longer set or mention it. A values file that still sets it is refused by
      the schema, a clean break (`additional properties 'privileged' not allowed`). *Done 2026-11-24. RED: real mode
      without the key failed to render, and a values file with it rendered. GREEN: `helm unittest charts/lightstack`, 25
      suites, 181 tests; `ci/run-tests.sh`; 3 of 3 mutations on scratch copies of the chart caught. Earlier features'
      runbooks still pass `--set realMode.privileged=true`; they are those features' records, and UPGRADING names the
      break.*

- [X] T178 [HELM-3] The chart README's floating-IP section, `NOTES.txt`, the README's row, `values.yaml` and the schema
      still described the earlier layout: DNAT on the VM's node, the pool routed to every node, and a MetalLB recipe
      announcing it from every compute node, which competes with the network node's ARP answers. They now say the
      network node translates the floating addresses and answers for them on its uplink, the pool goes to that node only
      (on a LAN nothing more; behind an underlay, a route or an allowance there), nothing else announces it, and
      single-node is verified while multi-node is not supported yet. `notes_test.yaml` pins the note. *Done 2026-11-24.
      RED: the note still sent the pool to every node. GREEN: `helm unittest charts/lightstack`, 25 suites, 182 tests;
      both notes rendered by a client dry run.*

- [X] T179 [SRC-7] Port create and update stored any `device_owner` a member sent, and `dhcpPortOf` took the oldest
      `network:dhcp` port whose device was the network, of any project: on a shared network a member could plant the
      network's DHCP port. FR-062: `networkDeviceOwnerRefusal` answers upstream's `403 PolicyNotAuthorized` to a
      `network:` owner from anyone but an administrator or the network's project, on create and on update
      (`internal/neutron/ports.go`); `dhcpPortOf` reads only the network's project's ports
      (`internal/neutron/dhcp_unit.go`). Tests: `port_device_owner_test.go`, `dhcp_port_owner_test.go`. *Done
      2026-11-24. RED: the member's create and update stood, and the planted port was the network's DHCP port. GREEN:
      every unit package on both backends; 5 of 5 mutations caught (`go test -overlay`).*

- [X] T180 [SRC-8] Nova found a server's ports by `device_id` alone, which any port creator sets: another project's port
      naming a server was changed by the server's group actions, could veto them (`409`, a stateless group beside a
      stateful one), showed in its reported groups, and was withdrawn, unbound, detached or deleted with it, its
      floating IP taken off. FR-063: the group change locks the ports naming the server in its project (`lockPorts`
      takes the query's arguments), the report joins through the instance as the address map does
      (`internal/nova/advanced_actions.go`), and the deletion's withdrawal, lock, detach and delete read the same ports
      (`internal/nova/handlers.go`); the deletion path was not in the review. Tests: `server_ports_project_test.go`,
      `server_delete_project_ports_test.go`. *Done 2026-11-24. RED: the add answered 409, the other project's
      `compute:nova` port was deleted and its plain one detached. GREEN: every unit package on both backends; 6 of 6
      mutations caught (`go test -overlay`), one per query.*

- [X] T181 [SRC-9] A boot whose ports could not be bound recorded `"failed to bind the server's ports: " + err.Error()`
      as the server's fault, which the tenant reads: a node's name and URL, up to 512 bytes of its answer, iptables'
      output. The fault is now `failed to bind the server's ports`; the error stays in the log line above, as
      `allocationFault` keeps a host's error out, after upstream's `exception_to_dict` (`internal/nova/handlers.go`).
      `TestBootFailsWhenItsPortCannotBeBound` binds with a node's own words and pins the fault. *Done 2026-11-24. RED:
      the fault carried the node, its URL and iptables' output. GREEN: `internal/nova` on both backends, lint clean.
      Every other fault a boot records is fixed text or `allocationFault`'s.*

- [X] T182 [SRC-10] The host's own metadata listener (`:8775`, every interface by default) still named a caller by its
      address, in any network, though round 2 moved guests to their network's namespace: a host or pod holding some
      guest's fixed IP read its metadata and `user_data`. FR-064: `instanceFromRequest` names a guest by address only on
      the network its request came through, and `instanceHolding` takes that network (`internal/metadata/service.go`);
      the configuration's two comments, which still asked for a wide bind for the retired per-namespace DNAT, say what
      the listener serves. The identity tests that called the host listener as guests now ask through their network's
      namespace; `host_listener_test.go` pins the host listener. *Done 2026-11-24. RED: the host listener served the
      guest's instance id, `user_data` and `meta_data.json` to its address. GREEN: every unit package on both backends
      (the first PostgreSQL run hit `internal/neutron`'s `TestSubnetReferenceIntegrity`, whose administrator's listings
      race other packages on the shared database; green alone and on a re-run). Every bash and contract caller of
      `:8775` names its instance by `X-Instance-ID`.*

- [X] T183 [SRC-11] Any member could choose a floating IP's address and set a router gateway's `enable_snat`: with SNAT
      off, the router's subnets left the uplink under their own addresses. FR-054 and FR-038, upstream's policy: a
      `floating_ip_address` from a caller holding neither `admin` nor `manager` answers `403 PolicyNotAuthorized`, a
      `null` one choosing none (`internal/neutron/floatingip.go`); `enable_snat` from a non-administrator does, on
      create and update and whatever its value, beside `external_fixed_ips` (`parseGatewayRequest`,
      `internal/neutron/router_gateway.go`). `common.RoleManager` names the role, which nothing seeds
      (`internal/common/roles.go`). Tests: `attribute_policy_test.go`; the three tests that set either as a member now
      do so as a manager or an administrator. *Done 2026-11-24. RED: the member's address and both `enable_snat` values
      were stored, on create and update. GREEN: every unit package on both backends (the PostgreSQL run hit
      `TestSubnetReferenceIntegrity` again; green alone), lint clean; 7 of 7 mutations caught (`go test -overlay`).*

- [X] T184 [SRC-12] The port API deleted a router's interface port, a member's own included, and an administrator its
      gateway port: the interface's record went with the port, the floating IPs reaching through it kept a router
      without a path, and the network node kept the interface until its next pass. Deleting a port forwarding's internal
      port, a server's included, left the forwarding's DNAT on the router until that pass, and an interface a forwarding
      reached through could be removed. FR-065: `DeletePort` answers upstream's `409 ServicePortInUse` for a port the
      router's records hold, administrators included, before anything is withdrawn (`routerPortOwner`,
      `internal/neutron/ports.go`). FR-040: the withdrawal takes the ports' port forwardings off their routers with
      their floating IPs (`readPortWithdrawal`, `internal/neutron/port_unit.go`). FR-039: `requireInterfaceUnused`
      refuses for a forwarding's internal address as for a floating IP's fixed IP, each inside the subnet and on a port
      of its network, upstream's two conditions (`internal/neutron/router_gateway.go`). Tests:
      `router_port_delete_test.go`; `router_unit_test.go` (`TestRouterUnitPortDeleteTakesItsPortForwardings`,
      `TestRouterUnitRefusedPortDeleteConvergesNothing`); `router_floating_ip_refusals_test.go` (the interface a port
      forwarding reaches through). *Done 2026-11-24. RED: the member's and the administrator's deletes answered 204, the
      port's delete never converged the router, and the interface was removed. GREEN: every unit package on both
      backends (the PostgreSQL run hit `TestAdministratorListsEveryProject`: its unfiltered port listing read a port
      another package's test had inserted without a MAC, which `ListPorts` cannot scan; green alone, twice), lint clean;
      10 of 10 mutations caught (`go test -overlay`).*

- [X] T185 [SRC-13] The auto-allocated topology's delete checked that nothing used its network once, unlocked, then
      removed the network's DHCP through a node call of up to a second and deleted the network unconditionally: a port
      created in that second, its creator already answered `201`, went with the network. FR-040's sequence is now one
      function, `removeNetwork` (`internal/neutron/network.go`): refused while in use; the DHCP leaves the network node;
      the record goes in a transaction that locks it and checks again; a failure or refusal puts the DHCP back; then the
      host's bridge and the cached views go. `DeleteNetwork` and `DeleteAutoAllocatedTopology` both call it, so the
      topology's delete also evicts the cache, which it never did. The handlers' refusal-or-500 answer, copied into five
      helpers and six handlers, is one function, `sendChangeError` (`internal/neutron/change_error.go`); the helpers
      that map errors of their own (`sendFloatingIPStoreError`, `sendRuleStoreError`, `sendPortChangeError`, which
      replaces `sendGroupLookupError`) end in it, and `sendRouterError` and `sendPortForwardingError` are gone. Messages
      are unchanged. Test: `network_delete_race_test.go`, a port created while the DHCP is removed, through both routes.
      *Done 2026-11-24. RED: the topology's delete answered 204, the port went with the network and its DHCP was not put
      back. GREEN: every unit package on both backends, lint clean; 3 of 3 mutations caught (`go test -overlay`).*

- [X] T186 [SRC-14] Deleting a subnet read only the network's ports, and a Lightstack floating IP is a row of its own:
      an external subnet whose addresses only floating IPs held deleted with `204`, leaving them holding addresses of no
      subnet. FR-052: `subnetInUse` also reads the network's floating IPs and tests each address against the subnet's
      prefix, as upstream counts its `network:floatingip` ports (`internal/neutron/allocation.go`); the delete's check,
      its locked re-check and the create's undo all read it. Test: `subnet_floating_in_use_test.go`. *Done 2026-11-24.
      RED: the subnet deleted with 204. GREEN: every unit package on both backends, lint clean; 3 of 3 mutations caught
      (`go test -overlay`).*

- [X] T187 [SRC-15] A floating IP's router was chosen from records read unlocked and then locked, and never chosen
      again: an interface removal or gateway clear holding the lock, which passed FR-039's check before the floating IP
      existed, committed in between, and the create stored a floating IP through a router that no longer joined its
      subnet. FR-028: `routerForFloatingIP` chooses again under the lock and answers 404 when the choice is no longer
      the locked router; the choice is `selectRouterForFloatingIP` and the answer `errExternalGatewayNotFound`
      (`internal/neutron/floatingip_association.go`). Creates and associations both go through it. Test:
      `floating_ip_router_race_test.go`, PostgreSQL only: a transaction holds the router's lock while it removes the
      interface, the create is started, and the transaction commits once `pg_blocking_pids` shows the create waiting;
      with no other router, and with a second router now serving the subnet. *Done 2026-11-24. RED: the create answered
      201 through the router that had lost its interface. GREEN: every unit package on both backends, lint clean; its
      two PostgreSQL queries carry `//sqlcheck:postgres-only`; 2 of 2 mutations caught (`go test -overlay`), the
      comparison and the second choice each.*

- [X] T188 [SRC-16] Three undos put an address back without the allocator's locks or a holder check: a deleted floating
      IP's restore checked only other floating IPs, a router gateway's undo re-inserted the previous gateway port as it
      was, and a subnet update's undo restored the previous gateway. An address a port took while the node call failed
      ended with two holders, and a concurrent create could lose its insert to the unique index. FR-008:
      `requireAddressesFree` (`internal/neutron/allocation.go`) takes the locks of the subnets holding the addresses,
      the caller having taken the floating-IP allocation lock first as every allocation does, and answers
      `addressHeldError` when a port, a subnet's gateway or a floating IP holds one; `restoreFloatingIP`,
      `undoRouterGateway` and `undoSubnetUpdate` call it, the subnet update's undo found with the review's two. Test:
      `address_reclaim_test.go`, one case per undo, a port taking the address during the failing node call. *Done
      2026-11-24. RED: the floating IP was restored and the previous gateway port re-inserted beside the port (two
      holders), and the subnet's gateway went back to the port's address. GREEN: every unit package on both backends,
      lint clean; 4 of 4 mutations caught (`go test -overlay`).*

- [X] T189 [SRC-17] A subnet's gateway change converged only the network's DHCP. A router's default route is its gateway
      subnet's gateway, so the routers on an external subnet kept the previous next hop until the next pass, with no
      apply and no undo (Article II). FR-045: the update converges, before the answer, the network's DHCP and every
      router whose gateway port has an address on the subnet (`subnetGatewayRouters`,
      `internal/neutron/router_gateway.go`), as upstream's `_notify_subnet_gateway_ip_update`; when one cannot take it,
      the update is undone and the routers converge back (`internal/neutron/network.go`). Test:
      `TestRouterUnitFollowsItsGatewaySubnetsGateway` (`router_unit_test.go`): the next hop moves; with two routers and
      the second failing, the gateway and both routers' next hops are back. *Done 2026-11-24. RED: the router kept the
      previous next hop and the update answered 200 with a failing router. GREEN: every unit package on both backends,
      lint clean; 3 of 3 mutations caught (`go test -overlay`).*

- [X] T190 [SRC-18] Every reconciliation pass, on every node, removed the earlier layout's state by the records too: an
      unmarked `-s <cidr> -o <uplink> -j MASQUERADE` for any subnet's CIDR, unmarked DNAT and uplink `/32`s for any
      floating address. A tenant chooses a subnet's CIDR, and an operator's own masquerade has that shape, so a tenant's
      subnet deleted the operator's rule within a pass, on hosts that never ran the earlier layout too. FR-044: the
      reconciler removes the retired state once per process — again the next pass only when it failed — and hands the
      node no records, so only what a mark or a name makes Lightstack's goes (`removeRetired`,
      `internal/neutron/reconciler.go`); the records are `lightstack cleanup`'s, which the reset before this release
      runs (`pkg/networking/retired.go`'s comments say so). UPGRADING's reset section says which removes what, and lists
      the masquerade rules too. Tests: `TestReconcileRemovesTheRetiredStateOncePerProcess` (`reconcile_retired_test.go`,
      rewritten: once, no records, again after a failure); `TestRemoveRetired_WithoutRecordsOnlyMarksAndNames`
      (`pkg/networking/retired_test.go`). *Done 2026-11-24. RED: two removals in two passes, each handed every floating
      address and subnet CIDR. GREEN: every unit package on both backends, lint clean; 3 of 3 mutations caught (`go test
      -overlay`).*

- [X] T191 [SRC-19] A router namespace's sysctls and `lo` were written only when the namespace was new, and a leg's
      bridge and up state only when the leg was new: a failing command, or the unit's deadline on a large router after a
      reboot, left a router that never forwarded, or a leg on no bridge, for good. The DHCP namespace had the same, `lo`
      and its device's leg, found with the review's. FR-010: `readNamespace` reads a namespace as the node holds it, now
      with its devices' state and the host's, and `namespaceSteps` and `legSteps` write only what differs
      (`pkg/networking/namespace_held.go`); the router and the DHCP namespace both use them, and the router also reads
      its sysctls and connection tracking's liberal windows (`router_namespace.go`, `dhcp_namespace.go`). A whole unit
      is still only read. Tests: `TestConvergeRouter_FinishesWhatAPassCutShort`,
      `TestConvergeDHCP_FinishesWhatAPassCutShort`; the fake nodes answer the new reads. *Done 2026-11-24. RED: each
      earlier "only when new" condition, restored as a mutation, fails the tests: 6 of 6 caught (`go test -overlay`).
      GREEN: every unit package on both backends, lint clean.*

- [X] T192 [SRC-20] A port forwarding found its router only through its floating IP's `router_id`, which only a port
      association sets: a forwarding on a free floating IP, upstream's normal case, answered 400, and one on a
      port-associated floating IP worked. Upstream does the reverse (`pf_plugin`). FR-049: the create reads the floating
      IP, refuses one a port uses (`409 FloatingIPPortAlreadyAssociated`, the constructor now shared with FR-029's
      check) and an internal port with its own (`409 PortHasBindingFloatingIP`), finds the router as a floating IP's
      (`routerForFloatingIP`, a miss upstream's `400`), locks the floating IP after the router as an association does,
      and records the router on it; `releaseForwardingRouters` frees a floating IP with its last forwarding, on the
      forwarding's delete, the create's undo and its internal port's deletion (`deletePortForwardingsOf`, called beside
      `DisassociatePortFloatingIPs` in both port deletions); a floating IP its forwardings use refuses a port (`409
      FipInUseByPortForwarding`) and keeps its router on a null `port_id` (`internal/neutron/port_forwarding.go`,
      `floatingip.go`, `ports.go`). The contract test's fixture, `TestDeletionOrder_Router` and the host runbook's 2.9
      now forward through a free floating IP. Test: `port_forwarding_router_test.go`. *Done 2026-11-24. RED: the free
      floating IP's forwarding answered 400, the associated one's 201. GREEN: every unit package on both backends, lint
      clean; 8 of 8 mutations caught (`go test -overlay`).*

- [X] T193 [SRC-21] Listing or creating groups never ensured the project's `default`: only a port's creation and Nova's
      server create and attach did. On a project with no port yet, `?name=default` listed nothing, so Terraform's
      `openstack_networking_secgroup_v2` data source for `default` and `openstack security group rule create … default`
      failed there. FR-019a: the group listing, the rule listing (upstream's `get_security_group_rules` ensures it too)
      and the group create ensure the default of the project concerned — `listedProject`: the first one an administrator
      names, else the caller's — skipping a project that does not exist (`ensureDefaultOf`,
      `internal/neutron/security_group_default.go`); `EnsureDefaultSecurityGroup` reads first, outside a transaction, so
      a listing writes nothing once it exists and takes no SQLite write lock. The two listing tests that pinned the
      divergence now expect the default. Test: `security_group_default_ensure_test.go`. *Done 2026-11-24. RED: no
      default in any of the four. GREEN: every unit package on both backends, lint clean; 5 of 5 mutations caught (`go
      test -overlay`); the read-first's "writes nothing" is not pinned.*

- [X] T194 [SRC-22] The rule listing read no query parameter: `openstack security group rule list <group>` sends
      `security_group_id` and hides the group column, so it showed every visible rule as that group's, for an
      administrator every project's. FR-019a: `filterByColumns` (`internal/neutron/security_group_api.go`) is the group
      listing's equality filters made one function, and the rule listing applies upstream's filters through it
      (`securityGroupRuleFilters`: `id`, `security_group_id`, `direction`, `ethertype`, `protocol`, `port_range_min`,
      `port_range_max`, `remote_ip_prefix`, `remote_group_id`), each repeatable. Test:
      `security_group_rule_list_filter_test.go`. *Done 2026-11-24. RED: every filter listed every rule. GREEN: every
      unit package on both backends, lint clean; 2 of 2 mutations caught (`go test -overlay`).*

- [X] T195 [SRC-23] A port's show and listing read its groups by hand, unordered, and went on when the read failed,
      answering 200 with `security_groups: []`; the listing read them per port while its rows were open, holding a
      second connection. Both also read and rendered a port each its own way, and a row with no name, MAC or status — no
      path of Lightstack's writes one, other packages' tests do — aborted the whole listing with 500, the cause of
      `TestAdministratorListsEveryProject`'s failures in shared-database runs. `portsSecurityGroups` is now the one
      reader of ports' groups, `PortSecurityGroupIDs` its single-port form; `GetPort` and `ListPorts` share
      `portColumns` (the never-NULL columns coalesced), `scanPort` and `portRecord.render`, the listing reading its
      ports' groups in one query once its rows are read, and both answer 500 when a read fails
      (`internal/neutron/ports.go`). Test: `port_read_groups_test.go`. *Done 2026-11-24. RED: the listing answered 500
      for the row without a name. GREEN: every unit package on both backends, lint clean; 4 of 4 mutations caught (`go
      test -overlay`).*

- [X] T196 [SRC-24] A rule's delete read the rule twice, the first time into eight variables nothing used ("to remove
      from iptables", which it no longer does), and a rule's create checked both groups' ownership before its
      transaction and again inside it. The delete reads the rule once, locked and scoped, in the transaction that
      deletes it; the create validates both group ids before anything is read — `security_group_id` too, which answered
      404 where upstream's `type:uuid` answers 400 (FR-059) — and leaves ownership to its transaction's check
      (`internal/neutron/ports.go`). Three queries and the stale iptables comments are gone. Test:
      `security_group_rule_refusals_test.go`. *Done 2026-11-24. RED: the malformed `security_group_id` answered 404.
      GREEN: every unit package on both backends, lint clean; 3 of 3 mutations caught (`go test -overlay`).*

- [X] T197 [SRC-25] Three boolean query parsers disagreed with upstream's `strutils.bool_from_string` and with
      `common.ParseQueryBool`, which already speaks it: the group listing's `queryBooleans` knew `true`/`false`/`1`/`0`
      and answered 400 to `?shared=no` and `?stateful=yes`; the network listing's `neutronBool` and Cinder's `?usage=`
      used Go's `strconv.ParseBool`, ignoring `yes`, `no`, `on` and `off`. All three now read through
      `common.ParseQueryBool`; `neutronBool`, which was exactly it but for the words, is gone
      (`internal/neutron/security_group_api.go`, `network.go`, `internal/cinder/quotas.go`). Tests:
      `TestNetworkListDecisionTakesUpstreamsBooleans` replaces `TestNeutronBool`;
      `TestListSecurityGroupsAttributeFilters` gains `shared=no` and `stateful=yes`; `TestBackupUsageIsCounted` asks
      with `usage=yes`. *Done 2026-11-24. RED: each test against its old parser. GREEN: every unit package on both
      backends, lint clean.*

- [X] T198 [SRC-26] Three copies resolved a requested address and had drifted: a subnet's gateway was refused with 400
      for a port and a router gateway (`requestedAddress`) and with 409 for a floating IP (`requestedFloatingAddress`),
      and `IPAvailable`, Nova's interface check, called it unavailable. Upstream's IPAM (`_verify_ip`) refuses neither:
      only the pools leave the gateway out, and a router interface normally holds it. FR-038, FR-053:
      `requestedAddressRefusal` is upstream's one check — a host address of the subnet (400 `InvalidIpForSubnet`) that
      nothing but the port itself holds (409 `IpAddressAlreadyAllocated`) — over `heldAddresses`, what ports and
      floating IPs hold; `usedAddresses`, what an automatic allocation leaves out, is that and the gateway.
      `requestedAddress` passes it, a floating IP's requested address goes through `requestedAddress` and
      `requestedFloatingAddress` is gone, `IPAvailable` answers from it, and `requireAddressesFree` (T188) asks
      `heldAddresses` (`internal/neutron/allocation.go`, `router_gateway.go`, `floatingip_association.go`). Tests:
      `requested_address_test.go`; `TestRequestedAddresses`, `TestIPAvailableCanonicalIPv6` and the router gateway's
      refusal table, which held the gateway unavailable, now hold it free. *Done 2026-11-24. RED: the free gateway was
      refused on all three paths. GREEN: every unit package on both backends, lint clean; 4 of 4 mutations caught (`go
      test -overlay`).*

- [X] T199 [SRC-27] The undo paths the Article II exception rests on, and two refusals, had no test. Now each has one,
      through a failing node step after the store: a router gateway's move undone with its port put back as it was; a
      subnet's gateway and pools undone together; a floating IP's deletion undone without the router that went
      meanwhile, and left deleted when its project went; a port forwarding's move to another port applied, and undone
      when the router cannot take it; a failed attach's compare-and-set undo, an existing port released rather than
      deleted and a port another server took meanwhile left (`bindRecorder` gains a hook that acts during the failing
      bind); `deleteServer`'s split, 500 and the server kept for a delete, carrying on for `forceDelete`. The refusals:
      a floating IP on a subnet without a gateway (400), `internal_port` out of range on update (400). A test seam:
      `hypervisor.NewDisconnectedVMManager`, a real-mode manager whose connection is down, the state between a lost
      connection and its re-dial (`pkg/hypervisor/libvirt.go`). No product code changed otherwise; `forceDelete`'s
      `204`, where upstream answers `202`, went to `follow-ups.md` §6. Tests: `undo_coverage_test.go`,
      `delete_hypervisor_failure_test.go`, `TestInterfaceAttachUndoIsCompareAndSet` (`port_binding_test.go`). *Done
      2026-11-24. GREEN: every unit package on both backends, lint clean; 9 of 9 mutations caught (`go test -overlay`),
      one per path; the project-gone branch of the restore is pinned by its outcome only (without the check, the
      restore's insert fails on the foreign key and the floating IP stays deleted too).*

- [X] T200 [MD-2] UPGRADING read as if every `default` gained member rules, and said nothing of the groups the earlier
      release left without any: migration 084 gives them to the seeded project's `default` only,
      `EnsureDefaultSecurityGroup` leaves an existing group as found, and the earlier release's API-created groups have
      no rule, where every port chain now ends in a drop. The row now names the `default`s created from now on and the
      seeded one, and a subsection, *Existing security groups and the port firewall*, gives the egress rules an
      API-created group needs and the member rules an older `default` needs, with the commands to list them. *Done
      2026-11-24; the blocks parse in zsh.*

- [X] T201 [MD-3] UPGRADING did not say that eleven creates — ports, security groups, subnets, routers, floating IPs,
      QoS policies, RBAC policies, trunks, metering labels, subnet pools, address scopes — now honour
      `project_id`/`tenant_id` (`ownerOf`), which the earlier release ignored. A row says so, with the `400`s. *Done
      2026-11-24.*

- [X] T202 [MD-4] `docs/NETWORKING_MODES.md` said the modes are the same and still sold eBPF's throughput, gave
      eBPF-only requirements, an eBPF example with `bpftool`, `clang` and `llvm`, and test steps (`bpftool prog list`,
      `tc filter show dev tap-…`) that cannot pass. eBPF mode is now described as IPTables mode under another name, with
      its requirements, behaviour and test; the unsourced throughput and latency tables are gone, and so is the iptables
      section's claim of userspace processing (netfilter is the kernel's). *Done 2026-11-24.*

- [X] T203 [MD-5] `docs/VXLAN_IMPLEMENTATION.md`'s node 2 had its own Neutron port (`port: 19696`), which breaks every
      node call: a node dials another at its own Neutron port on the other's tunnel IP
      (`internal/neutron/node_call.go`). The example drops the different ports; the prerequisites and the chart README's
      multi-node notes add TCP to the Neutron port between tunnel IPs and the one `jwt_secret` that signs the calls.
      *Done 2026-11-24.*

- [X] T204 [MD-6] The runbook's §3 teardown sourced `/tmp/ls579.env` and deleted it at its end, so a second run — or one
      after a reboot emptied `/tmp` — ran `$ENGINE exec …` with `ENGINE` unset: zsh's `exec` builtin, which closed the
      shell. The block now sources the file only when it is there, finds `ENGINE` and `K3S` again, guards the engine
      commands and the restored settings on the values it has, and gives the guest check ids that match nothing when the
      run's are gone. *Done 2026-11-24; the block parses in zsh.*

- [X] T205 [SRC-28] On a node with no bound port the raw table's `ls-ct` is wanted with no rule; a node lacking it
      compared equal (no rules either way), so it was never declared while the jump from `PREROUTING` to it was written,
      and the raw commit failed: a fresh node before its first guest, a dedicated network node and a node after teardown
      failed every pass. `firewallPayload` declares a chain the node lacks even when it is to hold nothing
      (`pkg/networking/port_firewall.go`). Test: `TestPortFirewallPayloadWithNoPort`. *Done 2026-11-24. RED: no `:ls-ct`
      before the jump. GREEN: both backends and lint, 2026-11-24 (batch 4); 1 of 1 mutation caught.*

- [X] T206 [SRC-29] A port's requested fixed IPs locked their subnets entry by entry, in the request's order, where
      every other allocation locks a network's subnets in creation order: two creates naming [B, A] and [A, B] could
      deadlock on PostgreSQL, the loser answered 500. `requestedAddresses` locks every subnet the entries name first, in
      the network's order (`requestedFixedIP.names`), then resolves them (`internal/neutron/router_gateway.go`); creates
      and router gateways both go through it. Test: `subnet_lock_order_test.go`, PostgreSQL only: with the network's
      first subnet locked by another transaction, a create naming [second, first] waits holding nothing. *Done
      2026-11-24. RED: the mutation restoring the request's order leaves the second subnet locked while the create
      waits. GREEN: both backends and lint, 2026-11-24 (batch 4).*

- [X] T207 [SRC-30] A server group change's undo walked the ports the change named, not the ones it locked: a port
      deleted since read `[]`, which a removal that emptied it had left, so the compare passed and putting the group
      back hit the foreign key, rolling the whole undo back — the server's other ports kept the removal while the client
      got 500. The undo walks what `lockPorts` found (`internal/nova/advanced_actions.go`). Test:
      `TestSecurityGroupActions_UndoSkipsAPortDeletedMeanwhile`. *Done 2026-11-24. RED: the port that stayed kept the
      removal. GREEN: both backends and lint, 2026-11-24 (batch 4).*

- [X] T208 [SRC-31] A server's deletion unbound the ports attached by id before it was decided, and its `restore`
      covered only the ports its boot made — a no-op when an unbind failed partway; `DetachInterface` unbound the port,
      then ran its two writes outside a transaction. A failed deletion or detach answered 500 and left the port attached
      with no chains, its TAP falling to the dispatch's drop. `withdrawServerPorts` reads each attached port's binding,
      binds the ones already unbound back when a later one or the withdrawal fails, and its `restore` binds them back
      too; `DetachInterface` reads the binding, writes the attachment and the port in one transaction and binds the port
      back when that fails — both through `rebindPorts` (`internal/nova/handlers.go`, `interface_attach.go`). Test:
      `TestFailedDeleteOrDetachBindsTheAttachedPortBack` (`port_binding_test.go`; `bindRecorder` can fail
      `WithdrawPorts`). *Done 2026-11-24. RED: each re-bind, removed as a mutation, leaves the port unbound: 2 of 2
      caught. GREEN: both backends and lint, 2026-11-24 (batch 4). The re-bind a later failed transaction gets through
      `restore` shares the helper and has no test of its own.*

- [X] T209 [SRC-32] A server's deletion read its volume attachments unlocked, updated or deleted each volume, then
      deleted the attachments, where a volume's detach deletes the attachment, then updates the volume: on PostgreSQL
      the cycle ended in 40P01 and a 500, for `forceDelete` too, which now shares the deletion. `releaseServerVolumes`
      locks the attachments first (`FOR UPDATE`, in id order) (`internal/nova/handlers.go`). Test:
      `TestServerDeleteLocksAttachmentsBeforeVolumes`, PostgreSQL only: with an attachment locked by another
      transaction, the deletion waits holding no volume. The lock-wait plumbing the three such tests share is
      `dbtest.HolderPID` and `dbtest.WaitUntilBlocked` (`internal/database/dbtest/dbtest.go`); T187's and T206's tests
      use it. *Done 2026-11-24. RED: the volume was locked while the deletion waited. GREEN: both backends and lint,
      2026-11-24 (batch 4).*

- [X] T210 [SRC-33] The shared authentication middleware exempted every path under `/internal/`, in the middleware Nova,
      Neutron, Cinder and Placement all use: a later `/internal/` route on any of them would have been unauthenticated,
      and no test went through the real middleware — deleting the carve-out kept every test green while every remote
      node call would have answered 401. Neutron's node call, which checks its own node token, is registered by
      `RegisterNodeRoutes` ahead of the user-token middleware in `createNeutronServer` — gin fixes a route's handlers
      when it is registered — and the carve-out is gone (`internal/middleware/auth.go`, `internal/neutron/network.go`,
      `cmd/lightstack/main.go`); the Neutron test routers register both, as the server does. Tests:
      `TestAuthMiddlewareExemptsNoInternalPath`; `TestNeutronServerRoutesTheNodeCallAheadOfUserAuth`
      (`cmd/lightstack/node_route_test.go`), through the real wiring. *Done 2026-11-24. RED: each half put back, as a
      mutation, fails its test: 2 of 2 caught. GREEN: both backends and lint, 2026-11-24 (batch 4).*

- [X] T211 [SRC-34] A node token's `iat` was never compared with the clock (`jwt.ParseWithClaims` without
      `WithIssuedAt`): a token a node with a fast clock minted stayed valid, and replayable, until that clock's time
      came. `checkNodeToken` refuses a token issued in the future, within two seconds' skew (`nodeTokenSkew`)
      (`internal/neutron/node_call.go`). Test: `TestNodeCallToken` gains a token issued an hour ahead (401) and one a
      second ahead (accepted). *Done 2026-11-24. RED: the hour-ahead token was accepted. GREEN: both backends and lint,
      2026-11-24 (batch 4).*

- [X] T212 [SRC-35] The routers' host forward chain was made with a check and an `iptables -N`, without `--wait`: two
      routers' first gateways converging on a fresh node, under their own unit locks, could both find it missing, and
      the loser failed with "Chain already exists", a 500 and an undo. `ensureForwardChain` reads the filter table and,
      where it differs, writes the chain, its rules and the jump from `FORWARD` in one `iptables-restore --noflush
      --wait 1`, as the port firewall's payload does (`pkg/networking/router_namespace.go`). Test:
      `TestConvergeRouter_ForwardChainInOneRestore`; the fake node answers the table's read. *Done 2026-11-24. RED: no
      restore carried the chain. GREEN: both backends and lint, 2026-11-24 (batch 4); 1 of 1 mutation caught.*

- [X] T213 [SRC-36] The fail-closed drop of a TAP without a bound port matched every `tap+` on every bridge of the host,
      since `ls-local` runs first in `FORWARD`: a bridged `tap0` of another program — a hand-run QEMU, OpenVPN — lost
      its traffic. The two drops are now emitted per network bridge, `-o`/`-i <bridge>`: the bridges the node holds,
      listed with `ip -j link show type bridge` and kept when they are Lightstack's (`br-` and eight hex digits;
      Docker's twelve are not), and those its bound ports join. A bridge with no bound port keeps the drop the last
      port's deletion needs, which the review's per-bound-bridge form would have lost (`PortFirewallRules` takes the
      node's bridges; `ConvergePorts` reads them) (`pkg/networking/port_firewall.go`); the contract says so. Test:
      `TestConvergePorts_DropsUnboundTAPsOnTheNetworksBridgesOnly`; `TestPortFirewallIPv4` and the fresh node's command
      list updated. *Done 2026-11-24. GREEN: both backends and lint, 2026-11-24 (batch 4); 1 of 1 mutation caught (the
      node's bridges ignored).*

- [X] T214 [SRC-37] Every port's egress jumped to its address check in both families, and a family the port has no
      address in had only `-j DROP` there: an IPv4-only port sent no IPv6, link-local included, where upstream's
      iptables driver gives a family the check only for the addresses the port has. `checksAddresses` gives a family the
      chain and the jump when the port has an address in it, or none at all (its MAC alone)
      (`pkg/networking/port_firewall.go`); the contract says so. Test: `TestPortFirewallIPv6`, which pinned the drop,
      now pins upstream's. *Done 2026-11-24. RED: the IPv4-only port's IPv6 check and jump. GREEN: both backends and
      lint, 2026-11-24 (batch 4); 1 of 1 mutation caught.*

- [X] T215 [SRC-38] An `If-Match` other than exactly `revision_number=<n>` was refused `400` with a message of
      Lightstack's own, where upstream reads the header's entity tags as webob parses them and constrains only on a
      `revision_number=` tag: `*`, a weak or foreign tag, or `"abc", revision_number=5` constrain nothing, and two
      revision tags or an unparsable number are `400 Bad if-match request: …` in upstream's words. `ifMatchRevision`
      walks `strongETags` (webob's `_rx_etag`, strong tags only, the whole header when none is quoted) as
      `check_request_for_revision_constraint` does. The four hand-built `Bad <resource> request: …` builders
      (`badFloatingIPRequest`, `forwardingRequestError`, the router interface's literals) and the new ones share one
      `badRequest(resource, msg)`, neutron_lib's `BadRequest` (`internal/neutron/security_group_api.go`, `errors.go`);
      FR-050 and the contract say so. Test: `TestSecurityGroupRevisionNumber`'s etag cases; the tests' two NeutronError
      decoders are one. *Done 2026-11-24. RED: 4 of the etag cases. GREEN: both backends and lint, 2026-11-24 (batch 4);
      7 of 7 mutations caught.*

- [X] T216 [SRC-39] Every in-use refusal of the Networking API — `NetworkInUse`, `RouterInUse`, `GatewayIpInUse`,
      `SecurityGroupInUse` — answered `Cannot <operation>` with upstream's text, or a reason of Lightstack's own, in
      `detail`; upstream's message is the exception's, its detail empty. Each is now named at its call site with
      upstream's message, as every other upstream class here is: `Security Group <id> in use.`; `Router <id> has ports
      still attached: <ports>`, the router's interface and gateway ports in the order they were made (`errRouterInUse`,
      `_ensure_router_not_in_use`); `Unable to complete operation on network <id>. <reason>.`, `networkInUse` naming the
      ports as `_ensure_network_not_in_use` does and the floating IPs and port-less gateways, which have no port here,
      the same way; `GatewayIpInUse` as before, in `message`. `common.NewResourceInUseError` and the resource table
      behind it (`neutronInUseTypes`) are gone, a `409` without a type is `Conflict`, and `removeNetwork` and
      `errSecurityGroupInUse` lose the operation they only passed on (`internal/neutron/network.go`, `router.go`,
      `security_group_api.go`, `internal/common/errors.go`, `neutron_errors.go`); FR-008c, the contract and UPGRADING
      say so. Tests: `assertNeutronError` (shape, type, message, empty detail) in `TestNeutronInUseNamesTheCondition`
      (now with two ports), `TestDeletingARouterWithInterfacesIsRouterInUse` (with a gateway port),
      `TestSubnetGatewayHeldByARouterIsNotChanged`, `TestDeleteSecurityGroup`, `TestStatelessSecurityGroups`. *Done
      2026-11-24. RED: all five. GREEN: both backends and lint, 2026-11-24 (batch 4); 8 of 8 mutations caught.*

- [X] T217 [SRC-40] A port's create answered a body written by hand from the request: `tenant_id` without `project_id`,
      no `binding:host_id` for an administrator, `status` always `ACTIVE` and the handler's timestamps, where show and
      the listing render the stored row. `answerPort` reads a port as its show does and answers it with a status, for
      `GetPort`, `UpdatePort` and now `CreatePort`, so create, show, update and listing render one body through
      `portRecord.render` (`internal/neutron/ports.go`); FR-056, the contract and UPGRADING say so. The process clock's
      local wall time in zone-less `created_at` columns, which the test showed, is older than this branch: follow-ups §6
      item 6. Test: `TestPortCreateAnswersItsShow` (new), a member's and an administrator's create equal to their show,
      with `project_id` and the administrator's `binding:host_id`. *Done 2026-11-24. RED: both, no `project_id`, no
      binding, the create's timestamps. GREEN: both backends and lint, 2026-11-24 (batch 4); 2 of 2 mutations caught.*

- [X] T218 [SRC-41] A floating IP on a port of a subnet without a gateway answered `400 Bad floatingip request: Cannot
      add floating IP to port on subnet … which has no gateway_ip`, a check upstream's `get_router_for_floatingip` no
      longer has: there the gateway only prefers a router, and a subnet no router joins is `404
      ExternalGatewayForFloatingIPNotFound`. No interface joins a gateway-less subnet here (FR-055; `GatewayIpInUse`
      keeps an interface's), so `selectRouterForFloatingIP` reads the gateway as a preference only and answers FR-028's
      `404` (`internal/neutron/floatingip_association.go`); FR-028, the contract and UPGRADING say so. Test:
      `TestUndoPathsPutTheRecordBack/refusals`, which pinned the `400` (T199), now pins the `404`. *Done 2026-11-24.
      RED: the 400. GREEN: both backends and lint, 2026-11-24 (batch 4); 2 of 2 mutations caught.*

- [X] T219 [SRC-42] A node's unit locks were made on first use and never removed, so the table grew by one for every
      router and network the node ever converged, deleted ones included. A lock is now reference-counted under the
      table's mutex: `join` counts a request in, making the lock if none has it, `leave` counts it out, removing the
      lock with the last, and a wait that reaches its deadline leaves as a release does (`unitLocks`, `unitSlot`,
      `internal/neutron/units.go`); the data model says so. Test: `TestUnitLocksKeepOnlyTheUnitsInUse` (new): the lock
      excludes, a wait ends busy at its deadline, and no lock is left once no request holds or waits; it runs under
      `-race`. *Done 2026-11-24. RED: a lock left after its last release, four after three units. GREEN: both backends
      and lint, 2026-11-24 (batch 4); 3 of 3 mutations caught.*

- [X] T220 [SRC-43] The node call's `absent` had two shapes, a boolean for a whole router or DHCP and a list of ids for
      records or ports, carried by a hand-written codec (`unitWire`, `MarshalJSON`, `UnmarshalJSON`) that no test sent
      for a router or DHCP, since the fake node caller passes the request unencoded. `unitRequest` now carries two plain
      fields, `absent` (a boolean) and `absent_ids` (a list), each left out when empty, and the codec is gone;
      `validate` refuses a router or DHCP absent whole and in part, which the wire can now say
      (`internal/neutron/units.go`); the node-apply contract says so. Tests: `TestNodeCallRequest`'s new subtest sends
      every unit's shape through the HTTP caller and decodes it as the node route does;
      `TestNodeCallUnitsTheTargetHolds` writes `absent_ids` and refuses a list in `absent` and both at once. *Done
      2026-11-24. RED: the body's field `absent` for the ports' list. GREEN: both backends and lint, 2026-11-24 (batch
      4); 3 of 3 mutations caught.*

- [X] T221 [SRC-44] Code the relayout left without a caller is removed: `pkg/networking`'s eBPF wrapper
      (`security_groups.go`, `security_groups_ebpf.go` and its test — `SecurityGroupManager`, `NewSecurityGroupManager`,
      `ApplySecurityGroupToPort`, the rule translation), whose last caller was port creation's eBPF step (R32), while
      `pkg/networking/ebpf` stays for the separate decision; `VXLANManager.DeleteVXLAN`, a wrapper of
      `DeleteVXLANDevice` only tests called; `VXLANCoordinator.RemoveFDBEntry`, whose comment asked to be called on a
      port's deletion that the forwarding row's cascade already serves, and `GetVNI`; `portsByNode`, which only its test
      called. `parseMetadataRule` and its `metadataRule` become the predicate `isMetadataRule`, the one thing the
      retired-state removal asks (`pkg/networking/metadata_redirect.go`, `retired.go`). R32, follow-ups §5 item 7,
      `docs/EBPF_STATUS.md` and `docs/VXLAN_IMPLEMENTATION.md` say so. No behaviour changes; the build, vet and the
      unused-code lint are the check, and the tests that called the removed code call what remains (`vxlan_test.go`,
      `metadata_redirect_test.go`, `network_node_test.go`). *Done 2026-11-24. GREEN: both backends and lint, 2026-11-24
      (batch 4).*

- [X] T222 [SRC-45] The database-less teardown read the namespace list itself and kept its own `isID`, beside the
      recognition `HeldRouters` and `HeldNetworks` use, and both comments promised to tell upstream's agents' namespaces
      apart, which neither can: upstream names its `qrouter-`/`qdhcp-` namespaces with the same lower-case UUIDs.
      `prefixPurge` now asks the managers what they hold — `HeldProjects` (new, `NetworkNamespaceManager`),
      `HeldRouters`, `HeldNetworks`, all `heldNamespaces` — and removes each, a failed listing failing the purge while
      the rest goes; `isID`'s copy, `parseNetnsList` and the fallback's own listing are gone. An older deployment's
      project namespace is now recognised as the others are, `light-ns-` and a lower-case UUID, as Lightstack named
      them, and a `light-ns-` name with any other suffix is left (`internal/teardown/teardown.go`,
      `pkg/networking/netns.go`, `router_namespace.go`, `dhcp_namespace.go`); the comments say what is recognised and
      that upstream's are not told apart; R48 says so. Tests: `TestHeldProjects` (new), `TestPrefixPurge_*` on the
      managers' lists, a failed listing of each kind on its own. *Done 2026-11-24. RED: no `HeldProjects` (build).
      GREEN: both backends and lint, 2026-11-24 (batch 4); 4 of 4 mutations caught.*

- [X] T223 [SRC-46] Nova's server deletion repeated Neutron's converge-and-log of disassociated floating IPs line for
      line, and its comment and the interface's still named "the filters of the fixed IPs they reached", which FR-042
      retired. Neutron's `ConvergeFloatingIPs` and `convergeDisassociated` are one exported `ConvergeDisassociated`,
      which converges the routers and logs a failure; the port's deletion, a failed boot's release and Nova's deletion
      call it, and the comments say what it converges (`internal/neutron/floatingip.go`, `ports.go`,
      `internal/nova/handlers.go`); R25 says so. The convergence after the commit had no test that needed it — the
      withdrawal before the commit takes the port out of its routers — so one stages its case: a floating IP associated
      with the port while the delete awaits the port's lock, in the router by then, and out of it after the answer.
      Nova's failed-convergence subtest went with the error Nova no longer sees. Tests:
      `TestFloatingIPDataplane_PortDeletion` (new subtest, PostgreSQL), `TestServerDeletion_FloatingIPs` on the renamed
      fakes. *Done 2026-11-24. No behaviour change. GREEN: both backends and lint, 2026-11-24 (batch 4); 2 of 2
      mutations caught, one of them only after the new subtest.*

- [X] T224 [SRC-47] Functions this branch grew far past the bar are split at their seams: `UpdateSubnet` (237 lines)
      into `parseSubnetUpdate`, `resolveWritableSubnet`, `checkSubnetGatewayChange`, `storeSubnetUpdate` and
      `applySubnetGatewayChange` (SRC-17's fix), 39 lines left; `CreateSubnet` (158) into `parseSubnetCreate`, 78;
      Nova's `deleteServer` (199) into `deleteDomain`, `deleteServerRecords`, `releaseServerPortRecords`,
      `cleanUpDeletedServer` and `readServerPorts`, which also serves `withdrawServerPorts`' read of the same ports, 84;
      `applyRouter` (144) into the step builders `routerLegSteps` and `routerFloatingSteps`, `applyRouterHostSide` and
      `runSteps`, 30; `storeRouterUpdate`'s eight parameters inlined into its one caller. The subnet body, written four
      times, is one: `subnetColumns`, `scanSubnet` and `renderSubnets` serve the listing and `answerSubnet`, which
      answers show, create and update read back as stored — the one behaviour change: create's answer had the handler's
      timestamps and update's had none (`internal/neutron/network.go`, `router.go`, `internal/nova/handlers.go`,
      `pkg/networking/router_namespace.go`); UPGRADING says so, and the subnet's missing `project_id`, older, is
      follow-ups §6 item 7. Test: `TestSubnetCreateAndUpdateAnswerTheirShow` (new); the rest is pinned by the packages'
      tests, the router's command order by its apply tests. *Done 2026-11-24. RED: the create's local timestamps, the
      update's missing ones. GREEN: both backends and lint, 2026-11-24 (batch 4).*

- [X] T225 [SRC-48] The migrator read the foreign-keys-off marker on a file's exact first line only: a byte-order mark,
      a blank line or a header comment above it sent a table rebuild down the ordinary path, where SQLite's `DROP TABLE`
      deletes the referencing rows through `ON DELETE CASCADE`, and the migration still succeeded.
      `hasForeignKeysOffMarker` ignores a UTF-8 byte-order mark before the marker and refuses a file with the marker on
      any other line, naming it (`internal/database/migrate_notx.go`); `migrations/README.md` says so. Test:
      `TestForeignKeysOffMarkerOnlyOnTheFirstLine` (new): after a byte-order mark the rebuild keeps the children; below
      a comment or a blank line the file is refused and the children stay. *Done 2026-11-24. RED: the byte-order mark
      cascaded, both misplaced markers were ignored. GREEN: both backends and lint, 2026-11-24 (batch 4); 3 of 3
      mutations caught.*

- [X] T226 [SRC-52] The marker was named `-- lightstack:no-transaction`, while a marked file runs in a transaction — of
      the migrator's own, begun once foreign keys are off — and four spec lines said it ran outside one. It is `--
      lightstack:foreign-keys-off`: the constant `foreignKeysOffMarker`, migration 085's two SQLite files,
      `migrations/README.md`, the migrator's comment, the tests (`TestForeignKeysOffMarker*`, on the constant rather
      than a copy of its text), and the spec, plan, research, data model and tasks lines, which now say foreign keys are
      off rather than that the file runs outside a transaction. 085 is not yet released, so its files change in place.
      *Done 2026-11-24. No behaviour change but the marker's text. GREEN: both backends and lint, 2026-11-24 (batch 4).*

- [X] T227 [SRC-49] Migration 085 dropped `UNIQUE (project_id, name)`, whose index was the only one on `security_groups`
      led by `project_id`, so a project's group lookups by name and its listings scanned the table. 085 creates
      `idx_security_groups_project_name (project_id, name)` in both up files, after the rebuild on SQLite, and both down
      files drop it first (`migrations/085_security_group_names.*`); the data model says so. 085 is not yet released, so
      it changes in place. Test: `TestMigration085KeepsAProjectIndexOnGroups` (new, both backends); `TestSchemaParity`
      and the round trip hold. *Done 2026-11-24. RED: no such index on either backend, which is also the mutation
      evidence: SQL files cannot be mutated through an overlay. GREEN: both backends and lint, 2026-11-24 (batch 4).*

- [X] T228 [SRC-50] The tests that a node already as its records say is only read answered `iptables-save` with the
      renderer's own output, so they held by construction, and nothing checked the rendering against what
      `iptables-save` prints, which compare-before-write and SC-010 rest on. They now read
      `pkg/networking/testdata/iptables-save/`: the port firewall of `fwPorts` (filter and raw, both families),
      `testRouter`'s nat and mangle tables and the host's forward chain, as a fresh node's convergence writes them,
      loaded into a scratch network namespace by the real `iptables-nft-restore` and printed back by `iptables-nft-save`
      (v1.8.13, nf_tables, the image's backend). `TestCaptureIPTablesSave -update` recaptures them through `unshare
      -rn`, nothing on the host changing. They found a defect: the anti-spoofing chains wrote the MAC in upper case,
      `iptables-save` prints it in lower, so every pass on every node rewrote every port's `ls-s` chain. `spoofChain`
      writes it in lower case (`pkg/networking/port_firewall.go`); the port-firewall contract says so. Tests:
      `TestConvergePorts_UnchangedOnlyReads` and `TestConvergeRouter_UnchangedOnlyReads` on the captured tables; the
      router's matched already. *Done 2026-11-24. RED: the ports' node rewrote its four `ls-s` chains. GREEN: both
      backends and lint, 2026-11-24 (batch 4); 2 of 2 mutations caught (the MAC's case, a port forwarding's `-m tcp`),
      both of which the self-comparing form let through.*

- [X] T229 [MD-7] The runbook's setup missed a port, deleted a shared file and could take over a release. 1.1's
      busy-port check now includes Placement's 8778, whose clash is fatal at start and shows only as a rollout timeout,
      and it stops while a namespace `lightstack` exists, whose release `helm upgrade --install` would upgrade in place
      and §3 deletes. 1.2 downloads cirros to the run's own `/tmp/ls579-cirros.img`, through `.part` and with `curl -f`,
      so neither an HTTP error page nor a cut transfer becomes the image, and stops when it has none; §3 deletes that
      file, not a `/tmp/cirros.img` another run may own (`HOST_TESTING.md`). Both blocks pass `zsh -n` and `bash -n`.
      *Done 2026-11-24. Documentation only.*

- [X] T230 [MD-8] Documents this branch made false are corrected. `docs/L3_ROUTER_IMPLEMENTATION.md`: a banner points to
      the router-namespace contract for the current layout; the namespace is `qrouter-<router id>` on the network node,
      so the debugging commands, which cut the id to eleven characters, now work; the create example no longer sends
      `enable_snat`, an administrator's to set; the SNAT example is the rule Lightstack writes, not `MASQUERADE`.
      `docs/STUB_PLACEHOLDERS.md` marks eBPF not wired and its integration as history; `docs/EBPF_STATUS.md` drops the
      references to the port lifecycle and to `northsouth.go`, which now holds the uplink lookup and the command runner;
      the Terraform report says the groups are the ports' since SPEC-033; the README's multi-tenancy list describes
      bridges per network, the network node and the ports' chains instead of a namespace per project. *Done 2026-11-24.
      Documentation only.*

- [X] T231 [MD-9] `docs/CONFIGURATION.md`'s table of the environment variables the binary honours, "exactly these",
      lacked `LIGHTSTACK_NEUTRON_NETWORK_NODE`, which this branch added, and described `LIGHTSTACK_NODE_NAME` by volume
      reservations alone, though the name now also keys the node registry, the ports' `binding:host_id` and the network
      node. Both rows say so. The set was derived from the code: every `LIGHTSTACK_` variable the binary reads is in the
      table (`LIGHTSTACK_TEST_DB_URL` is the tests'). *Done 2026-11-24. Documentation only.*

- [X] T232 [MD-10] The constitution and CLAUDE.md disagreed with each other and with the code. The constitution's
      required dependencies named `github.com/coreos/go-iptables`, which this branch removed from `go.mod`; they now
      name iptables and iproute2 run as commands, and the Sync Impact Report says so. The v1.6.0 amendment log now
      records Article V's change, which only the report did. CLAUDE.md credited the firewall's "`iptables` and `ebpf`
      mode alike" to Article V, which says nothing of modes; it now cites FR-041 and FR-047. It said 72 tables where a
      database migrated to 077 holds 74, counted on a scratch SQLite file. Its SPEC-033 entry named migration 076 only;
      it names 077 too. *Done 2026-11-24. Documentation only.*

- [X] T233 [MD-11] Two UPGRADING lines were inaccurate. The network-node line named the configuration key alone, which
      the chart's schema refuses; it names the chart's `neutron.networkNode` beside it. The row for a group named
      `default` gave `500` as the earlier answer in any case, where the earlier release refused only `default` exactly,
      through its unique name, and accepted `Default` (verified on the base commit: no case folding, a 500 on the
      insert's failure). *Done 2026-11-24. Documentation only.*

- [X] T234 [MD-12] The spec documents cited `ba700dc`, which squashing round 2 left unreachable: `baseline-gates.md`,
      `follow-ups.md` and `ticket-checklist-round2.md` now name the commit by its subject, "fix(neutron): answer the
      networking API as upstream does", which a rebase keeps, with the old id beside it. The node-apply contract's
      answers gained the `400` the node route gives a body that is no unit request or names no unit; `quickstart.md`
      names migration 085 beside 084. *Done 2026-11-24. Documentation only.*

- [X] T235 [OTHER-1] The review read a ticket body that described round 1 and said "No migration". The body was synced
      the same day, after the review (cloud-storage#579, updated 2026-11-23 16:02 UTC): round 2's scope, decisions and
      acceptance criteria, the retired floating-IP filter marked superseded, migrations 084 and 085 and
      `neutron.network_node`. Read back on 2026-11-24 against this walk-through's fixes, its statements hold at its
      level of detail; nothing is published now. T057's rebase re-syncs it. *Done 2026-11-24. No change.*

- [X] T236 [OTHER-2] `test/security_groups_test.sh`'s check that a member cannot delete `default` accepted any `409`,
      and the seeded `default` usually carries ports, which upstream refuses first as `SecurityGroupInUse`: the check
      passed without reaching FR-024's refusal. It now tries the delete on a fresh project's `default`, which no port
      carries, with a member of that project, and requires `409` with `NeutronError.type`
      `SecurityGroupCannotRemoveDefault`; its trap deletes the project and the user. Run against a stub server on a
      scratch SQLite file: 13 tests, all passed, the refusal's type read, nothing left behind. *Done 2026-11-24.*

- [X] T237 [HELM-4] The teardown purge Job mounted the host's `/lib/modules`, "as the workload has them", though
      `lightstack cleanup` loads no module: the one `modprobe` is `NodeSettings.Ensure` at server start. The Job drops
      the mount and its volume; the pod keeps them for `br_netfilter`
      (`charts/lightstack/templates/teardown-purge-job.yaml`); the chart README says only the pod needs them. Test:
      `teardown_test.yaml`'s real-mode purge case asserts the mount absent. *Done 2026-11-24. RED: the mount present.
      GREEN: helm unittest 182 of 182.*

- [X] T238 [SRC-51] `addSecurityGroup` and `removeSecurityGroup` with a `name` of blanks only reached the resolver and
      answered `404`; upstream's `_parse` refuses it with `400`. A missing `name` is `400` "Security group not
      specified" and one that strips to empty `400` "Security group name cannot be empty", upstream's words, before any
      lookup (`internal/nova/advanced_actions.go`); the compute contract and UPGRADING say so. Test:
      `TestSecurityGroupActions_BlankOrMissingName_400` (new), both actions. *Done 2026-11-24. RED: `404` for blanks.
      GREEN: Nova's group action tests; 2 of 2 mutations caught.*

- [X] T239 [MD-13] Prose lines this branch added over 120 characters are reflowed in the documents the review named: two
      paragraphs of the constitution (the Sync Impact Report's CLAUDE.md entry and Article II's loop exception, wording
      unchanged), the VXLAN document's network-node item, the Horizon report's update note and the chart README's
      host-mounts paragraph. Tables, headings, runbook blocks, the spec folder's one-line paragraphs and CLAUDE.md's
      generated entries keep their lines, as the review suggests. Counted from the diff against the base: none of those
      four documents adds a prose line over 120 characters. *Done 2026-11-24. Documentation only.*

- [X] T240 [SRC-5, SRC-17] The host runbook proved neither fix in real mode, though the run T166 recorded predates both.
      Two steps join it before the deletions, which become 2.18, so 2.15's number, which the Horizon report cites,
      stays: 2.16 destroys and undefines vm1's domain and starts the server, then reads that the revived domain's
      interface is `tap<port id[:11]>`, on the network's bridge, with the port's three `ls-local` rules, and that a port
      nothing listens on answers `timeout`, where an unfiltered guest would answer `refused`; 2.17 sets the external
      subnet's gateway to the address nearest the pool outside it, reads the router's default route follow, sets it back
      and pings out from vm1, stopping with a `STOP:` line on a subnet with no such address. The coverage table and the
      introduction name both. The blocks pass `zsh -n` and `bash -n`; the address choice was run on four subnets. *Done
      2026-11-24. Documentation only.*

- [ ] T241 Re-run `HOST_TESTING.md` on one Kubernetes node, as T166 did, on the branch review's fixes, which reach
      real-mode code the unit suites check against fake nodes only — the port firewall (SRC-1, SRC-28, SRC-36, SRC-37,
      SRC-50), the routers (SRC-2, SRC-12, SRC-17 to SRC-20, SRC-35, SRC-47), DHCP (SRC-4, SRC-7, SRC-13), Nova's
      revival and deletion (SRC-5, SRC-31), the chart (HELM-2, HELM-4) — and on the runbook's own changed setup and
      teardown (MD-6, MD-7) and new steps 2.16 and 2.17. The node calls between two nodes (SRC-33, SRC-34, SRC-43) stay
      with the unit suites: two nodes are not supported yet. Record each step's outcome. Before T057.

## Phase 27: Second branch review (2026-11-25, report not committed)

Every finding fixed, per the user's instruction of 2026-11-25 (the full, DRY fix; siblings too). Behaviour changes
test-first, RED confirmed; refactors and docs keep the gates green.

- [X] T242 [HELM-1] The chart refuses multi-node without VXLAN, in stub and real mode, before the network-node check;
  NOTES names a network node only when set. Test: `tests/multinode_test.yaml`, `tests/notes_test.yaml`.
- [X] T243 [HELM-2] The network-node and VXLAN checks sit under `not secrets.existingSecret`. Test:
  `tests/multinode_test.yaml`, `tests/config_checksum_test.yaml`.
- [X] T244 [HELM-3] Every negative mount check matches by name with `any: true`; six render mutations caught. Test:
  `tests/netns_mount_test.yaml`, `tests/teardown_test.yaml`.
- [X] T245 [HELM-4] `init-host-storage` creates the database directory only on SQLite, the one `database.url` names.
  Test: `tests/init_host_storage_test.yaml`, `tests/sqlite_storage_test.yaml`.
- [X] T246 [COMPOSE-1] The turnkey Compose mounts `/run/netns:rshared` and `/lib/modules:ro` and creates `/run/netns`
  first; the copy in `docs/HORIZON_INTEGRATION.md` too. Test: `test/compose_turnkey_mounts_test.sh`.
- [X] T247 [SRC-1] FR-068: a subnet's `ip_version` is its CIDR's, checked as upstream (`invalidAttribute`, shared by
  `security_groups`, `dns_nameservers` and `direction`); the DHCP renderer serves IPv4 subnets only (`servedSubnets`,
  `servedHosts`). Test: `subnet_ip_version_test.go`, `TestDHCPLeavesOutSubnetsThatAreNotIPv4`.
- [X] T248 [SRC-2] FR-041: `ls-bound-in`/`ls-bound-out` drop an orphan TAP's frames both ways before any dispatch;
  proved with real iptables. Test: `port_firewall_orphan_test.go`.
- [X] T249 [SRC-3] FR-059: protocol names exactly as iptables prints them; every protocol 0–255 pinned by a captured
  fixture. Test: `TestPortFirewallRendersProtocolsByNumber`, `TestPortFirewallWritesEveryProtocolAsIPTablesPrintsIt`.
- [X] T250 [SRC-4] A rule's delete takes its group's lock first (`deleteRuleRecord`). Test:
  `security_group_rule_lock_order_test.go` (PostgreSQL).
- [X] T251 [SRC-5] FR-049: `ReleaseDeletedPorts` lets a deleted port's floating IPs and forwardings go, for all three
  deleting paths. Test: `TestServerDeletionFreesItsForwardingsFloatingIP`.
- [X] T252 [SRC-6] `RebindPort` binds back compare-and-set: only a port still naming the server and unbound. Test:
  `TestALosingDeletionLeavesTheDetachedPortAlone`, `rebind_port_test.go`.
- [X] T253 [SRC-7] Requests waiting on a unit share one converge (`unitLocks.converge`). Test:
  `TestUnitWaitersShareAConverge`.
- [X] T254 [SRC-8] FR-032: `addressRequest` resolves a request's addresses each once; `duplicateAddress` refuses one
  named twice. Test: `fixed_ips_duplicate_test.go`.
- [X] T255 [SRC-9] FR-037: `external_fixed_ips: []` or naming only the held subnet keeps the gateway's address. Test:
  `TestGatewayUpdateKeepsTheAddressItIsNotAskedToChange`.
- [X] T256 [SRC-10] FR-049: a forwarding's update is checked as its create (`checkForwardingMove`); `forwardingRouter`
  always finds the router, `requireRouterMatch` compares it. Test: `port_forwarding_update_test.go`.
- [X] T257 [SRC-11] FR-070: `requireNoGatewayPorts`, `lockExternalNetwork`. Test: `network_external_in_use_test.go`.
- [X] T258 [SRC-12] FR-073: `trunkPort` resolves parent and subports in the caller's scope. Test:
  `trunk_port_scope_test.go`.
- [X] T259 [SRC-13] `health.AddCheck` and its mutex gone; the checker is built once with every check. Test:
  `TestBridgeNetfilterCheck_GatesReadiness`.
- [X] T260 [SRC-14] Tests for the branches no test reached. Test: `uncovered_branches_test.go`,
  `TestForeignKeysOffMarkerRollsBackAFailedFile`, `TestUndoOfARemoveWhoseGroupWasDeleted`,
  `TestNodePrefixPurgeInStubModeTouchesNothing`. Not covered: the migrator's foreign-keys-still-on guard
  (unreachable), a router changed while being locked (an interleaving no hook exposes).
- [X] T261 [SRC-15] A failed rebind keeps the binding in the records for the reconciliation pass (`RebindPort`). Test:
  `rebind_port_test.go`.
- [X] T262 [SRC-16] FR-014: a server's report names the groups its viewer can see. Test:
  `server_report_visibility_test.go`.
- [X] T263 [SRC-17] FR-008: a port update that answers 500 undoes all of itself (`restorePort`). Test:
  `port_update_undo_test.go`.
- [X] T264 [SRC-18] FR-041: a `device_owner` trust change converges the port's node (`portState.reachesFirewall`).
  Test: `port_update_undo_test.go`.
- [X] T265 [SRC-19] FR-072: a trusted port carries no group (`trustedOwner`, shared by five sites). Test:
  `trusted_port_groups_test.go`.
- [X] T266 [SRC-20] FR-059: an empty `remote_group_id` is none. Test: `TestEmptyRemoteGroupIsNone`.
- [X] T267 [SRC-21] FR-019a: the rule listing's filter values go through the attribute converters
  (`columnFilter.convert`). Test: `security_group_rule_filter_convert_test.go`.
- [X] T268 [SRC-22] FR-055: an interface takes the gateway only while no port or floating IP holds it
  (`addressRequest.refusal`). Test: `TestRouterInterfaceTakesAGatewayNothingHolds`.
- [X] T269 [SRC-23] GatewayIpInUse is checked under the subnet's lock (`requireGatewayFree`); an interface reads the
  gateway under it. Test: `subnet_gateway_lock_test.go` (PostgreSQL).
- [X] T270 [SRC-24] FR-028, FR-029, FR-039: undo and restore re-check the association rules
  (`keepRestorableAssociation`, the gateway undo, `undoInterfaceAdd`). Test: `undo_association_test.go`.
- [X] T271 [SRC-25] FR-071: `parseFloatingIPUpdate`, `errFixedIPWithoutPort`. Test: `floating_ip_request_test.go`.
- [X] T272 [SRC-26] FR-030: a zoned pool address is refused. Test: `TestAllocationPoolRefusesAZone`.
- [X] T273 [SRC-27] `networkTypeAndMTU`, shared by both network creates. Test: `auto_allocated_network_type_test.go`.
- [X] T274 [SRC-28] FR-069: `add_router_interface` by port (`takeInterfacePort`). Test:
  `router_interface_port_test.go`.
- [X] T275 [SRC-29] The node call's transport names no proxy. Test: `TestNodeCallUsesNoProxy`.
- [X] T276 [SRC-30] FR-033: `neutron.network_node` read trimmed; removal only while the named node is active. Test:
  `network_node_active_test.go`, `TestLoadConfigNeutronNetworkNode/trimmed`.
- [X] T277 [SRC-31] FR-059: IPv4 ICMP type 255 alone renders `any`. Test: `TestPortFirewallRendersProtocolsByNumber`.
- [X] T278 [SRC-32] FR-044: cleanup removes the earlier layout's `FORWARD` accepts per recorded subnet. Test:
  `TestRemoveRetired_RecognisedStateOnly`.
- [X] T279 [SRC-33] `DeleteNamespace(ctx, …)` bounded at one second; the unbounded namespace helpers removed. Test:
  `TestDeleteNamespace`.
- [X] T280 [SRC-34] `NetworkConfig.Device` and `tap()` removed; the renderer derives the TAP from the port.
- [X] T281 [SRC-35] `BridgeManager.AttachToBridge` removed.
- [X] T282 [SRC-36] Contract tests assert what they name (`shared`, `revision_number`) and use the listing's filters.
- [X] T283 [SRC-37] `createServer` shared by the contract suite's three create helpers; subset tests and
  `rawListStatus` removed.
- [X] T284 [SRC-38] `UpdatePort`, `DeletePort`, `DeleteSubnet`, `deleteServer`, `readPortFirewall` and the package's
  renderers split into named steps.
- [X] T285 [SRC-39] `callerScope`, `floatingIPRef`, `networkListQuery`, `VNIRange`, `InstancePort` and
  `serverGroupRequest` group the parameters.
- [X] T286 [SRC-40] The comments that contradicted the code corrected.
- [X] T287 [SRC-41] Undo log lines report the change not applied; the undo helpers log their own outcome.
- [X] T288 [SRC-42] `applyPorts` takes no ids it ignores.
- [X] T289 [SRC-43] Package-internal renderers unexported; `PlanRouter`, `RouterPlan` and `Leg` stay exported for a
  neutron test.
- [X] T290 [SRC-44] Comments cite the task, not a review ID.
- [X] T291 [MD-1] `docs/UPGRADING.md`: the purge hook runs `lightstack cleanup` only with `dataRetentionPolicy:
  delete`.
- [X] T292 [MD-2] `docs/UPGRADING.md`: two "before" cells rewritten against `d81f081`.
- [X] T293 [MD-3] `docs/HOST_TESTING_OPENSUSE.md` drops `realMode.privileged`.
- [X] T294 [MD-4] FR-020 lists the branch review's answers; FR-066, FR-067.
- [X] T295 [MD-5] spec.md and data-model.md split reconciliation's removals from `lightstack cleanup`'s.
- [X] T296 [MD-6] follow-ups.md §5.4 marked fixed by FR-065.
- [X] T297 [MD-7] plan.md: two migrations, one host run, FR-061's gateway port, entry 5.
- [X] T298 [MD-8] data-model.md: two migrations, `doc.go:46`, 085's rebuild, FR-061.
- [X] T299 [MD-9] ticket-checklist-round2.md matches the posted comment.
- [X] T300 [MD-10] docs/VXLAN_IMPLEMENTATION.md shows `AttachToBridge`'s real signature.
- [X] T301 [MD-11] docs/NETWORKING_MODES.md troubleshoots what an operator now sees.
- [X] T302 [MD-12] docs/L3_ROUTER_IMPLEMENTATION.md checks the NAT in the router's namespace.
- [X] T303 [MD-13] The new spec files' prose reflowed at 120 characters.
- [X] T304 [MD-14] deployments/README-KVM.md lists the two new mounts.
- [X] T305 [MD-15] docs/UPGRADING.md: the purge Job mounts no `/lib/modules`; the repeated clause gone.
- [X] T306 [MD-16] docs/CONFIGURATION.md's env row matches the setting's section.
- [X] T307 [MD-17] charts/lightstack/README.md's Scope paragraph.
- [X] T308 [MD-18] checklists/requirements.md iterations 10–15.
- [X] T309 [MD-19] The constitution records the 2026-11-24 editorial refinement.
- [X] T310 [MD-20] docs/ARCHITECTURE.md names `ip netns`, not an unused library.
- [X] T311 [OTHER-1] The ticket body and checklist re-synced with the spec — after the user's approval of the text.
      *Done 2026-11-27, on the user's request after the rebase.* Body: Status, the branch reviews' decisions (FR-059 to
      FR-073) and the rebase's, v1.6.0 and migrations 084/085 throughout, `realMode.privileged` gone, Out of scope
      synced with `follow-ups.md` §5 and §6. Checklist: T057's two items and this one ticked, the zone lock added; the
      migration and node items reworded, the constitution item reported in an earlier week left as it was, with a note.
      Acceptance criteria unchanged, still identical in both. A notice comment says what changed.
- [X] T312 Constitution Check re-run at the rebase (T057), Article III: the three statements that give a network its
      connection-tracking zone (`CreateNetwork`, the auto-allocated topology, `assignMissingZones`) retried themselves
      up to five times on a unique violation (`retryTakenZone`), which Article III forbids. They now take
      `lightstack:ct-zone` through `database.AdvisoryLock` in their transaction and run once; `retryTakenZone` is gone.
      The helper also replaces the four inline copies of the advisory-lock statement: Cinder's and Nova's quota locks
      and the floating-IP allocation lock. Test: `TestZoneIsTakenUnderItsLockAndNotRetried` (a recording database:
      every zone statement under the lock, an injected unique violation answered, not re-run), RED on all three paths
      first; four mutations (each site unlocked, the helper taking nothing) caught. *Done 2026-11-27.*

## Phase 28: Pull-request review (2026-11-26)

- [X] T313 FR-029, FR-008: an update's undo put a floating IP's old association back without re-checking it. A fixed IP
      another floating IP of the network had taken meanwhile got a second one; a port that had gone or lost the fixed
      IP failed the undo, leaving the record on the change the node never applied; a router that no longer joined the
      fixed IP was put back. The undo now goes through `keepRestorableAssociation`, as the deletion's restore does
      (T270), and the helper locks as an association does — the port, its router, then the floating IP's row — taking a
      port that is gone as unassociated; the restore takes the port and router before the address's subnet, as a
      create does. The fixed-IP choice is shared with associations (`floatingPort.withFixedIP`). Migration 084 gains
      upstream's unique index on `floating_ips (floating_network_id, port_id, fixed_ip_address)`, edited in place: no
      database runs 084 yet. Tests: `undo_association_test.go` (five update-undo cases,
      `TestUndoAndRestoreLockAsAnAssociationDoes`), `TestMigration084`; RED on both backends first; seven code and
      five migration mutations caught. *Done 2026-11-27.*
- [X] T314 FR-041: an orphan TAP's frames addressed to this host passed. Both `tap+` guards asked for
      `--physdev-is-bridged`, which a frame the bridge delivers to the host never matches, so in INPUT an orphan's
      frame left `ls-local` undecided and the host's policy took it — over the bridge's automatic IPv6 link-local
      address, or any address a bridge is given. The guard on frames from a TAP no longer asks for it and takes them
      in FORWARD and INPUT alike; a bound TAP returns from `ls-bound-in` to its dispatch as before, so its frames to
      the host still go through its egress chain. Test: `port_firewall_orphan_test.go`, whose walker now follows a
      frame delivered to the host (RED first: the orphan's frame left undecided); the captured `iptables-save`
      fixtures recaptured from iptables-nft, only the six guard lines changing. *Done 2026-11-27.*
- [X] T315 FR-069, FR-039: a router interface port with several fixed IPs — one IPv4 subnet and IPv6 ones, which
      `add_router_interface` by port accepts as upstream's — had only its first fixed IP on its leg, so an IPv6-first
      port left its IPv4 subnet unrouted, and only its first subnet in `router_interfaces`, so the duplicate check, a
      removal and an undo saw that one alone: the router could take a second port on the other subnet, and a removal
      by port could strand a floating IP reaching it. As upstream's (neutron master, `l3_db.py`, `router_info.py`):
      every fixed IP is on the leg (`RouterInterface.Addresses`); a `router_interfaces` row per subnet the port joins;
      a removal by port, and an add's undo, check every subnet for floating IPs; a removal by one subnet of a port
      holding others takes that subnet's fixed IP and row off it, the network node reading the subnet as gone first
      (an absent id); a port with an IPv6 address joins no router with another IPv6 port on its network, the gateway
      port included (`400`, upstream's words); an IPv6 subnet added by subnet joins the router's IPv6 interface port
      on its network, and its undo puts the port's fixed IPs back, compare-and-set. IPv6 routing in the namespace
      stays out (`follow-ups.md` 6.8). Test: `router_interface_subnets_test.go`, RED first on nine of ten (the
      tenth, the join's undo, guards the new path; the gateway case added after); twelve mutations caught.
      *Done 2026-11-27.*
- [X] T316 FR-038: a gateway moved to a new port — another external network, whose subnet has the same gateway
      address — kept the default route on the old leg: the route was compared by its next hop alone, and the old
      leg, deleted after the tables, took the route along, so the router had none until the next pass. The held
      route is now read with its device and replaced when either differs, in the leg steps, before the old leg
      goes. Test: `TestConvergeRouter_TheDefaultRouteMovesWithTheGatewayLeg` (RED first: no replacement, the old
      leg deleted); two mutations caught (the comparison reverted; the device not read, which
      `TestConvergeRouter_UnchangedOnlyReads` catches). *Done 2026-11-27.*
- [X] T317 FR-065, FR-038 (T170's rule): four queries found a router's gateway port by owner and device alone. A
      gateway change the network node could not apply was undone from snapshots read that way, so a member's port
      naming the router — on a network of their own, at an address they chose — was taken for the gateway, deleted
      and written back without a project, and held as the gateway the node routes and proxies on the uplink, the real
      one left beside it. A router's deletion deleted a member's port naming it, and the reconciler took such a
      port's address for one Lightstack hands out. `gatewayPortWhere` now holds the predicate — the owner and no
      project — and every gateway-port query uses it, the six that spelled it out included. Test:
      `TestARouterNeverTakesAMembersPortForItsGateway` (RED first on both backends); four mutations caught; the
      router create's undo moved onto it too, unreachable for a member's port, which cannot name a router before it
      exists. *Done 2026-11-27.*

T057 (the rebase and renumbering to v1.6.0) stays last, after Phase 26.


---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (T001 → T002)** comes before everything. T002, the approval, gates every later task.
- **Foundational (T003–T007)** comes before every story. T003, T005 and T006 run in parallel; T004 needs T003; T007
  needs T005 and T006.
- **US1 (T008–T018)**: tests T008–T011 run in parallel. T013 needs T008. T014 → T015 → T016. T017 comes before T018.
  T018 needs T004 and T015.
- **US2 (T019–T021)**: needs T018. T019 and T020 run in parallel, then T021.
- **US3 (T022–T024)**: needs Foundational, and T018 for its remove case. T024 needs T004.
- **US4 (T025–T029)**: needs Foundational. T028 changes a signature every caller uses; run it after T017 and T018 have
  settled the interface. T029 needs T028.
- **US5**: no tasks. Covered by T006 and T010.
- **US6 (T030–T034)**: needs T004 and US1's T014–T015 (refresh, lock, seam). T033 → T034, which share `ports.go`.
- **US7 (T035–T048)**: needs US1's T013–T016. Tests T035–T041 run in parallel; T036 extends T009's fake. T042 needs
  T035. T043 needs T042. T044, T045 and T046 come after US6's T033–T034, which share `ports.go`. T046 needs T043. T047
  needs T046 and comes after US4's T028–T029, which share `internal/nova/handlers.go`. T048 needs T043.
- **Polish (T049–T057)**: after the stories; T057 waits for `main` to carry the other amendment, and runs last, after
  T168, before review. T051 comes after all code; T052 after T051; T053 before T054; T055 after T054. T057 runs at the
  rebase onto a `main` that carries the other v1.5.0 amendment, before review, whenever that happens.

### Story completion order

US1 → US2 → US3 → US4 → US6 → US7, then Polish. US5 completes with US1.

### Within each story

Tests first, seen failing (RED), or passing from the start for a **lock**. Then the implementation, in the order given.
Each story ends at a checkpoint where its independent test passes.

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
3. **US4** (T025–T029): changes made mid-boot reach later ports; `server_security_groups` is retired; create uses the
   shared resolver.
4. **US6** (T030–T034): Horizon's *Edit Security Groups*, `port set`, Terraform's port and data source.
5. **US7** (T035–T048): floating-IP and rule changes in force when answered or undone; a group in use cannot be deleted;
   a deleted port or server takes its floating IPs' association with it; reconciliation removes what no record holds.
   Until it lands, floating-IP and rule changes still log a failed firewall step and continue, which v1.6.0 does not
   permit. The feature is not complete without it.
6. **Polish** (T049–T057): docs, gates, mutation check, runbook, host validation, follow-up drafts, and the
   constitution's renumbering at the rebase.

Each increment leaves the earlier stories' tests green.

---

## Notes

- 57 tasks: Setup 2, Foundational 5, US1 11, US2 3, US3 3, US4 5, US5 0 (covered by T006 and T010), US6 5, US7 14,
  Polish 9.
- Nothing here commits. The user commits.
- `server_security_groups` stays in the schema, unused (R1). No task drops it.

---

## Round 2: dependencies and execution order

- **T058 → T059** before any round-2 test. **Phase 12** (T060–T070) blocks every later round-2 phase.
- **US8** (Phase 13) and **US9** (Phase 14) depend only on Phase 12, and can run in parallel with each other.
- **US11** (Phase 15) before the router API (Phase 16), which allocates gateway addresses, and before **US10** (Phase
  17), whose allocation uses T090's allocator and whose router lookup needs Phase 16's gateway ports. FR-039's checks
  (Phase 16) read floating IPs' `router_id` as stored today.
- **US12** (Phase 17a) after Phase 12; it touches every listing handler, so it runs after US10's edits to
  `floatingip.go` and before Phase 18.
- **Phase 18** (node call) after Phase 12; **Phases 19–21** (router, DHCP, port units) after Phase 18. Phase 19 after
  Phases 16 and 17. Phases 20 and 21 can run in parallel with 19 except where files overlap
  (`internal/neutron/units.go`: sequential).
- **Phase 22** (deletions) after Phases 19–21. **Phase 23** (reconciliation) after Phase 22. **Phase 24** (node) after
  Phase 20 (its `main.go` change follows T124's); its chart work (T151, T153) any time after Phase 12.
- **Phase 25** after everything; T165 → T166 → T168 → T057 (T167 dropped).
- **Phase 26** (the branch review's fixes) after Phase 25, before T057.

**Parallel within round 2**: every `[P]` test task of a phase together (different files); US8 ∥ US9; T068 ∥ T069 ∥ T070;
T112 ∥ T113 ∥ T119 ∥ T125 ∥ T126 (pure `pkg/networking` tests); T154–T159 docs together.

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


**Strategy**: round 2's first useful increment is Phases 11–17a — the four API findings, the router API and
administrators' access, no layout change, deployable on its own. Phases 18–24 change the real-mode layout together; they
are one increment, since a router namespace without the port firewall or DHCP namespace would leave the node half on
each layout.
