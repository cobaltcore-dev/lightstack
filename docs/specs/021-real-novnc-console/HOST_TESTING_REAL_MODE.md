# HOST_TESTING — SPEC-021 real mode (a real console / noVNC)

**Feature**: `021-real-novnc-console` · **Branch**: `feature/534` · **Mode**: real (k3s, libvirt/KVM)

This is where F1 actually closes: a live guest framebuffer in the Horizon Console
tab, reachable only through a token that can be revoked.

Reuses the SPEC-018 real harness — Lightstack on k3s via `charts/lightstack` in real mode,
Horizon via `charts/horizon`, Ceph/RGW via Rook. **Note the chart moved**: SPEC-018's
runbooks still say `deployments/horizon-chart`; it is now `charts/horizon`. Those
runbooks belong to another feature and were deliberately left unedited.

> **Every shell block runs from the repository root, on this feature's branch.** No
> block changes directory or checks out anything, so paths like `charts/lightstack` and
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
docker build -f build/package/Dockerfile -t docker.io/library/lightstack:spec021 .
rm -f /tmp/lightstack-spec021.tar
docker save docker.io/library/lightstack:spec021 -o /tmp/lightstack-spec021.tar
sudo k3s ctr images import /tmp/lightstack-spec021.tar
sudo k3s ctr images ls | grep lightstack:spec021
```

**Desired outcome**, reading down the output: the k3s node is `Ready`, `virsh`
succeeds (an empty list is fine), and `docker.io/library/lightstack:spec021` is listed in
the k3s image store. Keep `KUBECONFIG` exported — every later `kubectl` and `helm`
call needs it.

### Why those particular lines

**k3s must run as real root.** The default `get.k3s.io` systemd install is; a
rootless container runtime cannot give the pod host netns or root-owned
`/var/lib/lightstack`.

**Building the image is not optional.** Pulling a published `ghcr.io/lightstack/lightstack` would
verify a binary that predates SPEC-021, so the whole run would prove nothing about
this feature. `pullPolicy=Never` in Setup pins the deploy to this local build.

**`rm -f` before `docker save`** is not tidiness — on a re-run, a leftover tar from
an earlier build gets imported instead of the code you just compiled, and the run
silently verifies the wrong binary.

**Tag it fully qualified.** Podman prefixes an unqualified local build with
`localhost/`, so `-t lightstack:spec021` lands in containerd as `localhost/lightstack:spec021`
while the chart's `image.repository=lightstack` resolves to `docker.io/library/lightstack:spec021`
— a mismatch that surfaces as `ImagePullBackOff`, with no useful message.

---

## Setup

One block, one paste — storage dirs, database, both charts, CLI, image and guest.
Idempotent: every step is guarded, so a re-run after a partial failure is safe.

```bash
export KUBECONFIG=~/.kube/k3s.yaml
export NODE_IP=$(hostname -I | awk '{print $1}')

echo "--- node-local storage ---"
sudo mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp /var/lib/lightstack/console-tokens

echo "--- database (recreated if it predates the socket console) ---"
docker exec lightstack-postgres psql -U lightstack -d lightstack -Atc "SELECT 1 FROM information_schema.columns WHERE table_name='console_auth_tokens' AND column_name='port'" 2>/dev/null | grep -q 1 && make db-down
docker inspect lightstack-postgres >/dev/null 2>&1 || make db-up

echo "--- lightstack (real mode, all services) ---"
helm upgrade --install lightstack charts/lightstack --namespace lightstack --create-namespace --set image.repository=lightstack --set image.tag=spec021 --set image.pullPolicy=Never --set deploymentMode=single-node --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true --set nova.libvirtMode=real --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables --set glance.storageMode=local --set cinder.storageMode=local --set consoleProxy.enabled=true --set nova.consoleProxyBaseUrl=http://$NODE_IP:30680 --set endpointHost=$NODE_IP --set secrets.jwtSecret=changeme --set secrets.adminPassword=secret --set database.url='postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable'
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s
kubectl -n lightstack rollout status deploy/lightstack-lightstack-console-proxy --timeout=120s

echo "--- service modes (every one must be real) ---"
kubectl -n lightstack get secret lightstack-lightstack-config -o jsonpath='{.data.lightstack\.yaml}' | base64 -d | grep -E "_mode:"

echo "--- horizon ---"
helm upgrade --install horizon charts/horizon --namespace lightstack
kubectl -n lightstack rollout status deploy/horizon-lightstack-horizon --timeout=180s

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
- both lightstack rollouts complete
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
`LIGHTSTACK_ENDPOINT_HOST`, which is what Lightstack advertises in the service catalog. Left
empty, the catalog falls back to the database-seeded endpoints, which say
`localhost` — so Horizon authenticates fine and then dies on the first catalog call
with *"Unable to establish connection to http://localhost:9696/v2.0/extensions"*,
because `localhost` inside the Horizon pod is the Horizon pod.

Use the **node IP**, not the Service DNS name. With `hostNetwork`, Lightstack listens on
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

**Pre-creating the storage subdirectories** is belt and braces. The chart's hostPath is
`DirectoryOrCreate`, which materialises only `/var/lib/lightstack`, and its initContainer
creates the subdirectories inside the **lightstack** pod's startup. The console proxy is a
separate Deployment with no ordering guarantee and may start first — which is harmless: it
mounts the whole storage root rather than the subdirectory, `TokenFile` re-reads it on every
lookup, and no token can exist before `console-tokens` does, because issuing one fails closed
without it. The list mirrors `pkg/paths`.

What the proxy's probe cannot tell you is whether it can **read** the tokens. It GETs
`/vnc_lite.html`, which answers whether or not the token directory is readable, so a UID or
mode mismatch leaves the pod Ready while every lookup fails. R2 and R3 are the checks that
catch that.

**The database is recreated when it predates the socket console.** Migration `065`
was changed in place on this branch — tokens now name a socket path instead of a
host and port — so a database that ran the earlier `065` keeps the old columns, and
every console request fails on the insert. Nothing migrates it, by design: no build
with the old table was ever released. If you ran this runbook before on an earlier
build of this branch, run **Teardown** first instead: guests created then still
have a TCP console, which the new build refuses (R1).

**The database is addressed by IP, not Service name.** The lightstack pod runs with
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
`sudo k3s ctr images ls | grep lightstack:spec021`.

**`lightstack.host` stays at its default** `lightstack-lightstack`, the Service name the lightstack chart renders
for release `lightstack` (`{release}-{chart}`). Horizon resolves it through Kubernetes DNS,
server-side, so no host `/etc/hosts` entry is involved.

If a rollout times out, read the pod log before anything else — Lightstack exits early and
loudly on a database it cannot reach:

```bash
kubectl -n lightstack logs -l app.kubernetes.io/name=lightstack --tail=20
```

---

## Test steps

### R1 — The guest console is a per-start unix socket, with no TCP port (FR-017)

```bash
VM=$(openstack server show vm1 -f value -c id)
sudo virsh dumpxml $VM | grep -A2 "<graphics"
```

**Desired outcome**: `socket='/var/lib/libvirt/qemu/domain-<id>-instance-<8 hex>/vnc.sock'`
on the `<graphics>` element, a `<listen type='socket' socket='…'/>` child with the
same path, and `sharePolicy='force-shared'` — and **no** `port=` and no
`listen='…'` address anywhere in the element. A `<listen type='socket'/>` without a
path means the readback would be reading the inactive definition.

```bash
sudo ss -ltnp | grep -c qemu
sudo ss -xlp | grep vnc.sock
```

**Desired outcome**: `0` TCP listeners owned by qemu, then one listening unix socket
at the path above. If qemu holds a TCP port, the guest was defined by an older
build — delete and recreate it (`docs/UPGRADING.md` explains the in-place fix).

```bash
sudo ls -la "$(dirname "$(sudo virsh dumpxml $VM | grep -o "socket='[^']*'" | head -1 | cut -d"'" -f2)")"
```

**Desired outcome**: the directory belongs to `libvirt-qemu` (group `kvm`) and is
not world-accessible, and `vnc.sock` is a socket (`s` in the mode). That is why
the console proxy keeps `DAC_OVERRIDE`: root with every capability dropped is just
"other" there, and `connect()` needs write permission on the socket.

### R2 — A console URL is issued, and it is real (SC-001, SC-003)

```bash
openstack console url show vm1
```

**Desired outcome**: a URL of the form
`http://<NODE_IP>:30680/vnc_lite.html?path=%3Ftoken%3D<32 hex chars>`.
It must carry your configured origin — **not** `localhost`, which is what F1
returned — and must not name `vnc_auto.html`, which does not exist in noVNC 1.6.0.

The binary is at `/app/lightstack` in the image and is not on `$PATH`:

```bash
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
```

**Desired outcome**: one row, showing the instance, the socket path from R1 and a
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
kubectl -n lightstack logs deploy/lightstack-lightstack-console-proxy | tail -20
```

`connecting to unix socket: /var/lib/libvirt/qemu/domain-…/vnc.sock` means the proxy
resolved the token. `Token '…' not found` means the projection entry is missing,
stale, or unreadable. After a `connecting to` line:

- `Failed to connect … Permission denied` — the proxy lost `DAC_OVERRIDE`; check
  `consoleProxy.containerSecurityContext`.
- `Failed to connect … No such file or directory` — the guest restarted since the
  link was issued (open the console again), or libvirt keeps domain state somewhere
  other than `consoleProxy.libvirtDomainDir`.

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

### R7 — A reboot revokes the link; a stop/start moves the socket (SC-006, FR-004)

```bash
openstack console url show vm1 -f value -c url
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
openstack server reboot vm1
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
sleep 20
openstack console url show vm1
```

**Desired outcome**: the first listing shows the link just printed; the second,
taken right after the reboot, shows **no** vm1 row. Opening the first link now fails,
and the new URL connects on the first attempt.

The link must be issued here, not reused from R2 or R4: those have expired by now, so
their absence proves nothing. A fresh one is minutes inside its TTL, so a missing row
is the reboot's revoke. And a reboot keeps the guest's qemu process, and with it the
socket, so the stale-token sweep — which only drops tokens whose socket changed — had
no reason to remove it either.

A hard reboot is a power cycle, not a request to the guest:

```bash
VM=$(openstack server show vm1 -f value -c id)
sudo virsh dumpxml $VM | grep -o "socket='[^']*'" | head -1
openstack server reboot --hard vm1
sudo virsh dumpxml $VM | grep -o "socket='[^']*'" | head -1
openstack console url show vm1
```

**Desired outcome**: the two socket lines name **different** `domain-<id>-…`
directories — the domain was destroyed and started again, so qemu is a new process,
which a hung guest cannot ignore — and the new URL connects on the first attempt. The
soft reboot above keeps the same directory.

A reboot without a valid `type` is refused, and refused before the instance is looked up,
so it cannot revoke a live link on its way to the 400:

```bash
openstack console url show vm1 -f value -c url
TOKEN=$(openstack token issue -f value -c id)
curl -s -o /dev/null -w 'no type -> %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$VM/action" -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"reboot":{}}'
curl -s -o /dev/null -w 'unknown type -> %{http_code}\n' -X POST "http://$NODE_IP:8774/v2.1/servers/$VM/action" -H "X-Auth-Token: $TOKEN" -H 'Content-Type: application/json' -d '{"reboot":{"type":"FAST"}}'
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
```

**Desired outcome**: `400` twice, and the listing still shows the vm1 link printed first.

```bash
VM=$(openstack server show vm1 -f value -c id)
sudo virsh dumpxml $VM | grep -o "socket='[^']*'" | head -1
openstack server stop vm1
for i in $(seq 1 60); do sudo virsh domstate $VM | grep -q 'shut off' && break; sleep 1; done
openstack server start vm1
for i in $(seq 1 60); do sudo virsh domstate $VM | grep -q running && break; sleep 1; done
sudo virsh dumpxml $VM | grep -o "socket='[^']*'" | head -1
openstack console url show vm1
```

**Desired outcome**: the two socket lines name **different** `domain-<id>-…`
directories — libvirt gives every start a new one, so a link issued before the stop
names a path that no longer exists — and the new URL connects on the first attempt.
Nothing may be cached.

### R8 — The security probe (SC-005)

```bash
LIGHTSTACK_MODE=real ./test/console_access_test.sh
```

**Desired outcome**: `failed: 0`. Covers reuse-within-window, invented tokens,
cross-instance tokens, revocation on stop and delete, and — step 8, which needs
`sudo virsh` — a guest stopped behind Lightstack's back losing its links within one
reconcile interval.

Add restart survival (SC-007) explicitly:

```bash
LIGHTSTACK_MODE=real LIGHTSTACK_RESTART_CMD='kubectl -n lightstack rollout restart deploy/lightstack-lightstack && kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s' ./test/console_access_test.sh
```

**Desired outcome**: the pre-restart link still resolves. This is the only reason
tokens are persisted rather than kept in memory.

### R9 — Expiry is actually enforced (SC-004)

Slow but worth running once. The proxy performs **no** expiry check of its own — a
token dies when Lightstack deletes its projection entry — so step 7 asks the
**proxy**, not the API. The API filters on `expires_at` itself and would refuse an
expired token even if the reconciler never reaped one:

```bash
LIGHTSTACK_MODE=real LIGHTSTACK_CHECK_EXPIRY=1 ./test/console_access_test.sh
```

**Desired outcome**: `failed: 0`, and step 7 reports `expired token refused by the
proxy`. It first checks that the proxy resolves the fresh token, so a probe that
cannot reach the proxy fails instead of passing. Effective validity is
`console_token_ttl + console_reconcile_interval` — 10m30s at the defaults.

### R10 — The serial fallback is unregressed (SC-009, FR-026)

```bash
openstack console log show vm1 | tail -5
```

**Desired outcome**: real cirros boot output.

### R11 — Fail-fast when libvirt is unreachable, and recovery when it returns (SC-010, FR-007)

The recovery below depends on how the pod mounts libvirt's socket. Check it first:

```bash
kubectl -n lightstack get deploy lightstack-lightstack -o jsonpath='{.spec.template.spec.volumes[?(@.name=="libvirt-run")].hostPath}'; echo
```

**Desired outcome**: `{"path":"/var/run/libvirt","type":"Directory"}` — the socket's
directory. A mount of the socket file itself would keep the socket the pod started with,
and the one libvirtd creates below would stay out of reach.

```bash
sudo systemctl stop libvirtd.socket libvirtd-ro.socket libvirtd-admin.socket libvirtd.service
time openstack console url show vm1
sudo systemctl start libvirtd.socket libvirtd-ro.socket libvirtd-admin.socket libvirtd.service
sleep 6
openstack console url show vm1
kubectl -n lightstack logs deploy/lightstack-lightstack --since=5m | grep 'libvirt connection'
```

**Desired outcome**: an error in **under 2 seconds**, not a hang. Then, with **no**
pod restart, a console URL again, and the log shows `libvirt connection lost;
reconnecting` followed by `libvirt connection re-established`.

Stop the socket units too. Ubuntu socket-activates libvirtd, so stopping only the
service leaves its sockets listening, and Lightstack's reconnect would start libvirtd
again within a second — the fail-fast check would race it. Guests keep running either
way: qemu does not depend on libvirtd.

Starting the sockets again re-creates the socket file. The pod mounts
`/var/run/libvirt` rather than the socket itself, so it sees the new one, and
Lightstack re-dials every 1–5 seconds: `sleep 6` covers one backoff step.

### R12 — A guest that powers itself off loses its links (the stale-token sweep)

The lifecycle revoke only sees the API. A guest that shuts itself down frees its
console without any call to Lightstack, so the reconciler compares every token with
the socket its guest serves now. R8's step 8 checks this with `virsh destroy`; this is
the real thing.

```bash
openstack console url show vm2
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
```

Open that URL, log in as `cirros` / `gocubsgo`, and run `sudo poweroff` inside the
guest. Then:

```bash
VM2=$(openstack server show vm2 -f value -c id)
for i in $(seq 1 60); do sudo virsh domstate $VM2 | grep -q 'shut off' && break; sleep 1; done
sudo virsh domstate $VM2
sleep 40
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
kubectl -n lightstack logs deploy/lightstack-lightstack --since=5m | grep 'no longer serves'
```

**Desired outcome**: `shut off` first. If it says `running`, the guest never powered
off — redo the poweroff before reading the rest, which would otherwise report a
failure that is not the sweep's. Then vm2's row is gone from the listing, the log
shows `console tokens revoked: their guest no longer serves that console`, and
reloading the old link fails instead of reconnecting. 40 seconds is one reconcile
interval (30s by default) plus slack.

The log line is what proves the sweep: a link issued more than 10 minutes before the
poweroff expires on its own, and its row disappears without it.

### R13 — No proxy, no URL (FR-027a)

Multi-node deploys no console proxy, and Lightstack then refuses consoles instead of
issuing URLs that lead nowhere. On one node the same path is reached by switching the
proxy off:

```bash
helm upgrade lightstack charts/lightstack --namespace lightstack --reuse-values --set consoleProxy.enabled=false
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s
kubectl -n lightstack get deploy lightstack-lightstack-console-proxy
openstack console url show vm1
```

**Desired outcome**: the proxy Deployment is gone (`NotFound`), and the console request
fails with `Unavailable console type novnc: this deployment runs no console proxy`
(HTTP 400) — no URL. The config change alone rolls the lightstack pod, since the chart
checksums its config.

Restore it:

```bash
helm upgrade lightstack charts/lightstack --namespace lightstack --reuse-values --set consoleProxy.enabled=true
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s
kubectl -n lightstack rollout status deploy/lightstack-lightstack-console-proxy --timeout=120s
openstack console url show vm1
```

**Desired outcome**: both rollouts complete and a console URL is issued again.

---

## Teardown

```bash
export KUBECONFIG=~/.kube/k3s.yaml
openstack server delete vm1 vm2
openstack subnet delete spec021-subnet
openstack network delete spec021-net
openstack image delete cirros
rm -f /tmp/cirros.img /tmp/lightstack-spec021.tar
helm -n lightstack uninstall horizon
helm -n lightstack uninstall lightstack
kubectl delete namespace lightstack
make db-down
sudo virsh list --all
sudo ls -la /var/lib/lightstack/console-tokens/
```

**Desired outcome**: no leftover domains, and the console-token directory is empty
— a surviving entry means a token outlived its instance, which is the one direction
this design must never fail in.

Check that *before* removing the node-local state, then drop it:

```bash
sudo rm -rf /var/lib/lightstack
```

Uninstalling the lightstack release runs the chart's pre-delete purge hooks, so do that
**before** removing k3s. Then remove k3s itself:

```bash
sudo /usr/local/bin/k3s-uninstall.sh
```

**Desired outcome**: `k3s` is gone. This also deletes the imported image and the
cluster's state; `~/.kube/k3s.yaml` is left behind, so remove it if you want a
completely clean host.
