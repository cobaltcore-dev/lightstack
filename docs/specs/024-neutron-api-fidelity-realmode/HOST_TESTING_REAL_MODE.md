# HOST_TESTING — SPEC-024 real mode (Neutron API fidelity & real-mode networking)

Validates the halves of [cloud-storage#546](https://github.com/cobaltcore-dev/cloud-storage/issues/546)
that stub mode cannot prove: a port's TAP and its bridge sharing a namespace, a guest actually
reaching `169.254.169.254`, guest egress through an external gateway, and Horizon's *Set Gateway*
dropdown. Run [HOST_TESTING.md](./HOST_TESTING.md) first — the API-level behaviour is proved there and
is not repeated here.

**Topology** — two clusters on one Ubuntu host:

- **k3s** — o3k in real mode (`libvirt_mode: real`, `networking_mode: iptables`) plus Horizon, both
  via the charts in this repo. o3k runs `hostNetwork`, so it binds the node's ports directly.
- **minikube (`rook` profile)** — Rook Ceph with an RGW S3 endpoint, stood up by the existing
  `deployments/rook/setup-rook-rgw.sh`. Glance stores images there; VM disks and volumes stay
  node-local under `/var/lib/o3k`.

**Run all commands from the repo root.** Real mode needs **genuine root** — rootless Podman cannot
create host network namespaces.

## Validates

| Acceptance criterion (ticket / spec) | Proved by |
|---|---|
| F10 — the port's TAP and its network's bridge are in the same namespace, no orphan `light-ns-*` | R1 |
| F10 — the five contract tests the ticket names (2026-08-12) pass in real mode | R2 |
| F10 / FR-004 — a guest boots with a carrier and fetches instance metadata | R3 |
| FR-023a — a gatewayless subnet still reaches metadata, with no default route | R4 |
| F11 / FR-036 — guest egress through an external gateway; a floating IP reaches the guest | R5 |
| F11 / FR-037 — Horizon's router *Set Gateway* dropdown offers the external network | R6 |
| B11 — Horizon and the API report a network's real owner | R7 |
| #546 (2026-08-12) — the reconciler logs its **effective** interval, not `0s` | R8 |
| SC-011 — Glance on RGW and the other services still work | R9 |

**Follow-up findings (2026-08-13) — not part of #546.** Found during this host pass and fixed on this
branch as a separate commit. Each step below carries the context it needs.

| Acceptance criterion | Proved by |
|---|---|
| Finding 1 — metadata DNAT rules are reclaimed on delete, by the reconciler, and at teardown | R10, and the teardown check in §3 |
| Finding 2 — Horizon's admin **Project** column shows the owning project | R11 |

---

## 0. Prerequisites

One paste. It installs lightweight CLI tools and k3s, and *verifies* the heavier preconditions — a
container engine, `helm`, `minikube` with a VM driver, and libvirt/KVM. Install those first if
missing; the block will not do it for you.

```bash
sudo apt-get update
sudo apt-get install -y curl jq python3-openstackclient libvirt-clients qemu-kvm awscli
sudo systemctl enable --now libvirtd
command -v docker >/dev/null || command -v podman >/dev/null || echo "install docker or podman first"
command -v helm >/dev/null || echo "install helm first"
command -v minikube >/dev/null || echo "install minikube first (kvm2 driver)"
command -v k3s >/dev/null || curl -sfL https://get.k3s.io | sudo sh -
sudo systemctl is-active k3s || sudo systemctl start k3s
mkdir -p ~/.kube
sudo k3s kubectl config view --raw | sudo tee ~/.kube/k3s.yaml >/dev/null
sudo chown "$USER" ~/.kube/k3s.yaml
chmod 600 ~/.kube/k3s.yaml
echo "--- versions ---"
docker --version || podman --version
helm version --short
minikube version --short
jq --version
openstack --version
sudo virsh list --all
KUBECONFIG=~/.kube/k3s.yaml kubectl get nodes
```

**Pass =** the k3s node is `Ready`, `virsh` succeeds (an empty list is fine), every version line
prints, and no `install … first` line appears.

**Do not skip this section**, even on a host that has run earlier features. It is what installs k3s and
writes `~/.kube/k3s.yaml`; without it section 1.2 wastes a full image build before failing with a
`localhost:8080` error that points nowhere near the cause.

---

## 1. Setup steps

Two terminals for the whole run: **T1** talks to k3s and runs every test step; **T2** talks to the
`rook` minikube profile and is only needed for 1.1 and teardown. They use different kubeconfigs, so
keep them apart.

### 1.1 Rook Ceph + RGW on minikube

**Shell T2 (rook)** — uses the default kubeconfig, not k3s. `--delete` first, because a stale
`/var/lib/rook` wedges cephx and the mon never reaches quorum.

```bash
unset KUBECONFIG
deployments/rook/setup-rook-rgw.sh --delete 2>/dev/null || true
deployments/rook/setup-rook-rgw.sh
export ROOK_IP=$(minikube -p rook ip)
export RGW_ACCESS=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-o3k -o jsonpath='{.data.AccessKey}' | base64 -d)
export RGW_SECRET=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-o3k -o jsonpath='{.data.SecretKey}' | base64 -d)
printf 'export RGW_URL=%s\nexport RGW_ACCESS=%s\nexport RGW_SECRET=%s\n' "http://$ROOK_IP:30080" "$RGW_ACCESS" "$RGW_SECRET" > /tmp/rgw.env
source /tmp/rgw.env
AWS_ACCESS_KEY_ID=$RGW_ACCESS AWS_SECRET_ACCESS_KEY=$RGW_SECRET AWS_DEFAULT_REGION=us-east-1 aws --endpoint-url "$RGW_URL" s3 mb s3://images 2>/dev/null || echo "bucket exists"
cat /tmp/rgw.env
```

**Pass =** the script ends with `ROUNDTRIP OK` / `HEALTH_OK`, and `s3 mb` succeeds or says
`bucket exists`. Both clusters are libvirt VMs on this host, so k3s reaches `$RGW_URL` directly with
no port-forward.

### 1.2 Build the image and deploy o3k + Horizon on k3s

**Shell T1 (k3s)** — keep this shell for the rest of the run. `endpointHost` must be the **node IP**:
with `hostNetwork` o3k listens on the node, so one value works from both the Horizon pod and your CLI.

The cluster check **gates** the rest of the stage rather than just warning. Without it this block
builds the image for a minute, starts Postgres, and only then fails with
`localhost:8080: connection refused` — which names neither the cause nor the fix, because
`export KUBECONFIG=<missing file>` does not error and kubectl quietly falls back to its default
endpoint. The gate is an `if`, deliberately: an `exit` here would close the terminal you pasted into.

The `extraEnv[0]` arguments are **quoted deliberately**. Unquoted, zsh reads `[0]` as a glob character
class, finds no match, and with its default `nomatch` aborts the whole `helm` command — so the
namespace is never created and every following line fails with `namespaces "o3k" not found`. The
static keys take single quotes; the two carrying `$RGW_*` take double quotes so they still expand.

```bash
export KUBECONFIG=~/.kube/k3s.yaml
export NODE_IP=$(hostname -I | awk '{print $1}')
source /tmp/rgw.env
if ! test -s "$KUBECONFIG" || ! kubectl get nodes >/dev/null 2>&1; then
  echo "STOP: no reachable k3s cluster. Run section 0 first — it installs k3s and writes $KUBECONFIG."
  echo "A missing kubeconfig does not error: kubectl falls back to localhost:8080, which is what the connection-refused message really means."
else
  kubectl get nodes
  docker build -f build/package/Dockerfile -t docker.io/library/o3k:spec024 .
  rm -f /tmp/o3k-spec024.tar
  docker save docker.io/library/o3k:spec024 -o /tmp/o3k-spec024.tar
  sudo k3s ctr images import /tmp/o3k-spec024.tar
  sudo mkdir -p /var/lib/o3k/images /var/lib/o3k/volumes /var/lib/o3k/instances /var/lib/o3k/cloud-init /var/lib/o3k/dhcp /var/lib/o3k/console-tokens
  docker inspect o3k-postgres >/dev/null 2>&1 || make db-up
  sleep 5
  ss -lntp 2>/dev/null | grep -E ':(35357|8774|9292|9696|8776|8775) ' && echo "PORTS BUSY: stop whatever is listed above" || echo "ports free"
  pgrep -x o3k >/dev/null && echo "WARNING: a host-local o3k is running; kill that PID first" || true
  helm upgrade --install o3k charts/o3k --namespace o3k --create-namespace --set image.repository=o3k --set image.tag=spec024 --set image.pullPolicy=Never --set deploymentMode=single-node --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true --set nova.libvirtMode=real --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables --set cinder.storageMode=local --set glance.storageMode=s3 --set glance.s3.endpoint=$RGW_URL --set glance.s3.bucket=images --set glance.s3.region=us-east-1 --set 'extraEnv[0].name=AWS_ACCESS_KEY_ID' --set "extraEnv[0].value=$RGW_ACCESS" --set 'extraEnv[1].name=AWS_SECRET_ACCESS_KEY' --set "extraEnv[1].value=$RGW_SECRET" --set endpointHost=$NODE_IP --set secrets.jwtSecret=changeme --set secrets.adminPassword=secret --set database.url='postgres://o3k:secret@127.0.0.1:5432/o3k?sslmode=disable'
  kubectl -n o3k rollout status deploy/o3k-o3k --timeout=240s
  helm upgrade --install horizon charts/horizon --namespace o3k
  kubectl -n o3k rollout status deploy/horizon-o3k-horizon --timeout=240s
  kubectl -n o3k get secret o3k-o3k-config -o jsonpath='{.data.o3k\.yaml}' | base64 -d | grep -E "_mode:"
  kubectl -n o3k get pods
fi
```

**Pass =** no `STOP:` line and `kubectl get nodes` shows the node `Ready`; `ports free`; every pod
`Running`, none `CrashLoopBackOff`; and the mode read-back shows
`libvirt_mode: "real"`, `networking_mode: "iptables"`, `security_group_mode: "iptables"`,
`storage_mode: "s3"` (glance) and `storage_mode: "local"` (cinder). A `stub` anywhere means the rest
of this runbook proves nothing about real mode.

### 1.3 Authenticate, load an image, create fixtures

```bash
export OS_AUTH_URL=http://$NODE_IP:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
export T=$(openstack token issue -f value -c id)
test -s /tmp/cirros.img || curl -sL -o /tmp/cirros.img http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image show cirros >/dev/null 2>&1 || openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img cirros >/dev/null
openstack image show cirros -f value -c name -c status -c size
export NET=$(openstack network create rt-net -f value -c id)
openstack subnet create rt-sub --network "$NET" --subnet-range 10.244.10.0/24 >/dev/null
export OTHER=$(openstack project create --or-show rt-other -f value -c id)
echo "token=${#T} net=$NET other=$OTHER"
docker exec o3k-postgres psql -U o3k -d o3k -tAc "SELECT version, dirty FROM schema_migrations"
```

**Pass =** `cirros` reports `active` with a non-zero size — proving the RGW backend accepted the
upload — and the migration version is **`66`**, `dirty = f`.

---

## 2. Test steps

### R1 — The TAP and its bridge share a namespace (F10, P5)

The real-mode half of F10: the bridge is built in the root netns for libvirt's benefit, and the TAP
used to go into `light-ns-<project>`, which nothing on that path ever created.

```bash
openstack port create --network "$NET" rt-p1 -f value -c id -c mac_address -c fixed_ips
echo "--- bridges in root ns ---"; ip -br link show type bridge | grep '^br-' || echo "NO BRIDGES"
echo "--- taps in root ns ---"; ip -br link | grep '^tap-' || echo "NO TAPS"
echo "--- namespaces ---"; sudo ip netns list || echo "none"
```

**Expect:** the port is created; at least one `br-*` **and** at least one `tap-*` appear in the root
namespace listing; and `ip netns list` shows **no `light-ns-*`** entry holding the port's TAP. Before
the fix this call returned `500 failed to attach TAP to bridge`.

### R2 — The five contract tests the ticket names (F10, 2026-08-12 evidence)

The ticket re-confirmed F10 on a clean real-mode k3s stack with exactly these five failing, while
Nova's *internal* port allocation succeeded in the same run — the two paths did not fail together.

```bash
go test -count=1 -v -run 'TestNeutronPortCreate_Contract|TestNeutronNetworkLifecycle_Contract|TestPortForwardingLifecycle_Contract|TestPortForwardingValidation_Contract|TestPortTopologyData_Contract' ./test/contract/neutron/ 2>&1 | grep -E '^(--- |ok|FAIL)'
```

**Expect:** all five `--- PASS` and the package `ok`. Any `Failed to create port` / `500` means the
namespace fix did not take effect in this deployment — re-check the mode read-back from 1.2.

### R3 — A guest boots with a carrier and reaches metadata (F10, FR-004, P6)

```bash
openstack server create --flavor m1.small --image cirros --network "$NET" rt-guest >/dev/null
for i in $(seq 1 60); do s=$(openstack server show rt-guest -f value -c status); test "$s" = ACTIVE && break; sleep 5; done
openstack server show rt-guest -f value -c status -c addresses
for i in $(seq 1 36); do openstack console log show rt-guest 2>/dev/null > /tmp/rt-guest.console; grep -q '=== cirros: current=' /tmp/rt-guest.console && break; sleep 5; done
grep -q '=== cirros: current=' /tmp/rt-guest.console && echo "boot finished: $(grep -o 'uptime=[0-9.]*' /tmp/rt-guest.console | tail -1)" || echo "BOOT DID NOT FINISH within 180s — the counters below are inconclusive, not a failure"
echo "metadata route delivered: $(grep -c 'ip-route:169.254.169.254' /tmp/rt-guest.console)"
echo "instance-id consumed:     $(grep -c 'instance-id: ' /tmp/rt-guest.console)"
echo "failed, excluding public-ipv4: $(grep 'failed to get http://169.254.169.254' /tmp/rt-guest.console | grep -vc 'public-ipv4')"
grep -E 'ip-route:169.254|checking http|failed to get|instance-id: ' /tmp/rt-guest.console | tail -8
```

**Expect:** status `ACTIVE` with a real address from `10.244.10.0/24` — **not** an empty `addresses`;
`metadata route delivered` is **1** (DHCP option 121 arrived, so FR-023a's plumbing works);
`instance-id consumed` is **1**; and `failed, excluding public-ipv4` is **0**.

`public-ipv4` is excluded deliberately, not tolerated. A guest with no floating IP has no public
address, so the metadata service answers 404 and omits the key from its directory listing — which is
what upstream does. Reporting `local-ipv4` there would tell the guest it has a routable address when
it does not. The datasource probes the key regardless and logs one `failed to get`; that line is the
correct outcome, so counting it as a failure would make this step unpassable. Attach a floating IP
(R5) and it goes away.

`instance-id consumed` is the strongest single signal here: it means the guest did not merely reach
the service, it parsed and used the response. The `=== datasource: ec2 net ===` block it comes from
resolves several keys (`instance-id`, `local-hostname`, `availability-zone`), so a populated block is
also the cheapest confirmation that the metadata service answers more than one key.

**The wait polls rather than sleeping a fixed interval, deliberately** (2026-08-13). It waits for the
`=== cirros: current=… uptime=… ===` line, which cirros emits at the end of its init — *after* the
datasource block. A fixed `sleep 45` was structurally too short: it starts when the server reports
`ACTIVE`, which is before the guest has finished booting, and an observed run needed `uptime=45.09`
of guest time plus console-flush lag on top of that. The failure mode it produced is the one worth
avoiding: the log was captured with only `checking http://169.254.169.254/…` in it, so two counters
read `0` and the third read `0` **vacuously** — nothing had been attempted yet, which looks identical
to everything having succeeded. Waiting for the boot-summary marker makes all three honest, and the
explicit `BOOT DID NOT FINISH` line keeps "still booting" distinguishable from "booted and metadata
failed" instead of leaving it to judgement.

Read those two numbers rather than eyeballing the log, because two lines look alarming and are not:

- `checking http://169.254.169.254/…` is the **success** path — it is the datasource starting, not an
  error. An earlier version of this step grepped for `169.254.169.254` and treated any hit as failure.
- `no interfaces have a carrier` / `eth0: waiting for carrier` appear transiently on a healthy boot:
  cirros polls before the TAP is up. Their presence proves nothing either way, so this step no longer
  asserts their absence. What matters is that the guest ends up with an address.

A non-zero `failed key fetches` count with the route present means the network path is fine and the
metadata **service** is refusing specific keys — see the note under R4.

### R4 — A gatewayless subnet still reaches metadata (FR-023a, P12–P13)

The DHCP template used to guard the router option and the metadata route on the same value, so
honouring `--gateway none` would have silently dropped metadata — reintroducing R3's symptom.

```bash
export NET2=$(openstack network create rt-net-nogw -f value -c id)
openstack subnet create rt-sub-nogw --network "$NET2" --subnet-range 10.244.20.0/24 --gateway none -f value -c gateway_ip
openstack server create --flavor m1.small --image cirros --network "$NET2" rt-guest2 >/dev/null
for i in $(seq 1 60); do s=$(openstack server show rt-guest2 -f value -c status); test "$s" = ACTIVE && break; sleep 5; done
sudo find /var/lib/o3k -name '*.conf' -exec grep -H dhcp-option {} + 2>/dev/null || echo 'no dnsmasq config found yet'
for i in $(seq 1 36); do openstack console log show rt-guest2 2>/dev/null > /tmp/rt-guest2.console; grep -q '=== cirros: current=' /tmp/rt-guest2.console && break; sleep 5; done
grep -q '=== cirros: current=' /tmp/rt-guest2.console && echo "boot finished: $(grep -o 'uptime=[0-9.]*' /tmp/rt-guest2.console | tail -1)" || echo "BOOT DID NOT FINISH within 180s — the counters below are inconclusive, not a failure"
echo "instance-id consumed:          $(grep -c 'instance-id: ' /tmp/rt-guest2.console)"
echo "failed, excluding public-ipv4: $(grep 'failed to get http://169.254.169.254' /tmp/rt-guest2.console | grep -vc 'public-ipv4')"
echo "default route advertised:      $(grep -c 'default via' /tmp/rt-guest2.console)"
grep -E 'leased|host route to 169.254.169.254|instance-id: ' /tmp/rt-guest2.console | tail -6
```

**Expect:** the subnet reports `None` for its gateway. In the config listing, the gatewayless
network's line has **only** `dhcp-option=121,169.254.169.254/32,<bridge-ip>` — no `dhcp-option=3`, and
no trailing `0.0.0.0/0` leg. The other networks still show **both** options, proving the change did
not regress the ordinary case (P15).

Then: `instance-id consumed` is **1**, `failed, excluding public-ipv4` is **0**, and
`default route advertised` is **0**.

Counting the consumed instance-id is what makes this step meaningful. Grepping only for
`169.254.169.254` shows a `checking http://…` line on a guest that never got an answer, so a broken
run and a working one look alike. The combination that can only occur if FR-023a holds is: a lease, a
host route to `169.254.169.254`, **no** default route, and a consumed instance-id.

### R5 — Guest egress and floating IPs through an external gateway (F11, FR-036, X17)

```bash
export EXT=$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"rt-ext","router:external":true}}' http://$NODE_IP:9696/v2.0/networks | jq -r '.network.id')
openstack subnet create rt-ext-sub --network "$EXT" --subnet-range 192.0.2.0/24 --no-dhcp >/dev/null
openstack router create rt-router >/dev/null
openstack router set --external-gateway rt-ext rt-router && echo "gateway set"
openstack router add subnet rt-router rt-sub && echo "interface added"
sudo iptables -t nat -S POSTROUTING | grep -E '10.244.10.0/24' || echo "NO SNAT RULE"
openstack floating ip create rt-ext -f value -c floating_ip_address
```

**Expect:** `gateway set` and `interface added`; a MASQUERADE/SNAT rule for `10.244.10.0/24` is
present; and the floating IP is allocated from `192.0.2.0/24`. Setting the gateway to a
**non**-external network must be refused — that is covered in the stub runbook (S10).

### R6 — Horizon's Set Gateway dropdown is populated (F11, FR-037, X16)

Horizon serves under **`/dashboard/`** — the bare port answers `200` but is not the dashboard, and
`/auth/login/` is a `404`.

R5 leaves `rt-router` **with** a gateway, and Horizon hides *Set Gateway* on a router that already has
one (it offers *Clear Gateway* instead). So this step needs its own gateway-less router; without it the
action can never appear and the step looks broken when it is not.

```bash
openstack router create rt-router-nogw -f value -c name -c external_gateway_info
echo "Horizon: http://$NODE_IP:30080/dashboard/"
curl -s -o /dev/null -w 'dashboard: HTTP %{http_code}\n' "http://$NODE_IP:30080/dashboard/auth/login/"
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:9696/v2.0/networks?router:external=true" | jq -c '[.networks[] | {name, "router:external"}]'
```

**Expect:** the router reports `None` for its gateway; `dashboard: HTTP 200`; and the API line lists
`rt-ext` — before F11 that query returned an unconditionally empty list, which is the defect the ticket
reports.

Then in the browser, logged in as `admin` / `secret`, project `default`: **Project → Network →
Routers**. *Set Gateway* is a **row action on the list**, not a tab on a router's detail page — the
detail page only has Overview, Interfaces and Static Routes. Open the actions dropdown on the
**`rt-router-nogw`** row, choose *Set Gateway*, and confirm the *External Network* dropdown offers
`rt-ext`. `rt-router` in the same list will offer *Clear Gateway* instead, which is R5's gateway showing
through the UI.

Note the page and any traceback. A dropdown that renders but is **empty** is the F11 defect; a dropdown
that is absent means the router already has a gateway.

### R7 — A network reports its real owner (B11, FR-017)

```bash
export NID=$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"network\":{\"name\":\"rt-b11\",\"project_id\":\"$OTHER\"}}" http://$NODE_IP:9696/v2.0/networks | jq -r '.network.id')
echo "stored: $(docker exec o3k-postgres psql -U o3k -d o3k -tAc "SELECT project_id FROM networks WHERE id = '$NID'")"
echo "wanted: $OTHER"
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:9696/v2.0/networks?router:external=true" | jq -c '[.networks[] | {name, project_id}]'
```

**Expect:** `stored` equals `wanted`, and the listed networks carry their owning project rather than
the caller's. Those two — the API and the row behind it — are what FR-017 says; read the verdict from
them.

As an administrator `openstack network show rt-b11` returns 404 — a known, deliberate gap (no
administrator override on network read); read the stored value above instead.

Horizon's **Admin → Network → Networks** Project column is a *secondary* observation, not the
assertion. Upstream assigns that column inside a block guarded by DHCP-agent-scheduler support, so
until the 2026-08-13 follow-up it rendered `-` for every row while the API answered correctly —
the column was testing Horizon's agent-scheduler coupling, not this feature. With
`dhcp_agent_scheduler` advertised and `GET /networks/{id}/dhcp-agents` answering, it should now show
the owning project and `0` agents; a blank column is a Horizon-side regression to chase separately,
and never on its own a B11 failure.

### R8 — The reconciler logs its effective interval (#546, 2026-08-12)

Only real mode starts the reconciler, so this cannot be checked in the stub runbook.

```bash
kubectl -n o3k logs deploy/o3k-o3k | grep -i "network reconciler" | tail -3
```

**Expect:** `Real-mode network reconciler started (interval: 30s)`. **`0s` is the defect** — the
configured value is unset by default while `NewNetworkReconciler` clamps it to 30s, so the old line
read as though periodic reconciliation were switched off.

### R9 — Glance on RGW, and nothing else regressed (SC-011)

```bash
AWS_ACCESS_KEY_ID=$RGW_ACCESS AWS_SECRET_ACCESS_KEY=$RGW_SECRET AWS_DEFAULT_REGION=us-east-1 aws --endpoint-url "$RGW_URL" s3 ls s3://images/ --recursive --human-readable
openstack image show cirros -f value -c id -c size -c status
go test -count=1 -p 1 ./test/contract ./test/contract/cinder ./test/contract/glance ./test/contract/keystone ./test/contract/neutron 2>&1 | grep -E '^(ok|FAIL)'
openstack volume create --size 1 rt-vol -f value -c status
openstack server list -f value -c Name -c Status
```

**Expect:** an object named `images/image-<uuid>.raw` whose uuid and size match `image show` — that is
the proof Glance used RGW rather than the local fallback. Then five suites `ok`, the volume
`available`, and both guests `ACTIVE`.

`--recursive` matters: Glance writes under an `images/` prefix inside the bucket, so a plain `s3 ls`
shows only `PRE images/` and reads as though the upload never happened.

**`test/contract/nova` is deliberately excluded, and must not be run here.** Four of its tests assert
**stub-mode** behaviour and cannot pass against real libvirt guests:

| Test | Asserts | Real mode returns |
|---|---|---|
| `TestNovaGetVNCConsole_Contract` | `501` ("stub mode must refuse") | `409 console not available: the instance is not running` |
| `TestNovaCreateRemoteConsole_Contract` | `501` | `409` |
| `TestChangePasswordInvalidLength_Contract` | `400` | `409` — a running guest hits a state conflict before the length check |
| `TestMigrateServer_Contract` | passes on a stub instance | 30s timeout against a real domain |

That is a gap in the *suite*, not in the product: the same package returns `ok` in ~16s against this
stack **before** any guest is booted, and none of the four files is touched by this feature. Run it
before R3 if you want it covered, or check it on the stub stack (`HOST_TESTING.md` S11) where its
assumptions hold.

---

## 2b. Follow-up findings (2026-08-13, not #546)

Found during this host pass and fixed as a separate commit. Run these after R9. R10 needs `sudo` to
read and write the `nat` table, and R11 needs the Horizon deployment from 1.2.

### R10 — Metadata DNAT rules are reclaimed, not accumulated (finding 1)

One rule per tenant bridge redirects `169.254.169.254:80` to the host metadata service. They were
written by the DHCP apply path and removed **only** by the pre-delete purge, so every other route —
`helm uninstall` without the hook, a recreated Postgres volume, a redeploy, an ordinary network
delete — leaked one. On a host redeployed a few times in an afternoon: 101 rules, 98 of them naming
bridges that no longer existed.

Stale rules are inert *until* a later network draws a UUID sharing its first 8 hex characters with a
deleted one and so reuses the bridge name. `PREROUTING` is ordered and the stale rule is earlier, so
it wins: that network's guests are sent to a gateway that no longer exists. Metadata breaks, nothing
logs an error. **R10c is that case**, and it is the reason the reconciler compares destinations rather
than merely asking whether each rule's bridge is still alive.

#### R10a — The census, and reclaim on delete

```bash
echo "total: $(sudo iptables -t nat -S PREROUTING | grep -c '169.254.169.254')"
sudo iptables -t nat -S PREROUTING | grep '169.254.169.254' | while read -r rule; do br=$(printf '%s\n' "$rule" | grep -o 'br-[0-9a-f]*' | head -1); ip link show "$br" >/dev/null 2>&1 && echo "LIVE  $br" || echo "STALE $br"; done
export NET3=$(openstack network create rt-net-md -f value -c id)
openstack subnet create rt-sub-md --network "$NET3" --subnet-range 10.244.30.0/24 >/dev/null
export BR3=br-$(printf '%s' "$NET3" | cut -c1-8)
sleep 3
echo "after create: $(sudo iptables -t nat -S PREROUTING | grep -c "$BR3") rule(s) for $BR3"
openstack subnet delete rt-sub-md && openstack network delete rt-net-md && echo "deleted"
sleep 3
echo "after delete: $(sudo iptables -t nat -S PREROUTING | grep -c "$BR3") rule(s) for $BR3"
```

**Expect:** the census prints only `LIVE` lines — on a host carrying leaks from before this fix the
first reconcile pass after deploying this build will already have cleared them, so a `STALE` line here
means the prune is not running. Then `after create` is **1** and `after delete` is **0**, with no wait
for the reconciler: both delete paths remove the rule at source.

`grep -c` prints `0` and exits non-zero when nothing matches, which is fine inside `$(…)` — the count
is what is read, not the exit status.

#### R10b — The reconciler reclaims a rule the delete paths never saw

This is the case that matters on a real host: iptables outlives the pod, the release and the database,
so a rule can survive with nothing left to name it. A hand-written rule for a bridge that does not
exist stands in for one whose network row is long gone.

```bash
sudo iptables -t nat -A PREROUTING -d 169.254.169.254/32 -i br-deadbeef -p tcp --dport 80 -j DNAT --to-destination 10.99.99.1:8775
sudo iptables -t nat -A PREROUTING -d 169.254.169.254/32 -i br-0123456789ab -p tcp --dport 80 -j DNAT --to-destination 10.99.99.2:8775
echo "planted, waiting 40s for a reconcile pass"
sleep 40
echo "ours (br-deadbeef):   $(sudo iptables -t nat -S PREROUTING | grep -c 'br-deadbeef')"
echo "decoy (docker shape): $(sudo iptables -t nat -S PREROUTING | grep -c 'br-0123456789ab')"
kubectl -n o3k logs deploy/o3k-o3k | grep 'pruned stale metadata DNAT' | tail -2
```

**Expect:** `ours` is **0** — the reconciler reclaimed it — and `decoy` is **1**. The log line reports
the count it removed.

The decoy is the important half. `br-0123456789ab` is a **docker**-shaped bridge name (12 hex, not 8),
so it fails the `isManagedBridge` check even though every other element of the rule matches ours. It
must survive: `nat/PREROUTING` is shared with k3s/kube-proxy, docker and o3k's own floating-IP DNAT,
and a sweep that deleted a foreign rule would be a far worse defect than the leak it fixes. If the
decoy disappears, **stop** — the match has been widened. Remove it by hand when done:

```bash
sudo iptables -t nat -D PREROUTING -d 169.254.169.254/32 -i br-0123456789ab -p tcp --dport 80 -j DNAT --to-destination 10.99.99.2:8775
```

#### R10c — A live bridge pointing at the wrong gateway is corrected

The collision case, reproduced deterministically: a rule for a bridge that **does** exist, pointing
somewhere it should not, inserted at position 1 so it precedes the real one exactly as an older rule
would. A sweep keyed on "does this interface still exist" keeps this forever.

`$NET` and its `10.244.10.0/24` subnet come from 1.3, so the correct destination is `10.244.10.1:8775`.

```bash
export BR=br-$(printf '%s' "$NET" | cut -c1-8)
sudo iptables -t nat -I PREROUTING 1 -d 169.254.169.254/32 -i "$BR" -p tcp --dport 80 -j DNAT --to-destination 10.99.99.3:8775
echo "--- immediately after planting, in chain order ---"
sudo iptables -t nat -S PREROUTING | grep "$BR"
echo "waiting 40s for a reconcile pass"
sleep 40
echo "--- after the prune ---"
sudo iptables -t nat -S PREROUTING | grep "$BR"
```

**Expect:** two rules for `$BR` immediately after planting, the bogus `10.99.99.3:8775` one **first**.
After the pass exactly **one** remains, and it is the `10.244.10.1:8775` one. A run that leaves both,
or leaves the wrong one, is the 1.4a defect still present.

If R3's guest is still running, `openstack console log show rt-guest | tail` between the two states is
worth a glance: while the bogus rule is first, a fresh metadata fetch from that guest has nowhere to
go. Nothing logs an error on either side — which is the point.

### R11 — Horizon's admin Project column shows the owner (finding 2)

The API returned the right owner all along; Horizon blanked the column because upstream assigns it
inside a block guarded by `is_dhcp_agent_scheduler_supported`, and O3K did not advertise that alias.
The same block fetches each network's agents, so the alias without the endpoint would have traded a
blank column for an error pop-up per row — R11 checks both, then the page.

`HOST_TESTING.md` S13 asserts the API contract in full. This step is the half that needs a browser.

```bash
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:9696/v2.0/extensions" | jq -r '[.extensions[].alias] | sort | join(" ")'
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:9696/v2.0/networks/$NET/dhcp-agents" | jq -c '[(.agents|length), (.agents|type)]'
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:9696/v2.0/networks" | jq -c '[.networks[] | {name, tenant_id}]'
echo "Horizon: http://$NODE_IP:30080/dashboard/"
```

**Expect:** `dhcp_agent_scheduler` in the alias list, `[0,"array"]` from the endpoint, and correct
`tenant_id` values.

Then in the browser as `admin` / `secret`: **Admin → Network → Networks**. Every row shows its owning
project in the **Project** column and `0` in **DHCP Agents** — not `-`, and **no error pop-up**. The
pop-up is the failure mode to watch for: it means the alias is advertised and the endpoint is not.

`rt-b11` from R7 should show `rt-other` here rather than `default`, which is the same ownership claim
R7 reads from the API and the database — the column agreeing with them is the point of this step, and
R7 remains the assertion.

---

## 3. Teardown steps

Removes the fixtures, both deployments, and both clusters' feature state. The Postgres container and
`/var/lib/o3k` are left alone — they are shared with other worktrees.

```bash
export KUBECONFIG=~/.kube/k3s.yaml
openstack server delete rt-guest rt-guest2 2>/dev/null || true
openstack floating ip list -f value -c ID | xargs -r openstack floating ip delete 2>/dev/null || true
openstack router unset --external-gateway rt-router 2>/dev/null || true
openstack router remove subnet rt-router rt-sub 2>/dev/null || true
openstack router delete rt-router rt-router-nogw 2>/dev/null || true
openstack port delete rt-p1 2>/dev/null || true
openstack volume delete rt-vol 2>/dev/null || true
openstack subnet delete rt-sub rt-sub-nogw rt-ext-sub rt-sub-md 2>/dev/null || true
openstack network delete rt-net rt-net-nogw rt-ext rt-b11 rt-net-md 2>/dev/null || true
openstack image delete cirros 2>/dev/null || true
openstack project delete rt-other 2>/dev/null || true
helm uninstall horizon -n o3k 2>/dev/null || true
helm uninstall o3k -n o3k 2>/dev/null || true
kubectl delete ns o3k --ignore-not-found
for t in $(ip -br link | grep -o "^tap-[0-9a-f]*"); do ip -o link show "$t" | grep -q "master " || sudo ip link delete "$t"; done
ip -br link | grep -E '^(br-|tap-)' || echo "no o3k interfaces left"
sudo iptables -t nat -D PREROUTING -d 169.254.169.254/32 -i br-0123456789ab -p tcp --dport 80 -j DNAT --to-destination 10.99.99.2:8775 2>/dev/null || true
echo "metadata DNAT rules left: $(sudo iptables -t nat -S PREROUTING | grep -c '169.254.169.254')"
unset KUBECONFIG
deployments/rook/setup-rook-rgw.sh --delete 2>/dev/null || true
rm -f /tmp/o3k-spec024.tar /tmp/cirros.img /tmp/rgw.env
```

**Pass =** `kubectl get ns` no longer lists `o3k`, `minikube profile list` no longer lists `rook`, no
`br-*` or `tap-*` interface remains, and **`metadata DNAT rules left: 0`**.

The TAP sweep is not cosmetic. Bridges and TAPs are **host kernel objects**: they outlive the pod, the
Helm release and the namespace, so nothing in `helm uninstall` removes them and they accumulate across
redeploys. The rule is "a TAP with no `master` is not attached to any bridge, so no live port owns it";
a TAP for a running guest has a master and is left alone. `sudo` is required — they are root-owned. k3s
pod networking uses `veth*` inside the `cni-*` namespaces and is untouched.

The explicit `-D` before it removes **R10b's decoy** — the deliberately foreign, docker-shaped rule
that o3k must never touch. It is only reachable by hand, so leaving it would make the count below read
`1` and look like a leak when it is the opposite.

The DNAT count is the same class of check, and the one the 2026-08-13 follow-up added. It is `0`
because `helm uninstall o3k` fires the SPEC-010 `pre-delete` purge job
(`charts/o3k/templates/teardown-purge-job.yaml` runs `o3k cleanup`), whose network purge now sweeps
every rule matching the shape it writes rather than enumerating one per surviving network row — so it
also reclaims rules whose rows are already gone. A non-zero count means the hook did not run — check
`kubectl -n o3k get jobs` before the namespace goes — and these remove what is left by hand:

```bash
sudo iptables -t nat -S PREROUTING | grep '169.254.169.254' | while read -r rule; do br=$(printf '%s\n' "$rule" | grep -o 'br-[0-9a-f]*' | head -1); ip link show "$br" >/dev/null 2>&1 || sudo iptables -t nat ${=rule/-A/-D}; done
sudo iptables -t nat -S PREROUTING | grep -c '169.254.169.254'
```

Dry-run it first by swapping `sudo iptables -t nat` for `echo iptables -t nat`; expect several
separate words starting `-D PREROUTING`, not one long string. The `${=rule…}` is load-bearing: the `=`
forces word splitting, which zsh does **not** do on an unquoted expansion the way bash does, and
without it `iptables` receives the whole rule as a single argument and fails with *"chain name … too
long"*. This only deletes rules whose **bridge is gone**, so it is safe to run with the stack up.

**Do not** reach for `iptables-save | grep -v … | iptables-restore`: k3s/kube-proxy keeps its rules in
the same table and a restore would clobber them.

If you removed k3s entirely instead of uninstalling the release, the count will read `0` for an
unrelated reason — `k3s-killall.sh` flushes iptables wholesale — so the check only means something on
a teardown that went through `helm uninstall`.

---

## Troubleshooting

**The o3k pod CrashLoopBackOffs on `bind: address already in use`.** It runs `hostNetwork`, so it
competes with anything already on the node's ports — usually a leftover container process. Find it
with `pgrep -x o3k` (exact name; `pgrep -f` will match your own shell) and stop it. A process left by
a removed container runs as root and may need `sudo kill`.

**`openstack image create` hangs or the image lands `queued`.** Glance cannot reach RGW. Re-check
`$RGW_URL` from the k3s host (`curl -s -o /dev/null -w '%{http_code}' $RGW_URL`) — both clusters are
libvirt VMs on this host and should route directly.

**Ceph never reaches `HEALTH_OK`.** A stale `/var/lib/rook` wedges cephx. Always run
`setup-rook-rgw.sh --delete` before a fresh bring-up, as 1.1 does.

**The guest is `ACTIVE` with `addresses: {}`.** Port allocation failed and Nova booted anyway — that
is the Nova-side half, filed as **B13 in #547**, not a Neutron defect. Check the o3k logs for
`network … not found` around the boot.
