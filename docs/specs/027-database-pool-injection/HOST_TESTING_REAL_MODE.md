# Host testing guide — database-pool injection (real mode smoke)

This is the real-mode gate of SPEC-027 (cloud-storage#554): FR-010, SC-003, and the constitution's "both modes" merge check. The refactor changes no behavior. The stub-mode gates are in [`HOST_TESTING.md`](HOST_TESTING.md) (unit, contract, CLI, Horizon probe, fail-fast, readiness, shutdown), and they already ran identical to the baseline. This pass covers the code that only runs in real mode, and every one of those paths now gets its pool injected instead of reading the old process-wide handle.

Topology: one Ubuntu host with KVM. Lightstack runs on root/privileged **k3s** through `charts/lightstack` in real mode: libvirt compute, iptables networking, and node-local Glance **and** Cinder. That means no Ceph and no minikube. PostgreSQL is the host container from `make db-up`. The image is **built from this branch**.

**Run all commands from the repo root, in one shell.** Each section is one zsh-copy-paste-safe block. Blocks that talk to k3s or Lightstack start with `source /tmp/spec027.env`, which §0 writes, so a fresh shell works too.

**Validates**

| Behavior, expected as before the change | Real-mode code path the refactor touched | Section |
|---|---|---|
| A server boots on a network and reaches `ACTIVE` as a real libvirt domain | Nova create path (`CheckQuota`, `neutron.EnsureDefaultSecurityGroup`, port allocation), libvirt boot | 2.1 |
| A node-local volume attaches and detaches | Nova volume attachment, Cinder | 2.2 |
| DHCP comes back after a control-plane restart | `NetworkReconciler` (`r.svc.pool`) | 2.3 |
| `cleanup --dry-run` enumerates with the DB up and degrades with the DB down | `teardown.Run`: `Connect` → pool, `enumerateDomainIDs(ctx, pool)` | 2.4 |
| Server delete removes the domain and the port | Nova delete path | 2.5 |
| Node registration, heartbeats and VNI allocation on one node | `compute.NodeRegistry`, `neutron.VXLANCoordinator` | 2.6 |
| The real purge clears host network state | `PurgeRealModeNetworkState` (`queryStrings(ctx, svc.pool, …)`) | 3 |

## 0. Prerequisites

This block installs `jq`, `curl`, `make` and `openssl`, and **k3s** if it is missing. k3s comes from the official `get.k3s.io` installer, which runs it as root under systemd; real mode needs privileged pods. The block waits until the node is `Ready`, copies the kubeconfig to `~/.kube/k3s.yaml`, and writes `/tmp/spec027.env`: that kubeconfig plus the `admin` credentials, for every later block.

The other runtimes stay preconditions it only verifies: `docker` (Docker, or Podman with `podman-docker`; `make db-up` calls `docker`), `helm`, the OpenStack client, Go, and libvirt/KVM. libvirt is checked by connecting to it. Ubuntu socket-activates `libvirtd`, so `systemctl is-active libvirtd` reads `inactive` whenever the daemon is idle. That is not a fault.

If the compose stack from the stub pass is still up, stop it first (`docker compose -f deployments/docker-compose.yml down`). It uses the same `lightstack-postgres` container name and port 5432.

```bash
sudo apt-get update
sudo apt-get install -y jq curl make openssl
command -v k3s >/dev/null || curl -sfL https://get.k3s.io | sh -
sudo systemctl enable --now k3s
sudo timeout 180 sh -c 'until k3s kubectl get nodes 2>/dev/null | grep -qw Ready; do sleep 2; done' || echo "k3s node is not Ready"
mkdir -p ~/.kube
sudo install -m 600 -o "$(id -u)" -g "$(id -g)" /etc/rancher/k3s/k3s.yaml ~/.kube/k3s.yaml
cat > /tmp/spec027.env <<'EOF'
export KUBECONFIG=$HOME/.kube/k3s.yaml
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
export OS_COMPUTE_API_VERSION=2.1
EOF
source /tmp/spec027.env
command -v docker >/dev/null || echo "install docker, or podman with podman-docker, first"
command -v helm >/dev/null || echo "install helm first"
command -v openstack >/dev/null || echo "install python-openstackclient first"
command -v go >/dev/null || echo "install Go first"
test -c /dev/kvm || echo "no /dev/kvm, real mode needs KVM"
sudo virsh -c qemu:///system uri >/dev/null || echo "libvirt is not reachable, install libvirt-daemon-system and qemu-system-x86 first"
kubectl get nodes
echo "versions:"; docker --version; k3s --version | head -1; helm version --short; openstack --version; go version; jq --version; sudo virsh -c qemu:///system version
```

**Pass =** `kubectl get nodes` lists this host as `Ready`, `virsh version` ends with `Running hypervisor: QEMU …`, every tool prints a version, and nothing prints `install … first`, `not Ready` or `not reachable`.

## 1. Setup steps

One block starts PostgreSQL, builds the image from this branch, imports it into k3s, deploys Lightstack in real mode, and creates the fixtures the tests use. If it fails, fix the cause and rerun the whole block.

- **Database.** On the first run, before a `lightstack` release exists, `make db-down` recreates `lightstack-postgres`, so the pod migrates a fresh database. Every worktree's `make db-up` shares that container, and a schema another branch left there can stop the migration. A rerun keeps the database. The real purge in §3 finds the host bridges through it, so wiping it would strand the bridges the fixtures created. The pod runs `hostNetwork` and reaches PostgreSQL at `127.0.0.1`. The directories are the node-local trees that Glance, Cinder, Nova and DHCP write into.
- **Image.** It is tagged fully qualified. Podman prefixes an unqualified build with `localhost/`, and the chart's `image.repository=lightstack` resolves to `docker.io/library/lightstack`. A mismatch shows up as `ImagePullBackOff` under `pullPolicy: Never`. The old archive goes first, because `podman save` refuses to write over one.
- **Deploy.** Every mode is spelled out. `realMode.enabled=true` alone leaves compute, networking and storage at their stub values. Glance and Cinder are both `local`, so this pass needs no object store. `endpointHost` stays unset: the catalog then advertises `localhost`, which the host's CLI reaches directly through the pod's host network. The values heredoc is unquoted so `$(openssl …)` expands.
- **Fixtures.** Each is created only if it is missing, so a rerun adds no duplicate names. `/tmp/spec027.env` pins `OS_COMPUTE_API_VERSION=2.1`. Lightstack's `GET /v2.1` version object omits `links`, and keystoneauth's microversion negotiation crashes on that (`list index out of range`).

```bash
source /tmp/spec027.env
helm status lightstack -n lightstack >/dev/null 2>&1 || make db-down
docker start lightstack-postgres >/dev/null 2>&1 || make db-up
docker ps --format '{{.Names}} {{.Status}}' | grep lightstack-postgres
sudo mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp
ENGINE=$(command -v podman || command -v docker)
$ENGINE build -f build/package/Dockerfile -t docker.io/library/lightstack:spec027 .
rm -f /tmp/lightstack-spec027.tar
$ENGINE save docker.io/library/lightstack:spec027 -o /tmp/lightstack-spec027.tar
sudo k3s ctr images import /tmp/lightstack-spec027.tar
sudo k3s ctr images ls | grep lightstack:spec027
cat > /tmp/spec027-values.yaml <<EOF
image: { repository: lightstack, tag: spec027, pullPolicy: Never }
deploymentMode: single-node
database:
  url: postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable
secrets:
  jwtSecret: $(openssl rand -hex 16)
  adminUser: admin
  adminPassword: secret
realMode: { enabled: true, privileged: true, hostNetwork: true }
nova: { libvirtMode: real }
neutron: { networkingMode: iptables, securityGroupMode: iptables }
cinder: { storageMode: local }
glance: { storageMode: local }
EOF
helm upgrade --install lightstack charts/lightstack -n lightstack --create-namespace -f /tmp/spec027-values.yaml --wait --timeout 10m
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=300s
kubectl -n lightstack logs deploy/lightstack-lightstack --tail=60 | grep -E 'Database connection established|Hypervisor initialized|Neutron initialized|Cinder initialized|Glance initialized|network reconciler started|Lightstack started'
openstack token issue -f value -c project_id
curl -fsSL -o /tmp/cirros.img http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image show spec027-cirros >/dev/null 2>&1 || openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img spec027-cirros
openstack network show spec027-net >/dev/null 2>&1 || openstack network create spec027-net
openstack subnet show spec027-sub >/dev/null 2>&1 || openstack subnet create --network spec027-net --subnet-range 10.47.0.0/24 spec027-sub
openstack image list -f value -c Name -c Status | grep spec027
```

**Pass =**
- `lightstack-postgres` is up, and `docker.io/library/lightstack:spec027` is listed in the k3s image store;
- the pod is Ready, and the log shows a database connection, the hypervisor initialized in `real` mode, Neutron in `iptables` mode, Cinder and Glance in `local` mode, the real-mode network reconciler started, and `Lightstack started successfully`;
- the token prints a project id, and `spec027-cirros` is `active`.

## 2. Test steps

### 2.1 Boot a server on the network

This runs Nova's create path: quota check, default security group, port allocation and libvirt boot. All of it now uses the injected pool.

```bash
source /tmp/spec027.env
openstack server create --flavor m1.tiny --image spec027-cirros --network spec027-net --wait spec027-vm
openstack server show spec027-vm -f value -c status
openstack server show spec027-vm -f value -c addresses
openstack port list --server spec027-vm -f value -c status
sudo virsh list --name | grep '^instance-'
sleep 30
openstack console log show spec027-vm | tail -15
```

**Expect:**
- the server is `ACTIVE` with an address in `10.47.0.0/24`, and its port is `ACTIVE`;
- exactly one `instance-…` domain is running (Lightstack names domains `instance-<hex>`, not after the server);
- the console log shows a cirros boot. Lines like `failed 5/20: up 10.83. iid had '<server id>'` are expected and predate this feature (the first few may read `request failed`): cirros's EC2 datasource wants an `i-…` instance-id, and the metadata service returns the bare server UUID. cirros gives up after 20 tries and boots on without metadata.

### 2.2 Attach and detach a node-local volume

```bash
source /tmp/spec027.env
openstack volume create --size 1 spec027-vol
sleep 3
openstack volume show spec027-vol -f value -c status
openstack server add volume spec027-vm spec027-vol
sleep 3
openstack volume show spec027-vol -f value -c status
openstack volume show spec027-vol -f json | jq -r '.attachments[0].server_id'
openstack server show spec027-vm -f value -c id
DOM=$(sudo virsh list --name | grep '^instance-' | head -1)
echo "== attached"; sudo virsh domblklist "$DOM"
openstack server remove volume spec027-vm spec027-vol
sleep 3
openstack volume show spec027-vol -f value -c status
echo "== detached"; sudo virsh domblklist "$DOM"
```

**Expect:**
- the volume goes `available` → `in-use` → `available`;
- the attachment's `server_id` equals the server's id;
- under `== attached`, `domblklist` lists a second disk backed by a file under `/var/lib/lightstack/volumes`; under `== detached`, that disk is gone.

### 2.3 The reconciler restores DHCP after a control-plane restart

dnsmasq dies with the pod. On startup, the network reconciler reads networks and subnets from the database through `r.svc.pool` and respawns it. Its successful runs are silent; only its failures log, as `reconcile: failed to query …`. dnsmasq runs with `--no-daemon`, which writes no pid file, so the step finds the process by its config path.

```bash
source /tmp/spec027.env
NET=$(openstack network show spec027-net -f value -c id)
echo "== before"; pgrep -a dnsmasq | grep "configs/$NET.conf"
kubectl -n lightstack rollout restart deploy/lightstack-lightstack
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=300s
sleep 15
echo "== after"; pgrep -a dnsmasq | grep "configs/$NET.conf"
kubectl -n lightstack logs deploy/lightstack-lightstack | grep -c 'reconcile: failed'
```

**Expect:** `== before` and `== after` each print exactly one `dnsmasq --conf-file=/var/lib/lightstack/dhcp/configs/<network id>.conf …` line, with different PIDs, and the failure count is `0`.

### 2.4 `cleanup --dry-run`, with the database up and down

The cleanup job opens a pool of its own, enumerates libvirt domains through it, and logs what it would purge. It deletes nothing in dry-run. The server from 2.1 is still running, so the DB-up run has a domain to enumerate. With the database unreachable, it must fall back to host-only cleanup rather than fail. Stopping PostgreSQL only makes the main process's readiness fail; liveness has no dependencies, so the pod keeps running. Both completion lines are worded as if the purge ran, dry-run or not.

```bash
source /tmp/spec027.env
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack cleanup --config /app/config/lightstack.yaml --dry-run > /tmp/spec027-cleanup-up.log 2>&1
echo "db up exit=$?"
grep -E 'would purge|class summary|complete' /tmp/spec027-cleanup-up.log
docker stop lightstack-postgres
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack cleanup --config /app/config/lightstack.yaml --dry-run > /tmp/spec027-cleanup-down.log 2>&1
echo "db down exit=$?"
grep -E 'database unreachable|best-effort' /tmp/spec027-cleanup-down.log
docker start lightstack-postgres
sleep 10
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=300s
sudo virsh list --name | grep -c '^instance-'
```

**Expect:**
- **DB up:** `exit=0`. The log has a `would purge (dry-run)` line for the server's domain, the network and storage `would purge … (dry-run)` lines, and a `class summary` per class, with `compute` at `"purged":1`. It ends with `teardown: complete — no lightstack-generated host state remaining (database left to its operator)`.
- **DB down:** `exit=0`, with `teardown: database unreachable — ignoring it` and the `best-effort` completion line.
- **Afterwards:** the pod is Ready again, and the domain count is `1`: the dry-run left the server alone.

### 2.5 Delete the server

```bash
source /tmp/spec027.env
openstack server delete --wait spec027-vm
openstack server list -f value -c Name | grep -c spec027-vm
openstack port list --network spec027-net --long -f value -c "Device Owner" | grep -c compute
sudo virsh list --all --name | grep -c '^instance-'
```

**Expect:** all three counts are `0`. The server, its compute port and its libvirt domain are gone.

Pre-existing, not from this feature: with Glance's `local` backend, the delete also removes the image's data. Nova evicts the unused raw base `/var/lib/lightstack/images/image-<id>.raw`, and that is the file Glance stores. `spec027-cirros` stays `active` but can no longer be downloaded or booted. To boot from it again, run `openstack image delete spec027-cirros` and rerun §1.

### 2.6 Node registry and VXLAN coordinator on one node

The chart enables VXLAN only for `multi-node`, and multi-node real mode needs Ceph RBD. To reach both components on one host, this step flips the rendered config's `vxlan_enabled` and restarts the pod. It also sets a VNI range, because the chart's rendered config carries none. Then it creates a network, which is now typed `vxlan`. Only the database side is graded here: the node registry writes and heartbeats `compute_nodes`, and the coordinator allocates a VNI for the new network. Both go through their injected pools.

```bash
source /tmp/spec027.env
kubectl -n lightstack get secret lightstack-lightstack-config -o jsonpath='{.data.lightstack\.yaml}' | base64 -d > /tmp/spec027-config.yaml
sed 's/vxlan_enabled: false/vxlan_enabled: true\n  vni_range_start: 1000\n  vni_range_end: 10000/' /tmp/spec027-config.yaml > /tmp/spec027-config-vxlan.yaml
grep -A2 'vxlan_enabled' /tmp/spec027-config-vxlan.yaml
kubectl -n lightstack create secret generic lightstack-lightstack-config --from-file=lightstack.yaml=/tmp/spec027-config-vxlan.yaml --dry-run=client -o yaml | kubectl -n lightstack apply -f -
kubectl -n lightstack rollout restart deploy/lightstack-lightstack
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=300s
kubectl -n lightstack logs deploy/lightstack-lightstack | grep -E 'Node registered|VXLAN overlay networking enabled'
openstack network create spec027-vx
openstack subnet create --network spec027-vx --subnet-range 10.48.0.0/24 spec027-vx-sub
VX=$(openstack network show spec027-vx -f value -c id)
sleep 5
docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT n.network_type, a.vni FROM networks n LEFT JOIN network_vni_allocations a ON a.network_id = n.id WHERE n.id = '$VX'"
docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT hostname, status, last_heartbeat FROM compute_nodes"
sleep 35
docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT hostname, status, last_heartbeat FROM compute_nodes"
kubectl -n lightstack logs deploy/lightstack-lightstack | grep -c -E 'failed to query networks for VXLAN coordination|failed to list active nodes|failed to allocate VNI|failed to query FDB entries'
ip -d link show "vxlan-$(echo $VX | cut -c1-8)" 2>&1 | head -3
```

**Expect:**
- the log shows `Node registered: <hostname> (tunnel IP: …)` and `VXLAN overlay networking enabled (VNI range: 1000-10000)`;
- the new network reads `vxlan|1000`;
- `compute_nodes` has this host's row with an active status, and its `last_heartbeat` advances between the two reads (the interval is 30 s);
- the failure count is `0`.

The last line is informational. It shows the `vxlan-…` link that `pkg/networking` creates, which is netlink code this feature did not change. If the link is missing, see Troubleshooting.

## 3. Teardown steps

One block tears everything down and runs the cleanup **for real**: the one path the dry-run in 2.4 skips. The purge reads the test networks from the database through `svc.pool` and removes their host state, so they are not deleted through the API first. The database and the node-local trees go anyway, along with everything in them. The Deployment is scaled to zero first, so the reconciler can't recreate what the purge removes. The binary runs on the host as root, using the release's rendered config. **It removes the node-local trees under `/var/lib/lightstack`, including anything earlier passes left there.** Run it only on a test host whose `/var/lib/lightstack` belongs to this pass.

The purge leaves two kinds of links behind, and neither is new: a server's `tap-…` device outlives the server, and nothing ever deletes a `vxlan-…` link (`DeleteVXLAN` in `pkg/networking/vxlan.go` has no callers). The block lists them under `== leftovers`, then deletes them. `helm uninstall --no-hooks` skips the chart's pre-delete quiesce and purge Jobs, which would only repeat what just ran.

```bash
source /tmp/spec027.env
kubectl -n lightstack scale deploy/lightstack-lightstack --replicas=0
kubectl -n lightstack wait --for=delete pod -l app.kubernetes.io/name=lightstack --timeout=120s
make build
kubectl -n lightstack get secret lightstack-lightstack-config -o jsonpath='{.data.lightstack\.yaml}' | base64 -d > /tmp/spec027-config.yaml
echo "== bridges before"; ip -br link | grep -E '^br-[0-9a-f]{8} '
sudo ./bin/lightstack cleanup --config /tmp/spec027-config.yaml > /tmp/spec027-purge.log 2>&1
echo "purge exit=$?"
grep -E 'class summary|complete|failed' /tmp/spec027-purge.log
echo "== bridges after"; ip -br link | grep -c -E '^br-[0-9a-f]{8} '
echo "== leftovers"; ip -br link | grep -E '^(tap|vxlan)-[0-9a-f]{8} ' | cut -d' ' -f1
for l in $(ip -br link | grep -E '^(tap|vxlan)-[0-9a-f]{8} ' | cut -d' ' -f1); do sudo ip link del "$l"; done
helm uninstall lightstack -n lightstack --no-hooks
kubectl delete namespace lightstack --ignore-not-found
make db-down
sudo k3s ctr images rm docker.io/library/lightstack:spec027
echo "== remaining"; helm list -A | grep -c lightstack; docker ps -a --format '{{.Names}}' | grep -c lightstack-postgres
rm -f /tmp/lightstack-spec027.tar /tmp/spec027-values.yaml /tmp/spec027-config.yaml /tmp/spec027-config-vxlan.yaml /tmp/spec027-*.log /tmp/cirros.img /tmp/spec027.env bin/lightstack
unset KUBECONFIG OS_AUTH_URL OS_USERNAME OS_PASSWORD OS_PROJECT_NAME OS_USER_DOMAIN_NAME OS_PROJECT_DOMAIN_NAME OS_IDENTITY_API_VERSION OS_COMPUTE_API_VERSION
```

**Expect:**
- `== bridges before` lists the `br-…` bridges of `spec027-net` and `spec027-vx`;
- `purge exit=0`, every `class summary` line reports `"failed":0` (the log is JSON), and the log ends with `teardown: complete — no lightstack-generated host state remaining (database left to its operator)`;
- `== bridges after` is `0`;
- `== leftovers` lists a `tap-…` for each server this pass booted and the `vxlan-…` of `spec027-vx`, and the next line deletes them;
- both `== remaining` counts are `0`: no `lightstack` release and no `lightstack-postgres` container.

§0 installs k3s when it is missing and leaves it in place. To remove it as well: `/usr/local/bin/k3s-uninstall.sh`, then `rm -f ~/.kube/k3s.yaml`.

## Troubleshooting

- **Did this fail before the change too?** Build the pre-change image and repeat just the failing step on it: `git archive --format=tar 44ed3d1 | podman build -f build/package/Dockerfile -t docker.io/library/lightstack:spec027-base -`. Import it as §1 does, then run `helm upgrade` with `image.tag=spec027-base`. A step that fails on both images is outside this feature.
- **`sudo: k3s: command not found`**: §0 did not install k3s. Rerun §0 and read the installer's output.
- **`ImagePullBackOff`**: the image was tagged without `docker.io/library/`. Rerun §1.
- **The pod crash-loops with `Failed to run migrations`**: the shared `lightstack-postgres` holds another branch's schema. Run `make db-down`, then rerun §1; it recreates the database.
- **`list index out of range`** from `openstack server …`: the shell has not sourced `/tmp/spec027.env`, so `OS_COMPUTE_API_VERSION=2.1` is unset.
- **No `vxlan-…` link in 2.6, but the VNI row and the heartbeat are fine**: the database side passed. The link is created by unchanged netlink code, so compare against the pre-change image as above.
- **`nil pointer dereference` in a log**: some real-mode component was built without its pool. Note the stack trace's package. Every owner must pass `pool` (contract invariant 5, `contracts/go-signatures.md`).
