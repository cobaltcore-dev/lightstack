# Quickstart: Tenant Scoping & Authorization Model

**Feature**: `023-tenant-scoping-authz`

How to see each defect before the fix, and confirm it after. Everything here runs against the **stub** stack — no libvirt, no Linux-only capabilities. Real mode adds nothing this feature needs.

---

## Static checks (no stack required)

```bash
grep -c 'HasRole' internal/nova/*.go                      # 4 sites, all admin-ONLY gates
grep -c 'project_id = \$' internal/nova/*.go              # 102 scoped predicates
grep -rn 'all_tenants' --include='*.go' .                 # nothing — greenfield
sed -n '55,65p' internal/keystone/auth.go                 # ScopeField: Project only
grep -c 'c.Query(' internal/keystone/handlers.go          # ListRoles reads none
grep -n 'project_id\|HasRole' internal/nova/migrations.go # zero of either  (R11)
grep -c 'HasRole\|Forbidden' internal/nova/aggregates.go  # 0 across 9 handlers (R11)
```

---

## Bring up the stack

```bash
export HORIZON_HTTP_PORT=8080 LIGHTSTACK_JWT_SECRET=dev-secret-change-me
docker compose -f deployments/docker-compose-horizon.yml up -d --build
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret
export OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
ADMIN_TOKEN=$(openstack token issue -f value -c id)
```

## Fixtures — a second project and a genuinely non-admin user

Both are required; without them every check below passes vacuously. Note the role is selected **by name on the client side** — `GET /v3/roles?name=member` does not filter yet, which is User Story 5.

```bash
MEMBER_ROLE=$(curl -s localhost:35357/v3/roles -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.roles[] | select(.name=="member") | .id')
ADMIN_ROLE=$(curl -s localhost:35357/v3/roles -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.roles[] | select(.name=="admin") | .id')
test "$MEMBER_ROLE" != "$ADMIN_ROLE" || echo "SETUP BROKEN: member resolved to admin"

PROJ_B=$(openstack project create qs-b -f value -c id)
openstack user create qs-user-b --password pw-b --domain Default >/dev/null
USER_B=$(openstack user show qs-user-b -f value -c id)
curl -s -o /dev/null -X PUT "localhost:35357/v3/projects/$PROJ_B/users/$USER_B/roles/$MEMBER_ROLE" -H "X-Auth-Token: $ADMIN_TOKEN"

B_TOKEN=$(curl -s -X POST localhost:35357/v3/auth/tokens -H 'Content-Type: application/json' -d "{\"auth\":{\"identity\":{\"methods\":[\"password\"],\"password\":{\"user\":{\"name\":\"qs-user-b\",\"password\":\"pw-b\",\"domain\":{\"name\":\"Default\"}}}},\"scope\":{\"project\":{\"id\":\"$PROJ_B\"}}}}" -D- -o /dev/null | awk '/X-Subject-Token/{print $2}' | tr -d '\r')

echo "$B_TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | jq -e '.roles | index("admin")' >/dev/null && echo "SETUP BROKEN: B carries admin"
```

Create one server per project — `SRV_A` owned by the admin's project, `SRV_B` owned by B. Both are needed: `SRV_B` proves the override, `SRV_A` proves isolation still holds.

```bash
ADMIN_PROJECT=$(openstack token issue -f value -c project_id)
SRV_A=$(curl -s -X POST localhost:8774/v2.1/servers -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"server":{"name":"qs-a-server","flavorRef":"m1.tiny"}}' | jq -r '.server.id')
SRV_B=$(curl -s -X POST localhost:8774/v2.1/servers -H "X-Auth-Token: $B_TOKEN"     -H 'Content-Type: application/json' -d '{"server":{"name":"qs-b-server","flavorRef":"m1.tiny"}}' | jq -r '.server.id')
```

---

## US1 — The administrative override

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8774/v2.1/servers/$SRV_B -H "X-Auth-Token: $ADMIN_TOKEN"
```

| | Before | After |
|---|---|---|
| admin reads B's server | `404` | `200`, `tenant_id` = B |

```bash
curl -s localhost:8774/v2.1/servers -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.servers | length'
curl -s 'localhost:8774/v2.1/servers?all_tenants=1' -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.servers | length'
curl -s 'localhost:8774/v2.1/servers?all_tenants'   -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.servers | length'
```

| | Before | After |
|---|---|---|
| default listing | admin's own | **unchanged** — admin's own |
| `?all_tenants=1` | admin's own | every project's |
| `?all_tenants` (bare) | admin's own | every project's |

Isolation must not regress — tenant B reaching the admin's server MUST stay `404` throughout:

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8774/v2.1/servers/$SRV_A -H "X-Auth-Token: $B_TOKEN"
```

And a non-admin's opt-in must be **ignored**, not refused — these two MUST match:

```bash
curl -s 'localhost:8774/v2.1/servers?all_tenants=1' -H "X-Auth-Token: $B_TOKEN" | jq -S .
curl -s  localhost:8774/v2.1/servers                -H "X-Auth-Token: $B_TOKEN" | jq -S .
```

### FR-008a — the two broken administrator-only actions

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8774/v2.1/servers/$SRV_B/action \
  -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"os-resetState":{"state":"error"}}'
```

| | Before | After |
|---|---|---|
| admin resets B's server state | `404` | `202` |
| admin evacuates B's server | `404` | `202` or `400` — never `404` |

### FR-005c — name matching never spans projects

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE localhost:8774/v2.1/servers/qs-b-server -H "X-Auth-Token: $ADMIN_TOKEN"
```

Expected `404` before **and** after. An admin addressing B's server by *name* must not reach it; only by identifier.

### FR-007 — attribution against the owning project

After an admin deletes `$SRV_B`, B's own action history shows it:

```bash
curl -s localhost:8774/v2.1/servers/$SRV_B/os-instance-actions -H "X-Auth-Token: $B_TOKEN" | jq '.instanceActions'
```

| | Before | After |
|---|---|---|
| B sees the admin's delete | no row written at all | row with `action=delete`, admin's `user_id` |

---

## US2 — Domain scope refused

The ticket's own reproduction:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:35357/v3/auth/tokens -H 'Content-Type: application/json' \
  -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","password":"secret","domain":{"name":"Default"}}}},"scope":{"domain":{"name":"Default"}}}}'
```

| | Before | After |
|---|---|---|
| domain-scoped request | `201` with a role-less token | `400` naming the scope |

Uniform refusal — all four MUST return the same status and body:

```bash
for d in '{"name":"Default"}' '{"name":"nope"}' '{"id":"default"}' '{"id":"not-a-uuid"}'; do
  curl -s -o /dev/null -w "$d -> %{http_code}\n" -X POST localhost:35357/v3/auth/tokens -H 'Content-Type: application/json' \
    -d "{\"auth\":{\"identity\":{\"methods\":[\"password\"],\"password\":{\"user\":{\"name\":\"admin\",\"password\":\"secret\",\"domain\":{\"name\":\"Default\"}}}},\"scope\":{\"domain\":$d}}}"
done
```

**Regression guard — the nested qualifier must keep working.** If this breaks, every client breaks:

```bash
openstack token issue -f value -c id   # MUST still succeed
```

---

## US3 — The usage leak

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8774/v2.1/os-simple-tenant-usage -H "X-Auth-Token: $B_TOKEN"
curl -s localhost:8774/v2.1/os-simple-tenant-usage/$ADMIN_PROJECT -H "X-Auth-Token: $B_TOKEN" | jq '.tenant_usage.tenant_id'
curl -s -o /dev/null -w '%{http_code}\n' localhost:8774/v2.1/os-simple-tenant-usage/not-a-uuid -H "X-Auth-Token: $ADMIN_TOKEN"
```

| | Before | After |
|---|---|---|
| non-admin reads the aggregate | `200` + every project's usage | `403` |
| non-admin reads another project by id | that project's usage | own project, or refused |
| malformed target | `200` with zeroes | `4xx` |

---

## US6 — Operator-only surfaces

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8774/v2.1/os-migrations  -H "X-Auth-Token: $B_TOKEN"
curl -s -o /dev/null -w '%{http_code}\n' localhost:8774/v2.1/os-aggregates  -H "X-Auth-Token: $B_TOKEN"
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8774/v2.1/os-aggregates \
  -H "X-Auth-Token: $B_TOKEN" -H 'Content-Type: application/json' \
  -d '{"aggregate":{"name":"qs-pwn","availability_zone":"nova"}}'
```

| | Before | After |
|---|---|---|
| tenant lists every project's migrations | `200` | `403` |
| tenant lists host aggregates | `200` | `403` |
| **tenant CREATES a host aggregate** | `200`/`201` | `403` |
| tenant deletes an aggregate / adds / removes a host | succeeds | `403` |

The create is the one to run first — it is the feature's only privilege escalation, and it currently succeeds.

Administrator behaviour must be unchanged:

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8774/v2.1/os-migrations -H "X-Auth-Token: $ADMIN_TOKEN"   # 200 before and after
curl -s -o /dev/null -w '%{http_code}\n' localhost:8774/v2.1/os-aggregates -H "X-Auth-Token: $ADMIN_TOKEN"   # 200 before and after
```

Per-server migrations gain an ownership check they never had — tenant B reading the admin's server:

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8774/v2.1/servers/$SRV_A/migrations -H "X-Auth-Token: $B_TOKEN"
```

| | Before | After |
|---|---|---|
| tenant B reads another project's server migrations | `200` | `404` |
| tenant B **deletes** another project's server migration | succeeds | `404` |

---

## US5 — Identity filtering

```bash
curl -s 'localhost:35357/v3/roles?name=member' -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.roles | length'
curl -s 'localhost:35357/v3/roles?name=nope'   -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.roles'
```

| | Before | After |
|---|---|---|
| `?name=member` | 3 (all roles) | 1 |
| `?name=nope` | 3 (all roles) | `[]` |

Unfiltered must be unchanged:

```bash
curl -s localhost:35357/v3/roles -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.roles | length'   # 3, before and after
```

---

## Suites

```bash
make test                      # unit
./test/multi_tenant_test.sh    # isolation + (new) override coverage
go test ./test/contract/nova/... ./test/contract/keystone/...
./test/quick_test.sh
make test-all                  # NOTE: currently red on B10 (#546), unrelated to this feature
```

## Teardown

```bash
openstack server delete $SRV_A $SRV_B 2>/dev/null || true
openstack aggregate delete qs-pwn 2>/dev/null || true
openstack user delete qs-user-b 2>/dev/null || true
openstack project delete qs-b 2>/dev/null || true
docker compose -f deployments/docker-compose-horizon.yml down -v
```
