# HOST_TESTING — SPEC-021 real mode (a real console / noVNC)

**Feature**: `021-real-novnc-console` · **Branch**: `feature/534` · **Mode**: real (k3s, libvirt/KVM)

This is where F1 actually closes: a live guest framebuffer in the Horizon Console
tab, reachable only through a token that can be revoked.

Reuses the SPEC-018 real harness — O3K on k3s via `charts/o3k` in real mode,
Horizon via `charts/horizon`, Ceph/RGW via Rook. **Note the chart moved**: SPEC-018's
runbooks still say `deployments/horizon-chart`; it is now `charts/horizon`. Those
runbooks belong to another feature and were deliberately left unedited.

> **Every shell block runs from the repository root, on this feature's branch.** No
> block changes directory or checks out anything, so paths like `charts/o3k` and
> `./test/…` resolve as written.
>
> Shell blocks are zsh-copy-paste-safe: no `!` inside double quotes, no `\` line
> continuations, no inline `#` comments.

---

## Prerequisites

One block, one paste. It **installs k3s** if absent and builds the image under test.
It only *verifies* the heavier preconditions — a container engine (Docker, or Podman
with `podman-docker`), `helm`, and libvirt/KVM. Install those first if missing.

```bash
sudo apt-get update
sudo apt-get install -y curl jq python3-openstackclient libvirt-clients qemu-kvm
sudo systemctl enable --now libvirtd

command -v k3s >/dev/null || curl -sfL https://get.k3s.io | sudo sh -
sudo systemctl is-active k3s || sudo systemctl start k3s
mkdir -p ~/.kube
sudo k3s kubectl config view --raw | sudo tee ~/.kube/k3s.yaml >/dev/null
sudo chown "$USER" ~/.kube/k3s.yaml
chmod 600 ~/.kube/k3s.yaml
export KUBECONFIG=~/.kube/k3s.yaml

command -v helm >/dev/null || echo "install helm first"
echo "--- k3s node ---"
kubectl get nodes
echo "--- kvm ---"
sudo virsh list --all

echo "--- building the image under test ---"
docker build -f build/package/Dockerfile -t docker.io/library/o3k:spec021 .
rm -f /tmp/o3k-spec021.tar
docker save docker.io/library/o3k:spec021 -o /tmp/o3k-spec021.tar
sudo k3s ctr images import /tmp/o3k-spec021.tar
sudo k3s ctr images ls | grep o3k:spec021
```

**Desired outcome**, reading down the output: the k3s node is `Ready`, `virsh`
succeeds (an empty list is fine), and `docker.io/library/o3k:spec021` is listed in
the k3s image store. Keep `KUBECONFIG` exported — every later `kubectl` and `helm`
call needs it.

### Why those particular lines

**k3s must run as real root.** The default `get.k3s.io` systemd install is; a
rootless container runtime cannot give the pod host netns or root-owned
`/var/lib/o3k`.

**Building the image is not optional.** Pulling a published `ghcr.io/o3k/o3k` would
verify a binary that predates SPEC-021, so the whole run would prove nothing about
this feature. `pullPolicy=Never` in Setup pins the deploy to this local build.

**`rm -f` before `docker save`** is not tidiness — on a re-run, a leftover tar from
an earlier build gets imported instead of the code you just compiled, and the run
silently verifies the wrong binary.

**Tag it fully qualified.** Podman prefixes an unqualified local build with
`localhost/`, so `-t o3k:spec021` lands in containerd as `localhost/o3k:spec021`
while the chart's `image.repository=o3k` resolves to `docker.io/library/o3k:spec021`
— a mismatch that surfaces as `ImagePullBackOff`, with no useful message.

---

## Setup

One block, one paste — storage dirs, database, both charts, CLI, image and guest.
Idempotent: every step is guarded, so a re-run after a partial failure is safe.

```bash
export KUBECONFIG=~/.kube/k3s.yaml
export NODE_IP=$(hostname -I | awk '{print $1}')

echo "--- node-local storage ---"
sudo mkdir -p /var/lib/o3k/images /var/lib/o3k/volumes /var/lib/o3k/instances /var/lib/o3k/cloud-init /var/lib/o3k/dhcp /var/lib/o3k/console-tokens

echo "--- database ---"
docker inspect o3k-postgres >/dev/null 2>&1 || make db-up

echo "--- o3k (real mode, all services) ---"
helm upgrade --install o3k charts/o3k --namespace o3k --create-namespace --set image.repository=o3k --set image.tag=spec021 --set image.pullPolicy=Never --set deploymentMode=single-node --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true --set nova.libvirtMode=real --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables --set glance.storageMode=local --set cinder.storageMode=local --set consoleProxy.enabled=true --set nova.consoleProxyBaseUrl=http://$NODE_IP:30680 --set endpointHost=$NODE_IP --set secrets.jwtSecret=changeme --set secrets.adminPassword=secret --set database.url='postgres://o3k:secret@127.0.0.1:5432/o3k?sslmode=disable'
kubectl -n o3k rollout status deploy/o3k-o3k --timeout=180s
kubectl -n o3k rollout status deploy/o3k-o3k-console-proxy --timeout=120s

echo "--- service modes (every one must be real) ---"
kubectl -n o3k get secret o3k-o3k-config -o jsonpath='{.data.o3k\.yaml}' | base64 -d | grep -E "_mode:"

echo "--- horizon ---"
helm upgrade --install horizon charts/horizon --namespace o3k
kubectl -n o3k rollout status deploy/horizon-o3k-horizon --timeout=180s

export OS_AUTH_URL=http://$NODE_IP:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
echo "--- auth ---"
openstack token issue -f value -c id >/dev/null && echo "authenticated"

echo "--- image ---"
test -s /tmp/cirros.img || curl -sL -o /tmp/cirros.img http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image show cirros >/dev/null 2>&1 || openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img cirros >/dev/null
openstack image list -f value -c Name

echo "--- network + guest ---"
openstack network show spec021-net >/dev/null 2>&1 || openstack network create spec021-net >/dev/null
openstack subnet show spec021-subnet >/dev/null 2>&1 || openstack subnet create --network spec021-net --subnet-range 10.30.0.0/24 spec021-subnet >/dev/null
openstack server show vm1 >/dev/null 2>&1 || openstack server create --flavor m1.small --image cirros --network spec021-net vm1 --wait >/dev/null
openstack server show vm1 -f value -c status -c addresses
```

**Desired outcome**, reading down the output:

- all six storage subdirectories created (silent on success)
- both o3k rollouts complete
- the mode read-back shows **every** service real:
  `libvirt_mode: "real"`, `networking_mode: "iptables"`,
  `security_group_mode: "iptables"`, and `storage_mode: "local"` twice
  (cinder and glance)
- the horizon rollout completes
- `authenticated`
- `cirros` listed
- `ACTIVE` and an address on `spec021-net`

### Why those particular lines

**`endpointHost` must be set, or Horizon breaks after login.** It renders
`O3K_ENDPOINT_HOST`, which is what O3K advertises in the service catalog. Left
empty, the catalog falls back to the database-seeded endpoints, which say
`localhost` — so Horizon authenticates fine and then dies on the first catalog call
with *"Unable to establish connection to http://localhost:9696/v2.0/extensions"*,
because `localhost` inside the Horizon pod is the Horizon pod.

Use the **node IP**, not the Service DNS name. With `hostNetwork`, O3K listens on
the node, so the node IP is reachable both from the Horizon pod and from your host
CLI — one value that works for both, and no `/etc/hosts` alias needed. Verify with
the catalog dump:

```bash
openstack catalog list -f value -c Name -c Endpoints
```

Every URL must carry the node IP. A `localhost` in there means the flag is missing.

**`realMode.enabled=true` does not make the services real.** It only grants the pod
privileges, host networking and host mounts; each service keeps its own mode, and
every one defaults to `stub`. Setting `nova.libvirtMode=real` alone gives a real
hypervisor served by a stub Glance, which stores no bytes — the VM then fails to
boot with `image … checksum mismatch`, because Nova materialises a base from an
image whose content was never written. Stub Neutron would leave the guest with no
bridge. The mode read-back exists so this cannot pass unnoticed.

**Pre-creating the storage subdirectories** removes a race. The chart's hostPath is
`DirectoryOrCreate`, which materialises only `/var/lib/o3k`, and its initContainer
creates the subdirectories inside the **o3k** pod's startup. The console proxy is a
separate Deployment with no ordering guarantee, so it can start first and find
`console-tokens` missing — every token lookup then fails at connect time while the
pod still reports Ready, since its probe is only a TCP check. The list mirrors
`pkg/paths`.

**The database is addressed by IP, not Service name.** The o3k pod runs with
`hostNetwork: true` and `dnsPolicy: ClusterFirst`, which Kubernetes silently
degrades to `Default` — so it resolves through the *host's* resolver and cannot look
up in-cluster Service names at all. `127.0.0.1` needs no DNS. (That dnsPolicy is a
pre-existing chart defect, not fixed here.)

**`?sslmode=disable` is required**, and its absence fails in a confusing place. The
pool uses pgx, which defaults to `sslmode=prefer` and falls back to plaintext — so
the log says *"Database connection established"*. Migrations then run through
golang-migrate on lib/pq, which defaults to `require`, and fail with `pq: SSL is not
enabled on the server`. One URL, two drivers, two defaults.

**`pullPolicy=Never`** with the imported tag pins the run to your build. An
`ImagePullBackOff` means the import did not land — re-check
`sudo k3s ctr images ls | grep o3k:spec021`.

**`o3k.host` stays at its default** `o3k-o3k`, the Service name the o3k chart renders
for release `o3k` (`{release}-{chart}`). Horizon resolves it through Kubernetes DNS,
server-side, so no host `/etc/hosts` entry is involved.

If a rollout times out, read the pod log before anything else — O3K exits early and
loudly on a database it cannot reach:

```bash
kubectl -n o3k logs -l app.kubernetes.io/name=o3k --tail=20
```

---

## Test steps

### R1 — The guest console binds loopback only (FR-017)

```bash
sudo virsh dumpxml $(sudo virsh list --name | head -1) | grep -A2 "<graphics"
```

**Desired outcome**: `port='59xx'` (a real number, **not** `-1`), `listen='127.0.0.1'`,
and `sharePolicy='force-shared'`. A `-1` means the readback would be reading the
inactive definition — the original bug in a new costume.

```bash
nc -vz $NODE_IP 5900
```

**Desired outcome**: **fails**. If this connects, the guest is reachable without a
token and the entire access-control model is void.

### R2 — A console URL is issued, and it is real (SC-001, SC-003)

```bash
openstack console url show vm1
```

**Desired outcome**: a URL of the form
`http://<NODE_IP>:30680/vnc_lite.html?path=%3Ftoken%3D<32 hex chars>`.
It must carry your configured origin — **not** `localhost`, which is what F1
returned — and must not name `vnc_auto.html`, which does not exist in noVNC 1.6.0.

The binary is at `/app/o3k` in the image and is not on `$PATH`:

```bash
kubectl -n o3k exec deploy/o3k-o3k -- /app/o3k console-tokens list
```

**Desired outcome**: one row, showing the instance, `127.0.0.1:<port>` and a
remaining lifetime. The token itself is redacted.

### R3 — The framebuffer renders and takes input (SC-001, SC-002) ⭐

Open Horizon at `http://$NODE_IP:<horizon-nodeport>/dashboard`, log in as
`admin` / `secret`, go to **Project → Compute → Instances → vm1 → Console**.

**Desired outcome**: the cirros boot output and login prompt appear **within 10
seconds**. Click into the console, press Enter, log in as `cirros` / `gocubsgo`,
and run `uname -a`.

This is the acceptance criterion for the whole ticket. Capture a screenshot.

If it fails, the probes in `quickstart.md` isolate which side is at fault:

```bash
kubectl -n o3k logs deploy/o3k-o3k-console-proxy | tail -20
```

`connecting to: 127.0.0.1:59xx` means the proxy resolved the token.
`Token '…' not found` means the projection entry is missing, stale, or unreadable.

### R4 — Both API surfaces agree (FR-005)

```bash
TOKEN=$(openstack token issue -f value -c id)
VM=$(openstack server show vm1 -f value -c id)
curl -s -X POST "http://$NODE_IP:8774/v2.1/servers/$VM/action" -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"os-getVNCConsole":{"type":"novnc"}}' | jq -r '.console.url'
```

**Desired outcome**: same shape and same origin as R2.

### R5 — Two viewers share one guest (SC-008, FR-018a)

Open the Console tab in two browser windows at once.

**Desired outcome**: **both** stay connected and show the guest. With libvirt's
default `allow-exclusive` the first viewer would be silently disconnected, which
reads as a bug during exactly the support scenario a console exists for.

### R6 — Two instances stay isolated (SC-008, FR-018)

```bash
openstack server create --flavor m1.small --image cirros --network spec021-net vm2 --wait
```

Open both consoles side by side for two minutes.

**Desired outcome**: each shows its own guest. No cross-talk.

### R7 — Reboot re-reads the port (SC-006, FR-004)

```bash
openstack server reboot --hard vm1
sleep 20
openstack console url show vm1
```

**Desired outcome**: a **new** token, and the console connects on the first
attempt. The port may well have changed; nothing may be cached.

### R8 — The security probe (SC-005)

```bash
O3K_MODE=real ./test/console_access_test.sh
```

**Desired outcome**: `failed: 0`. Covers reuse-within-window, invented tokens,
cross-instance tokens, and revocation on stop and delete.

Add restart survival (SC-007) explicitly:

```bash
O3K_MODE=real O3K_RESTART_CMD='kubectl -n o3k rollout restart deploy/o3k-o3k && kubectl -n o3k rollout status deploy/o3k-o3k --timeout=180s' ./test/console_access_test.sh
```

**Desired outcome**: the pre-restart link still resolves. This is the only reason
tokens are persisted rather than kept in memory.

### R9 — Expiry is actually enforced (SC-004)

Slow but worth running once, because the proxy performs **no** expiry check of its
own — a token dies when O3K deletes its projection entry, so this tests the
reconciler, not a clock:

```bash
O3K_MODE=real O3K_CHECK_EXPIRY=1 ./test/console_access_test.sh
```

**Desired outcome**: the expired token is refused. Effective validity is
`console_token_ttl + console_reconcile_interval` — 10m30s at the defaults.

### R10 — The serial fallback is unregressed (SC-009, FR-026)

```bash
openstack console log show vm1 | tail -5
```

**Desired outcome**: real cirros boot output.

### R11 — Fail-fast when libvirt is unreachable (SC-010, FR-007)

```bash
sudo systemctl stop libvirtd
time openstack console url show vm1
sudo systemctl start libvirtd
```

**Desired outcome**: an error in **under 2 seconds**, not a hang.

---

## Teardown

```bash
export KUBECONFIG=~/.kube/k3s.yaml
openstack server delete vm1 vm2
openstack subnet delete spec021-subnet
openstack network delete spec021-net
openstack image delete cirros
rm -f /tmp/cirros.img /tmp/o3k-spec021.tar
helm -n o3k uninstall horizon
helm -n o3k uninstall o3k
kubectl delete namespace o3k
make db-down
sudo virsh list --all
sudo ls -la /var/lib/o3k/console-tokens/
```

**Desired outcome**: no leftover domains, and the console-token directory is empty
— a surviving entry means a token outlived its instance, which is the one direction
this design must never fail in.

Check that *before* removing the node-local state, then drop it:

```bash
sudo rm -rf /var/lib/o3k
```

Uninstalling the o3k release runs the chart's pre-delete purge hooks, so do that
**before** removing k3s. Then remove k3s itself:

```bash
sudo /usr/local/bin/k3s-uninstall.sh
```

**Desired outcome**: `k3s` is gone. This also deletes the imported image and the
cluster's state; `~/.kube/k3s.yaml` is left behind, so remove it if you want a
completely clean host.
