# HOST_TESTING — Stub pass (portable)

The laptop-grade pass: the all-in-one `docker-compose-horizon.yml` stack (O3K + real Horizon +
memcached + noVNC). Proves the Horizon↔O3K UI/API contract with no Kubernetes, KVM, or Ceph.

O3K here runs the image's baked `config/o3k.yaml` (`build/package/Dockerfile` copies it and the CMD
points at it), so the modes are **mixed, not all stub**: Nova `libvirt_mode: stub` and Neutron
`networking_mode: stub`, but Cinder and Glance both run their **`local` filesystem backend** — which is
why the compose declares `o3k-images` and `o3k-volumes` volumes. Volume and image writes are therefore
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

```
export HORIZON_HTTP_PORT=8080
export O3K_JWT_SECRET=dev-secret-change-me
docker compose -f deployments/docker-compose-horizon.yml up -d --build
```

Wait until O3K and Horizon are healthy, then confirm both endpoints answer:

```
docker compose -f deployments/docker-compose-horizon.yml ps
curl -fsS http://localhost:35357/v3
curl -fsS "http://localhost:${HORIZON_HTTP_PORT:-80}/dashboard/auth/login/"
```

Desired outcome: `ps` shows o3k and horizon `healthy`; the Keystone `/v3` call returns JSON; the
login page returns HTTP 200.

## Test steps

Open the dashboard in a browser — `http://localhost:8080/dashboard` if you set `HORIZON_HTTP_PORT=8080`, otherwise `http://localhost/dashboard`. Log in — User `admin`, Password `secret`, Domain `Default`.

- **T1 — Sign-in (US1).** Desired outcome: authentication succeeds and the Project → Overview page
  renders without errors. Record working / not-working.
- **T2 — Project switching (US1).** Use the project/domain switcher in the top bar. Desired outcome:
  the view re-scopes. (With only the default project seeded there may be one entry — confirm the
  switcher lists it and the domain is `Default`.)
- **T3 — Create network (US2).** Project → Network → Networks → Create Network (add a subnet).
  Desired outcome: the network + subnet appear in the list.
- **T4 — Create volume (US2).** Project → Volumes → Volumes → Create Volume. Desired outcome: the
  volume appears with a status.
- **T5 — Launch instance (US2, stub).** Project → Compute → Instances → Launch Instance; pick the
  seeded image, a flavor, and the network. Desired outcome: the instance is created and listed.
  NOTE: stub mode has no real VM — SSH (US2) and the console (US3) are **not-exercised** here.

Record each verdict for the **stub** column of the report.

### Supporting API-level evidence (optional)

`horizon_compat_test.sh` exercises the launch flow, which needs an image. A freshly-started stack has
none, and the script fails rather than reporting a green pass for a path it never exercised — so seed
one first. The `openstack` CLI cannot do this from the host: the catalog advertises `http://o3k:9292`
(the compose service name, unresolvable here) and O3K's discovery response overrides any
`endpoint_override`. Use the API directly, which is how these scripts reach O3K anyway.

```
[ -f /tmp/cirros.img ] || curl -fsSLo /tmp/cirros.img https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
TOKEN=$(curl -s -i -X POST http://localhost:35357/v3/auth/tokens -H "Content-Type: application/json" -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","domain":{"name":"Default"},"password":"secret"}}},"scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}' | grep -i "^X-Subject-Token:" | tr -d '\r' | awk '{print $2}')
IMAGE_ID=$(curl -s -X POST http://localhost:9292/v2/images -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" -d '{"name":"cirros","disk_format":"qcow2","container_format":"bare","visibility":"public"}' | jq -r .id)
curl -s -o /dev/null -w 'upload HTTP %{http_code}\n' -X PUT "http://localhost:9292/v2/images/$IMAGE_ID/file" -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/octet-stream" --data-binary @/tmp/cirros.img
curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9292/v2/images/$IMAGE_ID" | jq -r .status
```

Desired outcome: `upload HTTP 204`, then status `active`.

Now run the scripts.

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
catalog advertises the compose-internal hostname (`http://o3k:8774` etc.), which the host cannot
resolve, so `curl` returns 6 ("couldn't resolve host"). Map it with `echo "127.0.0.1 o3k" | sudo tee -a
/etc/hosts` if you want that last section to run. Note the `volume` endpoint prints as
`http://localhost:8776/v3` while the others print `http://o3k:...` — that is finding **F5**.

`horizon_compat_test.sh` ends `Total Failed: 2` and exits 1 — **both are expected**, and both are the
project-scoped Cinder route: `GET /v3/{project}/volumes` returns **405** and
`/v3/{project}/volumes/detail` returns **404**, while unscoped `/v3/volumes` returns 200. Everything
else passes, including the launch flow reaching **ACTIVE**. These are evidence for, not a substitute
for, the UI walk-through (FR-001).

## Teardown

```
docker compose -f deployments/docker-compose-horizon.yml down -v
```
