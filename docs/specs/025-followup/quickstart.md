# Quickstart: SPEC-025 Follow-ups

How to **see each defect**, and what the fix changes. Every block below reproduces a live failure
against the current code — none of them needs real mode, since all seven findings are in API and
database layers.

**Why this file leads with reproduction**: every defect here answers with a success code, so "it
works" is what the current code already reports. The only way to know a fix landed is to have seen
the failure first.

## Setup

```
export LIGHTSTACK_JWT_SECRET=dev-secret-change-me
docker compose -f deployments/docker-compose.yml up -d
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret
export OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
T=$(openstack token issue -f value -c id)
P=$(openstack project show default -f value -c id)
```

---

## US1 — the server listing ignores every filter

```
openstack server create --flavor m1.small --image cirros alpha
openstack server create --flavor m1.small --image cirros beta

curl -s -H "X-Auth-Token: $T" "http://localhost:8774/v2.1/servers?name=alpha" | jq '.servers[].name'
curl -s -H "X-Auth-Token: $T" "http://localhost:8774/v2.1/servers?name=nope"  | jq '.servers[].name'
```

**Now**: both return `alpha` and `beta`. **After**: the first returns `alpha`, the second returns
nothing.

**The trap this defect sets** — worth running, because it is how the bug was found:

```
openstack server list --name alpha -f json | jq -c '.[0].Networks'
```

Reads as a fact about `alpha`. It is `beta`'s address, because `[0]` is whichever server sorts first
in an unfiltered list.

```
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" \
  "http://localhost:8774/v2.1/servers?host=compute-01"
```

**Now**: `200` with every server. **After**: `400` naming the filter.

---

## US2 — the image patch discards `add`

```
IMG=$(openstack image list -f value -c ID | head -1)

curl -s -X PATCH -H "X-Auth-Token: $T" \
  -H 'Content-Type: application/openstack-images-v2.1-json-patch' \
  -d '[{"op":"add","path":"/min_disk","value":7}]' \
  "http://localhost:9292/v2/images/$IMG" | jq '.min_disk'
openstack image show $IMG -f value -c min_disk
```

**Now**: `200`, and `min_disk` is still `0`. Nothing is logged at either end. **After**: `7`.

The CLI path, which is the one that matters:

```
openstack image set --min-disk 10 $IMG && openstack image show $IMG -f value -c min_disk
```

**Now**: reports success, value unchanged — `image set` sends `add`. **After**: `10`.

```
curl -s -o /dev/null -w '%{http_code}\n' -X PATCH -H "X-Auth-Token: $T" \
  -H 'Content-Type: application/openstack-images-v2.1-json-patch' \
  -d '[{"op":"remove","path":"/min_disk"}]' "http://localhost:9292/v2/images/$IMG"
```

**Now**: `200`, unchanged. **After**: `403` naming the path.

---

## US3 — no quota is enforced anywhere

```
openstack quota set --volumes 1 default
openstack volume list -f value -c ID | wc -l          # already at or over 1?
openstack volume create --size 1 over-limit-1
openstack volume create --size 1 over-limit-2
openstack volume list -f value -c ID | wc -l
```

**Now**: every creation succeeds regardless of the limit. **After**: `413` once the allowance is
gone.

The path a handler-level check would miss:

```
openstack server create --flavor m1.small --image cirros \
  --boot-from-volume 1 boot-past-quota
openstack volume list -f value -c ID | wc -l
```

**Now**: a volume appears even at the limit. **After**: the boot is refused and no volume exists.

The counting change, visible without creating anything:

```
V=$(openstack volume create --size 5 snap-src -f value -c id)
openstack volume snapshot create --volume $V snap-1
openstack quota show default -f value -c gigabytes    # limit
curl -s -H "X-Auth-Token: $T" "http://localhost:8776/v3/$P/limits" \
  | jq '.limits.absolute.totalGigabytesUsed'
```

**Now**: `5` — the snapshot's 5 GB is not counted. **After**: `10`. This number is user-visible, so
a project holding snapshots can read as over its limit the day this ships.

---

## US4 — the catalog ignores the host you addressed

```
docker compose -f deployments/docker-compose.yml exec -T lightstack \
  sh -c 'echo $LIGHTSTACK_ENDPOINT_HOST'

curl -s -X POST -H 'Content-Type: application/json' -H 'Host: lightstack.example.com:35357' \
  -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin",
      "domain":{"name":"Default"},"password":"secret"}}},
      "scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}' \
  http://localhost:35357/v3/auth/tokens \
  | jq -r '.token.catalog[] | select(.type=="compute") | .endpoints[].url'
```

**Now**: names the configured host (or `localhost`), not `lightstack.example.com`. **After**:
`http://lightstack.example.com:8774/v2.1` — the addressed host, the endpoint's own port.

**The failure this causes**, if the configured host is unresolvable to the client: every command
fails at catalog lookup, *before* any version document is read — one step earlier than the
self-links SPEC-025 fixed.

**Check the port is not doubled** after the fix: no endpoint may read `…:35357:8774`.

---

## US5 — an action that cannot succeed

```
S=$(openstack server list -f value -c ID | head -1)
curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' \
  -d '{"restore": null}' "http://localhost:8774/v2.1/servers/$S/action" | jq .
```

**Now**: `409` — the server is not soft-deleted. No server can ever be, since the status the handler
gates on is never written; an upstream deployment at its default reclaim interval answers the same.

```
grep -rn "SOFT_DELETED" internal/ --include='*.go'
```

One hit: the comparison itself. **After**: still `409` for a live server and `404` for a deleted
one — upstream's answers, never the unknown-action `400` — and the grep returns nothing, since the
unreachable success branch is gone.

---

## US6 — microversions are advertised, not honoured

```
curl -s -H "X-Auth-Token: $T" -H 'OpenStack-API-Version: compute 2.1' \
  -D- -o /dev/null "http://localhost:8774/v2.1/servers/detail" | grep -i api-version

curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" \
  -H 'OpenStack-API-Version: compute 2.99' "http://localhost:8774/v2.1/servers"
```

**Now**: the header echoes the maximum whatever was asked for, and an out-of-range version is
accepted. **After**: the header names the version applied, and 2.99 is `406`.

**Test explicitly.** A request with **no** header resolves to the *maximum* in this product (upstream
uses the minimum, and FR-040 keeps Lightstack's behaviour), so a test relying on the default proves nothing
about a gate.

---

## US7 — N address queries for N servers

```
docker compose -f deployments/docker-compose.yml logs lightstack --since 1m \
  | grep -c 'SELECT p.network_id'      # if query logging is on
curl -s -H "X-Auth-Token: $T" "http://localhost:8774/v2.1/servers/detail" \
  | jq '.servers | length'
```

**Now**: address queries scale with the server count. **After**: one, regardless.

**After the change, confirm the output did not shift**: each server's `addresses` must match
`openstack server show <id> -f json | jq .addresses` exactly, floating IPs included.

---

## Gates before the work is done

```
make fmt && make lint
go test ./...
./test/nova_gaps_test.sh
./test/quota_test.sh
./test/spec007_catalog_scheme_test.sh
```

One data migration, `073` (volume transfers), applies with the others; there is nothing else to apply.

**And the gate that matters most**: every new test must have been *seen to fail* against the current
code. All seven of these defects answer with a success status, so a test that only exercises the
working path passes before the fix exists and proves nothing after it.
