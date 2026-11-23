# Draft: round 2's items for cloud-storage#579's `## Checklist` (T163)

Posted 2026-11-23: appended to the `## Checklist` comment (id `6948762143`) below its existing items,
with the host-run items ticked after the run passed. The round-1 items keep their wording: the weekly summaries match
items by their text, and
a reworded item would be reported again. Three of them describe the floating-IP filter that round 2
retires (FR-042); their substance — what a change must have done before its answer — holds for the
ports' chains, which this round's items state.

Ticked: implemented, with unit and contract tests green on both backends (2026-11-23, working tree on
the API commit (`fix(neutron): answer the networking API as upstream does`, then `ba700dc`, rewritten since),
uncommitted). Unticked: needs the host run (T166), or is open.

---

### Acceptance criteria (round 2, clarified 2026-11-19 to 2026-11-20)

- [x] `GET /v2.0/security-groups` honours every attribute filter upstream does (`description`, `project_id`/`tenant_id`, `stateful`, `revision_number`, `shared`, the tag filters), repeatable; groups report `stateful`, `revision_number`, `shared: false` and `tags: []`
- [x] A group may be created stateless; a port never carries stateful and stateless groups at once (`409`); `If-Match: revision_number=<n>` on update and delete answers `412` when it differs
- [x] `default` cannot be deleted by a non-administrator, renamed, or created a second time (`409`, as upstream); group names may otherwise repeat, and an ambiguous name in a server action answers `409 NoUniqueMatch`
- [x] New groups start with upstream's two egress rules; a project's `default` also admits its own members
- [x] Floating-IP creates sent together each get an address of their own; an association needs a router joining the port's subnet to the floating network (`404` otherwise); a second floating IP on a fixed IP of one external network answers `409`; a network with no IPv4 subnet answers `400`
- [x] Subnets store, validate and report `allocation_pools`; ports, DHCP ports, router gateways and floating IPs take their addresses from them; a subnet in use answers `409` on delete
- [x] A router's gateway is a port with an address of the external network, reported in `external_fixed_ips`; requesting one is administrator-only; removing an interface or clearing a gateway that a floating IP goes through answers `409`
- [x] An administrator's Networking API listings show every project's resources, shows by id reach them, and writes by id are allowed, as upstream; names resolve in the caller's project only
- [x] Real mode: one network node holds every router (`qrouter-<id>`) and network DHCP (`qdhcp-<id>`, metadata included); a change served by another node is applied there through an authenticated one-second node call before the answer, or answers `500` and is undone
- [x] Real mode: every port is firewalled on its node by its own chains, both ways, remote groups resolved to their members; a change to a port's groups, a rule or a remote group's members is in force on the port's node before the answer
- [x] Real mode: a deletion removes its host state before its record, and re-applies it when the record stays
- [x] Real mode: each node's reconciliation converges its units both ways, removing only what it recognises as Lightstack's and nothing after an incomplete read; the retired state of the earlier layout is removed where recognised
- [x] Real mode: namespaces survive a restart of Lightstack (the host's `/run/netns` mounted both ways); `br_netfilter` loaded and the bridge settings set at start, readiness failing, naming `bridge-netfilter`, until they hold
- [x] The chart on one Kubernetes node: FR-048's single-node run passed (2026-11-23) and recorded, Horizon's instance *Edit Security Groups* included

### Tasks (round 2)

- [x] Plan round 2: the network layout and the API answers (`plan.md`, `research.md` R26–R53, contracts)
- [x] Migrations 084 (pools, `binding_host_id`, `ct_zone`, revision and statefulness, `default`'s member rules) and 085 (group names may repeat; one `default` per project), on both backends
- [x] Neutron: the security-group listing's filters, representation, `If-Match`, stateless groups, `default` and names, starting rules
- [x] Neutron: allocation pools and one allocator for every automatic address; the floating-IP allocation lock, router lookup and FR-029; requested fixed IPs on ports
- [x] Neutron: router gateway ports and interfaces in one transaction; FR-039's refusals
- [x] Neutron: administrators' listings, shows and writes across the Networking API
- [x] Node identity, `neutron.network_node`, and the authenticated node-to-node call (`/internal/v1/converge`)
- [x] Router units in `qrouter-` namespaces on the network node: legs, addresses, NAT and mangle in one restore each, proxy ARP on the uplink
- [x] DHCP units in `qdhcp-` namespaces: the DHCP port, dnsmasq with static ranges and option 121, metadata served in the namespace by network and address
- [x] The port firewall (upstream's iptables driver under Lightstack's names), port bindings set by Nova at boot and attach, the TAP named by its port; the floating-IP filters retired
- [x] Deletions: host state first, then the record; the re-apply on failure
- [x] Reconciliation by role, the retired state's removal, the VXLAN entries and devices both ways; teardown of the new layout and its database-less fallback
- [x] The node: bridge netfilter at start and in readiness; the chart's and Compose's `/run/netns` and `/lib/modules` mounts, real mode requiring `realMode.privileged`
- [x] Docs: `UPGRADING.md` (round 2's answers, the clean break), `EBPF_STATUS.md`, the networking, VXLAN, configuration, troubleshooting and scaling docs, the chart README
- [ ] Multi-node VXLAN flooding: broadcast does not cross nodes (no flooding destination), so a guest off the network node gets no DHCP lease — found in T144, outside the plan; blocks multi-node
- [x] Host runbook (`HOST_TESTING.md`) and its host run
- [ ] Rebase on `main` and constitution v1.6.0 (T057), after the migrations squash lands

The node item's "real mode requiring `realMode.privileged`" is the posted wording and stays: T177 has since removed
the key, and real mode always runs the workload and the purge Job privileged.
