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

**Follow-up findings (2026-08-13) — not part of #546.** Found during the real-mode host pass and
fixed on this branch as a separate commit. Each step below carries the context it needs.

| Acceptance criterion | Proved by |
|---|---|
| Finding 1 — a short network id is refused, not a panic to `500` | S12 |
| Finding 2 — `dhcp_agent_scheduler` is advertised and its endpoint answers | S13 |

Finding 1's substance — metadata DNAT rules being reclaimed — is **mode-gated and cannot be proved
here**. `MetadataRedirect` is a no-op in stub mode, so a stub stack writes no iptables rules to leak.
That half is [HOST_TESTING_REAL_MODE.md](./HOST_TESTING_REAL_MODE.md) R10.

**Later branch commits.** Three commits landed after the tables above were written, and all three
change behaviour a client can see. S14–S18 cover them.

| Acceptance criterion | Proved by |
|---|---|
| `refactor(sql)` — a page walk returns rows that share a `created_at`, once each | S14 |
| `fix(neutron)` — `routers/{id}/l3-agents` is scoped, and cannot probe for foreign routers | S15 |
| Branch review SRC-3/SRC-4 — a gateway is validated, and discloses no invisible network | S16 |
| Finding 1 (ports) — a short or unusable port id is refused, not a crash | S17 |
| Branch review SRC-1 — a cached network is not served to a caller who may not see it | S18 |

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

The **exit code is the load-bearing part**: `test-errors` is a member of `test-all` (`Makefile:142`),
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
(`internal/neutron/network.go:1629`), so teardown would have removed only one of them. O3K also does
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
printf '%-26s %s\n' "admin renames foreign net" "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"ht-b11-renamed"}}' http://localhost:9696/v2.0/networks/$NID)"
printf '%-26s %s\n' "and it really applied" "$(docker exec o3k-postgres psql -U o3k -d o3k -tAc "SELECT name FROM networks WHERE id = '$NID'" | tr -d ' ')"
curl -s -o /dev/null -X PUT -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"ht-b11"}}' http://localhost:9696/v2.0/networks/$NID
```

**Expect:** the non-external gateway is refused in Neutron's envelope
(`["NeutronError","BadRequest"]`); the external one is `201`; the floating IP on a non-external
network is a `4xx`; the rename is **`200`** and `and it really applied` reads `ht-b11-renamed`. The
third line renames it back, so the name teardown deletes by is restored.

**The rename is `200` on purpose, and the read-back is the assertion.** `UpdateNetwork` exempts an
administrator from the owner predicate (`internal/neutron/network.go:1129`), which is upstream
Neutron's `admin_or_owner` default for `update_network`. It was added for research R6.4: owner-scoped,
an administrator marking a **tenant's** network external matched zero rows and was still answered
`200` — the same dishonest success B11 is about, inside the fix for a different defect. So what has to
be checked here is not refusal but that the write *landed*: a `200` with the old name still stored is
the defect returning.

The mirror contract — a **non-owner** must not rename a network they can merely see — is real and is
X9 in `internal/neutron/external_networks_test.go:291`, which uses a tenant header. It cannot be run
here: this runbook holds only an administrator token until S18, and an earlier version of this step
asserted X9's `404` against that admin token, which the exemption makes unreachable. S18 runs it with
a second caller.

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

S11's unit line also covers the follow-up findings below: their tests live in `internal/neutron` and
`pkg/networking`, both inside `./internal/... ./pkg/...`.

---

## 2b. Follow-up findings (2026-08-13, not #546)

Both were found during the real-mode host pass and fixed as a separate commit. Run these after S11;
neither creates a fixture the teardown does not already remove.

### S12 — A short network id is refused, not a crash (finding 1)

`DeleteNetwork` derives the bridge name by slicing the first 8 characters of the path parameter. It
did that **before** checking anything, so an id shorter than 8 characters sliced out of range, and
the panic-recovery middleware turned a request that should simply match no row into a `500`.

```bash
for id in foo ab 1234567 12345678 00000000-0000-0000-0000-999999999999; do printf '%-46s %s\n' "DELETE /networks/$id" "$(curl -s -H "X-Auth-Token: $T" -o /tmp/546-del-$id.json -w '%{http_code}' -X DELETE "http://localhost:9696/v2.0/networks/$id") $(jq -rc '[(keys|first), .NeutronError.type]' /tmp/546-del-$id.json 2>/dev/null || echo '<empty body>')"; done
docker logs --since 5m o3k 2>&1 | grep -c 'slice bounds out of range' || true
```

**Expect:** every line is `404 ["NeutronError","NetworkNotFound"]`. None is `500`, and none prints
`<empty body>`.

Both halves of each line matter. When this step was written `RecoveryMiddleware` ended in
`c.AbortWithStatus(500)`, which writes **no body at all** — so before the fix these requests returned
a bodiless `500` that also silently escaped B10's envelope guarantee, and the status code alone would
not have distinguished that from an ordinary internal error. That middleware now renders through
`common.SendError` (branch review SRC-2), so a panic carries an envelope too; the body assertion here
still earns its place, because it is what distinguishes "matched no row" from "crashed and recovered".

The log count is the independent check: a recovered panic still logs `panic recovered` with the
runtime error, so a `404` reached by crashing on some other path would show up here even when the
status looks right. **`0`** is the pass. The grep is deliberately the exact runtime message rather
than `panic`, because S2 and `make test-errors` exercise the panic path on purpose and a broader
pattern would match those.

The 8-character boundary is what the loop probes: `1234567` is the longest id that used to panic and
`12345678` the shortest that never did, so a fix that guarded the wrong comparison shows up as one
line out of place rather than passing by luck.

### S13 — The DHCP-agent-scheduler alias and its endpoint (finding 2)

Horizon's admin Network list assigns its **Project** column inside a block guarded by
DHCP-agent-scheduler support — an unrelated capability that happens to share the loop — so without the
alias every row rendered `-` while the API returned the right owner. The alias alone would have made
it *worse*: the same block fetches each network's agents, so a missing endpoint raises an error pop-up
per row. Both halves are asserted here; the column itself needs Horizon (real-mode R11).

`$NID` comes from S8 and is the network owned by `$OTHER`.

```bash
curl -s -H "X-Auth-Token: $T" http://localhost:9696/v2.0/extensions | jq -r '[.extensions[].alias] | sort | join(" ")'
printf '%-26s %s\n' "own network" "$(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/networks/$NET_A/dhcp-agents" | jq -c '[(.agents|length), (.agents|type)]')"
printf '%-26s %s\n' "external network" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/networks/$EXT/dhcp-agents")"
printf '%-26s %s\n' "unknown network" "$(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/networks/00000000-0000-0000-0000-999999999999/dhcp-agents" | jq -c '[(keys|first), .NeutronError.type]')"
printf '%-26s %s\n' "another project's" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/networks/$NID/dhcp-agents")"
echo "agents collection: $(curl -s -H "X-Auth-Token: $T" http://localhost:9696/v2.0/agents | jq -c '.agents | length')"
```

**Expect:** the alias list contains `dhcp_agent_scheduler` alongside the seven that were already there.
Then `own network` reads `[0,"array"]` — an **empty array**, not `null` and not a missing key, which is
what Horizon needs to render `0` rather than raise; `external network` is `200`, since visibility is
owner-or-shared-or-external; `unknown network` is `["NeutronError","NetworkNotFound"]` in Neutron's
envelope; `another project's` is **`404`**, so the endpoint cannot be used to probe for networks the
caller cannot see. The last line is `0`, which is why an empty list is the honest answer here —
`neutron_agents` is created by migration `038` and seeded by nothing.

An alias that is present while the endpoint 404s is the combination to watch for: it is worse than
neither, and it is exactly what advertising the capability alone would produce.

---

## 2c. Later branch commits

`refactor(sql)`, the l3-agents/DHCP fix, and two findings from the branch review all landed after
§2 and §2b were written. Run these after S13; S18 rebuilds the stack and must go last.

### S14 — A page walk returns tied rows, once each (marker paging)

Six listings resolved a pagination marker to `created_at < $n` — Cinder volumes and volumes/detail,
Glance images, Neutron floating IPs, routers and ports. `created_at` is not unique: rows written by
one seed, script or Terraform apply share it to the microsecond, so that predicate excluded every row
**tied** with the marker rather than only those before it, and a page walk dropped them without
saying so. Five of the six also ordered by `created_at` alone, which is not a total order, so the page
boundary was not stable even between two identical requests. Both halves now use `(created_at, id)`.

The tie is forced with an `UPDATE` deliberately. Resources created through the API get distinct
timestamps, so the defect does not reproduce on naturally-created rows — a walk over five ports made a
second apart passes **unfixed**, which is what makes this the one step here that needs the database.

```bash
for i in 1 2 3 4 5; do openstack port create --network "$NET_A" ht-pg-$i -f value -c id; done > /tmp/546-pg-ports.txt
docker exec o3k-postgres psql -U o3k -d o3k -tAc "UPDATE ports SET created_at = '2026-01-01 00:00:00+00' WHERE name LIKE 'ht-pg-%'"
: > /tmp/546-pg-walk.txt
M=""
for i in $(seq 1 10); do PAGE=$(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/ports?limit=2&marker=$M" | jq -r '.ports[].id'); test -z "$PAGE" && break; printf '%s\n' "$PAGE" >> /tmp/546-pg-walk.txt; M=$(printf '%s\n' "$PAGE" | tail -1); done
echo "seeded ports:     $(wc -l < /tmp/546-pg-ports.txt)"
echo "seen in the walk: $(grep -c -F -f /tmp/546-pg-ports.txt /tmp/546-pg-walk.txt)"
echo "duplicated rows:  $(sort /tmp/546-pg-walk.txt | uniq -d | wc -l)"
```

**Expect:** `seeded ports` and `seen in the walk` are both **5**, and `duplicated rows` is **0**.
Before the fix `seen` was **1** — the first page's marker carried the shared timestamp, and
`created_at < that` excluded the other four for the remainder of the walk.

Both halves of the assertion earn their place. Dropped rows are the reported defect; duplicates are
the failure mode a *non-total* order produces, and a fix that changed only the comparison operator
without adding `id` to the `ORDER BY` would trade one for the other.

Then the same defect in a second service, since the fix is one shared helper rather than six edits:

```bash
for i in 1 2 3; do openstack volume create --size 1 ht-vg-$i -f value -c id; done > /tmp/546-pg-vols.txt
docker exec o3k-postgres psql -U o3k -d o3k -tAc "UPDATE volumes SET created_at = '2026-01-01 00:00:00+00' WHERE name LIKE 'ht-vg-%'"
: > /tmp/546-vg-walk.txt
M=""
for i in $(seq 1 8); do PAGE=$(curl -s -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/volumes?limit=1&marker=$M" | jq -r '.volumes[].id'); test -z "$PAGE" && break; printf '%s\n' "$PAGE" >> /tmp/546-vg-walk.txt; M=$(printf '%s\n' "$PAGE" | tail -1); done
echo "seeded volumes:   $(wc -l < /tmp/546-pg-vols.txt)"
echo "seen in the walk: $(grep -c -F -f /tmp/546-pg-vols.txt /tmp/546-vg-walk.txt)"
```

**Expect:** both **3**.

The remaining four listings take the identical predicate from the same helper.
`internal/cinder/volume_paging_test.go` and `internal/glance/image_paging_test.go` seed the same tie
against them, and S11's unit line runs both.

### S15 — `l3-agents` is scoped to the caller

`GET` and `POST` on `/v2.0/routers/{id}/l3-agents` verified the router with
`SELECT EXISTS(... WHERE id = $1)` and no project predicate. Another project's router answered `200`
with an empty list; an id belonging to no router answered `404`. The pair distinguishes existence, so
the endpoint was a probe for routers the caller cannot see.

The `POST` matters more than the read: unscoped it would let a caller schedule an agent onto another
project's router. Nothing exploitable follows today — `neutron_agents` is never populated, so the
agent check refuses first — but that is incidental cover rather than a decision.

The foreign router is inserted directly. B11 lets an administrator name a project when creating a
**network**; `CreateRouter` has no such path, so there is no API route to a router owned by `$OTHER`.

```bash
export FR=$(docker exec o3k-postgres psql -U o3k -d o3k -tAc "INSERT INTO routers (name, project_id) VALUES ('ht-r-foreign', '$OTHER') RETURNING id" | head -1 | tr -d ' ')
export OWN=$(curl -s -H "X-Auth-Token: $T" http://localhost:9696/v2.0/routers | jq -r '.routers[] | select(.name=="ht-r-good") | .id')
echo "foreign=$FR own=$OWN"
printf '%-30s %s\n' "own router" "$(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/routers/$OWN/l3-agents" | jq -c '[(.agents|length), (.agents|type)]')"
printf '%-30s %s\n' "another project's router" "$(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/routers/$FR/l3-agents" | jq -c '[(keys|first), .NeutronError.type]')"
printf '%-30s %s\n' "unknown id" "$(curl -s -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/routers/00000000-0000-0000-0000-999999999999/l3-agents" | jq -c '[(keys|first), .NeutronError.type]')"
printf '%-30s %s\n' "malformed id" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $T" "http://localhost:9696/v2.0/routers/not-a-uuid/l3-agents")"
printf '%-30s %s\n' "POST to a foreign router" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"agent_id":"00000000-0000-0000-0000-000000000001"}' "http://localhost:9696/v2.0/routers/$FR/l3-agents")"
```

**Expect:** `own router` is `[0,"array"]` — an **empty array**, not `null`. The foreign router and the
unknown id both read `["NeutronError","RouterNotFound"]`: **the two must be indistinguishable**, which
is the whole assertion. `malformed id` is `404`, not `500` — the id is compared as text so it matches
nothing rather than erroring the query. The `POST` is `404`.

`ht-r-good` comes from S10. If S10 was skipped there is no own-router to read and that first line will
be empty rather than failing loudly — create one first.

The `head -1` is not decoration. `psql -tAc` prints the command status tag on its own line **after**
the returned row, so an `INSERT … RETURNING id` yields two lines and the capture becomes
`<uuid>\nINSERT01`. A `curl` URL built from that is malformed, and the symptom does not name the
cause: the request either returns an empty body or `000`, which reads like the endpoint failing rather
than the variable being wrong. `SELECT` has no such tag, which is why the other `psql` captures in
this runbook do not need it.

### S16 — A gateway is validated, and names nothing invisible (SRC-3, SRC-4)

S10 already refuses a gateway on the caller's own **non-external** network. Two neighbouring cases
were not covered, both found in the branch review.

SRC-3: `requireExternalNetwork` returned true for an empty or non-string `network_id`, deferring to
"the existing handling" — which on this path is `configureExternalGateway`, whose error `CreateRouter`
deliberately swallows. So the row was written with a gateway that never passed validation, and the
NAT reconciler then treats it as real forever. `201` for something the server ignored is precisely the
dishonest-success shape this ticket exists to remove.

SRC-4: the lookup carried no visibility predicate, so a network the caller cannot see answered `400`
("not an external network") while an unknown id answered `404` — the two distinguish existence.

```bash
printf '%-30s %s\n' "empty network_id" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"router":{"name":"ht-r-src3","external_gateway_info":{"network_id":""}}}' http://localhost:9696/v2.0/routers)"
printf '%-30s %s\n' "rows that call wrote" "$(docker exec o3k-postgres psql -U o3k -d o3k -tAc "SELECT count(*) FROM routers WHERE name = 'ht-r-src3'" | tr -d ' ')"
printf '%-30s %s\n' "absent network_id key" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"router":{"name":"ht-r-src3b","external_gateway_info":{"enable_snat":true}}}' http://localhost:9696/v2.0/routers)"
printf '%-30s %s\n' "network_id not a string" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"router":{"name":"ht-r-src3c","external_gateway_info":{"network_id":123}}}' http://localhost:9696/v2.0/routers)"
printf '%-30s %s\n' "invisible network" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"router\":{\"name\":\"ht-r-src4\",\"external_gateway_info\":{\"network_id\":\"$NID\"}}}" http://localhost:9696/v2.0/routers)"
printf '%-30s %s\n' "absent network" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"router":{"name":"ht-r-src4b","external_gateway_info":{"network_id":"00000000-0000-0000-0000-999999999999"}}}' http://localhost:9696/v2.0/routers)"
```

**Expect:** the three unusable spellings are **`400`** and `rows that call wrote` is **`0`** — a
refused request must persist nothing, which is the half a status-code-only check would miss. The last
two are both **`404`**: `$NID` is the network owned by `$OTHER` from S8, and an invisible network must
be indistinguishable from an absent one. A `400` on that line and `404` on the next is the oracle.

Nothing is lost by answering `404` there. A legitimately usable gateway network is external, and
external is its own visibility disjunct — S10's `ht-r-good` still succeeds against `$EXT`.

### S17 — A short or unusable port id is refused, not a crash (finding 1, ports)

S12 probes `DELETE /networks/{id}`. The same first-8-characters slice existed on the port path, and
port **create** took an unvalidated `network_id` the same way.

```bash
for id in foo ab 1234567 12345678 00000000-0000-0000-0000-999999999999; do printf '%-44s %s\n' "DELETE /ports/$id" "$(curl -s -H "X-Auth-Token: $T" -o /tmp/546-delp.json -w '%{http_code}' -X DELETE "http://localhost:9696/v2.0/ports/$id") $(jq -rc '[(keys|first), .NeutronError.type]' /tmp/546-delp.json 2>/dev/null || echo '<empty body>')"; done
printf '%-44s %s\n' "POST /ports with a short network_id" "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"port":{"network_id":"abc"}}' http://localhost:9696/v2.0/ports)"
docker logs --since 5m o3k 2>&1 | grep -c 'slice bounds out of range' || true
```

**Expect:** every `DELETE` line is `404 ["NeutronError","PortNotFound"]` — no `500`, no
`<empty body>`. The create is `404` or `400`, never `500`. The log count is **`0`**; as in S12 that is
the independent check, since a `404` reached by crashing and recovering would still log the runtime
error.

### S18 — A cached network is not served to a caller who may not see it (SRC-1)

**Run this last.** It changes `config/o3k.yaml` and rebuilds the image; the final block puts both back.

`GetNetwork`'s cache was keyed on the network id alone, and the cache lookup runs **before** the
visibility predicate the database path applies. So once an owner had fetched a private network, any
authenticated caller who knew its id got `200` with the full object for the whole TTL, where the
uncached answer is `404`. Making the body caller-independent — which this feature did, by reporting
the row's real owner instead of the caller's — fixed the *content* and not the authorization, and
gave the leak the owner's name to carry.

`internal/neutron/network_cache_test.go` pins the key and says in its own comment that it does not
prove the isolation, because `pkg/cache` needs a real Redis and CI has none. **This step is the only
end-to-end proof of the fix.**

It needs a deliberate config change to run at all. `docker-compose.yml` starts a Redis container and
sets `O3K_REDIS_URL` and `O3K_CACHE_ENABLED`, but **nothing in the Go source reads either variable** —
the cache is driven only by `cache.enabled` in the baked `config/o3k.yaml`, which is `false`. So every
step above this one runs with the cache dead, and would pass against the unfixed code.

```bash
sed -i 's|^  enabled: false  # Set to true to enable Redis caching|  enabled: true|' config/o3k.yaml
sed -i 's|^  redis_url: .*$|  redis_url: "redis://redis:6379/0"|' config/o3k.yaml
sed -n '/^cache:/,/^  key_prefix/p' config/o3k.yaml
docker compose -f deployments/docker-compose.yml up -d --build o3k
for i in $(seq 1 45); do curl -s -o /dev/null http://localhost:35357/v3 && break; sleep 2; done
export T=$(openstack token issue -f value -c id)
docker exec o3k-redis redis-cli FLUSHALL
openstack user create --project "$OTHER" --password secret ht-tenant -f value -c name
openstack role add --project "$OTHER" --user ht-tenant member
export T2=$(env OS_USERNAME=ht-tenant OS_PASSWORD=secret OS_PROJECT_NAME=hosttest-546 openstack token issue -f value -c id)
export PRIV=$(openstack network create ht-priv -f value -c id)
echo "owner reads it:   $(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $T" http://localhost:9696/v2.0/networks/$PRIV)"
echo "cached keys:      $(docker exec o3k-redis redis-cli --scan --pattern 'o3k:network:*' | tr '\n' ' ')"
echo "other tenant:     $(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $T2" http://localhost:9696/v2.0/networks/$PRIV)"
echo "owner again:      $(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $T" http://localhost:9696/v2.0/networks/$PRIV)"
echo "other tenant renames: $(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "X-Auth-Token: $T2" -H 'Content-Type: application/json' -d '{"network":{"name":"hijacked"}}' http://localhost:9696/v2.0/networks/$PRIV)"
echo "name after that:  $(docker exec o3k-postgres psql -U o3k -d o3k -tAc "SELECT name FROM networks WHERE id = '$PRIV'" | tr -d ' ')"
```

**Expect:** `owner reads it` is `200`; `cached keys` shows a key of the form
`o3k:network:<project-uuid>:<network-uuid>` — the **project segment is the fix**, and a key spelled
`o3k:network:<network-uuid>` is the defect; `other tenant` is **`404`**; and `owner again` is still
`200`, proving the scoping did not simply disable caching.

`other tenant renames` is **not** `200` and `name after that` is still `ht-priv` — contract X9, the
non-owner write refusal S10 could not run because it holds only an administrator token. The
administrator exemption in `UpdateNetwork` is scoped to administrators; a tenant still gets the owner
predicate, and this is the only place in the runbook with a caller to prove it.

The key shape and the `404` are two independent readings of the same fix, and both are worth having.
The `404` is the behaviour that matters, but it also passes when the cache is off entirely — which,
given how easy that is to end up with here, is a real risk. The key is what proves the cache was
actually live and consulted.

Put everything back:

```bash
git checkout -- config/o3k.yaml
docker compose -f deployments/docker-compose.yml up -d --build o3k
for i in $(seq 1 45); do curl -s -o /dev/null http://localhost:35357/v3 && break; sleep 2; done
export T=$(openstack token issue -f value -c id)
grep -A1 '^cache:' config/o3k.yaml
```

**Pass =** `enabled: false` is back and `git status --short config/o3k.yaml` prints nothing.

The first `sed` anchors on the **comment**, not on `enabled: false`. `config/o3k.yaml` carries two
lines spelled that way — the cache block and the rate-limiting block twelve lines below it — so the
obvious pattern turns on rate limiting as well, and the requests in this step start being refused
`429` for a reason that has nothing to do with the cache.

---

## 3. Teardown steps

Removes only what this run created, then the stack.

```bash
openstack router delete ht-r-good ht-r-src3 ht-r-src3b ht-r-src3c ht-r-src4 ht-r-src4b 2>/dev/null || true
docker exec o3k-postgres psql -U o3k -d o3k -tAc "DELETE FROM routers WHERE name IN ('ht-r-foreign','ht-r-src3','ht-r-src3b','ht-r-src3c','ht-r-src4','ht-r-src4b')" 2>/dev/null || true
openstack volume delete ht-vg-1 ht-vg-2 ht-vg-3 2>/dev/null || true
openstack user delete ht-tenant 2>/dev/null || true
git checkout -- config/o3k.yaml
openstack floating ip list -f value -c ID | xargs -r openstack floating ip delete 2>/dev/null || true
openstack port delete ht-p1 ht-pg-1 ht-pg-2 ht-pg-3 ht-pg-4 ht-pg-5 2>/dev/null || true
openstack subnet delete ht-sub-a ht-sub-b ht-nogw 2>/dev/null || true
docker exec o3k-postgres psql -U o3k -d o3k -tAc "DELETE FROM subnets WHERE name LIKE 'ht-nogw%'" 2>/dev/null || true
openstack network delete ht-net-a ht-net-b ht-ext ht-b11 ht-priv 2>/dev/null || true
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
