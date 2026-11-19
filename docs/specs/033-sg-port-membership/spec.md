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

**Independent Test**: Stub mode with a recording dataplane. Associate, move, disassociate and delete a floating IP, add and delete a rule, and delete a port or a server that has a floating IP: the dataplane follows before each answer. A failing dataplane gives `500` with the records unchanged. A reconciliation pass removes NAT and filters that no record holds. Deleting a group in use gives `409`. Real mode: quickstart §2 (g) and (h).

**Acceptance Scenarios**:

1. **Given** a port in `web` with a floating IP, in real mode, **When** a rule admitting tcp/8080 is added to `web`, **Then** the answer is `201` and a new connection to 8080 succeeds, with no restart. **When** the rule is deleted, **Then** the answer is `204` and new connections to 8080 are dropped.
2. **Given** a floating IP, **When** it is associated with a port, on create or on update, **Then** before the answer the port's filter is in place and then the floating IP's NAT.
3. **Given** an associated floating IP, **When** it is moved to another port, **Then** before the answer its NAT points at the new port only and the new port's filter is in place. The old port's filter is gone, unless something on the node still forwards to that address (FR-008a).
4. **Given** an associated floating IP, **When** it is disassociated or deleted, **Then** before the answer the node forwards no traffic to it: its address and NAT are gone first, then the filter.
5. **Given** a real-mode host where the NAT or the filter cannot be applied, **When** a floating IP is created on a port, associated, moved, disassociated or deleted, or a rule is added or deleted, **Then** the answer is `500` and the records are as they were before the request.
6. **Given** a floating IP deleted through one node, **When** every other node has run its next reconciliation pass, **Then** none of them forwards traffic to it.
7. **Given** a group that a port carries, **When** it is deleted, **Then** the answer is `409` and the port keeps the group. **Given** an id that names no group of the caller's project, **When** it is deleted, **Then** the answer is `404`.
8. **Given** a server whose port has a floating IP, **When** the server is deleted or force-deleted, or the port is deleted, **Then** before the answer the floating IP has no port, fixed IP or router, its status is `DOWN`, and the node no longer forwards it.

---

### Edge Cases

- **A server with no port**, created without a network or still booting: add and remove answer `404`, as upstream; the server reports no group.
- **Several ports**: one change rewrites every affected port in one step. No request leaves some ports changed and others not.
- **Ports with different groups** (one attached by id, or changed through the port API): add puts the group on every port. If any port already carries it, the answer is `400` and nothing changes; upstream answers `400` too, after changing the ports it reached first. Remove takes it off every port that carries it, and answers `404` only when none does.
- **The report's shape**: one entry per port and group, as upstream. A server with two ports in `default` reports `default` twice. The order is not specified.
- **The filter or the NAT cannot be applied** (an iptables error, no uplink): the old filter and NAT stay whole (FR-007a), the request answers `500`, and its own change is undone (FR-008). This holds for membership, rule and floating-IP changes alike. A reconciliation pass on a real-mode node without an uplink logs an error and skips.
- **A port or a server deleted while a floating IP is associated with it**, `forceDelete` included (FR-008e): the floating IP is disassociated in the same step, and its NAT and filter go before the answer. If their removal fails, the deletion stands and reconciliation removes them within one pass. Until then the freed fixed IP can still receive the floating IP's traffic. That window is the part of port teardown left as a recorded departure (FR-008d).
- **A floating IP deleted while the node cannot withdraw it**: `500`, and it is back, with its association and its port forwardings. If another request deleted its port meanwhile, it comes back unassociated. If its network went, or its address was allocated again, it stays deleted (FR-008).
- **A floating IP created on a port whose NAT fails**: `500`, and the floating IP is removed again. If another request changed it in the moment between, it stays with that change, as any later change does (FR-008).
- **Two floating IPs on one fixed IP**: the address's filter stays until the last of them is disassociated or deleted.
- **NAT rules from an earlier release**: they carry no Lightstack mark. For a floating IP that still exists, the first pass after the upgrade replaces its unmarked rules with marked ones. For one deleted before the upgrade, Lightstack cannot tell its rules from the operator's own. It leaves them, and the filter they reach; `docs/UPGRADING.md` says how to find them. Once such an address is allocated again, its own NAT step removes them (FR-010).
- **Another change to the same ports while a failed one is undone**: only the failed request's own change is undone. The other change stays.
- **A filter that already matches its stored list**: a reconciliation pass leaves it untouched, so the periodic passes open no window.
- **A slow firewall**: a firewall or NAT command that takes longer than one second, or a wait of more than one second for another change to the same node's floating-IP state, is a failed change (FR-007b), with FR-008's answer.
- **Multi-node**: the request applies its change on the node that serves it. Every other node's reconciler picks the change up within 30 seconds, removals included (FR-010).
- **Established connections**: removing a group stops new connections. A connection already open keeps its tracked state until it ends, as when a rule is removed today.
- **A floating IP associated after the change**: built from the port's list at association, as today.
- **A port without a floating IP**: nothing north-south to rebuild. Its list is right for when one is associated.
- **A duplicate add**: `400`, as upstream. **Removing a group no port carries**: `404`. **An unknown group**: `404`.
- **An id that is also another group's name**: the id wins, as at create. Names are unique per project, so no other ambiguity exists.
- **An administrator acting on another project's server**: the administrative override applies as today. A name or an id resolves in the server's project (register entry 5).
- **A group of another project**, named by anyone, administrators included, through either API: `404` (register entry 5). No port carries a group of another project.
- **A port of another project through the port API**: `404`, as today.
- **A group listing filtered by an id or a name that matches nothing**, another project's group included: an empty list, as upstream.
- **eBPF filter active** (not a stock build): out of scope. Ports Nova allocates carry no eBPF rule set. A port created through the Networking API keeps the eBPF rules it was created with after its groups change.
- **Stub mode**: lists are stored and reported. Nothing is programmed.
- **A group deleted while in use**: `409`, as upstream, and nothing changes (FR-008c). Terraform retries the delete until the ports carrying the group are gone.

## Requirements *(mandatory)*

### Functional Requirements

**Membership changes through the compute API**

- **FR-001**: `addSecurityGroup` MUST put the group on every port of the server, as one change. If any port already carries it, the answer MUST be `400` and nothing changes.
- **FR-002**: `removeSecurityGroup` MUST take the group off every port that carries it, as one change, and answer `404` when no port does.
- **FR-003**: Both MUST answer `404` for a server with no port, as upstream.
- **FR-004**: Both MUST resolve the group by name or by id in the server's project, an id match winning, as server create does, Lightstack's and upstream's. For an administrator acting on another project's server this is a recorded divergence (constitution register, entry 5). Upstream's add and remove resolve a name in the caller's own project and accept, by id, any group an administrator can see.
- **FR-005**: Removing a server's last group MUST answer `202` and leave its ports with no group. The `cannot remove last security group` refusal MUST go.
- **FR-006**: An unknown group or server MUST keep answering `404`.

**Enforcement**

- **FR-007**: After any change to a port's list, through the compute API or the Networking API, that port's floating-IP ingress filter MUST be rebuilt from the new list before the request returns, with no restart. A port left with no group MUST get the empty filter: replies to its own connections, nothing else. That port is rebuilt too, although no group links it any more.
- **FR-007a**: A filter MUST be replaced in one atomic step: at every moment it holds either the complete old rule set or the complete new one, each ending in its `DROP`. A rebuild that fails MUST leave the old filter whole. This holds for every rebuild — floating-IP association, rule changes, membership changes and reconciliation. A floating IP's NAT MUST likewise change in one step: it points at the old fixed IP or the new one, never at both or at neither while it stays associated.
- **FR-007b**: Every firewall and NAT command a change runs on the request path MUST be bounded at one second: reading a chain, replacing it, ensuring or removing its jump, and adding or removing the NAT and the floating address. So MUST the wait for another change to the same node's floating-IP state. Exceeding either is a failed change (constitution Article III; v1.5.0's change-then-undo exception).
- **FR-008**: If the serving node cannot apply a stored change, the request MUST answer `500` and undo exactly its own change, so the stored records match the node again. The change may be to a port's groups, a group's rules or a floating IP's association. The undo is compare-and-set: a record is put back only if it still holds what this request left, and a change another request made meanwhile MUST NOT be undone. Concretely:
  - a membership change is put back;
  - an added rule is removed, and a deleted rule is restored;
  - a floating IP created on a port is removed;
  - a moved or disassociated floating IP gets its association back;
  - a deleted floating IP is restored, with the port forwardings deleted with it. Rows other requests deleted meanwhile are applied as their foreign keys would apply them, so a deleted port leaves it unassociated, and a deleted network or an address allocated again keeps it deleted.

  Whatever the request had already applied on the node MUST be applied back from the restored records; if that fails too, reconciliation converges it. The failure MUST be logged naming the port or the floating IP, and the address. This holds through both APIs.
- **FR-008a**: A floating IP's NAT and filter MUST be in force on the serving node before the answer to creating it on a port, associating it, moving it to another port, disassociating it and deleting it. Traffic to a floating IP is only ever forwarded through its port's filter:
  - when it is associated, the filter goes in before the NAT and the floating address;
  - when it is disassociated or deleted, the filter goes after them;
  - the filter stays while anything on the node still forwards to that fixed IP: another floating IP, or a rule of the shape (Key Entities) that Lightstack cannot tell from an operator's.

  When the node cannot do this, FR-008 applies. A floating IP without a port needs nothing on the node.
- **FR-008b**: Adding or deleting a rule MUST rebuild, before the answer, the filter of every port on the serving node that carries the rule's group and has a floating IP. When a rebuild fails, FR-008 applies.
- **FR-008c**: Deleting a group that any port carries MUST answer `409` (`SecurityGroupInUse`) and change nothing, as upstream. Deleting an id that names no group of the caller's project MUST answer `404`, as upstream. A group's deletion never changes a port's list.
- **FR-008d**: Deleting a port, directly or with its server, MUST disassociate the floating IPs that name it in the same step, as upstream: no port, fixed IP or router. Their status MUST be `DOWN` in the same step too; upstream's L3 agent reports `DOWN` afterwards. Their NAT and filters MUST be gone from the serving node before the answer. A removal that fails cannot undo the deletion. It is logged naming the floating IP, and reconciliation completes it within one pass; the constitution's migration plan records this as part of port teardown.
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
- **FR-018**: Each id MUST name a group of the port's project; otherwise the answer is `404` and nothing changes. This holds for port create and update, and for administrators too. Upstream lets an administrator use any project's group; the difference is register entry 5. A repeated or malformed id MUST answer `400`, as upstream.
- **FR-019**: `POST /v2.0/ports` with `security_groups: []` or `null` MUST create a port with no group, as upstream. An absent field MUST keep giving `default`.
- **FR-019a**: `GET /v2.0/security-groups` MUST honour `id`, repeatable (`?id=a&id=b` lists every group named), and `name`, as upstream. An id or a name that matches no group of the caller's project lists nothing.

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
  - the floating-IP NAT is marked as Lightstack's. Rules left by earlier releases for floating IPs deleted before the upgrade are not removed automatically; the section gives the command that lists them.
- **FR-021**: The Horizon compatibility report MUST record instance *Edit Security Groups* as verified.
- **FR-021a**: The constitution's register of divergences MUST carry entry 5, added in v1.5.0. Divergence: a security group resolves only in the project it applies to, for administrators too. An administrator's `addSecurityGroup` / `removeSecurityGroup` on another project's server resolves a name or an id in the server's project. Port create and update accept only the port's project's groups. A group of any other project answers `404`. Upstream: add and remove resolve a name in the caller's own project and accept, by id, any group an administrator can see; Neutron lets an administrator put any project's group on a port. Why permitted: for names, administrators adding a tenant's group by name succeed today and upstream's rule would refuse them; for ids, Lightstack's groups are private to their project, even for administrators, since listing and showing are scoped to the caller's project, so "any group an administrator can see" has no counterpart in its data model. Recorded as: SPEC-033 FR-004, FR-018.
- **FR-022**: Every requirement above MUST have an automated test that fails without the change (the constitution's Test-First Development): the compute paths through the real network service in stub mode on the same test database, with a stand-in only where a failure must be injected; the network service against a real database (the empty list included); the contract suite through the public API; and the bash server-actions test through the CLI.
- **FR-023**: A real-mode host run MUST show, with no restart:
  - a rule on a non-default group opening and closing floating-IP traffic, through the compute API and through the Networking API, and removing the last group closing it;
  - a rule added to and deleted from a group opening and closing it;
  - a floating IP moved, disassociated and deleted, and a server deleted with its floating IP associated, closing at once on the serving node;
  - a marked NAT rule that no record holds removed within a pass, and an unmarked rule of an associated floating IP replaced by a marked one.

  It MUST also show that one new connection attempt per second to a closed port gets none through, across reconciliation passes and changes. The run is recorded in this feature's host-testing runbook. No CI substitute exists.

### Key Entities

- **A port's groups**: what is recorded on a port. What enforcement reads, and the only list.
- **A server's groups**: what the compute API reports for a server — the groups of its ports.
- **Floating-IP ingress filter**: one per fixed IP with a floating IP. Replies to the server's own connections, one allowance per ingress rule of the port's groups, then drop. Replaced as a whole, never edited in place.
- **Floating-IP NAT**: per associated floating IP, its address on the node's uplink and the rules that forward it to the fixed IP, marked as Lightstack's. Present exactly while a record associates the floating IP.
- **A rule of the shape**: a NAT rule that forwards one address to one fixed IP and does nothing else, the form Lightstack writes. It is marked when this release wrote it. It is unmarked when an earlier release wrote it, or when someone else wrote the same form. An unmarked one counts as Lightstack's only when its address has a floating-IP record or a marked rule.
- **Floating-IP record**: a floating IP's row, associated with a port or not.
- **Security group and rule**: unchanged, except that a group a port carries cannot be deleted.

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
- **SC-012**: An administrator adds a tenant's group to the tenant's server by name and by id, and both succeed. Naming a group of any other project answers `404`, through either API.
- **SC-013**: A rule added to or deleted from a group is in force through every floating IP of the ports carrying the group when the API answers.
- **SC-014**: A floating IP that is moved, disassociated or deleted, or whose port or server is deleted or force-deleted, forwards nothing to its old fixed IP from the serving node once the API has answered. The one exception is a port or server deletion whose removal failed, which takes up to one reconciliation pass. From every node it forwards nothing within one pass.
- **SC-015**: Deleting a group that a port carries answers `409`, and the port keeps the group.

## Assumptions

- The reconciliation interval stays 30 seconds and runs on every node. It bounds how long a node that did not serve a change takes to enforce it.
- A membership, rule or floating-IP change succeeds only once the serving node has applied it; otherwise it is undone (FR-008). Other nodes follow within one reconciliation pass, removals included. Both are constitution v1.5.0 exceptions to Article II:
  - "a change undone when its external work fails": the client gets the answer one transaction would have given;
  - "stateless reconciliation loops": other nodes, a request whose process stopped, and a node whose own undo failed.
- Removing a group does not cut connections already established. Lightstack does not do so for a removed rule either.
- In a stock build the eBPF filter never initialises. The host run covers the floating-IP filter, the only enforcement a stock build has.
- The port API keeps its project scoping: a caller changes its own project's ports.
- SPEC-009's documents stay as written: a finished feature's spec belongs to its own change. This spec records that SPEC-009's FR-016a now holds for every group, not only `default`.
- No new dependency and no new configuration. No migration is expected: ports already record their groups.

## Out of Scope

- Deleting `default` as a non-administrator, which upstream refuses with `409` even when no port carries it. Follow-up.
- The create response does not echo `security_groups`, as upstream's does.
- eBPF: re-programming a port's rule set when its groups change, rules re-applied after a restart, the 100-rules-per-port limit, and the empty object path. Ports Nova allocates have no eBPF rule set, eBPF never initialises in a stock build, and the manager cannot be tested without a seam.
- The `LS-SG-*` chains, except one ordering change: a group's deletion removes its chain after the commit instead of before the check, so a refused deletion leaves it alone.
- Cutting established connections when a group is removed.
- A port named in server create's `networks`, which Lightstack does not accept.
- Filters on the security-group listing other than `id` and `name`. Today they are ignored, a pre-existing silent divergence from upstream (Article I). This feature does not mandate that; it is on the follow-up list.
- Other request paths that change node-local state and log a failed step: networks, subnets and DHCP, routers with their interfaces and gateways, port forwarding, port creation's FDB and eBPF steps, port and instance-port teardown, and the LS-SG chain writes in group and rule handlers. Network creation's undo is not compare-and-set. A floating IP's deletion takes its port forwardings with it and leaves their NAT, as it does today. Port teardown's floating IPs are the exception this feature handles (FR-008d); the rest of the teardown is not. All are recorded as departures in the constitution's v1.5.0 migration plan and listed in the follow-ups.
- Floating-IP behaviour beyond its dataplane: two creates racing for one address, the router lookup, and several floating IPs on one fixed IP, which upstream refuses within one external network.

## Dependencies

- SPEC-017 (cloud-storage#524): records and reports a server's groups and fixed the create path.
- SPEC-009: the floating-IP filter and the reconciler.
- A real-mode host for FR-023: libvirt, root, and a routed floating IP.
- A Horizon to verify SC-006: the Docker Compose stack or the verification chart.
