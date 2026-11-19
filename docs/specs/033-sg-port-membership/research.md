# Research: Server security-group membership reaches the ports

**Feature**: SPEC-033 ([spec.md](spec.md)), cloud-storage#579. **Base**: `main` @ `d81f081`; line numbers refer to it.

The spec's Clarifications (nine answers, then five analysis sessions) settle scope and answers. This document settles how. Every decision was read off the code or upstream's source named with it.
- The second analysis (2026-11-18) revised R5, R6, R8, R9 and R16–R19, and added R20–R24.
- The third and fourth analyses revised R9, R12, R16–R18 and R20–R24.
- The fourth added R25, which the fifth corrected: server deletion is Nova's own path, and the deleting transactions lock first. The sixth added `forceDelete` to it.

## R1 — The ports are the only list; `server_security_groups` is retired

**Decision**: After this change nothing reads or writes `server_security_groups`. Server create stops inserting it (`internal/nova/handlers.go:1039-1046`). Add and remove stop touching it (`advanced_actions.go:791-820`, `:879-925`), and the report reads the ports (R10). The table stays in the schema, unused.

**Rationale**: FR-014 makes a server's groups the groups its ports carry, which is upstream's definition. A second list is how the defect arose; one kept written but unread invites the next reader to trust it. Today its only readers are the two report helpers (`advanced_actions.go:668`, `:697`) and the add/remove checks; its only writers are create, add, and remove (which deletes rows, `:917`). The boot already receives the requested groups as the slice the request resolved (`handlers.go:1146`), so create needs no stored copy. Dropping the table would be a migration. No requirement needs one, so the spec expects none, and a later migration can drop it.

**Alternatives**:
- Keep writing it as the boot's source: FR-011 is met from the boot's own first port (R3).
- Keep it as the source of truth and push it to every port, the ticket's first design: rejected in clarification, because one server-level list cannot describe ports that legitimately differ (User Story 3).
- Drop it now: a migration nothing requires.

## R2 — One Neutron step writes a port's groups; Neutron rebuilds the filters

**Decision**:
- Package-level helpers in `internal/neutron` take the caller's transaction, as `LockSubnet` and `AllocateFreeIP` do: `PortSecurityGroupIDs`, `AddPortSecurityGroup`, `RemovePortSecurityGroup` and `SetPortSecurityGroups` (replace). Each write bumps the port's `updated_at`, as upstream's `update_port` does.
- `ResolveSecurityGroup(ctx, q, ref, projectID)` moves to `internal/neutron` and serves create, add and remove. It resolves a name or an id in the project, the id winning; today the same query is inline in create (`handlers.go:881-887`).
- `NeutronService` (`handlers.go:61`) gains `RefreshPortFilters(ctx, portIDs []string) error`, which rebuilds the floating-IP filters of those ports on this node (R6–R9). In stub mode it returns at once.

**Rationale**: The database work has to happen inside Nova's transaction (R3's locks, R4's checks). Package-level code on a transaction is the pattern Nova already uses for Neutron's subnet lock and address allocation (`interface_attach.go:206-226`). The rebuild needs Neutron's dataplane and its node lock (R8, R9), so it is a service method reached through the interface Nova already holds. The fake (`boot_failure_test.go:46`) gains it to inject failures (R16). Add and remove write single rows, so undoing them (R5) is the inverse row operation; the port API replaces the whole list (R12).

**Alternatives**:
- One Neutron method doing the whole compute change: Neutron would then own Nova's `404`/`400` rules and the instance lock.
- A Nova copy of the SQL: two writers of one table drift, which is how this defect began.

## R3 — Lock order, and the boot's later ports

**Decision**: Every membership write for a server first locks the instance row (`SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`, as `volume_attachment.go:166` does), then the server's ports in id order (`SELECT id FROM ports WHERE device_id = $1 ORDER BY id FOR UPDATE`). `AllocatePortForInstance` locks the instance row in its transaction before the subnet (`LockSubnet`, `ports.go:678`).

The boot's first port carries the groups the create resolved. Each later port copies the current groups of the boot's first port, read inside the allocation transaction under the instance lock. For that, `AllocatePortForInstance` gains a `copyGroupsFrom` port id, empty for the first port.

**Rationale**: FR-011 says a change accepted while the boot runs must reach the ports created after it. Add and remove answer `404` until the first port exists (FR-003), so only a change between the first port and a later one matters. Holding the instance row in both the change and the allocation serialises them, and the later port reads the first port's list after any committed change. Copying from the boot's own first port, by id, cannot pick up a port attached during the boot, which FR-013 gives other groups.

The single order (instance, then ports, then subnet) has no cycle: changes never take a subnet lock, and allocations never lock existing ports. On SQLite `FOR UPDATE` is dropped and immediate transactions serialise writers (`internal/database/sqlite_translate.go:451`), which gives the same order trivially.

**Alternatives**:
- Read the current list in the boot goroutine before allocating: a change between the read and the insert would be lost.
- `server_security_groups` as the boot's source: see R1.
- An advisory lock keyed on the instance: a no-op on SQLite. A row lock is the existing precedent.

## R4 — Add and remove

**Decision**: One transaction, after the locks of R3:
1. Resolve the group (R2) in the server's project. The dispatcher has already rebound `project_id` to the owner (`handlers.go:2416-2443`), so an administrator resolves in the tenant's project (register entry 5).
2. A server with no port gets `404` (FR-003).
3. Add: if any port already carries the group, answer `400` and change nothing; otherwise insert it on every port.
4. Remove: delete it from every port that carries it; if none does, answer `404`. The last-group guard (`advanced_actions.go:897-913`) and the two stale comments (`:824`, `:928`) go.
5. Commit, then `RefreshPortFilters(ports)`, with R5 on failure. Answer `202`.

**Rationale**: Upstream's `add_to_instance` and `remove_from_instance` (read in source) apply to every port and answer `404` for a server with none. Refusing the whole add when any port has the group is one of upstream's own outcomes: upstream stops at the first port that already has it, in an unspecified port order. Server create resolves by name or id with the id winning; sharing that one function keeps the two paths from drifting again.

**Alternatives**: a partial add in whatever port order the database returns. That is nondeterministic, and no client depends on it.

## R5 — A change the firewall cannot take is undone

**Decision**: This is the shape of every change in this feature that the node must apply: membership here, floating IPs in R20, rules in R21. For membership: transaction 1 stores the change and commits. For each port, it records the group list it leaves. The serving node then rebuilds the affected filters (R6–R9). If any rebuild fails:
- Transaction 2 locks the same rows in the same order (R3) and undoes exactly this request's change. Every path uses compare-and-set: a port is put back only if its list still equals the list transaction 1 left. A port another request has changed since is left as it is.
  - add: the group's row is deleted again;
  - remove: the group's row is inserted again;
  - port update: the previous list is restored.
- The affected filters are rebuilt again from the database. This is best effort: a failure here is logged, and the node's reconciliation pass converges it (R7, the v1.5.0 loop exception).
- The request answers `500`, and the log names the port and the address.

**Rationale**: FR-008, clarification Q3 and the analysis session. A transaction may not stay open across external work (Article II; on SQLite it would hold the whole file), so the change commits first and is undone afterwards. The client gets the answer one transaction would have given: `500`, nothing changed. This is constitution v1.5.0's exception, "a change undone when its external work fails" (R19), and each of its limits holds:
- a membership row is complete without the rebuild, which is derived from it alone;
- one transaction makes the checks, takes the locks and stores the change;
- the undo is compare-and-set;
- every step is bounded at one second (R6, R8);
- a change whose process stopped mid-request stays stored and is applied by the reconciliation pass.

Reversing only this request's rows without the comparison is not enough. If A removes `web`, B then sets the port to `[db]`, and A's rebuild fails, A would re-insert `web` and leave `[db, web]`, undoing part of B. Because the rebuild is atomic (R6), a failed rebuild leaves the old filter whole, and the undo only has to put the database back.

**Alternatives**:
- Rebuild before committing, from rules computed in memory: a concurrent change makes the computed base stale, and recovering would need a retry, which Article III forbids.
- Hold the transaction across the rebuild: forbidden.
- A transitional row under v1.3.0's reservation exception: it would need a schema change that no reader consults. The analysis chose the amendment instead (R19).

## R6 — A filter is replaced in one `iptables-restore` commit

**Decision**: `EnsureIngressFilter` (`pkg/networking/northsouth.go:157`) renders the chain's complete content and applies it with one `iptables-restore --noflush --wait 1` run:

```text
*filter
:LS-NSI-<ip> - [0:0]
-F LS-NSI-<ip>
-A LS-NSI-<ip> -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A LS-NSI-<ip> <match> -j ACCEPT          (one per allowance, R7's form)
-A LS-NSI-<ip> -j DROP
COMMIT
```

It then ensures the `FORWARD` jump (`ensureRule`). Every command the rebuild runs — `iptables -S`, `iptables-restore`, and the jump's check and append — goes through one command runner on the `RouterManager`, and each runs under a one-second context (`exec.CommandContext`). A command that overruns is killed and counts as a failed rebuild (FR-007b). The runner is a function field defaulting to the real execution, so the real-mode logic is unit-testable without root (R16). Floating-IP association (`floatingip.go:281-284`, `:522-528`) and the reconciler (`reconciler.go:133-136`) build the filter **before** the DNAT, no longer after it. When the filter cannot be built, the DNAT is not added in that step; an existing DNAT stays, because the old filter is still whole. R23 sets the whole order, for the NAT and the floating address too, and R20 the answer: an association the node cannot apply answers `500` and is undone. The same runner and bound cover the NAT and address commands (`iptables -t nat -S`, the nat table's restore commit, `ip -o addr show`, `ip addr add` and `del`) and the filter's removal, which becomes one restore commit as well (R23).

**Rationale**: The declaration creates the chain or keeps the existing one, and the explicit `-F` empties it. Everything up to `COMMIT` is applied in one step: one table replacement with the legacy backend, one netlink transaction with nft. So the chain holds either the old rules or the new ones, and a failure applies nothing (FR-007a). The image installs Alpine's `iptables` package (`build/package/Dockerfile:41-46`), which ships `iptables-restore`. `--wait 1` keeps a wait for the legacy xtables lock inside the command's own one-second bound, so the command fails cleanly rather than being killed mid-wait. Both backends accept the declaration under `--noflush` and the explicit `-F`. The host run (R18 d) exercises whichever backend the host uses. Today the DNAT goes in first at all three sites, so a new floating IP, or one re-asserted after a host restart, reaches the guest unfiltered until the filter follows. With the filter first, there is no such moment.

**Alternatives**:
- Build a new chain and swap the jump (`-I`, `-D`, `-X`, `-E`): four more calls, and the swap still takes two steps.
- Insert the `DROP` first and the allowances above it: the replies rule would sit after a `DROP` for a moment, dropping established connections.
- Keep the in-place rebuild: rejected in clarification Q2.

## R7 — Reconciliation leaves an unchanged filter alone

**Decision**: `EnsureIngressFilter` first reads the chain (`iptables -S LS-NSI-<ip>`) and compares it, line by line, with what it would apply. If equal, it only ensures the jump; if different or unreadable, it rebuilds atomically (R6). Every caller gets this; reconciliation is the one it matters for. The renderer emits iptables' canonical form, so an untouched chain compares equal:
- `-s` with a prefix length, before `-p`;
- `-p tcp -m tcp --dport N[:M]` (and the same for udp);
- `-p icmp -m icmp --icmp-type N`;
- `--ctstate RELATED,ESTABLISHED`.

**Rationale**: FR-010. The comparison is against the kernel, not a cache, so a chain edited or flushed by hand still differs and is rebuilt within a pass, as today. A comparison that fails only on formatting costs one atomic rebuild and never opens the filter (R6), so the canonical renderer is an optimisation whose failure mode is safe. On the host the skip is visible: the rules' packet counters (`iptables -L <chain> -v -n -x`) keep growing across passes. In unit tests the command runner (R6) shows it: an unchanged chain leads to no `iptables-restore`, a changed one to exactly one.

**Alternatives**:
- Cache the last applied set: a hand edit would never be corrected.
- Keep a hash in a rule comment: an edit that keeps the comment line survives.

## R8 — A pass never writes a list it read before a change

**Decision** (revised in the second analysis): The Neutron service keeps one lock for the node's floating-IP state: a one-slot channel. Every change to that state acquires it with a one-second deadline, reads the records it needs, compares or applies, and releases it. That covers a filter rebuilt for a membership or rule change, a floating IP's convergence (R23) and a reconciliation step (R24), and one acquisition covers one address.
- A request that cannot acquire the lock within the second has a failed change (FR-007b), with FR-008's answer.
- A reconciliation step that cannot acquire it skips that address until the next pass.

**Rationale**: FR-010 ("never reverted") and FR-007b. Without the lock, a pass that read the old rules before a change committed could write them after the change's rebuild, and the old set would hold for up to 30 s. With the read inside the lock, a pass either finishes before the change's rebuild starts, and is overwritten by it, or reads after the commit.

The first design kept one lock per fixed IP. A floating IP's convergence touches its NAT, keyed by the floating address, and up to two fixed IPs' filters when it moves. Per-address locks would need an acquisition order across two kinds of key, and they would buy little parallelism, because iptables already serialises writes on a node: the xtables lock with the legacy backend, one netlink transaction at a time with nft.

The deadline keeps the wait within Article III's one second, and each holder's own commands are bounded too (R6). A plain `sync.Mutex` cannot wait with a deadline, so the lock is a channel. All changes on a node run in one process (Article VI); other nodes have their own tables and their own passes. A database advisory lock would wrongly serialise across nodes and does nothing on SQLite.

**Alternatives**:
- One lock per fixed IP, the first design: see above.
- Serialise whole passes against requests: one slow pass would stall every change.
- Accept a rare revert of up to 30 s: FR-010 forbids it.
- An unbounded mutex: a slow holder would delay a request's answer without limit, which Article III forbids.

## R9 — The per-port rebuild finds the port by its floating IP; narrow seams for the node

**Decision** (seams revised in the second and third analyses):
- For each port, `RefreshPortFilters` reads `SELECT fixed_ip_address FROM floating_ips WHERE port_id = $1 AND fixed_ip_address IS NOT NULL` and rebuilds each address's filter (R6–R8). A port without a floating IP has nothing to rebuild.
- The uplink comes from a function on the service: `networking.DefaultUplinkInterface` in real mode, an empty name in stub mode.
  - A real-mode request that cannot find the uplink has a failed change, with FR-008's answer. It never skips the work.
  - A reconciliation pass without an uplink skips its floating-IP step and logs an error (R24).
- The service reaches the node through one narrow interface over the router manager, holding exactly the floating-IP dataplane. Its methods:
  - ensure and remove a filter;
  - ensure and withdraw a floating IP's NAT, each reporting the fixed IPs its replaced rules pointed at (R23);
  - list the node's state for reconciliation (R24).
- `*networking.RouterManager` implements it. Tests replace it with an in-memory fake that holds the node's state, records the calls, and can block or fail any method.
- Below it, R6's command runner makes the router manager's own real-mode commands testable.

**Rationale**:
- `refreshIngressFiltersForSG` finds ports through `port_security_groups` (`floatingip.go:698`), so it never finds a port its change emptied (FR-007). `portIngressRules` already joins the port's groups (`floatingip.go:643`), so an empty list yields the reply-only filter.
- Today the rule refresh and the floating-IP create log a missing uplink and go on (`floatingip.go:725`, `:288`). A real-mode request then answers success with nothing applied, which is the defect FR-008 removes. The lookup asks the kernel through netlink, so a function on the service is the narrowest way for a test to make it fail.
- `RouterManager` is a concrete type (`pkg/networking/router.go:10`), so the service needs a seam to test FR-008's failures. The first design used two function fields, for the filter and the DNAT, and rejected an interface as a seam on every method when the service needed two. Convergence and removal now need the methods above, and checking removal (R24) needs a fake that holds state rather than one that only records.

**Alternatives**:
- Function fields, the first design: one per method, with no state to check removal against.
- An interface over the whole router manager: a seam on methods this feature never calls.
- Call `DefaultUplinkInterface` directly: no test can reach the missing-uplink path, and that path was the silent one.

## R10 — A server reports its ports' groups

**Decision**: Show and detail use one query: the server's ports (`device_id = ANY($1::uuid[])`; `device_id` is a UUID column, `001_initial_schema.up.sql`) joined to `port_security_groups` and `security_groups`, ordered by port (`created_at`, `id`) and then group name.
- Each port and group gives one `{"name": …}` entry. A group with an empty name is reported by its id, upstream's fallback.
- `security_groups` is set only when there is at least one entry.
- `formatSecurityGroups` stops inventing `default` (`advanced_actions.go:729-739`).

**Rationale**: FR-014 and FR-015, as upstream's `_add_security_grps` (`views/servers.py`) and `get_instances_security_groups_bindings` (read in source). Every group on a port belongs to the port's project (R12, register entry 5), so no visibility filter is needed. Servers from releases up to v0.6.1, whose ports carry no group, then report none. That is true, and needs no backfill. The brief listing has no `security_groups` and is unchanged.

**Alternatives**: deduplicate the entries. Upstream does not, and Terraform's set and Horizon deduplicate on their side.

## R11 — An interface attached later

**Decision**:
- `os-interface` with `net_id`: ensure the project's `default` group (`neutron.EnsureDefaultSecurityGroup`, in its own transaction, before the port's). Insert the `port_security_groups` row in the transaction that inserts the port (`interface_attach.go:206-232`).
- `os-interface` with `port_id`: no change.
- No filter to rebuild in either case: a new port has no floating IP yet.

**Rationale**: FR-013, upstream's `_attach_interface` (read in source). `EnsureDefaultSecurityGroup` runs its own transaction and cannot nest; `AllocatePortForInstance` calls it before its transaction for the same reason (`ports.go:1629-1646`).

## R12 — The port API's `security_groups`

**Decision**: `CreatePort` and `UpdatePort` read `security_groups` as raw JSON:
- absent: create gives `default`; update leaves the list as it is;
- `null` or `[]`: no group;
- a list: exactly that list.

Validation runs in this order:
1. each entry is a UUID string, else `400`;
2. no entry is repeated, else `400`;
3. for an update, the port exists in the caller's project, else `404`;
4. each names a group of the port's project, else `404` (`NewResourceNotFoundError("security group", id)`, as `ports.go:207` does).

This is upstream's order: the request body is validated before the port is looked up.

Update locks the port row (`FOR UPDATE`), which is check 3. It reads the groups in the same transaction, which is check 4, as constitution v1.5.0's second limit puts a change-then-undo operation's checks in its first transaction. Then it replaces the list (R2), bumps `updated_at`, commits, rebuilds (R9), undoes on failure (R5, compare-and-set), and answers with the port as today (`GetPort`). The group read takes no lock: a group's project never changes, and the foreign key re-checks its existence at the write. A body with only `security_groups` counts as an update (`ports.go:599-601`). Administrators get the port's project only (register entry 5).

**Rationale**: FR-017 to FR-019, matching upstream Neutron's attribute definition (`neutron/extensions/securitygroup.py`, read in source): `convert_none_to_empty_list`, `type:uuid_list` (repeated or non-UUID entries are `400`), and an `ATTR_NOT_SPECIFIED` default. Go's decoder yields `nil` for both an absent field and `null` in a `*[]string`, so the field has to be read raw.

## R13 — The group listing's `id` and `name`

**Decision**: `ListSecurityGroups` (`ports.go:961`) builds its `WHERE` with `common.SQLFilter`: `id::text = ANY($n)` over every `id` (`c.QueryArray("id")`), and `name = $n`. When nothing matches, the list is empty. `= ANY` is translated on SQLite (`internal/database/doc.go`).

**Rationale**: FR-019a. Horizon sends a repeated `?id=` (`list_by_instance`, read in source). Terraform's data source sends `?name=` and refuses more than one result (read in source). `id::text` keeps a malformed id from erroring the query, as the network listing's id filter does (`network.go:736`).

Upstream honours every other attribute filter as well (`description`, `project_id`/`tenant_id`, `tags`, `shared`, `stateful`, …). Lightstack ignores them, which is a pre-existing silent divergence. This feature neither fixes nor mandates it; the follow-up list carries it. Refusing them with `400`, as register entry 4 does for server-list filters, would break Horizon, which sends `tenant_id`. The follow-up has to honour the ones the data model can answer.

## R14 — eBPF

**Decision**: No code change. `docs/EBPF_STATUS.md` says that a membership change does not re-program a port's eBPF rule set, and its `network.go:40` reference becomes `:41`.

**Rationale**: FR-009 was withdrawn in clarification (Q5).

## R15 — Register entry 5

**Decision**: Register entry 5, with FR-021a's text, is part of constitution v1.5.0 (R19). It was ratified at the gate rather than left to a later task. `CLAUDE.md`'s Project Isolation gained one line: security groups resolve only in their own project, administrators included.

**Rationale**: Article I requires every permitted divergence to appear in the register, and v1.2.0 says "Future divergences require a register entry". The entry was approved in clarification Q4. Recording it in the same amendment as the Article II changes gives one version bump instead of two.

## R16 — Test strategy (Test-First Development)

**Decision**: Each requirement's test is written first and seen failing:
- **`internal/nova`**:
  - A server's deletion, and a `forceDelete`, with an associated floating IP: the floating IP is disassociated in the deleting transaction, `ConvergeFloatingIPs` is called before the answer, and the instance is locked before the ports. A `forceDelete` also deletes the boot ports and releases the volumes, as a delete does (R25).
  - Tests use the real Neutron service in stub mode on the shared test database (`neutron.NewService(testPool, "stub", nil)`, as `ip_allocation_concurrency_test.go:39` does), set as the Nova service's network service, with ports allocated through it.
  - A wrapper or the fake (`boot_failure_test.go:46`) is used only to make `RefreshPortFilters` fail, and to change a port in between to exercise compare-and-set.
  - `server_detail_secgroups_test.go` is reworked, since the report now reads ports; its orphan case becomes a deleted-group case.
  - Adding a group to a server shaped like v0.6.1's is a US1 case, red before the add is rewritten.
- **`internal/neutron`** (harness at `network_db_test.go:24`):
  - the helpers, `AllocatePortForInstance`'s copy-from, port create and update decoding and validation, the listing filters, and the refresh lookup;
  - failure and compare-and-set undo, through the seam (R9), a missing uplink included;
  - the node lock: a pass that read old rules cannot write after a change, and an acquire that waits past one second fails;
  - floating-IP create, update and delete, rule add and delete, and group delete: what reaches the fake before each answer, its order, and the undo when a method fails (R20–R22). Also a port deletion, and a failed boot's release, with an associated floating IP: disassociated in the same transaction, after the port's lock, and withdrawn before the answer (R25);
  - reconciliation in both directions: what a pass adds (a missing address included), what it removes, and what it leaves alone: unmarked rules without a record and the filters they reach, and anything after a failed or partial read (R24). Also the filter before the NAT.
- **`pkg/networking`**:
  - the restore payload and the canonical renderer and comparison, as pure functions;
  - `EnsureIngressFilter`'s real-mode path with a fake command runner (R6): an unchanged chain runs no `iptables-restore`, a changed one runs exactly one, a failing restore returns its error, every command gets a deadline of one second at most, and a runner that blocks makes the call fail within about a second;
  - the NAT (R23) the same way: the nat payload for an association, a move and a withdrawal, the order of filter, NAT and address, unmarked legacy rules replaced, removal of a chain or rule that is already gone counted as done, and the listing the reconciler reads;
  - stub mode runs no command.
- **Contract** (gophercloud, stub stack): servers with a network, using `createNetworkWithSubnet`, `createNamedServerOnNetwork` and `awaitAddresses` from `server_addresses_test.go`. Cases: membership after create, add, remove and last-group removal; a server with no port; a group named by id; port update and create; the listing filters; Horizon's *Edit Security Groups* sequence (list ports by device, list groups by `id`, update each port); and a group delete answering `409` while a port carries it and `404` for a missing id. Floating-IP and rule failures need a failing node, so they are unit tests only.
- Both database backends, through `LIGHTSTACK_TEST_DB_URL`.

**Rationale**: Clarification Q7. The report reads the same port rows the firewall reads, and only failure injection needs a stand-in.

## R17 — Documentation

**Decision** (revised in the second analysis):
- `docs/UPGRADING.md`: a section "Security-group membership reaches the ports" with a before/after table (FR-020's list). It also gives the command that lists NAT rules left unmarked by an earlier release, `iptables -t nat -S PREROUTING | grep -- '-j DNAT' | grep -v lightstack-fip`, and says that rules for floating IPs deleted before the upgrade are the operator's to remove.
- `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`: instance *Edit Security Groups* recorded as verified (FR-021).
- `docs/EBPF_STATUS.md`: R14.
- `CLAUDE.md`: done with the amendment (R15, R19). Project Isolation states register entry 5. The database paragraph states the change-then-undo shape for membership, rules and floating IPs.
- `docs/specs/033-sg-port-membership/follow-ups.md`: tickets drafted for the user to file.
  1. The listing filters beyond `id` and `name` (R13).
  2. The remaining request paths that log a failed node-local step and answer success, each with its location, and network creation's unconditional undo (R19). This is the list the constitution's migration plan points to. It includes the LS-SG chain removal this feature moves after the commit (R22). It also includes a gap found while redesigning deletion: deleting a floating IP deletes its port forwardings with it, through the foreign key, and leaves their NAT in place. For port and instance-port teardown it notes what this feature now does: the floating IPs of a deleted port are disassociated in the deleting transaction and withdrawn after it (R25). A failed withdrawal there is only logged, which is the part that remains a departure.
  3. Deleting `default` as a non-administrator: upstream refuses it with `409`. `test/security_groups_test.sh`'s Test 11 already expects that.
  4. Floating-IP behaviour beyond its dataplane: two creates racing for one address, a port whose network has no router (upstream `404`, Lightstack stores the association), and several floating IPs on one fixed IP.
- SPEC-009's documents stay as written (spec Assumptions).

## R18 — Host validation (FR-023; SC-002, SC-003, SC-010, SC-013, SC-014)

**Decision**: One real-mode host with iptables' `comment` match, a server with a floating IP, and listeners on two guest ports:
- **(a)** Add and remove a non-default group through the compute API: its port opens and closes.
- **(b)** The same through `openstack port set --security-group`.
- **(c)** Remove the last group: new connections drop, replies still arrive.
- **(d)** Make one connection attempt per second against a port no rule admits, for two minutes (at least four reconciliation passes) while groups change: no connection succeeds. A refused connection would mean the packet got past the filter; a timeout means it was dropped.
- **(e)** `iptables -L LS-NSI-<ip> -v -n -x` across passes: the rule counters keep growing, so an unchanged filter is left alone; they reset only when the set changes, because a rebuild replaces the rules.
- **(f)** Horizon's *Edit Security Groups*.
- **(g)** Rules and floating IPs. Add a rule admitting the second listener's port to a group the port carries: it opens at once; delete the rule: it closes. Move the floating IP to a second server: the first server stops answering through it at once, and `iptables -t nat -S PREROUTING` shows one marked rule for it, pointing at the new fixed IP. Disassociate it, then delete it: nothing answers, and its address, its NAT rules and the old filters are gone. Associate a floating IP with the second guest again and delete the guest: the floating IP shows no fixed IP and status `DOWN`, and its NAT is gone at once.
- **(h)** Removal by reconciliation. Add by hand a marked NAT rule for an address no record holds: within one pass it is gone. Replace an associated floating IP's marked rule by hand with an unmarked one of the legacy shape: within one pass the marked rule is back and the unmarked one is gone. Add an unmarked rule of the shape for an address no record holds, pointing at a fixed IP whose floating IP is then deleted: after a pass the unmarked rule and that fixed IP's filter are both still there. Remove an associated floating address from the uplink by hand: within one pass it is back.

The runbook is generated from this plan by the host-testing workflow.

## R19 — Constitution v1.5.0 (ratified 2026-11-17, refined 2026-11-18)

**Decision**: The first analysis found the plan justifying two departures from Article II rather than amending it, and the user chose to amend. The second analysis found the first draft of the amendment too narrow in one place and silent in two, and it was refined in place, since it had not reached `main`. Version 1.5.0 changes three things in `.specify/memory/constitution.md`:
- **A new Article II exception, "a change undone when its external work fails".** An operation whose external work only applies node-local state derived from database records may store its change in one transaction, do the work, and undo in a second. Five limits apply:
  1. every record stored, changed or removed is complete without the work, because the loop derives the node-local state from it alone; a resource whose external result cannot be derived again (a volume's backing) keeps the reservation exception, and a stored record counts against its limits from the first transaction;
  2. checks, locks and the change in one transaction, none open across the work;
  3. a compare-and-set undo (a created record removed, a removed one restored, a changed one put back), the state re-applied, and the single-transaction answer;
  4. every external step and every wait bounded at one second;
  5. a change whose process stopped stays stored and is applied by the reconciliation loop, removals included.
- **The reconciliation-loop exception's scope, stated.** The loop corrects the record, or makes node-local state match it: it re-asserts what the record holds and removes what it no longer holds, removing only state of its own making and nothing on an incomplete read. That is the network reconciler v1.1.0 was written for, which already removes deleted networks' bridges and DHCP. Its drift includes node-local state lost to a restart or edited by hand, the nodes that did not serve a change, a request whose process stopped after storing its change, and a node whose own undo failed after an error answer. A request that changes node-local state applies it before answering, or fails and undoes it.
- **Register entry 5** (R15).

**Rationale**:
- A membership row has no limit and no state between before and after, so v1.3.0's transitional row would be a schema change that no reader consults.
- The first limit's draft, "no resource comes into being", drew the line in the wrong place. A rule, a floating IP and a network are created and removed with node-local state the loop derives from the record alone, and network creation already undoes a created row. The line that matters is whether the loop can rebuild the external result from the record.
- Multi-node convergence, a stopped process and a failed undo cannot be finished by the request without a queue, and for a removal they need the loop to remove.
- The v1.2.0 lesson applies: a plan's tradeoff table is not an amendment.

The amendment's migration plan names, as departures it does not permit, the request paths outside this feature that log a failed node-local step and answer success. It also names network creation's undo, which is not compare-and-set. The follow-ups list each with its location (R17).

**Alternatives**:
- A transitional row (a `pending` flag, or a changes table): a migration nothing reads.
- Reusing `ports.status` as the transitional state: changes what clients see for a reason upstream never shows.
- Pushing a change to every node from the request: needs inter-node messaging, which is a queue.
- Keeping "no resource comes into being" and moving rules and floating-IP creates to the reservation exception: a transitional state for records that are complete on commit, which readers of floating IPs and rules would have to learn to skip.

## R20 — A floating-IP change is stored, applied, and undone on failure

**Decision** (create and delete revised in the third analysis, locks in the fourth): Every floating-IP change that touches the node takes R5's shape:
1. one transaction makes the checks and stores the change;
2. the floating IP is converged on the serving node under the node lock (R8, R23);
3. on a failure, a second transaction undoes the change compare-and-set, the floating IP is converged again from the restored records, and the answer is `500`.

- **Create on a port** (`CreateFloatingIP`). One transaction makes the checks and inserts the row, and nothing is programmed before it commits. Then the floating IP is converged. The checks:
  - The floating network is locked `FOR UPDATE` and must be external.
  - The port is read: it must be the caller's, and gives the fixed IP.
  - The router is looked up as today, then locked by id `FOR UPDATE` in a query of its own.
  - The address is chosen before the transaction, as today, and the insert's unique constraint re-checks it. Two creates racing for one address stay a follow-up.
  - On a failure, a second transaction deletes the row if its `port_id`, `fixed_ip_address` and `description` still hold what the insert wrote, and the floating IP is converged again, which removes whatever was applied. A row another request has changed meanwhile stays.
- **Update** (`UpdateFloatingIP`). One transaction locks the new port `FOR UPDATE` and checks it as today, then locks its router by id, as the create does. Only then does it lock the floating IP's row `FOR UPDATE`, read the old association, write the new one and commit. A disassociation locks only the row. Then the floating IP is converged, which also removes the old fixed IP's filter unless something on the node still reaches it (R23).
  - The port comes before the row because a port's deletion takes the same two locks in that order: it holds the port and then clears `port_id` on the floating IPs that name it (`ON DELETE SET NULL`). Locking the row first would let the two deadlock.
  - On a failure, a second transaction locks the row and restores the old association if the row still holds the new one; then it converges again.
  - A description-only update touches nothing on the node.
- **Delete** (`DeleteFloatingIP`). One transaction locks the row (`FOR UPDATE`), reads it and its port forwardings in full, and deletes it. The port forwardings go with it (`ON DELETE CASCADE`, migration 050). Then the floating IP is converged, which withdraws its address and NAT and removes its fixed IP's filter unless something on the node still reaches it.
  - On a failure, a second transaction restores the row and its port forwardings as they were. What other requests deleted meanwhile is applied the way the foreign keys would have applied it (migrations 003 and 050):
    - with its project or its floating network gone, nothing comes back;
    - with its port gone, it comes back unassociated;
    - with its router gone, it comes back without a router;
    - a port forwarding whose internal port is gone stays deleted;
    - an address allocated again meanwhile keeps the floating IP deleted, since that later change stays.

    Then the floating IP is converged again, and the answer is `500`.
  - A floating IP without an association has nothing on the node, and is deleted in one transaction, as today.
- A foreign-key violation in the first transaction means a row the request named was deleted meanwhile. It answers `404`, never `500`. In an undo transaction the request already answers `500`, and rows deleted meanwhile are handled by the foreign keys' delete actions, as the delete's restore describes.

**Rationale**: FR-008, FR-008a, the second and third analyses, and the limits of constitution v1.5.0's change-then-undo exception.
- Limit 2 puts the operation's checks in the first transaction. Today's create checks the network and the port before it writes anything, outside any transaction, and nothing inside the insert re-checks the network's `external` flag.
- Only rows whose checked state another request can change get a lock: the network (`external`) and the router (its gateway). A port's or group's project never changes, and the foreign keys re-check that they still exist at the write.
- `FOR UPDATE` is the lock: it is the only one in the shared SQL subset. `internal/database/doc.go:45` lists `FOR SHARE` as supported on neither backend, and `TestSQLConstructs` fails the build on it. SQLite drops `FOR UPDATE`, since its immediate transactions serialise writers, and refuses `FOR UPDATE OF`, so the router is locked in a query of its own.
- Locking the network serialises floating-IP creates on that network. Each transaction lasts a few statements, and the node work runs after the commit.
- Storing first is what lets reconciliation remove state no record holds (R24). Every path commits the record before it creates node state, so whatever a pass finds on the node belongs to a record it can read. Network creation and `pruneDeletedNetworks` already follow this rule (`network.go:628-630`). Today's create does the reverse, programming and then inserting, so a failed insert leaves NAT that no record holds.
- A delete stored in one transaction is a complete final state, as a committed single-transaction delete would be (limit 5). A process that stops after it has still deleted the floating IP, and reconciliation removes its NAT.
- The restore is R21's undo of a deleted rule, applied to a row with dependants: the port forwardings it cascaded, and foreign keys whose targets other requests can delete in between. Applying their delete actions gives exactly the state those later changes would have left had this request never run.

**Alternatives**:
- Program first and insert after, as today's create does: a pass that lists the node before reading the records would take a just-programmed address away, and a failed insert leaves NAT behind.
- Disassociate first, then delete (the design until the third analysis). The deletion is stored after the external work, in a second transaction, and a process that stops in between leaves a floating IP neither deleted nor as it was. Neither of Article II's exceptions has that shape.
- Restore a deleted floating IP only when every row it names still exists, and leave it deleted otherwise. The client would get an error for a floating IP that is gone, where one transaction would have left it in place, disassociated by the later port deletion.
- Record a failed association as `status: ERROR`, as upstream's agent does: Lightstack has no agent to retry it. The floating IP would stay broken, or the loop would retry request work, which Article III forbids.

## R21 — A rule change rebuilds every filter that uses its group

**Decision** (checks revised in the third analysis): `CreateSecurityGroupRule` and `DeleteSecurityGroupRule` keep their LS-SG step where it is.
1. The rule row is inserted or deleted in a transaction that makes the request's checks.
   - An add reads the group, and the remote group when one is named, both the caller's, and inserts. The check before the LS-SG step stays as well, so a refused rule writes nothing there. Neither read takes a lock: a group's project never changes, and the foreign keys re-check that both still exist at the insert.
   - A delete reads the whole row `FOR UPDATE`, scoped to the caller's project, for the undo, and deletes it.
2. Every fixed IP whose floating IP's port carries the group gets its filter rebuilt, one address at a time under the node lock (R8). The addresses come from `floating_ips` joined to `port_security_groups`.
3. On a failure, a second transaction undoes the change. It deletes the added rule if it is still there, or re-inserts the deleted rule with every column as it was, if its group still exists and its id is free. The same addresses are rebuilt again, and the answer is `500`.
4. A foreign-key violation in the first transaction, the group deleted meanwhile, answers `404`, never `500`.

`refreshIngressFiltersForSG` is replaced by this.

**Rationale**: FR-008b, and FR-008 for the undo.
- Limit 2 puts the checks in the first transaction. Today's ownership checks read the pool before the insert. They also stay before the LS-SG step, which writes into the group's chain and must not run for another project's group (`ports.go:1186-1188`).
- Rules have no update route (`network.go:136-139`), so "still holds what this request left" means present for an add and absent for a delete.
- The LS-SG chain write before the insert stays as it is. Nothing jumps to these chains (spec Context), and their failure policy is a departure the constitution's migration plan records. An undone add can leave its rule in the LS-SG chain, where it has no effect on traffic. The follow-up notes it.

**Alternatives**: rebuild only the ports of the request's own project. A group belongs to one project and so do its ports (register entry 5), so the set is the same.

## R22 — A group that a port carries cannot be deleted

**Decision**: `DeleteSecurityGroup` runs one transaction.
1. It locks the group's row with `SELECT … FROM security_groups WHERE id = $1 AND project_id = $2 FOR UPDATE`. No row → `404` (`SecurityGroupNotFound`).
2. Any `port_security_groups` row for the group → `409`, through `common.NewResourceInUseError("security group", …)`, as network and router deletion answer theirs (`network.go:1092`, `router.go:427`). `neutronInUseTypes` (`internal/common/neutron_errors.go:81`) gains `"security group": "SecurityGroupInUse"`, so the envelope's type is upstream's.
3. Otherwise the group is deleted.

The LS-SG chain removal moves after the commit, so a refused delete leaves it alone. Its failure is still only logged. That makes it the one recorded departure inside this feature's diff. The constitution's v1.5.0 migration plan names the LS-SG chain writes, and follow-up 2 lists this call at its new place. Nothing jumps to these chains, so it has no effect on traffic. Whether to remove the chains or wire them is the follow-up's decision.

**Rationale**: FR-008c, upstream's `delete_security_group` (read in source): a group bound to any port raises `SecurityGroupInUse`, an `InUse` and so `409`, and a missing group raises `SecurityGroupNotFound`, `404`.
- With the refusal, deleting a group never changes a port's list, so FR-007 holds without a rebuild and nothing needs undoing.
- On PostgreSQL a membership insert takes a key-share lock on the group's row through the foreign key, so a delete and a concurrent add serialise. Either the add commits first and the delete sees it (`409`), or the add waits and then fails on the foreign key, and answers `404` (`database.IsForeignKeyViolation`). On SQLite, immediate transactions serialise writers.
- Rules of other groups that name it as their remote group are removed with it, as upstream's foreign key does. The floating-IP filter skips remote-group rules (`portIngressRules`), so no filter changes.
- Terraform's group resource retries a delete that answers `409` until it succeeds or times out (`networking_secgroup_v2.go`, read in source).
- Removing the chain before the check, as today, would change node state for a request that is then refused. Dropping the removal would leave the chains of deleted groups for good, and the chart's purge removes only the chains of groups still in the database (`internal/neutron/teardown.go:99`).

**Alternatives**:
- Rebuild the filters of every port that carried the group, and undo on failure: restoring a deleted group, its rules and its memberships is a far larger undo, for a deletion upstream refuses anyway.
- Keep the cascade and let reconciliation catch up: contradicts FR-007 and v1.5.0's rule for requests.

## R23 — The floating-IP NAT: marked, changed in one step, the address last

**Decision** (revised in the third analysis):
- Every NAT rule Lightstack writes carries `-m comment --comment lightstack-fip`. `iptables -t nat -S PREROUTING` and `-S OUTPUT` print the mark back.
- **The shape.** A rule of the shape is `-d <F>/32 [-m comment --comment lightstack-fip] -j DNAT --to-destination <X>` in `PREROUTING` or `OUTPUT`, with nothing else in it. A port forwarding's rule, with `-p` and `--dport`, is not of the shape.
- **Lightstack's rules.** These are the marked rules, and the unmarked rules of the shape for an address that has a record or a marked rule. A record is a floating IP's row, associated or not. Earlier releases wrote them unmarked, and nobody else forwards an address Lightstack allocated.
  - Only Lightstack's rules are ever removed.
  - Every rule of the shape, Lightstack's or not, keeps the filter of the fixed IP it reaches.
- **Converging a floating IP F.** This runs under the node lock and reads F's record first. Then:
  - **F associated with fixed IP X.** First X's filter is ensured (R6–R7). Then `EnsureFloatingIP(F, X)` reads the two hooks and the uplink's addresses. In one `iptables-restore --noflush --wait 1` commit to the nat table it deletes every rule of F's that points elsewhere and appends the marked rule for X where it is missing, in both hooks. Last, it adds `F/32` to the uplink if it is missing. It reports the fixed IPs the deleted rules pointed at.
  - **F not associated, or without a record.** `WithdrawFloatingIP(F)` removes `F/32` from the uplink first, then deletes every rule of F's in one commit, and reports the fixed IPs they pointed at.
  - **The old filters.** For each fixed IP reported, the filter is removed unless a record still associates a floating IP with it, or a rule of the shape on the node still reaches it. The second check lists the node once (R24).
- **Removing a filter** is one commit too: it deletes every `FORWARD` jump to the chain, flushes the chain and deletes it. A chain, jump, rule or address that is already gone counts as removed.
- Every command goes through R6's runner, bounded at one second. `WithdrawFloatingIP` and `RemoveIngressFilter` return their errors, so the teardown's existing caller (`internal/neutron/teardown.go:67-68`) sees them too.
- **Commands per change**, each bounded at one second:
  - the filter reads its chain, restores it only on a difference, and checks its jump;
  - the NAT reads each hook once and commits at most once;
  - the address is read once and changed at most once;
  - a move or a removal lists the node once more, and removes the old filter in at most one commit.

**Rationale**: FR-007a, FR-008a and FR-010.
- The mark is what lets reconciliation remove a rule that has no record without ever touching an operator's (R24).
- An unmarked rule of the shape whose address has a record is Lightstack's from an earlier release. Converging that address replaces it, and the first pass after an upgrade does that for every associated floating IP.
- An unmarked rule whose address has neither a record nor a marked rule cannot be told from an operator's, so it stays. The filter it reaches stays with it: removing the filter would hand that traffic to the host's `FORWARD` policy, which Lightstack never sets and which is `ACCEPT` on a default host.
- One commit means a move never points at both fixed IPs, or at neither.
- The address is the last thing added and the first removed, so traffic for the floating IP reaches the node only while both the NAT and the filter are in place. The same holds for removal: a filter removed while a NAT still forwards would leave the fixed IP to the `FORWARD` policy.
- The NAT methods report what they replaced, so a change reads each hook once. Asking the node for the targets first would read them twice.

**Alternatives**:
- A Lightstack-owned nat chain jumped from `PREROUTING` and `OUTPUT`. Ownership would come from the chain's name, but rebuilding the whole chain from the records can apply another request's committed association before that request has built its filter. The jump's position relative to earlier releases' rules would also need a migration of its own.
- Recognise rules by the floating-IP pools' ranges instead of a mark: pools change, and an operator's own address inside a pool's range would be taken away.
- Keep the rules unmarked and remove by shape: indistinguishable from an operator's DNAT.
- A separate method that reports an address's targets before changing them: a second read of both hooks on every change.

## R24 — Reconciliation removes what no record holds

**Decision** (revised in the third and fourth analyses):
1. **List the node** (`NorthSouthState`): every NAT rule of the shape (R23), marked or not, with its floating and fixed address; every `LS-NSI-*` chain; and the uplink's addresses.
2. **Read the records**: every floating IP's row, associated or not, with its association if it has one. The read goes through a function on the reconciler that returns them and whether the read was complete.
3. **Work one address at a time** under the node lock (R8):
   - Every floating IP that has a record or a marked rule is converged (R23) where its listing differs. For one with no association, that removes its address and its rules, unmarked ones included, since its record makes them Lightstack's.
   - Every `LS-NSI-*` chain whose fixed IP no association names, and that no rule of the shape on the node still reaches, marked or not, is removed.
   - A floating IP whose only rules are unmarked and that has no record is left alone, and so is the filter its rules reach.

Limits:
- Nothing is removed after an incomplete read of the records. The associations it did read are still ensured, as `pruneDeletedNetworks` already does (`reconciler.go:202-210`).
- An address whose lock cannot be had within one second is skipped until the next pass.
- A real-mode node without an uplink skips the floating-IP step and logs an error, since nothing can be compared or applied.

For speed, the pass compares against its one listing. For each associated floating IP it checks the filter with one `iptables -S`, rebuilding it only on a difference (R7). It converges the NAT and the address only when the listing differs from the record: a rule missing or pointing elsewhere, or the address absent. A convergence re-reads everything under the lock before it writes.

**Rationale**: FR-010, SC-014, the second and third analyses, and constitution v1.5.0 (the loop removes only state of its own making, and nothing on an incomplete read).
- Listing the node before reading the records keeps a change made in between safe. Every path commits its record before it creates node state (R20), so anything the listing found has a record this read can see. If it has none, the record is gone, which is exactly the case for removal.
- The convergence re-reads the record under the lock before it writes, so a pass never acts on a listing a request has changed since.
- Leaving unmarked rules alone is safe only because their filters stay too. An unmarked rule of an earlier release, for a floating IP deleted before the upgrade, still forwards that address on every node that never withdrew it. The filter is the only thing between that traffic and the host's `FORWARD` policy.
- The address is part of the comparison, so an address removed out of band, by a netplan re-apply for instance, comes back within a pass, as today's pass re-adds it (`reconciler.go:133` → `ensureAddr`).
- The record read is a function so that a test can return a partial read. A pass that checked only for an error would remove after a partial read, and only a partial read shows it.
- The read covers unassociated floating IPs too. An unassociated floating IP's record still makes its unmarked rules Lightstack's (R23). Reading only associations would leave them looking unowned, and on the nodes that never withdrew them the address would keep forwarding to the old fixed IP.

**Alternatives**:
- Remove nothing and leave removals to the requests: the nodes that did not serve a removal would keep forwarding the address for ever, and once it is allocated again, the older rule forwards it to the old fixed IP.
- Remove every DNAT rule for an address in a pool: see R23.
- List only marked rules: a filter that an unmarked rule still reaches would be removed.

## R25 — A deleted port takes its floating IPs' association with it

**Decision** (added in the fourth analysis, paths and locks corrected in the fifth, `forceDelete` added in the sixth): every path that deletes a port clears the association of the floating IPs that name it, in the deleting transaction, and converges them after it. Three paths delete ports:
- `DeletePort` (`ports.go:516`): `DELETE /v2.0/ports/{id}`;
- Nova's `DeleteServer` (`internal/nova/handlers.go:2021`), which deletes the server's boot ports with its own SQL in its own transaction (`:2175`);
- `ReleaseInstancePorts` (`ports.go:1860`), which runs only when a boot fails (`handlers.go:1131-1135`).

The `forceDelete` server action (`ForceDeleteInstanceAction`, `:3720`) becomes the second caller of `DeleteServer`'s deletion. That deletion is extracted into one function: console tokens, libvirt domain, the transaction, the convergence after it, and the file cleanup. Today `forceDelete` deletes only the instance row and the domain. Its answer stays as today.

All three use one package-level Neutron helper on the caller's transaction, R2's pattern: `DisassociatePortFloatingIPs(ctx, tx, portIDs)`.
- It clears the floating IPs' `port_id`, `fixed_ip_address` and `router_id`, as upstream's port deletion does, and sets `status` to `DOWN`. Upstream's L3 agent reports `DOWN` afterwards; Lightstack sets it in the same step, its synchronous equivalent.
- It returns their addresses.

Each transaction takes its locks before it reads the floating IPs:
- `DeletePort` locks the port `FOR UPDATE`. Its port gone meanwhile → `404`, as today.
- `DeleteServer` and `ReleaseInstancePorts` lock the instance row, then the server's ports in id order, as membership changes do (R3).
  - An instance row gone meanwhile means a concurrent deletion won. `DeleteServer` then answers `404`, as today's exactly-once check does.
  - A failed boot's release carries on with whatever ports remain.
- Then each calls the helper, then deletes.

After the commit, the Neutron service converges each address (R23). Nova reaches it through a new `NeutronService` method, `ConvergeFloatingIPs`. A failed convergence is logged, naming the floating IP, and the deletion stands. Reconciliation removes what is left within one pass (R24). A tenant port that `DeleteServer` only detaches (`:2166`) keeps its floating IP, because the port still exists.

**Rationale**: FR-008d, FR-008e, SC-014, and upstream's ML2 `delete_port`, read in source. It calls `disassociate_floatingips` inside the same writer transaction as the port deletion (`neutron/plugins/ml2/plugin.py`), clearing `fixed_port_id`, `fixed_ip_address` and `router_id` (`neutron/db/l3_db.py`).
- **Today.** No path touches the floating IPs: neither `ports.go` nor Nova's `handlers.go` writes `floating_ips`. Only the foreign key clears `port_id`, so a floating IP keeps a stale fixed IP and `ACTIVE`, and its NAT stays in place. With this feature's reconciliation it would stay for up to 30 s.
- **The window.** The freed fixed IP can be allocated to another server in that window and receive the floating IP's traffic. A server's deletion is the common case, and it is Nova's own SQL, not a Neutron function, so the helper has to work on Nova's transaction.
- **The port lock first.** A floating-IP update that associates another floating IP with the port takes the port's lock first (R20). If the deletion read the floating IPs before taking that lock, an association committed in between would escape it, keeping a stale fixed IP and `ACTIVE`.
- **The instance first.** `DeleteServer` today changes volumes and ports before its instance row, the reverse of membership changes (instance, then ports; T018).
  - A group change and a server deletion on the same server lock the same ports and the instance in opposite orders. On PostgreSQL they can deadlock, and one of them answers `500`.
  - Volume attach locks the instance before the volume (`volume_attachment.go:163-167`), but an attach locks a different, available volume, so the two cannot deadlock. They can race: an attachment committed between the deletion's read of the attachments and its instance delete leaves a volume marked attached to a deleted server.
  - Taking the instance first removes both.
- **`forceDelete`.** It used to delete only the instance row (`handlers.go:3720-3768`), leaving the server's ports, their floating IPs and its volume attachments behind. Upstream's `force_delete` runs the same deletion as a delete: both call `_delete(…, self._do_delete, …)` (`nova/compute/api.py`, read in source). Terraform sends it for `force_delete = true`. Sharing one function is what keeps the two from drifting apart again.
- **No undo.** A failed convergence cannot be undone. The port's own teardown — its TAP, its FDB entry, its DHCP reservation — has already run without an undo (`teardownPortHost`, a recorded departure). Restoring the association would point the floating IPs at a port that no longer exists. This part stays a recorded departure, now limited to the case where removing the NAT fails, with reconciliation as the backstop.

**Alternatives**:
- Leave it to reconciliation: up to 30 s in which a freed fixed IP can receive another server's floating-IP traffic, and a record that names a fixed IP it no longer has.
- Fix only Neutron's two paths: server deletion, the common case, would keep the window.
- Leave `forceDelete` as it is and narrow SC-014: the ports and volumes it leaks today would stay leaked, and a client-visible difference from upstream would stay unrecorded.
- Disassociate and converge first, then delete the port in a second transaction: the deletion would be stored after the node work, the shape R20 rejects for a floating IP's deletion.
- Undo the port deletion when the convergence fails: the rest of the teardown has no undo, so the port would come back without its TAP.
