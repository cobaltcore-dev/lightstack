# Data Model: Server security-group membership reaches the ports

Round 1 (US1–US7) changed no schema: it changed which rows each path reads and writes, in which order, and under which locks. Round 2 (US8–US12, FR-019a–FR-058; research R26–R53) adds one migration, 076, and the node roles, units and per-node state at the end of this file.

## Tables

| Table | Role after this change | Read by | Written by |
|---|---|---|---|
| `port_security_groups (port_id, security_group_id)`, primary key on both; both cascade on delete (`006`, `053`) | The only list of a port's groups, read by enforcement and by the server's report | `portIngressRules` (the filter), the server report (R10), `GET /v2.0/ports` | boot allocation, `os-interface` with `net_id`, add, remove, port create and update |
| `ports (… device_id UUID, device_owner, updated_at)` | `device_id` ties a port to its server; `updated_at` moves on every group change | every membership path (locked `FOR UPDATE`, in id order) | the paths above |
| `instances` | Its row is the lock that orders a server's membership changes against its boot's port allocation | (locked only) | none |
| `floating_ips (port_id, fixed_ip_address, router_id, status, description)` | The association a floating IP's NAT and filter are converged to (R23); finds the filters to rebuild for a port. Each row is a record, associated or not | `RefreshPortFilters`, every convergence, reconciliation (every row) | create on a port, update, delete: each stored first, applied after, undone compare-and-set (R20). A port's or a server's deletion clears the association in its own transaction (R25) |
| `port_forwardings (floatingip_id … ON DELETE CASCADE)` | Deleted with their floating IP (migration 050) | a floating IP's delete, for its undo | restored with their floating IP when its withdrawal fails (R20) |
| `security_group_rules` | A group's rules, from which every filter of a port carrying the group is built | `portIngressRules`, the rule undo | rule add and delete, undone on a failed rebuild (R21) |
| `security_groups (… UNIQUE (project_id, name))` — round 2 (077): names may repeat, one `default` per project by a partial unique index | Resolution by name or id within one project (an ambiguous name answers `409`, FR-026); the listing's `id` and `name` filters; a delete is refused while a port carries the group | `ResolveSecurityGroup`, `ListSecurityGroups`, `DeleteSecurityGroup` (locked) | unchanged, except that a delete is refused while a port carries the group (R22) |
| `server_security_groups` | Retired: no reads, no writes. Its rows stay until a later migration drops the table | none | none |

## A port's groups

| Event | Resulting groups | Requirement |
|---|---|---|
| Boot, first port | The groups the create resolved, or `default` | FR-011, FR-012 |
| Boot, a later port | A copy of the boot's first port's current groups, read under the instance lock | FR-011 |
| `os-interface` with `net_id` | `{default}`, the group created if missing | FR-013 |
| `os-interface` with `port_id` | Unchanged | FR-013 |
| `POST /v2.0/ports` | absent → `{default}`; `[]` or `null` → `{}`; a list → that list | FR-019 |
| `addSecurityGroup g` | Every port of the server gains `g`. If any port already has it: `400`, nothing changes | FR-001 |
| `removeSecurityGroup g` | Every port carrying `g` loses it. If none carries it: `404` | FR-002, FR-005 |
| `PUT /v2.0/ports/{id}` | A list → that list; `[]` or `null` → `{}`; absent → unchanged | FR-017 |
| A rebuild that fails | Back to the groups before this request, on each port whose list still equals what this request left (compare-and-set); a port another request has changed since keeps that change | FR-008 |
| `DELETE /v2.0/security-groups/{id}` of a group a port carries | Unchanged: `409` | FR-008c |

The empty set is a legal state: a port with no group admits replies through its floating IP and nothing else.

## A floating IP's association

| Event | Stored | On the serving node before the answer | If the node cannot apply it | Requirement |
|---|---|---|---|---|
| `POST /v2.0/floatingips` with a port | The row, associated, in the transaction that makes the checks: its network and its router locked, its port read | Filter, then NAT, then the address | The row is deleted if it still holds what the insert wrote; `500` | FR-008, FR-008a |
| `POST` without a port | The row, unassociated | Nothing | — | — |
| `PUT` to a port (associate or move) | The new association; the new port, its router and then the row locked | New filter, NAT moved in one commit, the address; the old fixed IP's filter goes unless a record or any rule of the shape still reaches it | The old association is restored if the row still holds the new one; `500` | FR-008, FR-008a |
| `PUT` with `"port_id": null` | No association | The address, then the NAT, then the filter, with the same exception | The association is restored if the row is still unassociated; `500` | FR-008, FR-008a |
| A port or a server deleted while a floating IP is associated with it, `forceDelete` included | No port, fixed IP or router, status `DOWN`, in the transaction that deletes the port | Round 2: removed from the router namespace before the record goes (FR-040) | `500`; the port, its server and the association stay, re-applied from the records (FR-008d, FR-040) | FR-008d |
| `DELETE` of an associated floating IP | No row, in one transaction; its port forwardings go with it | The address, then the NAT, then the filter, with the same exception | The row and its port forwardings are restored. A port or router deleted meanwhile leaves it unassociated or without a router. A project or network deleted meanwhile, or the address allocated again, keeps it deleted. `500` | FR-008, FR-008a |
| A process that stopped after storing | As stored | Applied by the next reconciliation pass, removals included | — | FR-010 |

## A server's groups (derived, never stored)

`security_groups` lists, for each port whose `device_id` is the server (ports ordered by `created_at`, then `id`), each of its groups (ordered by name), as `{"name": <name, or the id when the name is empty>}`. The key is absent when the list is empty.

## Lock order

1. The `instances` row, `FOR UPDATE`: a server's membership changes and its boot's port allocation.
2. The server's `ports` rows in id order, `FOR UPDATE`: membership changes. `PUT /v2.0/ports` locks its one port.
3. The `subnets` row (`LockSubnet`): allocation only.

The floating-IP and group paths take their own locks, all `FOR UPDATE`, the only row lock in the shared SQL subset (`internal/database/doc.go:45`). A check whose subject cannot change takes none: a port's or a group's project never changes, and the foreign keys re-check existence at the write.
- A floating IP's create locks its network, then its router by id in a query of its own.
- An update locks the new port, then its router by id, and only then the floating IP's row. A port's deletion takes the same in that order, holding the port while it clears `port_id` on the floating IPs that name it.
- A delete locks the floating IP's row.
- A rule's add takes no lock; a rule's delete locks the rule's row.
- A group's delete locks the group's row.
- A port's deletion locks the port first. A server's deletion — `DELETE` and `forceDelete`, which share one function — and a failed boot's release lock the instance row and then the server's ports in id order, as membership changes do. Each then clears the floating IPs' association in the same transaction, then deletes (R25). Server deletion used to change volumes and ports before its instance row. Against a group change, which takes the instance and then the ports, that could deadlock. Against volume attach (`volume_attachment.go:163-167`) it could race, leaving a volume attached to a deleted server.

Apart from server deletion and a failed boot's release, none of them takes an `instances` row, and those two take it first, as membership changes do. The floating-IP update's and the port deletion's port lock come first in their transactions, as membership changes' do. A membership change never locks a network, a router or a floating IP. On SQLite `FOR UPDATE` is dropped, since immediate transactions serialise writers, and `FOR UPDATE OF` is refused, which is why the router is locked by a query of its own.

The node lock (below) is in-process and is taken only after a transaction commits, never inside one.

## In-process state (the Neutron service, per node)

- One lock for the node's floating-IP state, a one-slot channel. Every change to that state holds it for one address, from reading the records to applying them, and acquires it with a one-second deadline. That covers a filter rebuild, a floating IP's convergence and a reconciliation step. Missing the deadline is a failed change on the request path, and a skipped address in a pass (R8).
- One interface for the node's floating-IP dataplane, implemented by the router manager: ensure and remove a filter; ensure and withdraw a floating IP's NAT, each reporting the fixed IPs its replaced rules pointed at; and list the node's rules of the shape, its `LS-NSI-*` chains and the uplink's addresses. Tests replace it with an in-memory fake that holds state and can record, block or fail each method (R9).
- One function for the uplink: the kernel's answer in real mode, an empty name in stub mode. Tests make it fail. A request without an uplink in real mode has a failed change (R9).
- On the reconciler, one function for reading the records: every floating IP's row with its association, if any, and whether the read was complete. Tests make it return a partial read (R24).
- On the `RouterManager`, a command runner: a function field that runs `iptables`, `iptables-restore` and `ip` under a one-second context. Tests replace it with a fake (R6).

## Dataplane state (per node, real mode)

- Chain `LS-NSI-<fixed IP, dots as dashes>` in table `filter`: replies (`RELATED,ESTABLISHED`), one `ACCEPT` per allowance (IPv4 ingress rules without a remote group), then `DROP`. It is replaced as a whole, never edited in place (R6). Removed in one commit with its jumps, after the NAT that reached it (R23).
- The jump `FORWARD -i <uplink> -d <fixed IP> -j LS-NSI-…`, ensured once the chain exists.
- The floating IP's NAT: `-d <floating IP>/32 -m comment --comment lightstack-fip -j DNAT --to-destination <fixed IP>` in `nat PREROUTING` and `nat OUTPUT`. It is added after the filter and changed in one commit (R23).
- Rules of the shape are those with exactly that form, marked or not. Lightstack's rules are the marked ones, and the unmarked ones for an address that has a record (a floating IP's row, associated or not) or a marked rule: an earlier release's. Only Lightstack's rules are ever removed. Any rule of the shape keeps the filter of the fixed IP it reaches.
- The floating address, `<floating IP>/32` on the uplink: the last thing added, the first removed (R23).
- All of it present exactly while a record associates the floating IP; reconciliation adds and removes it (R24).


---

# Round 2 (2026-11-20)

## Migration 076 (a `.postgres`/`.sqlite` pair: it creates a table)

| Change | Type | Why |
|---|---|---|
| `subnet_allocation_pools (id, subnet_id → subnets ON DELETE CASCADE, first_ip, last_ip)` | new table | A subnet's pools (FR-030, R30) |
| `subnets.pools_stored` | nullable integer, `ADD COLUMN` | True: the rows are the pools, none meaning none. False or null: the default pool, which is how every subnet from before reads (FR-032a) |
| `ports.binding_host_id` | nullable text, `ADD COLUMN` | The node whose TAP the port is, reported as `binding:host_id` (R32) |
| `networks.ct_zone` | nullable integer, `ADD COLUMN`, plus a unique index | The network's connection-tracking zone, assigned at creation from 4097 (R32) |
| `security_groups.revision_number` | integer, default 0, `ADD COLUMN` | Upstream's revision number (FR-050, R46) |
| `security_groups.stateful` | integer boolean, default 1, `ADD COLUMN` | Stateless groups (FR-051, R47) |
| the seeded project's `default` | two rules inserted | Upstream's member rules, so a fresh install matches (FR-043, R39) |

## Migration 077 (a pair; its SQLite file runs outside a transaction)

| Change | Why |
|---|---|
| `security_groups` loses `UNIQUE (project_id, name)` (PostgreSQL: drop the constraint; SQLite: rebuild with foreign keys off, then `foreign_key_check`) | Duplicate names, as upstream (FR-026, R53) |
| `CREATE UNIQUE INDEX security_groups_one_default ON security_groups (project_id) WHERE name = 'default'` | Exactly one `default` per project; `EnsureDefaultSecurityGroup`'s conflict target |


No table that others reference is rebuilt on SQLite (`migrations/README.md`). Down drops the table, the columns and the index, and deletes the two rules.

## New kinds of port

| `device_owner` | Project | Fixed IPs | Created / removed | Requirement |
|---|---|---|---|---|
| `network:router_interface` | the router's | the subnet's gateway | with the interface, in one transaction with its `router_interfaces` row | FR-045, R31 |
| `network:router_gateway` | none | the requested address or one from the external pools | with the gateway; updated on a changed requested address; removed with the gateway | FR-038, R31 |
| `network:dhcp` | the network's | one per DHCP subnet, the lowest free pool address | with the network's first DHCP subnet; removed with its last | FR-037, R29 |

All three are trusted by the port firewall (R32). The gateway port is visible to administrators only, as upstream's.

## Addresses on a subnet

Free means: inside a pool, not the gateway, not a port's fixed IP, not a floating IP's address on the network. Every allocation holds the subnet's row lock (`LockSubnet`) while it chooses and stores, and a floating-IP create the allocation lock before it (R30, R36), so no two owners ever get one address (FR-032). One allocator serves every automatic choice:

| Owner | Choice |
|---|---|
| a port's fixed IP | random among free pool addresses |
| a DHCP port | the lowest free |
| a floating IP | the lowest free across the external network's IPv4 subnets, under the allocation lock |
| a router's gateway | random among free pool addresses of the external network |

A requested address must be inside the subnet, not its network or broadcast address, and free; it may lie outside the pools.

## Node roles (R26)

| State | Lives on |
|---|---|
| `qrouter-<id>` and `qdhcp-<network>` namespaces and everything in them; host routes, proxy neighbour entries and forward rules for router and floating addresses | the network node only |
| bridges, VXLAN devices and FDB entries | every node |
| a port's chains | the node in its `binding_host_id` |

## Units, their locks and who applies them

| Unit | Rendered from | Lock (in-process, within the call's 900 ms shared deadline) | Applied by |
|---|---|---|---|
| router | the router, its interface and gateway ports, their subnets, its floating IPs and port forwardings | per router, on the network node | the network node (R27, R28) |
| DHCP of a network | its DHCP subnets and port, every port's reservation, the metadata listener | per network, on the network node | the network node (R29) |
| a node's ports | each port's groups, their rules and statefulness, remote groups' member addresses, the port's addresses, MAC and network zone | per node | the port's node (R32); a moved or cleared binding also sends `absent` to the old node (R27) |

Each unit converges to its records, or to "absent" during a deletion (R34). A floating IP, a port forwarding, a router interface and a gateway all change their router's unit.

## Lock order, additions

Row locks stay as above. Unit locks are taken only after a transaction commits, or between a deletion's check and its final transaction, never inside one. A request holds at most one unit lock at a time, so unit locks have no order among themselves. The floating-IP allocation lock is a transaction-scoped advisory lock taken first in a create's transaction (R36).

## Dataplane state, round 2 (real mode)

**Network node, per router** (R28):
- namespace `qrouter-<router id>` with forwarding and upstream's ARP sysctls;
- veth `qg-<gw port[:11]>` (namespace) / `lg-<same>` (host); `qg-` holds the gateway address with the external prefix and each floating address as `/32`; default route to the external subnet's gateway;
- per interface, veth `qr-<port[:11]>` (namespace, the subnet's gateway address) / `lr-<same>` on the network's bridge;
- nat and mangle chains `ls-l3-*` (floating-IP DNAT/SNAT, gateway SNAT and its exception, hairpin mark, port forwardings `ls-l3-fip-pf`/`ls-pf-<id[:8]>`), every rule commented `lightstack`;
- on the host: `/32` routes via `lg-`, proxy neighbour entries on the uplink, `proxy_arp=1` on `lg-`, forward rules in a Lightstack chain jumped from `FORWARD`.

**Network node, per network with DHCP** (R29): namespace `qdhcp-<network id>`; veth `tap<dhcp port[:11]>` (namespace, the DHCP addresses and `169.254.169.254/32`) / `ld-<dhcp port[:11]>` on the bridge; `dnsmasq` with static ranges per subnet; Lightstack's metadata listener on `169.254.169.254:80` inside.

**Every node, per bound port** (R32): chains `ls-i`, `ls-o`, `ls-s` + `<port[:10]>` in filter (IPv4 and IPv6), dispatch in `ls-sg-chain` from `ls-filter-top`; `raw PREROUTING` zone rules per bridge and TAP, or `CT --notrack` for a port whose groups are stateless (R47). The TAP is libvirt's, named `tap<port[:11]>`. Trusted host ends (`lr-`, `ld-`) are accepted by name.

**Teardown** (R48) converges every unit to absent and removes every node's `ls-` chains; its database-less fallback removes `qrouter-*` and `qdhcp-*` by prefix.

**Retired** (removed by reconciliation where recognised, FR-044): bridge addresses, the per-bridge metadata DNAT and host `dnsmasq`; host-namespace floating-IP NAT, `/32` uplink addresses and `LS-NSI-*` filters; host egress SNAT; `LS-SG-*` chains; Neutron's `tap-<port8>`.

## In-process state, round 2

Replacing the node lock and `floatingIPDataplane` (R41): the unit locks above; `routerDataplane`, `dhcpDataplane`, `portFirewall` and `nodeCaller`, each faked in tests; the metadata listeners, one per `qdhcp-*` namespace, owned by the DHCP unit.
