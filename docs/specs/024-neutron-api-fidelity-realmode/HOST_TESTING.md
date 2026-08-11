# HOST_TESTING — SPEC-024 stub mode (Neutron API fidelity & real-mode networking)

Validates the six defects of [cloud-storage#546](https://github.com/cobaltcore-dev/cloud-storage/issues/546)
— B6, B9, B10, B11, F10, F11 — on a single Ubuntu host using the docker-compose stack, which runs with
`networking_mode: stub`. Everything except the real-mode halves of F10 and F11 is provable here.

The real-mode halves — the port's TAP and its bridge sharing a namespace, a guest reaching
`169.254.169.254`, guest egress through an external gateway, and Horizon's *Set Gateway* dropdown —
are in [HOST_TESTING_REAL_MODE.md](./HOST_TESTING_REAL_MODE.md).

**Run all commands from the repo root.** Paths below are repo-relative.

## Validates

| Acceptance criterion (ticket / spec) | Proved by |
|---|---|
| B10 — Neutron 404 returns `["NeutronError"]`, inner `type`/`message`/`detail`, no `code` | S1 |
| B10 — every failing path, not just 404s (401, unrouted 404, 405, malformed, panic) | S2 |
| B10 — `make test-errors` 10/10, exit 0; other services unchanged | S3 |
| F10 — `openstack port create` succeeds instead of 500 | S4 |
| B6 — `subnet list --network <A>` returns only A's subnets | S5 |
| B6 — a gatewayless subnet is listed, shown and updated | S6 |
| B9 — create response and read agree; a deliberate port `0` survives | S7 |
| B11 — admin creates into another project; non-admin refused `400` | S8 |
| F11 — `network list --external` returns the external networks | S9 |
| F11 — gateway/floating-IP validation, and cross-project read-only visibility | S10 |
| SC-011 — no previously passing check regressed | S11 |

---

## 0. Prerequisites

One paste. It installs only lightweight CLI tools and *verifies* the container engine and Go
toolchain, which are the host operator's setup.

```bash
sudo apt-get update
sudo apt-get install -y curl jq python3-openstackclient postgresql-client
command -v docker >/dev/null || command -v podman >/dev/null || echo "install docker or podman first"
command -v go >/dev/null || echo "install go 1.26+ first"
command -v golangci-lint >/dev/null || echo "golangci-lint missing: make install-tools"
echo "--- versions ---"
docker --version || podman --version
go version
jq --version
openstack --version
psql --version
```

**Pass =** every version line prints and no `install … first` line appears.

---

## 1. Setup steps

One stage, one paste. `docker-compose.yml` bakes `config/o3k.yaml`, so the stack runs in stub mode,
and `--build` picks up the branch under test. The exported variables are reused by every later step,
so keep this shell. B11 and F11 both need a project that is not the caller's, hence `hosttest-546`.

**Shell T1 (client)** — used for the whole run:

```bash
export O3K_JWT_SECRET=dev-secret-change-me
docker compose -f deployments/docker-compose.yml up -d --build
for i in $(seq 1 45); do curl -s -o /dev/null http://localhost:35357/v3 && break; sleep 2; done
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
export O3K_TEST_DB_URL='postgres://o3k:secret@localhost:5432/o3k?sslmode=disable'
export T=$(openstack token issue -f value -c id)
export P=00000000-0000-0000-0000-000000000002
echo "token length: ${#T}"
docker exec o3k-postgres psql -U o3k -d o3k -tAc "SELECT version, dirty FROM schema_migrations"
export OTHER=$(openstack project create --or-show hosttest-546 -f value -c id)
export NET_A=$(openstack network create ht-net-a -f value -c id)
export NET_B=$(openstack network create ht-net-b -f value -c id)
openstack subnet create ht-sub-a --network "$NET_A" --subnet-range 10.246.1.0/24 >/dev/null
openstack subnet create ht-sub-b --network "$NET_B" --subnet-range 10.246.2.0/24 >/dev/null
export SG=$(openstack security group create ht-sg -f value -c id)
echo "other=$OTHER net_a=$NET_A net_b=$NET_B sg=$SG"
```

**Pass =** `token length` is non-zero; the migration line reads **`66|f`**; and all four ids on the
last line are non-empty. A migration below 66 means `066_neutron_external_networks` did not apply and
every F11 step will fail for that reason rather than a real defect.

---

## 2. Test steps

### S1 — Neutron 404s use Neutron's envelope (B10, E1–E3)

The headline defect. Each resource must name its own condition, not one shared value.

```bash
for r in networks subnets ports routers floatingips security-groups; do printf '%-16s %s\n' "$r" "$(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/$r/00000000-0000-0000-0000-999999999999" | jq -c '[(keys|first), .NeutronError.type, (.NeutronError|has("code"))]')"; done
```

**Expect:** every line reads `["NeutronError","<Resource>NotFound",false]` — e.g.
`networks ["NeutronError","NetworkNotFound",false]`. `["itemNotFound",…]` anywhere is B10 unfixed.
The trailing `false` is the assertion that matters most: it proves the numeric `code` key is **absent**,
so this is not a wrapper renamed around the old body.

### S2 — Every failing path, not only the handlers (B10, E4–E7)

Auth, `NoRoute` and `NoMethod` answer outside any route group. A marker registered on the route group
would leave exactly these three wrong while S1 passed.

```bash
printf '%-14s %s\n' "401 no token" "$(curl -s http://localhost:9696/v2.0/networks | jq -c '[(keys|first), .NeutronError.type]')"
printf '%-14s %s\n' "404 unrouted" "$(curl -s -H "X-Auth-Token: $T" http://localhost:9696/v2.0/nope | jq -c '[(keys|first), .NeutronError.type]')"
printf '%-14s %s\n' "405 method" "$(curl -s -X DELETE -H "X-Auth-Token: $T" http://localhost:9696/v2.0/networks | jq -c '[(keys|first), .NeutronError.type]')"
printf '%-14s %s\n' "400 malformed" "$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{' http://localhost:9696/v2.0/networks | jq -c '[(keys|first), .NeutronError.type]')"
```

**Expect:** `["NeutronError","NotAuthorized"]`, `["NeutronError","NotFound"]`,
`["NeutronError","BadRequest"]`, `["NeutronError","BadRequest"]`. Any `unauthorized`, `itemNotFound`
or `badMethod` key means the envelope was applied per-group instead of per-engine.

### S3 — The committed gate, and the other services (B10, E10–E11, FR-010)

`make test-errors` has been red since `7b15418` (2026-03-07) and blocks `make test-all`.

```bash
make test-errors > /tmp/546-test-errors.log 2>&1; echo "exit code: $?"
grep -E 'Total Tests|Passed:|Failed:|ALL ERROR|SOME TESTS' /tmp/546-test-errors.log
curl -s -H "X-Auth-Token: $T" http://localhost:8774/v2.1/servers/00000000-0000-0000-0000-999999999999 | jq -c 'keys'
curl -s -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/volumes/00000000-0000-0000-0000-999999999999" | jq -c 'keys'
```

**Expect:** `exit code: 0`, then `Passed: 10` / `Failed: 0` / `ALL ERROR HANDLING TESTS PASSED`. Both
compute and block-storage still print `["itemNotFound"]` — this feature changes Neutron only.

The **exit code is the load-bearing part**: `test-errors` is a member of `test-all` (`Makefile:125`),
so a non-zero exit here is what has been failing the whole suite since `7b15418`. Redirecting to a
file rather than piping is deliberate — `$?` after a pipe reports the *last* command, and zsh spells
the per-stage array `$pipestatus[1]`, not bash's `${PIPESTATUS[0]}`.

### S4 — A port can be created, including on a pre-restart network (F10, P2–P4)

The stub half of F10 is process-local bridge bookkeeping, so it only reproduces once the service has
restarted since the network was created. Creating a port immediately would pass even unfixed.

```bash
docker compose -f deployments/docker-compose.yml restart o3k >/dev/null
for i in $(seq 1 45); do curl -s -o /dev/null http://localhost:35357/v3 && break; sleep 2; done
export T=$(openstack token issue -f value -c id)
openstack port create --network "$NET_A" ht-p1 -f value -c id -c mac_address -c fixed_ips
```

**Expect:** the port is created and prints an id, a MAC and a fixed IP from `10.246.1.0/24`. Before
the fix this was `500 failed to attach TAP to bridge` — the ticket's headline symptom.

### S5 — The subnet listing applies the filter (B6, S1–S4)

```bash
echo "unfiltered: $(openstack subnet list -f value -c Name | tr '\n' ' ')"
echo "network_id=A: $(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/subnets?network_id=$NET_A" | jq -r '[.subnets[].name] | join(" ")')"
echo "bogus filter: $(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/subnets?bogus=x" | jq '.subnets | length') vs unfiltered $(curl -s -H "X-Auth-Token: $T" http://localhost:9696/v2.0/subnets | jq '.subnets | length')"
```

**Expect:** the filtered line contains **only** `ht-sub-a` — never `ht-sub-b`. The two counts on the
last line are **equal**: an unrecognised parameter must leave the result unchanged rather than
becoming an accidental predicate.

### S6 — A gatewayless subnet is expressible and visible (B6, S9–S13)

`--gateway none` used to be silently overwritten with an invented address, which is why a NULL
gateway was unreachable through the API at all.

```bash
openstack subnet delete ht-nogw 2>/dev/null || true
openstack subnet create ht-nogw --network "$NET_A" --subnet-range 10.246.9.0/24 --gateway none -f value -c gateway_ip
echo "listed: $(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/subnets?network_id=$NET_A" | jq -c '[.subnets[] | {name, gateway_ip}]')"
openstack subnet show ht-nogw -f value -c gateway_ip -c name
openstack subnet set --dns-nameserver 1.1.1.1 ht-nogw && echo "update ok"
docker exec o3k-postgres psql -U o3k -d o3k -tAc "SELECT name, coalesce(gateway_ip,'NULL') FROM subnets WHERE name LIKE 'ht-nogw%'"
```

**Expect:** create prints `None`; the listing shows `ht-nogw` with `"gateway_ip":null` **present**
(before the fix the row vanished from the listing entirely), alongside `ht-sub-a` still carrying its
real `10.246.1.1` — the fix must not nullify every gateway; show and update both succeed (both used to
be `500`); and the stored value is literally `NULL`, proving the response and storage agree.

The leading `delete` makes the step re-runnable, and the update changes a **DNS server rather than the
name** so the subnet keeps a stable identity. An earlier version renamed it, which on a second run
left two subnets sharing one name — and `DeleteSubnet` resolves a name with `LIMIT 1`
(`internal/neutron/network.go:1542`), so teardown would have removed only one of them. O3K also does
not reject overlapping CIDRs on a network the way upstream does, so the duplicate produced no error to
notice.

### S7 — A rule reads back the way it was created (B9, G1–G6)

```bash
export RID=$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"security_group_rule\":{\"security_group_id\":\"$SG\",\"direction\":\"ingress\",\"ethertype\":\"IPv4\"}}" http://localhost:9696/v2.0/security-group-rules | jq -c '.security_group_rule | {id, remote_ip_prefix, remote_group_id, port_range_min, port_range_max, protocol}' | tee /dev/stderr | jq -r .id)
curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/security-group-rules/$RID" | jq -c '.security_group_rule | {remote_ip_prefix, remote_group_id, port_range_min, port_range_max, protocol}'
export RZ=$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"security_group_rule\":{\"security_group_id\":\"$SG\",\"direction\":\"ingress\",\"ethertype\":\"IPv4\",\"protocol\":\"icmp\",\"port_range_min\":0,\"port_range_max\":0}}" http://localhost:9696/v2.0/security-group-rules | jq -r '.security_group_rule.id')
docker exec o3k-postgres psql -U o3k -d o3k -tAc "SELECT coalesce(port_range_min::text,'NULL'), coalesce(port_range_max::text,'NULL') FROM security_group_rules WHERE id = '$RZ'"
```

**Expect:** the create response (first line) and the read (second line) are **identical**, with every
optional field `null` — not `""` and not `0`. The last line prints `0|0`, not `NULL|NULL`: a
deliberate port 0 is a valid ICMP type and must survive storage, which `Valid: portMin > 0` used to
discard.

### S8 — A requested project is honoured or refused, never ignored (B11, N1–N5)

```bash
export NID=$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"network\":{\"name\":\"ht-b11\",\"project_id\":\"$OTHER\"}}" http://localhost:9696/v2.0/networks | jq -r '.network.id')
echo "stored owner: $(docker exec o3k-postgres psql -U o3k -d o3k -tAc "SELECT project_id FROM networks WHERE id = '$NID'")"
echo "wanted:       $OTHER"
printf '%-22s %s\n' "both spellings differ" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"network\":{\"name\":\"ht-x\",\"project_id\":\"$OTHER\",\"tenant_id\":\"$P\"}}" http://localhost:9696/v2.0/networks)"
printf '%-22s %s\n' "absent project" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"ht-y","project_id":"11111111-2222-3333-4444-555555555555"}}' http://localhost:9696/v2.0/networks)"
printf '%-22s %s\n' "malformed project" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"ht-z","project_id":"not-a-uuid"}}' http://localhost:9696/v2.0/networks)"
```

**Expect:** `stored owner` equals `wanted` — the request took effect rather than landing in the
administrator's project. The three refusals are all **`400`**, never `201` and never `500`.

> The ticket's own probe adds `openstack network show ht-b11 -c project_id`. As an administrator that
> now returns **404**, because the network genuinely belongs to `$OTHER` and O3K has no administrator
> override on network read. That is a known, deliberate gap recorded in EVIDENCE.md — read the stored
> owner above instead.

### S9 — External networks exist and the filter is real (F11, X1, X5, X6)

```bash
export EXT=$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"ht-ext","router:external":true}}' http://localhost:9696/v2.0/networks | jq -r '.network.id')
openstack network list --external -f value -c Name
echo "external=true : $(curl -s -H "X-Auth-Token: $T" 'http://localhost:9696/v2.0/networks?router:external=true' | jq -c '[.networks[]."router:external"] | unique')"
echo "external=false: $(curl -s -H "X-Auth-Token: $T" 'http://localhost:9696/v2.0/networks?router:external=false' | jq -c '[.networks[]."router:external"] | unique')"
```

**Expect:** `ht-ext` appears in the `--external` listing (before the fix that listing was
*unconditionally empty*, which is why Horizon's Set Gateway dropdown never populated). The two filter
lines print `[true]` and `[false]` respectively — complements, not one of them always empty.

### S10 — External is enforced, and visibility grants nothing (F11, X8–X12)

```bash
printf '%-26s %s\n' "gateway on non-external" "$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"router\":{\"name\":\"ht-r-bad\",\"external_gateway_info\":{\"network_id\":\"$NET_B\"}}}" http://localhost:9696/v2.0/routers | jq -c '[(keys|first), .NeutronError.type]')"
printf '%-26s %s\n' "gateway on external" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"router\":{\"name\":\"ht-r-good\",\"external_gateway_info\":{\"network_id\":\"$EXT\"}}}" http://localhost:9696/v2.0/routers)"
printf '%-26s %s\n' "fip on non-external" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"floatingip\":{\"floating_network_id\":\"$NET_B\"}}" http://localhost:9696/v2.0/floatingips)"
printf '%-26s %s\n' "rename foreign network" "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"hijacked"}}' http://localhost:9696/v2.0/networks/$NID)"
```

**Expect:** the non-external gateway is refused in Neutron's envelope
(`["NeutronError","BadRequest"]`); the external one is `201`; the floating IP on a non-external
network is a `4xx`; and renaming the network owned by `$OTHER` is **`404`** — visibility confers no
authority, and a write that matches zero rows must not report success.

### S11 — Nothing that used to pass regressed (SC-011)

The five contract tests the ticket names on 2026-08-12 are included here. `-p 1` matters: the suites
share one Postgres and flake when run concurrently.

Two hazards are worked around here, and both are pre-existing rather than anything this feature did.
`TestLoadConfig` asserts the value in `config/o3k.yaml`, so the `O3K_JWT_SECRET` this runbook exports
in step 1 would fail it — hence `env -u`. And `./internal/keystone/...` deliberately rotates the
seeded admin password (`bootstrap_test.go:91`, proving bootstrap does not clobber a rotated one)
without restoring it, so it must be reset before the contract suites authenticate.

```bash
printf "UPDATE users SET password_hash = '\$2a\$10\$/DyBs44Yot8zgjC4j.n03Ox/QG94ftrksEXKKvadKcixLbGll0dR2' WHERE name = 'admin';\n" > /tmp/546-reset-admin.sql
env -u O3K_JWT_SECRET -u O3K_DB_URL go test -count=1 -p 1 ./internal/... ./pkg/... 2>&1 | grep -Ev '^ok|no test files' | head -5
docker exec -i o3k-postgres psql -U o3k -d o3k < /tmp/546-reset-admin.sql
go test -count=1 -run 'TestNeutronPortCreate_Contract|TestNeutronNetworkLifecycle_Contract|TestPortForwardingLifecycle_Contract|TestPortForwardingValidation_Contract|TestPortTopologyData_Contract' ./test/contract/neutron/ 2>&1 | tail -3
for pkg in contract contract/cinder contract/glance contract/keystone contract/neutron contract/nova; do docker exec -i o3k-postgres psql -U o3k -d o3k < /tmp/546-reset-admin.sql > /dev/null; printf '%-24s ' "$pkg"; go test -count=1 ./test/$pkg/ 2>&1 | grep -E '^(ok|FAIL)' | head -1; done
golangci-lint run ./... 2>&1 | tail -2
```

**Expect:** the unit line prints **nothing** (no failures); the five named tests report `ok`; all six
contract suites `ok`; `0 issues`.

The reset runs before **every** contract package, not once, because
`test/contract/keystone/password_test.go` rotates the password again mid-suite — so a single reset at
the top leaves everything after keystone failing to authenticate.

---

## 3. Teardown steps

Removes only what this run created, then the stack.

```bash
openstack router delete ht-r-good 2>/dev/null || true
openstack floating ip list -f value -c ID | xargs -r openstack floating ip delete 2>/dev/null || true
openstack port delete ht-p1 2>/dev/null || true
openstack subnet delete ht-sub-a ht-sub-b ht-nogw 2>/dev/null || true
docker exec o3k-postgres psql -U o3k -d o3k -tAc "DELETE FROM subnets WHERE name LIKE 'ht-nogw%'" 2>/dev/null || true
openstack network delete ht-net-a ht-net-b ht-ext ht-b11 2>/dev/null || true
openstack security group delete ht-sg 2>/dev/null || true
openstack project delete hosttest-546 2>/dev/null || true
docker compose -f deployments/docker-compose.yml down -v
```

**Pass =** `docker ps` shows no `o3k*` containers. The `-v` also drops the Postgres volume, so the
next run starts from a clean schema.

---

## Troubleshooting

**Every suite suddenly fails to authenticate.** Two different things rotate the seeded admin password
against the shared database and neither restores it: `internal/keystone/bootstrap_test.go:91` (a unit
test, so this bites even before any contract suite runs) and
`test/contract/keystone/password_test.go`. Restore the seeded hash and re-run:

```bash
printf "UPDATE users SET password_hash = '\$2a\$10\$/DyBs44Yot8zgjC4j.n03Ox/QG94ftrksEXKKvadKcixLbGll0dR2' WHERE name = 'admin';\n" > /tmp/reset-admin.sql
docker exec -i o3k-postgres psql -U o3k -d o3k < /tmp/reset-admin.sql
export T=$(openstack token issue -f value -c id)
```

**A port create still returns `500 failed to attach TAP to bridge`.** Check that the API you are
talking to is the build under test — an orphaned container process can survive `compose down` and keep
holding the ports, serving an old binary. `pgrep -x o3k` (exact name; `pgrep -f` will match your own
shell) and the response body tell you: the old binary answers `{"computeFault":…}`, which the current
code cannot produce.

**`TestTargetInstance_*` fails with `duplicate key … projects_name_key`.** Leftover `spec023-*`
fixtures from an interrupted run of another feature. Delete those projects and re-run.

**Migration version is not 66.** `066_neutron_external_networks` did not apply. Check
`docker logs o3k` for the migrate step; every F11 step depends on the `networks.external` column.
