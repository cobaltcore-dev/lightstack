# HOST_TESTING — SPEC-023 real mode (tenant scoping & authorization model)

Validates [cloud-storage#549](https://github.com/cobaltcore-dev/cloud-storage/issues/549) on a real
k3s deployment with real libvirt, real networking and a real Horizon — the layer the unit and
contract suites cannot reach. Single Ubuntu host, lightstack in real mode via the chart, Horizon alongside
it in the same namespace.

**Run all commands from the repo root.** Paths below are repo-relative.

The automated suites already prove the API behaviour (265 unit, 88 nova-contract, 23 multi-tenancy
checks). What this runbook adds is the part that has never been exercised: **an operator driving
Horizon's Admin panel against the surfaces this feature gates**, and every user story against a
deployment where instances actually boot.

## Validates

| Acceptance criterion | Section |
|---|---|
| US1 — an operator administers any tenant's server | R1, R2, R3 |
| US1 — listings widen only on explicit opt-in | R4 |
| US1 — a name never spans projects | R5 |
| US1 / FR-007 — actions attributed to the owning project | R6 |
| US2 — domain scope refused, nested qualifier unaffected | R7 |
| US3 — usage reporting confidentiality | R8 |
| US6 — operator-only surfaces reject tenants | R9 |
| US6 — the privilege escalation specifically | R10 |
| US5 — identity collection filtering | R11 |
| Isolation did not regress | R12 |
| **Horizon Admin panel** (Constitution merge gate, T086) | R13 ⭐ |
| SC-004 — services this feature does not edit | R14 |

---

## 0. Prerequisites

One block, one paste. It installs k3s if absent and builds the image under test. It only *verifies*
the heavier preconditions — a container engine (Docker, or Podman with `podman-docker`), `helm`, and
libvirt/KVM. Install those first if missing.

```bash
sudo apt-get update
sudo apt-get install -y curl jq python3-openstackclient libvirt-clients qemu-kvm
sudo systemctl enable --now libvirtd

command -v docker >/dev/null || command -v podman >/dev/null || echo "install docker or podman first"
command -v helm >/dev/null || echo "install helm first"

command -v k3s >/dev/null || curl -sfL https://get.k3s.io | sudo sh -
sudo systemctl is-active k3s || sudo systemctl start k3s
mkdir -p ~/.kube
sudo k3s kubectl config view --raw | sudo tee ~/.kube/k3s.yaml >/dev/null
sudo chown "$USER" ~/.kube/k3s.yaml
chmod 600 ~/.kube/k3s.yaml
export KUBECONFIG=~/.kube/k3s.yaml

echo "--- k3s node ---"
kubectl get nodes
echo "--- kvm ---"
sudo virsh list --all

echo "--- building the image under test ---"
docker build -f build/package/Dockerfile -t docker.io/library/lightstack:spec023 .
rm -f /tmp/lightstack-spec023.tar
docker save docker.io/library/lightstack:spec023 -o /tmp/lightstack-spec023.tar
sudo k3s ctr images import /tmp/lightstack-spec023.tar

echo "--- versions ---"
docker --version || podman --version
helm version --short
kubectl version --client=true -o yaml | head -3
jq --version
sudo k3s ctr images ls | grep lightstack:spec023

echo "--- openstack client (10.x required, see below) ---"
which -a openstack
openstack --version
openstack --version 2>&1 | awk '{print $2}' | cut -d. -f1 | \
  { read -r major; [ "${major:-0}" -ge 10 ] && echo "client OK" || \
    echo "CLIENT TOO OLD: step 1.3 will fail — install python-openstackclient >= 10 (see below)"; }
```

**Pass =** the k3s node is `Ready`, `virsh` succeeds (an empty list is fine), every version line
prints, `docker.io/library/lightstack:spec023` is listed in the k3s image store, and the client check says
`client OK`. Keep `KUBECONFIG` exported — every later `kubectl` and `helm` call needs it.

> **The `apt` line above is not enough for the client.** Debian and Ubuntu package
> python-openstackclient **6.6.0**, which cannot run `openstack project show`, `user show` or
> `role show` against Lightstack: it does an unconditional `project._info.pop('links')` and Lightstack's identity
> *show* responses omit `links`, so it exits `1` with a bare `'links'` on stderr. Step 1.3 reads
> `PROJ_A`/`PROJ_B`/`USER_A`/`USER_B` from exactly those commands, and would silently set them empty.
> A 10.x client tolerates the omission. Install one outside apt — `pipx install
> python-openstackclient` — and make sure it wins on `PATH` (`which -a openstack`); the apt package
> is still worth having for the rest of the CLI. The Lightstack-side gap is **#547 B16**.

---

## 1. Setup steps

### 1.1 Deploy lightstack in real mode, plus Horizon

`endpointHost` must be the **node IP**, not a Service DNS name: with `hostNetwork` lightstack listens on
the node, so one value is reachable both from the Horizon pod and from your host CLI. Left unset,
the catalog advertises `localhost` and Horizon dies on its first catalog call after login.

```bash
export KUBECONFIG=~/.kube/k3s.yaml
export NODE_IP=$(hostname -I | awk '{print $1}')
echo "node ip: $NODE_IP"

echo "--- node-local storage ---"
sudo mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp /var/lib/lightstack/console-tokens

echo "--- database ---"
docker inspect lightstack-postgres >/dev/null 2>&1 || make db-up
sleep 5

echo "--- nothing else may hold the service ports ---"
ss -lntp 2>/dev/null | grep -E ':(35357|8774|9292|9696|8776|8775) ' && echo "PORTS BUSY: stop whatever is listed above before deploying" || echo "ports free"
pgrep -x lightstack >/dev/null && echo "WARNING: a host-local lightstack process is running; kill it with: kill $(pgrep -x lightstack | head -1)" || true

echo "--- lightstack, real mode ---"
helm upgrade --install lightstack charts/lightstack --namespace lightstack --create-namespace --set image.repository=lightstack --set image.tag=spec023 --set image.pullPolicy=Never --set deploymentMode=single-node --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true --set nova.libvirtMode=real --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables --set glance.storageMode=local --set cinder.storageMode=local --set endpointHost=$NODE_IP --set secrets.jwtSecret=changeme --set secrets.adminPassword=secret --set database.url='postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable'
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s
kubectl -n lightstack get pods

echo "--- every service mode must read real ---"
kubectl -n lightstack get secret lightstack-lightstack-config -o jsonpath='{.data.lightstack\.yaml}' | base64 -d | grep -E "_mode:"

echo "--- horizon ---"
helm upgrade --install horizon charts/horizon --namespace lightstack
kubectl -n lightstack rollout status deploy/horizon-lightstack-horizon --timeout=180s
kubectl -n lightstack get svc
```

**Pass =** `ports free` (a `PORTS BUSY` line means the deploy will CrashLoopBackOff on
`bind: address already in use` — lightstack runs with `hostNetwork`, so it competes with anything already
on the node's ports); **every pod `Running`, none `CrashLoopBackOff`**; and the mode read-back shows `libvirt_mode: "real"`,
`networking_mode: "iptables"`, `security_group_mode: "iptables"` and `storage_mode: "local"` twice
(cinder and glance). A `stub` anywhere means the rest of this runbook proves nothing about real mode.

### 1.2 Authenticate and load an image

```bash
export OS_AUTH_URL=http://$NODE_IP:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3

openstack token issue -f value -c id >/dev/null && echo "authenticated"

echo "--- confirm the catalog points at the node, not at localhost ---"
openstack catalog list -f value -c Type -c Endpoints | grep -E 'http' | sed 's/^ *//'
echo -n "endpoints naming localhost (must be 0): "
openstack catalog list -f value -c Endpoints | grep -c 'localhost' || true
echo -n "endpoints naming the node (must be >= 5): "
openstack catalog list -f value -c Endpoints | grep -c "$NODE_IP" || true

test -s /tmp/cirros.img || curl -sL -o /tmp/cirros.img http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image show cirros >/dev/null 2>&1 || openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img cirros >/dev/null
openstack image show cirros -f value -c name -c status -c size -c visibility
```

**Pass =** `authenticated`; the localhost count is **`0`** and the node count is **`>= 5`** (one
per service: keystone, nova, neutron, glance, cinder — plus `cinderv3`); and `cirros` reporting
**`active`** with a non-zero size (roughly 117 MB — the raw expansion of the 21 MB qcow2).

`active` is the load-bearing word. A failed upload leaves the image at **`queued`** — metadata
created, no data — and `openstack image list` shows the name either way, so listing the name alone
cannot distinguish a working upload from a broken one.

The two counts matter more than the endpoint dump: a `grep` that matches nothing prints nothing and
reads as success. Both lines here produce a number you can check, so the step cannot pass vacuously.

If the catalog says `localhost`, or the image upload returns **`503 failed to upload image`**, you are
almost certainly talking to a **different lightstack** than the one you just deployed — see
*Troubleshooting → "The image upload returns 503"*.

### 1.3 Two projects, two genuinely non-administrative users

This is the fixture every check below depends on. The role is resolved **by name** — which only
works because of this feature's own US5 change; before it, `?name=member` returned all three roles
and taking the first gave you `admin`. The block asserts that explicitly: if either token carries
`admin`, every isolation result afterwards would be a tautology.

```bash
export ADMIN_TOKEN=$(openstack token issue -f value -c id)
export ADMIN_PROJECT=$(openstack token issue -f value -c project_id)

export MEMBER_ROLE=$(curl -s "http://$NODE_IP:35357/v3/roles?name=member" -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.roles[0].id')
export ADMIN_ROLE=$(curl -s "http://$NODE_IP:35357/v3/roles?name=admin" -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.roles[0].id')
test "$MEMBER_ROLE" != "$ADMIN_ROLE" || echo "SETUP BROKEN: member resolved to admin"
echo "member role: $MEMBER_ROLE"

openstack project show spec023-a >/dev/null 2>&1 || openstack project create spec023-a >/dev/null
openstack project show spec023-b >/dev/null 2>&1 || openstack project create spec023-b >/dev/null
export PROJ_A=$(openstack project show spec023-a -f value -c id)
export PROJ_B=$(openstack project show spec023-b -f value -c id)

openstack user show tenant-a >/dev/null 2>&1 || openstack user create tenant-a --password pw-a --domain Default >/dev/null
openstack user show tenant-b >/dev/null 2>&1 || openstack user create tenant-b --password pw-b --domain Default >/dev/null
export USER_A=$(openstack user show tenant-a -f value -c id)
export USER_B=$(openstack user show tenant-b -f value -c id)

curl -s -o /dev/null -X PUT "http://$NODE_IP:35357/v3/projects/$PROJ_A/users/$USER_A/roles/$MEMBER_ROLE" -H "X-Auth-Token: $ADMIN_TOKEN"
curl -s -o /dev/null -X PUT "http://$NODE_IP:35357/v3/projects/$PROJ_B/users/$USER_B/roles/$MEMBER_ROLE" -H "X-Auth-Token: $ADMIN_TOKEN"

export A_TOKEN=$(curl -s -X POST "http://$NODE_IP:35357/v3/auth/tokens" -H 'Content-Type: application/json' -d "{\"auth\":{\"identity\":{\"methods\":[\"password\"],\"password\":{\"user\":{\"name\":\"tenant-a\",\"password\":\"pw-a\",\"domain\":{\"name\":\"Default\"}}}},\"scope\":{\"project\":{\"id\":\"$PROJ_A\"}}}}" -D- -o /dev/null | awk '/[Xx]-[Ss]ubject-[Tt]oken/{print $2}' | tr -d '\r')
export B_TOKEN=$(curl -s -X POST "http://$NODE_IP:35357/v3/auth/tokens" -H 'Content-Type: application/json' -d "{\"auth\":{\"identity\":{\"methods\":[\"password\"],\"password\":{\"user\":{\"name\":\"tenant-b\",\"password\":\"pw-b\",\"domain\":{\"name\":\"Default\"}}}},\"scope\":{\"project\":{\"id\":\"$PROJ_B\"}}}}" -D- -o /dev/null | awk '/[Xx]-[Ss]ubject-[Tt]oken/{print $2}' | tr -d '\r')

echo "$A_TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | jq -c '.roles'
echo "$B_TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | jq -c '.roles'
```

**Pass =** the member role id differs from the admin role id, no `SETUP BROKEN` line, and both
token role lists print exactly `["member"]`. An `admin` in either list invalidates everything below.

### 1.4 A real guest in each project

Each network and subnet is created **with the tenant's own token**, not by the admin with
`--project`. That distinction is load-bearing: Lightstack's `CreateNetwork` reads the caller's project and
**silently ignores** any `project_id` in the body, so `openstack network create --project spec023-a`
run as admin returns `201` and puts the network in the *admin's* project. The instance then fails
port allocation with "network not found", boots anyway, and reports `addresses: {}` — which looks
exactly like the override bug R1 is testing for.

```bash
export NET_A=$(curl -s -X POST "http://$NODE_IP:9696/v2.0/networks" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d '{"network":{"name":"net-a"}}' | jq -r '.network.id')
export NET_B=$(curl -s -X POST "http://$NODE_IP:9696/v2.0/networks" -H "X-Auth-Token: $B_TOKEN" -H 'Content-Type: application/json' -d '{"network":{"name":"net-b"}}' | jq -r '.network.id')

curl -s -o /dev/null -X POST "http://$NODE_IP:9696/v2.0/subnets" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d "{\"subnet\":{\"name\":\"sub-a\",\"network_id\":\"$NET_A\",\"ip_version\":4,\"cidr\":\"10.40.0.0/24\"}}"
curl -s -o /dev/null -X POST "http://$NODE_IP:9696/v2.0/subnets" -H "X-Auth-Token: $B_TOKEN" -H 'Content-Type: application/json' -d "{\"subnet\":{\"name\":\"sub-b\",\"network_id\":\"$NET_B\",\"ip_version\":4,\"cidr\":\"10.41.0.0/24\"}}"

echo "--- each network must belong to its OWN project ---"
curl -s "http://$NODE_IP:9696/v2.0/networks/$NET_A" -H "X-Auth-Token: $A_TOKEN" | jq -r '"net-a owner: " + .network.project_id'
curl -s "http://$NODE_IP:9696/v2.0/networks/$NET_B" -H "X-Auth-Token: $B_TOKEN" | jq -r '"net-b owner: " + .network.project_id'
echo "expected A: $PROJ_A"
echo "expected B: $PROJ_B"

export SRV_A=$(curl -s -X POST "http://$NODE_IP:8774/v2.1/servers" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"web-1\",\"flavorRef\":\"m1.small\",\"imageRef\":\"$(openstack image show cirros -f value -c id)\",\"networks\":[{\"uuid\":\"$NET_A\"}]}}" | jq -r '.server.id')
export SRV_B=$(curl -s -X POST "http://$NODE_IP:8774/v2.1/servers" -H "X-Auth-Token: $B_TOKEN" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"web-1\",\"flavorRef\":\"m1.small\",\"imageRef\":\"$(openstack image show cirros -f value -c id)\",\"networks\":[{\"uuid\":\"$NET_B\"}]}}" | jq -r '.server.id')

echo "A: $SRV_A"
echo "B: $SRV_B"
sleep 20
echo "--- a port per instance must exist, or addresses will be empty ---"
curl -s "http://$NODE_IP:9696/v2.0/ports" -H "X-Auth-Token: $A_TOKEN" | jq -r '.ports | length'
curl -s "http://$NODE_IP:9696/v2.0/ports" -H "X-Auth-Token: $B_TOKEN" | jq -r '.ports | length'
sudo virsh list --all
```

**Pass =** each network reports its own project as owner (`net-a owner:` = `$PROJ_A`, `net-b owner:`
= `$PROJ_B` — if either says the admin's project, the `--project` trap above bit and R1 will fail for
the wrong reason); **each port count is `1`**; two distinct server ids; and `virsh list --all` shows a
**running** domain whose name
ends in the first 8 characters of each id (`instance-<first8>`). Both servers are deliberately named
`web-1` — the name collision R5 depends on.

**Shut-off domains you do not recognise are stale leftovers**, not a failure. A host that has run
real mode before accumulates them: lightstack deletes the instance row but the libvirt domain can survive.
Confirm which are which — lightstack should know about exactly the two you just created:

```bash
curl -s "http://$NODE_IP:8774/v2.1/servers?all_tenants=1" -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.servers[] | .id[0:8] + "  " + .name + "  owner=" + .tenant_id[0:8]'
```

Any `instance-*` domain whose prefix is absent from that list is an orphan. Optional cleanup before
testing, so `virsh` output stays readable:

```bash
for d in $(sudo virsh list --all --name --inactive); do sudo virsh undefine --nvram $d; done
sudo virsh list --all
```

---

## 2. Test steps

### R1 — An operator reads another tenant's server (US1, C1)

```bash
curl -s -o /dev/null -w 'admin -> B server: %{http_code}\n' "http://$NODE_IP:8774/v2.1/servers/$SRV_B" -H "X-Auth-Token: $ADMIN_TOKEN"
curl -s "http://$NODE_IP:8774/v2.1/servers/$SRV_B" -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.server.tenant_id'
curl -s "http://$NODE_IP:8774/v2.1/servers/$SRV_B" -H "X-Auth-Token: $ADMIN_TOKEN" | jq -c '.server.addresses'
```

**Expect:** `200`; the `tenant_id` equals `$PROJ_B` (**not** the admin's own project); and
`addresses` is **non-empty**. The addresses field is the one to watch — the ports sub-query is
scoped separately, so a widened server lookup with a narrow sub-lookup returns a server that appears
to have no network.

### R2 — A tenant is refused, indistinguishably from absent (US1, C2, FR-003)

```bash
curl -s -o /dev/null -w 'A -> B server:  %{http_code}\n' "http://$NODE_IP:8774/v2.1/servers/$SRV_B" -H "X-Auth-Token: $A_TOKEN"
curl -s -o /dev/null -w 'A -> absent:    %{http_code}\n' "http://$NODE_IP:8774/v2.1/servers/11111111-1111-1111-1111-111111111111" -H "X-Auth-Token: $A_TOKEN"
curl -s "http://$NODE_IP:8774/v2.1/servers/$SRV_B" -H "X-Auth-Token: $A_TOKEN" | jq -c .
curl -s "http://$NODE_IP:8774/v2.1/servers/11111111-1111-1111-1111-111111111111" -H "X-Auth-Token: $A_TOKEN" | jq -c .
```

**Expect:** both `404`, and the two bodies **identical**. A `403` for the first would confirm the
server exists; differing bodies would leak the same thing more subtly.

### R3 — An operator acts on another tenant's server, including the two repaired actions (US1, C3, C4)

```bash
curl -s -o /dev/null -w 'lock:        %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$SRV_B/action" -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"lock":null}'
curl -s -o /dev/null -w 'unlock:      %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$SRV_B/action" -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"unlock":null}'
curl -s -o /dev/null -w 'os-stop:     %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$SRV_B/action" -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"os-stop":null}'
sleep 5
curl -s -o /dev/null -w 'os-start:    %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$SRV_B/action" -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"os-start":null}'
curl -s -o /dev/null -w 'resetState:  %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$SRV_B/action" -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"os-resetState":{"state":"active"}}'
curl -s -o /dev/null -w 'evacuate:    %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$SRV_B/action" -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"evacuate":{"host":"nowhere"}}'
echo "--- and a tenant is still refused ---"
curl -s -o /dev/null -w 'A locks B:   %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$SRV_B/action" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d '{"lock":null}'
curl -s -o /dev/null -w 'A resets B:  %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$SRV_B/action" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d '{"os-resetState":{"state":"error"}}'
```

> **Known real-mode defect, unrelated to this feature:** `os-start` after `os-stop` returns **`500`**
> (`domain not found`). The **owner** fails identically on their own server, so it is a lifecycle bug
> in `StopVM`/`StartVM`, not an authorization one — see research R13. Treat a `500` on that one line
> as a known failure; every other line must still meet its expectation, and the two tenant lines are
> the ones that matter for this step.

**Expect:** every admin action `200`/`202` — **never `404`**. `evacuate` returns `200` here; a `4xx`
is acceptable (no such host as `nowhere`) but a `404` is the pre-SPEC-023 defect (gated to admins,
then scoped to the admin's own project).

Tenant A gets **`404` for both** — including `os-resetState`, which is administrator-only.
`ServerAction` resolves its target before dispatching, so a server tenant A cannot see is `404`
whatever action was asked for; the admin gate is never reached. A `403` here would confirm `SRV_B`
exists.

To see the `403` you have to address a server the caller **can** see:

```bash
curl -s -o /dev/null -w 'A resets A (own server): %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$SRV_A/action" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d '{"os-resetState":{"state":"error"}}'
```

**Expect:** `403`. The resolve succeeds — it is A's own server — and the administrative gate refuses.
The two codes together show the rule: `404` when the caller cannot see the resource, `403` when they
can see it but lack the role.

### R4 — Listings widen only on the explicit opt-in (US1, L1–L4)

```bash
echo "default (must NOT include B):"
curl -s "http://$NODE_IP:8774/v2.1/servers" -H "X-Auth-Token: $ADMIN_TOKEN" | jq --arg b "$SRV_B" '[.servers[]? | select(.id==$b)] | length'
for q in "all_tenants=1" "all_tenants=true" "all_tenants=yes" "all_tenants"; do
  printf '%-18s -> ' "?$q"
  curl -s "http://$NODE_IP:8774/v2.1/servers?$q" -H "X-Auth-Token: $ADMIN_TOKEN" | jq --arg b "$SRV_B" '[.servers[]? | select(.id==$b)] | length'
done
echo "tenant_id present on entries:"
curl -s "http://$NODE_IP:8774/v2.1/servers?all_tenants=1" -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '[.servers[].tenant_id] | unique | length'
echo "non-admin opt-in is IGNORED (both lines must match):"
curl -s "http://$NODE_IP:8774/v2.1/servers?all_tenants=1" -H "X-Auth-Token: $A_TOKEN" | jq -S -c .
curl -s "http://$NODE_IP:8774/v2.1/servers" -H "X-Auth-Token: $A_TOKEN" | jq -S -c .
```

**Expect:** default = `0`; all four opt-in spellings = `1` (including the bare `?all_tenants`, which
is what Horizon sends and which `strconv.ParseBool` cannot express); at least `2` distinct
`tenant_id` values; and the two non-admin lines **byte-identical**.

### R5 — A name never spans projects (US1, C6, FR-005c) ⭐

Both projects own a server called `web-1`. This is the check that stops
`openstack server delete web-1` from destroying the wrong tenant's guest.

```bash
echo -n 'admin -> name "web-1":  '; curl -s -o /dev/null -w '%{http_code}\n' "http://$NODE_IP:8774/v2.1/servers/web-1" -H "X-Auth-Token: $ADMIN_TOKEN"
echo -n '  resolved id:          '; curl -s "http://$NODE_IP:8774/v2.1/servers/web-1" -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.server.id // "none — refused"'
echo "  must NOT be A ($SRV_A)"
echo "  must NOT be B ($SRV_B)"
echo -n 'admin -> name only in B: '; curl -s -o /dev/null -w '%{http_code}\n' "http://$NODE_IP:8774/v2.1/servers/only-in-b" -H "X-Auth-Token: $ADMIN_TOKEN"
```

**Expect:** both lines `404`, and `resolved id` printing **`none — refused`**. The admin here is
scoped to the `default` project, which owns neither `web-1`, so the name matches nothing in their own
project — and must not fall through to a cross-project search that would find one of the tenants'.

The earlier form of this check piped a 404 body through `jq -r '.server.id + …'`, which prints
`  owner=` (jq treats `null + string` as the string) rather than saying `404`. That is unreadable and
cannot distinguish a refusal from an odd response — print the status code.

**The sharper variant is not runnable here**: an administrator *scoped to project A* addressing
`web-1` must resolve to **A's** server and never B's. The seeded admin holds no role on `spec023-a`,
so it cannot be produced from this fixture. It is covered by
`TestTargetInstance_AdminNameStaysInOwnProject` in `internal/nova/authz_test.go`, which seeds both
projects with a colliding `web-1` and asserts the answer is not B's id.

### R6 — Actions are attributed to the owning project (US1, FR-007, C7)

```bash
curl -s "http://$NODE_IP:8774/v2.1/servers/$SRV_B/os-instance-actions" -H "X-Auth-Token: $B_TOKEN" | jq -c '.instanceActions[]? | {action, project_id, user_id}'
echo "owner project should be: $PROJ_B"
echo "admin user should be:    $(openstack user show admin -f value -c id)"
```

**Expect:** tenant B — the owner — can see the `lock`/`stop`/`start` actions the **admin** performed
in R3, each recorded against `$PROJ_B` with the admin's `user_id`. Recording the caller's project
instead would leave the affected tenant's history silently empty.

### R7 — Domain scope refused; the nested qualifier unaffected (US2) ⭐

```bash
echo "--- the ticket's own reproduction ---"
curl -s -X POST "http://$NODE_IP:35357/v3/auth/tokens" -H 'Content-Type: application/json' -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","password":"secret","domain":{"name":"Default"}}}},"scope":{"domain":{"name":"Default"}}}}' -w '\nHTTP %{http_code}\n'
echo "--- uniform for every domain reference ---"
for d in '{"name":"Default"}' '{"name":"nope"}' '{"id":"default"}' '{"id":"not-a-uuid"}'; do
  printf '%-24s -> ' "$d"
  curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://$NODE_IP:35357/v3/auth/tokens" -H 'Content-Type: application/json' -d "{\"auth\":{\"identity\":{\"methods\":[\"password\"],\"password\":{\"user\":{\"name\":\"admin\",\"password\":\"secret\",\"domain\":{\"name\":\"Default\"}}}},\"scope\":{\"domain\":$d}}}"
done
echo "--- REGRESSION GUARD: nested project.domain must still work ---"
curl -s -o /dev/null -w 'nested qualifier: %{http_code}\n' -X POST "http://$NODE_IP:35357/v3/auth/tokens" -H 'Content-Type: application/json' -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","password":"secret","domain":{"name":"Default"}}}},"scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}'
openstack token issue -f value -c id >/dev/null && echo "openstack CLI still authenticates"
```

**Expect:** the reproduction returns `HTTP 400` with
`domain-scoped tokens are not supported…` — **not** the old `201` carrying no project, no domain and
no roles. All four domain references return the same `400`, so none reveals which domains exist.
The nested qualifier returns `201` and the CLI still authenticates — if either fails, every client
in existence is broken.

### R8 — Usage reporting confidentiality (US3, U1–U3)

```bash
curl -s -o /dev/null -w 'tenant -> aggregate:  %{http_code}\n' "http://$NODE_IP:8774/v2.1/os-simple-tenant-usage" -H "X-Auth-Token: $A_TOKEN"
curl -s -o /dev/null -w 'admin  -> aggregate:  %{http_code}\n' "http://$NODE_IP:8774/v2.1/os-simple-tenant-usage" -H "X-Auth-Token: $ADMIN_TOKEN"
echo "tenant A naming project B (must NOT describe B):"
curl -s "http://$NODE_IP:8774/v2.1/os-simple-tenant-usage/$PROJ_B" -H "X-Auth-Token: $A_TOKEN" | jq -r '.tenant_usage.tenant_id // "refused"'
echo "expected NOT: $PROJ_B"
curl -s -o /dev/null -w 'admin -> malformed:   %{http_code}\n' "http://$NODE_IP:8774/v2.1/os-simple-tenant-usage/not-a-uuid" -H "X-Auth-Token: $ADMIN_TOKEN"
```

**Expect:** tenant `403`, admin `200`; tenant A's answer for `$PROJ_B` is either their own project
or a refusal, never `$PROJ_B`; the malformed target is a `4xx`, not a `200` reporting zeroes
(zeroes are indistinguishable from a real project owning nothing).

### R9 — Operator-only surfaces reject tenants, both directions (US6, O1–O2, FR-019a)

```bash
for s in os-aggregates os-migrations os-simple-tenant-usage; do
  printf '%-24s tenant=' "$s"
  curl -s -o /dev/null -w '%{http_code}' "http://$NODE_IP:8774/v2.1/$s" -H "X-Auth-Token: $A_TOKEN"
  printf '  admin='
  curl -s -o /dev/null -w '%{http_code}\n' "http://$NODE_IP:8774/v2.1/$s" -H "X-Auth-Token: $ADMIN_TOKEN"
done
```

**Expect:** every line `tenant=403  admin=200`. Both halves matter — a gate that is never exercised
as a success is indistinguishable from one that broke the endpoint.

### R10 — The privilege escalation, specifically (US6, O3) ⭐

Before this feature every one of these succeeded for any authenticated tenant. Creating and deleting
host aggregates reshapes the deployment's scheduling topology.

```bash
export AGG=$(curl -s -X POST "http://$NODE_IP:8774/v2.1/os-aggregates" -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' -d '{"aggregate":{"name":"spec023-agg","availability_zone":"nova"}}' | jq -r '.aggregate.uuid // .aggregate.id')
echo "aggregate: $AGG"
printf 'tenant create:      '; curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/os-aggregates" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d '{"aggregate":{"name":"pwn","availability_zone":"nova"}}'
printf 'tenant rename:      '; curl -s -o /dev/null -w '%{http_code}\n' -X PUT "http://$NODE_IP:8774/v2.1/os-aggregates/$AGG" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d '{"aggregate":{"name":"renamed"}}'
printf 'tenant add_host:    '; curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/os-aggregates/$AGG/action" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d '{"add_host":{"host":"h1"}}'
printf 'tenant set_meta:    '; curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/os-aggregates/$AGG/action" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d '{"set_metadata":{"metadata":{"k":"v"}}}'
printf 'tenant unknown act: '; curl -s -o /dev/null -w '%{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/os-aggregates/$AGG/action" -H "X-Auth-Token: $A_TOKEN" -H 'Content-Type: application/json' -d '{"no_such_action":{}}'
printf 'tenant delete:      '; curl -s -o /dev/null -w '%{http_code}\n' -X DELETE "http://$NODE_IP:8774/v2.1/os-aggregates/$AGG" -H "X-Auth-Token: $A_TOKEN"
printf 'admin delete:       '; curl -s -o /dev/null -w '%{http_code}\n' -X DELETE "http://$NODE_IP:8774/v2.1/os-aggregates/$AGG" -H "X-Auth-Token: $ADMIN_TOKEN"
```

**Expect:** every `tenant …` line `403`, including the **unknown action** — a gate placed only on
the three recognised actions would leave the dispatcher's default branch open. The final admin
delete is `200`/`202`/`204`.

### R11 — Identity collection filtering (US5, F1–F2)

```bash
printf 'roles unfiltered:   '; curl -s "http://$NODE_IP:35357/v3/roles" -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.roles | length'
printf 'roles ?name=member: '; curl -s "http://$NODE_IP:35357/v3/roles?name=member" -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.roles | length'
printf 'roles ?name=nope:   '; curl -s "http://$NODE_IP:35357/v3/roles?name=nope" -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.roles | length'
printf 'projects ?name=…-a: '; curl -s "http://$NODE_IP:35357/v3/projects?name=spec023-a" -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.projects | length'
printf 'users ?name=tenant-a: '; curl -s "http://$NODE_IP:35357/v3/users?name=tenant-a" -H "X-Auth-Token: $ADMIN_TOKEN" | jq '.users | length'
printf 'projects unfiltered: '; curl -s -o /dev/null -w '%{http_code}\n' "http://$NODE_IP:35357/v3/projects" -H "X-Auth-Token: $ADMIN_TOKEN"
```

**Expect:** unfiltered roles `3`; `?name=member` `1`; `?name=nope` **`0`** (an empty collection, not
the full one — returning all three is the defect); the project and user filters `1` each; and
unfiltered projects `200` (this used to `500` when any project had a null `domain_id`).

### R12 — Isolation did not regress (SC-004)

```bash
printf 'A -> B server:      '; curl -s -o /dev/null -w '%{http_code}\n' "http://$NODE_IP:8774/v2.1/servers/$SRV_B" -H "X-Auth-Token: $A_TOKEN"
printf 'A deletes B server: '; curl -s -o /dev/null -w '%{http_code}\n' -X DELETE "http://$NODE_IP:8774/v2.1/servers/$SRV_B" -H "X-Auth-Token: $A_TOKEN"
printf 'A -> B network:     '; curl -s -o /dev/null -w '%{http_code}\n' "http://$NODE_IP:9696/v2.0/networks/$NET_B" -H "X-Auth-Token: $A_TOKEN"
printf 'A -> B migrations:  '; curl -s -o /dev/null -w '%{http_code}\n' "http://$NODE_IP:8774/v2.1/servers/$SRV_B/migrations" -H "X-Auth-Token: $A_TOKEN"
echo "B's server must still exist:"
curl -s -o /dev/null -w '%{http_code}\n' "http://$NODE_IP:8774/v2.1/servers/$SRV_B" -H "X-Auth-Token: $B_TOKEN"
```

**Expect:** the first four all `404` (or `403` for the network), and B's server still `200` for its
owner. The per-server migrations line is new behaviour — before this feature it had no ownership
check at all and any tenant could read and **delete** another's migration records.

### R13 — Horizon Admin panel ⭐ (Constitution merge gate, T086)

The only part of this feature no automated test reaches. **Shell T1** — leave running:

```bash
export KUBECONFIG=~/.kube/k3s.yaml
kubectl -n lightstack port-forward svc/horizon-lightstack-horizon 8080:80
```

Then open `http://localhost:8080` in a browser and sign in as `admin` / `secret`, domain `Default`.

Work through, in order:

1. **Admin › Compute › Host Aggregates** — loads, and you can create and delete an aggregate.
2. **Admin › Compute › Instances** — lists instances from **both** `spec023-a` and `spec023-b`
   (this panel sends `all_tenants`), each showing its owning project.
3. **Project › Compute › Instances** — as the same admin, shows **only** the admin's own project.
4. Sign out, sign in as `tenant-a` / `pw-a`. The **Admin** menu must be **absent or refused**;
   Project › Instances shows A's `web-1` and not B's.

**Expect:** every admin panel above renders without a traceback or an "Unable to establish
connection" error, the instance list spans projects only in the Admin panel, and `tenant-a` cannot
reach the Admin surfaces. A `403` rendered as a Horizon error page is a **pass** — the gate working.
A `500` or a blank page is a fail.

### R14 — Services this feature does not edit (SC-004)

**Shell T2** (client):

Run this **before R3** if you are short of time. R3 performs `os-stop` then `os-start`, which trips
the `os-start` defect (#547 B12) and leaves the database reporting `ACTIVE` for a domain libvirt no
longer has; the contract suites are far less informative against a stack in that state.

Each package runs separately and carries its own timeout. Piping the whole run through `tail`
discards every result earned before a hang — which is exactly what happens if
`./test/contract/nova/` is included (see below).

**Shell T2** (client):

```bash
export LIGHTSTACK_TEST_DB_URL="postgres://lightstack:secret@127.0.0.1:5432/lightstack_test_549?sslmode=disable"
docker exec lightstack-postgres psql -U lightstack -d postgres -c "CREATE DATABASE lightstack_test_549;" 2>/dev/null || true
export OS_AUTH_URL=http://$NODE_IP:35357/v3
go test -count=1 -timeout 3m ./test/contract/keystone/
go test -count=1 -timeout 3m ./test/contract/neutron/
./test/multi_tenant_test.sh 2>&1 | tail -6
```

**Expect:** `test/contract/keystone` is `ok`, and the multi-tenancy suite reports **26 passed / 0
failed**.

The count is higher here than the 24 a stub run reports, and that is the point rather than a
discrepancy. The suite guards its volume-dependent checks — `if [ -n "$VOL1_ID" ] …` and
`if [ -n "$SERVER2_ID" ] && … && [ -n "$VOL1_ID" ]`. As non-root against a stub stack, volume
creation `503`s, so the volume check itself fails and the cross-project volume checks behind those
guards never execute at all. In real mode `/var/lib/lightstack` is writable and the pod runs privileged, so
volume creation succeeds and those previously-skipped checks run — and pass. **More checks executing
is the stronger result**; a run reporting 24 here means the volume path is not working.

`test/contract/neutron` may fail on **#546 F10** (port creation `500`, root-netns bridge vs a
`light-ns-<project>` TAP). That is a sibling ticket's defect in a package this feature does not
touch — confirm with `git status internal/neutron/` being empty rather than treating it as a
regression here.

> **`./test/contract/nova/` is deliberately not in the list.** It cannot pass against a real-mode
> stack, and it does not fail — it hangs until its own `-timeout`, taking the other packages' results
> with it if they share a pipe. The suite boots from the seeded image id
> `00000000-0000-0000-0000-000000000001`, which exists as a database row but has no bytes in
> `/var/lib/lightstack/images`, so every server it creates fails to build and every wait-for-`ACTIVE` spins
> out; other cases send `imageRef: ""` outright. Tracked as **#548 F15**. Its real-mode coverage of
> this feature comes from the multi-tenancy suite above and from R1–R13; run it against a **stub**
> stack if you want its 88 checks.

---

## 3. Teardown steps

```bash
export KUBECONFIG=~/.kube/k3s.yaml

openstack server delete $SRV_A $SRV_B 2>/dev/null || true
openstack subnet delete sub-a sub-b 2>/dev/null || true
openstack network delete net-a net-b 2>/dev/null || true
openstack user delete tenant-a tenant-b 2>/dev/null || true
openstack project delete spec023-a spec023-b 2>/dev/null || true
openstack image delete cirros 2>/dev/null || true

helm uninstall horizon --namespace lightstack 2>/dev/null || true
helm uninstall lightstack --namespace lightstack 2>/dev/null || true
kubectl delete namespace lightstack --ignore-not-found --timeout=120s

docker exec lightstack-postgres psql -U lightstack -d postgres -c "DROP DATABASE IF EXISTS lightstack_test_549;" 2>/dev/null || true
make db-down 2>/dev/null || true

for d in $(sudo virsh list --all --name --inactive); do sudo virsh undefine --nvram $d 2>/dev/null || true; done
sudo virsh list --all
sudo rm -rf /var/lib/lightstack
rm -f /tmp/cirros.img /tmp/lightstack-spec023.tar
sudo k3s ctr images rm docker.io/library/lightstack:spec023 2>/dev/null || true
```

Stop the port-forward in **T1** with Ctrl-C.

**Pass =** the `lightstack` namespace is gone, `virsh list --all` is empty — the undefine loop removes any
domain left behind after the servers are deleted, which is the residue that otherwise accumulates
across runs — and `/var/lib/lightstack` no longer exists. k3s, libvirt and the container engine are left installed — they were preconditions, not
things this runbook added.

---

## Troubleshooting

**An `openstack` command prints a bare `'links'` and exits `1`.** That is a
python-openstackclient 6.x traceback — `KeyError: 'links'` from an unconditional
`project._info.pop('links')` — against Lightstack's identity *show* responses, which omit the key.
It hits `project show`, `user show` and `role show`, so step 1.3 sets `PROJ_A`/`PROJ_B`/`USER_A`/
`USER_B` to empty strings and every later step reading them does something quiet and wrong. Check
which client you are on (`which -a openstack`; a 10.x one is fine), or read the ids straight from the
API — the same `?name=` filter this feature adds:

```bash
export PROJ_A=$(curl -s "http://$NODE_IP:35357/v3/projects?name=spec023-a" -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.projects[0].id')
export USER_A=$(curl -s "http://$NODE_IP:35357/v3/users?name=tenant-a" -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.users[0].id')
```

**Horizon logs in, then every page errors with "Unable to establish connection to
`http://localhost:9696/...`".** `endpointHost` was not set, so the catalog advertises `localhost`,
which inside the Horizon pod is the Horizon pod. Re-run the `helm upgrade` in 1.1 with
`--set endpointHost=$NODE_IP`.

**A service mode reads `stub` in 1.1.** `realMode.enabled=true` alone does **not** make services
real — each `--set nova.libvirtMode=real` / `neutron.networkingMode=iptables` /
`glance.storageMode=local` / `cinder.storageMode=local` is separately required. Anything left stub
makes the corresponding checks meaningless.

**Server create returns `413 Quota exceeded`.** The `quotas` table has a zero compute quota for the
project. Check with `openstack quota show`; a fresh database seeds 10/20/51200.

**The image upload returns `503 failed to upload image`, and the URL says `localhost:9292`.**
You are talking to a different lightstack than the one you deployed. The usual cause is a **host-local lightstack
process** left running from earlier work: lightstack uses `hostNetwork`, so a process already bound to
`35357`/`9292` both (a) serves your CLI instead of the pod and (b) makes the pod CrashLoopBackOff with
`bind: address already in use`. A host process runs as your user and cannot write the root-owned
`/var/lib/lightstack`, which is where the `503` comes from. Diagnose and fix:

```bash
export KUBECONFIG=~/.kube/k3s.yaml
kubectl -n lightstack get pods
kubectl -n lightstack logs deploy/lightstack-lightstack --tail=5
ss -lntp 2>/dev/null | grep -E ':(35357|9292) '
pgrep -ax lightstack
```

If `pgrep` shows a process, stop it and restart the deployment:

```bash
export KUBECONFIG=~/.kube/k3s.yaml
PID=$(pgrep -x lightstack | head -1)
test -n "$PID" && kill $PID
sleep 2
kubectl -n lightstack rollout restart deploy/lightstack-lightstack
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=150s
```

**Volume or image operations return `503` with no port conflict.** `/var/lib/lightstack` is not writable by
the pod. The 1.1 block creates the subdirectories; confirm the pod runs privileged with
`realMode.privileged=true`.

**R1 returns `200` and the right `tenant_id`, but `addresses` is `{}`.** No port was allocated.
Check whether the networks actually belong to the tenants, and whether any port exists at all:

```bash
curl -s "http://$NODE_IP:9696/v2.0/networks/$NET_B" -H "X-Auth-Token: $B_TOKEN" | jq -r '.network.project_id'
curl -s "http://$NODE_IP:9696/v2.0/ports" -H "X-Auth-Token: $B_TOKEN" | jq '.ports | length'
export KUBECONFIG=~/.kube/k3s.yaml
kubectl -n lightstack logs deploy/lightstack-lightstack -c lightstack --tail=200 | grep -i "allocate port"
```

`Failed to allocate port from Neutron: network … not found` with a network owned by the admin's
project means the fixture was built with `openstack network create --project`, which Lightstack silently
ignores. Rebuild 1.4 using the tenant tokens as written. Note the instance still boots when port
allocation fails — the error is logged, not returned — so this is invisible unless you look.

**The image list contains `test-backup-daily-*` entries.** Residue from a previous contract-test run
against the same database — the contract suite creates backup images and does not always clean up.
Harmless, but it clutters R11's counts. Remove with:

```bash
for img in $(openstack image list -f value -c Name | grep '^test-backup-daily-'); do openstack image delete "$img"; done
```

**Both tokens in 1.3 print `["admin"]`.** The role lookup resolved to the wrong role. Every
isolation result afterwards would be a tautology — stop and fix the fixture before continuing.

**`R5` returns B's server id.** Name matching has escaped its project. This is the defect the check
exists for: an operator's `openstack server delete web-1` could destroy a different tenant's guest.
