# Host testing guide — Horizon create/write UI enablement F2/F3/F7/F8 (stub mode)

Validates the SPEC-020 fixes (cloud-storage#533) against the all-in-one
`deployments/docker-compose-horizon.yml` stack (stub Lightstack + real Horizon + memcached + noVNC) — no
Kubernetes, KVM, or Ceph. This is the laptop-grade pass; the authoritative pass is
`HOST_TESTING_REAL_MODE.md`. The image is **built from source**, so it carries the quota/limits/capability
handler changes. Unlike the SPEC-018 *verification* runbook (which expected these findings to reproduce),
here each one must be **fixed** — and Horizon must stay **stock**: no config or policy override may be
added to make a control appear (FR-018).

Assumptions: fresh Ubuntu host, single machine, stub mode. **Run all commands from the repo root.** Shell
blocks are zsh-copy-paste-safe.

**Validates**

| Finding | Success criterion | Section |
|---|---|---|
| API surface | Every quota/limits/capability endpoint Horizon's create flows read returns 200, and limits agree across views — SC-005 | 2.1 |
| F2 | Project Overview usage/limits widget renders, no "Unable to retrieve limits information" — SC-004 | 2.2 |
| F3 | Project → Network → Networks offers **Create Network**; network created and listed once — SC-003 | 2.3 |
| F8 | Project → Volumes **Create Volume** dialog opens and creates a volume — SC-002 | 2.4 |
| F7 | Instances panel offers **Launch Instance**; wizard's Networks step has a selectable row; instance ACTIVE — SC-001 | 2.5 |
| Authz | No cross-project quota read/write; quota writes admin-only; phantom project 404s — SC-007 | 2.6 |

## 0. Prerequisites

Assumes a Docker-compatible container engine (Docker, or **Podman + `podman-docker`** so `docker compose`
resolves) and the **OpenStack client** (`openstack`) are already installed — this runbook installs neither.
It adds only small unprivileged helpers, then prints versions.

```bash
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

First confirm the Lightstack API ports are free — a leftover stub compose or a real-mode k3s pod holding them
makes the `lightstack` container fail with `address already in use` (see Troubleshooting).

**Remove §1.2's `/etc/hosts` alias before starting, if a previous run left it behind.** Podman seeds each
container's `/etc/hosts` from the **host's**, so a leftover `127.0.0.1 lightstack` line is copied into the Horizon
container, where `lightstack` then resolves to Horizon itself and every backend call fails with *Unable to
establish connection to keystone endpoint*. The order in this runbook is deliberate: containers first,
host alias second. Re-running `up -d` with the alias in place is the trap.

```bash
sudo sed -i "/spec020-hosttest/d" /etc/hosts
ss -ltn 2>/dev/null | grep -qE ':(35357|8774|9696|8776|9292|8775)\b' && echo "an Lightstack port is IN USE — free it first (see Troubleshooting)" || echo "Lightstack ports free"
export HORIZON_HTTP_PORT=8080
export LIGHTSTACK_JWT_SECRET=dev-secret-change-me
docker compose -f deployments/docker-compose-horizon.yml up -d --build
```

**Pass =** the build completes and the stack starts.

### 1.2 Wait for health and authenticate

The stub compose sets `LIGHTSTACK_ENDPOINT_HOST=lightstack`, so the catalog advertises `lightstack:<port>` URLs — correct for
the Horizon container, which calls Lightstack server-side over the compose network. The `openstack` CLI **follows
that catalog**, so from the host it would try to resolve `lightstack` and fail on every service except Keystone
(which uses the explicit `OS_AUTH_URL`). Map the name to loopback: the compose file publishes each Lightstack port
1:1 on the host, so `http://lightstack:9696` then reaches the published `9696`. Teardown removes the line again.

`OS_*_ENDPOINT_OVERRIDE` does **not** work as a substitute here, because of a known Lightstack defect:
`common.BaseURL` lets `LIGHTSTACK_ENDPOINT_HOST` outrank the request Host header, so every version document
advertises `links[].href = http://lightstack:<port>`. keystoneauth follows that self-link during discovery and
discards the override. Upstream OpenStack derives self-links from the request, which is why this works
against real Nova. Tracked as a follow-up; until it is fixed, the hosts entry is the only clean option.

`OS_COMPUTE_API_VERSION=2.1` is required, not cosmetic: Lightstack's `GET /v2.1` version object omits the `links`
array upstream Nova returns, so keystoneauth's microversion negotiation indexes an empty list and dies with
`list index out of range`. Pinning the version skips that negotiation. (`server list` and the Glance/Neutron
commands are unaffected — only the negotiating paths crash.) Tracked as a follow-up alongside the
`common.BaseURL` self-link defect.

Do **not** set `LIGHTSTACK_ENDPOINT_HOST=localhost` instead — Horizon resolves the same catalog from inside the
compose network, where `localhost` is the Horizon container itself.

The entry is tagged so teardown removes only the line this runbook added, leaving any pre-existing Lightstack
host aliases untouched.

**Add it only now, with the stack already running, and take it out again before any later `up -d`** —
under Podman it is inherited by every container created afterwards (see §1.1).

```bash
grep -q "spec020-hosttest" /etc/hosts || echo "127.0.0.1 lightstack spec020-hosttest" | sudo tee -a /etc/hosts
getent hosts lightstack
for i in $(seq 1 30); do curl -sf -o /dev/null http://localhost:35357/v3 && break; sleep 3; done
printf 'keystone:%s nova:%s neutron:%s cinder:%s glance:%s\n' "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:35357/v3)" "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8774/v2.1)" "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:9696/v2.0)" "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8776/v3)" "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:9292/v2/images)"
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
export OS_COMPUTE_API_VERSION=2.1
openstack token issue -f value -c project_id
openstack flavor list -f value -c Name >/dev/null && echo "nova reachable via the catalog"
```

**Pass =** keystone/nova/neutron/cinder report `200` and **glance reports `401`** (its `/v2/images` route
requires a token — an unauthenticated 401 proves the service is up and enforcing auth; only a connection
error or 404 is a failure). `getent hosts lightstack` resolves to `127.0.0.1`, `token issue` prints the project id
`00000000-0000-0000-0000-000000000002`, and `nova reachable via the catalog` confirms the CLI can follow
the advertised `lightstack:<port>` URLs.

### 1.3 Create the fixtures the create-UI tests need

The launch wizard needs an image, a flavor, and a network with a subnet. Flavors are seeded; create the
rest. (The network is also what section 2.5's wizard step must offer as a selectable row.)

```bash
curl -sL -o /tmp/cirros.img http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img cirros
openstack network create spec020-net
openstack subnet create --network spec020-net --subnet-range 10.44.0.0/24 spec020-sub
openstack image list -f value -c Name; openstack network list -f value -c Name; openstack flavor list -f value -c Name
```

**Pass =** `cirros` is listed, `spec020-net` is listed **exactly once**, and at least one flavor exists.

## 2. Test steps

Sections 2.1 and 2.6 are terminal-only. Sections 2.2–2.5 are **browser** checks against stock Horizon at
`http://localhost:8080/dashboard` — sign in as `admin` / `secret`, domain `Default`.

**Diagnostic cookie jar.** The troubleshooting commands in 2.4 and 2.5 replay Horizon's own requests from
the terminal, which needs a logged-in Django session. Build one once; skip this unless a check fails. Note
`region` is a **hidden index into `AVAILABLE_REGIONS`**, not a URL — sending the Keystone URL silently
re-renders the login page with a 200.

```bash
rm -f /tmp/hz.cookies
curl -s -c /tmp/hz.cookies -o /tmp/hz_login.html http://localhost:8080/dashboard/auth/login/
HZ_CSRF=$(grep -o 'name="csrfmiddlewaretoken" value="[^"]*"' /tmp/hz_login.html | head -1 | sed 's/.*value="//;s/"$//')
curl -s -b /tmp/hz.cookies -c /tmp/hz.cookies -o /dev/null -e http://localhost:8080/dashboard/auth/login/ -d "csrfmiddlewaretoken=$HZ_CSRF" -d username=admin -d password=secret -d domain=Default -d region=0 -d next=/dashboard/ http://localhost:8080/dashboard/auth/login/
curl -s -o /dev/null -w 'session works: %{http_code}\n' -b /tmp/hz.cookies http://localhost:8080/dashboard/project/
```

**Pass =** `session works: 200`. A `302` means the login was rejected.

### 2.1 API surface: every create-flow endpoint resolves and limits agree (SC-005)

The repo ships a probe that walks exactly the endpoints Horizon's create flows load. It is the fastest
signal that the backend half of F2/F3/F7/F8 is in place.

```bash
./test/horizon_create_ui_test.sh
```

**Expect:** `Passed: 54   Failed: 0`. Every quota variant returns 200 (`os-quota-sets/{id}/detail`,
`quotas/{id}/default`, `quotas/{id}/details.json`, `quota-sets/{id}?usage=true`, `quota-sets/{id}/defaults`),
Cinder advertises `AvailabilityZones` via `/v3/extensions`, Neutron advertises `quota_details`, the
limits and quota-set views agree on instances/cores/ram/volumes, and every network carries `project_id`
with all its `subnets` ids resolvable. Before this feature the same script reported `Passed: 15  Failed: 11`.

The last two blocks cover ground the original probe missed:

- **client paths** — the spellings the OpenStack clients actually request rather than the ones the API
  reference documents: cinder `groups/detail` (FR-021) and `os-quota-sets`, plus the security-group rule
  shape (FR-022/FR-023). An earlier pass reported 26/26 while two workflows were still broken precisely
  because these went unprobed.
- **phantom quota targets** (SC-007a) — a 3×3 grid asserting nova, neutron and cinder all 404 on an
  unknown UUID, the nil UUID and a non-UUID, plus positive controls.

If the total differs from 54, check whether probe checks were added or removed before treating it as a
failure — `Failed: 0` is the criterion that matters.

### 2.2 F2 — Project Overview limits widget (SC-004)

In the browser, open **Project → Overview**.

**Expect:** the usage/limits panel renders compute, block-storage and network usage. The error
**"Unable to retrieve limits information" must NOT appear.** Reload once to confirm it is stable.

### 2.3 F3 — Create Network from the Project panel (SC-003)

In the browser, open **Project → Network → Networks**. Then create a network named `ui-net` with a subnet
`ui-sub` / `10.45.0.0/24` through the dialog, and confirm it from the terminal.

```bash
openstack network list -f value -c Name | grep -c '^ui-net$'
openstack subnet list --network ui-net -f value -c Name
```

**Expect:** a **Create Network** button is present on the *Project* panel (not only under Admin); the
wizard completes; the grep prints `1` (listed exactly once, no duplicate row); and the subnet list includes
**`ui-sub`**. `spec020-sub` appears too: `ListSubnets` does not honour the `network_id` filter yet (B6,
tracked in #546), so `--network ui-net` lists every subnet the caller can see.

### 2.4 F8 — Create Volume dialog (SC-002)

In the browser, open **Project → Volumes** and click **Create Volume**. Create a 1 GiB volume named
`ui-vol`, then confirm from the terminal.

```bash
openstack volume list -f value -c Name -c Status
```

**Expect:** the dialog **opens** (before this feature the button did nothing) and is populated with a
volume type (`ceph-rbd`, the only seeded one) and an availability zone; after submitting, `ui-vol` appears with status
`available`, and the Volumes panel lists it without a 500.

If the button is still inert, do **not** read the apache log first — this failure mode leaves no traceback
and no 500 there. Capture the modal fetch itself, which is the only place it is visible:

```bash
curl -s -o /dev/null -D - -H "X-Requested-With: XMLHttpRequest" -b /tmp/hz.cookies "http://localhost:8080/dashboard/project/volumes/create/"
```

An **empty `200` carrying `X-Horizon-Location`** means the modal aborted server-side: Horizon caught an
exception while building `CreateForm` and redirected instead. Decode the `messages` cookie in that response
for the reason — it names the failing call verbatim. That is what B1 looked like (`Unable to retrieve the
volume group list.…failed to get group (HTTP 500)`). A `302` to the login page means the session expired; a
real `500` means a Horizon traceback, which *is* in the apache log; a `200` with an HTML body means the
backend is fine and the problem is client-side.

### 2.5 F7 — Launch an instance from the dashboard (SC-001)

In the browser, open **Project → Compute → Instances** and use **Launch Instance**. In the wizard choose
the source image `cirros`, any flavor, and — on the **Networks** step — `spec020-net`. Name it `ui-vm` and
launch. Then confirm from the terminal.

```bash
openstack server list -f value -c Name -c Status -c Networks
```

**Expect:** a **Launch Instance** button is present on the Instances panel; the **Networks step renders
`spec020-net` as a selectable row** (before this feature the table was empty, so Launch stayed disabled);
after launching, `ui-vm` reaches `ACTIVE` and shows an address on `spec020-net`.

Leave the Source step's **Create New Volume** toggle at Horizon's default (**Yes**) — that is the path that
used to fail. With it on, Horizon sends no `imageRef` and puts the image in `block_device_mapping_v2` as
`source_type: image`; Lightstack now adopts that as the boot image. **Known limitation:** Lightstack boots the image
directly and does **not** materialize a Cinder volume, so `volume_size` and *Delete Volume on Instance
Delete* are ignored and no new volume appears under Project → Volumes. `ACTIVE` is still the pass
condition. An instance in `ERROR` with an empty `image` field is the old failure — check the lightstack log for
`Invalid imageRef`.

Distinguish the two failure modes before concluding. If the **wizard never finishes loading**, one of its
data sources rejected — every promise in `launch-instance-model.service.js` is guarded by `noop` except
`novaAPI.getAvailabilityZones()`, so check `/api/nova/availzones/` first. If the wizard renders but
**Launch does nothing**, it is the write, not the read: a CSRF-rejected POST returns an empty `200` and
creates nothing (see FR-018a / §2.7). Sweep the reads from the terminal with a logged-in cookie jar:

```bash
for p in /api/nova/availzones/ /api/nova/limits/ /api/nova/flavors/ /api/glance/images/ /api/neutron/networks/ /api/network/securitygroups/ /api/cinder/volumetypes/default/ /api/cinder/tenantabsolutelimits/; do printf '%-40s %s\n' "$p" "$(curl -s -o /dev/null -w '%{http_code}' -b /tmp/hz.cookies -H 'X-Requested-With: XMLHttpRequest' "http://localhost:8080/dashboard$p")"; done
```

**Expect:** all `200`. Note a toast on this panel may be a **queued message from a previous page** — Django
replays them on the next render, which is how the Create Volume group-list error first appeared here and
was misattributed to the wizard.

### 2.6 Authorization: quota scoping, admin-only writes, phantom projects (SC-007)

A non-admin may read only their own project's quotas (another project is a `403`), and an admin may read any. Quota writes are admin-only, and a target
project that does not exist is a 404 rather than defaults for a phantom project.

```bash
TOKEN=$(openstack token issue -f value -c id)
PROJECT=$(openstack token issue -f value -c project_id)
echo "admin real project:"; curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" "http://localhost:8776/v3/quota-sets/$PROJECT"
echo "admin by name default (want 200):"; curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" http://localhost:8776/v3/quota-sets/default
echo "nova quota-set detail (want 200):"; curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/os-quota-sets/$PROJECT/detail"
echo "cinder usage view (want a number, not null - block storage has no /detail):"; curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:8776/v3/quota-sets/$PROJECT?usage=true" | jq -r '.quota_set.volumes.in_use'
echo "neutron quota carries project scope:"; curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9696/v2.0/quotas/$PROJECT" | jq -r '.quota.project_id'
echo "phantom targets on ALL THREE services (want 404 each):"
for ref in 11111111-1111-1111-1111-111111111111 00000000-0000-0000-0000-000000000000 no-such-project; do
  printf '  %-22s nova:%s neutron:%s cinder:%s\n' "$ref" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/os-quota-sets/$ref")" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TOKEN" "http://localhost:9696/v2.0/quotas/$ref")" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TOKEN" "http://localhost:8776/v3/quota-sets/$ref")"
done
```

**Expect:** `200`, `200`, `200`, a number from the Cinder usage view (`null` means the `usage` parameter
is being ignored again), the project id echoed back, and then `404` in **all nine** cells of the
phantom-target grid (SC-007a). That grid is the existence check, and it must hold on every service — a
syntactically valid but unknown UUID must not yield a default quota-set, and a non-UUID must not 500.
All three services gained this check when project resolution moved into `common.ResolveProjectRef`;
before, each answered an unknown UUID with default quotas.

Now confirm the pre-existing suites still pass — these cover the same endpoints and are **not** run by
`make test`. The quota suites assume an **exclusive** project: delete the instance left over from §2.5
first, or `TEST 5` fails because `ui-vm` already consumes part of the quota it sets.

```bash
make test-contract
openstack server delete ui-vm 2>/dev/null || true
./test/quota_test.sh
./test/quota_admin_test.sh
```

**Expect:** all `ok` / `ALL QUOTA TESTS PASSED` / `All tests passed - Quota admin check working correctly`.
The last one exercises non-admin quota update → 403.

### 2.7 Confirm Horizon stayed stock (FR-018)

No fix may depend on patching Horizon's own config or policy — no policy relaxation, feature toggle, or
override added to force a control to appear. The bar is the **upstream Horizon image and its defaults**,
which this stack never touches.

Per **FR-018a**, correcting an outright misconfiguration in Lightstack's *own* deployment artifacts is allowed and
is not a workaround. This feature has exactly one: `CSRF_COOKIE_HTTPONLY`, which was `True`. Horizon's
AngularJS layer reads the CSRF token from `document.cookie` and has no DOM fallback, so a write on that path
is sent with no `X-CSRFToken` and Django rejects it. Upstream Horizon does not set it.

The effect is **selective** — it does not break every Angular write. In the real pass with `True` still in
place, keypair import *and* generate returned `201` and the launch wizard succeeded, while Key Pairs →
**Delete Key Pair** silently failed with Django's `Forbidden (CSRF token missing.)`. Do not infer that a
control works because a neighbouring one does.

This stub stack mounts `deployments/horizon-config/local_settings` directly. Real mode reads a **generated
copy** at `deployments/horizon-chart/files/local_settings`, so any change to the former must be followed by
`make horizon-chart-sync`.

"Stayed stock" is a property of the **current configuration**, so the checks below read that configuration —
deliberately **no `git diff`**. A diff against a base commit stops working the moment the branch is rebased,
squashed or merged, and a bare `git diff` stops working the moment the work is committed; neither answers
whether Horizon is stock, and both fail in ways that look like a pass.

```bash
# 1. The image is upstream's, by digest-less tag but never a local build or patched layer.
grep -n 'quay.io/openstack.kolla/horizon' deployments/docker-compose-horizon.yml

# 2. Horizon enforces the image's own policies: the standard per-service file mapping at the
#    image's own path, no policy file shipped by us, no POLICY_CHECK_FUNCTION override.
grep -nE '^(POLICY_FILES_PATH|POLICY_CHECK_FUNCTION)' deployments/horizon-config/local_settings
find deployments/horizon-config deployments/horizon-chart/files -type f -name '*policy*' | grep . && echo "POLICY FILE SHIPPED - not stock" || echo "no policy files shipped"

# 3. No feature toggle flipped to force a control to render: the launch wizard sits at
#    Horizon's own defaults (NG on, legacy off).
grep -nE '^LAUNCH_INSTANCE_(LEGACY|NG)_ENABLED' deployments/horizon-config/local_settings

# 4. The one FR-018a correction, in BOTH copies.
grep -c 'CSRF_COOKIE_HTTPONLY = False' deployments/horizon-config/local_settings deployments/horizon-chart/files/local_settings

# 5. Does regenerating the chart copy change it? Snapshot, regenerate, compare. Any output
#    is drift in SOME setting, which is the class of bug F14 was -- grepping only for
#    CSRF_COOKIE_HTTPONLY would miss it.
cp deployments/horizon-chart/files/local_settings /tmp/lightstack-chart-ls.before
make horizon-chart-sync >/dev/null
diff -u /tmp/lightstack-chart-ls.before deployments/horizon-chart/files/local_settings && echo "chart copy is in sync"
```

**Expect:** the image is `quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble`; `POLICY_FILES_PATH` is the
image's own `/etc/openstack-dashboard` and `POLICY_CHECK_FUNCTION` does not appear at all; `no policy files
shipped`; `LAUNCH_INSTANCE_LEGACY_ENABLED = False` with `LAUNCH_INSTANCE_NG_ENABLED = True` (Horizon's own
defaults, not a toggle); **both** copies report `1`; and `chart copy is in sync` prints.

Any policy override, any shipped policy file, any other flipped toggle, or a patched image means the workflow
it enabled is a **documented finding**, not a fix.

## 3. Teardown steps

Removes the fixtures, the stack, and the downloaded image. Leaves the host as it was.

```bash
openstack server delete ui-vm 2>/dev/null || true
openstack volume delete ui-vol 2>/dev/null || true
openstack network delete ui-net 2>/dev/null || true
openstack network delete spec020-net 2>/dev/null || true
openstack image delete cirros 2>/dev/null || true
docker compose -f deployments/docker-compose-horizon.yml down -v
rm -f /tmp/cirros.img
sudo sed -i "/spec020-hosttest/d" /etc/hosts
getent hosts lightstack || echo "hosts entry removed"
docker ps --format '{{.Names}}' | grep -E '^lightstack' || echo "no lightstack containers remain"
```

**Pass =** the compose stack and its volumes are gone and no `lightstack*` container remains.

## Troubleshooting

**`address already in use` on the lightstack container** — a previous stub compose or a real-mode k3s pod holds
35357/8774/9696/8776/9292/8775. Find it with `ss -ltnp | grep -E ':(35357|8774)'` and stop it
(`docker compose -f deployments/docker-compose.yml down`, or `sudo systemctl stop k3s`).

**Horizon fails to bind port 80** — rootless Podman cannot. `export HORIZON_HTTP_PORT=8080` before
`up -d` (section 1.1).

**Login fails with `session works: 302`, or the browser shows "Unable to establish connection to keystone
endpoint"** — the Horizon container inherited §1.2's `/etc/hosts` alias. Podman seeds every container's
`/etc/hosts` from the host's, so a container created *after* that line exists resolves `lightstack` to `127.0.0.1`
— itself. Confirm, then rebuild Horizon without the alias:

```bash
podman exec lightstack-horizon getent hosts lightstack
sudo sed -i "/spec020-hosttest/d" /etc/hosts
docker compose -f deployments/docker-compose-horizon.yml up -d --force-recreate horizon
podman exec lightstack-horizon curl -s -o /dev/null -w 'keystone from horizon: %{http_code}\n' http://lightstack:35357/v3
grep -q "spec020-hosttest" /etc/hosts || echo "127.0.0.1 lightstack spec020-hosttest" | sudo tee -a /etc/hosts
```

The first command printing `127.0.0.1` is the confirmation; after the rebuild it resolves to the lightstack
container's address and keystone answers `200`. To patch the running container instead, without recreating
it and losing sessions — note `sed -i` fails on the bind-mounted file, so rewrite it in place:

```bash
podman exec -u root lightstack-horizon sh -c "grep -v 'spec020-hosttest' /etc/hosts > /tmp/h && cat /tmp/h > /etc/hosts && rm -f /tmp/h"
```

**`Failed to resolve 'lightstack'`, or `list index out of range`, on an `openstack` command other than
`token issue`** — the `/etc/hosts` line from §1.2 is missing. The catalog and every version document
advertise `lightstack:<port>` for Horizon's benefit and the CLI follows them. Re-run that block.
`OS_*_ENDPOINT_OVERRIDE` will not help (see §1.2).

**`invalid credentials` on `token issue`** — the DB was mutated by a previous DB-backed unit-test run
(`LIGHTSTACK_TEST_DB_URL` pointed at this stack's Postgres, and the keystone tests rewrite the admin password).
Recreate the volume: `docker compose -f deployments/docker-compose-horizon.yml down -v` then repeat 1.1.
Always point `LIGHTSTACK_TEST_DB_URL` at a throwaway database.

**A panel renders but a create button is missing** — capture Horizon's traceback before concluding:
`docker compose -f deployments/docker-compose-horizon.yml exec horizon sh -c 'tail -80 /var/log/apache2/*error*'`.

**Known unrelated failures** (pre-existing, tracked separately, not caused by this feature):
`POST /v3/projects` returns 500 when `domain_id` is a non-UUID such as `default` (F13), and
`test/horizon_compat_test.sh`'s "Cinder - List volumes" check fails
because `GET /v3/{project_id}/volumes` is not registered (405). Both reproduce on the base commit.
