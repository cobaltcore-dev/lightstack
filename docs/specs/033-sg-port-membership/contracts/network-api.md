# Contract: the Networking API

Errors use the Neutron error envelope (`NeutronError` with `type`, `message`, `detail`).

## `security_groups` on `POST /v2.0/ports` and `PUT /v2.0/ports/{id}`

| Value in the body | `POST` | `PUT` |
|---|---|---|
| absent | the project's `default` (created if missing) | groups unchanged |
| `null` | no group | no group |
| `[]` | no group | no group |
| `["<id>", …]` | exactly these | exactly these, replacing the old list |

| Check, in this order | Answer | Effect |
|---|---|---|
| An entry that is not a UUID string | `400`: "Invalid input for security_groups. Reason: '`<value>`' is not a valid UUID." | none |
| A repeated entry | `400`: "Invalid input for security_groups. Reason: Duplicate items in the list: '`<id>`'." | none |
| The port is missing or another project's (`PUT`) | `404` (`PortNotFound`) | none |
| An id that names no group of the port's project, administrators included (register entry 5) | `404` (`SecurityGroupNotFound`) | none |
| Success (`PUT`) | `200` with the port | The list is replaced, `updated_at` moves, and the port's floating-IP filters are rebuilt on the serving node before the answer |
| Success (`POST`) | `201` with the port | As today, with the list above |
| `PUT`: the serving node cannot rebuild the filter (a command fails or overruns one second, or the lock wait does) | `500` | The list goes back to what it was, unless another request has changed it since (compare-and-set); the old filter stays whole |

`security_groups` sent with other fields (`name`, `admin_state_up`, …) applies together with them. A `PUT` body carrying only `security_groups` is a valid update, no longer `400 no updates provided`.

## `GET /v2.0/security-groups`

| Query | Lists |
|---|---|
| none | every group of the caller's project, as today |
| `?id=<a>` | the group `a`, if it is the caller's |
| `?id=<a>&id=<b>` | groups `a` and `b`, as far as they are the caller's |
| `?name=<n>` | the caller's group named `n` (names are unique per project) |
| `?id=…&name=…` | the groups matching both |
| An id or a name that matches nothing, a malformed id, or another project's id | `200` with `[]` |

Other query parameters are ignored, as today. Consumers: Horizon's *Edit Security Groups* and instance page (`?id=` repeated) and Terraform's `openstack_networking_secgroup_v2` data source (`?name=`).

## Floating IPs: `POST`, `PUT` and `DELETE /v2.0/floatingips`

Every answer that exists today keeps its shape. What changes is when a success is given: only once the serving node has applied the change (`contracts/floating-ip-dataplane.md`). A network, port or router the request names that another request deletes while it runs answers `404`, never `500`.

| Request | Success, as today | When the serving node cannot apply it (a command fails or overruns one second, or the lock wait does) |
|---|---|---|
| `POST` with `port_id` | `201`; the filter, NAT and address are in place before the answer | `500`. The floating IP is removed again, unless another request has changed it since (compare-and-set) |
| `POST` without `port_id` | `201`; nothing on the node | — |
| `PUT` with a `port_id` (associate or move) | `200`; the NAT points at the new fixed IP only, its filter is in place, and the old fixed IP's filter is gone unless another floating IP reaches it | `500`. The old association is back, unless another request has changed it since |
| `PUT` with `"port_id": null` | `200`; the address, NAT and filter are gone | `500`. The association is back, unless another request has changed it since |
| `PUT` with only `description` | `200`; nothing on the node | — |
| `DELETE` of an associated floating IP | `204`; nothing on the node forwards it any more | `500`. The floating IP is back, with its association and the port forwardings deleted with it. If another request deleted its port meanwhile, it comes back unassociated; if its router, without a router. If its network went, or its address was allocated again, it stays deleted |
| `DELETE` of an unassociated floating IP | `204` | — |

## Security-group rules: `POST` and `DELETE /v2.0/security-group-rules`

| Request | Success, as today | When the serving node cannot rebuild a filter of a port carrying the group |
|---|---|---|
| `POST` | `201`; the filter of every port carrying the group that has a floating IP includes the rule before the answer | `500`. The rule is removed again |
| `DELETE` | `204`; those filters no longer include it | `500`. The rule is restored, with its id, unless its group is gone |

A group that a rule's `POST` names and that another request deletes while it runs answers `404`, never `500`.

## `DELETE /v2.0/security-groups/{id}`

| Situation | Answer | Effect |
|---|---|---|
| A port carries the group | `409`, type `SecurityGroupInUse`, the group's id in `detail`, as network and router deletion answer theirs (was `204`, and the group was dropped from every port) | none |
| The id names no group of the caller's project, administrators included (register entry 5) | `404` (was `204`) | none |
| Otherwise | `204`, as today | The group and its rules are deleted. Rules of other groups that name it as their remote group go with it, as upstream |

## Deleting a port or a server: its floating IPs

`DELETE /v2.0/ports/{id}` and a server's deletion keep their answers. A server's deletion is `DELETE /v2.1/servers/{id}`, or the `forceDelete` action, which now deletes the same way, as upstream's does; either deletes the ports its boot created. What changes is what happens to a floating IP associated with a deleted port.

| | Before | After |
|---|---|---|
| The floating IP's record | `port_id` cleared by the foreign key; `fixed_ip_address`, `router_id` and `status` (`ACTIVE`) left as they were | `port_id`, `fixed_ip_address` and `router_id` cleared in the transaction that deletes the port, as upstream's port deletion does, and `status` `DOWN` in the same step (upstream's L3 agent reports `DOWN` afterwards) |
| Its NAT and filter on the serving node | Left in place: no request and no reconciliation pass removes them | Gone before the answer. If their removal fails, the deletion still answers as today, the failure is logged, and reconciliation removes them within one pass |
| The `forceDelete` action | Deletes the instance row and its libvirt domain only; the server's ports, their floating IPs and its volume attachments stay | The same deletion as `DELETE /v2.1/servers/{id}`: boot ports deleted with their floating IPs disassociated, ports attached by id detached, volumes released or deleted on termination |
