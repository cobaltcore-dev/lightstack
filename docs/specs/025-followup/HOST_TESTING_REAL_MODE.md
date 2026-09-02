# Host testing guide — SPEC-025 follow-ups (real mode)

Validates all seven stories of `docs/specs/025-followup/` against a **real-mode** deployment —
libvirt/KVM guests, real Linux networking, node-local storage. No stub mode anywhere in this file.

That is deliberate rather than thorough-for-its-own-sake: **five of the seven defects were found by a
human on a host and none by a contract suite**, because every one of them answers with a success
status. The Go suites (72 tests, 226 failure paths — see `BASELINE.md`) prove the logic; this proves
the behaviour a client actually sees.

Single Ubuntu host, k3s, libvirt/KVM, node-local storage — no Ceph, no multi-node.

**Run all commands from the repo root.** Sections 1 and 2 run in **one shell** and carry their
exports forward; only R6 uses a second shell.

## Validates

| Acceptance criterion | Proved by |
|---|---|
| FR-001/FR-003/FR-009 — a filter narrows; a non-matching value returns nothing | R1a |
| FR-003a — `name` matches as a regular expression | R1b |
| FR-002 — both listings honour the same filters | R1c |
| FR-004/FR-004a — an unrecordable attribute is refused naming it; a benign extra is ignored | R2 |
| FR-006/FR-006a — a filter narrows and never widens, administrators included | R2c |
| FR-010/FR-016/FR-017 — `image set` writes what it reports | R3a |
| FR-011/FR-012/FR-013 — `remove`, unknown ops and unwritable paths are refused naming them | R3b |
| FR-014/FR-015 — a patch is atomic; a zero-row write is 404 | R3c |
| FR-018/FR-020/FR-021/FR-024 — volume creation refused at the limit, nothing left behind | R4a |
| FR-019/FR-022 — the compute boot path is enforced and leaves no server implying a volume | R4b |
| FR-028 — extension charges the delta, not the resulting size | R4c |
| FR-019/FR-019a — a transfer refused for quota stays pending and is retryable | R4d |
| FR-028a — snapshots and backups are enforced | R4e |
| FR-026/FR-026a — snapshot size counts toward `gigabytes`, in every view | R5 |
| FR-023 — two concurrent creates against one allowance yield one volume | R4f |
| FR-029a — a denial is recorded for the operator | R4g |
| FR-030/FR-031/FR-032 — the catalog names the host the client addressed | R6 |
| FR-035/FR-036/FR-037 — `restore` answers as upstream (409/404); deletion is unchanged | R7 |
| FR-038/FR-041 — the applied version is reported; out-of-range is refused | R8a |
| FR-039 — the 2.45 `image_id` gate | R8b |
| FR-040a — no header still resolves to the maximum | R8c |
| FR-045/FR-046/FR-047 — one address query per listing, identical output | R9a |
| FR-003b — `ip` is honoured, not ignored | R9b |

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
openstack --version
```

**Pass =** no `MISSING:` line, the k3s node is `Ready`, `virsh` succeeds (an empty list is fine), and
every version line prints. Keep `KUBECONFIG` exported — every later `kubectl` and `helm` call needs it.

> **Real mode needs real root.** Rootless Podman cannot join the host network namespace, so a
> rootless engine will deploy and then fail every networking assertion. If `docker info` reports
> `rootless`, switch to a rootful engine before starting.

---

## 1. Setup steps

### 1.1 Deploy in real mode, with the fixtures the tests need

One block, with the same **mode gate** SPEC-025's runbook uses and for the same reason: a *failed*
deploy cascades loudly, but a deploy that succeeds in the **wrong mode** completes setup perfectly and
lets every test below pass without proving anything about real mode. The gate makes that impossible —
if the deployed config does not read real, no fixture is created and every step fails on a missing
variable instead.

`endpointHost` must be the **node IP**. With `hostNetwork` Lightstack listens on the node, so one value is
reachable from both the pod network and your host CLI. `NODE_IP` comes from the **default route**, not
from `hostname -I | awk '{print $1}'` — that form returns addresses in kernel interface order and this
host has several (`enp1s0`, `virbr0`, `flannel.1`, `cni0`), so which lands first is luck rather than
routability.

**The quota limits are set deliberately low.** US3 is the largest story here and its every assertion is
a refusal, so the fixtures put the project close to its ceiling on purpose.

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
docker build -f build/package/Dockerfile -t docker.io/library/lightstack:followup .
rm -f /tmp/lightstack-followup.tar
docker save docker.io/library/lightstack:followup -o /tmp/lightstack-followup.tar
sudo k3s ctr images import /tmp/lightstack-followup.tar

echo "--- deploy, real mode ---"
helm upgrade --install lightstack charts/lightstack --namespace lightstack --create-namespace --set image.repository=lightstack --set image.tag=followup --set image.pullPolicy=Never --set deploymentMode=single-node --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true --set nova.libvirtMode=real --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables --set glance.storageMode=local --set cinder.storageMode=local --set endpointHost=$NODE_IP --set secrets.jwtSecret=changeme --set secrets.adminPassword=secret --set database.url='postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable'
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s
kubectl -n lightstack get pods

echo "=== the gate: every mode must read real ==="
MODES=$(kubectl -n lightstack get secret lightstack-lightstack-config -o jsonpath='{.data.lightstack\.yaml}' | base64 -d | grep -E "_mode:")
echo "$MODES"

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

  export T=$(openstack token issue -f value -c id)
  export PROJ=$(openstack project show default -f value -c id)
  echo "token acquired for project $PROJ"

  echo "=== image ==="
  test -s /tmp/cirros.img || curl -sL -o /tmp/cirros.img http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
  openstack image show cirros >/dev/null 2>&1 || openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img cirros >/dev/null
  export IMG=$(openstack image show cirros -f value -c id)
  openstack image show cirros -f value -c name -c status -c size
  echo "--- the raw base must exist on the node: the boot-from-volume step needs it ---"
  sudo ls -la /var/lib/lightstack/images

  echo "=== network and subnet ==="
  openstack network show fu-net >/dev/null 2>&1 || openstack network create fu-net >/dev/null
  openstack subnet show fu-sub >/dev/null 2>&1 || openstack subnet create fu-sub --network fu-net --subnet-range 10.244.20.0/24 >/dev/null
  export NET=$(openstack network show fu-net -f value -c id)

  echo "=== three booted guests, differing in every filterable attribute ==="
  openstack server create --flavor m1.small --image cirros --network fu-net alpha >/dev/null
  openstack server create --flavor m1.tiny --image cirros --network fu-net beta >/dev/null
  openstack server create --flavor m1.small --image cirros --network fu-net gamma >/dev/null
  sleep 20
  export ALPHA=$(openstack server show alpha -f value -c id)
  export BETA=$(openstack server show beta -f value -c id)
  openstack server list -f value -c Name -c Status -c Networks
  sudo virsh -c qemu:///system list --all

  echo "=== quota fixtures, set LOW so the refusals are reachable ==="
  openstack quota set --volumes 2 --gigabytes 12 --snapshots 1 --backups 1 default
  openstack quota show default -f value | grep -E '^(volumes|gigabytes|snapshots|backups) ' | sort

  echo "=== every variable the test steps rely on ==="
  for v in NODE_IP T PROJ IMG NET ALPHA BETA; do printf '%s=%s\n' "$v" "${(P)v}"; done
else
  echo "STOP: this deployment is not real mode. No fixtures were created — the image, network and"
  echo "      guests are all absent, so every test step will fail on a missing variable rather than"
  echo "      pass against a stub. Fix the --set flags above and re-run this block."
fi
```

**Pass =** `ports free`; the pod `Running` and not `CrashLoopBackOff`; the read-back showing
`libvirt_mode: "real"`, `networking_mode: "iptables"`, `security_group_mode: "iptables"` and
`storage_mode: "local"` twice; a token; `cirros` **`active`** with a non-zero size; an
`image-<uuid>.raw` under `/var/lib/lightstack/images`; `alpha`, `beta` and `gamma` all **`ACTIVE`** with
addresses from `10.244.20.0/24`; three running domains; the quota read-back printing four
`<resource> <limit>` rows reading `backups 1`, `gigabytes 12`, `snapshots 1`, `volumes 2`; and the
final loop printing **seven non-empty values**.

> **The read-back is `grep`ed out of the table, not selected with `-c`.** `openstack quota show`
> emits `Resource` and `Limit` columns, so `-c volumes` answers
> `No recognized column names in ['volumes']` — an earlier form of this line did exactly that, and the
> Pass criterion above asked for a `2 / 12 / 1 / 1` that could never print. The same mistake appeared
> independently in R5, where it meant the step never read the number it existed to check.

**An empty variable in that last loop is the thing to stop on.** A `STOP:` line means the gate tripped
and nothing after it ran.

### 1.2 A second project, for the scoping assertions

R2c needs a project the admin does not belong to, so that "a filter cannot widen scope" is testable
rather than assumed.

```bash
openstack project show fu-other >/dev/null 2>&1 || openstack project create fu-other >/dev/null
export OTHER=$(openstack project show fu-other -f value -c id)
echo "other project: $OTHER"
openstack server list --all-projects -f value -c Name | sort | tr '\n' ' '
echo ""
```

**Pass =** a non-empty `$OTHER`, and the cross-tenant listing showing `alpha beta gamma`.

---

## 2. Test steps

**One block per step.** Each `### R<n>` below is a single paste; the `=== R<n>x ===` markers inside
name the sub-assertions so the output stays navigable, and the expectation table after each block maps
every marker to what it should print. No step has `set -e`, so an early failure does not hide the
later parts — you see the whole picture from one paste.

### R1 — A filter narrows, and a non-matching value returns nothing (FR-001, FR-002, FR-003, FR-003a, FR-009)

This is the defect's own reproduction. Before this feature `--name` returned the whole project, so
reading `[0]` gave an arbitrary server — which is exactly how the bug was found during SPEC-025's host
pass, where `.154` was read as `probe`'s address and belonged to `drift`.

```bash
echo "=== R1a: the narrowing, and the trap that hid it ==="
echo "--- name: one match ---"
openstack server list --name alpha -f value -c Name
echo "--- name: no match (MUST print nothing) ---"
openstack server list --name doesnotexist -f value -c Name
echo "--- status ---"
openstack server list --status ACTIVE -f value -c Name | sort | tr '\n' ' '
echo ""
echo "--- the trap: this line was a wrong answer before the fix ---"
openstack server list --name alpha -f json | jq -r '.[0].Name + " -> " + (.[0].Networks | tostring)'

echo "=== R1b: name is a regular expression, as upstream matches it ==="
echo -n "anchored prefix (expect alpha only): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?name=^alpha$" | jq -r '[.servers[].name] | join(" ")'
echo -n "class match (expect alpha and beta): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?name=^(alpha|beta)$" | jq -r '[.servers[].name] | sort | join(" ")'
echo -n "no match (expect an empty array): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?name=^zzz" | jq -c '.servers'
echo -n "invalid pattern (expect 400, not 500): "
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?name=%5B"

echo "=== R1c: both listings agree (FR-002) ==="
for q in "" "?name=alpha" "?status=ACTIVE" "?name=doesnotexist"; do
  B=$(curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers$q" | jq -r '[.servers[].name] | sort | join(",")')
  D=$(curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers/detail$q" | jq -r '[.servers[].name] | sort | join(",")')
  printf 'query %-22s brief=[%s] detail=[%s] %s\n' "'$q'" "$B" "$D" "$([ "$B" = "$D" ] && echo SAME || echo DIFFER)"
done
```

**Expect:**

| Marker | Expected |
|---|---|
| R1a name: one match | `alpha` alone |
| R1a name: no match | **no output at all** |
| R1a status | `alpha beta gamma` |
| R1a the trap | **`alpha`** with an address from `10.244.20.0/24` |
| R1b anchored / class / no match / invalid | `alpha`; `alpha beta`; `[]`; **`400`** |
| R1c | four lines, every one `SAME` |

Before this feature the trap line named whichever server sorted first — a confident, meaningless
answer. A `500` on R1b's last line means the pattern reached PostgreSQL unguarded. `DIFFER` on any
R1c line means the two listings honour different filters, which is the shape a client cannot detect.

### R2 — An unrecordable attribute is refused; a benign extra is ignored (FR-004, FR-004a, FR-006, FR-006a)

```bash
echo "=== R2a: the refusals ==="
for f in host hostname availability_zone tags; do
  code=$(curl -s -o /tmp/r2a -w '%{http_code}' -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?$f=x")
  printf '%-18s status=%s  %s\n' "$f" "$code" "$(jq -r '.badRequest.message // .' /tmp/r2a)"
done

echo "=== R2b: a parameter that is neither honoured nor refused stays ignored ==="
for pr in "sort_key=created_at" "sort_dir=desc" "deleted=false" "some_future_param=x"; do
  printf '%-26s count=%s\n' "$pr" "$(curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?$pr" | jq '.servers | length')"
done

echo "=== R2c: a filter narrows and never widens ==="
echo -n "admin filtering by another project, WITHOUT all_tenants (expect 0): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?project_id=$OTHER" | jq '.servers | length'
echo -n "admin filtering by their OWN project, without all_tenants (expect 3): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?project_id=$PROJ" | jq '.servers | length'
echo -n "admin WITH all_tenants, filtered to the other project (expect 0, it has no servers): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?all_tenants=1&project_id=$OTHER" | jq '.servers | length'
echo -n "admin WITH all_tenants, filtered to their OWN project (expect 3): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?all_tenants=1&project_id=$PROJ" | jq '.servers | length'
```

**Expect:**

| Marker | Expected |
|---|---|
| R2a | four lines, each `status=400`, each message naming its filter (`Unsupported filter: host`, …) |
| R2b | `count=3` on every line |
| R2c | `0`, `3`, `0`, `3` |

R2a is a **deliberate divergence from upstream**, which honours `host` for administrators and
*silently ignores* it for everyone else — accepted so the failure is visible, and recorded as entry 4
in the constitution's Known Divergences register. A `0` or a 400 in R2b would mean the refusal is no
longer bounded to the enumerated set, which would newly break every client that sends a benign extra.
R2c's first line is the point: scope ∩ filter, so an administrator asking for another project's
servers without asking to see other projects gets nothing.

> **Two details in this block exist because of the 2026-09-02 run.**
>
> **The body goes to a file and the status stays on stdout.** An earlier form used
> `curl -w ' status=%{http_code}'` piped into `jq`. That sends the status into `jq`'s *input*, where it
> is not valid JSON: `jq` printed the message, failed on the trailing text, and `2>/dev/null` swallowed
> the failure — so the step printed four correct-looking messages and asserted **nothing** about the
> status code.
>
> **R2c's fourth line is not padding.** Lines 1–3 are all *negative* assertions, each passing when a
> listing comes back empty — so all three are satisfied by a `project_id` predicate that is silently
> broken just as well as by one that works. The fourth is the positive counterpart. Without it R2c
> cannot tell a working filter from one that empties every result, which is exactly how the `ip`
> filter's subquery bug survived its own negative tests during implementation.

### R3 — `image set` writes what it reports (FR-010 – FR-017)

`openstack image set --min-disk` sends `add`, not `replace`, so before this feature it reported
success and changed nothing.

```bash
echo "=== R3a: the CLI path, which is the one that matters ==="
echo -n "min_disk before: "
openstack image show cirros -f value -c min_disk
openstack image set --min-disk 10 cirros && echo "image set: exit 0"
echo -n "min_disk after (expect 10): "
openstack image show cirros -f value -c min_disk
echo -n "min_ram via the same path (expect 64): "
openstack image set --min-ram 64 cirros && openstack image show cirros -f value -c min_ram

echo "=== R3b: the refusals, each naming what it refused ==="
echo -n "remove a writable property (expect 403): "
curl -s -o /tmp/r3b1 -w '%{http_code} ' -X PATCH -H "X-Auth-Token: $T" -H 'Content-Type: application/openstack-images-v2.1-json-patch' -d '[{"op":"remove","path":"/min_disk"}]' "http://$NODE_IP:9292/v2/images/$IMG"
jq -r '.forbidden.message // .' /tmp/r3b1
echo -n "an operation the service does not implement (expect 400): "
curl -s -o /tmp/r3b2 -w '%{http_code} ' -X PATCH -H "X-Auth-Token: $T" -H 'Content-Type: application/openstack-images-v2.1-json-patch' -d '[{"op":"move","path":"/min_disk","value":1}]' "http://$NODE_IP:9292/v2/images/$IMG"
jq -r '.badRequest.message // .' /tmp/r3b2
echo -n "a path the service cannot write (expect 403): "
curl -s -o /tmp/r3b3 -w '%{http_code} ' -X PATCH -H "X-Auth-Token: $T" -H 'Content-Type: application/openstack-images-v2.1-json-patch' -d '[{"op":"replace","path":"/status","value":"queued"}]' "http://$NODE_IP:9292/v2/images/$IMG"
jq -r '.forbidden.message // .' /tmp/r3b3
echo -n "an arbitrary custom property, silently dropped before (expect 403): "
curl -s -o /dev/null -w '%{http_code}\n' -X PATCH -H "X-Auth-Token: $T" -H 'Content-Type: application/openstack-images-v2.1-json-patch' -d '[{"op":"replace","path":"/hw_disk_bus","value":"scsi"}]' "http://$NODE_IP:9292/v2/images/$IMG"
echo -n "min_disk unchanged by all four refusals (expect 10): "
openstack image show cirros -f value -c min_disk
echo -n "status unchanged (expect active): "
openstack image show cirros -f value -c status

echo "=== R3c: atomicity and the zero-row write ==="
echo -n "a document whose second member cannot be applied (expect 403, from the remove): "
curl -s -o /dev/null -w '%{http_code}\n' -X PATCH -H "X-Auth-Token: $T" -H 'Content-Type: application/openstack-images-v2.1-json-patch' -d '[{"op":"replace","path":"/min_disk","value":42},{"op":"remove","path":"/name"}]' "http://$NODE_IP:9292/v2/images/$IMG"
echo -n "min_disk after it (expect 10, NOT 42): "
openstack image show cirros -f value -c min_disk
echo -n "an image that does not exist (expect 404): "
curl -s -o /dev/null -w '%{http_code}\n' -X PATCH -H "X-Auth-Token: $T" -H 'Content-Type: application/openstack-images-v2.1-json-patch' -d '[{"op":"replace","path":"/min_disk","value":5}]' "http://$NODE_IP:9292/v2/images/00000000-0000-0000-0000-0000000000ff"
```

**Expect:**

| Marker | Expected |
|---|---|
| R3a | `0`, then `image set: exit 0`, then **`10`**, then **`64`** |
| R3b | `403`, `400`, `403`, `403` — each with a message naming the path or operation — then `10` and `active` |
| R3c | **`403`**, then **`10`** (not 42), then **`404`** |

A `0` after R3a's set is the F8 defect: the CLI reports success, the value never changes, nothing is
logged at either end. A `200` anywhere in R3b is the discard defect.

> **R3c's first line expects 403, and it did not on the first run.** It answered **400** — from the
> *first* member, not the `remove` — because `coerceImagePatchValue` accepted only `float64` while
> `internal/middleware` sets `binding.EnableDecoderUseNumber`, making every JSON number arrive as
> `json.Number`. The atomicity path was never reached, and R3c's third line answered 400 instead of
> 404 for the same reason. Fixed; see `BASELINE.md`. **The unit tests did not catch it** because the
> harness never imported the package whose `init()` sets that flag, so they bound numbers as
> `float64` and asserted a shape production never produces.

### R4 — Block-storage quota is enforced on every route (FR-018 – FR-029a)

Nine call sites consume quota. This step walks the ones a client can reach. It is the longest block in
the runbook because it juggles limits between assertions — each `openstack quota set` is part of the
fixture for the assertion that follows it.

```bash
echo "=== R4a: the volume API, and nothing left behind ==="
openstack quota set --volumes 2 --gigabytes 12 default
openstack volume create --size 5 q-one >/dev/null && echo "first 5G: created"
openstack volume create --size 5 q-two >/dev/null && echo "second 5G: created"
echo -n "third volume, over the 2-volume limit (expect 413): "
curl -s -o /tmp/r4a -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"volume":{"size":1}}' "http://$NODE_IP:8776/v3/$PROJ/volumes"
jq -r '.overLimit.message // .' /tmp/r4a
echo -n "volume count after the refusal (expect 2): "
openstack volume list -f value -c ID | wc -l

echo "=== R4b: the compute boot path, which no handler-level check can see ==="
openstack quota set --volumes 5 --gigabytes 40 default
echo -n "the image's min_disk, which the boot volume must not be smaller than: "
openstack image show cirros -f value -c min_disk
echo -n "boot from a new volume within quota (expect a server id): "
openstack server create --flavor m1.small --image cirros --network fu-net --boot-from-volume 10 bootvol -f value -c id
sleep 15
openstack volume list -f value -c Name -c Status -c Size
openstack quota set --gigabytes 20 default
echo -n "boot from a new volume that does not fit the gigabytes allowance (expect 413): "
curl -s -o /tmp/r4b -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"toobig\",\"flavorRef\":\"m1.small\",\"networks\":[{\"uuid\":\"$NET\"}],\"block_device_mapping_v2\":[{\"boot_index\":0,\"source_type\":\"image\",\"destination_type\":\"volume\",\"uuid\":\"$IMG\",\"volume_size\":50,\"delete_on_termination\":true}]}}" "http://$NODE_IP:8774/v2.1/servers"
jq -r '.overLimit.message // .' /tmp/r4b
echo -n "no server named toobig (expect 0): "
openstack server list --name toobig -f value -c ID | wc -l
echo -n "no orphan volume for it (expect 0): "
openstack volume list -f value -c Name | grep -c 'toobig' | head -1
echo "--- clean up bootvol AND its volume so the later parts start from a known usage ---"
openstack server delete bootvol 2>/dev/null || true
sleep 8
openstack volume delete bootvol-boot 2>/dev/null || true
sleep 3
echo -n "volumes remaining (expect q-one and q-two only): "
openstack volume list -f value -c Name | sort | tr '\n' ' '
echo ""

echo "=== R4c: extension charges the delta, not the resulting size ==="
openstack volume list -f value -c Name -c Size
export GBUSED=$(curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8776/v3/$PROJ/os-quota-sets/$PROJ?usage=True" | jq -r '.quota_set.gigabytes.in_use')
openstack quota set --gigabytes $((GBUSED + 2)) default
echo "gigabytes in use: $GBUSED; limit set to $((GBUSED + 2)) so a delta of exactly 2 fits and nothing more does"
echo -n "extend q-one 5G -> 7G, a delta of 2 (expect ok): "
openstack volume set --size 7 q-one && echo ok
echo -n "size now (expect 7): "
openstack volume show q-one -f value -c size
echo -n "extend again, past the allowance (expect 413): "
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"os-extend":{"new_size":40}}' "http://$NODE_IP:8776/v3/$PROJ/volumes/$(openstack volume show q-one -f value -c id)/action"
echo -n "size unchanged after the refusal (expect 7): "
openstack volume show q-one -f value -c size

echo "=== R4d: a transfer refused for quota stays pending (FR-019a) ==="
openstack quota set --volumes 9 --gigabytes 200 default
export XVOL=$(openstack volume create --size 1 q-xfer -f value -c id)
curl -s -o /tmp/r4d -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"transfer\":{\"volume_id\":\"$XVOL\",\"name\":\"q-xfer\"}}" "http://$NODE_IP:8776/v3/$PROJ/os-volume-transfer"
export XFER=$(jq -r '.transfer.id' /tmp/r4d)
export XKEY=$(jq -r '.transfer.auth_key' /tmp/r4d)
echo "transfer $XFER created for volume $XVOL"
echo -n "volume status while the transfer is pending (expect awaiting-transfer): "
openstack volume show $XVOL -f value -c status
echo -n "auth key captured (expect 32 hex chars, NOT null): "
echo "${#XKEY} chars: $XKEY"
openstack quota set --volumes 1 default
echo -n "accept while over the limit (expect 413): "
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"accept\":{\"auth_key\":\"$XKEY\"}}" "http://$NODE_IP:8776/v3/$PROJ/os-volume-transfer/$XFER/accept"
echo -n "the transfer is still pending and retryable (expect its id): "
openstack volume transfer request show $XFER -f value -c id
openstack quota set --volumes 9 default
echo -n "accept after raising the limit (expect 202): "
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"accept\":{\"auth_key\":\"$XKEY\"}}" "http://$NODE_IP:8776/v3/$PROJ/os-volume-transfer/$XFER/accept"
echo -n "volume status after the accept (expect available): "
openstack volume show $XVOL -f value -c status

echo "=== R4e: snapshots and backups ==="
openstack quota set --snapshots 1 --gigabytes 200 default
openstack volume snapshot create --volume q-two q-snap1 >/dev/null && echo "first snapshot: created"
echo -n "second snapshot, over the 1-snapshot limit (expect 413): "
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"snapshot\":{\"volume_id\":\"$(openstack volume show q-two -f value -c id)\",\"name\":\"q-snap2\"}}" "http://$NODE_IP:8776/v3/$PROJ/snapshots"
echo -n "snapshot count after the refusal (expect 1): "
openstack volume snapshot list -f value -c ID | wc -l
echo "--- backups: addressed on the FLAT path, see the note below ---"
openstack quota set --backups 1 default
curl -s -o /dev/null -w 'first backup: %{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"backup\":{\"volume_id\":\"$(openstack volume show q-two -f value -c id)\"}}" "http://$NODE_IP:8776/v3/backups"
curl -s -o /dev/null -w 'second backup, over the limit: %{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"backup\":{\"volume_id\":\"$(openstack volume show q-two -f value -c id)\"}}" "http://$NODE_IP:8776/v3/backups"

echo "=== R4f: two concurrent creates against one remaining allowance ==="
openstack quota set --volumes 20 --gigabytes 200 default
export USED=$(openstack volume list -f value -c ID | wc -l)
openstack quota set --volumes $((USED + 1)) default
echo "one allowance remaining; firing two creates at once"
curl -s -o /tmp/c1 -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"volume":{"size":1}}' "http://$NODE_IP:8776/v3/$PROJ/volumes" & curl -s -o /tmp/c2 -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"volume":{"size":1}}' "http://$NODE_IP:8776/v3/$PROJ/volumes"
wait
echo -n "volume count (expect exactly USED+1): "
openstack volume list -f value -c ID | wc -l
echo "expected: $((USED + 1))"

echo "=== R4g: the denial is recorded for the operator (FR-029a) ==="
kubectl -n lightstack logs deploy/lightstack-lightstack | grep '"event":"quota_denied"' | tail -3 | jq -c '{resource, limit, in_use, requested, project_id}'
```

**Expect:**

| Marker | Expected |
|---|---|
| R4a | two `created` lines, **`413`** naming `volumes`, then `2` |
| R4b | the image's `min_disk`; a server id; a `bootvol-boot` volume `in-use` at 10 GiB; **`413`** naming `gigabytes`; then `0`, `0`, and `q-one q-two` remaining |
| R4c | the in-use figure and the derived limit, then `ok`, `7`, **`413`**, `7` |
| R4d | a **32-char** auth key; **`413`**; the transfer id still resolving; then **`202`** |
| R4e | `created`, **`413`**, `1`; then `202` and **`413`** for the backups |
| R4f | one `202` and one `413`, and a final count of exactly `USED+1` |
| R4g | at least one line carrying `resource`, `limit`, `in_use`, `requested` |

A third volume existing in R4a means the refusal leaked one. A **`503`** instead of `413` in R4b would
mean the refusal was reported as a storage outage. Charging the resulting size rather than the delta
would have refused R4c's *first* extension too. R4d's middle line is the whole point: an accept
provisions nothing, so a refusal must lose nothing either. Two `202`s and `USED+2` in R4f is the race —
the check must hold its lock across the insert it authorises, not release it in between. R4g is the one
refusal in this feature that is logged; the filter, patch and microversion refusals are deliberately
**absent** from the log, each being already in the caller's hands.

> **Three details in this block come from the 2026-09-02 run, and two of them were runbook bugs.**
>
> **R4b's boot volume is 10 GiB, not 5.** R3a raises the image's `min_disk` to 10, so a 5 GiB boot
> volume is refused by SPEC-025's size floor (FR-052) — correctly, and with a clear message, but it
> made R4b's *within-quota* case fail for a reason that has nothing to do with quota. The step now
> prints `min_disk` first so the dependency is visible, and 10 is safe whether or not R3 has run.
>
> **R4b deletes the boot VOLUME as well as the server, and R4c calibrates its own limit.** Deleting
> the server does **not** remove `bootvol-boot`: the CLI's `--boot-from-volume` does not request
> delete-on-termination, so the volume is retained — which is *correct*, and is exactly what
> SPEC-025's own runbook asserts ("retained and reusable"). The first version of this cleanup assumed
> otherwise, so the 10 GiB stayed in the usage figure and R4c's hardcoded `--gigabytes 12` refused its
> own *first* extension with a real `413`.
>
> R4c now derives its limit from the **actual** in-use figure (`in_use + 2`), so a delta of exactly 2
> fits and nothing larger does — immune to whatever earlier steps left behind. A hardcoded fixture in
> a merged block is a fixture that depends on every preceding assertion having gone exactly as
> planned, which is not a property worth relying on.
>
> **`auth_key` is captured from CREATE, not from SHOW.** An earlier form read it with
> `openstack volume transfer request show -c auth_key`, which answered `No recognized column names in
> ['auth_key']` — and `$XKEY` was then empty, so the accept was refused **400** for a bad key and
> R4d asserted nothing about quota. That is not a product defect: `create` returns the key and `show`
> deliberately does not, which is exactly upstream's behaviour, since the key is a secret disclosed
> once. The step now reads both fields out of the create response and prints the key's length, so an
> empty one cannot pass silently.
>
> **Why the backups are addressed with `curl` on `/v3/backups` and not with `openstack volume backup
> create`.** The backup routes are registered **only** on the flat path — the project-scoped group
> (`/v3/{project_id}/…`) has volumes, snapshots, transfers, quota-sets, groups and manage, but **not
> backups**. Since the catalog serves a project-scoped volume URL, the CLI resolves
> `/v3/{project}/backups` and gets a **404**. That is a **pre-existing routing gap**, not something
> this feature introduced or fixed — same family as SPEC-020's B1 (`/v3/{project}/groups`). The quota
> enforcement itself is correct and reachable; the route it sits behind is not reachable by the CLI.
> **File it.**

### R5 — Snapshot size counts toward `gigabytes`, in every view (FR-026, FR-026a)

The user-visible number this feature changes. Read it from both surfaces, because they were computed
independently and disagreed until this feature made them share one resolution.

```bash
openstack quota set --volumes 20 --gigabytes 500 --snapshots 20 default
export SVOL=$(openstack volume create --size 3 g-vol -f value -c id)
echo "volume $SVOL created at 3 GiB"
echo -n "quota-set view, gigabytes in_use with the volume only: "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8776/v3/$PROJ/os-quota-sets/$PROJ?usage=True" | jq -r '.quota_set.gigabytes.in_use'
echo -n "limits view, totalGigabytesUsed:                       "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8776/v3/$PROJ/limits" | jq -r '.limits.absolute.totalGigabytesUsed'
openstack volume snapshot create --volume g-vol g-snap >/dev/null && echo "3 GiB snapshot created"
sleep 2
echo -n "quota-set view, gigabytes in_use after the snapshot:   "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8776/v3/$PROJ/os-quota-sets/$PROJ?usage=True" | jq -r '.quota_set.gigabytes.in_use'
echo -n "limits view, totalGigabytesUsed after the snapshot:    "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8776/v3/$PROJ/limits" | jq -r '.limits.absolute.totalGigabytesUsed'
echo -n "snapshot count reported alongside it:                  "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8776/v3/$PROJ/os-quota-sets/$PROJ?usage=True" | jq -r '.quota_set.snapshots.in_use'
```

**Expect:** both figures rise by exactly **3** when the snapshot is created, and **the two views agree
at every point** — four numbers, in two equal pairs, the second pair three higher than the first. The
snapshot count rises by 1 alongside.

Before this feature the quota-set view counted volume size only and the limits view had its own
separate copy of the counting, so a correction to one would not have reached the other. The two
disagreeing is the defect; matching-but-wrong was its predecessor.

> **Both surfaces are read with `curl`, and the fixture raises `--volumes`.** Two runbook bugs from
> the 2026-09-02 run:
>
> **`openstack quota show` reports LIMITS, not usage** — its columns are `Resource` and `Limit`, so
> `-c gigabytes` answered `No recognized column names in ['gigabytes']` and this step never read the
> number it exists to check. Usage comes from `os-quota-sets/{project}?usage=True`, which returns
> `in_use` per resource; Lightstack accepts the `True` spelling python-cinderclient sends. That the *limits*
> half printed `19` while the *quota* half printed an error is what gave it away — a step asserting
> agreement between two views was only ever reading one.
>
> **The fixture must raise `--volumes` too.** R4f deliberately leaves the volume limit at
> `USED + 1`, so R5's volume create was refused `413` — correctly, and for a reason belonging to the
> previous step. Every step's fixture is now self-sufficient rather than inheriting the last one's.

### R6 — The catalog names the host the client addressed (FR-030, FR-031, FR-032)

**Shell T2 (a second terminal)** — because this step **re-exports `OS_AUTH_URL` to the alias**, and
every step after it runs in T1 against the node IP. Run it in T1 and the rest of the session silently
addresses the deployment by a different name than it claims to.

It is not the other way round: the `curl` calls below carry an explicit `Host:` header and use neither
`$T` nor `OS_AUTH_URL`, so T1's environment cannot affect them — this step passes identically in
either shell. **T2 is about what this step leaves behind, not about what it needs.** (An earlier
version of this note had that backwards; the 2026-09-02 run in T1 passed and showed why.)

If you do run it in T1, restore the environment before R7:

```
export OS_AUTH_URL=http://$NODE_IP:35357/v3
```

```bash
export NODE_IP=$(ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')
grep -q 'lightstack-alias' /etc/hosts || echo "$NODE_IP lightstack-alias" | sudo tee -a /etc/hosts >/dev/null
echo "alias resolves to: $(getent hosts lightstack-alias | awk '{print $1}')"
export AUTHBODY='{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","domain":{"name":"Default"},"password":"secret"}}},"scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}'

echo "--- the deployment was configured with endpointHost=$NODE_IP, so the alias is a DIFFERENT name ---"
echo -n "compute endpoint served to a client addressing the alias: "
curl -s -X POST -H 'Content-Type: application/json' -H 'Host: lightstack-alias:35357' -d "$AUTHBODY" "http://$NODE_IP:35357/v3/auth/tokens" | jq -r '[.token.catalog[] | select(.type=="compute") | .endpoints[].url] | join(" ")'
echo -n "endpoints naming the alias (expect >= 5): "
curl -s -X POST -H 'Content-Type: application/json' -H 'Host: lightstack-alias:35357' -d "$AUTHBODY" "http://$NODE_IP:35357/v3/auth/tokens" | jq -r '[.token.catalog[].endpoints[].url] | join(" ")' | grep -o 'lightstack-alias' | wc -l
echo -n "any endpoint with two ports (expect 0): "
curl -s -X POST -H 'Content-Type: application/json' -H 'Host: lightstack-alias:35357' -d "$AUTHBODY" "http://$NODE_IP:35357/v3/auth/tokens" | jq -r '[.token.catalog[].endpoints[].url] | join(" ")' | grep -cE ':[0-9]+:[0-9]+' || echo 0

echo "--- and a full command sequence under the alias, with no override ---"
export OS_AUTH_URL=http://lightstack-alias:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
openstack server list -f value -c Name | sort | tr '\n' ' '
echo ""
openstack flavor list -f value -c Name | head -3
```

**Expect:** the compute endpoint naming **`lightstack-alias:8774`** — the addressed host with compute's own
port; **`>= 5`** endpoints naming the alias; **`0`** endpoints with two ports; and `server list` and
`flavor list` both succeeding under the alias with **no** `OS_*_ENDPOINT_OVERRIDE` set.

Before this feature every endpoint named `$NODE_IP` whatever host the client used, so a client that
could only resolve the alias failed at catalog lookup — before any version document was read, and
therefore before SPEC-025's self-link fix could help. The two-port check is not hypothetical:
substituting a request host that carries its own port into a URL pattern that supplies one produces
`http://lightstack-alias:35357:8774`, an authority nothing resolves.

### R7 — `restore` answers as upstream does, and deletion is unchanged (FR-035, FR-036, FR-037)

Back in **T1**:

```bash
echo -n "restore on a live server (expect 409, upstream's answer): "
curl -s -o /tmp/r7 -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"restore": null}' "http://$NODE_IP:8774/v2.1/servers/$ALPHA/action"
jq -r '.conflictingRequest.message // .badRequest.message // .' /tmp/r7
G=$(openstack server show gamma -f value -c id)
echo -n "deletion is still immediate (expect 0): "
openstack server delete gamma && sleep 5
openstack server list --name gamma -f value -c ID | wc -l
echo -n "restore on the deleted server (expect 404): "
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"restore": null}' "http://$NODE_IP:8774/v2.1/servers/$G/action"
```

**Expect:** **`409`** with `Instance <id> is in an invalid state for 'restore'`, `0` servers named
`gamma`, then **`404`** for the deleted one — what an upstream deployment answers at its default
reclaim interval. A **`400`** `unknown action` is the defect: upstream routes `restore` at every
configuration, so a client written against it never sees that answer for this action.

### R8 — Microversion negotiation (FR-038, FR-039, FR-040a, FR-041)

```bash
echo "=== R8a: the applied version is reported, and out-of-range is refused ==="
for v in "compute 2.1" "compute 2.21" "compute latest"; do
  printf 'negotiated %-16s applied=%s\n' "'$v'" "$(curl -s -D- -o /dev/null -H "X-Auth-Token: $T" -H "OpenStack-API-Version: $v" "http://$NODE_IP:8774/v2.1/servers" | grep -i '^openstack-api-version' | tr -d '\r' | cut -d' ' -f2-)"
done
echo -n "out of range, above the maximum (expect 406): "
curl -s -o /tmp/r8 -w '%{http_code} ' -H "X-Auth-Token: $T" -H 'OpenStack-API-Version: compute 2.99' "http://$NODE_IP:8774/v2.1/servers"
jq -r '.computeFault.message // .' /tmp/r8
echo -n "below the minimum (expect 406): "
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" -H 'OpenStack-API-Version: compute 1.0' "http://$NODE_IP:8774/v2.1/servers"
echo -n "a malformed header is TOLERATED, not refused (expect 200): "
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $T" -H 'OpenStack-API-Version: compute banana' "http://$NODE_IP:8774/v2.1/servers"

echo "=== R8b: the 2.45 image_id gate, on BOTH actions upstream changed together (FR-039) ==="
echo -n "createBackup at 2.45 (expect a body carrying image_id): "
curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -H 'OpenStack-API-Version: compute 2.45' -d '{"createBackup":{"name":"bk245","backup_type":"daily","rotation":1}}' "http://$NODE_IP:8774/v2.1/servers/$ALPHA/action" | jq -c '.'
echo -n "createBackup at 2.44 (expect an EMPTY body of 0 bytes): "
curl -s -o /tmp/r8bk -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -H 'OpenStack-API-Version: compute 2.44' -d '{"createBackup":{"name":"bk244","backup_type":"daily","rotation":1}}' "http://$NODE_IP:8774/v2.1/servers/$ALPHA/action"
wc -c < /tmp/r8bk
echo -n "createImage at 2.45 (expect a body carrying image_id): "
curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -H 'OpenStack-API-Version: compute 2.45' -d '{"createImage":{"name":"snap245"}}' "http://$NODE_IP:8774/v2.1/servers/$ALPHA/action" | jq -c '.'
echo -n "createImage at 2.44 (expect an EMPTY body of 0 bytes, and a Location header): "
curl -s -D /tmp/r8b -o /tmp/r8b_body -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -H 'OpenStack-API-Version: compute 2.44' -d '{"createImage":{"name":"snap244"}}' "http://$NODE_IP:8774/v2.1/servers/$ALPHA/action"
wc -c < /tmp/r8b_body
grep -i '^location' /tmp/r8b | tr -d '\r'

echo "=== R8c: no header still resolves to the maximum (FR-040a) ==="
echo -n "no version header at all, applied version: "
curl -s -D- -o /dev/null -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers" | grep -i '^openstack-api-version' | tr -d '\r' | cut -d' ' -f2-
echo -n "and it still returns the image_id body (expect image_id present): "
curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"createImage":{"name":"snapnone"}}' "http://$NODE_IP:8774/v2.1/servers/$ALPHA/action" | jq -r 'if .image_id then "image_id present" else "EMPTY" end'
```

**Expect:**

| Marker | Expected |
|---|---|
| R8a versions | `compute 2.1`, `compute 2.21`, `compute 2.90` — the version *applied*, not a constant |
| R8a refusals | **`406`** with a message naming the supported range, then `406`, then **`200`** for the malformed header |
| R8b `createBackup` | at 2.45 `{"image_id":"<uuid>"}`; at 2.44 a body of **`0`** bytes |
| R8b `createImage` | the same, plus a `Location` header at 2.44 |
| R8c | `compute 2.90` and `image_id present` |

**Both actions are exercised because upstream changed them together at 2.45, and Lightstack had opposite
defects in the two.** The 2026-09-02 host run tested `createImage` alone and found it returning no
body at any version, while the gate had been applied to `createBackup` — the handler the misleading
comment was actually *in*. A step naming one of a pair cannot see a divergence between them.

The malformed header answering `200` is a deliberate divergence — upstream answers 400 — decided in
SPEC-025 as A-006 and recorded as register entry 1. R8c is register entry 2: **upstream would assume
its minimum here** and give neither, kept deliberately because adopting upstream's default would strip
behaviour from every existing caller in one release. It also bounds R8b: **only a client that
negotiates explicitly is affected by any gate.**

### R9 — One address query per listing, and `ip` honoured (FR-045 – FR-048, FR-003b)

```bash
echo "=== R9a: the listing's addresses equal the per-server view, floating IPs included ==="
echo -n "reachability and variables, before anything is asserted: "
test -n "$NODE_IP" && test -n "$T" && test -n "$ALPHA" && test -n "$IMG" || echo -n "MISSING VARIABLE "
curl -s -o /dev/null -w 'compute=%{http_code} ' -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers"
echo "(a 000 means nothing below is meaningful; re-export NODE_IP/T/ALPHA/IMG from setup 1.1)"
echo "--- a floating IP needs an EXTERNAL network, a router and a gateway: build all three ---"
export EXT=$(curl -s -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d '{"network":{"name":"fu-ext","router:external":true}}' "http://$NODE_IP:9696/v2.0/networks" | jq -r '.network.id')
echo "external network: ${EXT:-<CREATE FAILED>}"
openstack subnet show fu-ext-sub >/dev/null 2>&1 || openstack subnet create fu-ext-sub --network fu-ext --subnet-range 192.0.2.0/24 --no-dhcp >/dev/null 2>&1 || true
openstack network show fu-ext -f value -c name -c router:external
openstack router show fu-router >/dev/null 2>&1 || openstack router create fu-router >/dev/null
openstack router set --external-gateway fu-ext fu-router && echo "gateway set"
openstack router add subnet fu-router fu-sub 2>/dev/null && echo "interface added" || echo "(interface already present)"
export FIP=$(openstack floating ip create fu-ext -f value -c floating_ip_address 2>/dev/null || true)
if test -n "$FIP"; then openstack server add floating ip alpha "$FIP" && sleep 3 && echo "associated $FIP"; else echo "NO FLOATING IP allocated: the floating half of this step is UNPROVEN, not passed"; fi
echo "--- per-server view, RAW (not the CLI's reformatting) ---"
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers/$ALPHA" | jq -cS '.server.addresses'
echo "--- the listing's entry for the same server, RAW ---"
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers/detail?name=^alpha$" | jq -cS '.servers[0].addresses'
echo -n "the two are byte-identical: "
A=$(curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers/$ALPHA" | jq -cS '.server.addresses')
B=$(curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers/detail?name=^alpha$" | jq -cS '.servers[0].addresses')
if test -z "$A" || test "$A" = "null" || test "$A" = "{}"; then echo "UNPROVEN: the per-server read returned [$A] — nothing was compared"; elif test "$A" = "$B"; then echo "IDENTICAL: $A"; else echo "DIFFER: per-server=[$A] listing=[$B]"; fi
echo -n "the map carries a FLOATING entry alongside the fixed one: "
echo "$A" | jq -r 'if ([.[][] | select(."OS-EXT-IPS:type"=="floating")] | length) > 0 then "YES - " + ([.[][] | select(."OS-EXT-IPS:type"=="floating") | .addr] | join(",")) else "NO (unproven if no floating IP was allocated above)" end' 
echo "--- a portless server, created with an EXPLICIT empty networks array ---"
curl -s -o /tmp/r9np -w 'create status: %{http_code}\n' -X POST -H "X-Auth-Token: $T" -H 'Content-Type: application/json' -d "{\"server\":{\"name\":\"noports\",\"flavorRef\":\"m1.tiny\",\"imageRef\":\"$IMG\",\"networks\":[]}}" "http://$NODE_IP:8774/v2.1/servers"
export NOPORTS=$(jq -r '.server.id // empty' /tmp/r9np)
echo "noports id: ${NOPORTS:-<CREATE FAILED>}"
sleep 8
echo -n "its addresses (expect {} — not null, not a missing key): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers/detail?name=^noports$" | jq -c '.servers[0].addresses'

echo "=== R9b: ip is honoured rather than ignored ==="
export AIP=$(curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers/$ALPHA" | jq -r '[.server.addresses[][] | select(."OS-EXT-IPS:type"=="fixed") | .addr] | first')
echo "alpha's fixed address: ${AIP:-<EMPTY — the next assertion would test nothing>}"
echo -n "matching ip (expect alpha): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?ip=$AIP" | jq -r '[.servers[].name] | join(" ")'
echo -n "non-matching ip (expect an empty array): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?ip=203.0.113.99" | jq -c '.servers'
echo -n "regex over the subnet (expect at least alpha and beta): "
curl -s -H "X-Auth-Token: $T" "http://$NODE_IP:8774/v2.1/servers?ip=^10\.244\.20\." | jq -r '[.servers[].name] | sort | join(" ")'
```

**Expect:**

| Marker | Expected |
|---|---|
| R9a reachability | `compute=200` and no `MISSING VARIABLE` — **read this first**, a `000` makes everything below meaningless |
| R9a topology | `fu-ext` with `router:external True`, `gateway set`, `interface added`, then `associated <addr>` — or an explicit `NO FLOATING IP allocated`, never silence |
| R9a floating entry | **`YES - <addr>`** — B17's aggregation, which the batched rewrite had to preserve |
| R9a per-server vs listing | **`IDENTICAL`**, each map carrying a `fixed` entry from `10.244.20.0/24` and, if a floating IP was allocated, a `floating` entry tagged as such |
| R9a portless server | `create status: 202`, an id, then **`{}`** — not `null`, not a missing key |
| R9b | alpha's fixed address printed non-empty; then `alpha` alone; `[]`; a list containing `alpha` and `beta` |

`ip` was **refused** with 400 while the address data was unreachable and is honoured now that the
batched ports query exists — the one answer it may never give is a silently unfiltered list.

> **Three corrections from the 2026-09-02 run, all of them this runbook's.**
>
> **The equality check refuses to pass on two empty strings, and the block opens with a reachability
> gate.** The first version compared `"$A" = "$B"` and printed `IDENTICAL` when the service was
> unreachable — both sides were empty, and empty equals empty. That is the same can't-fail shape
> this feature exists to remove, introduced *in the assertion written to fix a previous instance of
> it*, and caught within minutes on the 2026-09-02 run. An equality assertion between two reads needs
> to establish that either read produced something before comparing them.
>
> **A floating IP needs an external network, a router and a gateway — the step builds all three.**
> The first version allocated from `fu-net` and got
> `Network ... is not an external network and cannot be used as a floating-IP pool` — SPEC-024's
> external-network check working correctly. With an external network it then failed on
> `no router with external gateway found for this network`. The floating half of FR-046 therefore
> went unproven twice, reported honestly as `UNPROVEN` rather than passing. The CLI has no flag for
> `router:external`, which is why the network is created through the API.
>
> **Both address maps are read RAW.** The per-server side used `openstack server show -f json`, whose
> `addresses` the CLI reformats into a bare list of strings — so the step compared a CLI rendering
> against a raw API response, printed two visibly different shapes, and never tested FR-046 at all.
> Both sides now come from `curl`, sorted with `jq -cS`, and the comparison is asserted rather than
> left to the eye.
>
> **`$AIP` comes from the raw response too.** Extracting it from the CLI's reformatted output failed
> with `Cannot index string with string "addr"`, leaving it empty — so `?ip=` was sent with no value,
> treated as absent, and the "expect alpha" case returned **all three servers**. It reads as the
> filter being ignored; it was the variable being empty. The step now prints the address and says so
> when it is blank.
>
> **The portless server is created with an explicit `"networks": []`.** Neither
> `openstack server create --no-network` nor a plain create works against Lightstack: from microversion 2.37
> upstream accepts `networks` as the **string** `"auto"` or `"none"`, which is what the CLI sends in
> those two cases, while Lightstack's request struct binds `networks` as an **array** — so both answer
> `400 invalid request body`. **A pre-existing compatibility gap, not this feature's**, and a wide one:
> it means `openstack server create` fails against Lightstack unless a network is named explicitly. **File
> it.** The empty array is the equivalent request Lightstack does accept, and it is what FR-047 needs.

---

## 3. Teardown steps

```bash
export KUBECONFIG=~/.kube/k3s.yaml
export NODE_IP=$(ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')
export OS_AUTH_URL=http://$NODE_IP:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3

for s in alpha beta gamma bootvol noports toobig; do openstack server delete $s 2>/dev/null || true; done
sleep 5
for s in q-snap1 q-snap2 g-snap; do openstack volume snapshot delete $s 2>/dev/null || true; done
sleep 3
for v in q-one q-two q-xfer g-vol; do openstack volume delete $v 2>/dev/null || true; done
openstack volume list -f value -c ID | while read -r v; do openstack volume delete "$v" 2>/dev/null || true; done
for i in snap245 snap244 snapnone; do openstack image delete $i 2>/dev/null || true; done
openstack image delete cirros 2>/dev/null || true
openstack floating ip list -f value -c ID | while read -r f; do openstack floating ip delete "$f" 2>/dev/null || true; done
openstack router unset --external-gateway fu-router 2>/dev/null || true
openstack router remove subnet fu-router fu-sub 2>/dev/null || true
openstack router delete fu-router 2>/dev/null || true
openstack subnet delete fu-ext-sub 2>/dev/null || true
openstack network delete fu-ext 2>/dev/null || true
openstack subnet delete fu-sub 2>/dev/null || true
openstack network delete fu-net 2>/dev/null || true
openstack project delete fu-other 2>/dev/null || true

echo "--- reset the quotas this run lowered ---"
openstack quota set --volumes 100 --gigabytes 10000 --snapshots 100 --backups 100 default 2>/dev/null || true

echo "--- the deployment ---"
helm uninstall lightstack -n lightstack 2>/dev/null || true
kubectl delete namespace lightstack --ignore-not-found
sudo virsh -c qemu:///system list --all

echo "--- the /etc/hosts alias R6 added ---"
sudo sed -i '/lightstack-alias/d' /etc/hosts
getent hosts lightstack-alias || echo "alias removed"

echo "--- node-local state and tmp files ---"
sudo rm -rf /var/lib/lightstack
rm -f /tmp/lightstack-followup.tar /tmp/cirros.img /tmp/r3b1 /tmp/r3b2 /tmp/r3b3 /tmp/r4a /tmp/r4b /tmp/r7 /tmp/r8 /tmp/r8b /tmp/r8b_body /tmp/c1 /tmp/c2
docker rmi docker.io/library/lightstack:followup 2>/dev/null || true

echo "--- the database is operator-owned and is NOT dropped; only this run's rows go ---"
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "DELETE FROM cinder_quotas WHERE project_id = (SELECT id FROM projects WHERE name='default');" 2>/dev/null || true
```

**Pass =** the `lightstack` namespace gone, `virsh list --all` empty, `alias removed`, `/var/lib/lightstack`
removed, and no `fu-*` or `q-*` resources left. The PostgreSQL container is left running — it is
shared with other worktrees and is operator-owned, so it is never reset here.

> **Why the `cinder_quotas` delete is in teardown.** This run *creates* rows for the `default`
> project by lowering limits, where the shared database had none. Leaving them behind moves the
> project from the code-defaults path onto the configured path permanently, which would silently
> change what a *later* feature's tests are exercising — the same class of cross-worktree
> contamination that once left `quotas.instances = 0` and made an unrelated contract suite report 47
> failures.

---

## Troubleshooting

**`Failed to discover available identity versions when contacting http://<ip>:35357/v3`** — `NODE_IP`
is stale or wrong. Re-run the export from the default route; do not use `hostname -I`.

**Every step in R4 returns 202 instead of 413** — the quota fixtures in 1.1 did not apply. Check
`openstack quota show default`; the CLI writes both the compute and block-storage quota sets, and only
the latter matters here.

**`openstack volume backup create` returns 404** — expected, and not a fault of this feature: the
backup routes are registered only on the flat `/v3/backups` path, not under the project-scoped group
the catalog advertises. R4e uses `curl` on the flat path for exactly this reason.

**R6's alias resolves but the catalog still names the IP** — check that the `Host` header actually
reached Lightstack. With `hostNetwork` there is no proxy in the way, but an `http_proxy` in your environment
will rewrite it.

**A server sticks in `BUILD`** — real mode needs a real root-capable container engine. `docker info |
grep -i rootless` should print nothing.

**`image set --min-disk` reports success and the value does not change** — that is the F8 defect
itself. Confirm the deployed image is the one built from this branch (`image.tag=followup`), not a
cached earlier tag.
