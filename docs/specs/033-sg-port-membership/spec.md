# Feature Specification: Server security-group membership reaches the ports

**Feature Branch**: `bugfix/579` (feature id: `033-sg-port-membership`)
**Created**: 2026-11-17
**Status**: Draft
**Input**: cobaltcore-dev/cloud-storage#579 — "lightstack — server security-group membership never reaches ports (create, os-add/removeSecurityGroup, os-interface)"

## Context

The compute API and the firewall read two different lists. A server's security groups are recorded per server. A port's are recorded per port, and the port's list is the one enforcement reads. Only server create writes both. So a server that reports `web` can be filtered by `default`, a removed group keeps admitting traffic, and an added group opens nothing. Not a regression. Paths and line numbers refer to `d81f081`, the `main` commit this branch is based on.

**Where each path stands.**

| Path | Server's list | Port's list |
|---|---|---|
| server create | recorded (`internal/nova/handlers.go:1039`) | the same groups, from a copy taken when the request was accepted (`handlers.go:1146` → `internal/neutron/ports.go:1639`); fixed by cloud-storage#524 |
| `addSecurityGroup` / `removeSecurityGroup` | changed (`internal/nova/advanced_actions.go:741`, `:829`) | untouched; `:824` and `:928` still say "In real mode, would … update Neutron port security groups" |
| `os-interface`, new port on a network | — | no group at all (`internal/nova/interface_attach.go:228`) |
| `os-interface`, existing port | — | left as it is (`interface_attach.go:72`) |
| `PUT /v2.0/ports/{id}` | — | cannot be changed (below) |

The same two compute handlers carry three more defects:

- `removeSecurityGroup` refuses a server's last group with `400 cannot remove last security group from instance` (`advanced_actions.go:897-913`). Upstream has no such guard. Terraform removes groups before it adds them, so replacing a server's only group fails at the first step.
- Both look the group up by name only (`:777`, `:865`). Server create accepts a name or an id, as upstream does, and `openstack server add security group` sends whatever the user typed.
- A server with no recorded group is reported as `[{"name": "default"}]` (`formatSecurityGroups`, `advanced_actions.go:729`, used at `handlers.go:1695` and `:1983`). Once the last group can be removed, that answer is invented.

**What enforcement reads.** The north-south floating-IP filter: replies to the server's own connections, one allowance per ingress rule of the port's groups, then drop (`portIngressRules`, `internal/neutron/floatingip.go:643`). It is built when a floating IP is associated and rebuilt per group when that group's rules change (`refreshIngressFiltersForSG`, `:691`). That rebuild finds ports through the port's list (`:698`), so it never finds a port with no group. Every node's reconciler re-asserts the filters from the database at startup and every 30 seconds (`internal/neutron/reconciler.go:85-140`). A firewall fixed by hand is gone within half a minute; the database row is the only thing that counts. The `default` group has egress rules only, so a port carrying only `default` admits replies through its floating IP and nothing else.

Every rebuild happens in place, one `iptables` call per step: flush the chain, append the replies rule and one rule per allowance, append the `DROP` last (`EnsureIngressFilter`, `pkg/networking/northsouth.go:157`). Until the `DROP` lands, a new inbound connection falls through to the host's `FORWARD` policy. Lightstack never sets that policy, and on a default host it is `ACCEPT`. The reconciler opens this window for every floating IP every 30 seconds, and a rebuild that fails part-way leaves the chain without its `DROP`.

**Floating IPs, rules and group deletion.** These answer success whatever the firewall did:
- `CreateFloatingIP` programs the NAT and the filter before it stores the row, and logs either failure (`floatingip.go:281-288`).
- `UpdateFloatingIP` answers `500` when the new NAT fails but only logs a failed filter (`:522-531`). It withdraws the old association best effort (`:447-450`).
- `DeleteFloatingIP` withdraws best effort (`:593-596`). `WithdrawFloatingIP` ignores every error (`pkg/networking/northsouth.go:124`).
- A rule added or deleted rebuilds the filters of the ports carrying its group and logs a failure (`refreshIngressFiltersForSG`, `ports.go:1291`, `:1487`).

The NAT is `DNAT` rules appended to `PREROUTING` and `OUTPUT` plus the floating address on the uplink (`EnsureFloatingIP`, `northsouth.go:95`). The reconciler re-asserts them, for every associated floating IP on every node, and removes nothing. A floating IP that is disassociated or deleted keeps its NAT and filter on every node that did not serve the change, and on the serving node when the withdrawal failed. An address allocated again later is then forwarded to the old fixed IP by the older rule, which matches first.

The `forceDelete` server action deletes only the instance row and its libvirt domain (`ForceDeleteInstanceAction`, `internal/nova/handlers.go:3720`). Its ports, their floating IPs and its volume attachments are left behind. Upstream's `force_delete` runs the same deletion as a delete (`nova/compute/api.py`: both call `_delete(…, self._do_delete, …)`), and Terraform sends it for `force_delete = true`.

Deleting a group deletes its port memberships with it (`DeleteSecurityGroup`, `ports.go:1059`), so the ports' lists change and no filter is rebuilt. It answers `204` for a group that does not exist. Upstream refuses a group any port carries with `409 SecurityGroupInUse` and a missing group with `404` (`neutron/db/securitygroups_db.py`, `delete_security_group`). Terraform's group resource retries the delete while it gets `409`.

The east-west eBPF filter never initialises in a stock build: Neutron passes an empty object path (`internal/neutron/network.go:41`, `docs/EBPF_STATUS.md`). Where it does initialise, only port creation through the Networking API programs it (`ports.go:224`). The `LS-SG-*` chains are not an enforcement path; nothing jumps an interface into them.

**The Networking API cannot change a port's groups.** `PUT /v2.0/ports/{id}` accepts a name, `admin_state_up`, `device_id` and `device_owner` (`ports.go:563`). A body carrying only `security_groups` answers `400 no updates provided` (`:600`); alongside another field it is silently ignored. Horizon's instance *Edit Security Groups* makes exactly this call, once per port of the instance (`openstack_dashboard/api/neutron.py`, `update_instance_security_group`). So do `openstack port set --security-group` and Terraform's port `security_group_ids`. Port creation treats `security_groups: []` like an absent field and attaches `default` (`ports.go:173-186`).

Nor can a client find a group. `GET /v2.0/security-groups` ignores every filter and returns all of the project's groups (`ListSecurityGroups`, `ports.go:961`). Horizon pre-selects an instance's groups in *Edit Security Groups* with `?id=`, using the ids on its ports, so the dialog shows every group as the instance's. Terraform's `openstack_networking_secgroup_v2` data source looks a group up with `?name=` and fails with "More than one Security Group found" in any project with two groups.

**Upstream** (nova master, read in source):

- Server create resolves names and ids in the server's own project, ids first (`nova/network/neutron.py`, `_process_security_groups`, `_get_security_group_ids`).
- `addSecurityGroup` and `removeSecurityGroup` resolve the group by name, in the caller's own project, or by id, any group an administrator can see, and rewrite the group list of **every port** of the server. A server with no port answers `404` (`nova/network/security_group_api.py`, `add_to_instance`, `remove_from_instance`). No last-group guard; a duplicate is Neutron's `400`.
- A server's `security_groups` are the groups of its ports, one entry per port and group. The key is omitted when there are none (`nova/api/openstack/compute/views/servers.py`, `_add_security_grps`; `security_group_api.get_instances_security_groups_bindings`).
- `os-interface` with a network creates the port without groups, so Neutron gives it the project's `default`. A port attached by id keeps its own (`nova/compute/manager.py`, `_attach_interface` → `allocate_for_instance` without groups → `_clean_security_groups(None)` is `[]`).

**History.** SPEC-009 FR-016a (`docs/specs/009-single-node-real-mode/spec.md:188`) promises floating-IP traffic "subject to the server's security groups". T034 (`tasks.md:129`) delivered "VM ports now get the project default SG". The archived sprint 56-57 gap analysis planned "INSERT to server_security_groups + Neutron port update" (`docs/archive/sprints/SPRINT_56-57_GAP_ANALYSIS.md:312-313`). The INSERT landed. Releases up to v0.6.1 gave VM ports no group and recorded none for the server; recording at create (`fbc03ba`) is in no release yet. Those servers report the invented `default` today.

## Clarifications

### Session 2026-11-17

- Decided on the ticket before this spec: removing a server's last group behaves as upstream — `202`, and the server and its ports are left with no group.
- Q: Is setting a port's groups through the Networking API in scope — `PUT /v2.0/ports/{id}` with `security_groups`, as Horizon's instance *Edit Security Groups*, `openstack port set --security-group` and Terraform's port `security_group_ids` send it? → A: In scope. The port API replaces a port's groups and rebuilds its filter, as the server actions do; the server's report follows its ports (User Story 6, FR-017–FR-019).
- Q: Which groups does a port attached to a running server get, and what does a server report? → A: As upstream. A new port gets the project's `default`; a port attached by id keeps its own groups; a server reports the groups of its ports, the key omitted when there are none; add and remove answer `404` for a server with no port (FR-003, FR-013, FR-014).
- Q: Should the security-group listing honour the filters Horizon's *Edit Security Groups* and Terraform's group data source send? → A: Yes: `id`, repeatable, and `name` (option B, which includes option A's `id`). Without `id` the dialog pre-selects every project group, and once the port API works, saving it unchanged puts them all on the instance's ports (FR-019a, User Story 6).
- Q: How is a floating-IP filter rebuilt, given the in-place rebuild leaves a moment without its `DROP`? → A: Atomically (option A). Each filter is replaced in one step, so it holds the new rule set or the old one, never neither, and a failed rebuild leaves the old filter whole. A reconciliation pass leaves a filter that already matches its stored list alone (FR-007a, FR-010, SC-010).
- Q: What happens when a filter rebuild fails after a membership change is stored? → A: Fail and undo (option C). The request answers `500` and its own change is undone, so the old filter, still whole, matches the stored lists again. This covers membership changes through both APIs. Floating-IP association and rule add/delete were left to a separate ticket here; the second analysis session brought them in (FR-008, SC-011).
- Q: For an administrator acting on another project's server or port, where is a group resolved? → A: In the project it applies to, names and ids alike (option A2), recorded as register entry 5. Both halves pass Article I's necessity test. Names: an administrator adding a tenant's group by name works today and would get `404` under upstream's rule. Ids: upstream's "any group an administrator can see" depends on administrators seeing every project's groups, and Lightstack's groups are private to their project, even for administrators. It is also how server create resolves groups, Lightstack's and upstream's. A1 (ids as upstream) was briefly chosen and withdrawn: it rested on reading only half of the necessity test, and it would have put groups on ports that their owner cannot list (FR-004, FR-018, FR-021a, SC-012).
- Q: Does FR-009, re-programming a port's eBPF rule set on every change, stay? → A: No, out of scope (option A). eBPF never initialises in a stock build. Ports Nova allocates get no eBPF program or rule set at all, so there is nothing to refresh. And the eBPF manager cannot be tested without a seam built for code no stock build runs. The ticket's acceptance criterion drops "/ eBPF rule set" (FR-009 withdrawn, Out of Scope).
- Q: Do the two requirements added without a question stay, and what does `security_groups: null` mean? The two are `POST /v2.0/ports` with `[]` giving no group, inferred from the port-API answer, and add/remove accepting an id. → A: Both stay (option A). `POST` with `[]` has today's `default`, which is the mismatch this feature removes, on the same field. Ids: the CLI sends whatever the user typed, the ticket's own bash test sends one, and create already accepts both. `null` means `[]` on port create and update, as upstream (Neutron's `convert_none_to_empty_list`), so on `PUT` it removes every group (FR-004, FR-017, FR-019).
- Q: How are the compute paths tested, now that a server's report is read from its ports? → A: Against the real network service in stub mode on the same test database, as `internal/nova/ip_allocation_concurrency_test.go` already does. The stand-in network service stays only to inject a failure, such as a rebuild that fails (option A; FR-022).

### Session 2026-11-17 (analysis)

- Q: Storing a change and undoing it when the firewall rebuild fails does not meet Article II's reservation exception, which requires a transitional row. Relying on reconciliation for other nodes, and for a node whose own undo failed, is outside the reconciliation exception's wording. Justify this in the plan, or amend? → A: **Amend.** Constitution v1.5.0, ratified 2026-11-17:
  - a new exception, "a change undone when its external work fails": no limit counted, a compare-and-set undo, a one-second bound on every external step and wait, and the single-transaction answer;
  - the reconciliation exception's wording aligned with the loop it was written for, which re-asserts node-local state from the record;
  - register entry 5 in the same amendment (FR-007b, FR-008, FR-010, FR-021a).
- Q: Undoing an add or remove by reversing its own rows can undo part of a port update made in between. → A: **Every undo is compare-and-set.** A port is put back only if it still carries the list this request left (FR-008).
- Q: Only the xtables lock wait was bounded. The firewall commands themselves, and the wait for another rebuild of the same address, were not. → A: **Each is bounded at one second**, and exceeding it is a failed rebuild (FR-007b).
- Q: FR-019a said the other listing filters "stay as they are today", that is, silently ignored: an unregistered silent divergence. → A: **The requirement no longer mandates it.** Other filters stay out of scope and go to the follow-up list (FR-019a, Out of Scope).

### Session 2026-11-18 (second analysis)

The user asked for the real fix of every finding, not a quick one. The answers below follow from that.
- Q: v1.5.0 requires a request that changes node-local state to apply it before answering, or fail and undo it. FR-008 exempted floating-IP association and rule changes, and this feature rewrites their filter code. Keep the exemption? → A: **No.** Floating-IP create on a port, association, move, disassociation and deletion, and rule add and delete, follow FR-008: in force on the serving node before the answer, or `500` and a compare-and-set undo. This reverses the part of the earlier failure answer that left them to a separate ticket (FR-008, FR-008a, FR-008b, User Story 7).
- Q: v1.5.0's first limit excluded any operation that creates or removes a record, yet a rule add and a floating-IP create do, and so does network creation already. → A: **The limit is restated, still in v1.5.0, which has not reached `main`.** A record whose node-local state is derived from it alone is complete without that work, so it may be created or removed under the exception. A resource whose external result cannot be derived again, such as a volume's backing, keeps the reservation exception. Network creation's unconditional undo goes to the follow-ups.
- Q: A disassociation or deletion leaves NAT and filters behind on the nodes that did not serve it, after a stopped process and after a failed undo, because the reconciler only adds. The amended loop exception promised those nodes converge. → A: **Reconciliation converges in both directions.** It removes floating-IP NAT and filters that no record holds. It removes only rules it can recognise as its own, so the NAT rules are marked as Lightstack's. It removes nothing on an incomplete read (FR-010).
- Q: Deleting a group in use drops it from every port with no filter rebuilt, against FR-007. → A: **Refuse it with `409`, as upstream,** and answer `404` for a missing group, as upstream. A group's deletion then never changes a port's list (FR-008c).
- Q: Other Neutron request paths also log a failed node-local step and answer success: networks, subnets and DHCP, routers, port forwarding, port creation's FDB and eBPF steps, port teardown, and the LS-SG chains. → A: **Recorded, not fixed here.** The constitution's migration plan names them as departures, and the follow-ups list each with its location (Out of Scope).

### Session 2026-11-18 (third analysis)

The user again asked for the real fix of every finding.
- Q: Deleting a floating IP disassociated it, applied that, and then deleted it in a second transaction, a shape neither of Article II's exceptions permits. → A: **One transaction deletes it, with the port forwardings that go with it.** If the node cannot withdraw it, both are restored. Rows that other requests deleted meanwhile are applied as the foreign keys would apply them: a deleted port leaves it unassociated, and a deleted network or an address allocated again keeps it deleted (FR-008).
- Q: Reconciliation removed a fixed IP's filter when no record and no marked rule reached it. Unmarked rules of earlier releases are kept, though, and can still forward to it. → A: **A filter stays while any rule of that shape reaches its fixed IP, marked or not.** Without the filter, that traffic would fall to the host's `FORWARD` policy (FR-008a, FR-010).
- Q: A real-mode request that cannot find the node's uplink skipped the work and answered success. → A: **It is a failed change, `500` and undone.** A reconciliation pass without an uplink logs an error and skips (FR-008).
- Q: Another constitution amendment numbered v1.5.0 reaches `main` first. → A: **This amendment becomes v1.6.0 at the rebase onto it, before review.** The task that does it no longer depends on a condition.

### Session 2026-11-18 (fourth analysis)

The user asked for the findings to be fixed; the answers follow the same rule as before.
- Q: Deleting a port or a server clears only `port_id` on its floating IPs, through the foreign key. Their fixed IP and status go stale, and their NAT stays until the next pass, while the freed fixed IP can already belong to another server. → A: **The deletion disassociates them in its own transaction, as upstream's port deletion does, and removes their NAT before answering.** A removal that fails cannot undo the deletion, whose port teardown has no undo. It is logged, and reconciliation completes it within one pass, the part that stays a recorded departure (FR-008d).
- Q: An unmarked rule counts as Lightstack's when its address has a record, but the pass read only associated floating IPs. → A: **A record is any floating IP's row, associated or not.** The pass reads them all, so an unassociated floating IP's leftover rules are removed (FR-010).

### Session 2026-11-18 (sixth analysis)

The fifth analysis changed no requirement. In the sixth, the user again asked for the findings to be fixed.
- Q: SC-014 says a floating IP whose server is deleted stops forwarding, but `forceDelete` deletes only the instance row, leaving its ports, their floating IPs and its volume attachments behind. → A: **`forceDelete` deletes a server exactly as `DELETE /v2.1/servers/{id}` does, as upstream's `force_delete` does (FR-008e).**

### Session 2026-11-19 (follow-ups brought into scope)

After implementation, the user reviewed the four follow-ups and moved each into this feature. Findings 1, 3 and 4 are specified below: User Stories 8–11, FR-019a, FR-019b and FR-024–FR-032a. Finding 2 and the router namespace are specified in the next session: FR-033–FR-049.
- Q: The security-group listing honours only `id` and `name` and silently ignores every other filter. → A: **Honour all of them.** `description`, and `project_id`/`tenant_id` narrowed within the caller's scope as the network listing does (another project → an empty list). `shared`, `stateful` and the tag filters answer from what is always true of a Lightstack group: never shared, always stateful, no tags. `revision_number`, which Lightstack does not keep, is refused with `400`.
- Q: About 25 real-mode request paths log a failed host step and answer success (constitution v1.5.0 migration plan). → A: **Bring all of them in line with Article II here.**
- Q: In real mode the router namespace, its interfaces, port forwarding's DNAT and the LS-SG chains are on no traffic path: the host end of a router interface's veth is on no bridge, and north-south runs through the host's own NAT. → A: **Wire the router namespace into the path, as upstream does**, so north-south traffic, floating IPs and port forwarding run through it. Its design is clarified before it is planned.
- Q: A non-administrator may delete or rename `default`, which silently resets or duplicates it; a group named `default` answers `500`. → A: **Upstream's rules.** Deleting `default` → `409 SecurityGroupCannotRemoveDefault` for a non-administrator; an administrator may delete it when unused. Renaming `default` → `409 SecurityGroupCannotUpdateDefault`. `default`, in any case, as a new or changed name → `409 SecurityGroupDefaultAlreadyExists`. Any other duplicate name answers `409` instead of `500`. `test/security_groups_test.sh` stops using the `GROUPS` builtin, so its Test 11 runs.
- Q: Floating-IP creates race for the lowest free address (`500`); an association needs no matching router (create `201`, update `400`); several floating IPs may reach one fixed IP; addresses ignore allocation pools and fall back to `192.0.2.0/24`. → A: **Fix all four.** The address is chosen inside the create's transaction, after the network lock. Create and update answer `404 ExternalGatewayForFloatingIPNotFound` unless a router joins the port's subnet to the floating IP's network, preferring the one whose interface is the subnet's gateway. A second floating IP on a fixed IP within one external network answers `409 FloatingIPPortAlreadyAssociated`. Addresses come from the allocation pools of the external network's subnets, and a network without a subnet is refused instead of handing out `192.0.2.x`.
- Q: Lightstack keeps no allocation pools. A subnet's requested `allocation_pools` is ignored, and responses show one computed from the CIDR, `.2` to the last usable address; floating IPs start at `.100` as a guess at what is free. On an external network that is the operator's LAN, the computed pool would hand out addresses other machines hold. → A: **Store the pools.** Subnet create and update honour and validate `allocation_pools` as upstream; an absent field gets the computed default. Every address Lightstack allocates on a subnet without a requested address comes from its pools. Existing subnets of external networks keep today's floating range, `.100` up, as their pool; other subnets get the default (FR-030–FR-032).
- Q: Answering `409` for a duplicate group name keeps names unique per project, where upstream allows duplicates and reserves only `default`. → A: **Recorded as register entry 6.** Name resolution in both APIs relies on a name naming one group (FR-026).

### Session 2026-11-20 (router namespace and Article II paths)

Clarifies finding 2 and the router namespace, brought into scope on 2026-11-19.
- Q: Where do router namespaces live? Today every node puts the gateway address on its own bridge and every floating address on its own uplink, so with several nodes each address appears several times. → A: **One network node hosts every router namespace (option A)**, as upstream's default L3 layout does. It is the only node that holds router and floating addresses; other nodes reach it over VXLAN. It is set in configuration and is the node itself on a single node. Spreading routers across nodes and a standby router stay possible later; distributed routing was considered and not taken: per-node external legs, the gateway's MAC on every node, and floating IPs following their VM (FR-033).
- Q: Every node serves the API on the shared database, and no node calls another. How does a change served by a node other than the network node reach it before the answer? → A: **The serving node asks the network node to apply it (option A)**: an authenticated node-to-node call, bounded at one second like every host step, returns the network node's result. A failure there is the request's failure: `500` and the compare-and-set undo. On the network node itself the call is local. Forwarding whole requests, and storing then converging within a pass as a recorded departure, were not taken (FR-034).
- Q: How does a router namespace reach the outside, given that the usual deployment is a Kubernetes cluster whose node network belongs to the kubelet and the CNI? → A: **A routed veth (option A).** One veth pair per router, `qg-` in the namespace; the network node answers ARP on its uplink for the router's and its floating addresses and routes them to the veth. The node's own card and addresses are left alone. An external bridge with the node's card enslaved can take a Kubernetes node `NotReady`; macvlan adds MACs many underlays drop; a dedicated external card cannot be counted on (FR-035).
- Q: A namespace lives through its bind mount under `/run/netns`, and the chart mounts no host `/run/netns`, so a pod restart would destroy every router namespace and cut north-south traffic until rebuilt. → A: **Namespaces belong to the host (option A).** The chart and the Compose file mount the host's `/run/netns` with bidirectional propagation; namespaces survive a restart of Lightstack, the reconciler re-asserts them on start, and teardown removes them. Rebuilding on every start was not taken: it cuts traffic and established connections on each restart (FR-036).
- Q: Once a router holds a subnet's gateway address, what becomes of the bridge's address, which `dnsmasq` binds to and the metadata redirect hangs off, and which every node's bridge carries today with a `dnsmasq` per node? → A: **Upstream's shape (option A).** The router's `qr-` veth joins the network's bridge on the network node and holds the gateway address. DHCP gets an address of its own from the pools, reported as a `network:dhcp` port; one `dnsmasq` per subnet (corrected during planning to one per network, as upstream's agent runs it; FR-037) runs on the network node, bound to it, and metadata is reached through a static route to that address, as gatewayless subnets already do. Bridges hold no address (FR-037).
- Q: Upstream gives each router gateway an address of the external network, reported in `external_gateway_info.external_fixed_ips`, and SNATs its subnets to it; Lightstack masquerades to the host's uplink address and reports none. Which? → A: **Upstream's gateway address (option A).** One address per router from the external network's pools, or the one requested, held on `qg-`, reported as a `network:router_gateway` port and in `external_fixed_ips`, and the SNAT source of the router's subnets. Double NAT to the host's address was not taken: it leaves `external_fixed_ips` absent, a divergence Terraform and Horizon read (FR-038).
- Q: Removing the router interface or clearing the gateway that a floating IP's association goes through leaves it with a stale router and, once the namespace carries the traffic, no path. → A: **Upstream's refusals (option A):** `409` with upstream's error, nothing changed. Disassociating the floating IPs instead was not taken (FR-039).
- Q: Deletions that tear down host state (networks, subnets with their DHCP, routers, ports, port forwardings, a cleared gateway's SNAT) delete the record, try the teardown, log a failure and answer `204`. How do they meet Article II? → A: **Host first, record after (option A).** The deletion checks its refusals in a short transaction, removes the host state under the node lock, on the network node through FR-034, then deletes the record in a transaction that re-checks them. A failed removal, or a final transaction that refuses or fails, answers `500` with the record kept, and what was removed is applied again from it. This replaces FR-008d's recorded departure for port teardown. Restoring deleted rows, and letting reconciliation complete a removal after a success answer, were not taken (FR-040).
- Q: The `LS-SG-*` chains are filled and never jumped to; security groups are enforced only through floating IPs, ingress only, so traffic between VMs and every egress rule go unfiltered. → A: **Wire them in as a firewall driver (option B)**, as upstream's iptables driver does: every port on every node filtered in both directions, through `physdev` jumps per TAP into per-port chains built from the port's groups, remote groups resolved, applied under Article II on the port's node (FR-041).
- Q: With every port firewalled, what becomes of the per-fixed-IP floating-IP filter, a stand-in for the missing port firewall that covers only ingress through a floating IP? → A: **Retire it (option A).** The port firewall enforces floating-IP traffic as any other, as upstream, which has no such filter; its mechanics (atomic replacement, compare-and-set undo, bounds, two-way reconciliation) carry over to the port's chains. No API or CLI answer changes. Keeping both, as defence in depth, was not taken: every change would apply, undo and reconcile in two places (FR-042).
- Q: With egress and traffic between VMs enforced, a group created through Lightstack has no rule at all, and `default` has no rule admitting its own members, both unlike upstream; every group that exists today lacks them. → A: **Upstream's rules, on upgrade too (option A).** New groups get egress allow-all for IPv4 and IPv6; `default` also admits ingress from its own members. A one-time migration adds egress allow-all to every existing group with no egress rule, and the member rule to every existing `default`. Traffic between VMs in other groups needs a rule, as upstream (FR-043).
- Q: How does an existing deployment move onto the new layout? The user noted that no Lightstack deployment exists yet. → A: **A clean break (option A).** The reconciler builds the new layout from the records and removes nothing of the old beyond what it already recognises; `docs/UPGRADING.md` says to reset a host's networking with teardown before running this release. This also drops two upgrade-only provisions settled earlier: existing external subnets keeping the `.100` floating range as their pool (the 2026-11-19 answer on pools), and FR-043's one-time migration of existing groups' rules. Full automatic conversion was not taken: it would only ever run on development hosts (FR-032a, FR-043, FR-044).
- Q: The port chains see bridged traffic only with `br_netfilter` loaded and `net.bridge.bridge-nf-call-iptables`/`-ip6tables` set, host-wide settings that are usually on for Kubernetes nodes and often off on a bare host. Who ensures them? → A: **Lightstack, and it refuses to be ready otherwise (option A).** In real mode it loads the module and sets both at start; if it cannot, readiness fails naming the reason. Leaving them to the operator, and best effort, were not taken (FR-046).
- Q: eBPF never loads in a stock build, FR-009 took it out of this feature, and the iptables port firewall now covers every port. What of `networking_mode: ebpf`? → A: **eBPF stays out of scope; the port firewall runs in both modes (option A).** The user will clarify separately whether eBPF is still needed; if not, it is removed entirely in its own change (FR-047).
- Q: How is the new layout verified on real hosts, given that multi-node and the Kubernetes deployment are now part of it? → A: **Two runs (option A):** a single-node run of the chart on Kubernetes, and a two-node run with a shared PostgreSQL and VXLAN for the network node and the node-to-node apply (FR-048).

### Session 2026-11-20 (round-2 analysis)

The user asked for every finding to be fixed, as near to upstream as possible, the constitution changed where it is out of date, and any finding still open brought to them.
- Q: `?revision_number` was refused because Lightstack keeps no revision: an unregistered divergence. → A: **Keep upstream's revision number** on security groups, starting at 0 and going up by one on every update of the group and every rule added to or deleted from it, as upstream's revision plugin does; report it and honour the filter (FR-019a, FR-050).
- Q: A create or update with `"stateful": false` was unspecified; Terraform's group resource sends it. → A: **Support stateless groups, as upstream**: stored, reported, changeable only while no port carries the group (`409 SecurityGroupInUse`), never mixed with stateful groups on one port (`409 SecurityGroupConflict`), and enforced with `CT --notrack` instead of a zone, without the connection-tracking rules (FR-051).
- Q: Port creation's eBPF step logs and continues, and attaches to the Neutron TAP that FR-041 removes. → A: **The step goes with that TAP.** eBPF stays out of scope otherwise; the user decides its future separately (FR-045, FR-047).
- Q: Several answers existed only in tasks, with no requirement: a subnet in use, a requested network or broadcast address, a requested fixed IP that is not the port's, a taken floating address, a duplicate or gatewayless router interface, `binding:host_id`. → A: **Each becomes a requirement, as upstream** (FR-052–FR-056), and FR-020 lists every round-2 answer.
- Q: A port whose binding moves or is cleared keeps chains on its old node; at boot nothing orders the chains before the guest starts. → A: **The old node's chains go in the same request, and a booting port's chains are in force on its node before the domain starts; a failure fails the boot as a libvirt failure does** (FR-041).
- Q: An administrator's `?project_id=<another project>` lists nothing, where upstream lists that project's groups (finding D2); every Neutron listing scopes administrators to their own project, and register entry 5 cited that scoping. → A: **An administrator's explicit project filter lists that project's groups, as upstream (option A).** Entry 5 is restated on its own ground: a port never carries a group its owner cannot list or show. The unfiltered administrator listing is the next question (FR-019a).
- Q: An administrator can now list and show another project's networking resources, but writing them answers `404`, where upstream allows administrators to write any project's resources. → A: **Upstream (option A)**: administrators update, delete and act on any project's resource by id, and reference it in creates; names stay own-project; register entry 5 is unchanged (FR-058).
- Q: Every Networking API listing shows an administrator only their own project's resources (networks also shared and external), where upstream shows every project's; registering it is impossible, since a client cannot detect what is missing (Article I, limit 4). → A: **Fix every Networking API listing as upstream (option 2)**: an administrator's listing shows every project's resources and the project filters narrow it; a show by id reaches any project's resource. A resource named rather than given by id still resolves only in the caller's project (FR-057, User Story 12).

### Session 2026-11-20 (second round-2 analysis)

The user's rule stands: fix every finding, as near to upstream as possible; change the constitution where it is out of date; bring open questions to the user.
- Q: An undone rule change left the group's revision two higher, though the request answered "nothing changed". → A: **The undo puts the revision back, compare-and-set**, as upstream, where a failed change is rolled back with its transaction (FR-050).
- Q: Upstream honours `If-Match: revision_number=<n>` with `412`; once revisions are reported, ignoring it is silent. → A: **Honour it on security groups**, the only resource carrying a revision, as upstream's revision-if-match does (FR-050).
- Q: A stateful/stateless mix at server create: the contract said `409`, the test `ERROR`. → A: **`ERROR` with a fault**, as upstream's build fails when Neutron refuses the port; `addSecurityGroup` answers `409` as upstream Nova's `SecurityGroupConnectionStateConflict`; the check is per port, so an interface attach is never refused for it (FR-051).
- Q: Pre-076 networks have no connection-tracking zone and existing ports no binding. → A: **A zone is assigned when first needed; the clean break removes guests**, which are recreated (FR-044).
- Q: Register entry 5's id half rested on administrators being unable to see other projects' groups; FR-057 removed that ground, and "a port never carries a group its owner cannot list" is not a ground limit 1 accepts. → A: **Ids follow upstream (option A)**: an administrator may name any project's group by id, in server create, `addSecurityGroup`/`removeSecurityGroup` and port create and update. Entry 5 keeps only the names half (FR-004, FR-018, FR-021a).

### Session 2026-11-20 (third round-2 analysis)

- Q: A stateful/stateless mix was refused on interface attach, but upstream checks per port against that port's own groups. → A: **Per port, as upstream**; attach is never refused for it (FR-051).
- Q: Register entry 6 (unique group names) rested on a schema rebuild the migration rules call effort, and on callers that do not exist (a duplicate answers `500` today); neither is a ground Article I's limit 1 accepts. → A: **Follow upstream (option A)**: names may repeat, only `default` is reserved; an ambiguous name answers `409` (`NoUniqueMatch`) where a server action resolves it and `404` in the group's path; migration 077 drops the constraint, its SQLite rebuild running outside a transaction through a migrator change; entry 6 is withdrawn (FR-026).

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Adding or removing a group changes what reaches the server (Priority: P1)

A tenant runs a server with a floating IP. They add `web`, a group that admits tcp/80. A new connection to port 80 gets through. No restart, nothing done by hand. They remove `web`, and new connections to port 80 are dropped again. At every step the server reports the groups its ports carry.

**Why this priority**: It is the defect. The API reports one set and the firewall enforces another, and in Kubernetes the reconciler re-asserts the wrong one every 30 seconds.

**Independent Test**: Stub mode: add and remove a group on a server that has a port; `GET /v2.0/ports?device_id=<server>` and the server's detail follow each change. Real mode: tcp/80 through the floating IP before and after the add and the remove, with no restart.

**Acceptance Scenarios**:

1. **Given** a server with one port and only `default`, **When** `web` is added, **Then** the answer is `202`, the port lists `default` and `web`, and the server reports both.
2. **Given** that server with a floating IP in real mode, **When** `web` is added, **Then** a new inbound connection matching a `web` rule succeeds, with no restart.
3. **Given** a server in `default` and `web`, **When** `web` is removed, **Then** the answer is `202`, the port lists only `default`, and a new inbound connection only `web` admitted is dropped, with no restart.
4. **Given** a server with two ports, **When** a group is added or removed, **Then** both ports change.
5. **Given** the group is named by its id, **When** it is added or removed, **Then** the outcome is the same as by name.
6. **Given** a server with no port, **When** a group is added or removed, **Then** the answer is `404`, as upstream, and nothing changes.
7. **Given** a change made through the API, **When** the next reconciliation passes run, **Then** they re-assert the new set, never the old one.
8. **Given** a real-mode host where the port's filter cannot be rebuilt, **When** a group is added or removed, through either API, **Then** the answer is `500`, the port's list is unchanged, and the server still reports its old groups.

---

### User Story 2 — Removing the last group closes the server, as upstream (Priority: P1)

A tenant removes the only group a server has. They do it directly, or Terraform does it while replacing `["a"]` with `["b"]`, because it removes before it adds. Today the answer is `400` and the apply stops. Afterwards it is `202`. The server's ports have no group, inbound traffic through the floating IP stops except replies to the server's own connections, and the server no longer claims to be in `default`.

**Why this priority**: The guard has no upstream counterpart and breaks a routine Terraform change. Decided on the ticket. A server with no group admits nothing new, which is the safe direction.

**Independent Test**: API: remove a server's only group; `202`, its ports list no group, the server reports none. Real mode: the floating IP stops admitting new connections, with no restart.

**Acceptance Scenarios**:

1. **Given** a server whose only group is `web`, **When** `web` is removed, **Then** the answer is `202`, every port lists no group, and the server's detail omits `security_groups`, as upstream.
2. **Given** that server with a floating IP in real mode, **When** `web` is removed, **Then** a new inbound connection is dropped, with no restart; replies to connections the server opened still arrive.
3. **Given** a Terraform configuration that changes a server's only group from `a` to `b`, **When** it is applied, **Then** the apply succeeds and every port ends with `b` only.
4. **Given** a server with no group, **When** a group is added, **Then** its ports carry it and the server reports it.

---

### User Story 3 — An interface attached later is filtered as upstream (Priority: P2)

A tenant attaches another interface to a running server: a new port on a network (`openstack server add network`) or an existing port (`openstack server add port`). Today a new port gets no group at all. Afterwards a new port gets the project's `default`, as upstream, and an existing port keeps the groups it has. The server reports the groups of all its ports, the new one included.

**Why this priority**: Rarer than add and remove, but it is the third compute path that writes a port.

**Independent Test**: Stub mode: attach an interface on a network, and attach an existing port, to a server in `web`; read each port and the server.

**Acceptance Scenarios**:

1. **Given** a server in `web`, **When** an interface is attached on a network, **Then** the new port lists `default` only, and the server reports `web` and `default`.
2. **Given** an existing port in `db`, **When** it is attached to that server, **Then** it still lists `db`, and the server reports `db` among its groups.
3. **Given** that server, **When** `web` is removed, **Then** the answer is `202` and `web` comes off the port that carries it; the other ports are untouched.
4. **Given** that server, **When** the attached port is detached, **Then** the server no longer reports `db`, and the port keeps it.

---

### User Story 4 — Create and boot keep their behaviour (Priority: P2)

A server created with `web` has `web` on its port, not `default`. A server created with no group has `default` only. Both are on `main` already and become pinned by tests. New: a group change made while a multi-network server is still booting reaches the ports the boot creates after it.

**Why this priority**: The create path is fixed; this keeps it fixed and closes the race.

**Independent Test**: Stub mode: create with and without groups and read the port; change the groups between the first and second port of a two-network boot and read both.

**Acceptance Scenarios**:

1. **Given** `POST /servers` with `security_groups: [{"name": "web"}]` and a network, **When** the boot creates the port, **Then** `GET /v2.0/ports?device_id=<server>` lists `web` and not `default`.
2. **Given** `POST /servers` with no `security_groups`, **When** the boot creates the port, **Then** the port lists `default` only and the server reports `default`.
3. **Given** a server booting on two networks, **When** a group is added after its first port exists and before its second, **Then** both ports carry it.
4. **Given** a server still booting with no port yet, **When** a group is added, **Then** the answer is `404`, as upstream.

---

### User Story 5 — Servers from earlier releases report the truth (Priority: P3)

A server created by v0.6.1 or earlier has ports with no group and no recorded group. Today it reports `default`. Afterwards it reports no group, which is what its ports carry, and a group added to it reaches its ports.

**Why this priority**: Visible on upgrade, but it is a report becoming true, not a change to what the dataplane does.

**Independent Test**: A server and ports shaped like v0.6.1's: the server reports no group; add a group, and the ports carry it.

**Acceptance Scenarios**:

1. **Given** a server whose ports carry no group, **When** it is read, **Then** its detail omits `security_groups`.
2. **Given** that server, **When** a group is added, **Then** its ports carry it and the server reports it.

---

### User Story 6 — The Networking API and Horizon change a port's groups (Priority: P2)

A tenant opens an instance in Horizon and picks its groups under *Edit Security Groups*. Horizon sets that list on every port of the instance through the Networking API. Today each call answers `400` and nothing changes. Afterwards each port carries the chosen groups, its filter follows, and the server reports them. The same holds for `openstack port set --security-group` and `--no-security-group`, and for Terraform's port `security_group_ids`.

**Why this priority**: It is the dashboard's only way to change an instance's groups, and Terraform's port resource depends on it. Clarified into scope.

**Independent Test**: Contract: `PUT /v2.0/ports/{id}` with `security_groups`; the port and its server report the new list; `[]` leaves none. Horizon: *Edit Security Groups* on an instance with one port.

**Acceptance Scenarios**:

1. **Given** a port in `default`, **When** `PUT /v2.0/ports/{id}` sets `security_groups` to `[web]`, **Then** the answer is `200`, the port lists `web` only, and in real mode its floating-IP filter follows, with no restart.
2. **Given** that port, **When** `security_groups` is set to `[]` or `null`, **Then** the port has no group and its floating IP admits only replies.
3. **Given** a body with `name` and `security_groups`, **When** it is sent, **Then** both apply.
4. **Given** a body without `security_groups`, **When** it is sent, **Then** the port's groups are unchanged.
5. **Given** an id that names no group of the port's project, **When** it is sent, **Then** the answer is `404` and nothing changes. A repeated id or a malformed one answers `400`.
6. **Given** an instance with one port, **When** its groups are saved in Horizon's *Edit Security Groups*, **Then** the port carries the chosen groups and the server reports them.
7. **Given** `POST /v2.0/ports` with `security_groups: []` or `null`, **When** the port is created, **Then** it has no group, as upstream.
8. **Given** a project with `default`, `web` and `db`, and an instance whose port carries only `web`, **When** *Edit Security Groups* opens, **Then** only `web` is pre-selected, and saving without changes leaves the port with `web` only.
9. **Given** that project, **When** `GET /v2.0/security-groups?name=web` is sent, **Then** only `web` is listed, so Terraform's `openstack_networking_secgroup_v2` data source finds it.

---

### User Story 7 — Floating-IP and rule changes are in force when the API answers (Priority: P2)

A tenant associates a floating IP with a server's port, adds a rule to `web`, then moves the floating IP to another server and deletes it. Today each answer is a success whatever the firewall did, and a deleted floating IP keeps forwarding on every node that did not serve the deletion. Afterwards each change is in force on the serving node when the API answers, or the answer is `500` and nothing changed. Every node follows within a reconciliation pass, removals included. A group that a port carries cannot be deleted, as upstream.

**Why this priority**: The same defect as User Story 1, a success answer over a firewall that did not change, on the paths that build the same filter. It is not the ticket's headline.

**Independent Test**: Stub mode with a recording dataplane. Associate, move, disassociate and delete a floating IP, add and delete a rule, and delete a port or a server that has a floating IP: the dataplane follows before each answer. A failing dataplane gives `500` with the records unchanged. A reconciliation pass removes NAT and filters that no record holds. Deleting a group in use gives `409`. Real mode: quickstart §5 (i).

**Acceptance Scenarios**:

1. **Given** a port in `web` with a floating IP, in real mode, **When** a rule admitting tcp/8080 is added to `web`, **Then** the answer is `201` and a new connection to 8080 succeeds, with no restart. **When** the rule is deleted, **Then** the answer is `204` and new connections to 8080 are dropped.
2. **Given** a floating IP, **When** it is associated with a port, on create or on update, **Then** before the answer its NAT and address are in place in its router's namespace, and its traffic reaches the port only through the port's chains (FR-008a, FR-041).
3. **Given** an associated floating IP, **When** it is moved to another port, **Then** before the answer its NAT points at the new port only (FR-008a).
4. **Given** an associated floating IP, **When** it is disassociated or deleted, **Then** before the answer the network node forwards no traffic to it: its address and NAT are gone.
5. **Given** a real-mode host where the NAT or the filter cannot be applied, **When** a floating IP is created on a port, associated, moved, disassociated or deleted, or a rule is added or deleted, **Then** the answer is `500` and the records are as they were before the request.
6. **Given** a floating IP deleted through one node, **When** every other node has run its next reconciliation pass, **Then** none of them forwards traffic to it.
7. **Given** a group that a port carries, **When** it is deleted, **Then** the answer is `409` and the port keeps the group. **Given** an id that names no group of the caller's project, **When** it is deleted, **Then** the answer is `404`.
8. **Given** a server whose port has a floating IP, **When** the server is deleted or force-deleted, or the port is deleted, **Then** before the answer the floating IP has no port, fixed IP or router, its status is `DOWN`, and the node no longer forwards it.

---

### User Story 8 — A client lists groups by any attribute (Priority: P3)

A tool filters the group listing: Terraform's `openstack_networking_secgroup_v2` data source by `description`, `tags` or `stateful`, an administrator with `openstack security group list --project <id>`. Today every filter but `id` and `name` is ignored and the whole project comes back. Afterwards each filter applies as upstream's does.

**Why this priority**: A silent wrong answer. Nothing leaks: a member's listing stays scoped to their project; an administrator's covers every project (FR-057).

**Independent Test**: Contract: each filter against a project whose groups differ in it.

**Acceptance Scenarios**:

1. **Given** groups `web`, described `http`, and `db`, **When** the listing is filtered with `?description=http`, **Then** only `web` is listed.
2. **Given** a caller in project A, **When** the listing is filtered with `?project_id=A` or `?tenant_id=A`, **Then** all of A's groups are listed. **Given** an administrator, **When** it is filtered with project B's id, **Then** B's groups are listed, as upstream; for a non-administrator, another project's id lists nothing, as upstream.
3. **When** the listing is filtered with `?shared=false`, **Then** every group is listed; with `?shared=true`, none. With `?stateful=false`, only stateless groups; with `?stateful=true`, only stateful ones.
4. **When** it is filtered with `?tags=x` or `?tags-any=x`, **Then** none is listed; with `?not-tags=x` or `?not-tags-any=x`, every group.
5. **When** it is filtered with `?revision_number=1`, **Then** only groups at revision 1 are listed. A boolean filter that is not a boolean answers `400`.
6. **When** a filter repeats, as in `?description=a&description=b`, **Then** groups matching any of its values are listed.
7. **When** a group is listed or shown, **Then** it reports its `stateful` and `revision_number`, `shared: false` and `tags: []`.

---

### User Story 9 — `default` stays the project's default (Priority: P2)

A tenant cannot delete or rename their project's `default` group, or make a second one, as upstream. Today deleting it answers `204`, and the next server create quietly makes a new `default` with only the seed rules, dropping every rule the tenant added. Renaming it leaves the next create making a fresh `default` beside it. Creating a group named `default` answers `500`.

**Why this priority**: A silent loss of the tenant's own firewall rules, through a call upstream refuses.

**Independent Test**: Contract and unit: delete, rename and create against `default` as a tenant and as an administrator. `test/security_groups_test.sh` Test 11.

**Acceptance Scenarios**:

1. **Given** a project whose `default` no port carries, **When** a non-administrator deletes it, **Then** the answer is `409` (`SecurityGroupCannotRemoveDefault`) and it stays, with its rules.
2. **When** an administrator deletes it, **Then** the answer is `204`, as upstream, and the next server or port create in the project makes a new `default`.
3. **When** `default` is updated with a `name`, **Then** the answer is `409`: `SecurityGroupCannotUpdateDefault`, or `SecurityGroupDefaultAlreadyExists` when the name is `default`. Its description can still change.
4. **When** a group is created as `default`, in any case, or another group is renamed to it, **Then** the answer is `409` (`SecurityGroupDefaultAlreadyExists`) and nothing changes.
5. **Given** a group `web`, **When** a second `web` is created, or another group is renamed to `web`, **Then** it succeeds, as upstream. **When** `web` is then added to a server by name, **Then** the answer is `409` (`NoUniqueMatch`) and nothing changes; by id it succeeds.
6. **When** `test/security_groups_test.sh` runs, **Then** its Test 11 runs and passes.

---

### User Story 10 — Floating IPs are allocated and associated as upstream (Priority: P2)

Terraform creates several floating IPs at once and associates them with servers' ports. Today creates sent together take the same lowest free address and all but one answer `500`. An association with a port no router reaches is accepted on create and refused with a `400` of Lightstack's own on update. One fixed IP can get several floating IPs. Afterwards creates sent together each get an address, and associations follow upstream's rules.

**Why this priority**: A parallel `terraform apply` with two or more floating IPs fails today. Terraform compatibility comes first.

**Independent Test**: Unit, both backends: concurrent creates; associations with and without a joining router; a second floating IP on one fixed IP.

**Acceptance Scenarios**:

1. **Given** ten floating-IP creates on one external network sent at once, **When** they complete, **Then** all answer `201` with ten distinct addresses.
2. **Given** a port on a subnet that no router joins to the floating IP's network, **When** a floating IP is created on it or associated with it, **Then** the answer is `404` (`ExternalGatewayForFloatingIPNotFound`) and nothing is stored.
3. **Given** a router that joins the port's subnet to another external network, **When** a floating IP of this network is associated with the port, **Then** the answer is `404` too.
4. **Given** two routers that join the subnet to the network, **When** a floating IP is associated, **Then** its router is the one whose interface holds the subnet's gateway address, as upstream.
5. **Given** a fixed IP with a floating IP of network N, **When** a second floating IP of N is associated with it, **Then** the answer is `409` (`FloatingIPPortAlreadyAssociated`) and nothing changes. One of another external network is accepted.
6. **Given** an external network with no IPv4 subnet, **When** a floating IP is created on it, **Then** the answer is `400` and nothing is stored.
7. **Given** an external network whose pools are all taken, **When** a floating IP is created, **Then** the answer is `400` (`ExternalIpAddressExhausted`).

---

### User Story 11 — Operators choose the floating range with allocation pools (Priority: P2)

An operator creates the external network's subnet on their LAN, with `allocation_pools` set to the range reserved for floating IPs. Today the field is ignored, the response shows a pool covering the whole subnet, and floating IPs start at `.100` whatever was asked, possibly on another machine's address. Afterwards the subnet keeps its pools, reports them, and every address Lightstack picks on it comes from them.

**Why this priority**: The documented way to keep floating IPs off addresses the LAN uses. Clarified into scope with User Story 10.

**Independent Test**: Unit and contract, both backends: subnet create and update with pools, refusals, and the addresses floating IPs and ports get.

**Acceptance Scenarios**:

1. **Given** a subnet created with pools `.200`–`.220`, **When** it is read, **Then** it reports them, and floating IPs on its network come only from `.200`–`.220`.
2. **Given** a subnet created without pools, **When** it is read, **Then** it reports the default pool, `.2` to the last usable address, as today.
3. **Given** a pool reaching outside the subnet, **Then** the answer is `400` (`OutOfBoundsAllocationPool`). A malformed pool, or one ending before it starts, answers `400` (`InvalidAllocationPool`). Overlapping pools answer `409` (`OverlappingAllocationPools`). A pool holding the gateway address, through the pools or a gateway change, answers `409` (`GatewayConflictWithAllocationPools`).
4. **Given** a subnet whose pools are updated, **When** addresses are allocated afterwards, **Then** they come from the new pools. Addresses allocated before stay.
5. **Given** a port created without a requested address on a subnet with pools, **Then** its fixed IP is from the pools. A requested address inside the subnet but outside the pools is accepted, as upstream.
6. **Given** a subnet that existed before the upgrade, **When** it is read, **Then** it reports the default pool.

---

### User Story 12 — An administrator sees and manages every project's networking resources (Priority: P3)

An administrator opens Horizon's admin panels, or runs `openstack network list` or `openstack port list --project <id>`. Today they see only their own project's resources, with nothing to say others exist, and cannot change another project's. Afterwards they see every project's, as upstream, can filter by project, open any of them, and change or delete them.

**Why this priority**: an administrator's view of the deployment is wrong without a sign; tenants are unaffected.

**Independent Test**: contract — as an administrator, each Networking API listing returns another project's resource, filters by project, and shows it by id; as a member, nothing changes.

**Acceptance Scenarios**:

1. **Given** project B with a network, a subnet, a port, a router, a floating IP and a security group, **When** an administrator of project A lists each resource type, **Then** B's resources are listed with A's.
2. **When** the administrator filters with `?project_id=B` or `?tenant_id=B`, **Then** only B's are listed.
3. **When** the administrator shows one of B's resources by id, **Then** it is returned.
4. **When** the administrator names one of B's resources by name where a name is accepted, **Then** it does not resolve, as for any name outside the caller's project.
5. **Given** a member of project A, **When** they list or show, **Then** the answers are as today: their own project's resources, shared and external networks, and nothing of B's.
6. **When** the administrator updates or deletes one of B's resources by id, or adds an interface on B's subnet to B's router, **Then** it succeeds, as upstream; a member gets `404` as today.
7. **When** the administrator puts B's security group on a port of project A by id, **Then** it succeeds, as upstream; a member naming B's group gets `404`.

---

### Edge Cases

- **A server with no port**, created without a network or still booting: add and remove answer `404`, as upstream; the server reports no group.
- **Several ports**: one change rewrites every affected port in one step. No request leaves some ports changed and others not.
- **Ports with different groups** (one attached by id, or changed through the port API): add puts the group on every port. If any port already carries it, the answer is `400` and nothing changes; upstream answers `400` too, after changing the ports it reached first. Remove takes it off every port that carries it, and answers `404` only when none does.
- **The report's shape**: one entry per port and group, as upstream. A server with two ports in `default` reports `default` twice. The order is not specified.
- **The filter or the NAT cannot be applied** (an iptables error, no uplink): the old filter and NAT stay whole (FR-007a), the request answers `500`, and its own change is undone (FR-008). This holds for membership, rule and floating-IP changes alike. A reconciliation pass on a real-mode node without an uplink logs an error and skips.
- **A port or a server deleted while a floating IP is associated with it**, `forceDelete` included (FR-008e): the floating IP is disassociated in the same step, and its NAT and filter go before the answer. If their removal fails, the deletion answers `500` and nothing is deleted: the port, its floating IPs' associations and, for a server, the server stay, and what was removed is applied again (FR-008d, FR-040). A server whose libvirt domain is already gone stays as when its libvirt deletion fails today; deleting it again retries.
- **A floating IP deleted while the node cannot withdraw it**: `500`, and it is back, with its association and its port forwardings. If another request deleted its port meanwhile, it comes back unassociated. If its network went, or its address was allocated again, it stays deleted (FR-008).
- **A floating IP created on a port whose NAT fails**: `500`, and the floating IP is removed again. If another request changed it in the moment between, it stays with that change, as any later change does (FR-008).
- **Two floating IPs on one fixed IP**, possible only from different external networks (FR-029): each has its own router's NAT; the port's chains apply to both.
- **Host-namespace NAT rules from an earlier layout**: reconciliation removes the marked ones and the `/32` addresses of floating IPs with a record (FR-044); unmarked ones without a record are the operator's and stay. The clean break of FR-044 is to reset the host with teardown first.
- **Another change to the same ports while a failed one is undone**: only the failed request's own change is undone. The other change stays.
- **A filter that already matches its stored list**: a reconciliation pass leaves it untouched, so the periodic passes open no window.
- **A slow firewall**: a firewall or NAT command that takes longer than one second, or a unit-lock wait that, with the apply, exceeds the call's shared deadline, is a failed change (FR-007b), with FR-008's answer.
- **Multi-node**: a change to north-south state is applied on the network node before the answer, whichever node serves the request (FR-034). A network node that cannot be reached within the bound is a failed change, `500` and undone.
- **Established connections**: removing a group stops new connections. A connection already open keeps its tracked state until it ends, as when a rule is removed today.
- **A port without a floating IP**: its chains are enforced all the same (FR-041); a floating IP associated later reaches it through them.
- **A duplicate add**: `400`, as upstream. **Removing a group no port carries**: `404`. **An unknown group**: `404`.
- **An id that is also another group's name**: the id wins, as at create. **A name matching several groups**: `409` in a server action, `404` in the group's path (FR-026).
- **An administrator acting on another project's server**: the administrative override applies as today. A name resolves in the server's project (register entry 5); an id names any project's group, as upstream.
- **A group of another project**: `404` for a non-administrator, through either API, as upstream. An administrator may apply it by id, as upstream; the port then carries a group its own project cannot list, as upstream allows.
- **A port of another project through the port API**: `404` for a non-administrator, as today; an administrator may change it (FR-058).
- **A group listing filtered by an id or a name that matches nothing**, another project's group included for a non-administrator: an empty list, as upstream.
- **eBPF filter active** (not a stock build): out of scope. Port creation no longer runs an eBPF step (FR-045, FR-047); the iptables port chains enforce in every mode.
- **Stub mode**: lists are stored and reported. Nothing is programmed.
- **A group deleted while in use**: `409`, as upstream, and nothing changes (FR-008c). Terraform retries the delete until the ports carrying the group are gone.
- **`default` in use, deleted by an administrator**: `409 SecurityGroupInUse`, as for any group in use; upstream checks use first too.
- **A listing filter Lightstack's groups have no value for**: `shared` and the tag filters answer from what is always true of a Lightstack group (never shared, no tags). `stateful` and `revision_number` compare stored values (FR-050, FR-051).
- **Floating-IP creates on two external networks with overlapping subnets**: each network's pools are its own, but an address is unique across all floating IPs. A create whose address another network's create took meanwhile picks again; it never answers `500` for that.
- **A pool update that leaves allocated addresses outside the new pools**: they stay, as upstream. Only new allocations follow the pools.

## Requirements *(mandatory)*

### Functional Requirements

**Membership changes through the compute API**

- **FR-001**: `addSecurityGroup` MUST put the group on every port of the server, as one change. If any port already carries it, the answer MUST be `400` and nothing changes.
- **FR-002**: `removeSecurityGroup` MUST take the group off every port that carries it, as one change, and answer `404` when no port does.
- **FR-003**: Both MUST answer `404` for a server with no port, as upstream.
- **FR-004**: Both MUST resolve the group by id or by name, an id match winning, as server create does. An id names any group the caller can see: a non-administrator's own project's, an administrator's any project's, as upstream. A name resolves in the server's project; for an administrator acting on another project's server that is a recorded divergence (constitution register, entry 5), since upstream resolves a name in the caller's own project.
- **FR-005**: Removing a server's last group MUST answer `202` and leave its ports with no group. The `cannot remove last security group` refusal MUST go.
- **FR-006**: An unknown group or server MUST keep answering `404`.

**Enforcement**

- **FR-007**: After any change to a port's list, through the compute API or the Networking API, that port's floating-IP ingress filter MUST be rebuilt from the new list before the request returns, with no restart. A port left with no group MUST get the empty filter: replies to its own connections, nothing else. That port is rebuilt too, although no group links it any more.
- **FR-007a**: A filter MUST be replaced in one atomic step: at every moment it holds either the complete old rule set or the complete new one, each ending in its `DROP`. A rebuild that fails MUST leave the old filter whole. This holds for every rebuild — floating-IP association, rule changes, membership changes and reconciliation. A floating IP's NAT MUST likewise change in one step: it points at the old fixed IP or the new one, never at both or at neither while it stays associated.
- **FR-007b**: Every firewall and NAT command a change runs on the request path MUST be bounded at one second: reading a chain, replacing it, ensuring or removing its jump, and adding or removing the NAT and the floating address. So MUST the wait for the unit's lock, which shares the node call's deadline (FR-034). Exceeding either is a failed change (constitution Article III; v1.5.0's change-then-undo exception).
- **FR-008**: If the node that holds the change's state cannot apply a stored change (FR-034), the request MUST answer `500` and undo exactly its own change, so the stored records match the node again. The change may be to a port's groups, a group's rules or a floating IP's association. The undo is compare-and-set: a record is put back only if it still holds what this request left, and a change another request made meanwhile MUST NOT be undone. Concretely:
  - a membership change is put back;
  - an added rule is removed, and a deleted rule is restored;
  - a floating IP created on a port is removed;
  - a moved or disassociated floating IP gets its association back;
  - a deleted floating IP is restored, with the port forwardings deleted with it. Rows other requests deleted meanwhile are applied as their foreign keys would apply them, so a deleted port leaves it unassociated, and a deleted network or an address allocated again keeps it deleted.

  Whatever the request had already applied on the node MUST be applied back from the restored records; if that fails too, reconciliation converges it. The failure MUST be logged naming the port or the floating IP, and the address. This holds through both APIs.
- **FR-008a**: A floating IP's NAT and address MUST be in force in its router's namespace on the network node (FR-033, FR-034) before the answer to creating it on a port, associating it, moving it to another port, disassociating it and deleting it. The traffic it forwards reaches the fixed IP only through the port's chains on the port's node (FR-041), which exist whenever the port is bound, with or without a floating IP. When the network node cannot apply it, FR-008 applies. A floating IP without a port needs nothing on any node.
- **FR-008b**: Adding or deleting a rule MUST converge, before the answer, the chains of every port that carries the rule's group, or whose groups name that group as a remote group, on the nodes the ports are bound to (FR-034, FR-041). When that fails, FR-008 applies.
- **FR-008c**: Deleting a group that any port carries MUST answer `409` (`SecurityGroupInUse`) and change nothing, as upstream. Deleting an id that names no group of the caller's project MUST answer `404`, as upstream. A group's deletion never changes a port's list.
- **FR-008d**: Deleting a port, directly or with its server, MUST disassociate the floating IPs that name it in the same step, as upstream: no port, fixed IP or router. Their status MUST be `DOWN` in the same step too; upstream's L3 agent reports `DOWN` afterwards. Their NAT and filters MUST be gone before the answer. The removal comes before the port's record is deleted (FR-040): when it fails, the answer is `500`, the port and its floating IPs' associations stay, and what was removed is applied again.
- **FR-008e**: The `forceDelete` server action MUST delete a server exactly as `DELETE /v2.1/servers/{id}` does, as upstream's `force_delete` does. Its boot ports go, with their floating IPs disassociated (FR-008d). The ports it attached by id are detached. Its volumes are released, or deleted when marked for deletion on termination. Its answer stays as today.
- **FR-009**: Withdrawn in clarification (2026-11-17). Re-programming a port's eBPF rule set is out of scope.
- **FR-010**: Reconciliation MUST keep every node's floating-IP filters and NAT matching the stored records, in both directions:
  - It re-asserts what the records hold, so a change made through either API survives restarts and is never reverted. A record is a floating IP's row, associated or not.
  - It removes what they no longer hold. A disassociation or deletion thereby reaches the nodes that did not serve it, a request whose process stopped after storing it, and a node whose undo failed.
  - It removes only state Lightstack created and can recognise as its own, and never a filter that anything on the node still forwards to. It removes nothing when its read of the records is incomplete, and still re-asserts what it did read.
  - A filter or a NAT that already matches its records MUST be left untouched.

**Create and boot**

- **FR-011**: Every port the boot creates MUST carry the groups requested at create, or `default` when none were, changed by any add or remove accepted while the boot runs.
- **FR-012**: A server created with no group MUST behave exactly as today: `default` on its ports and in its report.

**Interfaces attached later**

- **FR-013**: A port created for an interface attached on a network MUST get the project's `default` group, created if the project has none, as upstream. A port attached by id MUST keep its groups unchanged.

**What a server reports**

- **FR-014**: A server's reported groups MUST be the groups its ports carry, as upstream: one entry per port and group, and the `security_groups` key omitted when no port carries any. A server MUST NOT be reported as `default` unless a port of it carries `default`.
- **FR-015**: A server from an earlier release, with no group on its ports, MUST report no group.
- **FR-016**: `GET /v2.0/ports?device_id=<server>` MUST list on each port exactly the groups recorded on it, which are the groups its floating-IP filter is built from.

**The Networking API**

- **FR-017**: `PUT /v2.0/ports/{id}` MUST accept `security_groups`. A list of group ids replaces the port's groups, `[]` or `null` leaves it with none, as upstream, and an absent field leaves them unchanged. Sent with other fields, every field applies. The response reports the new list, and FR-007 applies.
- **FR-018**: Each id MUST name a group the caller can see — for a non-administrator, a group of the port's project; for an administrator, any project's group, as upstream; otherwise the answer is `404` and nothing changes. This holds for port create and update. A repeated or malformed id MUST answer `400`, as upstream.
- **FR-019**: `POST /v2.0/ports` with `security_groups: []` or `null` MUST create a port with no group, as upstream. An absent field MUST keep giving `default`.
- **FR-019a**: `GET /v2.0/security-groups` MUST honour every attribute filter upstream does, within what the caller may see (FR-057), and each MAY repeat, listing groups that match any of its values, as upstream:
  - `id` and `name`;
  - `description`;
  - `project_id` and `tenant_id`: for an administrator, the named projects' groups, as upstream; for anyone else, the caller's own groups when the caller's project is named, else nothing, as upstream;
  - `stateful` and `revision_number`, against the stored values (FR-050, FR-051);
  - `shared`, `tags`, `tags-any`, `not-tags` and `not-tags-any`, answered from what is true of every Lightstack group: not shared, no tags.

  A value that matches no group lists nothing. A boolean filter whose value is not a boolean MUST answer `400`, as upstream's validation does.
- **FR-019b**: A group in a listing, a show, a create and an update MUST report its `stateful` and `revision_number`, and `shared: false` and `tags: []`.

**The `default` group and group names**

- **FR-024**: Deleting a project's `default` group as a non-administrator MUST answer `409` (`SecurityGroupCannotRemoveDefault`) and change nothing, even when no port carries it, as upstream. An administrator MAY delete it when no port carries it; FR-008c applies first, as upstream checks use first.
- **FR-025**: Updating `default` with a `name` MUST answer `409` (`SecurityGroupCannotUpdateDefault`), and its description MUST stay updatable, as upstream. Creating a group named `default`, in any case, or renaming any group to it MUST answer `409` (`SecurityGroupDefaultAlreadyExists`), as upstream; this name check comes before the previous one, as upstream's validation comes before its update.
- **FR-026**: Group names MAY repeat within a project, as upstream; only `default` is reserved (FR-025). A name matching several of the caller's groups where Lightstack resolves a group by name — server create, `addSecurityGroup`, `removeSecurityGroup` — MUST answer `409` (`NoUniqueMatch`), as upstream Nova does, changing nothing. `GET /v2.0/security-groups/{name}` with an ambiguous name MUST answer `404`, as upstream answers any name there, so clients fall back to listing by name; `?name=` lists every match. The schema drops its unique constraint on group names (migration 077), and exactly one `default` per project stays guaranteed by a partial unique index.

**Floating-IP allocation and association**

- **FR-027**: Floating-IP creates sent at the same time MUST each get a distinct address; none may answer `500` because another took its address. The address is chosen inside the create's transaction.
- **FR-028**: Creating a floating IP on a port, or associating one with a port, MUST answer `404` (`ExternalGatewayForFloatingIPNotFound`) and store nothing unless a router has its gateway on the floating IP's network and an interface on the subnet of the fixed IP. Of several such routers, the one whose interface holds the subnet's gateway address is chosen, else the first, as upstream. This replaces the create's association without a router and the update's `400`.
- **FR-029**: Associating a floating IP with a fixed IP that already has a floating IP of the same external network MUST answer `409` (`FloatingIPPortAlreadyAssociated`) and change nothing, as upstream, on create and on update.

**Allocation pools**

- **FR-030**: A subnet MUST store its allocation pools. Subnet create and update MUST accept `allocation_pools`, and every read MUST report the stored pools. An absent field on create gives upstream's default pool: the second address to the last usable one, with the gateway removed, which can split it in two. Validation as upstream: a pool outside the subnet answers `400` (`OutOfBoundsAllocationPool`); a malformed pool or one ending before it starts, `400` (`InvalidAllocationPool`); overlapping pools, `409` (`OverlappingAllocationPools`); a pool holding the gateway address, through the pools or a gateway change, `409` (`GatewayConflictWithAllocationPools`).
- **FR-031**: A floating IP's address MUST come from the pools of its network's IPv4 subnets, skipping the gateway and every address a port or a floating IP holds. A network with no IPv4 subnet MUST answer `400`, where it hands out an address of `192.0.2.0/24` today. When the pools are taken, the answer MUST be `400` (`ExternalIpAddressExhausted`).
- **FR-032**: A port's fixed IP chosen without a requested address MUST come from its subnet's pools. A requested address MAY lie outside them, inside the subnet, as upstream. No two allocations on a subnet, of any owner (a port, a DHCP port, a router's gateway, a floating IP), MAY ever hand out one address, however concurrent.
- **FR-032a**: The schema migration MUST give each existing subnet the default pool. No subnet keeps a range of its own from before: no deployment exists to preserve (Clarifications, 2026-11-20).

**The router namespace**

- **FR-033**: In real mode, every router MUST live in a namespace of its own on one network node, which carries the router's north-south traffic. No other node may hold a router's or a floating IP's address. With VXLAN on, the network node MUST be set in configuration; with VXLAN off, the node is its own network node whatever the setting says.
- **FR-034**: A request served by another node MUST have the network node apply its north-south change before the answer: an authenticated node-to-node call, bounded at one second, whose failure or timeout is the request's failure under FR-008. On the network node the apply is local. No request may answer success for a north-south change the network node has not applied.
- **FR-035**: A router's external leg MUST be a veth pair, its `qg-` end in the namespace holding the router's and its floating addresses. The network node MUST answer ARP for those addresses on its uplink and route them to the veth, and MUST NOT change the uplink's own configuration or addresses. Floating addresses MUST be deliverable to the network node's uplink: on a LAN through those ARP answers; where an underlay delivers only addresses assigned to the node, `docs/` MUST say the operator allows them there.
- **FR-036**: Router namespaces, and everything in them, MUST survive a restart of Lightstack: its pod, its container or its process. North-south traffic and established connections MUST keep flowing meanwhile. The chart and the Compose file MUST mount the host's `/run/netns` with bidirectional propagation. On start, the reconciler re-asserts each namespace from the records; the chart's teardown removes them.
- **FR-037**: A router's interface on a subnet MUST be a veth whose `qr-` end, in the namespace, holds the subnet's gateway address, and whose other end joins the network's bridge on the network node. Each subnet with DHCP MUST have one DHCP address of its own, allocated from its pools and reported as a port with `device_owner` `network:dhcp`, as upstream; one `dnsmasq` per network MUST serve each of its DHCP subnets, as upstream's agent does, on the network node only, and metadata MUST be reached through a static route to that address. No bridge, on any node, MAY hold an address. A subnet no router joins has no holder of its gateway address, as upstream.
- **FR-038**: Setting a router's gateway MUST give it an address of the external network: the one requested in `external_gateway_info.external_fixed_ips`, or one from the pools of the network's IPv4 subnets (FR-031). The address MUST be held on the router's `qg-` end, reported as a port with `device_owner` `network:router_gateway`, and listed in `external_gateway_info.external_fixed_ips` with its subnet, as upstream. Requesting an address is administrator-only, as upstream's policy has it; a non-administrator's request MUST answer `403`. A requested address outside the network's subnets MUST answer `400` (`InvalidIpForNetwork`), one equal to a subnet's gateway `400`, and one already held `409` (`IpAddressAlreadyAllocated`, IPAM's error), as upstream. With SNAT on, the router's subnets MUST leave through that address. Clearing the gateway releases it.
- **FR-039**: Removing a router's interface on a subnet whose fixed IPs a floating IP reaches through that router MUST answer `409` (`RouterInterfaceInUseByFloatingIP`), and clearing or changing the gateway of a router a floating IP goes through MUST answer `409` (`RouterExternalGatewayInUseByFloatingIp`), changing nothing, as upstream. Deleting such a router is already refused, since it has an interface.
- **FR-040**: A deletion that removes host state MUST remove it before it deletes the record, and MUST answer success only once both are done. It checks its refusals (`404`, in use) in a short transaction first; removes the host state under the node lock, through FR-034 for the network node's; then deletes the record in a transaction that checks the refusals again. When the removal fails or exceeds its bound, or the final transaction refuses or fails, the answer is `500` (or that refusal), the record stays, and what was removed MUST be applied again from it, by the request or, failing that, by reconciliation within one pass. This holds for networks, subnets and their DHCP, routers with their namespace, interfaces and gateway, ports and port forwardings. A floating IP's deletion keeps FR-008's restore, already built. State on nodes other than the network node, such as a deleted network's bridges, is removed by their reconciliation within one pass, as today.

**Security groups on every port**

- **FR-041**: In real mode, every port's security groups MUST be enforced on the node its TAP is on, in both directions and for all traffic, as upstream's iptables firewall driver does: bridged traffic through `physdev` matches per TAP into per-port chains built from the port's groups, ingress and egress rules, remote groups resolved to their members' addresses, connection tracking for replies, and everything else dropped. A port with no group admits only replies. A change to a port's groups, a group's rules or a remote group's members MUST be in force on that node before the answer, through FR-034's node-to-node call where another node holds the TAP, or answer `500` and undo (FR-008). When a port's binding moves to another node or is cleared, its chains MUST be removed from the old node in the same request. At boot and at interface attach, a port's chains MUST be in force on its node before the guest uses the port; when they cannot be applied, the boot fails as a libvirt failure does (`ERROR` with a fault) and the attach answers `500`. Reconciliation keeps every node's port chains matching the records, in both directions. The `LS-SG-*` chains are replaced by this; teardown keeps removing them where earlier releases left them.
- **FR-042**: The per-fixed-IP floating-IP ingress filter MUST be retired; the port's chains (FR-041) enforce floating-IP traffic. Wherever FR-007, FR-007a, FR-008, FR-008a, FR-008b, FR-010, FR-016, the Key Entities and the Success Criteria name the floating-IP filter, they apply to the port's chains instead, and their ordering rules for the filter fall away. The floating-IP NAT keeps them as written. Reconciliation removes the filter chains and jumps that earlier releases of this feature left.
- **FR-043**: A group created through the API MUST start with upstream's two egress rules, allowing all IPv4 and all IPv6. A project's `default`, seeded or created on demand, MUST start with those and two ingress rules whose remote group is `default` itself, IPv4 and IPv6, as upstream; still no rule open to every address (cloud-storage#528). Existing groups are not changed, except the seeded project's `default`, which migration 076 gives the two member rules so a fresh install matches: no deployment exists to preserve (FR-044).
- **FR-044**: This release's real-mode layout MUST be built by reconciliation from the records alone. It MUST NOT convert a host running the earlier layout beyond removing what reconciliation already recognises as Lightstack's. `docs/UPGRADING.md` MUST state the break: run `lightstack cleanup` before this release, which removes the node's guests and networking, and recreate the servers, since a port without `binding_host_id` has no chains. A network without a connection-tracking zone (one created before migration 076) gets the lowest free one the first time a unit or pass needs it.
- **FR-045**: No request path may log a failed host step and answer success. A create or an update that changes host state MUST follow FR-008: store, apply, and on failure `500` with a compare-and-set undo; network creation's undo included. A deletion MUST follow FR-040. This covers every path the constitution's v1.5.0 migration plan names: networks, subnets and DHCP, routers with their namespace, interfaces and gateway, port forwarding, port creation's FDB step, port and instance-port teardown, and group and rule changes (FR-041). Port creation's eBPF step goes with the Neutron TAP it attaches to (FR-041). The constitution's migration plan MUST then name no departure.
- **FR-046**: In real mode, Lightstack MUST load `br_netfilter` and set `net.bridge.bridge-nf-call-iptables` and `net.bridge.bridge-nf-call-ip6tables` to `1` on every node at start. When it cannot, its readiness MUST fail and name the reason, and it MUST NOT report ready with ports unfiltered. The host-testing runbook checks both.
- **FR-047**: The port firewall (FR-041) MUST enforce in `ebpf` mode as in `iptables` mode. Port creation no longer runs an eBPF step: its attach point, Neutron's own TAP, is gone (FR-045). `docs/EBPF_STATUS.md` MUST say both.
- **FR-048**: FR-023's checks, as restated for the router namespace and the port firewall, belong to the single-node run below. Two real-mode runs MUST be recorded in this feature's host-testing runbook, with no restart unless the step is one:
  - **The chart on a single Kubernetes node**: the router namespace and both legs; DHCP and metadata through the DHCP address; the router's gateway address and SNAT; a floating IP and a port forwarding reaching a VM; the port firewall in both directions, between VMs, through floating IPs and for egress; a deletion of each kind; FR-039's refusals; the pod restarted with traffic and an established connection flowing throughout (FR-036); FR-046's settings, with readiness failing when they cannot be set; and Horizon's instance *Edit Security Groups* pre-selecting an instance's groups and changing its ports (FR-021, SC-006).
  - **Two nodes** with a shared PostgreSQL and VXLAN: only the network node holds router and floating addresses; a change served by the other node is in force on the network node when the API answers, and fails with `500` and is undone when the network node cannot be reached; a VM on the second node reaches the outside, and is reached through its floating IP, via the network node; a port on the second node is firewalled there.
- **FR-049**: A port forwarding MUST forward through its floating IP's router namespace, on the router's `qg-` end, and follow FR-008 on create and update and FR-040 on delete. Its NAT MUST be marked as Lightstack's and kept matching the records by reconciliation in both directions, as a floating IP's is. Deleting a floating IP MUST remove its port forwardings' NAT with it.
- **FR-050**: A security group MUST carry upstream's `revision_number`: `0` when created, one more on every update of the group and on every rule added to or deleted from it, as upstream's revision plugin bumps a group for its rules. It is reported (FR-019b) and filterable (FR-019a). An undone change puts the revision back, compare-and-set, with the rest of the change (FR-008). `PUT` and `DELETE` of a security group MUST honour `If-Match: revision_number=<n>`, answering `412` (`RevisionNumberConstraintFailed`) and changing nothing when the group's revision differs, as upstream's revision-if-match does. The Networking API's extension list MUST advertise `stateful-security-group` (FR-051).
- **FR-051**: A security group MAY be created stateless with `"stateful": false`, as upstream; absent means stateful. Changing `stateful` while any port carries the group MUST answer `409` (`SecurityGroupInUse`). A port MUST NOT carry stateful and stateless groups at once: the check is per port, against that port's own groups, as upstream's `_validate_sgs_for_port`: a port create or update that would make one port mix them MUST answer `409` (`SecurityGroupConflict`), and an `addSecurityGroup` that would make any of the server's ports mix them `409` (as upstream Nova's `SecurityGroupConnectionStateConflict`), changing nothing. Ports of one server may differ in kind; an interface attach is never refused for it, since a port attached by id keeps its groups (FR-013); a server create whose groups mix both kinds ends `ERROR` with a fault, as upstream's build does. A port whose groups are stateless gets `CT --notrack` instead of its network's zone and no connection-tracking rules in its chains, as upstream's driver does (FR-041).
- **FR-052**: Deleting a subnet on which a port other than its network's DHCP port has an address MUST answer `409` (`SubnetInUse`) and change nothing, as upstream.
- **FR-053**: A requested fixed IP, on a port or an interface attach, that is its subnet's network or broadcast address MUST answer `400` (`InvalidIpForSubnet`), as upstream.
- **FR-054**: A floating IP's requested `fixed_ip_address` that is not one of the port's MUST answer `400`, and a requested `floating_ip_address` that is taken MUST answer `409` (`IpAddressAlreadyAllocated`), as upstream; `subnet_id` and `floating_ip_address` in a create MUST be honoured.
- **FR-055**: Adding a router interface on a subnet the router already has, or on a subnet without a gateway, MUST answer `400`, as upstream, storing nothing.
- **FR-056**: A port MUST report `binding:host_id`, the node its TAP is on, to administrators, as upstream; Nova sets it at boot and at interface attach and clears it at detach.
- **FR-057**: Every Networking API listing — networks, subnets, ports, security groups and their rules, routers, floating IPs and their port forwardings, QoS policies, RBAC policies, trunks, address scopes, subnet pools, IP availabilities and metering labels and rules — MUST show an administrator every project's resources, and `project_id`/`tenant_id` MUST narrow it to the named projects, as upstream. A show by id MUST reach any project's resource for an administrator, as upstream's read policy does. A name resolves only in the caller's own project, administrators included, as for Nova's servers. A non-administrator's listings and shows are unchanged. Writes: FR-058.
- **FR-058**: An administrator MUST be able to update, delete and act on any project's Networking API resource addressed by id, as upstream's `admin_or_owner` policy allows — networks, subnets, ports, security groups and their rules, routers and their interfaces and gateways, floating IPs and their port forwardings, QoS and RBAC policies, trunks, address scopes, subnet pools, metering labels and rules — and to reference another project's resource by id in a create (a port on its network, a floating IP on its port, an interface on its subnet). A resource an administrator creates belongs to the project the request names, as SPEC-024's rule already has it, or else to the administrator's own. Names resolve only in the caller's project. Applying another project's security group by id is allowed to an administrator too (FR-018). A non-administrator's writes are unchanged.

**Documentation and verification**

- **FR-020**: `docs/UPGRADING.md` MUST record each answer that changes, and the one operator step:
  - last-group removal `400` → `202`;
  - add and remove accept an id, change the ports and so the firewall, and answer `404` (was `202`) for a server with no port;
  - a server reports its ports' groups: no invented `default`, servers from earlier releases included, one entry per port;
  - a new interface's port gets `default`;
  - `PUT /v2.0/ports/{id}` applies `security_groups`, and `POST /v2.0/ports` with `[]` or `null` gives no group;
  - `GET /v2.0/security-groups` honours `id` and `name`;
  - deleting a group a port carries `204` → `409`, and a missing group `204` → `404`;
  - deleting a port or a server disassociates its floating IPs completely, as upstream: no fixed IP or router and status `DOWN`, where only the port used to be cleared;
  - `forceDelete` deletes a server as a delete does: its boot ports and its volume attachments go, where they used to be left behind;
  - a membership, rule or floating-IP change that the node cannot apply answers `500` and changes nothing, where it used to answer success;
  - the floating-IP NAT is marked as Lightstack's. Rules left by earlier releases for floating IPs deleted before the upgrade are not removed automatically; the section gives the command that lists them;
  - `GET /v2.0/security-groups` honours every attribute filter; groups report `stateful`, `revision_number`, `shared` and `tags`, and may be created stateless (FR-050, FR-051);
  - `default` can no longer be deleted by a non-administrator, nor renamed, nor created a second time (`204`/`200`/`500` → `409`), and duplicate group names are allowed (`500` → `201`), an ambiguous name in a server action answering `409` (FR-026);
  - floating-IP creates sent together no longer fail with `500`; an association needs a router joining the port's subnet to the floating IP's network (create `201` → `404`, update `400` → `404`); a second floating IP on a fixed IP within one external network answers `409`; a network with no IPv4 subnet answers `400` instead of handing out `192.0.2.x`;
  - subnets keep and report their `allocation_pools`; floating IPs and ports without a requested address come from them; existing subnets get the default pool, so the floating range of an external network is set through its pools.
  - new groups start with two egress rules, and `default` admits its own members (FR-043);
  - a subnet in use answers `409` on delete; a requested network or broadcast address `400`; a router interface on a subnet the router has, or one without a gateway, `400` instead of `500`; a floating IP's foreign `fixed_ip_address` `400` and a taken `floating_ip_address` `409` (FR-052–FR-055);
  - routers report `external_fixed_ips` from a gateway port; requesting one is administrator-only; removing an interface or clearing a gateway a floating IP uses answers `409` (FR-038, FR-039);
  - ports report `binding:host_id`; ports with `device_owner` `network:dhcp` and `network:router_gateway` appear (FR-037, FR-038, FR-056);
  - every real-mode change that the node holding its state cannot apply, deletions included, answers `500` and changes nothing (FR-040, FR-045);
  - an administrator's Networking API listings show every project's resources, shows by id reach them, and writes by id are allowed, as upstream (FR-057, FR-058);
  - the real-mode layout: one network node (`neutron.network_node`), router and DHCP namespaces, the port firewall between VMs and for egress, `br_netfilter` required; reset a host with `lightstack cleanup` before running this release (FR-033–FR-047).
- **FR-021**: The Horizon compatibility report MUST record instance *Edit Security Groups* as verified.
- **FR-021a**: The constitution's register of divergences MUST carry entry 5, added in v1.5.0 and narrowed on 2026-11-20 to names. Divergence: an administrator's `addSecurityGroup` / `removeSecurityGroup` on another project's server resolves a group name in the server's project. Upstream: a name resolves in the caller's own project. Why permitted: administrators adding a tenant's group by name succeed today, and upstream's rule would refuse them (Article I, limit 1). Ids follow upstream. Recorded as: SPEC-033 FR-004.
- **FR-022**: Every requirement above MUST have an automated test that fails without the change (the constitution's Test-First Development): the compute paths through the real network service in stub mode on the same test database, with a stand-in only where a failure must be injected; the network service against a real database (the empty list included); the contract suite through the public API; and `test/security_groups_test.sh` through the CLI, its Test 11 included.
- **FR-023**: A real-mode host run MUST show, with no restart:
  - a rule on a non-default group opening and closing floating-IP traffic and traffic from another VM, through the compute API and through the Networking API, and removing the last group closing it;
  - a rule added to and deleted from a group opening and closing it;
  - a floating IP moved, disassociated and deleted, and a server deleted with its floating IP associated, closing at once;
  - a NAT rule in a router namespace that no record holds removed within a pass.

  It MUST also show that one new connection attempt per second to a closed port gets none through, across reconciliation passes and changes. The run is recorded in this feature's host-testing runbook. No CI substitute exists.

### Key Entities

- **A port's groups**: what is recorded on a port. What enforcement reads, and the only list.
- **A server's groups**: what the compute API reports for a server — the groups of its ports.
- **Floating-IP ingress filter** (retired by FR-042; its role passes to the port's chains): one per fixed IP with a floating IP. Replies to the server's own connections, one allowance per ingress rule of the port's groups, then drop. Replaced as a whole, never edited in place.
- **Floating-IP NAT**: per associated floating IP, its address on its router's `qg-` end in the router namespace on the network node, answered for on the uplink (FR-035), and the rules that forward it to the fixed IP, marked as Lightstack's. Present while a record associates the floating IP or gives it port forwardings (R37).
- **A rule of the shape**: a NAT rule that forwards one address to one fixed IP and does nothing else, the form Lightstack writes. It is marked when this release wrote it. It is unmarked when an earlier release wrote it, or when someone else wrote the same form. An unmarked one counts as Lightstack's only when its address has a floating-IP record or a marked rule.
- **Floating-IP record**: a floating IP's row, associated with a port or not.
- **Router namespace**: one per router, on the network node: `qg-` to the outside with the router's gateway and floating addresses, `qr-` on each joined network's bridge with the subnet's gateway address. Survives restarts of Lightstack (FR-033–FR-038).
- **Port chains**: per port, on the node its TAP is on: the port's groups' rules in both directions, replies, then drop (FR-041).
- **Security group and rule**: a group a port carries cannot be deleted. A project's `default` cannot be renamed, nor deleted by a non-administrator, and no second group takes its name. Names may repeat, except `default` (FR-026). Every group is unshared and untagged; it is stateful unless created stateless (FR-051), and carries a revision number (FR-050).
- **Allocation pool**: a range of a subnet's addresses that Lightstack allocates from: a floating IP's address, and a port's fixed IP when none is requested. Stored per subnet.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After every operation that changes membership — create, add, remove, last-group removal, interface attach and detach, port update — each port lists exactly the groups enforced on it, and the server reports exactly its ports' groups. Zero mismatches across the automated cases.
- **SC-002**: On a real-mode host, adding a non-default group opens its port through the floating IP on the next connection attempt, and removing the group closes it, through either API. No restart, no manual step.
- **SC-003**: Removing a server's last group answers `202`, and afterwards no new inbound connection reaches the server through its floating IP.
- **SC-004**: For two minutes after a change (at least four reconciliation passes), the enforced set stays the new one.
- **SC-005**: Changing a server's only group from one to another, removal first as Terraform does it, succeeds end to end.
- **SC-006**: Horizon's instance *Edit Security Groups* pre-selects exactly the instance's groups, completes, and the instance's ports carry the chosen groups.
- **SC-007**: A server created with no group is indistinguishable from today: `default` on its port and in its report.
- **SC-008**: The unit and contract suites pass on both database backends.
- **SC-009**: Terraform's `openstack_networking_secgroup_v2` data source finds a group by name in a project with several groups.
- **SC-010**: New connection attempts to a port the filter does not allow, one per second, get none through — across at least four reconciliation passes and while groups are added and removed.
- **SC-011**: When a filter or a NAT cannot be applied, the change answers `500`, and afterwards the stored records and the node's filters and NAT agree. This holds for membership, rule and floating-IP changes.
- **SC-012**: An administrator adds a tenant's group to the tenant's server by name and by id, and both succeed; by id an administrator may also apply another project's group, as upstream. A non-administrator naming a group of another project gets `404`, through either API.
- **SC-013**: A rule added to or deleted from a group is in force through every floating IP of the ports carrying the group when the API answers.
- **SC-014**: A floating IP that is moved, disassociated or deleted, or whose port or server is deleted or force-deleted, forwards nothing to its old fixed IP once the API has answered success. No node but the network node ever forwards it (FR-033).
- **SC-015**: Deleting a group that a port carries answers `409`, and the port keeps the group.
- **SC-016**: Each attribute filter of the group listing returns exactly the groups upstream's would, within what the caller may see (FR-057); zero filters silently ignored.
- **SC-017**: A non-administrator cannot lose `default` or its rules through the API: deleting, renaming or re-creating it answers `409` and leaves it unchanged.
- **SC-018**: Ten floating IPs created at once on one external network all succeed, with distinct addresses, on both database backends.
- **SC-019**: Every floating IP and every port address chosen without a request lies in its subnet's pools.

## Assumptions

- The reconciliation interval stays 30 seconds and runs on every node. It bounds how long a request whose process stopped, a failed undo, or state on a node other than the one a change applies on takes to converge.
- A change to host state succeeds only once the node that holds that state has applied it, the network node for north-south and the port's node for its chains (FR-034, FR-041); otherwise it is undone (FR-008), or for a deletion not made (FR-040). Reconciliation converges the rest within one pass. They rest on constitution v1.5.0's exceptions to Article II — "a change undone when its external work fails", "a removal applied before its record goes" (FR-040) and the reconciliation loops:
  - "a change undone when its external work fails": the client gets the answer one transaction would have given;
  - "stateless reconciliation loops": other nodes, a request whose process stopped, and a node whose own undo failed.
- Removing a group does not cut connections already established. Lightstack does not do so for a removed rule either.
- In a stock build the eBPF filter never initialises. The iptables port firewall is the enforcement in every mode (FR-041, FR-047), and the host runs cover it.
- The port API keeps its project scoping for non-administrators: a member changes their own project's ports. Administrators may change any project's (FR-058).
- SPEC-009's documents stay as written: a finished feature's spec belongs to its own change. This spec records that SPEC-009's FR-016a now holds for every group, not only `default`.
- No new dependency. One new setting names the network node (FR-033). Two migrations on both backends — 076: subnets' allocation pools (FR-030, FR-032a), `ports.binding_host_id`, `networks.ct_zone`, `security_groups.revision_number` and `stateful`, and the seeded `default`'s member rules; and 077, which drops the unique constraint on group names and adds a partial unique index for `default`, its SQLite file running outside a transaction (a migrator change, `migrations/README.md`). Group membership needs none, since ports already record their groups.
- No Lightstack deployment exists yet (stated by the user, 2026-11-20). Changes to the real-mode layout are a clean break, documented, with no conversion of a running host (FR-044).

## Out of Scope

- The create response does not echo `security_groups`, as upstream's does.
- eBPF: re-programming a port's rule set when its groups change, rules re-applied after a restart, the 100-rules-per-port limit, and the empty object path. eBPF never initialises in a stock build; the iptables port firewall enforces in both modes (FR-047). Whether eBPF stays is the user's separate decision.
- Cutting established connections when a group is removed.
- A port named in server create's `networks`, which Lightstack does not accept.

## Dependencies

- SPEC-017 (cloud-storage#524): records and reports a server's groups and fixed the create path.
- SPEC-009: the floating-IP filter and the reconciler.
- Real-mode hosts for FR-023 and FR-048: a Kubernetes node running the chart, with libvirt and a routed floating IP; and two hosts sharing a PostgreSQL and a VXLAN segment.
- A Horizon to verify SC-006: the Docker Compose stack or the verification chart.
