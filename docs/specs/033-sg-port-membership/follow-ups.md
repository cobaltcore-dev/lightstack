# Follow-ups — SPEC-033

Drafted as four tickets for the user to file (research R17). On 2026-11-19 the user moved all four into this feature
instead (spec.md, Clarifications, Session 2026-11-19), so none is to be filed. Each section says where it went.
Locations are at `d81f081`, the commit this feature is based on, unless they say otherwise.

---

## 1. The security-group listing's other filters

**Status.** In scope: User Story 8, FR-019a, FR-019b.

**Problem.** `GET /v2.0/security-groups` honours `id` and `name` since SPEC-033 (FR-019a). Every other attribute filter
is ignored, and the whole project is listed instead: `description`, `project_id`/`tenant_id`, `tags`, `shared`,
`stateful`, `revision_number`. Upstream applies them, so the divergence is silent.

**Proposal.** Honour the filters the data model can answer: `description`, and `project_id`/`tenant_id` within what the
caller may see. For the rest, register the divergence or refuse it. Refusing them all with `400`, as the server-list
filters do (register entry 4), would break Horizon, which sends `tenant_id` (research R13).

**Done when.** Each filter is honoured, registered or refused, and a test pins each one's answer.

---

## 2. Request paths that log a failed node-local step and answer success

**Status.** In scope: FR-033–FR-049 (spec.md, Session 2026-11-20). The user also decided to wire the router namespace
into the traffic path. In real mode it is on none today: the host end of a router interface's veth is on no bridge, so
port forwarding's DNAT, which goes into the namespace, is likely never reached. The list below is the input to that
clarification.

**Problem.** Constitution v1.6.0's Article II has a request either apply its node-local work or undo its change and
answer `500`. Its migration plan names the paths that still log a failure and answer success:
- `CreateAutoAllocatedTopology` (`auto_allocated_topology.go:119`);
- network teardown (`network.go:1167`, `:1185`);
- `CreateSubnet` (`network.go:1515`), and the subnet delete and gateway-change paths (`:1884`, `:1890`, `:1943`,
  `:1951`);
- `DeleteRouter` (`router.go:445`), `AddRouterInterface` (`:541`, `:558`), `RemoveRouterInterface` (`:626`), and gateway
  set and clear (`:692`, `:799`);
- port forwarding create, update and delete (`port_forwarding.go:220`, `:361`, `:383`, `:447`);
- `CreatePort`'s eBPF and FDB steps (`ports.go:235`, `:276`);
- `DeletePort` and `ReleaseInstancePorts` teardown (`ports.go:546`, `:1887`, `:1899`; `teardownPortHost` `:1919`,
  `:1928`, `:1934`, `:1938`). SPEC-033 disassociates their floating IPs in the deleting transaction and removes the NAT
  after it. A failed removal is logged and left to reconciliation, because the rest of the teardown has no undo;
- `AllocatePortForInstance`'s FDB and DHCP steps (`ports.go:1726`, `:1735`);
- the LS-SG chain writes in `DeleteSecurityGroup` (`:1066`, after the commit since SPEC-033), `CreateSecurityGroupRule`
  (`:1263`) and `DeleteSecurityGroupRule` (`:1462`). Nothing jumps to these chains, so an undone rule add can leave its
  rule in one, without effect;
- `DeleteFloatingIP` (`floatingip.go:573`) takes the floating IP's port forwardings with it through the foreign key
  (migration 050) and leaves their NAT.

Network creation's undo (`network.go:631-637`) also deletes the row unconditionally, where Article II asks for
compare-and-set.

**Proposal.** Bring each path in line with Article II as amended, or amend the constitution for it. For the LS-SG
chains, decide between removing them and wiring them.

**Done when.** No path in the list logs a failed node-local step and answers success, or the constitution names the ones
that may.

---

## 3. Deleting `default` as a non-administrator

**Status.** In scope: User Story 9, FR-024–FR-026. Renaming `default`, creating a second one, and duplicate names
answering `500` were found while checking it.

**Problem.** Upstream refuses to delete a project's `default` group with `409` (`SecurityGroupCannotRemoveDefault`),
even when no port carries it. Lightstack deletes it with `204` when no port carries it; since SPEC-033 a carried
`default` answers `409 SecurityGroupInUse`. `test/security_groups_test.sh` Test 11 already expects a refusal. Today it
skips itself, because the script reads `GROUPS`, a bash builtin (`baseline-gates.md`).

**Proposal.** Refuse it with `409 SecurityGroupCannotRemoveDefault` for a non-administrator, as upstream. Decide whether
an administrator may.

**Done when.** The refusal is tested, and Test 11 runs and passes.

---

## 4. Floating IPs beyond their dataplane

**Status.** In scope: User Stories 10 and 11, FR-027–FR-032a. Checking it found a fourth divergence: addresses ignore
allocation pools, which Lightstack does not store, and a network without a subnet hands out `192.0.2.x`.

**Problem.** Three divergences from upstream remain outside SPEC-033's scope:
- Two creates can race for one address. The address is chosen before the transaction, and the unique constraint turns
  the loser into a `500`.
- A port whose network has no router to the external network stores an association. Upstream answers `404`
  (`ExternalGatewayForFloatingIPNotFound`).
- Several floating IPs can reach one fixed IP. Upstream refuses that within one external network.

**Proposal.** Choose the address inside the create's transaction, or retry on the constraint. Refuse an association
without a route, and a second floating IP for one fixed IP, with upstream's answers.

**Done when.** Each case is refused or resolved as upstream, and tested on both backends.

---

## 5. Found while implementing round 2, outside its scope

**Status.** Not in scope; drafted for the user to decide (2026-11-23). Locations are at the working tree on the API
commit (`fix(neutron): answer the networking API as upstream does`, then `ba700dc`, rewritten since) with round 2
applied.

1. **VXLAN carries no broadcast** (blocks multi-node). A VXLAN device gets no flooding destination — no
   `00:00:00:00:00:00` forwarding entry per remote node and no multicast group (`pkg/networking/vxlan.go`,
   `createVXLANReal`) — so broadcast frames never cross nodes. Round 1 never needed it, every node serving DHCP and
   holding the gateway on its own bridge. With DHCP and the routers on the network node only, a guest on another node
   gets no lease and cannot resolve its gateway. Upstream's linuxbridge agent with l2population adds a flooding entry
   per remote VTEP that has a port on the network. *Proposal*: the coordinator appends one flooding entry per active
   remote node on each VXLAN network's device (`bridge fdb append 00:00:00:00:00:00 dst <vtep>`) and removes the entries
   of nodes no longer active, as it does the per-port ones.
2. **Forwarding rows name the serving node.** A port's row names the node that served its create (`RecordFDB`); the
   network's DHCP port and the routers' legs live on the network node, which the DHCP port's row does not name and the
   legs have none for. With flooding (1) the legs are learned from traffic; the DHCP port's row would still send its
   unicast to the wrong node.
3. **An interface attached on a network** creates its port in Nova's own transaction
   (`internal/nova/interface_attach.go`), so the network's DHCP reservation and the remote-group members' chains do not
   converge for it; and real mode does not hot-plug an attached interface (it predates this work). The port is bound and
   its own chains converge.
4. **Router ports can be deleted through the port API.** *Fixed by FR-065 (T184)*: `DELETE /v2.0/ports/{id}` on
   a router's interface port, or on the gateway port of a router that exists, answers `409 ServicePortInUse`,
   upstream's `prevent_l3_port_deletion`, administrators included (`internal/neutron/ports.go`, `DeletePort`).
5. **IPv6 subnets get no DHCP address.** The DHCP port takes an address on IPv4 subnets only; DHCPv6 is not served.
6. **Teardown leaves the VXLAN devices.** The purge has no VXLAN manager; the reconciler removes a deleted network's
   device, but `lightstack cleanup` does not remove the devices of the networks it purges.
7. **eBPF is not wired.** Nothing calls `pkg/networking/ebpf` since Neutron's TAP went; its wrapper in `pkg/networking`
   went with its last caller (SPEC-033 T221). Whether eBPF goes or is re-targeted at libvirt's TAP is the user's
   decision (`docs/EBPF_STATUS.md`).
8. **Glance unit tests depend on the shared database being clean.** `internal/glance`'s
   `TestTaskListAndGetAreOwnerScoped` and `TestImageMarkerPagingDoesNotDropTiedRows` fail after a run of the Glance
   contract tests, which leave pending tasks and backup images in the seeded project (`baseline-gates.md`, Phases 22 and
   24).

---

## 6. Found while fixing the branch review, outside its scope

**Status.** Not in scope; recorded for the user to decide (2026-11-24).

1. **A duplicate security group rule is stored.** Creating a rule its group already holds answers `201` and stores it
   again. Upstream answers `409 SecurityGroupRuleExists` ("Security group rule already exists. Rule id is <id>."), after
   `_validate_security_group_rule`, comparing direction, ethertype, protocol (by number, so `tcp` and `6` are one), the
   port range, the remote group and the remote prefix (`0.0.0.0/0`, `::/0` and none alike) —
   `_check_for_duplicate_rules`. The port firewall writes a duplicate once, so traffic is the same; the listings show
   both.
2. **Subnet update ignores `dns_nameservers`.** `PUT /v2.0/subnets/{id}` takes no `dns_nameservers` (`UpdateSubnet`'s
   request has `name`, `gateway_ip` and `allocation_pools` only), so a change answers as if made and stores nothing.
   Upstream updates them (`allow_put`) with the create's checks (FR-060). Doing it means converging the network's DHCP
   before the answer and undoing the change when that fails, as a gateway change does.
3. **A subnet overlapping another of its network is accepted.** `CreateSubnet` stores a CIDR that overlaps a subnet of
   the same network — the same CIDR twice, even. Upstream's `_validate_subnet_cidr` refuses it with `400`. Found while
   writing T172's test, which created eight subnets of one CIDR on one network before the fix.
4. **The auto-allocated topology's routes ignore their project.** `GET`, `POST` and `DELETE
   /v2.0/auto-allocated-topology/{project}` act on the caller's project whatever the path names
   (`auto_allocated_topology.go`), so an administrator's `DELETE` naming another project deletes the administrator's own
   topology. Upstream's take the project from the path, an administrator's reaching any project's. Older than this
   branch; found with SRC-13.
5. **`forceDelete` answers `204`.** Upstream's `POST /servers/{id}/action` `forceDelete` answers `202 Accepted`;
   Lightstack's `ForceDeleteInstanceAction` answers `204` (`internal/nova/handlers.go`). Older than this branch; found
   with SRC-27.
6. **Timestamps bound from the process clock are local wall time.** Handlers bind `time.Now()` into `TIMESTAMP` columns,
   which have no zone. On PostgreSQL the driver keeps the wall clock and drops the zone, so on a host outside UTC a
   port's `created_at` reads an hour off its `updated_at`, which the database's `now()` wrote: `2026-11-24T14:02:29Z`
   beside `13:02:29Z` under CET. Containers run in UTC, which hides it. There are about 200 such bindings across 59
   files. Binding `time.Now().UTC()`, or writing `now()` in the SQL, would fix it. Older than this branch; found with
   SRC-40.
7. **A subnet reports `tenant_id` without `project_id`.** Upstream's subnet carries both, as Lightstack's ports,
   floating IPs and address scopes do; the one subnet body (`renderSubnets`, `internal/neutron/network.go`) gives
   `tenant_id` alone. Older than this branch; found with SRC-47.
8. **Router namespaces route IPv4 only.** An interface's IPv6 addresses are on its leg (T315), but the namespace
   turns on `net.ipv4.ip_forward` alone (`pkg/networking/router_namespace.go`), sends no router advertisements —
   upstream runs radvd per router for its SLAAC subnets — and the gateway leg gets no IPv6 default route or proxy
   entry on the uplink. IPv6 routing is a feature of its own, with DHCPv6 (5.5): a new process in the namespace,
   its reconciliation, and an image dependency. Found with the pull-request review's T315.
