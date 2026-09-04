# Host testing — Lightstack on k3s: deploy → boot a VM → SSH in → ping the internet

End-to-end functional validation of **single-node real mode on k3s**: deploy Lightstack via Helm, boot a
real (nested) KVM guest, inject your SSH key via the keypair/config-drive, **SSH into the VM from the
node**, and **`ping` the outside world from inside the guest**.

```
clean slate → install k3s → build+import image (this worktree) → deploy real mode →
smoke test → image + network + keypair → boot VM → SSH in → give egress → ping 1.1.1.1
```

> **Run two shells:** **T1** = host + `kubectl`/`helm` (k3s as root — real mode needs host
> netns/iptables + libvirt). **T2** = the OpenStack CLI (`source ~/.lightstack-k3s-env`).
>
> **Build + deploy from THIS worktree** (`/home/fnk/code/sap/ora/lightstack/bugfix/510`): the image carries
> the SPEC-011 binary fix and the chart carries the `init-host-storage` initContainer, so the deploy
> needs **no manual `mkdir`** under `/var/lib/lightstack` (that old workaround is gone).

---

## 0. Prerequisites (one-time)

```bash
sudo apt install -y qemu-utils genisoimage dnsmasq iproute2 iptables postgresql-client
systemctl status libvirtd        # active
kvm-ok                           # KVM available (nested — see §8a)
podman --version || docker --version ; go version    # container build + Go 1.26+
helm version                     # helm 3.x
# OpenStack client in T2: pipx install python-openstackclient  (or apt install python3-openstackclient)
```
> k3s must run as **real root** (the default `get.k3s.io` install). Rootless can't do host netns or
> root-owned `/var/lib/lightstack`.

---

## 1. Clean slate

```bash
# release + namespace (the chart MANAGES the lightstack namespace, so uninstall deletes it — wait for it):
helm uninstall lightstack-rt -n lightstack --no-hooks 2>/dev/null || true
kubectl wait --for=delete ns/lightstack --timeout=120s 2>/dev/null || true
kubectl delete ns lightstack --ignore-not-found 2>/dev/null || true

# host real-mode state: VMs, lightstack dnsmasq, br-* bridges, q*/light-ns-* netns, lightstack iptables, FIP /32s,
# then the storage tree + dev Postgres:
sudo rm -rf /var/lib/lightstack
docker rm -f lightstack-postgres 2>/dev/null; podman rm -f lightstack-postgres 2>/dev/null; sudo podman rm -f lightstack-postgres 2>/dev/null; true
```
Verify clean:
```bash
sudo virsh list --all                                  # no domains
kubectl get ns lightstack 2>&1 | grep -q NotFound && echo "clean: ns gone"
sudo ls -d /var/lib/lightstack 2>/dev/null || echo "clean: /var/lib/lightstack absent"
```
> Simplest full reset: **reboot the host** (clears kernel networking), then redo VM-undefine +
> `sudo rm -rf /var/lib/lightstack` + Postgres-container removal.

---

## 2. (Optional) local gates first

```bash
cd /home/fnk/code/sap/ora/lightstack/bugfix/510
go build ./... && golangci-lint run
go test $(go list ./... | grep -v '/test/contract')        # full unit suite (skip live-server contract)
helm lint charts/lightstack --set image.repository=lightstack --set secrets.jwtSecret=x \
  --set secrets.adminPassword=x --set database.url=postgres://x
helm unittest charts/lightstack
```

---

## 3. Install k3s (service runs as root — not rootless)

```bash
curl -sfL https://get.k3s.io | sh -
ps -o user= -p "$(pgrep -xo k3s)"                # → root
mkdir -p ~/.kube && sudo install -m 600 -o "$USER" /etc/rancher/k3s/k3s.yaml ~/.kube/config
export KUBECONFIG=~/.kube/config
kubectl get nodes                                # Ready
```
> `kubectl`/`helm` run as your user after this. Still need `sudo`: §4's `k3s ctr images import` and
> the host `virsh`/`ip`/`iptables` checks.

---

## 4. Build the image (this worktree) and import it into k3s

k3s has its own containerd; docker/podman images aren't visible until imported. **Tag with the full
`docker.io/library/` name** or the chart's `image: lightstack:dev` won't match.
```bash
cd /home/fnk/code/sap/ora/lightstack/bugfix/510
podman build -t docker.io/library/lightstack:dev -f build/package/Dockerfile .   # docker: identical flags
podman save  docker.io/library/lightstack:dev -o /tmp/lightstack-dev.tar
sudo k3s ctr images import /tmp/lightstack-dev.tar
sudo k3s ctr images ls | grep -i lightstack             # → docker.io/library/lightstack:dev
```
> ⚠️ A bare `podman build -t lightstack:dev` becomes **`localhost/lightstack:dev`** → pod stuck on
> `ErrImageNeverPull` (pullPolicy is `Never`). Use the `docker.io/library/` tag. See §10.

The image already carries the real-mode deps (`qemu-img`, genisoimage, `dnsmasq`, `iproute2`,
`iptables`, `ipset`) — see `build/package/Dockerfile`.

---

## 5. External Postgres (the chart deploys none)

```bash
make db-up                                       # lightstack-postgres on :5432 (lightstack/secret/lightstack)
PGPASSWORD=secret psql -h 127.0.0.1 -U lightstack -d lightstack -c '\dt' 2>/dev/null | head
```
> The pod is `hostNetwork: true`, so `127.0.0.1` inside it is the host — the published `:5432` is
> reachable as `localhost`. Lightstack auto-migrates the schema on pod startup.

---

## 6. Helm deploy — single-node REAL mode (no manual mkdir)

```bash
cd /home/fnk/code/sap/ora/lightstack/bugfix/510
helm install lightstack-rt charts/lightstack -n lightstack --create-namespace \
  --set image.repository=lightstack --set image.tag=dev --set image.pullPolicy=Never \
  --set database.url='postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" \
  --set secrets.adminPassword='Host-Admin-1' \
  --set nova.libvirtMode=real \
  --set neutron.networkingMode=iptables \
  --set neutron.securityGroupMode=iptables \
  --set cinder.storageMode=local \
  --set glance.storageMode=local \
  --set realMode.enabled=true \
  --set realMode.privileged=true \
  --set realMode.hostNetwork=true \
  --set podSecurity.applyPrivilegedNamespaceLabel=true

kubectl -n lightstack rollout status deploy/lightstack-rt-lightstack --timeout=300s
kubectl -n lightstack get pod -l app.kubernetes.io/instance=lightstack-rt -o wide   # Running, on the node, host IP
```
`realMode.enabled+privileged+hostNetwork` gives the pod the host net namespace, the host libvirt
socket, and the host storage mount (`/var/lib/lightstack`). Real mode pins **1 replica** (`Recreate`).

Confirm the SPEC-011 auto-create did its job (no manual mkdir was run):
```bash
kubectl -n lightstack get pod -l app.kubernetes.io/instance=lightstack-rt \
  -o jsonpath='{.items[0].spec.initContainers[*].name}{"\n"}'    # → init-host-storage
sudo ls /var/lib/lightstack                                             # images volumes instances cloud-init dhcp
```
> PodSecurity rejection on schedule? Confirm the namespace label:
> `kubectl get ns lightstack -o jsonpath='{.metadata.labels}'` → `pod-security.kubernetes.io/enforce: privileged`.

---

## 7. Smoke test + point the CLI at the node

```bash
kubectl -n lightstack get endpoints lightstack-rt-lightstack          # lists <nodeIP>:35357 once Ready
helm test lightstack-rt -n lightstack                          # identity-check pod PASSes
```
> **`helm test` "Could not connect … 35357" right after install?** Startup race — the test pod fired
> before kube-proxy programmed the endpoint. The API is fine; confirm with
> `curl -fsS http://127.0.0.1:35357/v3`, then `kubectl -n lightstack delete pod lightstack-rt-lightstack-identity-check`
> and re-run `helm test`.

Env file for T2 — `OS_PASSWORD` MUST equal §6's `secrets.adminPassword`:
```bash
cat > ~/.lightstack-k3s-env <<'EOF'
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD='Host-Admin-1'                 # ← MUST match §6 secrets.adminPassword
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF
source ~/.lightstack-k3s-env
openstack token issue >/dev/null && echo "auth OK"
openstack catalog list -f value -c Name          # the six services
```
> The CLI talks to `127.0.0.1:35357` directly (hostNetwork binds it on the node), so it bypasses the
> ClusterIP routing the `helm test` pod uses — auth works even if `helm test` raced.

---

## 8. Boot a VM, SSH in, and ping the outside world — the main event

All `openstack` commands run from **T2** on the node (auth via §7's `~/.lightstack-k3s-env`).

> **Why SSH/ping works on the node:** the Lightstack pod is `hostNetwork`, so the bridge `br-<netid>`, the VM
> tap, dnsmasq, and the NAT rules it creates all live in the **node's** network namespace — exactly as
> if Lightstack ran bare. You SSH from the node's shell straight to the VM's fixed IP, and egress masquerades
> out the node's uplink.

### 8a. Preflight — nested virtualization
```bash
kubectl exec -n lightstack deploy/lightstack-rt-lightstack -- sh -c 'ls -l /dev/kvm 2>&1; grep -Ec "vmx|svm" /proc/cpuinfo'
# /dev/kvm present + non-zero → nested KVM. Missing → TCG (slow, still boots cirros).
```
> No `/dev/kvm`? On the **L0** host: `options kvm_intel nested=1` (or `kvm_amd`), give this VM a
> `host-passthrough` CPU (`virsh edit <thisvm>` → `<cpu mode='host-passthrough'/>`), reboot.

### 8b. Image, network, keypair, and a VM with your SSH key injected
```bash
[ -f cirros.img ] || wget -O cirros.img https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image show cirros >/dev/null 2>&1 || \
  openstack image create cirros --disk-format qcow2 --container-format bare --file cirros.img

openstack network create net1
openstack subnet create sub1 --network net1 --subnet-range 10.0.0.0/24   # enable_dhcp on by default → dnsmasq

# keypair from your host key → injected into the guest via the config-drive ISO,
# which works even if the 169.254.169.254 metadata route is unreachable:
[ -f ~/.ssh/id_rsa.pub ] || ssh-keygen -t rsa -N '' -f ~/.ssh/id_rsa
openstack keypair show k1 >/dev/null 2>&1 || openstack keypair create --public-key ~/.ssh/id_rsa.pub k1

openstack server create --image cirros --flavor m1.small --network net1 --key-name k1 vm1 --wait
openstack server show vm1 -c status -c addresses                         # ACTIVE, net1=10.0.0.x
```

### 8c. SSH into the VM (from the node)
Re-derive the IP from the **current** `vm1` right before connecting (don't reuse a stale value):
```bash
VMIP=$(openstack server show vm1 -f value -c addresses | grep -oE '10\.0\.0\.[0-9]+' | head -1); echo "VMIP=$VMIP"
# give cloud-init ~30–60s after --wait to apply the config-drive, then:
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null cirros@"$VMIP"
#   inside:  $ id ; cat ~/.ssh/authorized_keys      # your k1 key is present (injected)
```
> Run this **from the node**, not a laptop — `10.0.0.x` lives inside the node's network namespace.
> SSH hanging? Confirm DHCP gave the VM its IP and the key landed, via the serial console
> (login `cirros` / `gocubsgo`):
> ```bash
> DOM=$(sudo virsh list --name | grep -i "$(openstack server show vm1 -f value -c id | cut -c1-8)")
> sudo virsh console "$DOM"        # ENTER for a prompt; Ctrl-] to exit
> ```

### 8d. Give the VM internet egress, then ping out from inside it
A VM reaches the internet once its subnet sits behind a router with an external gateway; Lightstack
masquerades it out the node's default-route interface (no operator `br-ex`). On a nested node this
double-NATs through the libvirt L0 — ICMP/TCP still get out.
```bash
openstack network create ext-net
openstack subnet create ext-sub --network ext-net --subnet-range 172.24.4.0/24 --no-dhcp
openstack router create r1
openstack router set r1 --external-gateway ext-net
openstack router add subnet r1 sub1
```
Host side — confirm the egress NAT is programmed (privileged pod shares the node netns):
```bash
kubectl exec -n lightstack deploy/lightstack-rt-lightstack -- sh -c \
  'cat /proc/sys/net/ipv4/ip_forward; iptables -t nat -S POSTROUTING | grep MASQUERADE; iptables -S FORWARD | grep 10.0.0.0/24'
```
Guest side — ping the outside from inside the VM:
```bash
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null cirros@"$VMIP" \
  'ping -c3 1.1.1.1 ; echo "--- TCP ---"; wget -S -T5 -O /dev/null http://1.1.1.1 2>&1 | head -3'
```
**Pass** = `ping -c3 1.1.1.1` from inside the VM succeeds (DNS may be unset — use an IP). If ping
fails but `POSTROUTING` shows the `MASQUERADE`, it's host **FORWARD ordering** (libvirt + flannel +
Lightstack all write `FORWARD`) — see §10.

---

## 9. Cleanup / reset between runs

```bash
helm uninstall lightstack-rt -n lightstack --no-hooks 2>/dev/null || true   # this also deletes the lightstack namespace…
kubectl wait --for=delete ns/lightstack --timeout=120s 2>/dev/null || true   # …so WAIT before any reinstall
# host reset: undefine VMs, remove br-*, netns, lightstack iptables, then:
sudo rm -rf /var/lib/lightstack
# wipe the cluster entirely:  /usr/local/bin/k3s-uninstall.sh
```

---

## 10. Troubleshooting

- **Pod `ErrImageNeverPull` (`image "lightstack:dev" is not present with pull policy of Never`)** → the
  image isn't in k3s containerd under the expected name. Do §4 (`docker.io/library/lightstack:dev`). If you
  built bare with podman it's `localhost/lightstack:dev` → re-import correctly, or
  `helm upgrade lightstack-rt charts/lightstack -n lightstack --reuse-values --set image.repository=localhost/lightstack`.
  Check: `sudo k3s ctr -n k8s.io images ls | grep lightstack`.
- **`INSTALLATION FAILED: … namespace lightstack … is being terminated`** → the chart manages the `lightstack`
  namespace, so `helm uninstall` deletes it; a back-to-back `helm install` races the termination.
  Wait first: `kubectl wait --for=delete ns/lightstack --timeout=120s`, then reinstall.
- **`helm test` connection refused right after (re)deploy** → kube-proxy/endpoint startup race;
  confirm `curl -fsS http://127.0.0.1:35357/v3`, delete the Error test pod, re-run `helm test`.
- **`401 "Unrecognized schema in response body"`** → `OS_PASSWORD` ≠ §6 `secrets.adminPassword`;
  re-source `~/.lightstack-k3s-env`.
- **VM boots to `ERROR`, log: `Could not open backing image '/var/lib/lightstack/images/image-*.raw'`** →
  the running **image** lacks the SPEC-011 binary fix (built from the wrong worktree), so Glance fell
  back to `~/.lightstack`. Rebuild from `bugfix/510` (§4) and re-import. (Do **not** `mkdir` the subdirs to
  paper over it.)
- **SSH hangs** → DHCP/key timing; use the serial console (8c) to confirm the IP and
  `authorized_keys`. Ensure you're SSHing **from the node**.
- **Ping fails but `POSTROUTING` shows `MASQUERADE`** → host **FORWARD** chain ordering (libvirt +
  flannel + lightstack). Inspect `sudo iptables -S FORWARD`; an early `REJECT`/`DROP` before lightstack's accept
  rules blocks the guest subnet. Reordering / an explicit accept for `10.0.0.0/24` fixes it.
- **No `/dev/kvm` in the pod** → nested virt not enabled on L0 (see §8a); VMs still boot under TCG,
  just slowly.
- **Reset everything** → §9, or reboot the host then redo the three persistent items (undefine VMs,
  `rm -rf /var/lib/lightstack`, recreate Postgres).
