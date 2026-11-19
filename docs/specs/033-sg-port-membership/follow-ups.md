# Follow-ups — SPEC-033

Four tickets for the user to file (research R17). None is filed from here. Locations are at `d81f081`, the commit this feature is based on, unless they say otherwise.

---

## 1. The security-group listing's other filters

**Problem.** `GET /v2.0/security-groups` honours `id` and `name` since SPEC-033 (FR-019a). Every other attribute filter is ignored, and the whole project is listed instead: `description`, `project_id`/`tenant_id`, `tags`, `shared`, `stateful`, `revision_number`. Upstream applies them, so the divergence is silent.

**Proposal.** Honour the filters the data model can answer: `description`, and `project_id`/`tenant_id` within what the caller may see. For the rest, register the divergence or refuse it. Refusing them all with `400`, as the server-list filters do (register entry 4), would break Horizon, which sends `tenant_id` (research R13).

**Done when.** Each filter is honoured, registered or refused, and a test pins each one's answer.

---

## 2. Request paths that log a failed node-local step and answer success

**Problem.** Constitution v1.5.0's Article II has a request either apply its node-local work or undo its change and answer `500`. Its migration plan names the paths that still log a failure and answer success:
- `CreateAutoAllocatedTopology` (`auto_allocated_topology.go:119`);
- network teardown (`network.go:1167`, `:1185`);
- `CreateSubnet` (`network.go:1515`), and the subnet delete and gateway-change paths (`:1884`, `:1890`, `:1943`, `:1951`);
- `DeleteRouter` (`router.go:445`), `AddRouterInterface` (`:541`, `:558`), `RemoveRouterInterface` (`:626`), and gateway set and clear (`:692`, `:799`);
- port forwarding create, update and delete (`port_forwarding.go:220`, `:361`, `:383`, `:447`);
- `CreatePort`'s eBPF and FDB steps (`ports.go:235`, `:276`);
- `DeletePort` and `ReleaseInstancePorts` teardown (`ports.go:546`, `:1887`, `:1899`; `teardownPortHost` `:1919`, `:1928`, `:1934`, `:1938`). SPEC-033 disassociates their floating IPs in the deleting transaction and removes the NAT after it. A failed removal is logged and left to reconciliation, because the rest of the teardown has no undo;
- `AllocatePortForInstance`'s FDB and DHCP steps (`ports.go:1726`, `:1735`);
- the LS-SG chain writes in `DeleteSecurityGroup` (`:1066`, after the commit since SPEC-033), `CreateSecurityGroupRule` (`:1263`) and `DeleteSecurityGroupRule` (`:1462`). Nothing jumps to these chains, so an undone rule add can leave its rule in one, without effect;
- `DeleteFloatingIP` (`floatingip.go:573`) takes the floating IP's port forwardings with it through the foreign key (migration 050) and leaves their NAT.

Network creation's undo (`network.go:631-637`) also deletes the row unconditionally, where Article II asks for compare-and-set.

**Proposal.** Bring each path in line with Article II as amended, or amend the constitution for it. For the LS-SG chains, decide between removing them and wiring them.

**Done when.** No path in the list logs a failed node-local step and answers success, or the constitution names the ones that may.

---

## 3. Deleting `default` as a non-administrator

**Problem.** Upstream refuses to delete a project's `default` group with `409` (`SecurityGroupCannotRemoveDefault`), even when no port carries it. Lightstack deletes it with `204` when no port carries it; since SPEC-033 a carried `default` answers `409 SecurityGroupInUse`. `test/security_groups_test.sh` Test 11 already expects a refusal. Today it skips itself, because the script reads `GROUPS`, a bash builtin (`baseline-gates.md`).

**Proposal.** Refuse it with `409 SecurityGroupCannotRemoveDefault` for a non-administrator, as upstream. Decide whether an administrator may.

**Done when.** The refusal is tested, and Test 11 runs and passes.

---

## 4. Floating IPs beyond their dataplane

**Problem.** Three divergences from upstream remain outside SPEC-033's scope:
- Two creates can race for one address. The address is chosen before the transaction, and the unique constraint turns the loser into a `500`.
- A port whose network has no router to the external network stores an association. Upstream answers `404` (`ExternalGatewayForFloatingIPNotFound`).
- Several floating IPs can reach one fixed IP. Upstream refuses that within one external network.

**Proposal.** Choose the address inside the create's transaction, or retry on the constraint. Refuse an association without a route, and a second floating IP for one fixed IP, with upstream's answers.

**Done when.** Each case is refused or resolved as upstream, and tested on both backends.
