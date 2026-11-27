# Quickstart: verifying SPEC-033

**Feature**: `033-sg-port-membership` (cloud-storage#579) | **Date**: 2026-11-17

An outline of how the result is checked: the gates, a stub-mode walk through the API, then the real-mode host validation
of FR-023. The step-by-step host runbook (paste-safe, with teardown) is generated separately into `HOST_TESTING.md`.

## 0. Gates

```bash
make build && make lint
make db-up
LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable' \
  go test ./internal/nova/... ./internal/neutron/... ./pkg/networking/...
LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db' \
  go test ./internal/nova/... ./internal/neutron/...
# contract suite: against a running stub Lightstack, once on PostgreSQL and once on SQLite
make test-contract
```

Expected:
- The membership, report, attach, port API, listing-filter, failure-and-undo and node-lock tests pass on both backends.
  So do the floating-IP, rule, group-delete and reconciliation tests.
- The restore payload, canonical-form and NAT tests pass.
- The existing suites stay green (SC-008).

## 1. Stub-mode walk (the Docker Compose stack, any host)

Check each row with `openstack port show <port> -c security_groups` and `openstack server show <server> -c
security_groups`. The server is created with `--network` so it has a port.

| Do | Expect |
|---|---|
| `server create --security-group web …` | The port lists `web` only; the server reports `web` (FR-011) |
| `server add security group <server> db` | `202`; the port lists `web` and `db` (FR-001) |
| The same with `db`'s id | Same outcome (FR-004) |
| `server remove security group <server> web`, then `… db` | `202` both times; the port lists none; `security_groups` is absent from the server (FR-002, FR-005, FR-014) |
| `server add security group` on a server created without a network | `404` (FR-003) |
| `server add network <server> <net>` | The new port lists `default`; the server reports its ports' groups (FR-013) |
| `port set --security-group web <port>`, then `port set --no-security-group <port>` | The list follows both times; the server's report follows (FR-017) |
| `port create --no-security-group …` | A port with no group (FR-019) |
| `curl …/v2.0/security-groups?id=<a>&id=<b>` and `?name=web` | Only those groups (FR-019a) |
| Another project's group id on a port or a server action, as an administrator | round 1: `404`; round 2: allowed, as upstream (FR-004, FR-018) |
| `security group delete web` while a port carries `web` | `409`; the port keeps `web` (FR-008c) |
| `curl -X DELETE …/v2.0/security-groups/<an id of no group>` | `404` (FR-008c) |

## 2. Host validation (FR-023, research R18)

> **Superseded by §5** (round 2): the `LS-NSI-*` filters and host-namespace NAT this section checks are retired (FR-042,
> FR-044). FR-023's checks are restated in §5 (i).

One real-mode node (libvirt, root, a routed floating IP, and iptables' `comment` match: `iptables -m comment --help`
succeeds, and with the legacy backend `modinfo xt_comment` finds the module). A cirros guest with a floating IP and two
listeners, for example `nc -lk -p 8080` and `nc -lk -p 8081`. Group `web` admits tcp/8080 and tcp/22; no group admits 8081. Steps (g) and (h) also need a second guest on the same network.

| Step | Do | Expect |
|---|---|---|
| (a) compute API | Add `web`, connect to 8080; remove `web`, connect again | Connected; then timed out. No restart (SC-002) |
| (b) Networking API | `port set --security-group web`, then remove it from the list | Same as (a) |
| (c) last group | Open an ssh session while `web` is on the port, then remove every group | New connections time out; the open ssh session stays up, since the filter admits established connections (SC-003) |
| (d) no window | One connection attempt per second to 8081 for 2 minutes, while (a) runs repeatedly | None connects. A "refused" would mean a packet passed the filter; only timeouts are allowed (SC-010) |
| (e) unchanged filters | `iptables -L LS-NSI-<ip> -v -n -x` before and after a pass with no change | The counters keep growing, so the filter was not rebuilt (FR-010) |
| (f) Horizon | Instance → *Edit Security Groups* | Only the instance's groups are pre-selected; after saving, the port carries the chosen ones (SC-006) |
| (g) rules and floating IPs | Add a rule admitting tcp/8081 to `web`, connect to 8081, delete the rule, connect again. Then move the floating IP to the second guest, disassociate it, and delete it, checking after each step | 8081 connects, then times out (SC-013). After the move, the first guest no longer answers through the floating IP, and `iptables -t nat -S PREROUTING` shows one marked rule for it, pointing at the second guest. After the disassociation and the delete, nothing answers, and its address, its NAT rules and both old filters are gone (SC-014). Then associate a floating IP with the second guest again and delete the guest: the floating IP shows no fixed IP and status `DOWN`, and its NAT is gone at once (FR-008d) |
| (h) removal by reconciliation | By hand: add a marked NAT rule for an address no record holds; replace an associated floating IP's marked rule with an unmarked one of the earlier shape; add an unmarked rule of the shape for an address no record holds, pointing at a fixed IP whose floating IP you then delete; remove an associated floating address from the uplink. Wait one pass (30 s) | The marked rule without a record is gone. The associated floating IP has its marked rule back, and the unmarked one is gone. The other unmarked rule and the filter it reaches are both still there. The removed address is back (FR-010) |

Afterwards: delete the guests, the floating IPs and the groups. `iptables -S | grep LS-NSI` and `iptables -t nat -S |
grep lightstack-fip` list nothing for them.

---

## Round 2 (US8–US12, FR-019a–FR-058)

### 3. Gates, added

```bash
LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable' \
  go test -count=1 -p 1 ./internal/database/... ./internal/neutron/... ./internal/nova/... ./internal/common/... ./internal/health/... ./pkg/networking/... ./cmd/lightstack/...
LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db' \
  go test -count=1 -p 1 ./internal/database/... ./internal/neutron/... ./internal/nova/...
helm unittest charts/lightstack
./test/security_groups_test.sh        # Test 11 now runs and passes
```

Migrations 084 and 085 are covered by `TestSchemaParity` and `TestMigrationsRoundTrip` in `internal/database`, and what
they mean to the code by `migration_084_test.go`.

### 4. Stub-mode walk (the API answers)

Against the Compose stack, as an administrator and as a member of another project:
1. `openstack security group list --description http`, `--tags x` (empty); `curl …/security-groups?revision_number=0`
   lists the unchanged groups.
2. `openstack security group delete default` as a member → `409 SecurityGroupCannotRemoveDefault`; `openstack security
   group set --name x default` → `409`; `openstack security group create default` → `409`; a second group with an
   existing name succeeds, and adding that name to a server then answers `409`. A new group shows two egress rules.
3. `openstack subnet create --allocation-pool start=…,end=…`: read back; a pool holding the gateway → `409`.
4. `openstack security group create --stateless s`: shows `stateful: false` and `revision_number: 0`; adding a rule
   moves the revision to 1; a port carrying `default` and `s` → `409`.
5. A router with a gateway: `openstack router show` lists `external_fixed_ips`; removing the interface or clearing the
   gateway while a floating IP uses it → `409`.
6. Ten `openstack floating ip create` in parallel (`xargs -P 10`): ten distinct addresses from the pool; a floating IP
   on a port no router reaches → `404`; a second one on the same fixed IP → `409`.

### 5. Host validation (FR-048, research R45)

`HOST_TESTING.md`, generated by `/cl.hosttest` (T165, replacing T053), holds one run:
- **Single node, the chart on Kubernetes**: FR-023's checks as restated — a rule on a non-default group opening and
  closing floating-IP traffic and traffic from another VM, through the compute API and the Networking API, the last
  group's removal closing it; a rule added and deleted; a floating IP moved, disassociated and deleted, and a server
  deleted with one, closing at once; a router-namespace NAT rule no record holds removed within a pass; one connection
  attempt per second to a closed port getting none through across passes and changes (SC-010). Then router namespace and
  legs, DHCP and metadata through the DHCP address, the gateway address and SNAT, a floating IP and a port forwarding,
  the port firewall between VMs, through floating IPs and for egress, a deletion of each kind, FR-039's refusals, the
  pod deleted with an established connection surviving, readiness failing while `br_netfilter` cannot be loaded, and
  Horizon's instance *Edit Security Groups* pre-selecting an instance's groups and changing its ports (FR-021, SC-006).
- **Two nodes**: not part of this feature; Lightstack does not support two nodes yet.
