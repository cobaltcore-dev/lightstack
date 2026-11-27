# Contract: Networking API changes, round 2

Every answer below is upstream's unless it names a register entry. Errors are Neutron envelopes (`{"NeutronError":
{"type", "message", "detail"}}`) with the type named.

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
| any | the project's `default` group is ensured first: an administrator's first named project's, else the caller's (SRC-21) |

`GET /v2.0/security-group-rules` (SRC-22): `id`, `security_group_id`, `direction`, `ethertype`, `protocol`,
`port_range_min`, `port_range_max`, `remote_ip_prefix`, `remote_group_id` filter by equality, repeatable; the project's
`default` is ensured as for groups. `POST /v2.0/security-groups` ensures the new group's project's `default` too.

Every group in a list, show, create or update body carries its `stateful` and `revision_number`, `"shared": false` and
`"tags": []`. `revision_number` starts at 0 and goes up on every update of the group and every rule added or deleted
(FR-050).

**Stateless groups** (FR-051): `POST` with `"stateful": false` creates one. A `PUT` changing `stateful` while a port
carries the group → `409 SecurityGroupInUse`. A port create or update that would make one port carry both kinds → `409
SecurityGroupConflict`; an `addSecurityGroup` that would make any of the server's ports carry both → `409` (Nova's
`SecurityGroupConnectionStateConflict`); ports of one server may differ, and an interface attach is never refused for
it; a server create mixing both → the server ends `ERROR` with a fault. `PUT`/`DELETE` with `If-Match:
revision_number=<n>` and a different stored revision → `412 RevisionNumberConstraintFailed`, nothing changed. The
header's entity tags are webob's strong ones — the quoted tags but `W/` ones, or the whole header when none is quoted —
and only a `revision_number=` tag constrains: `*`, `"abc"` and `"abc", revision_number=5` (an unquoted tag beside a
quoted one is not a tag) constrain nothing; two revision tags → `400` `Bad if-match request: Multiple revision_number
etags are not supported..`; a number that does not parse → `400` `Bad if-match request: Revision number etag must be in
the format of revision_number=<int>.` `GET /v2.0/extensions` lists `stateful-security-group`.

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

## `POST /v2.0/security-group-rules`: what a rule may hold (FR-059; SPEC-033 T169)

Checked in upstream's order, before the group's ownership (`404`): each attribute's converter, then
`_validate_security_group_rule`. Every refusal is `400` and stores nothing.

| Rule | Answer |
|---|---|
| `direction` not `ingress` or `egress` | `BadRequest`, `Invalid input for direction. Reason: <value> is not in valid_values.` |
| `protocol` not `any`, a number from 0 to 255, or one of upstream's 25 names (any case) | `SecurityGroupRuleInvalidProtocol` |
| `port_range_min` or `port_range_max` outside 0–65535 | `SecurityGroupInvalidPortValue` |
| `ethertype` not `IPv4` or `IPv6`, in any case (`""` included) | `SecurityGroupRuleInvalidEtherType` |
| `remote_ip_prefix` not an address, or an address with a prefix length, netmask or hostmask | `InvalidCIDR` |
| ports and no protocol | `SecurityGroupProtocolRequiredWithPorts` |
| ports on a protocol other than TCP, UDP, SCTP, DCCP, UDP-Lite or ICMP | `SecurityGroupInvalidProtocolForPort`, naming the protocol's number |
| TCP, UDP, SCTP, DCCP or UDP-Lite: port 0 | `SecurityGroupInvalidPortValue` |
| the same: one end missing, or `port_range_min` > `port_range_max` | `SecurityGroupInvalidPortRange` |
| ICMP: type or code above 255 | `SecurityGroupInvalidIcmpValue` |
| ICMP: a code without a type | `SecurityGroupMissingIcmpType` |
| `remote_ip_prefix` of the other family | `SecurityGroupRuleParameterConflict` |
| an IPv6 protocol (`ipv6-icmp`, `icmpv6`, `ipv6-encap`, `-frag`, `-nonxt`, `-opts`, `-route`, or their numbers) in an IPv4 rule | `SecurityGroupEthertypeConflictWithProtocol` |
| `remote_ip_prefix` and `remote_group_id` both | `SecurityGroupMultipleRemoteEntites` |

Stored and answered as upstream stores them: an address as its host prefix (`192.0.2.10` → `192.0.2.10/32`), a netmask
or hostmask as a prefix length (`10.0.0.0/255.0.0.0` → `10.0.0.0/8`), host bits kept (`10.0.0.5/24`); `""` as none; a
protocol name lowercased, `any` as none, a number as given; in an IPv6 rule `icmp` and `icmpv6` as `ipv6-icmp`, `1` as
`58`; ports 1–65535 as none; the ethertype as `IPv4` or `IPv6`.

## Subnets: `allocation_pools` (FR-030, FR-032; R30)

`POST` and `PUT /v2.0/subnets[/{id}]` accept `allocation_pools: [{"start", "end"}, …]`. Absent on create → the default
pool; `[]` → no pool. Every read reports the stored pools.

| Check, in this order | Answer |
|---|---|
| an address that does not parse, or `start` after `end`, or another IP version | `400 InvalidAllocationPool` |
| a pool reaching outside the subnet's usable range | `400 OutOfBoundsAllocationPool` |
| two pools overlapping | `409 OverlappingAllocationPools` |
| the gateway inside a pool, on create, on a pools update, or on a gateway update against the stored pools | `409 GatewayConflictWithAllocationPools` |

`dns_nameservers` on `POST /v2.0/subnets` (FR-060), checked before anything else, in this order: an entry that is no IP
address, has a zone, or is an IPv4 address with leading zeros → `400` (`Invalid input for dns_nameservers. Reason: …`);
one entry twice → `400` (`Duplicate nameserver …`); more than 5 → `400 DNSNameServersExhausted`; one of the other IP
version → `400 InvalidInput`. Stored as given.

An update replaces the pools; existing allocations stay. `DELETE /v2.0/subnets/{id}` while a port other than the DHCP
port, or a floating IP, has an address on it: `409 SubnetInUse` (FR-052).

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

Collections: `networks`, `subnets`, `ports`, `security-groups`, `security-group-rules`, `routers`, `floatingips` (and
`floatingips/{id}/port_forwardings`), `qos/policies`, `rbac-policies`, `trunks`, `address-scopes`, `subnetpools`,
`network-ip-availabilities`, `metering/metering-labels`, `metering/metering-label-rules`.

## Ports (FR-032, FR-037, FR-038; R29–R32)

- An automatic fixed IP comes from the subnet's pools. A requested one must be inside the subnet and free (`409
  IpAddressAlreadyAllocated` otherwise), and not its network or broadcast address (`400 InvalidIpForSubnet`, FR-053).
- `binding:host_id` (FR-056): the node a port's TAP is on, set by Nova at boot and attach, empty when unbound; shown to
  administrators, as upstream. `POST`, `GET`, `PUT` and the listing answer the same body for a port, read back as
  stored.
- Who reads a port (FR-061): its project and its network's project, administrators any; a shared network is every
  project's to read, the ports on it are not. A marker naming a port the caller may not read answers `404`, as one
  naming none.
- A `device_owner` starting `network:` (FR-062): an administrator's or the network's project's to set, on create and
  update; `403 PolicyNotAuthorized` (`(rule:create_port:device_owner) is disallowed by policy`, `update_port` alike)
  otherwise. The network's DHCP port is the network's project's.
- A router's ports (FR-065): `DELETE /v2.0/ports/{id}` of an interface's port, or of the gateway port of a router that
  exists, answers `409 ServicePortInUse` ("Port <id> cannot be deleted directly via the port API: has device owner
  network:router_interface.", `network:router_gateway` alike), administrators included, and changes nothing; the
  router's own requests remove them. A port that only names a router as its owner and device is deleted as any port.
- New `device_owner` values appear in listings: `network:router_gateway` (administrators and the external network's
  owner), `network:dhcp` (the network's project).

## Routers (FR-038, FR-039; R31)

| Request | Answer |
|---|---|
| gateway set | `external_gateway_info.external_fixed_ips: [{"subnet_id", "ip_address"}]` reported, from the requested address or the pools |
| `external_fixed_ips` requested by a non-administrator | `403` |
| `enable_snat` given by a non-administrator, on create or update, whatever its value | `403 PolicyNotAuthorized` (`(rule:create_router:external_gateway_info:enable_snat) is disallowed by policy`, `update_router` alike) |
| a requested address in no subnet of the network | `400 InvalidIpForNetwork` |
| a requested address equal to a subnet's gateway | allocated while nothing holds it, `409 IpAddressAlreadyAllocated` once something does, as for a port and a floating IP (SRC-26) |
| a requested address already held | `409 IpAddressAlreadyAllocated` |
| `PUT .../remove_router_interface` while a floating IP of this router reaches a fixed IP, or one of its port forwardings an internal address, inside that subnet on a port of the subnet's network | `409 RouterInterfaceInUseByFloatingIP` |
| clearing or changing the gateway while a floating IP uses the router | `409 RouterExternalGatewayInUseByFloatingIp` |
| `PUT .../add_router_interface` for a subnet the router already has | `400` (FR-055) |
| `PUT .../add_router_interface` for a subnet without a gateway | `400` (FR-055) |
| `PUT .../add_router_interface` by a non-administrator for another project's subnet, a shared network's included | `400 BadRequest`, "Bad router request: Cannot add interface to router because subnet <id> is not owned by project making the request." (FR-055) |
| `PUT .../add_router_interface` for the subnet of the router's gateway, or one overlapping a subnet of its interfaces | `400` (FR-055) |
| `PUT .../add_router_interface` for a subnet whose gateway address a port holds | `409 IpAddressAlreadyAllocated` (FR-055) |
| a failure applying any of the above on the network node | `500`, nothing changed (FR-008, FR-040) |

## Floating IPs (FR-027–FR-029, FR-031; R36)

| Request | Answer |
|---|---|
| ten creates on one network at once | ten `201`, ten distinct addresses |
| create on an external network with no IPv4 subnet | `400` |
| create when the pools are taken | `400 ExternalIpAddressExhausted` |
| create or associate with a port no router joins to the floating network — a port on a subnet without a gateway among them, which no interface joins | `404 ExternalGatewayForFloatingIPNotFound` |
| associate a fixed IP that already has a floating IP of the same network | `409 FloatingIPPortAlreadyAssociated` |
| a requested `fixed_ip_address` that is not the port's | `400` (FR-054) |
| `subnet_id`, `floating_ip_address` in a create | honoured: the address from that subnet, or that address if free (FR-054) |
| a requested `floating_ip_address` that is taken | `409 IpAddressAlreadyAllocated` (FR-054) |
| a `floating_ip_address` from a caller holding neither `admin` nor `manager` | `403 PolicyNotAuthorized` (`(rule:create_floatingip:floating_ip_address) is disallowed by policy`), nothing stored; `null` chooses none (FR-054) |

## Port forwardings (FR-049; R37)

Create, update and delete answer as today when the router namespace applies them; `500` and nothing changed when it
cannot. Deleting a floating IP removes its port forwardings' rules with it.

| Request | Answer |
|---|---|
| create on a floating IP no port uses | `201`; the router a floating IP on the internal port would go through is recorded on the floating IP |
| create on a floating IP a port uses | `409 FloatingIPPortAlreadyAssociated` |
| create to an internal port with a floating IP of its own | `409 PortHasBindingFloatingIP` |
| create to a port no router joins to the floating network | `400 BadRequest` ("External network … is not reachable from subnet …") |
| delete of the floating IP's last forwarding, or of its internal port | the floating IP is freed: no router, `DOWN` |
| `PUT /v2.0/floatingips/{id}` with a `port_id` on a floating IP its forwardings use | `409 FipInUseByPortForwarding`; a null `port_id` leaves it bound |

(SRC-20, as upstream's `pf_plugin`.)
