# Implementation Plan: Server security-group membership reaches the ports

**Branch**: `bugfix/579` (feature id: `033-sg-port-membership`) | **Date**: 2026-11-17 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `docs/specs/033-sg-port-membership/spec.md` (cloud-storage#579)

## Summary

The ports become the only list of a server's security groups. Every path writes them, every filter is rebuilt from them, and a server reports what its ports carry, as upstream does. Every change the node must apply — to membership, to a group's rules, to a floating IP's association — is in force on the serving node when the API answers, or answers `500` and is undone. Reconciliation brings every node into line in both directions. The work is mostly in Neutron's port, rule, floating-IP and dataplane code and Nova's two security-group actions.

**No new Go module, no schema change, no migration, no new configuration.** Several API answers change, all toward upstream; `docs/UPGRADING.md` lists each. Constitution amendment v1.5.0 was ratified on 2026-11-17, after the user chose it at the analysis gate: two Article II exceptions and register entry 5. The second analysis refined it in place on 2026-11-18, before it reached `main` (R19).

- **One list (R1, R10).** Nothing reads or writes `server_security_groups` any more; the table stays, unused. Show and detail read the server's ports: one entry per port and group, the key absent when there is none, and no invented `default`.
- **One write path (R2).** Package-level Neutron helpers write a port's groups inside the caller's transaction. `ResolveSecurityGroup` (name or id, the id winning) serves create, add and remove. `NeutronService` gains `RefreshPortFilters`.
- **Order (R3).** Membership changes lock the instance row and then the server's ports. The boot's allocation locks the instance row too, and each later port copies the boot's first port. So a change made mid-boot reaches the later ports.
- **Add and remove (R4).** As upstream:
  - every port changes;
  - a server with no port gets `404`;
  - an add finding the group already on any port gets `400`;
  - a remove finding it on none gets `404`.

  The last-group guard goes.
- **Failure (R5).** Commit, then apply on the serving node. If that fails, undo this request's own change and answer `500`. The undo is compare-and-set on every path: a record another request has changed since keeps that change. The old filter is still whole.
- **The filter (R6–R9).**
  - One `iptables-restore` commit replaces a chain, so it holds the old rules or the new ones.
  - Every command runs under a one-second context, through a command runner that tests can replace.
  - The filter goes in before the DNAT.
  - `EnsureIngressFilter` compares `iptables -S` with the rendering and leaves an equal chain alone.
  - One lock for the node's floating-IP state, acquired per address with a one-second deadline, keeps a pass from writing rules it read before a change.
  - The service reaches the node through one dataplane interface, which tests replace with a fake that holds state.
  - The per-port rebuild finds the port by its floating IP, so a port left with no group is rebuilt too.
- **Floating IPs and rules (R20, R21).** Create on a port, update and delete each store their change in the transaction that makes their checks, apply it, and undo it on failure. A failed delete restores the floating IP with the port forwardings deleted with it. A rule add or delete rebuilds the filter of every port carrying the group.
- **Group deletion (R22).** A group a port carries answers `409`, a missing one `404`, as upstream.
- **Port and server deletion (R25).** The floating IPs of a deleted port are disassociated in the deleting transaction, as upstream, and withdrawn before the answer. There are three paths: Neutron's port deletion, Nova's server deletion and a failed boot's release. They share one helper on the caller's transaction, and each locks the port, or the instance and then its ports, first. The `forceDelete` action now runs the server deletion too, as upstream's does; it used to delete only the instance row. A failed withdrawal is logged and left to reconciliation, because the port's teardown has no undo.
- **The NAT (R23).** Rules are marked as Lightstack's and change in one commit. The address is added last and removed first, so traffic reaches a fixed IP only through its filter.
- **Reconciliation (R24).** It lists the node, reads the records, and converges both ways. It removes marked state that no record holds, never a filter that any rule of the shape still reaches, and nothing after an incomplete read. It re-adds a floating address that has gone missing.
- **Attach (R11).** A port attached on a network gets `default`; a port attached by id keeps its groups.
- **Networking API (R12, R13).** Port create and update take `security_groups` as absent, `null`/`[]`, or a list. It is checked for format, repeats, then existence, and an update is undone (compare-and-set) on failure. The group listing honours `id` (repeatable) and `name`; other filters are a follow-up.
- **Out (R14).** eBPF: documentation only.
- **Governance (R15, R19).** Constitution v1.5.0:
  - "a change undone when its external work fails", for records whose node-local state the loop derives from them alone;
  - the reconciliation exception's scope stated, removals included;
  - register entry 5;
  - the remaining non-compliant request paths named in the migration plan as departures.

  `CLAUDE.md` gained two lines.

## Technical Context

**Language/Version**: Go 1.27.3 (`go.mod`). Markdown.
**Primary Dependencies**: `gin`; `internal/database` over pgx/pgxpool and `modernc.org/sqlite`; `google/uuid`. In real mode, `iptables-restore` and `iptables -S` from Alpine's `iptables` package, already in the image (`build/package/Dockerfile:41-46`), and `ip` from iproute2, which the NAT already uses. The NAT's mark needs iptables' `comment` match: the extension, which the package ships, and, with the legacy backend, the host kernel's `xt_comment` module. The runbook checks both (T053). `gophercloud` v1.14.1 for contract tests. **No new module.**
**Storage**: PostgreSQL or SQLite, chosen by `database.url`. No schema change, no migration. `server_security_groups` is retired, not dropped (R1).
**Testing**:
- `go test` on both backends (`LIGHTSTACK_TEST_DB_URL`). `internal/nova` drives the real Neutron service in stub mode, with a stand-in only to inject a failure or a change in between.
- `internal/neutron` on its database harness, with a fake dataplane that holds state. `pkg/networking` with pure functions, plus a fake command runner for the filter's and the NAT's real-mode paths.
- The contract suite (gophercloud) against a stub stack.
- The real-mode host run (FR-023, research R18; `quickstart.md`).

**Target Platform**: Linux real mode for the dataplane. Stub mode anywhere, macOS included.
**Project Type**: single Go project, plus docs and the constitution.
**Performance Goals**: none new.
- A membership or rule change adds one `iptables -S`, and at most one `iptables-restore`, per affected floating IP on the serving node. Each is bounded at one second.
- A floating-IP change reads each nat hook once, through the NAT method that changes it, and the uplink's addresses once. It writes at most one nat commit and one address change, and ensures at most one filter. A move or a removal also lists the node once, and removes the old filter in at most one commit.
- A reconciliation pass adds one `iptables -S` per floating IP, plus one listing of the nat hooks, the filter chains and the uplink's addresses. It writes only on a difference.
- Show and detail replace one query with one query.

**Constraints**:
- Article I: register entry 5 only; every other changed answer moves to upstream's.
- Article II (v1.5.0):
  - no transaction is held across node-local work;
  - a failed change to membership, rules or a floating IP is undone, compare-and-set, under "a change undone when its external work fails";
  - other nodes, a stopped process and a node whose own undo failed converge through the reconciliation loop under its stated scope, removals included.
- Article III: every firewall, NAT and address command, and every wait for the node's lock, is bounded at one second. Nothing is retried.
- A change is in force on the serving node when it answers, and on other nodes within one pass (30 s).

**Scale/Scope** (from the file reads in research):

| Area | Files |
|---|---|
| Neutron | `internal/neutron/ports.go`: helpers, `ResolveSecurityGroup`, the instance lock and copy-from in `AllocatePortForInstance`, `security_groups` in `CreatePort`/`UpdatePort`, filters in `ListSecurityGroups`, rule add and delete with undo, group delete's `409`/`404`, the floating IPs of a deleted port (`DisassociatePortFloatingIPs`, `DeletePort`, `ReleaseInstancePorts`, `ConvergeFloatingIPs`). `internal/neutron/floatingip.go`: `RefreshPortFilters`, floating-IP convergence, create, update and delete with undo. `internal/neutron/reconciler.go`: two-way convergence. `internal/neutron/network.go`: the node lock and the dataplane field. `internal/neutron/teardown.go`: the changed removal calls |
| Networking | `pkg/networking/northsouth.go`: `EnsureIngressFilter` reads, compares and applies one restore commit through a bounded command runner; the canonical renderer and comparison; the marked NAT converged in one commit, the address last; the filter removed in one commit; the node listing for reconciliation |
| Nova | `internal/nova/handlers.go`: `NeutronService`, create stops writing `server_security_groups` and uses the shared resolver, the boot's copy-from, the report in show and detail. `internal/nova/advanced_actions.go`: add and remove rewritten (compare-and-set undo), the report helpers, `formatSecurityGroups`. `internal/nova/interface_attach.go`: `default` on a `net_id` port. `DeleteServer`: one deletion, shared with `ForceDeleteInstanceAction`, with the instance locked first and the floating IPs of the deleted ports disassociated and converged (R25, FR-008e) |
| Tests | `internal/nova`: membership, report, attach and failure tests, with `server_detail_secgroups_test.go` reworked and the `boot_failure_test.go` fake extended. `internal/neutron`: helpers, port API, listing filters, refresh and undo, the node lock and its deadline, floating-IP, rule and group-delete paths, two-way reconciliation, against a fake dataplane. `pkg/networking`: restore payloads, canonical form, the NAT, the fake command runner. `test/contract/nova/security_group_actions_test.go` and the note at `server_actions_test.go:387`. `test/contract/neutron/`: port `security_groups`, the listing filters, group delete. |
| Docs | `docs/UPGRADING.md`, `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, `docs/EBPF_STATUS.md`, and `docs/specs/033-sg-port-membership/follow-ups.md`. `CLAUDE.md` is already done |
| Governance | `.specify/memory/constitution.md` → v1.5.0, ratified 2026-11-17 and refined 2026-11-18 (R19) |

No `NEEDS CLARIFICATION` remains. The spec's Clarifications (the clarification session and five analysis sessions) settled scope and answers, and research R1–R25 settle the design.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (below the table).*

| Principle (`.specify/memory/constitution.md` v1.4.0 at planning; v1.5.0 ratified 2026-11-17) | Verdict | Why |
|---|---|---|
| I. OpenStack Flamingo API compatibility | PASS | Every changed answer moves to upstream's, each read in upstream's source: add, remove, the last group, a server with no port, the report, attach, port `security_groups`, the listing's `id` and `name`, and group deletion's `409` and `404`. One recorded divergence: groups resolve only in the project they apply to, administrators included (FR-004, FR-018). It is register entry 5, in v1.5.0 (FR-021a), and meets all four limits: necessity (names: a caller succeeds today; ids: no cross-project group visibility in the data model), recorded as requirements, narrow, never silent (`404`). The other listing filters stay ignored, a pre-existing silent divergence. This feature does not mandate it, and it goes to a follow-up (R13). A `500` when the serving node cannot program its firewall or NAT is an internal failure, not an API divergence: upstream's agent applies asynchronously and has no synchronous answer to diverge from. |
| II. Synchronous operations | **PASS (v1.5.0, refined 2026-11-18)** — failed against v1.4.0, and against v1.5.0's first draft | A change answers only after the database write and the serving node's work: a membership change, a rule change, a floating IP's association. No transaction spans that work, and a failed one is answered as one transaction would have been: `500`, nothing changed. Against v1.4.0 this held only through a reading the text does not support. The reservation exception requires a transitional row, and the loop exception did not name the nodes that did not serve a change or a node whose own undo failed. v1.5.0's first draft excluded every create and removal, so a rule add or a floating-IP create would have had nowhere to go. Its loop only re-asserted, which leaves removals unfinished on other nodes. And it left floating IPs and rules logging and continuing, against its own rule for requests. The refined v1.5.0 covers each: "a change undone when its external work fails" for records whose node-local state the loop derives from them alone (R5, R20, R21 meet all five limits), and a loop that re-asserts and removes (R24). The request paths outside this feature that still log and continue are named in its migration plan as departures, and listed in the follow-ups. Three departures of those classes sit inside this feature's diff:
  - the LS-SG chain removal, which R22 moves after the commit; the chains enforce nothing;
  - the port forwardings' NAT, which a floating IP's deletion leaves behind, as today (R20);
  - a deleted port's floating IPs whose withdrawal fails, which are logged and left to reconciliation (R25). This narrows a departure that used to leave them for good.

  The first two are unchanged in effect. Every operation stores its change in one transaction that also makes its checks (R20, R21; limits 2 and 5). The boot stays asynchronous, as upstream's is. |
| III. Fail-fast | PASS | Every firewall, NAT and address command on the request path runs under a one-second context, and the wait for the node's lock has a one-second deadline. Overrunning either is a failed change, answered at once (FR-007b). Nothing is retried; the undo restores the earlier state and is not a retry. Row-lock waits follow the v1.3.0 exception (SQLite busy timeout ≤ 5 s). |
| IV. Multi-mode architecture | PASS | Stub mode stores, reports and orders the lists exactly as real mode does, and programs nothing. Every test runs without Linux except the host run. |
| V. Project isolation | PASS | Strengthened: a group never applies outside its own project, so no port carries a group its owner cannot see. |
| VI. Single binary | PASS | Nothing is added outside the binary. `iptables-restore` comes from the package that already provides `iptables`. |
| Technology constraints | PASS | No new module. |
| Forbidden patterns | PASS | No queue, worker or state machine. The node lock orders work inside one process with a bounded wait; it is not a queue. The reconciliation loop is v1.5.0's stated exception: it reads records and the node, and accepts no work from a request. |
| Test-first | PASS by plan | Each requirement's test is written first and seen failing (R16). The earlier-release case is red in its US1 batch, no longer written after its code. |
| Backwards compatibility | PASS | "Breaking API changes require OpenStack microversion bumps" yields to Article I under Governance rule 1 ("OpenStack API compatibility always wins"). Upstream answers this way at every microversion, and the Networking API has no microversions, so a Lightstack bump would itself diverge. `docs/UPGRADING.md` lists each change (FR-020). NAT rules from earlier releases are replaced once their address has a record; the rest are documented, not shimmed. No migration. |
| Security | PASS | Closes six holes. A removed group kept admitting traffic. A rebuild left a moment without `DROP` every 30 s per floating IP. Horizon's *Edit Security Groups*, once the port API works, would have put every project group on an instance. A deleted or moved floating IP kept forwarding to its old fixed IP on every node that did not serve the change, and on the serving node after a failed withdrawal. An address allocated again was forwarded to the old fixed IP by the older rule. A group deleted while in use left its ports' filters unrebuilt. |
| Quality gates before merge | PASS by plan | Both modes: stub tested, real on the host per FR-023. Both database backends. No migration, so the up/down gate does not apply. |

**Gate decision (2026-11-17)**: the first verdict, PASS against v1.4.0, was wrong. The analysis found Article II justified in this plan instead of amended, which the constitution's v1.2.0 rationale rules out. The user chose to amend. Research R19 holds the reasoning, and v1.5.0 was ratified on 2026-11-17 (`.specify/memory/constitution.md`). **Task generation may proceed**; `tasks.md` was regenerated to match.

**Gate decision (2026-11-18)**: the second analysis found this plan claiming Article II while the spec kept floating-IP association and rule changes logging and continuing, against v1.5.0's own rule that a request applies its node-local change or fails and undoes it. The user asked for the real fix. The paths were brought in (R20–R22). v1.5.0 was refined in place where its first draft was too narrow or silent (R19). The NAT became recognisable, so the loop can remove what no record holds (R23, R24). `tasks.md` was regenerated to match.

**Gate decision (2026-11-18, third analysis)**: the third analysis found two conflicts with v1.5.0's limits and two safety gaps, all fixed. Each fix is the real one the user asked for:
- **The delete.** It stored its deletion after the node work, in a second transaction (limit 5). It is now one transaction, and a failed withdrawal restores the floating IP with its port forwardings (R20).
- **The creates.** They checked outside their first transaction (limit 2). The checks now run in the transaction that inserts (R20, R21).
- **Unmarked rules.** Reconciliation could remove a filter that an unmarked rule still reached. It now keeps it (R23, R24).
- **Addresses.** The pass compared no addresses, so it could not re-add a lost one. It now does (R24).
- **Uplink and partial reads.** A missing uplink is a failed change, and the record read has a seam for partial reads (R9, R24).

The version collision is settled for the rebase: another amendment numbered v1.5.0 reaches `main` first, so this one becomes v1.6.0 then (T057).

**Gate decision (2026-11-18, fourth analysis)**: no constitution conflict. Fixed:
- The third analysis's locks used `FOR SHARE`, which the shared SQL subset does not have. Locks are now `FOR UPDATE`, and only where a check reads state another request can change (R20, R21).
- A deleted port left its floating IPs half associated and forwarding for up to a pass. They are now disassociated in the deleting transaction and withdrawn before the answer (R25, a new task).
- The pass read only associated floating IPs. It now reads every record (R24).
- T016's uplink, T033's check placement, and two wordings were tightened.

**Gate decision (2026-11-18, fifth analysis)**: no constitution conflict.
- R25 had taken `ReleaseInstancePorts` for server deletion. That function runs only when a boot fails; servers are deleted by Nova's own transaction. The fix now covers all three paths with one helper (R25; tasks T041, T046, T047).
- The deleting transactions now lock the port, or the instance and then its ports, before they read the floating IPs. That also removes server deletion's existing lock inversion with membership changes and volume attach.

**Gate decision (2026-11-18, sixth analysis)**: no constitution conflict.
- `forceDelete` deleted only the instance row, so SC-014 did not hold for it. It now shares `DeleteServer`'s deletion, as upstream's `force_delete` does (FR-008e, R25, T041, T047).
- The instance locks' missing-row cases are stated, the file of `ConvergeFloatingIPs` is named, and R25 no longer claims a deadlock with volume attach that cannot happen; it is a race.

**Post-design re-check (after Phase 1)**: unchanged.
- The design adds four narrow seams:
  - the dataplane interface and the uplink function on the Neutron service (R9);
  - the record read on the reconciler (R24);
  - the command runner on the `RouterManager` (R6).

  It adds one bounded in-process lock for the node (R8). It reuses `LockSubnet`'s pattern for row locks, `SQLFilter` for the listing, `NewResourceInUseError` for the `409`, and `pruneDeletedNetworks`' list-first, skip-when-incomplete rule for removal.
- The contracts (`contracts/*.md`) pin existing endpoints. They add one field to port update, two filters to the group listing and two answers to group deletion, all upstream's, and they state the bounds and the dataplane's order.
- No new divergence beyond entry 5.

## Project Structure

### Documentation (this feature)

```text
docs/specs/033-sg-port-membership/
├── spec.md                       # clarification (9 answers) and five analysis sessions (the first to fourth and the sixth: 4, 5, 4, 2 and 1 answers); the fifth reworded FR-008d only
├── plan.md                       # this file
├── research.md                   # Phase 0: R1–R25 (R19 = constitution v1.5.0)
├── data-model.md                 # Phase 1: tables touched, a port's groups, a floating IP's association, locks, per-node state
├── quickstart.md                 # Phase 1: gates, stub-mode walk, host validation (a)–(h)
├── contracts/
│   ├── compute-security-groups.md   # add/remove answers and effects, attach, boot
│   ├── server-report.md             # security_groups in show/detail
│   ├── network-api.md               # port security_groups; group listing filters; floating IPs, rules, group delete
│   └── floating-ip-dataplane.md     # filter and NAT content, order, atomic changes, reconciliation, bounds
├── checklists/requirements.md
├── tasks.md                      # Phase 2
└── follow-ups.md                 # drafted in Polish: tickets for the user to file
```

### Source Code (repository root)

```text
internal/neutron/
├── ports.go          # PortSecurityGroupIDs/Add/Remove/SetPortSecurityGroups, ResolveSecurityGroup;
│                     # AllocatePortForInstance: instance lock, copyGroupsFrom;
│                     # CreatePort/UpdatePort: security_groups (raw), validation, compare-and-set undo;
│                     # ListSecurityGroups: id, name; rule add/delete with undo; group delete 409/404
├── floatingip.go     # RefreshPortFilters (by floating_ips.port_id); convergence of a floating IP under
│                     # the node lock; create/update/delete stored, applied, undone
├── reconciler.go     # lists the node, reads the records, converges both ways
├── network.go        # Service: the node lock, the dataplane interface
├── teardown.go       # the removal calls' changed signatures
└── *_test.go         # DB-backed against a fake dataplane: helpers, port API, filters, refresh, undo,
                      # lock, floating IPs, rules, group delete, reconciliation

pkg/networking/
├── northsouth.go     # EnsureIngressFilter: read, compare, one iptables-restore commit; command runner
│                     # (one-second context); canonical renderer; comparison; the marked NAT in one
│                     # commit, the address last; filter removal in one commit; the node listing
└── *_test.go         # payloads and canonical form (pure); real-mode paths with a fake runner

internal/nova/
├── handlers.go       # NeutronService.RefreshPortFilters and ConvergeFloatingIPs; create: shared resolver,
│                     # no server_security_groups, boot copy-from; show/detail read ports;
│                     # DeleteServer and forceDelete: one deletion, the instance locked first, deleted
│                     # ports' floating IPs disassociated
├── advanced_actions.go  # add/remove (every port, 404/400, no guard, compare-and-set undo); report helpers
├── interface_attach.go  # net_id port gets default
└── *_test.go         # real Neutron in stub mode; stand-in only for failures and changes in between

test/contract/nova/security_group_actions_test.go   # ports.Get(...).SecurityGroups after each change
test/contract/neutron/                              # port security_groups; listing filters; Horizon sequence;
                                                    # group delete 409/404
docs/UPGRADING.md, docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md, docs/EBPF_STATUS.md
.specify/memory/constitution.md, CLAUDE.md          # v1.5.0 and its two runtime-guidance lines (done)
```

**Structure Decision**: the existing single-project layout. No new package, and no new source file outside tests. The dataplane change stays in `pkg/networking/northsouth.go`, beside the function it replaces.

## Complexity Tracking

All three rows are resolved by the constitution amendment (v1.5.0, research R19), not by this table. They are listed because the departures are real, and the v1.2.0 rationale requires that they be visible.

| Violation | Why needed | Simpler alternative rejected because |
|---|---|---|
| Two transactions around the node-local work, with an undo and no transitional row, for membership, rules and floating IPs (Article II) | The firewall and the NAT cannot be inside a database transaction, and on SQLite a held transaction blocks the whole file. These records are complete without the work, because the loop derives it from them alone, so there is no state between before and after for a transitional row to express. | A transitional row (a `pending` flag, a changes table, a new floating-IP status) is a migration no reader consults, or a state readers would have to learn to skip. Reusing `ports.status` changes what clients see for a reason upstream never shows. Applying before committing needs a retry when a concurrent change makes the base stale, and it breaks the list-first removal (R24). |
| Convergence through the reconciliation loop for other nodes, a stopped process, and a node whose own undo failed (Article II) | No request can reach another node, or repair a node after it has answered, without inter-node messaging. | Pushing a change to every node from the request needs a queue, which is forbidden. Failing the request when another node cannot be reached would make every change depend on every node. |
| The loop removes node state no record holds (Article II) | A removal served by one node must reach the others, and a stopped process or a failed undo can leave state that no record holds; an address allocated again would otherwise be forwarded to the old fixed IP. | Leaving removals to the requests: the nodes that did not serve one never learn of it. Removing by shape: an operator's own rules would be taken. So the NAT is marked (R23). |
