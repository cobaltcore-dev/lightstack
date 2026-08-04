# Host testing guide — Project/tenant lifecycle & isolation fixes (stub mode)

Validates [SPEC-022](./spec.md) / [cloud-storage#545](https://github.com/cobaltcore-dev/cloud-storage/issues/545)
by hand on a fresh Ubuntu host: a tenant can be created with the payload every OpenStack client sends
(**B5/P1**), a tenant can no longer write a subnet into another tenant's network (**B8** 🔒), the
project-scoped volume URL answers (**B2**), and the multi-tenancy suite reports on something real
(**T1 + guards**). Proves SC-001 … SC-008.

**Topology**: one Ubuntu host, **stub mode**, O3K run as the `bin/o3k` binary against a PostgreSQL
container. No libvirt, no Ceph, no Kubernetes. Assumption A8 — stub mode demonstrates every fix.

**Run all commands from the repo root.** Blocks use repo-relative paths and never `cd`.

## Validates

| Acceptance criterion | Proved in |
|---|---|
| US1 / SC-001, SC-002 — an operator can create a tenant; no generic faults | [2.1](#21-b5p1--tenant-creation-with-the-default-domain-alias) |
| US1 / SC-001a, FR-001a — the client's own `--domain` resolution works | [2.2](#22-b5--domain-retrieval-by-alias-and-the-name-filter) |
| US2 / SC-003, SC-004, SC-004a, SC-004b — no cross-tenant subnet write 🔒 | [2.4](#24-b8--the-cross-tenant-subnet-write-) |
| US2 / FR-013a — the network owner may inspect and remove a foreign subnet on their network | [2.4a](#24a-fr-013a--the-owner-may-inspect-and-remove-what-an-admin-put-on-their-network) |
| US3 / SC-005, SC-005a — both volume URL forms agree; nothing shadowed | [2.5](#25-b2--the-project-scoped-volume-routes) |
| US4 / SC-006, SC-006a — the suite reports on something real, and is re-runnable | [2.6](#26-us4--the-multi-tenancy-suite) |
| SC-008 — no regression; the Horizon suite goes from failing to passing | [2.7](#27-sc-008--the-horizon-compatibility-suite), [2.8](#28-sc-008--the-full-gate-set) |

---

## 0. Prerequisites

**A container engine (Docker or Podman) and a Go 1.26 toolchain must already be installed** — both are
host-operator setup, so the block below only *verifies* them. Everything else it installs is a
lightweight unprivileged CLI the tests actually invoke: `jq`, `psql`, `curl`, `make` and the OpenStack
client. Re-running the block is a no-op.

```bash
command -v docker >/dev/null || command -v podman >/dev/null || echo "MISSING: install docker or podman first (not installed by this runbook)"
command -v go >/dev/null || echo "MISSING: install Go 1.26+ first (not installed by this runbook)"
sudo apt-get update -qq
sudo apt-get install -y jq curl make postgresql-client python3-openstackclient
```

Verify. Every line must print a version, and the two `MISSING:` lines above must not have appeared.

```bash
echo "engine: $(docker --version 2>/dev/null || podman --version 2>/dev/null)"; echo "go:     $(go version)"; echo "jq:     $(jq --version)"; echo "psql:   $(psql --version)"; echo "curl:   $(curl --version | head -1)"; echo "make:   $(make --version | head -1)"; echo "osc:    $(openstack --version 2>&1 | head -1)"
```

**Pass =** seven version lines, no `MISSING:` line. Go must be **1.26 or newer** (the module targets
1.21+ but the toolchain directive requires 1.26).

> If your engine is Podman with the Docker CLI shim, `docker` resolves to Podman and every block below
> works unchanged — that is how this guide was validated.

---

## 1. Setup steps

### 1.1 Start PostgreSQL

`make db-up` runs `postgres:18` as a container named `o3k-postgres` on port 5432 with
`o3k/secret`. It mounts **no volume**, so removing the container discards every database — which is
what makes teardown clean. The `db-down` first makes the block re-runnable.

```bash
make db-down 2>/dev/null || true
make db-up
export PGPASSWORD=secret
for i in $(seq 1 30); do psql -h localhost -U o3k -d postgres -tAc 'select 1' >/dev/null 2>&1 && { echo "postgres ready after ${i}s"; break; }; sleep 1; done
psql -h localhost -U o3k -d postgres -c 'CREATE DATABASE o3k_test;'
psql -h localhost -U o3k -d o3k_test -tAc "select 'connected to ' || current_database()"
```

**Pass =** `postgres ready after Ns`, then `CREATE DATABASE`, then `connected to o3k_test`.

### 1.2 Build the binary

```bash
make build
ls -la bin/o3k
```

**Pass =** `bin/o3k` exists, roughly 63–66 MB.

### 1.3 Write a stub-storage config

`config/o3k.yaml` ships `storage_mode: local` for Cinder and Glance, which writes under
`/var/lib/o3k/{volumes,images}`. That directory is root-owned and absent on a fresh host, and
`pkg/paths.Base` is a compile-time constant with no override — so local mode would fail volume and image
creation with a `503`. Assumption A8 sanctions stub mode, which is sufficient for every fix here. The
`sed` rewrites exactly the two `storage_mode: local` lines (Cinder at 81, Glance at 87).

```bash
sed 's/^  storage_mode: local/  storage_mode: stub/' config/o3k.yaml > /tmp/o3k-hosttest.yaml
grep -nE '^(cinder|glance):|^  storage_mode:' /tmp/o3k-hosttest.yaml
```

**Pass =** both `storage_mode:` lines read `stub`.

### 1.4 Start O3K — **Shell T1 (server)**

Leave this shell running. The binary applies migrations itself on startup (`cmd/o3k/main.go:104`), so
there is no separate migrate step. The PID goes to a file for teardown.

**Shell T1 (server)** — leave this running:

```bash
./bin/o3k --config /tmp/o3k-hosttest.yaml > /tmp/o3k-hosttest.log 2>&1 & echo $! > /tmp/o3k-hosttest.pid
sleep 10
grep -E 'Migrations applied|Cinder initialized|Glance initialized' /tmp/o3k-hosttest.log
for p in 35357 8774 9696 8776 9292; do printf 'port %s: %s\n' $p "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:$p/)"; done
```

**Pass =** `Migrations applied successfully` with `version: 65`, both services `initialized in stub
mode`, and all five ports answer `200`.

### 1.5 Authenticate — **Shell T2 (client)**

Every remaining block runs here. Seed credentials come from `migrations/002_seed_data.up.sql`: user
`admin` / `secret`, project `default` (`00000000-0000-0000-0000-000000000002`).

**Shell T2 (client)**:

```bash
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
export AT=$(openstack token issue -f value -c id)
export P=00000000-0000-0000-0000-000000000002
echo "token length: ${#AT}"
```

**Pass =** `token length: 373` (any length over 300 is fine). A length of 0 means T1 is not up.

---

## 2. Test steps

All blocks run in **T2** unless stated. Each step names the value the defect produced *before* this
feature, so a pass is unambiguous.

### 2.1 B5/P1 — tenant creation with the default-domain alias

The headline defect: `domain_id: "default"` — what every OpenStack client sends, because upstream
Keystone's Default domain literally carries that id — reached a `uuid` column and surfaced as a generic
`500`.

```bash
openstack project create --domain default probe -f value -c id
curl -s -o /dev/null -w 'user create with alias:      %{http_code}\n' -X POST http://localhost:35357/v3/users -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"user":{"name":"probe-u","password":"x","domain_id":"default"}}'
curl -s -o /dev/null -w 'group create with alias:     %{http_code}\n' -X POST http://localhost:35357/v3/groups -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"group":{"name":"probe-g","domain_id":"default"}}'
curl -s -o /dev/null -w 'project, omitted domain:     %{http_code}\n' -X POST http://localhost:35357/v3/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-omitted"}}'
```

**Expect:** a UUID printed for `probe` (was `500`); then `201`, `201`, `201`. The user and group lines
were `400 specified domain not found or disabled` and `500` respectively before this feature.

Now the rejected payloads. Each must be a **client** error naming the cause — never a `5xx` (SC-002).

```bash
curl -s -X POST http://localhost:35357/v3/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-bad-domain","domain_id":"11111111-1111-1111-1111-111111111111"}}' | jq -c .
curl -s -o /dev/null -w 'unknown domain:   %{http_code}\n' -X POST http://localhost:35357/v3/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-bad-domain","domain_id":"11111111-1111-1111-1111-111111111111"}}'
curl -s -o /dev/null -w 'non-identifier:   %{http_code}\n' -X POST http://localhost:35357/v3/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-bad-domain2","domain_id":"not-a-domain"}}'
curl -s -X POST http://localhost:35357/v3/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe","domain_id":"default"}}' | jq -c .
curl -s -o /dev/null -w 'duplicate name:   %{http_code}\n' -X POST http://localhost:35357/v3/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe","domain_id":"default"}}'
```

**Expect:** the first body is `{"badRequest":{"code":400,"message":"specified domain not found or
disabled"}}` — a message naming the **domain** (FR-002). Then `unknown domain: 400`,
`non-identifier: 400`, a `{"conflict":…}` body naming `probe`, and `duplicate name: 409`. All four were
`500` before.

Finally, the alias must never be echoed back — responses keep reporting the **stored UUID** (FR-007/A2,
the thing the identity contract suite guards).

```bash
openstack project show probe -f value -c domain_id
```

**Expect:** `00000000-0000-0000-0000-000000000100`, **not** the string `default`.

### 2.2 B5 — domain retrieval by alias, and the name filter

Standard clients resolve a `--domain` argument client-side *before* sending any creation request: they
retrieve the domain by the given reference, then fall back to a name lookup. Both legs were broken —
retrieval `404`d and the list ignored every filter.

```bash
curl -s -o /dev/null -w 'GET /v3/domains/default:  %{http_code}\n' -H "X-Auth-Token: $AT" http://localhost:35357/v3/domains/default
curl -s -H "X-Auth-Token: $AT" http://localhost:35357/v3/domains/default | jq -c '.domain | {id, name, enabled}'
echo "?name=Default   -> $(curl -s -H "X-Auth-Token: $AT" 'http://localhost:35357/v3/domains?name=Default' | jq '.domains|length')"
echo "?name=default   -> $(curl -s -H "X-Auth-Token: $AT" 'http://localhost:35357/v3/domains?name=default' | jq '.domains|length')"
echo "?name=zzz-nope  -> $(curl -s -H "X-Auth-Token: $AT" 'http://localhost:35357/v3/domains?name=zzz-nope' | jq '.domains|length')"
echo "unfiltered      -> $(curl -s -H "X-Auth-Token: $AT" http://localhost:35357/v3/domains | jq '.domains|length')"
```

**Expect:** `200` (was `404`); the domain is `{"id":"00000000-0000-0000-0000-000000000100",
"name":"Default","enabled":true}`; then `?name=Default -> 1`, `?name=default -> 0` (names stay
case-sensitive), **`?name=zzz-nope -> 0`**, `unfiltered -> 1`.

`?name=zzz-nope -> 0` is the discriminator. Before this feature it returned `1` — the filter was ignored
entirely, so a client's name lookup silently received every domain.

Non-identifier values must still get #515's verbatim 404, now produced in the handler rather than the
middleware, and the sibling domain routes must still get it from the middleware.

```bash
curl -s -H "X-Auth-Token: $AT" http://localhost:35357/v3/domains/not-a-uuid | jq -c .
curl -s -o /dev/null -w 'PATCH not-a-uuid:  %{http_code}\n' -X PATCH http://localhost:35357/v3/domains/not-a-uuid -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"domain":{"name":"x"}}'
```

**Expect:** `{"itemNotFound":{"code":404,"message":"Could not find domain: not-a-uuid."}}` and
`PATCH not-a-uuid: 404`. The exception is one route, one method, one alias.

> **Known unrelated gap.** `openstack user create --domain default …` and `openstack domain show default`
> print `'links'` and exit non-zero even though the API returns `201`/`200` and the resource *is*
> created — python-openstackclient's display code expects a `links` field that O3K's identity
> `users`/`domains` responses omit. It is pre-existing and unrelated: `openstack domain show <UUID>`, a
> path this feature never touches, fails identically. That is why this section uses `curl` for those two
> and the CLI only for `project create`, whose response does carry `links`. Recorded as observation O8.

### 2.3 Two tenants with genuine member tokens (fixture for 2.4)

**Read this before running it.** Selecting the role correctly is the whole point of the block.
`GET /v3/roles?name=…` **ignores the name filter** and returns every role, so `.roles[0].id` is `admin`
(alphabetically first) — and there is no `_member_` role at all: O3K seeds `admin`, `member`, `reader`.
Getting this wrong grants admin to both tenants, and an admin legitimately bypasses the check under test,
so §2.4 would report a breach that does not exist. Select by name, client-side.

```bash
mkp() { curl -s -X POST "http://localhost:35357/v3/$1" -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d "$2"; }
export PA=$(mkp projects '{"project":{"name":"tenant-a","domain_id":"default"}}' | jq -r '.project.id')
export PB=$(mkp projects '{"project":{"name":"tenant-b","domain_id":"default"}}' | jq -r '.project.id')
export UA=$(mkp users '{"user":{"name":"user-a","password":"pw-a","domain_id":"default"}}' | jq -r '.user.id')
export UB=$(mkp users '{"user":{"name":"user-b","password":"pw-b","domain_id":"default"}}' | jq -r '.user.id')
export MEMBER=$(curl -s -H "X-Auth-Token: $AT" http://localhost:35357/v3/roles | jq -r '.roles[] | select(.name=="member") | .id')
echo "PA=$PA PB=$PB UA=$UA UB=$UB member=$MEMBER"
curl -s -o /dev/null -w 'role assign A: %{http_code}\n' -X PUT "http://localhost:35357/v3/projects/$PA/users/$UA/roles/$MEMBER" -H "X-Auth-Token: $AT"
curl -s -o /dev/null -w 'role assign B: %{http_code}\n' -X PUT "http://localhost:35357/v3/projects/$PB/users/$UB/roles/$MEMBER" -H "X-Auth-Token: $AT"
```

**Expect:** five non-empty UUIDs, `member=00000000-0000-0000-0000-000000000004`, and both role
assignments `204`. If `member` came back empty, the seed differs — stop and investigate rather than
falling back to `.roles[0]`.

Mint the scoped tokens and prove they are **not** admin.

```bash
tok() { curl -i -s -X POST http://localhost:35357/v3/auth/tokens -H 'Content-Type: application/json' -d "{\"auth\":{\"identity\":{\"methods\":[\"password\"],\"password\":{\"user\":{\"name\":\"$1\",\"password\":\"$2\",\"domain\":{\"name\":\"Default\"}}}},\"scope\":{\"project\":{\"id\":\"$3\"}}}}" | grep -i '^X-Subject-Token:' | awk '{print $2}' | tr -d '\r'; }
export TA=$(tok user-a pw-a "$PA")
export TB=$(tok user-b pw-b "$PB")
echo "TA length ${#TA}, TB length ${#TB}"
python3 -c 'import base64,json,sys;p=sys.argv[1].split(".")[1];print("roles:",json.loads(base64.urlsafe_b64decode(p+"="*(-len(p)%4)))["roles"])' "$TB"
```

**Expect:** both lengths over 300, and `roles: ['member']`. If it prints `['admin']` the role selection
above went wrong and §2.4 is invalid.

Create the three networks the matrix needs — one private and one shared owned by tenant A, one owned by
tenant B.

```bash
export NA=$(curl -s -X POST http://localhost:9696/v2.0/networks -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d '{"network":{"name":"net-a"}}' | jq -r '.network.id')
export NS=$(curl -s -X POST http://localhost:9696/v2.0/networks -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d '{"network":{"name":"net-a-shared","shared":true}}' | jq -r '.network.id')
export NB=$(curl -s -X POST http://localhost:9696/v2.0/networks -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"network":{"name":"net-b"}}' | jq -r '.network.id')
echo "NA=$NA NS=$NS NB=$NB"
```

**Pass =** three non-empty UUIDs.

### 2.4 B8 — the cross-tenant subnet write 🔒

The parent issue's only security defect. Before this feature every row below returned `201`: any tenant
could choose the address range, gateway and DHCP behaviour on any other tenant's network, and the
victim's own views then advertised it as theirs.

```bash
sub() { curl -s -o /dev/null -w '%{http_code}' -X POST http://localhost:9696/v2.0/subnets -H "X-Auth-Token: $1" -H 'Content-Type: application/json' -d "{\"subnet\":{\"name\":\"$2\",\"network_id\":\"$3\",\"cidr\":\"$4\",\"ip_version\":4}}"; }
printf '%-38s %s   want 404\n' "foreign PRIVATE network" "$(sub "$TB" intruder-priv "$NA" 10.199.1.0/24)"
printf '%-38s %s   want 403\n' "foreign SHARED network" "$(sub "$TB" intruder-shar "$NS" 10.199.2.0/24)"
printf '%-38s %s   want 404\n' "nonexistent network id" "$(sub "$TB" ghost 11111111-1111-1111-1111-111111111111 10.199.3.0/24)"
printf '%-38s %s   want 400\n' "malformed network id" "$(sub "$TB" bad not-a-uuid 10.199.4.0/24)"
printf '%-38s %s   want 201\n' "tenant B on its OWN network" "$(sub "$TB" ok-own "$NB" 10.199.5.0/24)"
printf '%-38s %s   want 201\n' "admin on tenant A's network" "$(sub "$AT" ok-admin "$NA" 10.199.6.0/24)"
```

**Expect:** exactly `404, 403, 404, 400, 201, 201`.

`404` for the private network and `403` for the shared one is the deliberate distinction (FR-010a): a
network the caller cannot see must not be confirmed to exist, but a *shared* network is already visible
in their own network list, so `404` there would be a lie they could detect. A shared network grants the
right to *attach to* it, never to define its address ranges — which is why this handler is stricter than
its `CreatePort` neighbour.

The last two rows matter as much as the first two: a handler that simply refused everything would pass
the refusal checks (SC-004).

Now the victim's views, and the audit record.

```bash
echo "tenant A subnet names: $(curl -s -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets | jq -c '[.subnets[].name]')"
echo "intruder subnets:      $(curl -s -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets | jq '[.subnets[]|select(.name|startswith("intruder"))]|length')"
echo "audit warnings:        $(grep -c 'refused cross-tenant subnet write' /tmp/o3k-hosttest.log)"
grep 'refused cross-tenant subnet write' /tmp/o3k-hosttest.log | jq -c '{caller_project, network_id, network_owner, network_shared}'
```

**Expect:** tenant A's list is `["ok-admin"]` — the admin's legitimate subnet only; `intruder subnets:
0` (SC-003, no residue); `audit warnings: 2` — one per refusal (SC-004b); and each record names
`caller_project`, `network_id`, `network_owner` and `network_shared`. Legitimate creations log nothing.

### 2.4a FR-013a — the owner may inspect and remove what an admin put on their network

`ok-admin` above is the case. It sits on tenant A's network but belongs to the **admin** project, because
subnet creation attributes the row to the caller (as upstream does). Tenant A's listing shows it — the
line you just read proves that. Until this fix, tenant A got `404` fetching it by id and could not delete
it: visible, uninspectable, undeletable. Upstream grants the network's owner both, so this is parity, not
a new rule. Added after the first host pass — **not yet run on a real stack**.

```bash
export SID=$(curl -s -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets | jq -r '.subnets[]|select(.name=="ok-admin")|.id')
echo "ok-admin id: $SID"
printf '%-34s %s   want 200\n' "owner fetches it by id" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets/$SID)"
printf '%-34s %s   want 200\n' "owner renames it" "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d '{"subnet":{"name":"renamed-by-owner"}}' http://localhost:9696/v2.0/subnets/$SID)"
echo "rename actually stored:    $(curl -s -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets/$SID | jq -r '.subnet.name')   want renamed-by-owner"
echo "tenant_id on the by-id read: $(curl -s -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets/$SID | jq -r '.subnet.tenant_id')   want the ADMIN project, not $PA"
echo "tenant_id in the listing:    $(curl -s -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets | jq -r '.subnets[]|select(.id=="'$SID'")|.tenant_id')   want the same"
printf '%-34s %s   want 404\n' "tenant B fetches it" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TB" http://localhost:9696/v2.0/subnets/$SID)"
printf '%-34s %s   want 404\n' "tenant B renames it" "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"name":"hijacked"}}' http://localhost:9696/v2.0/subnets/$SID)"
printf '%-34s %s   want 404\n' "tenant B deletes it" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "X-Auth-Token: $TB" http://localhost:9696/v2.0/subnets/$SID)"
printf '%-34s %s   want 204\n' "owner deletes it" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets/$SID)"
echo "gone from the listing: $(curl -s -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets | jq '[.subnets[]|select(.name=="renamed-by-owner")]|length')   want 0"
```

**Expect:** `200`, `200`, `renamed-by-owner`, the admin project id, then `404, 404, 404, 204`, then `0`.
Before this fix every status was `404` and the subnet survived.

Three lines carry more weight than their status codes. **`rename actually stored`** — a `200` that
changes nothing was the first version of this fix, because the entitlement query and the `UPDATE`
disagreed about scope; the same trap sat in `DELETE`. And **both `tenant_id` lines** — all three handlers
used to report the caller's project, which was right only while each could return nothing but the
caller's own rows. The listing line is here because the first host run passed the by-id read and still
had the listing wrong; the unit tests were green throughout, because none of them read `tenant_id` back
through a `GET`.
Tenant B's three `404`s are the control: widening to the *network owner* must not widen to everyone.

Sharing stays read-only, which is upstream's split (`rule:shared` is in `get_subnet` and not in
`delete_subnet`):

```bash
export SSID=$(curl -s -X POST http://localhost:9696/v2.0/subnets -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d "{\"subnet\":{\"name\":\"shared-sub\",\"network_id\":\"$NS\",\"cidr\":\"10.199.8.0/24\",\"ip_version\":4}}" | jq -r '.subnet.id')
printf '%-34s %s   want 200\n' "B reads a shared net subnet" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TB" http://localhost:9696/v2.0/subnets/$SSID)"
printf '%-34s %s   want 404\n' "B deletes a shared net subnet" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "X-Auth-Token: $TB" http://localhost:9696/v2.0/subnets/$SSID)"
curl -s -o /dev/null -w 'owner cleans up: %{http_code}\n' -X DELETE -H "X-Auth-Token: $TA" http://localhost:9696/v2.0/subnets/$SSID
```

**Expect:** `200`, `404`, `204`.

### 2.5 B2 — the project-scoped volume routes

Cinder registers most operations twice: a token-scoped form and the project-in-path form. `GET` was
missing from the latter for volumes (a `405`), and three sub-resource names were being swallowed by a
sibling `:id` route — two of them answering `500`.

```bash
for u in volumes volumes/detail snapshots/detail types/default backups/detail; do printf '%-18s %s\n' "$u" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $AT" "http://localhost:8776/v3/$P/$u")"; done
```

**Expect:** `volumes 200` (was `405`), `volumes/detail 200` (was a `404` lookup for a volume named
"detail"), `snapshots/detail 200` (was `500`), `types/default 200` (was `500`), and
**`backups/detail 404` — unchanged by design**. That last one is not shadowed: the project-scoped group
registers no backup route at all, so it already answered an honest 404 and stays out of scope (FR-016a).

Both URL forms must return the same collection for the same caller, and the path segment must not widen
what anyone can see.

```bash
curl -s -o /dev/null -w 'create a volume: %{http_code}\n' -X POST "http://localhost:8776/v3/$P/volumes" -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"volume":{"name":"probe-vol","size":1}}'
curl -s -H "X-Auth-Token: $AT" http://localhost:8776/v3/volumes | jq -S '.volumes' > /tmp/o3k-flat.json
curl -s -H "X-Auth-Token: $AT" "http://localhost:8776/v3/$P/volumes" | jq -S '.volumes' > /tmp/o3k-scoped.json
diff -q /tmp/o3k-flat.json /tmp/o3k-scoped.json && echo "IDENTICAL" || echo "DIFFER"
echo "tenant B naming its own project:    $(curl -s -H "X-Auth-Token: $TB" "http://localhost:8776/v3/$PB/volumes" | jq '.volumes|length')"
echo "tenant B naming the admin project:  $(curl -s -H "X-Auth-Token: $TB" "http://localhost:8776/v3/$P/volumes" | jq '.volumes|length')"
```

**Expect:** `create a volume: 202`, then **`IDENTICAL`** (SC-005), then both tenant-B counts `0` and
equal — naming a foreign project in the path returns the caller's own collection, never the named
project's (FR-015). The segment is decoration: every handler in that group reads the project from the
token.

### 2.6 US4 — the multi-tenancy suite

This suite reported `Passed: 0 / Failed: 5` against `PROJECT1=null` before the feature: its setup could
not create a tenant (B5), nothing guarded that, and six sections ran against tenants that did not exist.

Remove the fixtures from §2.3 first, so the suite starts from a clean slate and its own names are free.

```bash
for s in $(curl -s -H "X-Auth-Token: $TB" http://localhost:9696/v2.0/subnets | jq -r '.subnets[].id'); do curl -s -o /dev/null -X DELETE "http://localhost:9696/v2.0/subnets/$s" -H "X-Auth-Token: $TB"; done
for s in $(curl -s -H "X-Auth-Token: $AT" http://localhost:9696/v2.0/subnets | jq -r '.subnets[].id'); do curl -s -o /dev/null -X DELETE "http://localhost:9696/v2.0/subnets/$s" -H "X-Auth-Token: $AT"; done
for n in "$NA" "$NS"; do curl -s -o /dev/null -X DELETE "http://localhost:9696/v2.0/networks/$n" -H "X-Auth-Token: $TA"; done
curl -s -o /dev/null -X DELETE "http://localhost:9696/v2.0/networks/$NB" -H "X-Auth-Token: $TB"
for u in "$UA" "$UB"; do curl -s -o /dev/null -X DELETE "http://localhost:35357/v3/users/$u" -H "X-Auth-Token: $AT"; done
for p in "$PA" "$PB"; do curl -s -o /dev/null -X DELETE "http://localhost:35357/v3/projects/$p" -H "X-Auth-Token: $AT"; done
echo "fixtures removed"
```

Run it twice, back to back, with no manual cleanup in between.

```bash
make test-multi-tenant 2>&1 | tail -8
echo "===== SECOND RUN ====="
make test-multi-tenant 2>&1 | tail -8
```

**Expect:** **`Passed: 13`, `Failed: 0`, `All multi-tenancy tests passed!`** — both times (SC-006,
SC-006a). Two consecutive runs agreeing with no manual cleanup is the point: teardown now runs from an
`EXIT` trap, so a crashed run cannot leave a fixed-name project behind to block the next one.

Five of those 13 checks are new in this feature. Confirm they are present and passing:

```bash
make test-multi-tenant 2>&1 | grep -E 'subnet to User1|no trace|own network|project-scoped URL|does not widen'
```

**Expect:** five `✓` lines — the cross-tenant subnet write refused, the victim's list clean, the
own-network control still working, the project-scoped volume list, and the foreign-project scoping
control.

Now prove the guards actually fire. Pre-creating `test-project-1` makes the suite's own create return
`409` (the new FR-004 behaviour), so `jq` yields `null` and setup must abort.

```bash
curl -s -o /dev/null -w 'pre-create: %{http_code}\n' -X POST http://localhost:35357/v3/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"test-project-1","domain_id":"default"}}'
make test-multi-tenant > /tmp/o3k-guard.log 2>&1; echo "exit code: $?"
grep -E 'SETUP FAILED|Refusing' /tmp/o3k-guard.log
echo "passes reported: $(grep -c '✓' /tmp/o3k-guard.log)"
```

**Expect:** `pre-create: 201`, `exit code: 2` (make's wrapper around the suite's `1`),
`SETUP FAILED: could not create project test-project-1 (got 'null')` followed by
`Refusing to run isolation checks against resources that do not exist.`, and
**`passes reported: 0`** — no check ran, so no result was reported (FR-017).

Clear the deliberate leftover and confirm the suite recovers. The trap correctly does *not* delete a
project the run did not create.

```bash
export STALE=$(curl -s -H "X-Auth-Token: $AT" http://localhost:35357/v3/projects | jq -r '.projects[]|select(.name=="test-project-1")|.id')
curl -s -o /dev/null -X DELETE "http://localhost:35357/v3/projects/$STALE" -H "X-Auth-Token: $AT"
make test-multi-tenant 2>&1 | tail -6
```

**Expect:** `Passed: 13`, `Failed: 0` again.

### 2.7 SC-008 — the Horizon compatibility suite

This suite failed on B2 before the feature: it calls `GET /v3/{project}/volumes` (the `405`) and
`GET /v3/{project}/volumes/detail` (the shadowed route). It also could not report more than one failure
— `set -e` plus `((FAILED++))`, which exits 1 when the counter is 0, killed it at the first one. Both
are fixed, so SC-008's "failing to passing" is observable rather than assumed.

```bash
bash test/horizon_compat_test.sh > /tmp/o3k-horizon.log 2>&1; echo "exit code: $?"
grep -E 'Total Passed|Total Failed' /tmp/o3k-horizon.log
grep -E 'Cinder - List volumes|Listed .* volumes' /tmp/o3k-horizon.log
```

**Expect:** `exit code: 0`, `Total Passed: 18`, `Total Failed: 0`, and both volume lines showing
`[PASS]`. Before: exit 1 after 5 passes, dead at `[FAIL] Cinder - List volumes`.

### 2.8 SC-008 — the full gate set

Nothing previously passing may regress. The identity contract suite is the guard for FR-007/A2 — it
asserts the stored domain UUID in five places, so it fails if accepting the alias on input ever starts
changing what responses return. It targets `localhost:35357`, so it must run against **this** build.

```bash
export O3K_TEST_DB_URL='postgres://o3k:secret@localhost:5432/o3k_test?sslmode=disable'
make test 2>&1 | grep -cE '^FAIL'
make lint 2>&1 | tail -1
make test-integration 2>&1 | grep -E 'Passed:|Failed:' | tail -2
go test ./test/contract/keystone/... ./test/contract/neutron/... ./test/contract/cinder/... 2>&1 | tail -4
```

**Expect:** `0` failing packages, `0 issues.`, `Passed: 28` with `Failed: 0`, and three `ok` lines.

Two confirmations that the change stayed in its lane:

```bash
git status --short migrations/ go.mod go.sum
echo "exit above should print nothing"
```

**Expect:** no output — **no migration** was added (A9) and **no dependency** (`go.mod`/`go.sum`
untouched).

> **Known unrelated failure.** `go test ./test/contract/glance/...` fails
> `TestGlanceDownloadImageData_Contract` under this runbook: §1.3 sets `glance.storage_mode: stub`, which
> returns zero-filled bytes of the right length instead of the uploaded content. The test needs
> `local`, hence root-owned `/var/lib/o3k/images`. Not a code defect, and unrelated to this feature —
> which is why it is excluded from the block above.

---

## 3. Teardown steps

Removes everything this run created and leaves the host as it was. Idempotent.

```bash
kill $(cat /tmp/o3k-hosttest.pid) 2>/dev/null || true
sleep 2
make db-down 2>/dev/null || true
rm -f /tmp/o3k-hosttest.yaml /tmp/o3k-hosttest.pid /tmp/o3k-hosttest.log /tmp/o3k-guard.log /tmp/o3k-horizon.log /tmp/o3k-flat.json /tmp/o3k-scoped.json
unset OS_AUTH_URL OS_USERNAME OS_PASSWORD OS_PROJECT_NAME OS_USER_DOMAIN_NAME OS_PROJECT_DOMAIN_NAME OS_IDENTITY_API_VERSION AT P PA PB UA UB TA TB NA NS NB MEMBER STALE O3K_TEST_DB_URL PGPASSWORD
echo "remaining o3k processes: $(pgrep -cx o3k 2>/dev/null)"
echo "remaining o3k-postgres:  $(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -c o3k-postgres)"
```

**Pass =** both counts `0`. The process check matches the executable **name** (`pgrep -x`) rather than
the full command line: `pgrep -f bin/o3k` would match the checking shell's own command line and report a
phantom process. Neither count uses `|| echo 0` — `pgrep -c` and `grep -c` already print `0`, and they
also exit non-zero when they do, so the fallback would print `0` twice. Every database (`o3k`, `o3k_test`) goes with the container — `make db-up`
mounts no volume — so no fixture cleanup is needed.

Optionally remove the build artifact and the CLI tools §0 installed. **Only do this if the host did not
have them before.**

```bash
make clean
sudo apt-get remove -y python3-openstackclient postgresql-client jq
```

---

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `token length: 0` in §1.5 | T1 is not up, or died. `tail -30 /tmp/o3k-hosttest.log`. A `Failed to bind` line means another process holds a port — O3K binds all listeners up front and `log.Fatalf`s on the first conflict. |
| `Failed to bind :8778` | Something else holds the Placement port. Unlike the other five services, Placement's port is hard-coded (`cmd/o3k/main.go:715`) with no config override, so two O3K instances cannot coexist on one host. Stop the other one (observation O6). |
| `503 failed to create volume backing` | `storage_mode` is still `local`. Redo §1.3 and restart T1 with `/tmp/o3k-hosttest.yaml`. |
| §2.4 returns `201` where `404`/`403` is expected | The tenant token carries `admin`. Re-read §2.3: `?name=` is ignored on `/v3/roles`, so `.roles[0]` is `admin`, and `_member_` does not exist. Re-check with the `python3` decode line — it must print `['member']`. |
| `openstack …` exits with `'links'` | Expected for `user create` and `domain show`: those responses omit a `links` field the CLI's display code wants. The API call succeeded — verify with `curl`. Observation O8, pre-existing. |
| `make db-up` fails with a name conflict | A previous `o3k-postgres` container is still around. `make db-down` then retry; §1.1 already does this. |
| `make test` skips DB-backed tests | `O3K_TEST_DB_URL` is unset in this shell. Export it as in §2.8. |
| The multi-tenancy suite aborts at `SETUP FAILED` unexpectedly | A leftover `test-project-1`/`test-project-2` from an aborted run. Delete it as in the last block of §2.6 — the trap deliberately does not remove projects the run did not create. |
