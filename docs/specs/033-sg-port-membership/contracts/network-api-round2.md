# Contract: Networking API changes, round 2

Every answer below is upstream's unless it names a register entry. Errors are Neutron envelopes (`{"NeutronError": {"type", "message", "detail"}}`) with the type named.

## `GET /v2.0/security-groups` (FR-019a, FR-019b; R38)

| Parameter | Effect |
|---|---|
| `id`, `name`, `description` | equality; repeatable, any value matching |
| `project_id`, `tenant_id` | an administrator: the named projects' groups, as upstream; anyone else: own groups when the own project is named, else nothing |
| `shared` | `false` → every group; `true` → none |
| `stateful` | equality on the stored value |
| `revision_number` | equality on the stored value |
| `tags`, `tags-any` | none listed |
| `not-tags`, `not-tags-any` | every group listed |
| a boolean that is not one | `400 BadRequest` |

Every group in a list, show, create or update body carries its `stateful` and `revision_number`, `"shared": false` and `"tags": []`. `revision_number` starts at 0 and goes up on every update of the group and every rule added or deleted (FR-050).

**Stateless groups** (FR-051): `POST` with `"stateful": false` creates one. A `PUT` changing `stateful` while a port carries the group → `409 SecurityGroupInUse`. A port create or update that would make one port carry both kinds → `409 SecurityGroupConflict`; an `addSecurityGroup` that would make any of the server's ports carry both → `409` (Nova's `SecurityGroupConnectionStateConflict`); ports of one server may differ, and an interface attach is never refused for it; a server create mixing both → the server ends `ERROR` with a fault. `PUT`/`DELETE` with `If-Match: revision_number=<n>` and a different stored revision → `412 RevisionNumberConstraintFailed`, nothing changed. `GET /v2.0/extensions` lists `stateful-security-group`.

## Security groups: `default` and names (FR-024–FR-026, FR-043; R39)

| Request | Answer |
|---|---|
| `DELETE` `default`, a port carries it | `409 SecurityGroupInUse` |
| `DELETE` `default`, unused, non-administrator | `409 SecurityGroupCannotRemoveDefault` |
| `DELETE` `default`, unused, administrator | `204` |
| `PUT` `default` with `name` other than `default` (any case) | `409 SecurityGroupCannotUpdateDefault` |
| `POST` or `PUT` with `name` `default` (any case) | `409 SecurityGroupDefaultAlreadyExists` |
| `POST` or `PUT` with another group's name in the project | allowed, as upstream (FR-026) |
| `GET /v2.0/security-groups/{name}` with a name several groups share | `404`, as upstream answers any name there |
| `POST` a group | `201`; `security_group_rules` holds two egress rules, IPv4 and IPv6, no remote |
| a project's `default`, created on demand | the two egress rules and two ingress rules whose `remote_group_id` is `default`'s id |

## Subnets: `allocation_pools` (FR-030, FR-032; R30)

`POST` and `PUT /v2.0/subnets[/{id}]` accept `allocation_pools: [{"start", "end"}, …]`. Absent on create → the default pool; `[]` → no pool. Every read reports the stored pools.

| Check, in this order | Answer |
|---|---|
| an address that does not parse, or `start` after `end`, or another IP version | `400 InvalidAllocationPool` |
| a pool reaching outside the subnet's usable range | `400 OutOfBoundsAllocationPool` |
| two pools overlapping | `409 OverlappingAllocationPools` |
| the gateway inside a pool, on create, on a pools update, or on a gateway update against the stored pools | `409 GatewayConflictWithAllocationPools` |

An update replaces the pools; existing allocations stay. `DELETE /v2.0/subnets/{id}` while a port other than the DHCP port has an address on it: `409 SubnetInUse` (FR-052).

## Administrators' listings, shows and writes (FR-057, FR-058; R50)

| Caller, request | Answer |
|---|---|
| administrator, `GET /v2.0/<collection>` | every project's resources |
| administrator, `?project_id=B` or `?tenant_id=B` (repeatable) | B's resources only |
| administrator, `GET /v2.0/<collection>/<id of B's>` | `200`, the resource |
| administrator, a name where a name is accepted, naming B's resource | not resolved (`404`), as any name outside the caller's project |
| administrator, `PUT`/`DELETE` or an action on B's resource by id | allowed, as upstream (FR-058) |
| administrator, a create referencing B's resource by id | allowed; the new resource belongs to the project the request names, else the administrator's |
| administrator, B's security group on a port of another project, by id | allowed, as upstream (FR-018) |
| member | as before: own project (networks: also shared and external) for reads and writes |

Collections: `networks`, `subnets`, `ports`, `security-groups`, `security-group-rules`, `routers`, `floatingips` (and `floatingips/{id}/port_forwardings`), `qos/policies`, `rbac-policies`, `trunks`, `address-scopes`, `subnetpools`, `network-ip-availabilities`, `metering/metering-labels`, `metering/metering-label-rules`.

## Ports (FR-032, FR-037, FR-038; R29–R32)

- An automatic fixed IP comes from the subnet's pools. A requested one must be inside the subnet and free (`409 IpAddressAlreadyAllocated` otherwise), and not its network or broadcast address (`400 InvalidIpForSubnet`, FR-053).
- `binding:host_id` (FR-056): the node a port's TAP is on, set by Nova at boot and attach, empty when unbound; shown to administrators, as upstream.
- New `device_owner` values appear in listings: `network:router_gateway` (administrators only), `network:dhcp` (the network's project).

## Routers (FR-038, FR-039; R31)

| Request | Answer |
|---|---|
| gateway set | `external_gateway_info.external_fixed_ips: [{"subnet_id", "ip_address"}]` reported, from the requested address or the pools |
| `external_fixed_ips` requested by a non-administrator | `403` |
| a requested address in no subnet of the network | `400 InvalidIpForNetwork` |
| a requested address equal to a subnet's gateway | `400` |
| a requested address already held | `409 IpAddressAlreadyAllocated` |
| `PUT .../remove_router_interface` while a floating IP of this router reaches a fixed IP on that subnet | `409 RouterInterfaceInUseByFloatingIP` |
| clearing or changing the gateway while a floating IP uses the router | `409 RouterExternalGatewayInUseByFloatingIp` |
| `PUT .../add_router_interface` for a subnet the router already has | `400` (FR-055) |
| `PUT .../add_router_interface` for a subnet without a gateway | `400` (FR-055) |
| a failure applying any of the above on the network node | `500`, nothing changed (FR-008, FR-040) |

## Floating IPs (FR-027–FR-029, FR-031; R36)

| Request | Answer |
|---|---|
| ten creates on one network at once | ten `201`, ten distinct addresses |
| create on an external network with no IPv4 subnet | `400` |
| create when the pools are taken | `400 ExternalIpAddressExhausted` |
| create or associate with a port no router joins to the floating network | `404 ExternalGatewayForFloatingIPNotFound` |
| associate a fixed IP that already has a floating IP of the same network | `409 FloatingIPPortAlreadyAssociated` |
| a requested `fixed_ip_address` that is not the port's | `400` (FR-054) |
| `subnet_id`, `floating_ip_address` in a create | honoured: the address from that subnet, or that address if free (FR-054) |
| a requested `floating_ip_address` that is taken | `409 IpAddressAlreadyAllocated` (FR-054) |

## Port forwardings (FR-049; R37)

Create, update and delete answer as today when the router namespace applies them; `500` and nothing changed when it cannot. Deleting a floating IP removes its port forwardings' rules with it.
