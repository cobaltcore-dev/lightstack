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
sudo mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp /var/lib/lightstack/console-tokens /var/lib/lightstack/console-sockets

echo "--- database (recreated if its console table predates the domain ID) ---"
docker exec lightstack-postgres psql -U lightstack -d lightstack -Atc "SELECT 1 FROM information_schema.tables WHERE table_name='console_auth_tokens' AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='console_auth_tokens' AND column_name='domain_id')" 2>/dev/null | grep -q 1 && make db-down
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

- all seven storage subdirectories created (silent on success)
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
Placement must be among them, at `:8778` (R15).

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
mounts `console-tokens` itself as a `DirectoryOrCreate` hostPath, so whichever pod starts
first creates it, `TokenFile` re-reads it on every lookup, and no token can exist before
`console-tokens` does, because issuing one fails closed without it. `console-sockets` works
the same way, and Lightstack makes it QEMU's at startup without replacing it, so a proxy
that mounted it first keeps seeing the same directory. The proxy mounts nothing else of the
storage root (R1). The list mirrors `pkg/paths`.

What the proxy's probe cannot tell you is whether it can **read** the tokens. It GETs
`/vnc_lite.html`, which answers whether or not the token directory is readable, so a UID or
mode mismatch leaves the pod Ready while every lookup fails. R2 and R3 are the checks that
catch that.

**The database is recreated when its console table predates the domain ID.** Migration
`065` was changed in place on this branch, twice: tokens first named a socket path instead
of a host and port, and now also record the libvirt domain ID of the run they were issued
for. A database that ran an earlier `065` keeps the old columns, and every console request
fails on the insert. Nothing migrates it, by design: no build with the old table was ever
released. If you ran this runbook before on an earlier build of this branch, run
**Teardown** first instead: guests created then have their console on a TCP port or in
libvirt's per-domain directory, both of which the new build refuses (R1).

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

### R1 — The guest console is a unix socket in `console-sockets`, with no TCP port (FR-017)

```bash
VM=$(openstack server show vm1 -f value -c id)
echo $VM
sudo virsh dumpxml $VM | grep -A2 "<graphics"
```

**Desired outcome**: `socket='/var/lib/lightstack/console-sockets/<vm1's id>.sock'` on
the `<graphics>` element, with the id printed above, a `<listen type='socket' socket='…'/>`
child with the same path, and `sharePolicy='force-shared'` — and **no** `port=` and no
`listen='…'` address anywhere in the element. The path is the instance's own and does not
change when the guest restarts (R7).

```bash
sudo ss -ltnp | grep -c qemu
sudo ss -xlp | grep console-sockets
```

**Desired outcome**: `0` TCP listeners owned by qemu, then one listening unix socket
at the path above, held by qemu. That socket also proves AppArmor let QEMU create it:
libvirt adds an explicit socket path to that guest's profile, and only that guest's.

If qemu holds a TCP port, or its socket is under `/var/lib/libvirt/qemu/`, the guest was
defined by an older build, and its console request answers 500 `console misconfigured on
this host`. Delete and recreate it (`docs/UPGRADING.md` explains the in-place fix).

If the guest did not start at all, Lightstack may be refusing starts: it does so while it
cannot make the directory QEMU's, and logs why at startup:

```bash
kubectl -n lightstack logs deploy/lightstack-lightstack | grep 'console socket directory'
```

**Desired outcome**: nothing. A line means every guest start is refused until the cause
it names is fixed and Lightstack restarted.

```bash
sudo stat -c '%U:%G %a %F' /var/lib/lightstack/console-sockets /var/lib/lightstack/console-sockets/$VM.sock
```

**Desired outcome**: `libvirt-qemu:kvm 750 directory`, then `libvirt-qemu:kvm 775
socket`. Lightstack set the directory's owner at startup to the user libvirt reports for
QEMU (`sudo virsh capabilities | grep -A3 '<model>dac'`). QEMU created the socket. That
is why the console proxy keeps `DAC_OVERRIDE`: root with every capability dropped is
just "other" there, and `connect()` needs write permission on the socket.

The proxy reaches that directory and the tokens, and nothing else:

```bash
kubectl -n lightstack get deploy lightstack-lightstack-console-proxy -o jsonpath='{range .spec.template.spec.volumes[*]}{.hostPath.path}{"\n"}{end}'
kubectl -n lightstack exec deploy/lightstack-lightstack-console-proxy -- ls /var/lib/lightstack
kubectl -n lightstack exec deploy/lightstack-lightstack-console-proxy -- ls /var/lib/libvirt/qemu
```

**Desired outcome**: exactly two volumes, `/var/lib/lightstack/console-tokens` and
`/var/lib/lightstack/console-sockets`. Inside the proxy, `/var/lib/lightstack` holds only
those two, and `/var/lib/libvirt/qemu` does not exist (`No such file or directory`). That
is the point of the move: libvirt's per-domain directories hold every guest's QMP monitor
socket and key material, and the storage root holds every tenant's disks. With
`DAC_OVERRIDE`, anything mounted is readable.

A real-mode `state_path` must leave room for the socket path. The longest one allowed is
50 characters, because `<state_path>/console-sockets/<uuid>.sock` must fit QEMU's 108-byte
limit. Check the refusal against the image under test, which needs nothing running:

```bash
printf 'state_path: /var/lib/lightstack/a-state-root-of-fifty-one-chars\nnova:\n  libvirt_mode: real\n' > /tmp/spec021-state-path.yaml
docker run --rm -v /tmp/spec021-state-path.yaml:/tmp/c.yaml:ro docker.io/library/lightstack:spec021 /app/lightstack --config /tmp/c.yaml
rm -f /tmp/spec021-state-path.yaml
```

**Desired outcome**: `Failed to load config: state_path "…" is 51 characters; real mode
allows at most 50, …`, before any connection is attempted. The chart passes
`realMode.hostStoragePath` as `state_path`, so a longer one leaves the Lightstack pod
failing at startup with the same message.

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

**Desired outcome**: one row, showing the instance, the socket path from R1, the domain
ID of the guest's current run (`sudo virsh domid $VM` prints the same number) and a
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

`connecting to unix socket: /var/lib/lightstack/console-sockets/<id>.sock` means the
proxy resolved the token. `Token '…' not found` means the projection entry is missing,
stale, or unreadable. After a `connecting to` line:

- `Failed to connect … Permission denied` — the proxy lost `DAC_OVERRIDE`; check
  `consoleProxy.containerSecurityContext`.
- `Failed to connect … No such file or directory` — the guest is not running (QEMU
  removes its socket on shutdown), or the proxy's `console-sockets` is not the host
  directory QEMU writes to (the mount check in R1).
- `Failed to connect … Connection refused` — a socket left behind by a QEMU that
  crashed. The stale-token sweep revokes the link within one interval, and the next
  start replaces the socket.

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

### R7 — A reboot revokes the link; a restart keeps the path and changes the domain ID (SC-006, FR-004, FR-014a)

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
is the reboot's revoke. A soft reboot keeps the guest's QEMU process, and with it the
domain ID and the socket, so the stale-token sweep — which drops a token only when its
guest serves another socket, runs under another domain ID, or is not running — had no
reason to remove it either.

A hard reboot is a power cycle, not a request to the guest:

```bash
VM=$(openstack server show vm1 -f value -c id)
sudo virsh domid $VM
openstack server reboot --hard vm1
sudo virsh domid $VM
sudo virsh dumpxml $VM | grep -o "socket='[^']*'" | head -1
openstack console url show vm1
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
```

**Desired outcome**: two **different** domain IDs — the domain was destroyed and started
again, so QEMU is a new process, which a hung guest cannot ignore — while the socket stays
at `/var/lib/lightstack/console-sockets/<vm1's id>.sock`. The new URL connects on the
first attempt, and the listing shows its row with the second ID. A soft reboot keeps the ID.

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

A stop, a console request while stopped, and a start:

```bash
VM=$(openstack server show vm1 -f value -c id)
sudo virsh domid $VM
openstack server stop vm1
for i in $(seq 1 60); do sudo virsh domstate $VM | grep -q 'shut off' && break; sleep 1; done
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
openstack console url show vm1
openstack server start vm1
for i in $(seq 1 60); do sudo virsh domstate $VM | grep -q running && break; sleep 1; done
sudo virsh domid $VM
sudo virsh dumpxml $VM | grep -o "socket='[^']*'" | head -1
openstack console url show vm1
```

**Desired outcome**: after the stop, no vm1 row, and the console request is refused with
`console not available: the instance is not running` (HTTP 409). The stopped domain's
definition still names the socket, because the path is fixed, so that refusal shows the
readback asks libvirt whether the guest runs instead of trusting the XML. After the start, a
**different** domain ID, the same socket path, and a new URL that connects on the first
attempt. Nothing may be cached.

The same restart behind Lightstack's back reaches no API, so only the domain ID can give
it away:

```bash
openstack console url show vm1 -f value -c url
sudo virsh destroy $VM
sudo virsh start $VM
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
sudo virsh domid $VM
sleep 40
kubectl -n lightstack exec deploy/lightstack-lightstack -- /app/lightstack console-tokens list
kubectl -n lightstack logs deploy/lightstack-lightstack --since=2m | grep 'no longer serves'
```

**Desired outcome**: the first listing, taken with the guest already running again, still
shows the new link, under the domain ID it was issued for, and `virsh domid` prints another
one. After one reconcile interval (30s by default, plus slack) the row is gone, the log
shows `console tokens revoked: their guest no longer serves that console`, and opening the
link fails. The guest was running and the socket path never changed, so the ID is what the
sweep went by. If the first listing already has no vm1 row, a sweep fell in the second
between the destroy and the start and revoked the link for the stop; run the block again.

### R8 — The security probe (SC-005)

```bash
LIGHTSTACK_MODE=real ./test/console_access_test.sh
```

**Desired outcome**: `failed: 0`. Covers reuse-within-window, invented tokens,
cross-instance tokens, revocation on stop and delete, and — steps 8 and 9, which need
`sudo virsh` — a guest stopped, or restarted, behind Lightstack's back losing its links
within one reconcile interval. Step 9 is R7's last check, automated.

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

The same log shows cirros reading its metadata. Its EC2 datasource accepts an
instance-id only once it starts with `i-`, and used to retry 20 times, two seconds apart,
against the bare UUID:

```bash
openstack console log show vm1 | grep -E 'successful after|failed [0-9]+/20'
VM=$(openstack server show vm1 -f value -c id)
echo "i-$(echo $VM | tr -d - | cut -c1-17)"
curl -s -w '\n' -H "X-Instance-ID: $VM" http://127.0.0.1:8775/latest/meta-data/instance-id
```

**Desired outcome**: one `successful after N/20 tries: … iid=i-…` line, and no line
ending in `iid had '…'` — that was the old failure, the guest turning down the bare UUID on
every try. The two ids below it match: `i-` and the first 17 hex digits of vm1's UUID, as
the metadata service reports it over loopback.

A few `failed N/20: … request failed` lines before the success are the guest's own. cirros
0.6's DHCP client goes to the background while `eth0` waits for carrier, so the datasource
starts before the guest has an address, and those requests never reach Lightstack: after a
fresh boot the pod log shows only `200`s for `/2009-04-04/meta-data/instance-id`, and no
`404`.

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
the socket and domain ID its guest has now. R8's step 8 checks this with `virsh destroy`;
this is the real thing.

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

### R14 — The metadata service answers every caller as itself

A caller used to be able to choose which instance it was: by naming it in `X-Instance-ID`,
or by forwarding for one of its fixed IPs in `X-Forwarded-For`. Both headers passed
through the guest DNAT untouched, so any guest could read any instance's metadata and
user-data. Asked from the node's own address, which is no guest and not loopback, both
must now be ignored:

```bash
VM=$(openstack server show vm1 -f value -c id)
VM1_IP=$(openstack server show vm1 -f json | jq -r '.addresses[][0]')
curl -s -o /dev/null -w 'named by header -> %{http_code}\n' -H "X-Instance-ID: $VM" "http://$NODE_IP:8775/openstack/latest/meta_data.json"
curl -s -o /dev/null -w 'forwarded for vm1 -> %{http_code}\n' -H "X-Forwarded-For: $VM1_IP" "http://$NODE_IP:8775/openstack/latest/meta_data.json"
curl -s -o /dev/null -w 'claiming loopback -> %{http_code}\n' -H "X-Instance-ID: $VM" -H "X-Forwarded-For: 127.0.0.1" "http://$NODE_IP:8775/openstack/latest/meta_data.json"
curl -s -H "X-Instance-ID: $VM" http://127.0.0.1:8775/openstack/latest/meta_data.json | jq -r .uuid
echo "$VM"
```

**Desired outcome**: `404` three times; before the fix, each returned vm1's metadata.
Then vm1's UUID over loopback, the same as the `echo` below it: tests that run on the node
keep their way to name an instance.

Then the real path, from inside a guest. Print what vm1 should see, then three commands
for vm1's guest with vm2's id and address filled in:

```bash
echo "i-$(echo $VM | tr -d - | cut -c1-17) $VM1_IP"
VM2=$(openstack server show vm2 -f value -c id)
VM2_IP=$(openstack server show vm2 -f json | jq -r '.addresses[][0]')
printf '%s\n' "curl -s -w '\n' http://169.254.169.254/latest/meta-data/instance-id" "curl -s -w '\n' -H 'X-Instance-ID: $VM2' http://169.254.169.254/latest/meta-data/instance-id" "curl -s -w '\n' -H 'X-Forwarded-For: $VM2_IP' http://169.254.169.254/latest/meta-data/local-ipv4"
```

Open **vm1's** console, log in as `cirros` / `gocubsgo`, and type the three printed
commands. vm2 need not be running — it has been off since R12 — since only its id and
address are used.

**Desired outcome**: vm1's own `i-…` twice, then vm1's own address — the first line
printed above, never vm2's.

The values must be vm2's real ones. A believed `X-Instance-ID` shows with any value, since
the instance-id answer is derived from it, but a forwarding header that is not an IP
address is skipped even by a server that trusts the header — so a placeholder in the third
command would leave it unable to fail.

The same goes for what the logs record. s3tokens logs every rejection with the caller's
address:

```bash
curl -s -o /dev/null -w 's3tokens -> %{http_code}\n' -X POST "http://$NODE_IP:35357/v3/s3tokens" -H 'X-Forwarded-For: 203.0.113.9' -H 'Content-Type: application/json' -d '{}'
kubectl -n lightstack logs deploy/lightstack-lightstack --since=2m | grep 'signature validation denied' | tail -1
```

**Desired outcome**: `401`, and the log line's `client_ip` is the node's address, never
`203.0.113.9`.

### R15 — Placement is in the catalog

```bash
openstack catalog list -f value -c Name -c Endpoints | grep placement
```

**Desired outcome**: `placement`, with one public endpoint at `http://<NODE_IP>:8778`.

In Horizon, logged in as `admin`, open **Admin → Compute → Hypervisors** and its third
tab, **Resource Provider**. The page opens on the **Hypervisor** tab, whose
`lightstack-node-1` row is Nova's hypervisor, not a resource provider.

**Desired outcome**: an empty table, not *"Unable to retrieve providers information."*
Lightstack's placement reports no resource providers — on a single node it schedules
nothing — so Horizon has nothing to list.

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

Uninstall the lightstack release **before** removing k3s. Under
`dataRetentionPolicy: delete` its pre-delete hooks purge the node, including
`console-tokens` and `console-sockets`, and they need the cluster. This runbook keeps the
default, `retain`, so uninstalling purges nothing and the `rm -rf` above does it. Then
remove k3s itself:

```bash
sudo /usr/local/bin/k3s-uninstall.sh
```

**Desired outcome**: `k3s` is gone. This also deletes the imported image and the
cluster's state; `~/.kube/k3s.yaml` is left behind, so remove it if you want a
completely clean host.
