# HOST_TESTING — Stub pass (portable)

The laptop-grade pass: the all-in-one `docker-compose-horizon.yml` stack (Lightstack + real Horizon +
memcached + noVNC). Proves the Horizon↔Lightstack UI/API contract with no Kubernetes, KVM, or Ceph.

Lightstack here runs the image's baked `config/lightstack.yaml` (`build/package/Dockerfile` copies it and the CMD
points at it), so the modes are **mixed, not all stub**: Nova `libvirt_mode: stub` and Neutron
`networking_mode: stub`, but Cinder and Glance both run their **`local` filesystem backend** — which is
why the compose declares `lightstack-images` and `lightstack-volumes` volumes. Volume and image writes are therefore
real; they just land inside container volumes. Backend-dependent workflows (launch → real VM, volume
attach, console) are **not exercised** here — their authoritative verdict comes from
`HOST_TESTING_REAL_MODE.md`.

Run all commands from the repo root unless noted. Shell blocks are zsh-copy-paste-safe.

## Prerequisites (Ubuntu; idempotent)

Assumes already installed (by whatever means — distro package, pipx, venv, …):
- a Docker-compatible container engine — Docker, or **Podman + `podman-docker`** (so `docker compose` resolves);
- the **OpenStack client** (`openstack`) — used by the API-level evidence scripts.

This runbook installs neither of those; only small helpers:

```
sudo apt-get update
sudo apt-get install -y jq curl
```

## Setup

**Rootless Podman**: the Horizon container publishes host port 80, which rootless Podman blocks.
The compose file parametrizes the host port as `${HORIZON_HTTP_PORT:-80}` — export a high port before
bringing it up. Docker/root users can skip the export (it defaults to 80).

`HORIZON_SECRET_KEY` is Django's `SECRET_KEY`. It has a lab default, so the export below is
optional — it is shown because apache runs three WSGI workers and all of them must sign CSRF
tokens with the same value, so this is the one secret worth setting deliberately.

```
export HORIZON_HTTP_PORT=8080
export LIGHTSTACK_JWT_SECRET=dev-secret-change-me
export HORIZON_SECRET_KEY=lab-only-change-me
docker compose -f deployments/docker-compose-horizon.yml up -d --build
```

Wait until Lightstack and Horizon are healthy, then confirm both endpoints answer:

```
docker compose -f deployments/docker-compose-horizon.yml ps
curl -fsS http://localhost:35357/v3
curl -fsS "http://localhost:${HORIZON_HTTP_PORT:-80}/dashboard/auth/login/"
```

Desired outcome: `ps` shows lightstack and horizon `healthy`; the Keystone `/v3` call returns JSON; the
login page returns HTTP 200.

## Seed fixtures (required — do this before the test steps)

A freshly-started stack has **no image, no network, and one project**, and the dashboard cannot create any
of them: network creation has no button (finding **F3**) and the launch wizard needs an image that only the
API can upload. So T2 and T5 are untestable until these exist. Seed them here, over the API.

The `openstack` CLI cannot do this from the host — the catalog advertises `http://lightstack:9292` (the
compose service name, unresolvable here) and Lightstack's discovery response overrides any
`endpoint_override`. Use the API directly, which is how the evidence scripts reach Lightstack anyway.

**Token** (re-run this block if a later call returns 401 — the token expires):

```
TOKEN=$(curl -s -i -X POST http://localhost:35357/v3/auth/tokens -H "Content-Type: application/json" -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","domain":{"name":"Default"},"password":"secret"}}},"scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}' | grep -i "^X-Subject-Token:" | tr -d '\r' | awk '{print $2}')
echo "token length ${#TOKEN}"
```

Desired outcome: a non-zero length (373 at the time of writing).

**Image.** Download first and check the **size**, not mere existence — a half-finished download is still a
file, and uploading one fails with `upload HTTP 503` and a `qemu-img convert qcow2->raw: I/O error` in the
Lightstack log that reads exactly like a Glance defect and is not one.

```
CIRROS_URL=https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
CIRROS_BYTES=21430272
if [ "$(stat -c %s /tmp/cirros.img 2>/dev/null || echo 0)" -eq "$CIRROS_BYTES" ]; then echo "cirros already complete"; else curl -fsSL -o /tmp/cirros.img "$CIRROS_URL"; fi
stat -c %s /tmp/cirros.img
```

Desired outcome: `21430272`. Anything else means the download is incomplete — re-run the block.

```
IMAGE_ID=$(curl -s -X POST http://localhost:9292/v2/images -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" -d '{"name":"cirros","disk_format":"qcow2","container_format":"bare","visibility":"public"}' | jq -r .id)
curl -s -o /dev/null -w 'upload HTTP %{http_code}\n' -X PUT "http://localhost:9292/v2/images/$IMAGE_ID/file" -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/octet-stream" --data-binary @/tmp/cirros.img
curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9292/v2/images/$IMAGE_ID" | jq -r '"status \(.status) size \(.size)"'
```

Desired outcome: `upload HTTP 204`, then `status active`. Stub Glance normalizes qcow2 to raw, so the
reported size is the **virtual** size `117440512`, not the 21 MB file — that is expected.

**Network + subnet** (T5 needs one; the UI cannot create it — F3):

```
NET_ID=$(curl -s -X POST http://localhost:9696/v2.0/networks -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" -d '{"network":{"name":"test-net","admin_state_up":true}}' | jq -r '.network.id')
curl -s -X POST http://localhost:9696/v2.0/subnets -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" -d "{\"subnet\":{\"network_id\":\"$NET_ID\",\"name\":\"test-subnet\",\"ip_version\":4,\"cidr\":\"10.0.0.0/24\"}}" | jq -r '"subnet \(.subnet.id) \(.subnet.cidr)"'
```

Desired outcome: a subnet id and `10.0.0.0/24`.

**Second project** (T2 needs somewhere to switch *to*; the seed data ships only `default`). Omit
`domain_id` — passing the literal string `"default"` triggers a known 500 on an unresolved domain:

```
PROJECT_ID=$(curl -s -X POST http://localhost:35357/v3/projects -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" -d '{"project":{"name":"verify-two","description":"second project so project switching can be exercised","enabled":true}}' | jq -r '.project.id')
curl -s -o /dev/null -w 'role assignment HTTP %{http_code}\n' -X PUT "http://localhost:35357/v3/projects/$PROJECT_ID/users/00000000-0000-0000-0000-000000000001/roles/00000000-0000-0000-0000-000000000003" -H "X-Auth-Token: $TOKEN"
curl -s -o /dev/null -w 'scope to verify-two HTTP %{http_code}\n' -X POST http://localhost:35357/v3/auth/tokens -H "Content-Type: application/json" -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","domain":{"name":"Default"},"password":"secret"}}},"scope":{"project":{"name":"verify-two","domain":{"name":"Default"}}}}}'
```

Desired outcome: `role assignment HTTP 204`, then `scope to verify-two HTTP 201`. Without the role
assignment the project exists but the switcher will not offer it.

## Test steps

Open the dashboard in a browser — `http://localhost:8080/dashboard` if you set `HORIZON_HTTP_PORT=8080`, otherwise `http://localhost/dashboard`. Log in — User `admin`, Password `secret`, Domain `Default`.

- **T1 — Sign-in (US1).** Desired outcome: authentication succeeds and the Project → Overview page
  renders without errors. Record working / not-working.
- **T2 — Project switching (US1).** Use the project/domain switcher in the top bar. Desired outcome:
  the switcher lists **both** `default` and `verify-two` (seeded above), and selecting the other one
  **re-scopes the view** — the resource panels should go empty, since the fixtures belong to `default`.
  Re-scoping is the acceptance check, not merely that the switcher renders.
- **T3 — Create network (US2).** Project → Network → Networks → Create Network (add a subnet).
  Desired outcome: the network + subnet appear in the list.
- **T4 — Create volume (US2).** Project → Volumes → Volumes → Create Volume. Desired outcome: the
  volume appears with a status.
- **T5 — Launch instance (US2, stub).** Project → Compute → Instances → Launch Instance; pick `cirros`,
  a flavor, and `test-net` (all seeded above). Desired outcome: the instance is created and listed.
  NOTE: stub mode has no real VM — SSH (US2) and the console (US3) are **not-exercised** here.
  Known failure (**F7**): the Networks step renders no selectable rows. If it does, check the browser
  console for `[ngRepeat:dupes] … Duplicate key: <network-id>` — that is F4 breaking the table, and it is
  the whole reason the step cannot validate.

Record each verdict for the **stub** column of the report.

### Supporting API-level evidence (optional)

The fixtures these scripts need were already seeded above — `horizon_compat_test.sh` exercises the launch
flow, and without an active image it fails rather than reporting a green pass for a path it never
exercised. If you skipped the seeding section, go back and run it.

```
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
bash test/horizon_dashboard_access_test.sh
bash test/horizon_compat_test.sh
```

Desired outcome: `horizon_dashboard_access_test.sh` prints the catalog and each service's public
endpoint, then **stops at "Testing endpoint connectivity" and exits 6** — expected from the host: the
catalog advertises the compose-internal hostname (`http://lightstack:8774` etc.), which the host cannot
resolve, so `curl` returns 6 ("couldn't resolve host"). Map it with `echo "127.0.0.1 lightstack" | sudo tee -a
/etc/hosts` if you want that last section to run. Note the `volume` endpoint prints as
`http://localhost:8776/v3` while the others print `http://lightstack:...` — that is finding **F5**.

`horizon_compat_test.sh` ends `Total Failed: 2` and exits 1 — **both are expected**, and both are the
project-scoped Cinder route: `GET /v3/{project}/volumes` returns **405** and
`/v3/{project}/volumes/detail` returns **404**, while unscoped `/v3/volumes` returns 200. Everything
else passes, including the launch flow reaching **ACTIVE**. These are evidence for, not a substitute
for, the UI walk-through (FR-001).

## Teardown

```
docker compose -f deployments/docker-compose-horizon.yml down -v
```
