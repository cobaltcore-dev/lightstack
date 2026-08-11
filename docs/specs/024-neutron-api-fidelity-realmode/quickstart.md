# Quickstart: reproduce and verify all six defects

**Feature**: `024-neutron-api-fidelity-realmode` | **Date**: 2026-08-11

Four of the six (B6, B9, B10, B11) and the stub half of F10 reproduce on the compose stack with no
Linux privileges. F11 and F10's real half need a privileged Linux host.

All blocks are zsh-copy-paste-safe: no `$!`, no bare `!` inside double quotes, no line continuations,
no inline comments.

---

## Setup

```zsh
cd /home/fnk/code/sap/ora/lightstack/feature/546
export HORIZON_HTTP_PORT=8080 LIGHTSTACK_JWT_SECRET=dev-secret-change-me
docker compose -f deployments/docker-compose-horizon.yml up -d --build
```

```zsh
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret
export OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
T=$(openstack token issue -f value -c id)
echo "token length ${#T}"
```

Baseline first — `make test-all` is already red on B10, and the shared `lightstack-postgres` container is used
by around a dozen worktrees, so a stale quota or project row produces failures unrelated to this
branch.

```zsh
openstack project list
make test-errors 2>&1 | tail -20
```

Expect `Total Tests: 10 · Passed: 9 · Failed: 1` and a non-zero exit. That one failure is B10.

---

## B10 — Neutron errors use the compute envelope

```zsh
curl -s -H "X-Auth-Token: $T" localhost:9696/v2.0/networks/00000000-0000-0000-0000-999999999999 | jq 'keys'
```

| Now | Expected after |
|---|---|
| `["itemNotFound"]` | `["NeutronError"]` |

Check the inner shape too — the wrapper alone is not the fix (contract E2):

```zsh
curl -s -H "X-Auth-Token: $T" localhost:9696/v2.0/networks/00000000-0000-0000-0000-999999999999 | jq '.[] | keys'
```

Expected after: `["detail","message","type"]` — and **no** `code`.

The path most likely to be missed is the shared auth middleware (E5):

```zsh
curl -s localhost:9696/v2.0/networks | jq 'keys'
curl -s -H "X-Auth-Token: $T" localhost:9696/v2.0/nonexistent | jq 'keys'
```

Both must be `["NeutronError"]` after the change.

---

## F10 (stub half) — a port cannot be created on a pre-restart network

```zsh
NET=$(openstack network create t-net -f value -c id)
openstack subnet create t-sub --network "$NET" --subnet-range 192.168.199.0/24 --no-dhcp
openstack port create --network t-net p1
```

That may pass, because the bridge is still in this process's in-memory map. Force the real condition:

```zsh
docker compose -f deployments/docker-compose-horizon.yml restart lightstack
sleep 15
openstack port create --network t-net p2
```

| Now | Expected after |
|---|---|
| `500 failed to attach TAP to bridge` | port created |

The cause is `BridgeManager.stubBridges`, a process-local map — the recorded network must be the
authority instead (FR-002).

---

## B6 — subnet filter ignored, and gatewayless subnets vanish

```zsh
NETA=$(openstack network create b6-a -f value -c id)
NETB=$(openstack network create b6-b -f value -c id)
openstack subnet create b6-sub-a --network "$NETA" --subnet-range 10.60.1.0/24
openstack subnet create b6-sub-b --network "$NETB" --subnet-range 10.60.2.0/24
openstack subnet list --network "$NETA" -f value -c Name
```

| Now | Expected after |
|---|---|
| both subnets | only `b6-sub-a` |

An unknown filter must not narrow the result (FR-020a):

```zsh
curl -s -H "X-Auth-Token: $T" "localhost:9696/v2.0/subnets?bogus=x" | jq '.subnets | length'
curl -s -H "X-Auth-Token: $T" "localhost:9696/v2.0/subnets" | jq '.subnets | length'
```

Both counts must match.

Gatewayless — currently the request is silently overwritten:

```zsh
openstack subnet create b6-nogw --network "$NETA" --subnet-range 10.60.9.0/24 --gateway none
openstack subnet show b6-nogw -f value -c gateway_ip
```

| Now | Expected after |
|---|---|
| an invented `10.60.9.1` | `None` |

Then confirm it survives a listing and a show (today the row silently disappears from the list and the
show returns 500):

```zsh
openstack subnet list --network "$NETA" -f value -c Name
openstack subnet show b6-nogw -f value -c id
```

---

## B9 — the create response disagrees with the read

```zsh
SG=$(openstack security group create b9-sg -f value -c id)
RULE=$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"security_group_rule\":{\"security_group_id\":\"$SG\",\"direction\":\"ingress\",\"ethertype\":\"IPv4\"}}" localhost:9696/v2.0/security-group-rules)
echo "$RULE" | jq '.security_group_rule | {remote_ip_prefix, remote_group_id, port_range_min, port_range_max}'
```

```zsh
RID=$(echo "$RULE" | jq -r '.security_group_rule.id')
curl -s -H "X-Auth-Token: $T" "localhost:9696/v2.0/security-group-rules/$RID" | jq '.security_group_rule | {remote_ip_prefix, remote_group_id, port_range_min, port_range_max}'
```

| Now | Expected after |
|---|---|
| create says `""` and `0`; read says `null` | both say `null` |

And the deliberate-zero case, which fails in **storage** today:

```zsh
curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"security_group_rule\":{\"security_group_id\":\"$SG\",\"direction\":\"ingress\",\"ethertype\":\"IPv4\",\"protocol\":\"icmp\",\"port_range_min\":0,\"port_range_max\":0}}" localhost:9696/v2.0/security-group-rules | jq '.security_group_rule | {port_range_min, port_range_max}'
```

Expected after: both `0`, and re-reading the rule still reports `0` rather than `null`.

---

## B11 — a requested project is discarded with a 201

```zsh
OTHER=$(openstack project create b11-probe -f value -c id)
openstack network create --project "$OTHER" b11-net -f value -c id
openstack network show b11-net -f value -c project_id
```

| Now | Expected after |
|---|---|
| the **admin's** project | `$OTHER` |

Verify against storage, not just the echo — the response reports the caller today, so an echo-only
check can pass for the wrong reason:

```zsh
docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT project_id FROM networks WHERE name = 'b11-net'"
echo "expected $OTHER"
```

Non-administrators must be refused rather than redirected (needs a second, non-admin user — see
`test/multi_tenant_test.sh` for the fixture pattern). Expected: `400`.

---

## F11 — no external networks

```zsh
openstack network list --external -f value -c Name
```

| Now | Expected after |
|---|---|
| empty | the external networks |

```zsh
curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"f11-ext","router:external":true}}' localhost:9696/v2.0/networks | jq '.network."router:external"'
```

| Now | Expected after |
|---|---|
| `false` — the request is discarded | `true` |

The guardrails are absent today — a non-external network is silently accepted as a gateway:

```zsh
R=$(openstack router create f11-r -f value -c id)
openstack router set --external-gateway b6-a "$R"
```

| Now | Expected after |
|---|---|
| accepted | refused, because `b6-a` is not external |

Grandfathering (FR-035a): a gateway configured **before** migration `066` must keep working after it,
and the condition must appear in the logs.

```zsh
docker compose -f deployments/docker-compose-horizon.yml logs lightstack 2>&1 | grep -i "external" | tail -10
```

---

## F10 / F11 real mode — privileged Linux only

Needs genuine root; rootless podman cannot create host network namespaces. Use the k3s path from
`charts/lightstack` with `realMode` enabled, as in SPEC-021's runbook.

```zsh
ip -br link show type bridge | grep '^br-'
sudo ip netns list
```

| Now | Expected after |
|---|---|
| bridges in the root namespace, no `light-ns-*` | the port's TAP is in the **same** namespace as its bridge |

Then, on a booted guest's console, the lines this feature exists to remove:

```text
no interfaces have a carrier
169.254.169.254 request failed
```

Expected after: an address from DHCP and a successful metadata fetch. Also verify the gatewayless case
keeps metadata (FR-023a):

```zsh
sudo cat /var/lib/lightstack/dnsmasq/*.conf | grep dhcp-option
```

Expected for a gatewayless subnet: **no** `dhcp-option=3`, but `dhcp-option=121` present.

---

## Full verification

```zsh
make fmt
make lint
make test
make test-contract
make test-errors
make test-all
```

`make test-all` composes unit, chart, contract, integration, multi-tenant, errors, horizon-UI and
performance suites, all against the compose stack — no Kubernetes cluster needed. It must exit 0,
where today it exits non-zero on B10.

Migration both ways (Constitution *Backwards Compatibility*):

```zsh
make migrate
docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "\d networks" | grep external
```

Then confirm the down migration drops the column cleanly.

## Teardown

```zsh
openstack router delete f11-r
openstack network delete f11-ext b11-net b6-a b6-b t-net
openstack security group delete b9-sg
openstack project delete b11-probe
docker compose -f deployments/docker-compose-horizon.yml down
```
