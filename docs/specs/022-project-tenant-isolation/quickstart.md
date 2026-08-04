# Quickstart: Project/Tenant Lifecycle & Isolation Fixes

**Feature**: `022-project-tenant-isolation` | **Ticket**: [cloud-storage#545](https://github.com/cobaltcore-dev/cloud-storage/issues/545)

Reproduce all four defects (RED), then verify all four fixes (GREEN). FR-021 makes the before/after pair
a requirement per defect, not a courtesy — so record both.

Stub mode is sufficient throughout (A8). Real mode is neither required nor exercised.

All shell blocks are zsh-copy-paste-safe: no line continuations, no inline comments, no history
expansion inside double quotes.

---

## 0. Environment

```
export HORIZON_HTTP_PORT=8080
export LIGHTSTACK_JWT_SECRET=dev-secret-change-me
docker compose -f deployments/docker-compose-horizon.yml up -d --build
```

```
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
```

```
export T=$(openstack token issue -f value -c id)
export P=00000000-0000-0000-0000-000000000002
echo "token length: ${#T}"
```

A token length of 0 means the stack is not up. Everything below assumes a non-empty `T`.

---

## 1. Static checks — no stack needed

These run against the source tree and are the cheapest RED/GREEN evidence.

| Check | RED (today) | GREEN (after) |
|---|---|---|
| B8: subnet create consults `networks` | `0` | non-zero |
| B2: project-scoped volume list registered | `0` | `1` |

```
awk '/func \(svc \*Service\) CreateSubnet/,/^}/' internal/neutron/network.go | grep -c 'FROM networks'
```

```
awk '/v3 := r.Group\("\/v3\/:project_id"\)/,0' internal/cinder/volumes.go | grep -c 'v3.GET("/volumes",'
```

Confirm the four shadowed sub-resources are registered too, and that `backups/detail` is *not* (it is
absent, not shadowed — research R6):

```
awk '/v3 := r.Group\("\/v3\/:project_id"\)/,0' internal/cinder/volumes.go | grep -cE 'v3.GET\("/(volumes|snapshots|os-volume-transfer)/detail"|v3.GET\("/types/default"'
```

Expect `0` before, `4` after.

---

## 2. B5 / P1 — tenant creation

### RED

```
openstack project create --domain default probe
```

Expect a `500` generic server fault. The literal string `default` reaches a `uuid` column.

```
openstack user create --domain default probe-u --password x
```

Expect `400 "specified domain not found or disabled"` — the same missing alias, reported honestly by a
handler that does look the domain up.

```
openstack domain show default
```

Expect `404 "Could not find domain: default."` — the identifier middleware from #515 refuses the alias
before the handler runs.

```
curl -s -H "X-Auth-Token: $T" "http://localhost:35357/v3/domains?name=Default" | jq '.domains | length'
```

Expect a count greater than 1 if more than one domain exists — the filter is ignored entirely.

Unknown domain, and duplicate name, both as generic faults:

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:35357/v3/projects" -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-unknown-domain","domain_id":"11111111-1111-1111-1111-111111111111"}}'
```

Expect `500`. Then create a project twice with the same name and expect `500` on the second.

### GREEN

```
openstack project create --domain default probe
openstack user create --domain default probe-u --password x
openstack domain show default
```

All three succeed. Then:

```
curl -s -H "X-Auth-Token: $T" "http://localhost:35357/v3/domains?name=Default" | jq '.domains | length'
```

Expect `1`.

```
curl -s -H "X-Auth-Token: $T" "http://localhost:35357/v3/domains?name=default" | jq '.domains | length'
```

Expect `0` — domain names stay case-sensitive (FR-001a).

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:35357/v3/projects" -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-unknown-domain","domain_id":"11111111-1111-1111-1111-111111111111"}}'
```

Expect `400`, and the body should name the domain field:

```
curl -s -X POST "http://localhost:35357/v3/projects" -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-unknown-domain","domain_id":"11111111-1111-1111-1111-111111111111"}}' | jq .
```

Duplicate name now reports a conflict:

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:35357/v3/projects" -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"project":{"name":"probe","domain_id":"default"}}'
```

Expect `409`.

Group creation accepts the alias (FR-006):

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:35357/v3/groups" -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"group":{"name":"probe-g","domain_id":"default"}}'
```

Expect `201`.

And the response still carries the stored UUID, never the alias (FR-007, A2):

```
openstack project show probe -f value -c domain_id
```

Expect a UUID, not `default`.

---

## 3. B2 — the project-scoped volume list

### RED

```
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/volumes"
```

Expect `405`.

```
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/volumes/detail"
```

Expect `404` or `500` — the request is served as a lookup for a volume literally named `detail`.

### GREEN

```
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/volumes"
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/volumes/detail"
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/snapshots/detail"
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/types/default"
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/os-volume-transfer/detail"
```

All five `200`. Both URL forms must agree (SC-005):

```
diff <(curl -s -H "X-Auth-Token: $T" "http://localhost:8776/v3/volumes" | jq -S '.volumes') <(curl -s -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/volumes" | jq -S '.volumes') && echo "IDENTICAL"
```

A suite that already fails on this defect must now pass. `test/horizon_compat_test.sh` hits both B2
paths — `GET /v3/${PROJECT_ID}/volumes` at line 131 and `GET /v3/${PROJECT_ID}/volumes/detail` at
line 210 — so record its counts before and after:

```
bash test/horizon_compat_test.sh
```

Before: the "Cinder - List volumes" and Volumes-tab checks fail. After: both pass, with no new
failures (SC-008).

`backups/detail` stays a `404` on the project form — it is absent by decision, not shadowed (FR-016a):

```
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/backups/detail"
```

Expect `404` both before and after. This is a deliberate non-change.

---

## 4. B8 — the cross-tenant subnet write 🔒

Needs two tenants, so it depends on §2 being GREEN. That is the ticket's dependency chain.

### Setup

```
openstack project create --domain default tenant-a
openstack project create --domain default tenant-b
openstack user create --domain default user-a --password pw-a
openstack user create --domain default user-b --password pw-b
```

Lightstack seeds `admin`, `member` and `reader`; there is no `_member_` role.

```
export PA=$(openstack project show tenant-a -f value -c id)
export PB=$(openstack project show tenant-b -f value -c id)
openstack role add --project tenant-a --user user-a member
openstack role add --project tenant-b --user user-b member
```

```
export TA=$(openstack --os-username user-a --os-password pw-a --os-project-name tenant-a token issue -f value -c id)
export TB=$(openstack --os-username user-b --os-password pw-b --os-project-name tenant-b token issue -f value -c id)
echo "TA=${#TA} TB=${#TB}"
```

Tenant A creates a private network:

```
export NA=$(curl -s -X POST "http://localhost:9696/v2.0/networks" -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d '{"network":{"name":"net-a"}}' | jq -r '.network.id')
echo "NA=$NA"
```

### RED

Tenant B writes a subnet into tenant A's network:

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:9696/v2.0/subnets" -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"network_id":"'$NA'","cidr":"10.99.0.0/24","ip_version":4}}'
```

Expect `201`. That is the defect. Confirm tenant A now sees it:

```
curl -s -H "X-Auth-Token: $TA" "http://localhost:9696/v2.0/subnets" | jq '.subnets | length'
```

A nonexistent network id, and a malformed one, both as faults:

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:9696/v2.0/subnets" -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"network_id":"11111111-1111-1111-1111-111111111111","cidr":"10.98.0.0/24"}}'
```

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:9696/v2.0/subnets" -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"network_id":"not-a-uuid","cidr":"10.97.0.0/24"}}'
```

Both `500`.

### GREEN

Reset the fixture first, so tenant A's network has no subnet:

```
openstack --os-username user-a --os-password pw-a --os-project-name tenant-a subnet list -f value -c ID | while read s; do curl -s -o /dev/null -X DELETE "http://localhost:9696/v2.0/subnets/$s" -H "X-Auth-Token: $TA"; done
```

Cross-tenant write on a **private** network — expect `404`:

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:9696/v2.0/subnets" -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"network_id":"'$NA'","cidr":"10.99.0.0/24","ip_version":4}}'
```

No residue in tenant A's views (SC-003):

```
curl -s -H "X-Auth-Token: $TA" "http://localhost:9696/v2.0/subnets" | jq '.subnets | length'
```

Expect `0`.

Cross-tenant write on a **shared** network — expect `403` (FR-010a):

```
export NS=$(curl -s -X POST "http://localhost:9696/v2.0/networks" -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d '{"network":{"name":"net-a-shared","shared":true}}' | jq -r '.network.id')
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:9696/v2.0/subnets" -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"network_id":"'$NS'","cidr":"10.96.0.0/24"}}'
```

Nonexistent network id — expect `404`; malformed — expect `400` (FR-011, FR-011a):

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:9696/v2.0/subnets" -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"network_id":"11111111-1111-1111-1111-111111111111","cidr":"10.98.0.0/24"}}'
```

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:9696/v2.0/subnets" -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"network_id":"not-a-uuid","cidr":"10.97.0.0/24"}}'
```

Legitimate capability preserved (SC-004) — tenant B on tenant B's own network, expect `201`:

```
export NB=$(curl -s -X POST "http://localhost:9696/v2.0/networks" -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"network":{"name":"net-b"}}' | jq -r '.network.id')
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:9696/v2.0/subnets" -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"network_id":"'$NB'","cidr":"10.95.0.0/24"}}'
```

Admin on any network, expect `201`:

```
curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://localhost:9696/v2.0/subnets" -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"subnet":{"network_id":"'$NA'","cidr":"10.94.0.0/24"}}'
```

The audit record (FR-011b) — exactly one warning per refusal, none on success:

```
docker compose -f deployments/docker-compose-horizon.yml logs lightstack | grep -ci 'cross-tenant'
```

---

## 5. The harness

### RED

```
make test-multi-tenant
```

Today: fails at setup with `PROJECT1=null`, or reports passes for checks that ran against a tenant that
was never created.

### GREEN

```
make test-multi-tenant
make test-multi-tenant
```

Both runs pass and agree (SC-006a). Then prove the guards fire — point the suite at a stack where
tenant creation is broken and confirm a non-zero exit with **zero** reported passes.

Prove teardown runs on abort: interrupt a run mid-way, then re-run immediately. It must start clean
rather than failing on a leftover `test-project-1` conflict.

---

## 6. Unit and contract gates

```
make test
make lint
bash test/horizon_compat_test.sh
```

`horizon_compat_test.sh` must be **better** than its baseline, not merely unchanged — it fails two
volume checks today (SC-008).

DB-backed unit tests skip unless a test database is configured, and this feature's tests are DB-backed:

```
export LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack_test?sslmode=disable'
go test ./internal/keystone/... ./internal/neutron/... ./internal/cinder/...
```

```
make test-integration
go test ./test/contract/keystone/...
```

The contract suite is the guard for A2/FR-007: it asserts the stored domain UUID in five places, so it
fails if accepting the alias on input ever starts changing what responses return.

---

## 7. Teardown

```
openstack project delete probe tenant-a tenant-b
openstack user delete probe-u user-a user-b
docker compose -f deployments/docker-compose-horizon.yml down -v
```

---

## Evidence checklist (FR-021, SC-007)

Four defects, four recorded before/after pairs:

- [ ] **B5/P1** — `openstack project create --domain default probe`: `500` → created; plus `domain show default`, the `name` filter, unknown domain `400`, duplicate `409`
- [ ] **B8** 🔒 — cross-tenant subnet write: `201` → `404` private / `403` shared, with no residue in the victim's views and both legitimate paths still `201`
- [ ] **B2** — `GET /v3/{project}/volumes`: `405` → `200`, plus the four shadowed sub-resources, plus both URL forms returning identical collections, plus `test/horizon_compat_test.sh` going from two failing volume checks to zero
- [ ] **Harness** — `make test-multi-tenant`: setup failure → suite passes; guards fire with zero reported passes; two consecutive runs agree
