# Host testing guide — SPEC-010 Helm teardown on k3s (single-node real mode)

End-to-end validation of the **chart deployment + the `dataRetentionPolicy: delete`
blocking teardown** on a real single-node cluster. This is the piece the SPEC-009
runbook (`docs/specs/009-single-node-real-mode/HOST-TESTING.md`) can't cover: there
Lightstack runs as a bare host process; here it runs as a privileged pod and the teardown
runs as a Helm pre-delete hook Job. The flow is exactly what you asked for:

```
clean slate → run all local gates → install k3s → build+import image →
helm deploy real mode → smoke + functional tests → tear it down → verify zero residue
```

> **Run two shells:** **T1** = host + `kubectl`/`helm` (root — real mode needs host
> netns/iptables + libvirt), **T2** = the OpenStack CLI. The automated *stub*-mode
> version of §9–§12 is `test/helm_teardown_test.sh`; this guide is the **real**-mode
> manual equivalent plus the host-residue checks a stub cluster can't make.

| Validates | Where |
|---|---|
| SC-001 zero release residue (cluster **and** host) | §9c / §9d |
| SC-002 immediate redeploy healthy, no collisions | §12 |
| SC-003 the purge **blocks** uninstall (fail-closed) | §9b |
| SC-004 `retain` runs **no** teardown hooks (no regression) | §10 |
| SC-005 idempotent re-run | §11 |
| SC-006 `retain` leaves all state intact | §10 |
| **NEW — real-mode host purge** (domains, netns, bridges, iptables, `/var/lib/lightstack`) | §8 snapshot → §9d |
| DB is operator-owned — **NOT** reset by teardown | §9e |

---

## 0. Prerequisites (one-time)

Real mode needs the same host setup as SPEC-009 §0 — do that first:
```bash
sudo apt install -y qemu-utils genisoimage dnsmasq iproute2 iptables postgresql-client
systemctl status libvirtd        # active
kvm-ok                           # KVM available
docker --version ; go version    # docker (build + dev Postgres) + Go 1.26+
helm version                     # helm 3.x
helm plugin install https://github.com/helm-unittest/helm-unittest 2>/dev/null || true
# OpenStack client in T2: pipx install python-openstackclient  (or apt)
```
> **k3s must run as real root** (the default `get.k3s.io` systemd install is). Real
> mode can't work under rootless containers — host netns + root-owned `/var/lib/lightstack`
> need real privileges (same constraint #503 hit).

---

## 1. Start from a clean slate (host + any prior cluster)

You said you already cleaned up — this **verifies** it and gives the full reset if not.

```bash
# (a) any prior Helm release + k3s leftovers
sudo k3s kubectl get ns lightstack 2>/dev/null && helm uninstall lightstack-rt -n lightstack --no-hooks 2>/dev/null || true
sudo k3s kubectl delete ns lightstack --ignore-not-found 2>/dev/null || true

# (b) host real-mode state — reuse the SPEC-009 reset verbatim (it is authoritative):
#     VMs (destroy+undefine), lightstack dnsmasq, br-* bridges, q*/light-ns-* netns,
#     lightstack iptables, FIP /32s, /var/lib/lightstack/*, the lightstack-postgres container.
#     See docs/specs/009-single-node-real-mode/HOST-TESTING.md §1 + "Cleanup — full reset".
```
Verify clean:
```bash
sudo virsh list --all                                   # no domains
sudo ip netns list | grep -E 'light-ns-|qrouter-' || echo "clean: no lightstack netns"
ip -br link show type bridge | grep '^br-'   || echo "clean: no lightstack bridges"
sudo iptables -S | grep 'lightstack-'               || echo "clean: no lightstack chains"
sudo ls /var/lib/lightstack 2>/dev/null             || echo "clean: /var/lib/lightstack absent"
```
> Simplest guarantee: **reboot the host** — it clears all kernel networking
> (`bridges/taps/netns/iptables/ip_forward`) for free; then you only redo the
> VM-undefine + `rm -rf /var/lib/lightstack/*` + Postgres-container removal.

---

## 2. Run all the local gates first (no cluster needed)

Catch anything before you spend time on k3s. From the repo root (`feature/53` worktree):
```bash
cd /home/fnk/code/sap/ora/lightstack/feature/53

go build ./...                       # compiles
go vet ./internal/... ./pkg/...      # vet
go test -race ./internal/teardown/ ./internal/neutron/ ./pkg/storage/   # teardown unit tests (focused)
go test ./internal/... ./pkg/...     # full UNIT suite (everything under internal/ + pkg/)

# chart gates (lint needs the required values, else the validate gate fails fast)
helm lint charts/lightstack \
  --set image.repository=lightstack --set secrets.jwtSecret=x \
  --set secrets.adminPassword=x --set database.url=postgres://x
helm unittest charts/lightstack             # the tests/*.yaml (incl. teardown_test.yaml) — expect all pass

bash -n test/helm_teardown_test.sh   # scenario script parses
```
> **Not `make test` here.** `make test` runs `go test -v ./...`, which includes the
> `test/contract/*` gophercloud suite — those tests need a **running** Lightstack at
> `localhost:35357` and fail with `connection refused` until §6/§7. The unit sweep
> above (`./internal/... ./pkg/...`) is the right pre-cluster gate; run the contract
> suite later against the deployed server (§7).

**Pass** = build/vet/unit green, `helm lint` clean, `helm unittest` all-pass, `bash -n` silent.
(Contract-test `connection refused` failures from `go test ./...`/`make test` are **expected** without a server — not a regression.)

---

## 3. Install k3s (the service must run as root — do NOT use rootless)

Run the installer **as your normal user** — the script self-elevates with `sudo` for
the privileged steps. What matters is that the resulting `k3s` *service* runs as root
(the default); that's required for privileged + `hostNetwork` pods, host netns, and
root-owned `/var/lib/lightstack`. Rootless k3s would break the real-mode test.
```bash
curl -sfL https://get.k3s.io | sh -
sudo systemctl status k3s --no-pager | head -3   # active (running)
ps -o user= -p "$(pgrep -xo k3s)"                # → root  (confirms NOT rootless)
# make the kubeconfig usable by helm/kubectl as your user (it's root-owned, mode 600):
mkdir -p ~/.kube && sudo install -m 600 -o "$USER" /etc/rancher/k3s/k3s.yaml ~/.kube/config
export KUBECONFIG=~/.kube/config
kubectl get nodes                    # Ready — no sudo needed from here for kubectl/helm
```
> `kubectl`/`helm` run as your normal user after this. Still need `sudo`: §4's
> `k3s ctr images import` and the host-state checks in §8/§9d (`virsh`, `ip netns`, `iptables`).

---

## 4. Build the Lightstack image and import it into k3s

k3s has its **own** containerd — `docker`/`podman` images aren't visible to it until
you import them. Build, save, import, then **read back the exact name**:
```bash
cd /home/fnk/code/sap/ora/lightstack/feature/53
# Tag with the FULLY-QUALIFIED docker.io/library/ name so the chart's default
# image.repository=lightstack matches under BOTH podman and docker (see note).
podman build -t docker.io/library/lightstack:dev -f build/package/Dockerfile .   # docker: identical flags
podman save  docker.io/library/lightstack:dev -o /tmp/lightstack-dev.tar
sudo k3s ctr images import /tmp/lightstack-dev.tar
sudo k3s ctr images ls | grep -i lightstack                    # → docker.io/library/lightstack:dev
```
> ⚠️ **Why the full `docker.io/library/` name:** the chart renders `image: lightstack:dev`, which
> containerd normalizes to `docker.io/library/lightstack:dev`. Tagging the build that way makes
> §6's default `image.repository=lightstack` match **as-is**. A bare `podman build -t lightstack:dev`
> becomes **`localhost/lightstack:dev`** (podman prefixes local builds) → the chart can't find it
> → `ErrImageNeverPull`. (docker already tags bare builds under `docker.io/library/`, so it
> works either way — the full name is just safe for both.)

The image already carries the real-mode deps (`qemu-img`, `cdrkit`/genisoimage,
`dnsmasq`, `iproute2`, `iptables`, `ipset`) — see `build/package/Dockerfile`.

> Optional — pre-import the kubectl image the quiesce hook uses, so §9 doesn't pull at
> hook-time. **`rancher/kubectl` has no rolling `v1.31` tag — use a real patch version**
> (verify it resolves first):
> ```bash
> podman pull rancher/kubectl:v1.31.0 || podman pull alpine/kubectl:1.31     # whichever resolves
> podman save rancher/kubectl:v1.31.0 -o /tmp/kubectl.tar && sudo k3s ctr images import /tmp/kubectl.tar
> ```

---

## 5. External Postgres (the chart deploys none — FR-006)

Reuse SPEC-009's dev Postgres; Lightstack **auto-migrates the schema on pod startup**
(FR-012b), so a fresh empty DB is enough:
```bash
make db-up                           # lightstack-postgres on 0.0.0.0:5432 (lightstack/secret/lightstack)
PGPASSWORD=secret psql -h 127.0.0.1 -U lightstack -d lightstack -c '\dt' 2>/dev/null | head   # reachable
```
> The real-mode pod runs with `hostNetwork: true`, so **`127.0.0.1` inside the pod
> is the host** — the published `:5432` is reachable as `localhost`. If your Postgres
> binds elsewhere, use the node's LAN IP in the URL below.
> If `make db-up` errors with *"container name lightstack-postgres is already in use"*, it's
> already running — reuse it, or reset first with `make db-down` (or `podman rm -f lightstack-postgres`).

---

## 6. Helm deploy — single-node REAL mode

> `image.repository=lightstack` matches §4's `docker.io/library/lightstack:dev` under both podman and
> docker — leave it as-is. If the pod still shows `ErrImageNeverPull`, the imported name
> differs: check `sudo k3s ctr images ls | grep lightstack` and fix the running release with
> `helm upgrade lightstack-rt charts/lightstack -n lightstack --reuse-values --set image.repository=<name-without-:dev>`.

**Real mode — pre-create the node-local storage subdirs on the host first.** The chart's
`hostPath` is `DirectoryOrCreate` on `/var/lib/lightstack` *only*; it does NOT create the service
subdirs. If `images/` is missing when the pod starts, Glance silently falls back to
`~/.lightstack/images` inside the pod and Nova can't find the image base → VMs boot to `ERROR`
(see §15). Proper fix is a chart initContainer / binary `MkdirAll` — until then:
```bash
sudo mkdir -p /var/lib/lightstack/{images,volumes,instances,cloud-init,dhcp}
```
```bash
helm install lightstack-rt charts/lightstack -n lightstack --create-namespace \
  --set image.repository=lightstack --set image.tag=dev --set image.pullPolicy=Never \
  --set database.url='postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" \
  --set secrets.adminPassword='Teardown-Admin-1' \
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
`realMode.enabled+privileged+hostNetwork` gives the pod the host net namespace, the
host libvirt socket (`/var/run/libvirt/libvirt-sock`), and the host storage mount
(`/var/lib/lightstack`) — the same access #503 validated for the bare process. Real mode
pins **1 replica** with a `Recreate` strategy (host ports can't overlap).

> If the pod won't schedule with a PodSecurity error, the namespace label didn't
> apply — confirm `kubectl get ns lightstack -o jsonpath='{.metadata.labels}'` shows
> `pod-security.kubernetes.io/enforce: privileged`.

---

## 7. Smoke test the deployment

```bash
kubectl -n lightstack rollout status deploy/lightstack-rt-lightstack --timeout=120s   # wait until Ready
kubectl -n lightstack get endpoints lightstack-rt-lightstack                          # must list <nodeIP>:35357 before testing
helm test lightstack-rt -n lightstack              # runs the in-cluster identity-check pod — should PASS
```
> **If the test fails with `Could not connect ... port 35357` right after a (re)deploy,
> it's a startup race** — the test pod can fire before kube-proxy programs the new
> Endpoint into iptables. Confirm the Endpoint is listed (above), then just re-run
> `helm test`. It is not a Lightstack or chart fault.

Then point the CLI at the host ports (hostNetwork binds them on the node). Write the
env file fresh so the credentials match THIS release — **`OS_PASSWORD` MUST equal the
`secrets.adminPassword` you deployed in §6** (here `Teardown-Admin-1`, NOT the SPEC-009
bare-host `secret`; a stale password shows up as `401 "Unrecognized schema in response body"`):
```bash
cat > ~/.lightstack-env <<'EOF'
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD='Teardown-Admin-1'        # ← MUST match §6 secrets.adminPassword
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF
source ~/.lightstack-env
openstack token issue >/dev/null && echo "auth OK"
openstack catalog list -f value -c Name        # the six services
```
> This **overwrites** the SPEC-009 `~/.lightstack-env` (which used `localhost` + `secret`). If
> you later go back to bare-host SPEC-009 testing, restore that password — or keep them
> separate by writing this to `~/.lightstack-k3s-env` and sourcing that instead.

**Pass** = `helm test` PASS + `token issue` works against the node.

---

## 8. Build a topology, boot a VM, SSH in, reach the internet — then snapshot

The SPEC-009 §11–§13 functional flow, driven through the cluster-hosted API: a real VM
you can SSH into that can ping the outside. The product behavior is identical (same
binary, same host access) — only the API endpoint moved into a pod. All `openstack`
commands run from `ora` (auth via §7's `~/.lightstack-env`). End state is also the host state
§9 must purge, so §8e snapshots it.

> **Why this works on `ora`:** the Lightstack pod is `hostNetwork`, so the bridge `br-<netid>`,
> the VM tap, dnsmasq, and the NAT rules it creates all live in **`ora`'s** network
> namespace — exactly as if Lightstack ran bare. So you SSH from `ora`'s shell straight to the
> VM's fixed IP, and egress masquerades out `ora`'s uplink.

### 8a. Preflight — nested virtualization
Lightstack boots real KVM guests via `ora`'s libvirtd; since `ora` is itself a libvirt VM, those
guests are **nested**. Confirm KVM is usable (else qemu falls back to slow TCG — cirros
still boots, just sluggishly):
```bash
kubectl exec -n lightstack deploy/lightstack-rt-lightstack -- sh -c 'ls -l /dev/kvm 2>&1; grep -Ec "vmx|svm" /proc/cpuinfo'
# /dev/kvm present + non-zero count → nested KVM. Missing /dev/kvm / count 0 → TCG (slow, still works).
```
> If `/dev/kvm` is missing: on the **L0** host set `options kvm_intel nested=1` (or
> `kvm_amd`), give the `ora` VM a passthrough CPU (`virsh edit ora` →
> `<cpu mode='host-passthrough'/>`), and reboot `ora`.

### 8b. Image, network, keypair, and a VM with your SSH key injected
```bash
[ -f cirros.img ] || wget -O cirros.img https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image show cirros >/dev/null 2>&1 || \
  openstack image create cirros --disk-format qcow2 --container-format bare --file cirros.img

openstack network create net1
openstack subnet create sub1 --network net1 --subnet-range 10.0.0.0/24   # enable_dhcp on by default → dnsmasq

# keypair from your host key → injected into the guest via the config-drive ISO (WS-4),
# which works even if the 169.254.169.254 metadata route is unreachable
[ -f ~/.ssh/id_rsa.pub ] || ssh-keygen -t rsa -N '' -f ~/.ssh/id_rsa
openstack keypair show k1 >/dev/null 2>&1 || openstack keypair create --public-key ~/.ssh/id_rsa.pub k1

openstack server create --image cirros --flavor m1.small --network net1 --key-name k1 vm1 --wait
openstack server show vm1 -c status -c addresses                                 # ACTIVE, net1=10.0.0.x
VMIP=$(openstack server show vm1 -f value -c addresses | grep -oE '10\.0\.0\.[0-9]+' | head -1); echo "VM IP: $VMIP"
openstack volume create --size 1 datavol && openstack server add volume vm1 datavol   # gives the storage purge something too
```

### 8c. SSH into the VM (from `ora`)
Re-derive the IP from the **current** `vm1` right before connecting — don't reuse a `$VMIP`
set during an earlier (maybe failed or recreated) attempt, or you'll SSH to a dead address:
```bash
VMIP=$(openstack server show vm1 -f value -c addresses | grep -oE '10\.0\.0\.[0-9]+' | head -1); echo "VMIP=$VMIP"
# give cloud-init ~30–60s after --wait to apply the config-drive, then:
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null cirros@"$VMIP"
#   inside:  $ id ; cat ~/.ssh/authorized_keys      # your k1 key is present (injected)
```
> **Run this from the `ora` node**, not a laptop/remote shell — `10.0.0.x` lives inside
> `ora`'s network namespace and isn't routable elsewhere (no floating IP on this single
> nested node).
> SSH hanging? Confirm DHCP gave the VM its IP and the key landed via console
> (password `cirros` / `gocubsgo`):
> ```bash
> DOM=$(sudo virsh list --name | grep -i "$(openstack server show vm1 -f value -c id | cut -c1-8)")
> sudo virsh console "$DOM"        # ENTER for a prompt; Ctrl-] to exit
> ```

### 8d. Give the VM internet egress, then ping out from inside it
A VM reaches the internet once its subnet sits behind a router with an external gateway;
Lightstack masquerades it out `ora`'s default-route interface (no operator `br-ex`). On this
nested node that double-NATs through the libvirt L0 — ICMP/TCP still get out.
```bash
openstack network create ext-net
openstack subnet create ext-sub --network ext-net --subnet-range 172.24.4.0/24 --no-dhcp
openstack router create r1
openstack router set r1 --external-gateway ext-net
openstack router add subnet r1 sub1
```
Host side — confirm the egress NAT is programmed (privileged pod = `ora`'s netns):
```bash
kubectl exec -n lightstack deploy/lightstack-rt-lightstack -- sh -c \
  'cat /proc/sys/net/ipv4/ip_forward; iptables -t nat -S POSTROUTING | grep MASQUERADE; iptables -S FORWARD | grep 10.0.0.0/24'
```
Guest side — ping the outside from inside the VM:
```bash
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null cirros@"$VMIP" \
  'ping -c3 1.1.1.1 ; echo "--- TCP ---"; wget -S -T5 -O /dev/null http://1.1.1.1 2>&1 | head -3'
```
**Pass** = `ping -c3 1.1.1.1` from inside the VM succeeds (DNS may be unset — use an IP).
If ping fails but `POSTROUTING` shows the `MASQUERADE`, it's **host FORWARD ordering** on
`ora` (libvirt + flannel + Lightstack all write `FORWARD`) — see §15.

### 8e. Snapshot the "before" state (§9d compares against this)
```bash
echo "== domains =="   ; sudo virsh list --all
echo "== bridges =="   ; ip -br link show type bridge | grep '^br-'
echo "== iptables =="  ; sudo iptables -S | grep -E 'lightstack-(SG|NSI)' ; sudo iptables -t nat -S | grep -E 'lightstack|169\.254\.169\.254|MASQUERADE'
echo "== storage =="   ; sudo ls -R /var/lib/lightstack | head -40
echo "== netns(pod) ==" ; kubectl exec -n lightstack deploy/lightstack-rt-lightstack -- ip netns list
```
> **Note the last line.** Lightstack creates *named* netns (`light-ns-*`, `qrouter-*`).
> In `hostNetwork` mode **bridges + iptables are host-level** (the pod shares the
> host net ns) — so `virsh`, `ip link`, `iptables` on the host see them. But named
> netns are bind-mounts in the **pod's mount namespace**, so they show up under
> `kubectl exec … ip netns list`, **not** under `ip netns list` on the host. This
> distinction matters in §9d — read the §15 note before judging a "leak".

---

## 9. Tear it down — `dataRetentionPolicy: delete`

### 9.0 Pre-flight: dry-run the purge (non-destructive, recommended)
Preview exactly what `lightstack cleanup` would remove, deleting nothing:
```bash
kubectl exec -n lightstack deploy/lightstack-rt-lightstack -- /app/lightstack cleanup --config /app/config/lightstack.yaml --dry-run
```
**Pass** = it logs "would purge" lines for the domains/network/storage and exits 0.

### 9a. Flip the release to delete mode
```bash
helm upgrade lightstack-rt charts/lightstack -n lightstack --reuse-values \
  --set dataRetentionPolicy=delete \
  --set teardown.kubectlImage=alpine/k8s:1.31.0      # MUST ship /bin/sh — rancher/kubectl is shell-less → StartError
```
> ⚠️ The quiesce hook runs `/bin/sh -c …`, so `teardown.kubectlImage` **must contain a
> shell**. `rancher/kubectl` is distroless (no `/bin/sh`) — do not use it. `alpine/k8s:1.31.0`
> and `dtzar/helm-kubectl:3.16` both ship `sh`+`kubectl`. k3s pulls the tag at hook time
> (`IfNotPresent`); `sudo k3s ctr images import` it first if you'd rather not pull.

### 9b. Uninstall — it BLOCKS on the pre-delete hooks (SC-003)
Run the uninstall in **T1** and watch the hook Jobs in **T2**:
```bash
# T2 — watch the hook pods appear (single type — `kubectl get -w` can't watch multiple types):
kubectl get pods -n lightstack -w &
# T2 — when the *-teardown-purge pod shows up above, tail its audit log (deleted on success):
#   kubectl logs -n lightstack -f job/lightstack-rt-lightstack-teardown-purge

# T1 — this returns only after the blocking purge SUCCEEDS:
helm uninstall lightstack-rt -n lightstack
```
Order you should see: the `lightstack-rt-lightstack-teardown` SA/Role/RoleBinding appear →
`…-teardown-quiesce` Job scales the Deployment to 0 and deletes PVCs → `…-teardown-purge`
Job runs `lightstack cleanup` (privileged, hostNetwork, host mounts) → only then does the
release get removed.

**Fail-closed check (SC-003):** if the purge exits non-zero, `helm uninstall`
**aborts** and the failed Job is kept for inspection:
```bash
kubectl logs -n lightstack job/lightstack-rt-lightstack-teardown-purge      # the JSON audit trail + the failure
```

### 9c. Verify ZERO cluster residue (SC-001)
```bash
kubectl get deploy,svc,secret,pvc,job,sa,role,rolebinding -n lightstack -l app.kubernetes.io/instance=lightstack-rt
helm list -n lightstack
```
**Pass** = the first command prints nothing, `helm list` no longer shows `lightstack-rt`.

### 9d. Verify ZERO host residue (SC-001 — the real-mode payload)
Compare against the §8 snapshot:
```bash
sudo virsh list --all                                                   # no lightstack domains (destroyed+undefined)
ip -br link show type bridge | grep '^br-'        || echo "OK: no lightstack bridges"
sudo iptables -S | grep -E 'lightstack-(SG|NSI)'         || echo "OK: no lightstack SG/NSI chains"
sudo iptables -t nat -S | grep -E 'lightstack|169\.254\.169\.254|MASQUERADE' || echo "OK: no lightstack nat rules"
sudo ls /var/lib/lightstack/instances /var/lib/lightstack/volumes /var/lib/lightstack/images \
        /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp 2>&1                   # all "No such file" (RemoveAll'd)
```
**Pass** = domains gone, no `br-*`, no `lightstack-*` chains, no Lightstack NAT rules, the
`/var/lib/lightstack` trees removed. (`lightstack cleanup` `RemoveAll`s the dirs, so they're absent,
not just empty.)

> **Named netns:** `ip netns list` **on the host** was already empty in §8 (they
> lived in the pod). After quiesce terminated the server pod, the pod-scoped netns
> were reclaimed with it. So the host-level check is "still empty" — expected. The
> open question (cloud-storage#472) is whether the *purge pod's* `ip netns del`
> reaches anything; in this topology it's a no-op (absent=success). **If you see any
> leftover `light-ns-*`/`qrouter-*` on the host, or any `br-*`/`lightstack-*` survive,
> capture §15's diagnostics and report back** — that's a chart-mount fix, not a
> code bug.

### 9e. Verify the DATABASE is UNTOUCHED (operator-owned — NOT reset)
```bash
PGPASSWORD=secret psql -h 127.0.0.1 -U lightstack -d lightstack -c "select count(*) from instances; select count(*) from networks;"
```
**Pass** = the rows are **still there**. Teardown reads the DB read-only to enumerate
and never resets it (FR-013, the dropped-DB-reset scope change). Getting a *resource*-
pristine redeploy is therefore the operator's DB responsibility — see §12.

---

## 10. `retain` (default) runs NO teardown hooks (SC-004 / SC-006)
> This is a **stub-mode** install (no `realMode` flags) → the pod is **not** `hostNetwork`,
> so `127.0.0.1` is the pod, not the host, and `postgres://…@127.0.0.1` is unreachable
> (CrashLoop). Point the DB at the **node IP** where the host's postgres is published.
> (§6 used `127.0.0.1` only because real-mode hostNetwork makes the pod's localhost the
> host.) The pod's health is incidental here — the test is only that `uninstall` runs no
> teardown hooks, which render solely under `delete`.
```bash
NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
helm install lightstack-ret charts/lightstack -n lightstack --create-namespace \
  --set image.repository=lightstack --set image.tag=dev --set image.pullPolicy=Never \
  --set database.url="postgres://lightstack:secret@${NODE_IP}:5432/lightstack?sslmode=disable" \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" --set secrets.adminPassword='Teardown-Admin-1'
kubectl -n lightstack rollout status deploy/lightstack-ret-lightstack --timeout=120s
helm uninstall lightstack-ret -n lightstack
sleep 3
kubectl get jobs -n lightstack | grep teardown && echo "FAIL: hooks ran under retain" || echo "PASS: retain ran NO teardown hooks (SC-004)"
```
**Pass** = no `…-teardown-*` Jobs ever appeared; host state + DB untouched (SC-006).

---

## 11. Idempotency (SC-005)
Re-run the purge on already-clean state — must succeed:
```bash
# Either via the dry-run/real exec against a fresh real-mode install with nothing created,
# or simplest: install+immediately tear down with no topology built. Expect exit 0,
# every class reported "absent".
```
Unit-level idempotency (not-found ⇒ success) is covered in `internal/teardown`; this
is the on-cluster confirmation that a retry after a partial run doesn't error.

---

## 12. Immediate redeploy (SC-002)
`--create-namespace` is required: §9's teardown deleted the chart-owned namespace
(`psa-namespace.yaml`), so it must be recreated. (Real mode → hostNetwork, so `127.0.0.1`
in the DB URL is correct here.)
```bash
helm install lightstack-rt charts/lightstack -n lightstack --create-namespace \
  --set image.repository=lightstack --set image.tag=dev --set image.pullPolicy=Never \
  --set database.url='postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" --set secrets.adminPassword='Teardown-Admin-1' \
  --set nova.libvirtMode=real --set neutron.networkingMode=iptables \
  --set neutron.securityGroupMode=iptables --set cinder.storageMode=local --set glance.storageMode=local \
  --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true \
  --set podSecurity.applyPrivilegedNamespaceLabel=true
kubectl -n lightstack rollout status deploy/lightstack-rt-lightstack --timeout=300s
helm test lightstack-rt -n lightstack && echo "PASS: immediate redeploy healthy, no collisions (SC-002)"
```
**Pass** = the redeploy comes up healthy with no naming/port collisions.

> **Resource pristineness vs. health.** Because teardown left the DB intact, the
> redeployed control plane still *lists* the old VMs/networks while the host disks
> and domains are gone — `openstack server list` shows stale ACTIVE rows the
> reconciler can't fully rebuild. That's the documented consequence of "DB is
> operator-owned". For a **truly pristine** redeploy, reset the DB yourself before
> reinstalling — e.g.:
> ```bash
> PGPASSWORD=secret psql -h 127.0.0.1 -U lightstack -d lightstack -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
> # Lightstack re-migrates the empty schema on the next pod start.
> ```

---

## 13. Cleanup / full reset between runs
```bash
helm uninstall lightstack-rt lightstack-ret -n lightstack --no-hooks 2>/dev/null || true   # remove releases, skip purge
kubectl delete ns lightstack --ignore-not-found
# then the SPEC-009 §1 host reset (VMs, bridges, netns, iptables, /var/lib/lightstack/*, lightstack-postgres)
# To wipe the cluster entirely:  /usr/local/bin/k3s-uninstall.sh
```

---

## 14. The `--no-hooks` escape hatch (C5)
When the node is gone or the purge can't complete and you must remove the release anyway:
```bash
helm uninstall lightstack-rt -n lightstack --no-hooks    # removes the release, runs NO purge
```
You then own any orphaned host state. (`helm uninstall` takes no value overrides, so
this flag — not a value — is the escape.)

---

## 15. Troubleshooting

- **`teardown.kubectlImage is required …`** → you set `dataRetentionPolicy=delete`
  without `--set teardown.kubectlImage=…`. It's required-when-delete (FR-018).
- **Quiesce Job can't scale the Deployment** → check the `lightstack-rt-lightstack-teardown`
  Role/RoleBinding rendered (weight -20) and the SA is bound; `kubectl logs job/lightstack-rt-lightstack-teardown-quiesce`.
- **Purge Job `CrashLoopBackOff`/exit 2** → config error. `kubectl logs job/lightstack-rt-lightstack-teardown-purge`;
  the config Secret must still exist when the purge runs (it's a release object — Helm
  removes it only *after* the hooks succeed, so it's present during the purge).
- **Purge exits 1 (fail-closed) → uninstall aborts** → read the JSON audit log; it
  names the failing class (compute/network/storage). Fix the host condition and
  re-run `helm uninstall` (idempotent), or use `--no-hooks` (§14).
- **DB unreachable at teardown** → not fatal (C9): the purge ignores it and falls back
  to host-only cleanup (storage trees + a netns prefix scan). Expect a warning, exit 0
  if the host classes succeed.
- **⚠️ Host network state survived the purge** (leftover `br-*`, `lightstack-*` chains, or
  `light-ns-*`/`qrouter-*` netns visible on the host after §9d) → this is the
  **known open risk** (cloud-storage#472 "real-mode host-purge reachability"). The
  purge pod is a *separate mount namespace* from the terminated server pod; named
  netns it created are pod-scoped, and the chart does not bind-mount `/run/netns`.
  Capture and report back:
  ```bash
  kubectl get pod -n lightstack -o yaml | grep -A3 -iE 'hostNetwork|mountPropagation|/run/netns'
  sudo ip netns list ; ip -br link show type bridge ; sudo iptables -S | grep lightstack-
  # plus the full purge Job log
  ```
  Likely fix is a chart change (bind-mount `/run/netns` with `mountPropagation:
  Bidirectional` on both the server pod and the purge Job, or run the purge in the
  host mount namespace), **not** an `lightstack cleanup` code change — the command already
  removes everything it can reach.
- **VM boots to `ERROR`; Lightstack log shows `Could not open backing image '/var/lib/lightstack/images/image-*.raw'`** →
  Glance stored the base under its `~/.lightstack` fallback because `/var/lib/lightstack/images` didn't
  exist when the pod started (chart `hostPath` is `DirectoryOrCreate` on `/var/lib/lightstack`
  only). Fix: `sudo mkdir -p /var/lib/lightstack/{images,volumes,instances,cloud-init,dhcp}`,
  `kubectl -n lightstack rollout restart deploy/lightstack-rt-lightstack`, then delete + re-upload the image and
  recreate the VM. Confirm the base landed right:
  `kubectl exec -n lightstack deploy/lightstack-rt-lightstack -- ls /var/lib/lightstack/images`. Proper fix = a chart
  initContainer pre-creating the subdirs (or the binary `MkdirAll`-ing its store) — see §6.
- **Reset everything** → §13, or reboot the host then redo the three persistent items
  (undefine VMs, `rm -rf /var/lib/lightstack/*`, recreate Postgres).
