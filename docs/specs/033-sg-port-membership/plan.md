# Implementation Plan: Server security-group membership reaches the ports

**Branch**: `bugfix/579` (feature id: `033-sg-port-membership`) | **Date**: 2026-11-17, round 2 2026-11-20 | **Spec**:
[spec.md](spec.md)
**Input**: Feature specification from `docs/specs/033-sg-port-membership/spec.md` (cloud-storage#579)

## Summary

The ports become the only list of a server's security groups. Every path writes them, every filter is rebuilt from them,
and a server reports what its ports carry, as upstream does. Every change the node must apply — to membership, to a
group's rules, to a floating IP's association — is in force on the serving node when the API answers, or answers `500`
and is undone. Reconciliation brings every node into line in both directions. The work is mostly in Neutron's port,
rule, floating-IP and dataplane code and Nova's two security-group actions.

**Round 1** (below, US1–US7, implemented as `dcda565`): no new Go module, no schema change, no migration, no new
configuration. Several API answers change, all toward upstream; `docs/UPGRADING.md` lists each. Constitution amendment
v1.6.0 was ratified on 2026-11-17, after the user chose it at the analysis gate: two Article II exceptions and register
entry 5. The second analysis refined it in place on 2026-11-18, before it reached `main` (R19).

- **One list (R1, R10).** Nothing reads or writes `server_security_groups` any more; the table stays, unused. Show and
  detail read the server's ports: one entry per port and group, the key absent when there is none, and no invented
  `default`.
- **One write path (R2).** Package-level Neutron helpers write a port's groups inside the caller's transaction.
  `ResolveSecurityGroup` (name or id, the id winning) serves create, add and remove. `NeutronService` gains
  `RefreshPortFilters`.
- **Order (R3).** Membership changes lock the instance row and then the server's ports. The boot's allocation locks the
  instance row too, and each later port copies the boot's first port. So a change made mid-boot reaches the later ports.
- **Add and remove (R4).** As upstream:
  - every port changes;
  - a server with no port gets `404`;
  - an add finding the group already on any port gets `400`;
  - a remove finding it on none gets `404`.

  The last-group guard goes.
- **Failure (R5).** Commit, then apply on the serving node. If that fails, undo this request's own change and answer
  `500`. The undo is compare-and-set on every path: a record another request has changed since keeps that change. The
  old filter is still whole.
- **The filter (R6–R9).**
  - One `iptables-restore` commit replaces a chain, so it holds the old rules or the new ones.
  - Every command runs under a one-second context, through a command runner that tests can replace.
  - The filter goes in before the DNAT.
  - `EnsureIngressFilter` compares `iptables -S` with the rendering and leaves an equal chain alone.
  - One lock for the node's floating-IP state, acquired per address with a one-second deadline, keeps a pass from
    writing rules it read before a change.
  - The service reaches the node through one dataplane interface, which tests replace with a fake that holds state.
  - The per-port rebuild finds the port by its floating IP, so a port left with no group is rebuilt too.
- **Floating IPs and rules (R20, R21).** Create on a port, update and delete each store their change in the transaction
  that makes their checks, apply it, and undo it on failure. A failed delete restores the floating IP with the port
  forwardings deleted with it. A rule add or delete rebuilds the filter of every port carrying the group.
- **Group deletion (R22).** A group a port carries answers `409`, a missing one `404`, as upstream.
- **Port and server deletion (R25).** The floating IPs of a deleted port are disassociated in the deleting transaction,
  as upstream, and withdrawn before the answer. There are three paths: Neutron's port deletion, Nova's server deletion
  and a failed boot's release. They share one helper on the caller's transaction, and each locks the port, or the
  instance and then its ports, first. The `forceDelete` action now runs the server deletion too, as upstream's does; it
  used to delete only the instance row. A failed withdrawal is logged and left to reconciliation, because the port's
  teardown has no undo.
- **The NAT (R23).** Rules are marked as Lightstack's and change in one commit. The address is added last and removed
  first, so traffic reaches a fixed IP only through its filter.
- **Reconciliation (R24).** It lists the node, reads the records, and converges both ways. It removes marked state that
  no record holds, never a filter that any rule of the shape still reaches, and nothing after an incomplete read. It
  re-adds a floating address that has gone missing.
- **Attach (R11).** A port attached on a network gets `default`; a port attached by id keeps its groups.
- **Networking API (R12, R13).** Port create and update take `security_groups` as absent, `null`/`[]`, or a list. It is
  checked for format, repeats, then existence, and an update is undone (compare-and-set) on failure. The group listing
  honours `id` (repeatable) and `name`; other filters are a follow-up.
- **Out (R14).** eBPF: documentation only.
- **Governance (R15, R19).** Constitution v1.6.0:
  - "a change undone when its external work fails", for records whose node-local state the loop derives from them alone;
  - the reconciliation exception's scope stated, removals included;
  - register entry 5;
  - the remaining non-compliant request paths named in the migration plan as departures.

  `CLAUDE.md` gained two lines.

### Round 2 (2026-11-20): the follow-ups and the router namespace

The user moved the four follow-ups into this feature (spec Sessions 2026-11-19 and 2026-11-20). Round 2 replaces the
real-mode network layout with upstream's, and adds the API answers the follow-ups named. **No new Go module. Two
migrations (084 and 085). One new setting (`neutron.network_node`). The constitution was refined at the gate (R42).** No
deployment exists, so the layout change is a clean break (FR-044).

- **API (R30, R31, R36, R38, R39, R46, R47, R49).** The group listing honours every filter; groups carry upstream's
  revision number and may be stateless. `default` cannot be deleted by a non-administrator, renamed or duplicated; other
  names may repeat, as upstream, an ambiguous one answering `409` (R53). New groups start with upstream's rules. Subnets
  store allocation pools, and one allocator, under the subnet's lock, serves every automatic address. Floating-IP
  creates serialise under an allocation lock, need a router joining the port's subnet to the floating network, and allow
  one floating IP per fixed IP per network. Routers get gateway ports with `external_fixed_ips` and refuse to drop a
  path a floating IP uses. Answers that had no requirement now have one (FR-052–FR-056).
- **One network node (R26, R27).** It holds every `qrouter-*` and `qdhcp-*` namespace. Other nodes ask it to apply
  through an authenticated, stateless call naming a unit, bounded at one second, the target's lock and apply sharing a
  900 ms deadline; a port's chains are applied on its bound node the same way, and removed from its old node when the
  binding moves.
- **Units (R28, R29, R32, R34).** A router, a network's DHCP and a node's ports are each rendered from the records and
  converged — or converged to "absent" before a deletion's record goes, re-applied if anything fails.
- **Router namespace (R28).** Upstream's legacy L3 layout: `qg-` on a routed veth with proxy ARP on the uplink, `qr-` on
  the bridge holding the gateway, upstream's NAT and port-forwarding rules in Lightstack chains.
- **DHCP and metadata (R29).** A `network:dhcp` port per network, `dnsmasq` with static ranges in `qdhcp-*`, and
  Lightstack's metadata listener inside the namespace identifying instances by network and address.
- **Port firewall (R32, R33, R47).** Upstream's iptables bridge driver: per-TAP `physdev` dispatch, per-port chains,
  anti-spoofing, remote groups expanded, conntrack zones per network, `CT --notrack` for stateless groups. The VM's TAP
  gets upstream's name; `binding:host_id` records its node; chains are in force before the guest starts. The floating-IP
  filter is retired, and the eBPF step goes with Neutron's TAP.
- **The node (R35, R48).** The chart mounts `/run/netns` bidirectionally and `/lib/modules`; Lightstack loads
  `br_netfilter`, sets the bridge sysctls, and is not ready until they hold. Teardown removes the new layout.
- **Article II everywhere (R34, R40).** Every path that logged a failed host step now converges a unit with FR-008's
  undo or FR-040's order. The constitution's migration plan names no departure.
- **Seams (R41).** Four interfaces with stateful fakes; two-node tests run in one process through the fake node caller.

## Technical Context

**Language/Version**: Go 1.27.3 (`go.mod`). Markdown.
**Primary Dependencies**: `gin`; `internal/database` over pgx/pgxpool and `modernc.org/sqlite`; `google/uuid`. In real
mode, `iptables-restore` and `iptables -S` from Alpine's `iptables` package, already in the image
(`build/package/Dockerfile:41-46`), and `ip` from iproute2, which the NAT already uses. The NAT's mark needs iptables'
`comment` match: the extension, which the package ships, and, with the legacy backend, the host kernel's `xt_comment`
module. The runbook checks both (T053). `gophercloud` v1.14.1 for contract tests. **No new module.**
**Storage**: PostgreSQL or SQLite, chosen by `database.url`. Round 1: no schema change; `server_security_groups` is
retired, not dropped (R1). Round 2: migrations 084 and 085, both `.postgres`/`.sqlite` pairs (085 drops the unique
group-name constraint; its SQLite file runs with foreign keys off through a migrator change, R53); 084
(`084_<name>.postgres.up.sql` etc., per `migrations/README.md`) — `subnet_allocation_pools`, `subnets.pools_stored`,
`ports.binding_host_id`, `networks.ct_zone` with a unique index, `security_groups.revision_number` and `stateful`, and
the seeded `default`'s member rules (data-model.md).
**Testing**:
- `go test` on both backends (`LIGHTSTACK_TEST_DB_URL`). `internal/nova` drives the real Neutron service in stub mode,
  with a stand-in only to inject a failure or a change in between.
- `internal/neutron` on its database harness, with a fake dataplane that holds state. `pkg/networking` with pure
  functions, plus a fake command runner for the filter's and the NAT's real-mode paths.
- The contract suite (gophercloud) against a stub stack.
- The real-mode host run (FR-023, research R18; `quickstart.md`).
- Round 2: fakes for the router, DHCP and port units and the node caller (R41); pure renderers in `pkg/networking`;
  `TestSchemaParity`/`TestMigrationsRoundTrip` for 084 and 085; `helm unittest charts/lightstack` for the mounts; one
  host run, the chart on one Kubernetes node (with FR-023's checks restated) (FR-048, R45). The two-node run was dropped
  on 2026-11-23: two nodes are not supported yet (spec Session 2026-11-23, T167).

**Target Platform**: Linux real mode for the dataplane, usually the chart on a Kubernetes node (privileged,
`hostNetwork`). Stub mode anywhere, macOS included. Round 2 real mode needs `br_netfilter` loadable, `ip netns`, BusyBox
`modprobe` (in the image), and the host's `/run/netns` and `/lib/modules`.
**Project Type**: single Go project, plus docs and the constitution.
**Performance Goals**: none new.
- A membership or rule change adds one `iptables -S`, and at most one `iptables-restore`, per affected floating IP on
  the serving node. Each is bounded at one second.
- A floating-IP change reads each nat hook once, through the NAT method that changes it, and the uplink's addresses
  once. It writes at most one nat commit and one address change, and ensures at most one filter. A move or a removal
  also lists the node once, and removes the old filter in at most one commit.
- A reconciliation pass adds one `iptables -S` per floating IP, plus one listing of the nat hooks, the filter chains and
  the uplink's addresses. It writes only on a difference.
- Show and detail replace one query with one query.

**Constraints**:
- Article I: register entry 5 (names only, R52); every other changed answer moves to upstream's.
- Article II (v1.6.0):
  - no transaction is held across node-local work;
  - a failed change to membership, rules or a floating IP is undone, compare-and-set, under "a change undone when its
    external work fails";
  - other nodes, a stopped process and a node whose own undo failed converge through the reconciliation loop under its
    stated scope, removals included.
- Article III: every firewall, NAT and address command, and every wait for the node's lock, is bounded at one second.
  Nothing is retried.
- Round 1: a change is in force on the serving node when it answers, and on other nodes within one pass (30 s). Round 2:
  in force on the node that holds the state — the network node or the port's node — when it answers, through the node
  call; other nodes' bridges, VXLAN and FDB within one pass.
- Round 2, Article II as refined by R42: a request applies on the node that holds the state, asked synchronously; a
  deletion removes host state before its record (the new exception) and re-applies on failure.
- Round 2, Article III: the node call is one bounded step of one second; the target's lock wait and apply share 900 ms.

**Scale/Scope** (from the file reads in research):

| Area | Files |
|---|---|
| Neutron | `internal/neutron/ports.go`: helpers, `ResolveSecurityGroup`, the instance lock and copy-from in `AllocatePortForInstance`, `security_groups` in `CreatePort`/`UpdatePort`, filters in `ListSecurityGroups`, rule add and delete with undo, group delete's `409`/`404`, the floating IPs of a deleted port (`ReleaseDeletedPorts` (was `DisassociatePortFloatingIPs`), `DeletePort`, `ReleaseInstancePorts`, `ConvergeFloatingIPs`). `internal/neutron/floatingip.go`: `RefreshPortFilters`, floating-IP convergence, create, update and delete with undo. `internal/neutron/reconciler.go`: two-way convergence. `internal/neutron/network.go`: the node lock and the dataplane field. `internal/neutron/teardown.go`: the changed removal calls |
| Networking | `pkg/networking/northsouth.go`: `EnsureIngressFilter` reads, compares and applies one restore commit through a bounded command runner; the canonical renderer and comparison; the marked NAT converged in one commit, the address last; the filter removed in one commit; the node listing for reconciliation |
| Nova | `internal/nova/handlers.go`: `NeutronService`, create stops writing `server_security_groups` and uses the shared resolver, the boot's copy-from, the report in show and detail. `internal/nova/advanced_actions.go`: add and remove rewritten (compare-and-set undo), the report helpers, `formatSecurityGroups`. `internal/nova/interface_attach.go`: `default` on a `net_id` port. `DeleteServer`: one deletion, shared with `ForceDeleteInstanceAction`, with the instance locked first and the floating IPs of the deleted ports disassociated and converged (R25, FR-008e) |
| Tests | `internal/nova`: membership, report, attach and failure tests, with `server_detail_secgroups_test.go` reworked and the `boot_failure_test.go` fake extended. `internal/neutron`: helpers, port API, listing filters, refresh and undo, the node lock and its deadline, floating-IP, rule and group-delete paths, two-way reconciliation, against a fake dataplane. `pkg/networking`: restore payloads, canonical form, the NAT, the fake command runner. `test/contract/nova/security_group_actions_test.go` and the note at `server_actions_test.go:387`. `test/contract/neutron/`: port `security_groups`, the listing filters, group delete. |
| Docs | `docs/UPGRADING.md`, `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`, `docs/EBPF_STATUS.md`, and `docs/specs/033-sg-port-membership/follow-ups.md`. `CLAUDE.md` is already done |
| Governance | `.specify/memory/constitution.md` → v1.6.0, ratified 2026-11-17 and refined 2026-11-18 (R19) |
| Round 2, Neutron | `ports.go` (listing filters, `default`, names, revision, stateless, starting rules, allocator, `binding_host_id`, the eBPF step removed), `security_group_default.go`, `network.go` (subnets and pools, network create/delete, the setting, the unit locks and seams), `router.go` (gateway port, interfaces in one transaction, FR-039, the router unit), `floatingip.go` (allocation lock, router lookup, FR-029, FR-054, the router unit), `port_forwarding.go`, `reconciler.go` (roles, units, retired state, FDB and VXLAN removal), `teardown.go` (R48), `auto_allocated_topology.go`, `vxlan_coordinator.go`; new: `node_call.go`, `units.go` (router, DHCP, ports), `allocation.go` |
| Round 2, networking | new: `pkg/networking/router_namespace.go`, `dhcp_namespace.go`, `port_firewall.go` (renderers and real-mode apply through the command runner), `nodesettings.go` (`br_netfilter`); `router.go` and `northsouth.go` lose the host-namespace NAT, filter and SNAT; `security_groups.go` loses the `LS-SG-*` chains; `dhcp.go` and `metadata_redirect.go` reworked or retired; `pkg/hypervisor/xml_template.go` (`<target dev>`) |
| Round 2, elsewhere | `internal/metadata` (listener per namespace, identity by network); `internal/nova` (TAP name, `binding_host_id`, chains before the guest starts, stateful/stateless conflict on add and boot); `internal/common` (config key, explicit Neutron error type); `internal/health` + `cmd/lightstack/main.go` (start-up check, readiness); `internal/compute/node_registry.go` (node name); `internal/teardown` (prefix fallback); `internal/database` (the foreign-keys-off marker, R53); `migrations/084_*`, `migrations/085_*`, `migrations/README.md`; `charts/lightstack` (mounts, teardown job, tests, README); `deployments/docker-compose-kvm.yml`; `test/security_groups_test.sh` |
| Round 2, docs and governance | `docs/UPGRADING.md`, `EBPF_STATUS.md`, `NETWORKING_MODES.md`, `VXLAN_IMPLEMENTATION.md`, `CONFIGURATION.md`, `TROUBLESHOOTING.md`, `SCALING.md`; constitution (R42, refined at the gate); `CLAUDE.md` |

No `NEEDS CLARIFICATION` remains. The spec's Clarifications (the clarification session and five analysis sessions)
settled scope and answers, and research R1–R25 settle the design. Round 2: Sessions 2026-11-19, 2026-11-20 and the
round-2 analysis, and research R26–R53.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (below the table).*

| Principle (`.specify/memory/constitution.md` v1.4.0 at planning; v1.6.0 ratified 2026-11-17) | Verdict | Why |
|---|---|---|
| I. OpenStack Flamingo API compatibility | PASS | Every changed answer moves to upstream's, each read in upstream's source: add, remove, the last group, a server with no port, the report, attach, port `security_groups`, the listing's `id` and `name`, and group deletion's `409` and `404`. One recorded divergence: groups resolve only in the project they apply to, administrators included (FR-004, FR-018). It is register entry 5, in v1.6.0 (FR-021a), and meets all four limits: necessity (names: a caller succeeds today; ids: no cross-project group visibility in the data model), recorded as requirements, narrow, never silent (`404`). The other listing filters stay ignored, a pre-existing silent divergence. This feature does not mandate it, and it goes to a follow-up (R13). A `500` when the serving node cannot program its firewall or NAT is an internal failure, not an API divergence: upstream's agent applies asynchronously and has no synchronous answer to diverge from. |
| II. Synchronous operations | **PASS (v1.6.0, refined 2026-11-18)** — failed against v1.4.0, and against v1.6.0's first draft | A change answers only after the database write and the serving node's work: a membership change, a rule change, a floating IP's association. No transaction spans that work, and a failed one is answered as one transaction would have been: `500`, nothing changed. Against v1.4.0 this held only through a reading the text does not support. The reservation exception requires a transitional row, and the loop exception did not name the nodes that did not serve a change or a node whose own undo failed. v1.6.0's first draft excluded every create and removal, so a rule add or a floating-IP create would have had nowhere to go. Its loop only re-asserted, which leaves removals unfinished on other nodes. And it left floating IPs and rules logging and continuing, against its own rule for requests. The refined v1.6.0 covers each: "a change undone when its external work fails" for records whose node-local state the loop derives from them alone (R5, R20, R21 meet all five limits), and a loop that re-asserts and removes (R24). The request paths outside this feature that still log and continue are named in its migration plan as departures, and listed in the follow-ups. Three departures of those classes sit inside this feature's diff:
  - the LS-SG chain removal, which R22 moves after the commit; the chains enforce nothing;
  - the port forwardings' NAT, which a floating IP's deletion leaves behind, as today (R20);
  - a deleted port's floating IPs whose withdrawal fails, which are logged and left to reconciliation (R25). This
    narrows a departure that used to leave them for good.

  The first two are unchanged in effect. Every operation stores its change in one transaction that also makes its checks
  (R20, R21; limits 2 and 5). The boot stays asynchronous, as upstream's is. |
| III. Fail-fast | PASS | Every firewall, NAT and address command on the request path runs under a one-second context, and the wait for the node's lock has a one-second deadline. Overrunning either is a failed change, answered at once (FR-007b). Nothing is retried; the undo restores the earlier state and is not a retry. Row-lock waits follow the v1.3.0 exception (SQLite busy timeout ≤ 5 s). |
| IV. Multi-mode architecture | PASS | Stub mode stores, reports and orders the lists exactly as real mode does, and programs nothing. Every test runs without Linux except the host run. |
| V. Project isolation | PASS | Strengthened: a group never applies outside its own project, so no port carries a group its owner cannot see. |
| VI. Single binary | PASS | Nothing is added outside the binary. `iptables-restore` comes from the package that already provides `iptables`. |
| Technology constraints | PASS | No new module. |
| Forbidden patterns | PASS | No queue, worker or state machine. The node lock orders work inside one process with a bounded wait; it is not a queue. The reconciliation loop is v1.6.0's stated exception: it reads records and the node, and accepts no work from a request. |
| Test-first | PASS by plan | Each requirement's test is written first and seen failing (R16). The earlier-release case is red in its US1 batch, no longer written after its code. |
| Backwards compatibility | PASS | "Breaking API changes require OpenStack microversion bumps" yields to Article I under Governance rule 1 ("OpenStack API compatibility always wins"). Upstream answers this way at every microversion, and the Networking API has no microversions, so a Lightstack bump would itself diverge. `docs/UPGRADING.md` lists each change (FR-020). NAT rules from earlier releases are replaced once their address has a record; the rest are documented, not shimmed. Round 1: no migration (round 2 adds 084). |
| Security | PASS | Closes six holes. A removed group kept admitting traffic. A rebuild left a moment without `DROP` every 30 s per floating IP. Horizon's *Edit Security Groups*, once the port API works, would have put every project group on an instance. A deleted or moved floating IP kept forwarding to its old fixed IP on every node that did not serve the change, and on the serving node after a failed withdrawal. An address allocated again was forwarded to the old fixed IP by the older rule. A group deleted while in use left its ports' filters unrebuilt. |
| Quality gates before merge | PASS by plan | Both modes: stub tested, real on the host per FR-023. Both database backends. No migration, so the up/down gate does not apply. |

**Gate decision (2026-11-17)**: the first verdict, PASS against v1.4.0, was wrong. The analysis found Article II
justified in this plan instead of amended, which the constitution's v1.2.0 rationale rules out. The user chose to amend.
Research R19 holds the reasoning, and v1.6.0 was ratified on 2026-11-17 (`.specify/memory/constitution.md`). **Task
generation may proceed**; `tasks.md` was regenerated to match.

**Gate decision (2026-11-18)**: the second analysis found this plan claiming Article II while the spec kept floating-IP
association and rule changes logging and continuing, against v1.6.0's own rule that a request applies its node-local
change or fails and undoes it. The user asked for the real fix. The paths were brought in (R20–R22). v1.6.0 was refined
in place where its first draft was too narrow or silent (R19). The NAT became recognisable, so the loop can remove what
no record holds (R23, R24). `tasks.md` was regenerated to match.

**Gate decision (2026-11-18, third analysis)**: the third analysis found two conflicts with v1.6.0's limits and two
safety gaps, all fixed. Each fix is the real one the user asked for:
- **The delete.** It stored its deletion after the node work, in a second transaction (limit 5). It is now one
  transaction, and a failed withdrawal restores the floating IP with its port forwardings (R20).
- **The creates.** They checked outside their first transaction (limit 2). The checks now run in the transaction that
  inserts (R20, R21).
- **Unmarked rules.** Reconciliation could remove a filter that an unmarked rule still reached. It now keeps it (R23,
  R24).
- **Addresses.** The pass compared no addresses, so it could not re-add a lost one. It now does (R24).
- **Uplink and partial reads.** A missing uplink is a failed change, and the record read has a seam for partial reads
  (R9, R24).

The version collision was settled at the rebase: another amendment numbered v1.5.0 reached `main` first, so this one
became v1.6.0 (T057).

**Gate decision (2026-11-18, fourth analysis)**: no constitution conflict. Fixed:
- The third analysis's locks used `FOR SHARE`, which the shared SQL subset does not have. Locks are now `FOR UPDATE`,
  and only where a check reads state another request can change (R20, R21).
- A deleted port left its floating IPs half associated and forwarding for up to a pass. They are now disassociated in
  the deleting transaction and withdrawn before the answer (R25, a new task).
- The pass read only associated floating IPs. It now reads every record (R24).
- T016's uplink, T033's check placement, and two wordings were tightened.

**Gate decision (2026-11-18, fifth analysis)**: no constitution conflict.
- R25 had taken `ReleaseInstancePorts` for server deletion. That function runs only when a boot fails; servers are
  deleted by Nova's own transaction. The fix now covers all three paths with one helper (R25; tasks T041, T046, T047).
- The deleting transactions now lock the port, or the instance and then its ports, before they read the floating IPs.
  That also removes server deletion's existing lock inversion with membership changes and volume attach.

**Gate decision (2026-11-18, sixth analysis)**: no constitution conflict.
- `forceDelete` deleted only the instance row, so SC-014 did not hold for it. It now shares `DeleteServer`'s deletion,
  as upstream's `force_delete` does (FR-008e, R25, T041, T047).
- The instance locks' missing-row cases are stated, the file of `ConvergeFloatingIPs` is named, and R25 no longer claims
  a deadlock with volume attach that cannot happen; it is a race.

**Post-design re-check (after Phase 1)**: unchanged.
- The design adds four narrow seams:
  - the dataplane interface and the uplink function on the Neutron service (R9);
  - the record read on the reconciler (R24);
  - the command runner on the `RouterManager` (R6).

  It adds one bounded in-process lock for the node (R8). It reuses `LockSubnet`'s pattern for row locks, `SQLFilter` for
  the listing, `NewResourceInUseError` for the `409`, and `pruneDeletedNetworks`' list-first, skip-when-incomplete rule
  for removal.
- The contracts (`contracts/*.md`) pin existing endpoints. They add one field to port update, two filters to the group
  listing and two answers to group deletion, all upstream's, and they state the bounds and the dataplane's order.
- No new divergence beyond entry 5.

### Round 2 gate (2026-11-20)

| Principle | Verdict | Why |
|---|---|---|
| I. API compatibility | PASS (register entry 5, names only) | Every new answer is upstream's, read in source (R26–R39): the listing filters, `default`'s three `409`s, `allocation_pools` and its four errors, `external_fixed_ips` and its errors, FR-039's two `409`s, the floating-IP `404`/`409`/`400`s, `binding:host_id`, `SubnetInUse`. No new divergence: duplicate group names follow upstream (FR-026, R53), after entry 6 was withdrawn. `revision_number` is kept and honoured, as upstream (R46); stateless groups are supported (R47). Administrators' Networking API listings, shows and writes follow upstream (FR-057, FR-058, R50). |
| II. Synchronous operations | **PASS (v1.6.0 refined 2026-11-20, R42)** — failed against the wording before | v1.6.0 says a request "applies it on the node that serves it". FR-034 applies on the network node or the port's node, through a synchronous call. Its exception "a change undone when its external work fails" stores first; FR-040's deletions remove host state first and delete the record after, a shape no exception names. The plan cannot justify either in its own text — v1.2.0's rule, which round 1's first gate broke. R42 amends: the node that holds the state applies, asked synchronously; a new exception, "a removal applied before its record goes"; the loop's case narrowed; the migration plan's departures gone (FR-045). The node call is not a queue: synchronous, bounded, no retry, no record. |
| III. Fail-fast | PASS | The node call is one one-second step; the target's lock wait and apply share 900 ms of it; nothing retries. The floating-IP allocation lock is a transaction's advisory lock, under v1.3.0's bounded lock waits. |
| IV. Multi-mode | PASS | Stub mode stores, allocates, refuses and reports exactly as real mode; units render but apply nothing. Two-node behaviour is tested in one process through the fake node caller. |
| V. Project isolation | PASS, strengthened | Tenant traffic is filtered between VMs for the first time; overlapping CIDRs are isolated by namespaces and conntrack zones; metadata identity no longer depends on unique CIDRs. The node call refuses user tokens, and `/v2.0` refuses node tokens. The gateway port has no project and is visible to administrators and the external network's owner, as upstream (FR-061). |
| VI. Single binary | PASS | Metadata inside namespaces is served by the binary itself (no haproxy); remote groups expand without `ipset`; `modprobe` is BusyBox's, already in the image. |
| Technology constraints | PASS | No new module. Migrations 084 and 085 follow `migrations/README.md`: 084 a pair for the new table, plain columns, no rebuild; 085's SQLite file rebuilds `security_groups`, which others reference, under the foreign-keys-off marker (R53). |
| Forbidden patterns | PASS | No queue, worker or state machine. The node call is a synchronous request; reconciliation stays the stated loop. |
| Test-first | PASS by plan | R43: every requirement red first, fakes for every unit and the node caller, migration parity, chart tests, one host run (two-node run dropped, T167). |
| Backwards compatibility | PASS | No deployment exists (FR-044): the layout change is a clean break, documented; no shim, no conversion. API changes follow Article I, listed in `docs/UPGRADING.md`. |
| Security | PASS | Closes: no filtering between VMs or of egress; every node announcing every floating address and gateway; overlapping CIDRs colliding in one routing table and one conntrack table; metadata identity ambiguous across overlapping CIDRs; a silently unfiltered node without `br_netfilter`; `default` silently reset by a tenant. |
| Quality gates | PASS by plan | Both backends, migration up/down (now applicable), chart tests, both modes, one host run (two-node run dropped, T167). |

**Gate decision (2026-11-20)**: the design did not fit v1.6.0's wording, and per v1.2.0 that is amended, not justified
here. **The user chose to amend as R42 on 2026-11-20.** v1.6.0 is refined in place (not yet on `main`; numbered v1.5.0
until the rebase, T057): the request rule names the node that holds the state, asked synchronously; a new exception, "a
removal applied before its record goes"; the loop's first case narrowed; register entry 6 (withdrawn later, R53); the
migration plan names no departure. `CLAUDE.md`'s Article II line follows. **Task generation may proceed.**

**Gate decision (2026-11-20, round-2 analysis)**: two critical findings. **D1** — refusing `?revision_number` was an
unregistered divergence; fixed the upstream way: the revision is kept and honoured (FR-050, R46). **D2** — an
administrator filtering by another project got an empty list, silent and unregistered. Decided with the user: an
administrator's explicit filter lists that project (option A), and, since the unfiltered administrator listing cannot be
registered (limit 4), every Networking API listing and show becomes upstream's for administrators (FR-057, US12, R50),
and so do writes by id (FR-058); register entry 5 restated on its own ground and kept for writes. Fixed besides:
stateless groups as upstream (FR-051, R47); the eBPF step removed with its TAP (D3); teardown for the new layout (R48);
the node call's shared 900 ms deadline (A1); network create's undo compare-and-set on `updated_at` with a dependants
check (D4); one allocation lock per subnet for every owner (E2); device names for trusted ports (E5); binding moves and
boot order (E4); requirements FR-052–FR-056 for answers that had none (C2); FR-023 restated in §5 (C3); stale round-1
text (B1, A4); task order, names and the missing tasks in `tasks.md` (F1, F2, E3, E6, A2, C1, C4).

**Gate decision (2026-11-20, second round-2 analysis)**: one critical finding, D1 — register entry 5's id half no longer
met limit 1 once administrators see every project's groups (FR-057). Decided with the user: ids follow upstream, entry 5
keeps the names half (R52). The other findings (revision undo and `If-Match`, entry 6's ground, the exception's
placement under Article II, Article V's text, deadlines, the stateless mix at boot, the chain-removal set, stale text,
task order so the build never breaks, the FDB task, the Horizon step) are fixed in place.

**Gate decision (2026-11-20, third round-2 analysis)**: two critical findings. The stateful/stateless check had been
placed on interface attach, where upstream has none; it is per port now (FR-051). Register entry 6 had no limit-1
ground; decided with the user: names follow upstream and entry 6 is withdrawn (FR-026, R53), with migration 085 and the
migrator change `migrations/README.md` foresees. The other findings are fixed in place.

**Post-design re-check (round 2)**: the contracts (`network-api-round2.md`, `node-apply.md`,
`router-namespace-dataplane.md`, `port-firewall.md`) add no divergence beyond entry 5 (entry 6 was added and withdrawn
on 2026-11-20). `floating-ip-dataplane.md` is marked superseded.

**Re-check at the rebase (2026-11-27, T057)**: the table re-run against the merged Articles III and IV, `main`'s v1.5.0
beside this amendment, now v1.6.0. Article IV: PASS — real mode never falls back to stub at runtime; a missing uplink is
a failed change (T015, T043), and real-mode networking without bridge netfilter fails readiness, naming
`bridge-netfilter`; only an unset mode defaults to stub, as the article permits. Article III: one conflict, older than
the merge. The round-2 row's "nothing retries" did not hold: the three statements that give a network its
connection-tracking zone re-ran themselves up to five times when another request took the zone first, against Article
III's "MUST NOT retry a failed statement … prevented by taking the write lock". Fixed, not justified (T312): they take a
transaction-scoped zone lock and run once. `main`'s background re-establishment exception concerns libvirt's connection,
a loop this feature does not touch.

## Project Structure

### Documentation (this feature)

```text
docs/specs/033-sg-port-membership/
├── spec.md                       # clarification (9 answers) and five analysis sessions (the first to fourth and the sixth: 4, 5, 4, 2 and 1 answers); the fifth reworded FR-008d only
├── plan.md                       # this file
├── research.md                   # Phase 0: R1–R25 (R19 = constitution v1.6.0)
├── data-model.md                 # Phase 1: tables touched, a port's groups, a floating IP's association, locks, per-node state
├── quickstart.md                 # Phase 1: gates, stub-mode walk, host validation (a)–(h)
├── contracts/
│   ├── compute-security-groups.md   # add/remove answers and effects, attach, boot
│   ├── server-report.md             # security_groups in show/detail
│   ├── network-api.md               # port security_groups; group listing filters; floating IPs, rules, group delete
│   └── floating-ip-dataplane.md     # filter and NAT content, order, atomic changes, reconciliation, bounds
├── checklists/requirements.md
├── tasks.md                      # Phase 2
└── follow-ups.md                 # drafted in Polish; all four moved into scope on 2026-11-19
```

Round 2 adds `contracts/network-api-round2.md`, `contracts/node-apply.md`, `contracts/router-namespace-dataplane.md` and
`contracts/port-firewall.md`; research R26–R53; a round-2 part in `data-model.md` and `quickstart.md`.

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
.specify/memory/constitution.md, CLAUDE.md          # v1.6.0 and its two runtime-guidance lines (done)
```

**Structure Decision**: the existing single-project layout. Round 1: no new package, no new source file outside tests.
Round 2: still no new package; new files inside `internal/neutron` (`node_call.go`, `units.go`, `allocation.go`,
`list_scope.go`) and `pkg/networking` (`router_namespace.go`, `dhcp_namespace.go`, `port_firewall.go`,
`nodesettings.go`), and migration 084. Each unit's renderer is a pure function beside its real-mode apply.

## Complexity Tracking

All five rows are resolved by the constitution amendment (v1.6.0, research R19 and R42), not by this table. They are
listed because the departures are real, and the v1.2.0 rationale requires that they be visible.

| Violation | Why needed | Simpler alternative rejected because |
|---|---|---|
| Two transactions around the node-local work, with an undo and no transitional row, for membership, rules and floating IPs (Article II) | The firewall and the NAT cannot be inside a database transaction, and on SQLite a held transaction blocks the whole file. These records are complete without the work, because the loop derives it from them alone, so there is no state between before and after for a transitional row to express. | A transitional row (a `pending` flag, a changes table, a new floating-IP status) is a migration no reader consults, or a state readers would have to learn to skip. Reusing `ports.status` changes what clients see for a reason upstream never shows. Applying before committing needs a retry when a concurrent change makes the base stale, and it breaks the list-first removal (R24). |
| Convergence through the reconciliation loop for other nodes, a stopped process, and a node whose own undo failed (Article II) | Round 1: no request could reach another node, or repair a node after it had answered, without inter-node messaging. Round 2's node call (R27) reaches the one node that holds a change's state; the loop still covers the rest. | Pushing a change to every node from the request needs a queue, which is forbidden. Failing the request when any other node cannot be reached would make every change depend on every node; round 2 depends on one node only, the holder of the state. |
| Round 2: a request applies on another node through a synchronous call (Article II, R42) | One network node owns north-south (FR-033), and a port's chains live where its TAP is (FR-041); the serving node cannot program either. | Store and let the other node's loop converge (clarification Q2's option C: a departure, and changes not in force at the answer); forward whole requests (every API path would need a proxy decision). |
| Round 2: a deletion removes host state before deleting its record (Article II, R42) | A deleted record cannot be undone compare-and-set the way a change can; host-first keeps the record as the undo. | Restore rows (a graph of dependants per resource); let the loop finish removals after a success answer (a departure the user declined). |
| The loop removes node state no record holds (Article II) | A removal served by one node must reach the others, and a stopped process or a failed undo can leave state that no record holds; an address allocated again would otherwise be forwarded to the old fixed IP. | Leaving removals to the requests: the nodes that did not serve one never learn of it. Removing by shape: an operator's own rules would be taken. So the NAT is marked (R23). |
