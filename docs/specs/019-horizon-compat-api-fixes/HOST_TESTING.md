# Host testing guide — Horizon compat fixes F4/F5/F6/F9 (stub mode)

Validates the four SPEC-019 fixes (cloud-storage#532) against the all-in-one
`deployments/docker-compose-horizon.yml` stack (stub Lightstack + real Horizon + memcached + noVNC) — no
Kubernetes, KVM, or Ceph. This is the laptop-grade pass; the authoritative real-mode pass is
`HOST_TESTING_REAL_MODE.md`. The image is **built from source**, so it carries the F4/F5/F6/F9 handler
changes and migration `064`. Unlike the SPEC-018 *verification* runbook (which expected these findings to
reproduce), here each one must be **fixed**.

Assumptions: fresh Ubuntu host, single machine, stub mode. **Run all commands from the repo root.** Shell
blocks are zsh-copy-paste-safe.

**Validates**

| Finding | Success criterion | Section |
|---|---|---|
| F9 | Volume show/list carry non-null upstream fields; Volumes panel renders (no 500) — SC-001/002 | 2.1 |
| F5 | `volume` catalog endpoint resolves to the configured host, never `localhost` — SC-003 | 2.2 |
| F4 | Networks list once; `shared`/`router:external`/tenant filters honored — SC-004 | 2.3 |
| F6 | `image delete` removes the image; 404 for missing, 403 for non-owner — SC-005 | 2.4 |

## 0. Prerequisites

Assumes a Docker-compatible container engine (Docker, or **Podman + `podman-docker`** so `docker compose`
resolves) and the **OpenStack client** (`openstack`) are already installed — this runbook installs neither.
It adds only small unprivileged helpers, then prints versions.

```
command -v docker >/dev/null || command -v podman >/dev/null || echo "install docker or podman first"
command -v openstack >/dev/null || echo "install python-openstackclient first"
sudo apt-get update
sudo apt-get install -y jq curl
echo "versions:"; { docker --version || podman --version; }; openstack --version; jq --version; curl --version | head -1
```

**Pass =** a container engine, `openstack`, `jq`, and `curl` all report a version.

## 1. Setup steps

### 1.1 Build and start the stack

The Horizon container publishes host port 80, which rootless Podman blocks; the compose file parametrizes
it as `${HORIZON_HTTP_PORT:-80}`. Export a high port (Docker/root users may skip it). `LIGHTSTACK_JWT_SECRET` is
required by the compose file.

First confirm the Lightstack API ports are free — a prior Lightstack (a leftover stub compose, or a real-mode k3s pod)
holding them makes the `lightstack` container fail with `address already in use` (see Troubleshooting).

```
ss -ltn 2>/dev/null | grep -qE ':(35357|8774|9696|8776|9292|8775)\b' && echo "an Lightstack port is IN USE — free it first (see Troubleshooting)" || echo "Lightstack ports free"
export HORIZON_HTTP_PORT=8080
export LIGHTSTACK_JWT_SECRET=dev-secret-change-me
docker compose -f deployments/docker-compose-horizon.yml up -d --build
```

**Pass =** the build completes and the stack starts.

### 1.2 Wait for health and authenticate

The stub compose sets `LIGHTSTACK_ENDPOINT_HOST=lightstack`, so the catalog advertises `lightstack:<port>` URLs — correct for the
in-network Horizon container, but `lightstack` is not resolvable from the host. **Do NOT add `lightstack` to the host
`/etc/hosts`**: rootless podman seeds each container's `/etc/hosts` from the host's, so a `127.0.0.1 lightstack`
line leaks into the Horizon container and makes it resolve Keystone to its own loopback ("unable to establish
connection to keystone endpoint"). Instead, point the host CLI straight at the published ports with per-service
endpoint overrides (these bypass the catalog; the published ports are all on `localhost`).

```
docker compose -f deployments/docker-compose-horizon.yml ps
curl -fsS http://localhost:35357/v3 >/dev/null && echo "keystone OK"
curl -fsS "http://localhost:${HORIZON_HTTP_PORT:-80}/dashboard/auth/login/" >/dev/null && echo "horizon login OK"
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
export OS_VOLUME_ENDPOINT_OVERRIDE=http://localhost:8776/v3 OS_IMAGE_ENDPOINT_OVERRIDE=http://localhost:9292 OS_NETWORK_ENDPOINT_OVERRIDE=http://localhost:9696
openstack token issue -f value -c id >/dev/null && echo "auth OK"
```

There is deliberately no `OS_COMPUTE_ENDPOINT_OVERRIDE`: an override does not rescue the compute CLI, because
`keystoneauth` version discovery fails against Lightstack's `GET /v2.1` regardless (see §2.5). §2.5 talks to Nova
with `curl` instead. Keep this one shell for the whole run — every later section reuses these exports.

**Pass =** `ps` shows `lightstack` and `horizon` healthy; the two curls print OK; `auth OK` prints. Log into the
dashboard at `http://localhost:8080/dashboard` (User `admin`, Password `secret`, Domain `Default`).

## 2. Test steps

### 2.1 F9 — Volumes render once a volume exists

Create a volume and read it back at the raw API (avoids CLI field-name mapping), then load the panel.

```
openstack volume create --size 1 ht-vol >/dev/null && echo "created"
TOKEN=$(openstack token issue -f value -c id)
curl -fsS -H "X-Auth-Token: $TOKEN" "http://localhost:8776/v3/volumes/detail" | jq '.volumes[0] | {volume_type, availability_zone, encrypted, multiattach, consumes_quota, shared_targets, user_id, host: ."os-vol-host-attr:host", tenant: ."os-vol-tenant-attr:tenant_id", metadata}'
curl -fsS -H "X-Auth-Token: $TOKEN" "http://localhost:8776/v3/volumes/detail" | jq -e '.volumes[0] | (.volume_type=="__DEFAULT__") and (.availability_zone=="nova") and (.encrypted==false) and (.multiattach==false) and (.consumes_quota==true) and (.shared_targets==false) and (.user_id!=null) and (.["os-vol-host-attr:host"]!=null) and (.["os-vol-tenant-attr:tenant_id"]!=null) and (.metadata!=null)' && echo "F9 fields OK"
openstack volume show ht-vol -f value -c type
```

The volume endpoint in Lightstack's catalog has no project segment, so the raw path is `/v3/volumes/detail` (not
`/v3/<project>/volumes/detail`, which routes to the by-id handler and 404s).

**Expect:** the `jq` object shows every field non-null; `F9 fields OK` prints (`volume_type`=`__DEFAULT__`,
`availability_zone`=`nova`, booleans set); `volume show … type` prints `__DEFAULT__`. **In the browser:**
Project → Volumes lists `ht-vol` and the page returns HTTP 200 — **no** 500 / `TemplateSyntaxError` (the F9
regression). Empty-panel behavior is unchanged.

### 2.2 F5 — Block-storage catalog endpoint has no `localhost`

The stub host is `lightstack`; the `volume` alias endpoint (service `…015`, migration `064`) must now resolve to
`lightstack`, not `localhost`.

```
openstack catalog show volume -f json | jq -r '.endpoints[].url'
openstack catalog show volumev3 -f json | jq -r '.endpoints[].url'
openstack catalog show volume -f json | jq -e '[.endpoints[].url] | all(contains("localhost") | not)' && echo "F5 no-localhost OK"
openstack volume list -f value -c Name
```

**Expect:** every `volume` and `volumev3` URL uses `lightstack:8776` (the configured host); `F5 no-localhost OK`
prints; `volume list` reaches Cinder and lists `ht-vol` (proving the resolved endpoint works). *(There is no
`s3` service in stub — it is only advertised when RGW is configured; the s3 endpoint is covered by the real
pass.)*

### 2.3 F4 — Networks list once; filters honored

Create one project network, then exercise the filters at the raw Neutron API (what Horizon's Project panel
merges) and confirm the `router:external` attribute is present.

```
openstack network create ht-net >/dev/null && echo "created"
TOKEN=$(openstack token issue -f value -c id)
NET=http://localhost:9696/v2.0/networks
echo "unfiltered count:"; curl -fsS -H "X-Auth-Token: $TOKEN" "$NET" | jq '[.networks[] | select(.name=="ht-net")] | length'
echo "router:external present+false:"; curl -fsS -H "X-Auth-Token: $TOKEN" "$NET" | jq -e '.networks | all(has("router:external") and (.["router:external"]==false))' && echo "attr OK"
echo "router:external=true is empty:"; curl -fsS -H "X-Auth-Token: $TOKEN" "$NET?router:external=true" | jq -e '.networks == []' && echo "external-empty OK"
echo "shared=true excludes ht-net:"; curl -fsS -H "X-Auth-Token: $TOKEN" "$NET?shared=true" | jq -e '[.networks[].name] | contains(["ht-net"]) | not' && echo "shared-filter OK"
```

**Expect:** `ht-net` appears exactly once unfiltered; `attr OK` (every network has `router:external: false`);
`external-empty OK` (external filter returns nothing — Lightstack models no external networks); `shared-filter OK`
(a non-shared project net is excluded by `shared=true`). **In the browser:** Project → Network → Networks
shows `ht-net` **once** (not twice — the F4 regression).

### 2.4 F6 — Deleting an image actually removes it

Create a public image, delete it, and confirm it is gone and a second delete is a not-found.

```
head -c 1048576 /dev/zero > /tmp/ht.img
openstack image create ht-img --disk-format qcow2 --container-format bare --public --file /tmp/ht.img >/dev/null && echo "created"
ID=$(openstack image show ht-img -f value -c id)
openstack image delete "$ID" && echo "delete returned success"
openstack image list -f value -c Name | grep -qx ht-img && echo "STILL PRESENT (F6 FAIL)" || echo "F6 gone OK"
TOKEN=$(openstack token issue -f value -c id)
curl -s -o /tmp/ht-del.out -w 'second-delete http=%{http_code}\n' -X DELETE -H "X-Auth-Token: $TOKEN" "http://localhost:9292/v2/images/$ID"; cat /tmp/ht-del.out; echo
openstack image delete "$ID" >/dev/null 2>&1 && echo "SECOND DELETE SUCCEEDED (F6 FAIL)" || echo "F6 second-delete errors OK"
```

**Expect:** `created`, `delete returned success`, then **`F6 gone OK`** (the image no longer appears in
`image list` — the F6 no-op is fixed), **`second-delete http=404`** with an `itemNotFound` body, and
**`F6 second-delete errors OK`** (the CLI exits non-zero). Assert the 404 at the raw API, not on the CLI's
message: `python-openstackclient` catches `ResourceNotFound` in `image delete` and reports it as
`"Multi Backend support not enabled."` (`openstackclient/image/v2/image.py`), so matching on `not found` /
`404` never succeeds however correct the server is. **In the browser:** Project → Compute → Images no
longer lists `ht-img` after the CLI delete refresh.

### 2.5 Metadata serves only the boot keypair (PR #78 review follow-up)

Not one of the four findings — added during the PR review. `instances.key_name` is persisted (migration
`063`) but the metadata service served **every** keypair the owner holds. Upstream Nova serves the boot
keypair alone. The stack publishes the metadata port (`8775`) and honours the `X-Instance-ID` test header.

Drive Nova with `curl`, not the CLI. The compute service is the one this runbook cannot reach through
`python-openstackclient`: without an endpoint override the catalog hands out `lightstack:8774` (unresolvable from
the host), and *with* one `keystoneauth` still runs version discovery against `GET /v2.1`, which Lightstack
answers without a `links` array — so `discover.versioned_data_for` returns an empty list and every compute
command dies with `IndexError: list index out of range`. That discovery gap is tracked as #547's B3 and is
not in this feature's scope. Nothing seeds a `cirros` image either, so create the boot image here (§2.4
deleted the only other one).

```
TOKEN=$(openstack token issue -f value -c id)
head -c 1048576 /dev/zero > /tmp/ht.img
IMG=$(openstack image create ht-boot-img --disk-format qcow2 --container-format bare --file /tmp/ht.img -f value -c id); echo "IMG=$IMG"
for K in ht-boot ht-other; do curl -s -o /dev/null -w "$K create http=%{http_code}\n" -X POST -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d "{\"keypair\":{\"name\":\"$K\"}}" http://localhost:8774/v2.1/os-keypairs; done
FLAV=$(curl -fsS -H "X-Auth-Token: $TOKEN" http://localhost:8774/v2.1/flavors | jq -r '.flavors[]|select(.name=="m1.small").id')
SID=$(curl -fsS -X POST -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"ht-md\",\"imageRef\":\"$IMG\",\"flavorRef\":\"$FLAV\",\"key_name\":\"ht-boot\"}}" http://localhost:8774/v2.1/servers | jq -r '.server.id'); echo "SID=$SID"
curl -fsS -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/servers/$SID" | jq -r '"key_name=" + .server.key_name'
curl -s -H "X-Instance-ID: $SID" http://localhost:8775/openstack/latest/meta_data.json | jq -c '.public_keys | keys'
curl -s -H "X-Instance-ID: $SID" http://localhost:8775/2009-04-04/meta-data/public-keys/; echo
curl -s -o /dev/null -w 'index 0 http=%{http_code}\n' -H "X-Instance-ID: $SID" http://localhost:8775/2009-04-04/meta-data/public-keys/0/openssh-key
curl -s -o /dev/null -w 'index 1 http=%{http_code}\n' -H "X-Instance-ID: $SID" http://localhost:8775/2009-04-04/meta-data/public-keys/1/openssh-key
curl -s -o /dev/null -X DELETE -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/servers/$SID"
for K in ht-boot ht-other; do curl -s -o /dev/null -X DELETE -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/os-keypairs/$K"; done
openstack image delete ht-boot-img 2>/dev/null || true
```

**Expect:** both keypairs `201`, `key_name=ht-boot` on the server read-back (migration `063` persisted it),
then `["ht-boot"]` (not `["ht-boot","ht-other"]`), `0=ht-boot` alone, `index 0 http=200` and **`index 1
http=404`**. `ht-other` belongs to the same user but was not the boot key, so it must not reach the guest.

## 3. Teardown steps

Removes the fixtures, the stack + volumes, and tmp files. Leaves the host as it was. (§2.5 already deletes its
own server, keypairs, and image; the `|| true` lines below cover a run that stopped early.)

```
openstack volume delete ht-vol 2>/dev/null || true
openstack network delete ht-net 2>/dev/null || true
openstack image delete ht-boot-img 2>/dev/null || true
docker compose -f deployments/docker-compose-horizon.yml down -v
rm -f /tmp/ht.img /tmp/ht-del.out
```

**Pass =** the stack is down and `docker compose ps` shows nothing for this project.

## Troubleshooting

- **`openstack volume/image/network` on the host cannot connect** — the §1.2 endpoint overrides
  (`OS_*_ENDPOINT_OVERRIDE`) aren't exported in this shell (they make the CLI bypass the catalog's `lightstack` host,
  which is only reachable inside the compose network); re-run the §1.2 `export` lines.
- **Horizon login: "Unable to establish connection to keystone endpoint"** (creds are fine) — the Horizon
  container can't reach `lightstack:35357`. Almost always a stale `127.0.0.1 lightstack` in the **host** `/etc/hosts` that
  rootless podman copied into the container, so `lightstack` resolves to Horizon's own loopback. Check with
  `podman exec lightstack-horizon getent hosts lightstack` (should be the lightstack container IP, not `127.0.0.1`). Fix:
  `sudo sed -i '/^127\.0\.0\.1 lightstack$/d;/^127\.0\.0\.1 lightstack-lightstack$/d' /etc/hosts` then
  `docker compose -f deployments/docker-compose-horizon.yml up -d --force-recreate horizon`.
- **Volumes panel still 500s** — the running image predates the fix; rebuild with
  `docker compose -f deployments/docker-compose-horizon.yml up -d --build` (the `--build` is required).
- **`catalog show volume` still shows `localhost`** — migration `064` didn't apply; check
  `docker compose logs lightstack | grep -i migrat` and confirm `migrations/064_fix_cinder_volume_alias_localhost.up.sql`
  is present in the build context.
- **`lightstack` container fails: `rootlessport listen tcp 0.0.0.0:8774: bind: address already in use`** — another
  Lightstack already owns the API ports. Find it: `ss -ltnp 2>/dev/null | grep -E ':(35357|8774)'` and
  `docker ps -a | grep -i lightstack`. If it's a stray stub stack, `docker compose -f deployments/docker-compose-horizon.yml down`.
  If it's a **real-mode k3s pod** (parent is a k3s `containerd-shim`), free the ports by removing that
  workload — `sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml helm uninstall horizon lightstack -n lightstack` then
  `sudo k3s kubectl delete ns lightstack` (the chart's pre-delete hook purges the VM/netns); or, if the k3s API is
  unresponsive, `sudo systemctl stop k3s` (stops the whole cluster + its VM). Then re-check the ports are
  free and re-run `docker compose … up -d` (no `--build` needed the second time).
