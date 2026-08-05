# Host testing guide — Horizon compat fixes F4/F5/F6/F9 (real mode, authoritative)

Validates the four SPEC-019 fixes (cloud-storage#532) in **real mode**: O3K on a root/privileged **k3s**
cluster (real libvirt/KVM, node-local Cinder) with **Rook/Ceph on minikube** serving Glance over **RGW
(S3)**, and the verification Horizon chart (`deployments/horizon-chart/`) on k3s. The o3k image is **built
from this branch**, so it carries the F4/F5/F6/F9 handler changes and migration `064`. This is the graded,
authoritative pass; `HOST_TESTING.md` is the portable stub pass.

The stack bring-up (Rook, the `charts/o3k` real values, the Horizon chart) is **identical to the SPEC-018
real pass** — reused here verbatim; consult
[`../018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md`](../018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md)
and [SPEC-014 §3/§8e](../014-ceph-rgw-s3-backend/HOST_TESTING.md) for the deep Rook/firewall notes. The
**only difference** is the test steps (§2): each finding must be **fixed**, not reproduce.

**Run all commands from the repo root.** Shell blocks are zsh-copy-paste-safe. Two clusters on one host
(k3s + minikube); estimated setup ~45–75 min on a fresh host.

**Validates**

| Finding | Success criterion | Section |
|---|---|---|
| F9 | Volume show/list non-null; Volumes panel renders (no 500), node-local backend — SC-001/002 | 2.1 |
| F5 | `volume` **and** `s3` catalog endpoints resolve to the configured/real host, never `localhost` — SC-003 | 2.2 |
| F4 | Networks list once; `shared`/`router:external`/tenant filters honored — SC-004 | 2.3 |
| F6 | `image delete` removes the RGW-backed image (DB row gone regardless of backend); 404/403 — SC-005 | 2.4 |

## Shells

Three shells, each pinned to a context (every block is labelled `Tn — …`):
- **T1 — host + k3s** (`KUBECONFIG=~/.kube/k3s.yaml`): `podman build`, `k3s ctr`, `make db-up`, `helm`/`kubectl`.
- **T2 — OpenStack CLI** (drives O3K over `127.0.0.1`; no kubeconfig).
- **T3 — Rook/minikube** (default kubeconfig, `rook` context).

Cross-shell values (RGW endpoint + keys) pass **T3 → T1** via `/tmp/rgw.env`.

## 0. Prerequisites

This runbook installs **k3s** (the O3K cluster) if it's absent, plus lightweight helpers. The other
heavy/privileged runtimes stay **preconditions** it only verifies: a container engine for minikube's driver
(Docker, or **Podman + `podman-docker`**), **minikube**, `kubectl`, `helm`, **libvirt/KVM** (active
`libvirtd`), the **OpenStack client**, and the **AWS CLI**. If any of those are missing on a bare host,
install them per the SPEC-018 real-pass Prerequisites (`qemu-kvm libvirt-daemon-system`; minikube/helm/kubectl)
— unchanged.

Install k3s if absent (the official installer sets it up as a root systemd service with bundled containerd;
re-running is a no-op once `k3s` is on `PATH`). Then confirm the service is active.

**T1 — host (root):**
```
command -v k3s >/dev/null || curl -sfL https://get.k3s.io | sudo sh -
sudo systemctl is-active --quiet k3s && echo "k3s active" || echo "k3s NOT active — check: sudo systemctl status k3s"
```

**Pass =** `k3s active`. Now verify the remaining preconditions and install the lightweight helpers:

**T1 — host:**
```
command -v docker >/dev/null || command -v podman >/dev/null || echo "MISSING: docker/podman"
for t in minikube kubectl helm openstack aws virsh; do command -v "$t" >/dev/null || echo "MISSING: $t — install per docs/specs/018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md Prerequisites"; done
systemctl is-active --quiet libvirtd || echo "MISSING: libvirtd not active — see SPEC-018 real Prerequisites"
sudo apt-get update
sudo apt-get install -y jq curl
echo "versions:"; { docker --version || podman --version; }; k3s --version | head -1; kubectl version --client 2>/dev/null | head -1; helm version --short; openstack --version; aws --version | head -1
```

**Pass =** no `MISSING:` lines, and each tool prints a version. Make the k3s kubeconfig user-readable and pin
T1 to it:

**T1 — host:**
```
mkdir -p ~/.kube
sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/k3s.yaml
sudo chown $(id -u):$(id -g) ~/.kube/k3s.yaml
export KUBECONFIG=~/.kube/k3s.yaml
kubectl get nodes
```

**Pass =** the k3s node is `Ready`.

## 1. Setup steps

### 1.1 Rook/Ceph + RGW on minikube (T3)

Reuse the SPEC-014 fixture `deployments/rook/setup-rook-rgw.sh` (Rook Ceph + RGW on a dedicated `rook`
minikube profile, RGW on durable NodePort `30080`). Full reference: SPEC-018 §1 / SPEC-014 §3.

**T3 — rook (default kubeconfig, `rook` context):**
```
unset KUBECONFIG
deployments/rook/setup-rook-rgw.sh --delete 2>/dev/null || true
deployments/rook/setup-rook-rgw.sh
```

Capture the endpoint + keys for T1 and pre-create the Glance bucket.

**T3 — rook:**
```
ROOK_IP=$(minikube -p rook ip)
RGW_ACCESS=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-o3k -o jsonpath='{.data.AccessKey}' | base64 -d)
RGW_SECRET=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-o3k -o jsonpath='{.data.SecretKey}' | base64 -d)
printf 'export RGW_URL=%s\nexport RGW_ACCESS=%s\nexport RGW_SECRET=%s\n' "http://$ROOK_IP:30080" "$RGW_ACCESS" "$RGW_SECRET" > /tmp/rgw.env
cat /tmp/rgw.env
source /tmp/rgw.env
AWS_ACCESS_KEY_ID=$RGW_ACCESS AWS_SECRET_ACCESS_KEY=$RGW_SECRET AWS_DEFAULT_REGION=us-east-1 aws --endpoint-url "$RGW_URL" s3 mb s3://images 2>/dev/null || echo "bucket exists"
```

**Pass =** the script prints `ROUNDTRIP OK` / `HEALTH_OK`; `/tmp/rgw.env` has a non-empty `RGW_URL`; `s3 mb`
succeeds or "bucket exists".

### 1.2 Build this branch's image and import into k3s (T1)

**T1 — host:**
```
podman build -t docker.io/library/o3k:dev -f build/package/Dockerfile .
podman save docker.io/library/o3k:dev -o /tmp/o3k-dev.tar
sudo k3s ctr images import /tmp/o3k-dev.tar
sudo k3s ctr images ls | grep -i o3k
```

**Pass =** the build succeeds and `k3s ctr images ls` lists `docker.io/library/o3k:dev`.

### 1.3 Host Postgres + node-local storage dirs (T1)

The real-mode pod is `hostNetwork` with no `dnsPolicy`, so it reaches Postgres at `127.0.0.1`.

**T1 — host:**
```
make db-up
sudo mkdir -p /var/lib/o3k/images /var/lib/o3k/volumes /var/lib/o3k/instances /var/lib/o3k/cloud-init /var/lib/o3k/dhcp
```

**Pass =** the `o3k-postgres` container is up on `:5432`.

### 1.4 Install O3K (real mode) on k3s (T1)

Write the values with an unquoted heredoc so `$RGW_*` expand. Modes are TOP-LEVEL; `secrets` are required
(see SPEC-018 §2 for the full rationale). Glance → the RGW NodePort; DB → host Postgres.

**T1 — host:**
```
source /tmp/rgw.env
echo "using RGW_URL=$RGW_URL  access=$RGW_ACCESS   (both must be non-empty — else re-run 1.1 in T3)"
cat > o3k-real-values.yaml <<EOF
image: { repository: o3k, tag: dev, pullPolicy: Never }
deploymentMode: single-node
database:
  url: postgres://o3k:secret@127.0.0.1:5432/o3k?sslmode=disable
secrets:
  jwtSecret: $(openssl rand -hex 16)
  adminUser: admin
  adminPassword: secret
endpointHost: o3k-o3k
service:
  type: ClusterIP
realMode: { enabled: true, privileged: true, hostNetwork: true }
nova: { libvirtMode: real }
neutron: { networkingMode: iptables, securityGroupMode: iptables }
cinder: { storageMode: local }
glance:
  storageMode: s3
  s3: { endpoint: "$RGW_URL", bucket: images, region: us-east-1 }
extraEnv:
  - { name: AWS_ACCESS_KEY_ID, value: "$RGW_ACCESS" }
  - { name: AWS_SECRET_ACCESS_KEY, value: "$RGW_SECRET" }
EOF
helm install o3k charts/o3k -n o3k --create-namespace -f o3k-real-values.yaml
kubectl -n o3k rollout status deploy/o3k-o3k --timeout=300s
kubectl -n o3k logs deploy/o3k-o3k --tail=40 | grep -iE 'migrat|glance|hypervisor'
```

**Pass =** the O3K pod is Ready; logs show migrations applied (through `064`) and real hypervisor / **s3
glance** / local cinder. Verify the pod can reach RGW **before** seeding (a stale endpoint makes `image
create` 503):

**T1 — host:**
```
source /tmp/rgw.env
kubectl -n o3k exec deploy/o3k-o3k -- sh -c "wget -qO- $RGW_URL 2>&1 | head -c 120; echo"
```

**Pass =** the `wget` prints S3 XML (`<ListAllMyBucketsResult…>` or `<Error><Code>AccessDenied`). If it's
connection-refused, apply the two host iptables rules from SPEC-014 §8e, then re-check.

### 1.5 Install Horizon on k3s (T1)

**T1 — host:**
```
helm install horizon deployments/horizon-chart -n o3k --set o3k.host=o3k-o3k
kubectl -n o3k rollout status deploy/horizon-o3k-horizon --timeout=300s
kubectl get nodes -o wide
```

**Pass =** the Horizon deployment is Ready. The dashboard is at `http://<k3s-node-INTERNAL-IP>:30080/dashboard`.

### 1.6 Seed and authenticate (T2)

O3K is `hostNetwork`, so its API is on host loopback; map the advertised `o3k-o3k` host to loopback so the
host CLI resolves the catalog. Seed a cirros image (proves Glance→RGW) and a project network.

**T2 — openstack (drives O3K over 127.0.0.1; no kubeconfig):**
```
grep -qxF '127.0.0.1 o3k-o3k' /etc/hosts || echo '127.0.0.1 o3k-o3k' | sudo tee -a /etc/hosts
export OS_AUTH_URL=http://127.0.0.1:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
openstack token issue >/dev/null && echo "auth OK"
[ -f /tmp/cirros.img ] || curl -fsSLo /tmp/cirros.img https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image create cirros --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img
openstack image show cirros -c status -f value
openstack network create ht-net >/dev/null && echo "net created"
```

**Pass =** `auth OK`; `image show cirros … status` = **active** (Glance→RGW works); `net created`.

## 2. Test steps

Drive the UI at `http://<k3s-node-ip>:30080/dashboard` (admin / secret / Default); the `openstack`/`curl`
cross-checks run in **T2**. Record each verdict in the **real** column (authoritative).

### 2.1 F9 — Volumes render once a volume exists (node-local backend)

**T2 — openstack:**
```
openstack volume create --size 1 ht-vol >/dev/null && echo "created"
TOKEN=$(openstack token issue -f value -c id)
curl -fsS -H "X-Auth-Token: $TOKEN" "http://o3k-o3k:8776/v3/volumes/detail" | jq '.volumes[0] | {volume_type, availability_zone, encrypted, multiattach, consumes_quota, shared_targets, user_id, host: ."os-vol-host-attr:host", tenant: ."os-vol-tenant-attr:tenant_id", metadata}'
curl -fsS -H "X-Auth-Token: $TOKEN" "http://o3k-o3k:8776/v3/volumes/detail" | jq -e '.volumes[0] | (.volume_type=="__DEFAULT__") and (.availability_zone=="nova") and (.encrypted==false) and (.multiattach==false) and (.consumes_quota==true) and (.shared_targets==false) and (.user_id!=null) and (.["os-vol-host-attr:host"]!=null) and (.["os-vol-tenant-attr:tenant_id"]!=null) and (.metadata!=null)' && echo "F9 fields OK"
```

**Expect:** every field non-null; `F9 fields OK` prints. **Browser:** Project → Volumes lists `ht-vol` and
returns HTTP 200 — no 500 / `TemplateSyntaxError`.

### 2.2 F5 — `volume` AND `s3` catalog endpoints have no `localhost`

Real mode is the authoritative check for the S3 endpoint (spec Q3): the `volume` alias resolves to the
configured host, and `s3` resolves to the real RGW URL — neither `localhost`.

**T2 — openstack:**
```
source /tmp/rgw.env 2>/dev/null || true
openstack catalog show volume -f json | jq -r '.endpoints[].url'
openstack catalog show s3 -f json | jq -r '.endpoints[].url'
openstack catalog show volume -f json | jq -e '[.endpoints[].url] | all(contains("localhost") | not)' && echo "F5 volume no-localhost OK"
openstack catalog show s3 -f json | jq -e '[.endpoints[].url] | all(contains("localhost") | not)' && echo "F5 s3 no-localhost OK"
openstack volume list -f value -c Name
```

**Expect:** the `volume` URL is `http://o3k-o3k:8776/v3`; the `s3` URL is your `$RGW_URL`
(`http://<rook-ip>:30080`); both `F5 … no-localhost OK` lines print; `volume list` reaches Cinder and lists
`ht-vol`.

### 2.3 F4 — Networks list once; filters honored

**T2 — openstack:**
```
TOKEN=$(openstack token issue -f value -c id)
NET=http://o3k-o3k:9696/v2.0/networks
echo "count of ht-net (unfiltered):"; curl -fsS -H "X-Auth-Token: $TOKEN" "$NET" | jq '[.networks[] | select(.name=="ht-net")] | length'
curl -fsS -H "X-Auth-Token: $TOKEN" "$NET" | jq -e '.networks | all(has("router:external") and (.["router:external"]==false))' && echo "attr OK"
curl -fsS -H "X-Auth-Token: $TOKEN" "$NET?router:external=true" | jq -e '.networks == []' && echo "external-empty OK"
curl -fsS -H "X-Auth-Token: $TOKEN" "$NET?shared=true" | jq -e '[.networks[].name] | contains(["ht-net"]) | not' && echo "shared-filter OK"
```

**Expect:** `ht-net` appears once unfiltered; `attr OK`, `external-empty OK`, `shared-filter OK` all print.
**Browser:** Project → Network → Networks shows `ht-net` **once** (not twice).

### 2.4 F6 — Deleting an RGW-backed image removes it

Use a throwaway public image (keeps `cirros` for other steps). Proves the DB row — the list's source of
truth — is removed regardless of the best-effort RGW object delete.

**T2 — openstack:**
```
head -c 1048576 /dev/zero > /tmp/ht.img
openstack image create ht-img --disk-format qcow2 --container-format bare --public --file /tmp/ht.img >/dev/null && echo "created"
ID=$(openstack image show ht-img -f value -c id)
openstack image delete "$ID" && echo "delete returned success"
openstack image list -f value -c Name | grep -qx ht-img && echo "STILL PRESENT (F6 FAIL)" || echo "F6 gone OK"
openstack image delete "$ID" 2>&1 | grep -qiE 'not.?found|404|No Image found' && echo "F6 second-delete 404 OK"
```

**Expect:** `created`, `delete returned success`, then **`F6 gone OK`** and **`F6 second-delete 404 OK`**.
**Browser:** Project → Compute → Images no longer lists `ht-img`.

## 3. Teardown steps

**T1 — host (o3k side):**
```
export KUBECONFIG=~/.kube/k3s.yaml
helm uninstall horizon o3k -n o3k 2>/dev/null || true
kubectl delete namespace o3k --ignore-not-found
make db-down
rm -f o3k-real-values.yaml /tmp/o3k-dev.tar /tmp/ht.img
sudo sed -i '/^127\.0\.0\.1 o3k-o3k$/d' /etc/hosts
```

**T3 — rook (removes the whole `rook` minikube profile):**
```
deployments/rook/setup-rook-rgw.sh --delete
```

**Pass =** both Helm releases and the `o3k` namespace are gone, Postgres is down, the `rook` profile is
deleted, and the `/etc/hosts` line is removed. (Let the O3K chart's pre-delete hook finish before deleting
the namespace.)

## Troubleshooting

- **`image create cirros` → 503 / stays `queued`** — the pod can't reach RGW; re-run the 1.4 reachability
  `wget` and, if refused, apply the SPEC-014 §8e host iptables rules.
- **`catalog show s3` errors "service not found"** — `glance.s3.endpoint` wasn't set; check
  `helm -n o3k get values o3k | grep -iA2 s3` (the s3 service is only advertised when RGW is configured).
- **`catalog show volume` still shows `localhost`** — the imported image predates migration `064`; rebuild
  (1.2) and `helm upgrade o3k charts/o3k -n o3k -f o3k-real-values.yaml`.
- **Volumes panel still 500s** — stale image; rebuild + `helm upgrade` as above, then restart the pod.
