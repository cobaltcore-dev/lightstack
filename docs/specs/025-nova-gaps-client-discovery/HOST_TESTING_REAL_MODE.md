# Host testing guide — Nova gaps & client discovery (real mode)

Validates the parts of SPEC-025 (cloud-storage#547) that need a real hypervisor: the stop/start
divergence and its self-heal (B12), a boot that cannot get its network (B13), a floating IP in the
server's address map (B17), and boot-from-new-volume (F16). Single Ubuntu host, k3s, libvirt/KVM,
node-local storage — no Ceph, no multi-node.

Everything else in the feature is already covered without a host: `test/nova_gaps_test.sh` (19 checks)
and the Go suites verify B3, B4, B7, B14, B15 and B16 in stub mode. See `BASELINE.md`.

**Run all commands from the repo root.** Sections 1 and 2 run in **one shell** and carry their exports
forward; only R10 uses a second shell.

## Validates

| Acceptance criterion | Proved by |
|---|---|
| FR-022/FR-023/FR-024a — a stop and a start are persisted | R1 |
| FR-025/FR-025a/FR-025b — an absent guest is re-created from stored state | R2 |
| FR-025c/FR-026b — a self-heal that cannot complete is a conflict, not a fault | R3 |
| B12 root cause (research.md R2) | R4 |
| FR-017/FR-016a — a network the caller cannot use is refused up front | R5 |
| FR-015/FR-016/FR-018/FR-019 — a post-acceptance failure lands in ERROR with a readable fault | R6 |
| FR-024b/FR-024c/FR-024e — drift is corrected by the sweep in either direction, not by reads | R7a, R7b |
| FR-034/FR-035/FR-036/FR-039 — a floating IP appears in `addresses`, tagged | R8 |
| FR-049..FR-054 — a real boot volume of the requested size, delete-on-termination honoured | R9 |
| SC-014 / #548 F15 — the compute contract suite does not hang | R10 |
| FR-014 — the 6.6.0 client can read identity resources | R11 |
| FR-042/FR-042a/FR-043 — a deleted server's history survives, is served, is reclaimed by age | R12 |
| FR-022a — a stop waits for the guest and forces it off after the grace period | R13a |
| FR-022b — a forced stop still answers, inside the server's write deadline | R13e, R1 |
| FR-026c — an action whose end state already holds is a conflict, and the record is corrected | R13b, R13c |

---

## 0. Prerequisites

One paste. It **verifies** the heavy preconditions — a container engine, `helm`, libvirt/KVM — and
installs only lightweight CLI tools plus k3s if absent. Install Docker or Podman and Helm yourself
first if they are missing.

```bash
sudo apt-get update
sudo apt-get install -y curl jq python3-openstackclient libvirt-clients qemu-kvm qemu-utils
sudo systemctl enable --now libvirtd

command -v docker >/dev/null || command -v podman >/dev/null || echo "MISSING: install docker or podman first"
command -v helm >/dev/null || echo "MISSING: install helm first"

command -v k3s >/dev/null || curl -sfL https://get.k3s.io | sudo sh -
sudo systemctl is-active k3s || sudo systemctl start k3s
mkdir -p ~/.kube
sudo k3s kubectl config view --raw | sudo tee ~/.kube/k3s.yaml >/dev/null
sudo chown "$USER" ~/.kube/k3s.yaml
chmod 600 ~/.kube/k3s.yaml
export KUBECONFIG=~/.kube/k3s.yaml

echo "--- versions ---"
docker --version || podman --version
helm version --short
kubectl get nodes
sudo virsh -c qemu:///system list --all
jq --version
qemu-img --version | head -1
go version
which -a openstack
openstack --version
```

**Pass =** no `MISSING:` line, the k3s node is `Ready`, `virsh` succeeds (an empty list is fine), and
every version line prints. Keep `KUBECONFIG` exported — every later `kubectl` and `helm` call needs it.

> **Client version matters here, and differently from SPEC-023's runbook.** That runbook warned that
> Ubuntu's python-openstackclient **6.6.0** cannot run `openstack project show` against Lightstack, and told
> you to install a 10.x client instead. **This feature fixes that** (B16 / FR-014). So a 6.6.0 client
> is now the *reproduction case*, not a blocker: R11 uses it deliberately. If `openstack --version`
> reports 6.x, R11 is a genuine test; if it reports 10.x, R11 degrades to a smoke check and says so.

---

## 1. Setup steps

One block. It builds the image, deploys, and then **gates the fixture half on the deployed mode**: if
`libvirt_mode` or `networking_mode` does not read real, the image, network and guest are never created
and every test step below fails on a missing variable instead of passing against a stub.

That gate is the reason this is safe to paste as one thing. A *failed* deploy was never the hazard — it
cascades loudly, with no token, no image and no server. The hazard is a deploy that **succeeds in the
wrong mode**, because then setup completes perfectly and all thirteen test steps pass without proving
anything about real mode. Enforcing it beats printing it and hoping it is read, which is what a
sixty-line block would otherwise ask of you.

`endpointHost` must be the **node IP**. With `hostNetwork` Lightstack listens on the node, so one value is
reachable from both the pod network and your host CLI; left unset the catalog advertises `localhost`
and every client command fails at catalog lookup before reading a version document.

`NODE_IP` comes from the **default route**, not from `hostname -I | awk '{print $1}'`. That form returns
addresses in kernel interface order and this host has five — `enp1s0`, `mpqemubr0`, `virbr0`,
`flannel.1`, `cni0` — so which one lands first is luck, not routability. **Re-running from the top in
the same shell does not reset the exports**, so a stale `NODE_IP` survives; a wrong one surfaces as
`Failed to discover available identity versions when contacting http://<wrong-ip>:35357/v3`, which reads
like a service fault rather than a bad variable.

```bash
export KUBECONFIG=~/.kube/k3s.yaml
export NODE_IP=$(ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')
echo "node ip: $NODE_IP"

echo "--- node-local storage (the image creates these in-container; the hostPath needs them too) ---"
sudo mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp /var/lib/lightstack/console-tokens

echo "--- database ---"
docker inspect lightstack-postgres >/dev/null 2>&1 || make db-up
sleep 5

echo "--- nothing else may hold the service ports ---"
ss -lntp 2>/dev/null | grep -E ':(35357|8774|9292|9696|8776|8775) ' && echo "PORTS BUSY: stop whatever is listed above" || echo "ports free"
pgrep -x lightstack >/dev/null && echo "WARNING: a host-local Lightstack is running; kill it before deploying" || true

echo "--- build and import ---"
docker build -f build/package/Dockerfile -t docker.io/library/lightstack:spec025 .
rm -f /tmp/lightstack-spec025.tar
docker save docker.io/library/lightstack:spec025 -o /tmp/lightstack-spec025.tar
sudo k3s ctr images import /tmp/lightstack-spec025.tar

echo "--- deploy, real mode, with a SHORT reconcile interval so R7 is observable ---"
helm upgrade --install lightstack charts/lightstack --namespace lightstack --create-namespace --set image.repository=lightstack --set image.tag=spec025 --set image.pullPolicy=Never --set deploymentMode=single-node --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true --set nova.libvirtMode=real --set nova.stateReconcileInterval=10s --set nova.instanceActionRetention=2160h --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables --set glance.storageMode=local --set cinder.storageMode=local --set endpointHost=$NODE_IP --set secrets.jwtSecret=changeme --set secrets.adminPassword=secret --set database.url='postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable'
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s
kubectl -n lightstack get pods

echo "=== the gate: every mode must read real, and the new settings must be present ==="
MODES=$(kubectl -n lightstack get secret lightstack-lightstack-config -o jsonpath='{.data.lightstack\.yaml}' | base64 -d | grep -E "_mode:|state_reconcile_interval|instance_action_retention|shutdown_timeout")
echo "$MODES"
kubectl -n lightstack logs deploy/lightstack-lightstack | grep -i "state reconciler started"

if echo "$MODES" | grep -q 'libvirt_mode: "real"' && echo "$MODES" | grep -q 'networking_mode: "iptables"'; then
  echo "=== authenticate ==="
  export OS_AUTH_URL=http://$NODE_IP:35357/v3
  export OS_PROJECT_NAME=default
  export OS_USERNAME=admin
  export OS_PASSWORD=secret
  export OS_USER_DOMAIN_NAME=Default
  export OS_PROJECT_DOMAIN_NAME=Default
  export OS_IDENTITY_API_VERSION=3
  unset OS_COMPUTE_API_VERSION

  openstack token issue -f value -c id >/dev/null && echo "authenticated"
  export T=$(openstack token issue -f value -c id)

  echo -n "endpoints naming localhost (must be 0): "
  openstack catalog list -f value -c Endpoints | grep -c 'localhost' || true
  echo -n "endpoints naming the node (must be >= 5): "
  openstack catalog list -f value -c Endpoints | grep -c "$NODE_IP" || true

  echo "=== image ==="
  test -s /tmp/cirros.img || curl -sL -o /tmp/cirros.img http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
  openstack image show cirros >/dev/null 2>&1 || openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img cirros >/dev/null
  openstack image show cirros -f value -c name -c status -c size
  export IMG=$(openstack image show cirros -f value -c id)
  export FLV=$(openstack flavor show m1.small -f value -c id)
  echo "--- the raw base must exist on the node: R9 cannot work without it ---"
  sudo ls -la /var/lib/lightstack/images

  echo "=== network and subnet ==="
  openstack network show rt-net >/dev/null 2>&1 || openstack network create rt-net >/dev/null
  openstack subnet show rt-sub >/dev/null 2>&1 || openstack subnet create rt-sub --network rt-net --subnet-range 10.244.10.0/24 >/dev/null
  export NET=$(openstack network show rt-net -f value -c id)

  echo "=== one booted guest ==="
  openstack server create --flavor m1.small --image cirros --network rt-net probe --wait >/dev/null 2>&1 || openstack server create --flavor m1.small --image cirros --network rt-net probe >/dev/null
  sleep 10
  export SRV=$(openstack server show probe -f value -c id)
  export DOMNAME=instance-$(echo "$SRV" | cut -c1-8)
  echo "server: $SRV"
  echo "libvirt domain name: $DOMNAME"
  openstack server show probe -f value -c status -c addresses
  sudo virsh -c qemu:///system list --all
  echo -n "stored domain id (must equal the instance id): "
  docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT libvirt_domain_id FROM instances WHERE id = '$SRV';" | head -1

  echo "=== every variable the test steps rely on ==="
  for v in NODE_IP T IMG FLV NET SRV; do printf '%s=%s\n' "$v" "${(P)v}"; done
else
  echo "STOP: this deployment is not real mode. No fixtures were created — the image, network and"
  echo "      guest are all absent, so every test step will fail on a missing variable rather than"
  echo "      pass against a stub. Fix the --set flags above and re-run this block."
fi
```

**Pass =** `ports free`; the pod `Running` and not `CrashLoopBackOff`; the read-back showing
`libvirt_mode: "real"`, `networking_mode: "iptables"`, `security_group_mode: "iptables"`,
`storage_mode: "local"` twice, `state_reconcile_interval: "10s"`, `instance_action_retention: "2160h"`
and `shutdown_timeout: "30s"`; a startup line reading
`Compute state reconciler started (interval: 10s, action retention: 2160h0m0s)`; `authenticated`; the
localhost endpoint count **`0`** and the node count **`>= 5`**; `cirros` **`active`** with a non-zero
size; an `image-<uuid>.raw` under `/var/lib/lightstack/images`; `probe` **`ACTIVE`** with an address from
`10.244.10.0/24`; a running domain named `instance-<first 8 of $SRV>`; the stored `libvirt_domain_id`
equal to `$SRV`; and the final loop printing **six non-empty values**.

A `STOP:` line means the gate tripped and nothing after it ran. A `queued` image means the upload
failed — metadata exists, data does not, and `image list` shows the name either way. **An empty variable
in that last loop is the thing to stop on**: `$SRV`, `$NET` and `$HS` each produced a confident,
meaningless pass somewhere in the 2026-09-02 run when they were unset.

The stored domain id is worth reading: the domain XML sets `<uuid>` from the instance id, so **`virsh`
commands below address the domain by `$SRV`**. The domain *name* is `instance-<8 chars>` and is not a
name `virsh` will match against the server's OpenStack name.

---

## 2. Test steps

### R1 — Ten stop/start cycles with no divergence (T050, FR-022, FR-023, FR-024a)

Before this feature the real-mode branch called the hypervisor and wrote nothing, so the API reported
`ACTIVE` after every successful stop.

```bash
for i in 1 2 3 4 5 6 7 8 9 10; do
  openstack server stop probe
  sleep 3
  echo "stop $i: api=$(openstack server show probe -f value -c status) power=$(openstack server show probe -f value -c OS-EXT-STS:power_state) virsh=$(sudo virsh -c qemu:///system domstate $SRV 2>/dev/null || echo missing)"
  openstack server start probe
  sleep 3
  echo "start $i: api=$(openstack server show probe -f value -c status) power=$(openstack server show probe -f value -c OS-EXT-STS:power_state) virsh=$(sudo virsh -c qemu:///system domstate $SRV 2>/dev/null || echo missing)"
done
```

**Expect:** ten `stop` lines reading `api=SHUTOFF power=4 virsh=shut off` and ten `start` lines reading
`api=ACTIVE power=1 virsh=running`. Every line must agree with `virsh`; there are three ways this fails
and each names a different defect.

| Observed | What it means |
| --- | --- |
| `api=ACTIVE` on a `stop` line | the action did not persist its result — FR-024a |
| `api=SHUTOFF` with `virsh=running` | the stop reported success over a guest that had not stopped — FR-022a. libvirt's shutdown is a *request*; a stop that returns when the request is accepted is guessing |
| `500 failed to perform os-start` | a state problem answered as a fault, and the record left stale so every later start fails identically — FR-026c |

The third is a *latch*: once the record and the guest disagree, an action refused with a stale record
never clears the disagreement, so iterations 2 onward fail the same way. A run that fails at iteration 2
and then repeats itself is that, not ten independent failures.

### R2 — A guest removed out of band is re-created (T050, FR-025, FR-025a, FR-025b)

```bash
openstack server stop probe
sleep 3
sudo virsh -c qemu:///system undefine "$SRV"
sudo virsh -c qemu:///system list --all
openstack server start probe
echo "start exit: $?"
sleep 3
openstack server show probe -f value -c status
sudo virsh -c qemu:///system list --all
sudo virsh -c qemu:///system dumpxml "$SRV" | grep -E "<vcpu|<memory|<source (file|bridge)"
openstack server event list probe -f value -c "Action" -c "Start Time" | head -3
kubectl -n lightstack logs deploy/lightstack-lightstack | grep -i "self-heal" | tail -2
echo "--- the tenant-visible half: the repair recorded on the action ---"
docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT action, message FROM instance_actions WHERE instance_id = '$SRV' ORDER BY start_time DESC LIMIT 1;"
```

**Expect:** the start succeeds (`exit: 0`), the server reports `ACTIVE`, a domain exists again, and its
XML carries the flavor's vcpu/memory and the instance's disk and bridge — not defaults. The service log
carries `self-heal: the server's guest was absent and has been re-created from stored state`. The most
recent event is a `start`; **no new action name** appears. And the last line reads
`start|guest was absent and was re-created from stored state (domain …)` — the log proves the repair
happened, that row proves a **tenant** can see it happened (FR-025b).

### R3 — A self-heal that cannot complete is a conflict (T050, FR-025c, FR-026b)

The rebuild refuses when the instance's disk is gone: re-creating the overlay would discard the guest's
data, so its absence is a refusal rather than something to paper over.

```bash
openstack server stop probe
sleep 3
sudo virsh -c qemu:///system undefine "$SRV"
sudo mv /var/lib/lightstack/instances/$SRV/disk.qcow2 /tmp/disk-$SRV.qcow2
curl -s -o /dev/null -w 'os-start status: %{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"os-start": null}' http://$NODE_IP:8774/v2.1/servers/$SRV/action
echo -n "status while the disk is missing: "; openstack server show probe -f value -c status
sudo mv /tmp/disk-$SRV.qcow2 /var/lib/lightstack/instances/$SRV/disk.qcow2
echo "--- restore the guest for the steps that follow ---"
openstack server start probe
sleep 5
echo -n "status after the disk is back: "; openstack server show probe -f value -c status
```

**Expect:** `os-start status: 409`; the status while the disk is missing is **not** `ACTIVE` (it is
`ERROR`); and `ACTIVE` again once the disk is back. A `500` is a failure — an absent guest is a state
problem, not an internal fault. A `202` means the rebuild invented a disk.

The restore is in the same block deliberately: it must happen whatever the refusal did, and leaving it
to a second paste is how `probe` gets left in `ERROR` for every step that follows. That it works at all
is worth noting — `os-start` from `ERROR` succeeds because the precondition reads the hypervisor, not
the record.

### R4 — The B12 root-cause diagnostic (T051, research.md R2)

Reading the tree exonerated every obvious suspect: domains are persistent, the stop is
`DomainShutdown` and never destroy/undefine, the XML sets no `<on_poweroff>` override, and only the
delete path and the Helm purge undefine anything. The leading hypothesis is **two libvirtd instances** —
a container socket versus the host's `qemu:///system`.

```bash
echo "--- which libvirt does Lightstack talk to ---"
kubectl -n lightstack get secret lightstack-lightstack-config -o jsonpath='{.data.lightstack\.yaml}' | base64 -d | grep libvirt_uri
kubectl -n lightstack exec deploy/lightstack-lightstack -- ls -la /var/run/libvirt/libvirt-sock
sudo ls -la /var/run/libvirt/libvirt-sock

echo "--- a fresh guest, then stop it and look immediately ---"
openstack server create --flavor m1.small --image cirros diag --network rt-net >/dev/null
sleep 10
export DIAG=$(openstack server show diag -f value -c id)
sudo virsh -c qemu:///system list --all --uuid | grep -q "$DIAG" && echo "IN host libvirtd before stop" || echo "NOT in host libvirtd before stop"
openstack server stop diag
sleep 5
sudo virsh -c qemu:///system list --all --uuid | grep -q "$DIAG" && echo "SURVIVES the stop" || echo "GONE after the stop"
sudo virsh -c qemu:///system domstate "$DIAG" 2>&1
openstack server show diag -f value -c status
```

**Record in `BASELINE.md`:** whether the domain survives the stop as `shut off`, and whether the socket
Lightstack uses is the same inode as the host's. If it survives and the sockets match, the original report came
from a third cause and research.md R2's hypothesis list needs revisiting.

**This step has no pass/fail** — it closes an open question. The fix does not depend on the answer:
persist-on-action, self-heal and the sweep are each correct whatever removed the guest.

**Narrowed, not closed.** An R1 run on 2026-09-01 recorded the domain present and `running` after a stop
that had answered `202` and written `SHUTOFF`. For *that* divergence nothing removed the guest and both
sides were looking at the same `qemu:///system` — so the two-libvirtd hypothesis does not explain it, and
the cause was the missing wait (R13). It says nothing about the ORIGINAL report of a vanished guest,
which is what this step still exists to answer.

### R5 — A network the caller cannot use is refused up front (T064, FR-017, FR-016a)

**The CLI cannot test this, and a run through it proves nothing.** `openstack server create --network`
resolves the network in Neutron *client-side* and fails with openstacksdk's own `No Network found for
<uuid>` before it ever POSTs to Nova. That looks like a pass — non-zero exit, no server — and it would look
identical with the pre-flight check deleted. Go straight at the API.

```bash
export FLV=$(openstack flavor show m1.small -f value -c id)
curl -s -w '\nstatus: %{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"server":{"name":"badnet","flavorRef":"'$FLV'","imageRef":"'$IMG'","networks":[{"uuid":"99999999-9999-9999-9999-999999999999"}]}}' http://$NODE_IP:8774/v2.1/servers
echo "badnet servers: $(openstack server list -f value -c Name | grep -c badnet)"
kubectl -n lightstack logs deploy/lightstack-lightstack | grep "not usable by this project" | tail -1
```

**Expect:** `status: 400`, a body naming the network — `network 99999999-… is not available to this
project: no such network` — and **no** server called `badnet`. The log line
`refusing server create: requested network is not usable by this project` is what confirms
`NetworkUsableBy` was the thing that refused, rather than some earlier validation.

A `202` means the create was accepted and the server will reach `ACTIVE` with no interface at all, which is
the defect. A `500` means the refusal happened but as a fault rather than a bad request.

The refusal deliberately does **not** distinguish "no such network" from "someone else's network":
`NetworkUsableBy` (`internal/neutron/ports.go`) returns the same `no such network` for both, because
telling a caller which one it is confirms the existence of another project's network. An unshared
external network is the one exception, since every project can already see it: a non-administrator is
refused with `403` and upstream's `It is not allowed to create an interface on external network <id>`,
and an administrator may use it.

### R6 — A post-acceptance failure lands in ERROR with a readable fault (T064, FR-015, FR-016, FR-018, FR-019)

A network with **no subnet** makes port allocation fail deterministically: the allocator returns
`no subnet found for network`. This is the half that cannot be refused up front — the create has
already been answered by the time allocation runs.

```bash
openstack network show nosub-net >/dev/null 2>&1 || openstack network create nosub-net >/dev/null
openstack server create --flavor m1.small --image cirros --network nosub-net failboot >/dev/null
echo "create accepted: $?"
sleep 8
echo -n "status: "; openstack server show failboot -f value -c status
echo -n "fault: "; openstack server show failboot -f json | jq -c '.fault'
echo -n "addresses: "; openstack server show failboot -f value -c addresses
echo "--- and it must still be deletable by its owner (FR-019) ---"
openstack server delete failboot
echo "delete exit: $?"
sleep 3
echo "failboot servers: $(openstack server list -f value -c Name | grep -c failboot)"
echo -n "orphaned ports: "; docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT count(*) FROM ports WHERE device_id NOT IN (SELECT id FROM instances);" | head -1
```

**Expect:** the create is accepted, then `failboot` reports **`ERROR`** with a populated `fault.message`
naming the network, and `addresses` empty (`{}`). It must **never** read `ACTIVE` with empty addresses —
that is the exact state B13 describes. Then the delete succeeds, `failboot servers: 0`, and
`orphaned ports: 0`.

The fault message must read `… no subnet found for network <id>` and **stop there**. A trailing
`: no rows in result set` means the pod predates the fix that stopped pgx's own error text reaching a
tenant. Note the orphaned-port count is across the whole shared database, so a non-zero value may belong
to another worktree rather than to `failboot`.

### R7 — Out-of-band drift is corrected by the sweep, not by reads (FR-024b, FR-024c, FR-024e)

Two shapes of drift, because the sweep used to recognise only one. `virsh destroy` **stops** a persistent
domain, it does not remove it — so part (a) leaves a guest that exists and disagrees, and only part (b)
leaves no guest at all. Watch which reason the reconciler logs; they are different code paths.

```bash
openstack server create --flavor m1.small --image cirros drift --network rt-net >/dev/null
sleep 10
export DRIFT=$(openstack server show drift -f value -c id)
test -n "$DRIFT" || echo "STOP: drift was not created"

echo "=== a. a guest that exists and disagrees ==="
echo -n "before: "; openstack server show drift -f value -c status
sudo virsh -c qemu:///system destroy "$DRIFT"
echo -n "domstate (must be 'shut off', i.e. still defined): "; sudo virsh -c qemu:///system domstate "$DRIFT"
echo "immediately after destroy: $(openstack server show drift -f value -c status)"
sleep 15
echo "after one reconcile interval: $(openstack server show drift -f value -c status)"

echo "=== b. a guest that is genuinely gone ==="
openstack server start drift
sleep 8
sudo virsh -c qemu:///system destroy "$DRIFT" 2>/dev/null || true
sudo virsh -c qemu:///system undefine "$DRIFT"
docker exec lightstack-postgres psql -U lightstack -d lightstack -q -c "UPDATE instances SET status = 'ACTIVE', power_state = 1 WHERE id = '$DRIFT';"
echo "immediately after undefine: $(openstack server show drift -f value -c status)"
sleep 15
echo "after one reconcile interval: $(openstack server show drift -f value -c status)"

echo "=== both reasons, which are different code paths ==="
kubectl -n lightstack logs deploy/lightstack-lightstack | grep "guest is off; record corrected" | tail -1
kubectl -n lightstack logs deploy/lightstack-lightstack | grep "reported running but has no guest" | tail -1
```

**Expect, for (a):** `domstate` reads **`shut off`** — the domain is still defined, which is the whole
point of this half — then `ACTIVE` immediately after the destroy, because reads deliberately do **not**
contact the hypervisor (FR-024c), then `SHUTOFF` after one 10s interval.

**Expect, for (b):** `ACTIVE` immediately, `SHUTOFF` after the interval.

**Expect, for the log:** both lines present, each carrying `recorded=ACTIVE` and `corrected_to=SHUTOFF` —
`guest is off; record corrected` from (a) and `server reported running but has no guest; record corrected`
from (b).

An immediate `SHUTOFF` would mean a read is querying libvirt, which FR-024c forbids. A `SHUTOFF` that never
arrives is FR-024e unfixed: before this feature the sweep skipped every guest whose lookup succeeded, so a
domain that existed and merely disagreed was invisible to it and the record stayed `ACTIVE` forever.

Two details that made this step lie before: the record is forced back to `ACTIVE` in (b) because the
`undefine` follows a `destroy` the sweep may already have corrected — without that the row is already
`SHUTOFF` and (b) asserts nothing. And each log line is grepped by its **full** phrase: the self-heal
failure of R3 logs `the server has no guest and could not be given one`, so a short pattern matches the
wrong message.

### R8 — A floating IP appears in the address map, tagged (T077, FR-034, FR-035, FR-036, FR-039)

The external network is created through the API with `router:external` in the body. There is no
provider-network attribute to set — those are out of scope for Lightstack, and the CLI's provider flags would
be rejected or ignored.

```bash
export EXT=$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"rt-ext","router:external":true}}' http://$NODE_IP:9696/v2.0/networks | jq -r '.network.id')
echo "external network: $EXT"
openstack subnet show rt-ext-sub >/dev/null 2>&1 || openstack subnet create rt-ext-sub --network "$EXT" --subnet-range 192.0.2.0/24 --no-dhcp >/dev/null
openstack router show rt-router >/dev/null 2>&1 || openstack router create rt-router >/dev/null
openstack router set --external-gateway rt-ext rt-router && echo "gateway set"
openstack router add subnet rt-router rt-sub && echo "interface added"
export FIP=$(openstack floating ip create rt-ext -f value -c floating_ip_address)
echo "floating ip: $FIP"
openstack server add floating ip probe "$FIP" && echo "associated"
sleep 2

echo "=== the field the defect was about ==="
echo -n "server show:  "; openstack server show probe -f json | jq -c '.addresses'
echo -n "server list:  "; openstack server list -f json | jq -c '.[] | select(.Name == "probe") | .Networks'
echo -n "fip mapping:  "; openstack floating ip list -f value -c "Floating IP Address" -c "Fixed IP Address"

echo "=== the tagging, and the administrator's view (FR-039) ==="
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers/$SRV" | jq -c '.server.addresses'
```

**Expect:** `gateway set`, `interface added`, a floating address from `192.0.2.0/24`, and `associated`.
Then `server show` and `server list` **agree**, both carrying two addresses under `rt-net` — the fixed
one and `$FIP` — and `fip mapping` pairs `$FIP` with that same fixed address. Finally the raw API shows
two entries, one `"OS-EXT-IPS:type": "fixed"` and one `"OS-EXT-IPS:type": "floating"` whose `addr` is
`$FIP`.

Before this feature only the fixed address appeared, so a user who associated an address was told it had
not taken effect.

Two things this step gets wrong if written the obvious way. **Select by name in `jq`, never with
`--name`:** the compute listing ignores every query filter (FOLLOWUPS F7), so `--name probe` returns
every server and `.[0]` is whichever sorts first — which is how this read another server's address and
looked like a detail-versus-list disagreement in the product. And **the tagging is only visible in the
raw API**: the CLI flattens the address map to bare strings, so the two `openstack` lines above can
confirm the addresses are present but say nothing about `OS-EXT-IPS:type`.

### R9 — Boot from a new volume (T107, FR-049, FR-050, FR-052, FR-053, FR-054)

The dashboard's wizard sends the image as a `source_type=image` / `destination_type=volume` mapping
rather than as `imageRef`. Lightstack used to boot the image directly and create no volume.

Four things in one block: the volume is made at the requested size, `delete_on_termination` is honoured
in **both** directions, and the size floor refuses a request below the image's minimum.

```bash
echo "=== a. the wizard's mapping creates a real, bootable volume ==="
curl -s -X POST "http://$NODE_IP:8774/v2.1/servers" -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"volsrv\",\"flavorRef\":\"m1.small\",\"networks\":[{\"uuid\":\"$NET\"}],\"block_device_mapping_v2\":[{\"boot_index\":0,\"uuid\":\"$IMG\",\"source_type\":\"image\",\"destination_type\":\"volume\",\"volume_size\":5,\"delete_on_termination\":true}]}}" | jq -r '.server.id // .badRequest.message // .serviceUnavailable.message'
sleep 12
openstack volume list --long -f value -c ID -c Name -c Status -c Size -c Bootable
echo -n "volsrv status: "; openstack server show volsrv -f value -c status
openstack volume list -f json | jq -r '.[] | select(.Name == "volsrv-boot") | "\(.Name) \(.Status) \(.Size)GiB attached=\(.["Attached to"])"'
sudo ls -la /var/lib/lightstack/volumes

echo "=== b. delete_on_termination: true removes it ==="
openstack server delete volsrv
sleep 5
echo "volsrv-boot volumes: $(openstack volume list -f value -c Name | grep -c volsrv-boot)"

echo "=== c. delete_on_termination: false keeps it ==="
curl -s -X POST "http://$NODE_IP:8774/v2.1/servers" -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"volkeep\",\"flavorRef\":\"m1.small\",\"networks\":[{\"uuid\":\"$NET\"}],\"block_device_mapping_v2\":[{\"boot_index\":0,\"uuid\":\"$IMG\",\"source_type\":\"image\",\"destination_type\":\"volume\",\"volume_size\":4,\"delete_on_termination\":false}]}}" | jq -r '.server.id'
sleep 12
openstack server delete volkeep
sleep 5
openstack volume list -f json | jq -r '.[] | select(.Name == "volkeep-boot") | "\(.Name) \(.Status)"'

echo "=== d. the size floor refuses a volume smaller than the image needs ==="
curl -s -X PATCH -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '[{"op":"replace","path":"/min_disk","value":10}]' "http://$NODE_IP:9292/v2/images/$IMG" | jq -c '{min_disk}'
echo -n "min_disk now: "; openstack image show cirros -f value -c min_disk
curl -s -X POST "http://$NODE_IP:8774/v2.1/servers" -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"toosmall\",\"flavorRef\":\"m1.small\",\"block_device_mapping_v2\":[{\"boot_index\":0,\"uuid\":\"$IMG\",\"source_type\":\"image\",\"destination_type\":\"volume\",\"volume_size\":2,\"delete_on_termination\":true}]}}" | jq -c .
echo "toosmall volumes: $(openstack volume list -f value -c Name | grep -c toosmall-boot)"
curl -s -X PATCH -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '[{"op":"replace","path":"/min_disk","value":0}]' "http://$NODE_IP:9292/v2/images/$IMG" | jq -c '{min_disk}'
```

**Expect (a):** a server id, not a message; `volsrv-boot` `in-use` **5** GiB with `Bootable` **`true`**;
`volsrv status: ACTIVE`; attached at `/dev/vdb`; and a backing file under `/var/lib/lightstack/volumes`.

**Expect (b):** `volsrv-boot volumes: 0`.

**Expect (c):** `volkeep-boot available` — retained and reusable, not left `in-use` pointing at a server
that no longer exists.

**Expect (d):** `min_disk now: 10`, then **400** naming the 10 GiB minimum, `toosmall volumes: 0`, and
`min_disk` back to **`0`** on the last line.

Three traps this step is written around:

- **`--long` is required** for the `Bootable` column. Without it the CLI leaves it empty and (a) looks
  like the volume is not bootable when the API says it is.
- **A 1 GiB request cannot test the floor.** The floor is `max(image.min_disk, ceil(image.size))`, and
  cirros expands to ~117 MB while stating no minimum — so the floor is `max(0, 1) = 1` GiB and a 1 GiB
  request *meets* it. The create is accepted, a volume is made, and the step passes without the floor
  ever being consulted. Hence raising `min_disk` first.
- **`openstack image set --min-disk` cannot raise it.** openstacksdk sends a JSON-Patch `add`, the image
  PATCH honours only `replace`, and it returns 200 with the value unchanged (FOLLOWUPS F8). Hence the
  explicit `replace`, and hence *verifying* the final `0` rather than assuming it — leaving `min_disk` at
  10 makes every later boot-from-volume in this runbook demand a 10 GiB volume.

### R10 — The compute contract suite must not hang (T052, SC-014)

**Shell T2** — a second terminal, because this runs for a while and the exports above are not needed.
Run it from the repo root, against the stack R1–R3 has already exercised.

```bash
export LIGHTSTACK_JWT_SECRET=changeme
export NODE_IP=$(ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')
export OS_AUTH_URL=http://$NODE_IP:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
export LIGHTSTACK_MODE=real
time go test -count=1 ./test/contract/nova/
```

**Expect: the suite COMPLETES.** That is the whole assertion — SC-014 and #548 F15 are about a *hang*,
not about a green suite. A real-mode run took **307s** (2026-09-02); the ~18s figure quoted elsewhere is
the **stub** baseline and is not a target here, because every `server create` now boots an actual VM and
copies a disk. Anything past ten minutes or so, with no new `request completed` lines in the pod log, is
the hang.

**`LIGHTSTACK_MODE=real` is required.** `lightstackMode()` in `console_test.go` defaults to `stub`, so without it the
console tests demand `501` and get `409` — a mismatch in the test's expectation, not in the product.

**Some tests cannot pass against a real deployment, and that is not a finding.** They hardcode
`ImageRef: "00000000-0000-0000-0000-000000000001"` and `FlavorRef: "00000000-0000-0000-0000-000000000010"`.
That image id does not exist in a real deployment — the runbook loads `cirros` with a generated id — so the
server never boots, the test's 30-second wait expires, and the action is refused with `409`. Stub mode
tolerates it because it marks every server `ACTIVE` regardless of whether the image exists. Seen on
`TestChangePasswordInvalidLength_Contract` and `TestMigrateServer_Contract`; anything reporting `409` after
almost exactly 30 seconds is this.

**What a failure here IS worth chasing:** a `500`. The 2026-09-02 run produced four —
`TestNovaActionsByName_Contract/{suspend,resume,shelve,shelveOffload}`, all
`{"computeFault":{"code":500,"message":"database error"}}`. They read as a name-resolution defect and were
nothing of the kind: `instances.libvirt_domain_id` is nullable with no default, so a server has it NULL
until a guest is defined, and three handlers scanned it into a plain `string`. Fixed; the pod log named it
exactly (`cannot scan NULL into *string`), which is where to look first.

### R11 — The 6.6.0 client can read identity resources (FR-014, B16)

Address the 6.x client **by path**, not through `PATH`. Ubuntu's `python3-openstackclient` is 6.6.0 at
`/usr/bin/openstack`, but a pip/pipx install in `~/.local/bin` shadows it — and a 10.x client succeeds
whether or not B16 is fixed, so running the wrong one turns this step into a smoke check that cannot fail.

```bash
OSC6=/usr/bin/openstack
$OSC6 --version
$OSC6 project show default -f value -c id
echo "project show exit: $?"
$OSC6 user show admin -f value -c id
echo "user show exit: $?"
$OSC6 role show admin -f value -c id
echo "role show exit: $?"
curl -s -H "X-Auth-Token: $T" "$OS_AUTH_URL/projects" | jq -r '.projects[0].links.self'
```

If `$OSC6 --version` does not report `6.x`, this step is a smoke check — record it as *not exercised*
rather than as a pass, and note which version actually ran.

**Expect:** all three commands exit `0` and print a non-empty id, and the `links.self` line is an
absolute `http://…` URL.

If `openstack --version` reports **6.x**, this is the real reproduction: before this feature all three
exited `1` with a bare `'links'` on stderr. If it reports 10.x, the commands pass regardless and only
the `links.self` line is meaningful — record which client you ran.

This is also the fixture pattern SPEC-023's runbook depends on; on a 6.x client those variables came
back **empty rather than failing**, so every later step of that runbook read the wrong thing.

### R12 — A deleted server's history survives, is served, and is reclaimed by age (FR-042, FR-042a, FR-043)

```bash
openstack network show rt-net -f value -c id >/dev/null || echo "STOP: rt-net is missing — run §1 first"
openstack server create --flavor m1.small --image cirros histsrv --network rt-net >/dev/null
sleep 10
export HS=$(openstack server show histsrv -f value -c id)
test -n "$HS" || echo "STOP: histsrv was not created — every check below would 404 on an empty id and look like a gate working"
openstack server lock histsrv
openstack server delete histsrv
sleep 3
echo -n "actions at microversion 2.21: "
curl -s -H "X-Auth-Token: $T" -H 'OpenStack-API-Version: compute 2.21' "http://$NODE_IP:8774/v2.1/servers/$HS/os-instance-actions" | jq -r '.instanceActions | length'
echo -n "status at microversion 2.1: "
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" -H 'OpenStack-API-Version: compute 2.1' "http://$NODE_IP:8774/v2.1/servers/$HS/os-instance-actions"
echo -n "status with no header: "
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers/$HS/os-instance-actions"
```

**Expect:** a non-zero action count at 2.21; **404** at 2.1 — the microversion gate, matching upstream,
which exposes deleted-instance actions only from 2.21; and **200** with no header, because a client that
does not negotiate gets the product's full behaviour.

**An empty `$HS` fakes a pass.** The URL becomes `/v2.1/servers//os-instance-actions`, which answers
`0`, `404`, `404` — and two of those three are the values this step is looking for. If either STOP line
above printed, the result is meaningless.

Reclamation needs a short retention age, so redeploy with one, observe, and put it back.

**This stays a second block on purpose** — it is the only test step that does. It redeploys twice, and a
redeploy is a slow, config-changing action that should not run before you have read the microversion
results above. Everything else in §2 is one paste per step.

**Two rows, not one.** Reclaiming the orphan only shows the sweep deletes something; the property that
matters is what it *refuses* to delete. So plant an equally over-age action against a server that STILL
EXISTS — `probe`, from §1 — and require it to survive. Counting "actions of live servers" without
planting one is worthless: on a database with no servers the count is `0` before and after, which reads
as a pass and proves nothing.

```bash
test -n "$SRV" && echo "probe: $SRV" || echo "STOP: \$SRV is unset — run §1 first, or this step cannot fail"
helm upgrade --install lightstack charts/lightstack --namespace lightstack --reuse-values --set nova.instanceActionRetention=1m
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s
docker exec lightstack-postgres psql -U lightstack -d lightstack -q -c "INSERT INTO instance_actions (instance_id, action, request_id, user_id, project_id, start_time, message) VALUES ('bbbbbbbb-cccc-dddd-eeee-ffffffffff01','stop','r-old','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002', now() - interval '1 hour','probe-overage');"
docker exec lightstack-postgres psql -U lightstack -d lightstack -q -c "INSERT INTO instance_actions (instance_id, action, request_id, user_id, project_id, start_time, message) VALUES ('$SRV','stop','r-old-live','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002', now() - interval '1 hour','probe-live-overage');"
echo -n "orphan before: "; docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT count(*) FROM instance_actions WHERE message = 'probe-overage';"
echo -n "live-server row before: "; docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT count(*) FROM instance_actions WHERE message = 'probe-live-overage';"
sleep 15
echo -n "orphan after: "; docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT count(*) FROM instance_actions WHERE message = 'probe-overage';"
echo -n "live-server row after: "; docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc "SELECT count(*) FROM instance_actions WHERE message = 'probe-live-overage';"
kubectl -n lightstack logs deploy/lightstack-lightstack | grep -i "action records of deleted servers reclaimed" | tail -1
helm upgrade --install lightstack charts/lightstack --namespace lightstack --reuse-values --set nova.instanceActionRetention=2160h
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s
docker exec lightstack-postgres psql -U lightstack -d lightstack -q -c "DELETE FROM instance_actions WHERE message = 'probe-live-overage';"
```

**Expect:** `orphan before: 1` → `orphan after: 0`; `live-server row before: 1` → **`live-server row
after: 1`**; and a reclamation log line naming `reclaimed: 1` — one, not two. Both rows are the same age,
so the only thing separating them is whether their server still exists (FR-042c). A `live-server row
after: 0` means the `NOT EXISTS` clause is gone and an operator loses the history of a running server.

The last line removes the planted row so it does not confuse a later run.

> **This deletes orphaned action rows from the shared database.** On a host shared with other worktrees
> that backlog may be large (1731 rows were reclaimed on one such host). They are orphans by definition,
> but if another investigation depends on them, skip this sub-step.

### R13 — A stop waits for the guest, and a settled state refuses (FR-022a, FR-022b, FR-026c)

The checks R1 cannot separate, plus the one thing no unit test can reach: `stopVMReal` needs a real
libvirt connection, so the shutdown/wait/force sequence itself is only exercised here. The policy half
(`waitForShutoff`) is unit-tested; the wiring is not.

Four sub-steps in order, because each depends on the state the previous one leaves. **There are two
`time` outputs** — the first must be seconds, the last must be ~30s — so read them by their headings.

```bash
echo "=== a. the stop does not return before the guest is off ==="
openstack server start probe 2>/dev/null || true
sleep 5
time openstack server stop probe
echo -n "domstate: "; sudo virsh -c qemu:///system domstate $SRV
openstack server show probe -f value -c status -c OS-EXT-STS:power_state

echo "=== b. a settled state is a conflict, not a fault ==="
curl -s -o /dev/null -w 'stop an already-stopped server: %{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"os-stop": null}' http://$NODE_IP:8774/v2.1/servers/$SRV/action
openstack server start probe
sleep 5
curl -s -o /dev/null -w 'start an already-running server: %{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"os-start": null}' http://$NODE_IP:8774/v2.1/servers/$SRV/action

echo "=== c. a refusal corrects the record rather than latching ==="
docker exec lightstack-postgres psql -U lightstack -d lightstack -q -c "UPDATE instances SET status = 'SHUTOFF', power_state = 4 WHERE id = '$SRV';"
echo -n "forged read: "; openstack server show probe -f value -c status
curl -s -o /dev/null -w 'os-start over a stale record: %{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"os-start": null}' http://$NODE_IP:8774/v2.1/servers/$SRV/action
echo "read after the refusal:"; openstack server show probe -f value -c status -c OS-EXT-STS:power_state
openstack server stop probe
echo "stop after the refusal: $?"

echo "=== e. a guest that cannot honour ACPI is forced off AND still answers ==="
openstack server start probe
sleep 3
time openstack server stop probe
echo "stop exit: $?"
echo -n "domstate: "; sudo virsh -c qemu:///system domstate $SRV
openstack server show probe -f value -c status -c OS-EXT-STS:power_state
kubectl -n lightstack get secret lightstack-lightstack-config -o jsonpath='{.data.lightstack\.yaml}' | base64 -d | grep shutdown_timeout
kubectl -n lightstack logs deploy/lightstack-lightstack | grep -ic "exceeds the" | sed 's/^/clamp warnings (must be 0): /'
```

**Expect (a):** `shut off` and `SHUTOFF`/`4` **immediately**, with no sleep between the stop and the
reads — the assertion is that none is needed. An ordinary settled cirros guest answers ACPI, so `real` is
a couple of seconds, well under the 30s `shutdown_timeout`. A `running` here is FR-022a unfixed.

**Expect (b):** `409` for both. A `500` is the defect this whole step exists for.

**Expect (c):** the forged read says `SHUTOFF`; the start answers `409`; the read after it says
`ACTIVE`/`1`, because the refusal corrected the record on its way out; and the following stop succeeds
(`stop after the refusal: 0`). A second `409` on that stop means the refusal latched.

**Expect (e):** `real` of roughly **30s** rather than a couple of seconds — the guest never answered and
was forced off at the grace expiry — then `stop exit: 0`, `shut off`, `SHUTOFF`/`4`,
`shutdown_timeout: "30s"` and `clamp warnings (must be 0): 0`.

(a) and (e) are the same command against a different guest state: (a) measures a **normal** stop, (e) a
**forced** one, which is why (e) stops the guest three seconds after boot, before `acpid` is up. Both
matter, and only (e) exercises the force path.

The failure mode (e) exists for: a `RemoteDisconnected('Remote end closed connection without response')`
followed by `ConflictException: 409 ... Cannot os-stop`. That is the stop working, running past the
server's write deadline, and losing its response — the client then retries and is correctly told the
guest is already stopped. **The 409 is the symptom; the missing response is the cause.** A clamp warning
in the last line means the deployed `shutdown_timeout` was above the 50s ceiling and was shortened.

**d. The sweep sees a guest that disagrees, not only one that is gone** — covered by **R7**, which runs
the same scenario against the `drift` server and also covers the vanished-guest reason. Not repeated here.

---

## 3. Teardown steps

```bash
export KUBECONFIG=~/.kube/k3s.yaml

openstack server delete probe diag drift volsrv volkeep failboot histsrv toosmall 2>/dev/null || true
openstack floating ip list -f value -c ID | xargs -r openstack floating ip delete 2>/dev/null || true
openstack router unset --external-gateway rt-router 2>/dev/null || true
openstack router remove subnet rt-router rt-sub 2>/dev/null || true
openstack router delete rt-router 2>/dev/null || true
openstack subnet delete rt-sub rt-ext-sub 2>/dev/null || true
openstack network delete rt-net rt-ext nosub-net 2>/dev/null || true
openstack volume delete volkeep-boot toosmall-boot 2>/dev/null || true
openstack image delete cirros 2>/dev/null || true

echo "--- no floating address may survive on a host interface ---"
ip -4 -o addr show | grep -F 192.0.2 && echo "LEFTOVER: remove it before the next run" || echo "no TEST-NET-1 address remains"

helm uninstall lightstack --namespace lightstack 2>/dev/null || true
kubectl delete namespace lightstack --ignore-not-found --timeout=120s

for d in $(sudo virsh -c qemu:///system list --all --name); do sudo virsh -c qemu:///system destroy "$d" 2>/dev/null || true; sudo virsh -c qemu:///system undefine --nvram "$d" 2>/dev/null || true; done
sudo virsh -c qemu:///system list --all

docker exec lightstack-postgres psql -U lightstack -d lightstack -q -c "DELETE FROM instance_actions WHERE message = 'probe-overage';" 2>/dev/null || true
make db-down 2>/dev/null || true

sudo rm -rf /var/lib/lightstack
rm -f /tmp/cirros.img /tmp/lightstack-spec025.tar
sudo k3s ctr images rm docker.io/library/lightstack:spec025 2>/dev/null || true
```

**Pass =** the `lightstack` namespace is gone, `virsh list --all` is empty, `/var/lib/lightstack` is removed, and the
image is out of the k3s store. Stop the **T2** shell if the contract run is still going.

The floating-IP and server deletions are written to work in a fresh shell too — they name resources
rather than relying on the exports from sections 1 and 2 surviving.

---

> **Why the address check matters.** A floating address left on an interface is picked up by any
> `NODE_IP` derivation that scans interfaces, and §1's used to. The next run then deploys with
> `endpointHost` set to a floating IP, and the failure lands in §1 as
> `Failed to discover available identity versions when contacting http://192.0.2.100:35357/v3` — a long
> hang that reads like a broken identity service. §1 now derives `NODE_IP` from the default route, so
> this is belt-and-braces, but the leftover is still worth clearing.

## Troubleshooting

**The pod is `CrashLoopBackOff` on `bind: address already in use`.** Lightstack runs with `hostNetwork`, so it
competes with anything already on the node's ports — including a host-local `./bin/lightstack`. The §1 block
prints `PORTS BUSY` when this is the case; kill the other listener, then
`kubectl -n lightstack rollout restart deploy/lightstack-lightstack`.

**The image upload returns `503 failed to upload image`, or the catalog says `localhost`.** You are
talking to a different Lightstack than the one you deployed — most often a host-local process on the same
ports. Check `pgrep -x lightstack` and `openstack catalog list`.

**`virsh` shows no domains but the API reports `ACTIVE`.** That is either the divergence this feature
fixes (before R1) or the two-libvirtd hypothesis (R4). Compare the socket Lightstack uses with the host's
before concluding either.

**`openstack server show probe` reports `ERROR` right after §1.** Check
`openstack server show probe -f json | jq .fault` — with the fault field this feature adds, the reason
is now readable. A missing raw base under `/var/lib/lightstack/images` is the usual cause.

**R9 refuses with `503 could not create the boot volume`.** The image's raw base is not on this node.
`sudo ls /var/lib/lightstack/images` must show an `image-<uuid>.raw`. Note the refusal itself is correct
behaviour (FR-055 forbids falling back to an image boot) — it just means R9's happy path cannot run.

**A `virsh` command says `failed to get domain 'probe'`.** The libvirt domain is named
`instance-<first 8 characters of the instance id>`, not the OpenStack server name. Address it by
`$SRV` — the domain UUID equals the instance id.
