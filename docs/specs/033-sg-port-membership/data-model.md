# Data Model: Server security-group membership reaches the ports

No schema change and no migration. What changes is which rows each path reads and writes, in which order, and under which locks.

## Tables

| Table | Role after this change | Read by | Written by |
|---|---|---|---|
| `port_security_groups (port_id, security_group_id)`, primary key on both; both cascade on delete (`006`, `053`) | The only list of a port's groups, read by enforcement and by the server's report | `portIngressRules` (the filter), the server report (R10), `GET /v2.0/ports` | boot allocation, `os-interface` with `net_id`, add, remove, port create and update |
| `ports (… device_id UUID, device_owner, updated_at)` | `device_id` ties a port to its server; `updated_at` moves on every group change | every membership path (locked `FOR UPDATE`, in id order) | the paths above |
| `instances` | Its row is the lock that orders a server's membership changes against its boot's port allocation | (locked only) | none |
| `floating_ips (port_id, fixed_ip_address, router_id, status, description)` | The association a floating IP's NAT and filter are converged to (R23); finds the filters to rebuild for a port. Each row is a record, associated or not | `RefreshPortFilters`, every convergence, reconciliation (every row) | create on a port, update, delete: each stored first, applied after, undone compare-and-set (R20). A port's or a server's deletion clears the association in its own transaction (R25) |
| `port_forwardings (floatingip_id … ON DELETE CASCADE)` | Deleted with their floating IP (migration 050) | a floating IP's delete, for its undo | restored with their floating IP when its withdrawal fails (R20) |
| `security_group_rules` | A group's rules, from which every filter of a port carrying the group is built | `portIngressRules`, the rule undo | rule add and delete, undone on a failed rebuild (R21) |
| `security_groups (… UNIQUE (project_id, name))` | Resolution by name or id within one project; the listing's `id` and `name` filters; a delete is refused while a port carries the group | `ResolveSecurityGroup`, `ListSecurityGroups`, `DeleteSecurityGroup` (locked) | unchanged, except that a delete is refused while a port carries the group (R22) |
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
| A port or a server deleted while a floating IP is associated with it, `forceDelete` included | No port, fixed IP or router, status `DOWN`, in the transaction that deletes the port | The address, then the NAT, then the filter, with the same exception | Logged; the deletion stands, and reconciliation completes the removal within one pass (a recorded departure) | FR-008d |
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
