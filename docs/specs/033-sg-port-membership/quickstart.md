# Quickstart: verifying SPEC-033

**Feature**: `033-sg-port-membership` (cloud-storage#579) | **Date**: 2026-11-17

An outline of how the result is checked: the gates, a stub-mode walk through the API, then the real-mode host validation of FR-023. The step-by-step host runbook (paste-safe, with teardown) is generated separately into `HOST_TESTING.md`.

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
- The membership, report, attach, port API, listing-filter, failure-and-undo and node-lock tests pass on both backends. So do the floating-IP, rule, group-delete and reconciliation tests.
- The restore payload, canonical-form and NAT tests pass.
- The existing suites stay green (SC-008).

## 1. Stub-mode walk (the Docker Compose stack, any host)

Check each row with `openstack port show <port> -c security_groups` and `openstack server show <server> -c security_groups`. The server is created with `--network` so it has a port.

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
| Another project's group id on a port or a server action, as an administrator | `404` (FR-004, FR-018) |
| `security group delete web` while a port carries `web` | `409`; the port keeps `web` (FR-008c) |
| `curl -X DELETE …/v2.0/security-groups/<an id of no group>` | `404` (FR-008c) |

## 2. Host validation (FR-023, research R18)

One real-mode node (libvirt, root, a routed floating IP, and iptables' `comment` match: `iptables -m comment --help` succeeds, and with the legacy backend `modinfo xt_comment` finds the module). A cirros guest with a floating IP and two listeners, for example `nc -lk -p 8080` and `nc -lk -p 8081`. Group `web` admits tcp/8080 and tcp/22; no group admits 8081. Steps (g) and (h) also need a second guest on the same network.

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

Afterwards: delete the guests, the floating IPs and the groups. `iptables -S | grep LS-NSI` and `iptables -t nat -S | grep lightstack-fip` list nothing for them.
