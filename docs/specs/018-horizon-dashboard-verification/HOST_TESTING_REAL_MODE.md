# HOST_TESTING — Real pass (graded, authoritative)

The graded pass. Real-mode O3K on a **root/privileged k3s** cluster (real libvirt/KVM VMs) with
**Rook/Ceph on minikube** serving image storage over **RGW (S3)**, and the verification Horizon chart
(`deployments/horizon-chart/`) on the same k3s cluster. **No RBD** — volumes are node-local.

Two clusters on one host: **k3s** (O3K + Horizon) and **minikube** (Rook/Ceph). Run all commands from the
repo root. Shell blocks are zsh-safe. Estimated end-to-end setup: **~45–75 min** on a fresh host (SC-002
real-pass reference; no fixed bound).

## Shells

Three shells, each pinned to a fixed context — **each code block carries a bold `Tn — …` label just above
it** saying which shell to run it in:

- **T1 — host + k3s** (the O3K side). `KUBECONFIG=~/.kube/k3s.yaml`; run from the repo root. Does
  `podman build`, `sudo k3s ctr`, `make db-up`, and `helm`/`kubectl` against **k3s**.
- **T2 — OpenStack CLI** (drives O3K). The `OS_*` env → `http://127.0.0.1:35357/v3` (O3K is hostNetwork,
  so host loopback reaches it). Seeds fixtures and runs API-level cross-checks. No kubeconfig.
- **T3 — Rook / minikube**. The **default** kubeconfig (`~/.kube/config` holds the `rook` context, so
  `kubectl --context rook` and `minikube -p rook` work here). Runs the Rook fixture.

Cross-shell values (the RGW endpoint + keys) are handed **T3 → T1** via `/tmp/rgw.env`.

## Chart render evidence (pre-host gate — already validated)

`deployments/horizon-chart/` passes `helm lint` and `helm template` cleanly (SPEC-018 T010).

**T1 — host:**
```
helm lint deployments/horizon-chart
helm template testrel deployments/horizon-chart | grep -E '^kind:|  name:'
```

Desired outcome: `0 chart(s) failed`; the render lists the ConfigMap, the Horizon Deployment+Service
(NodePort), the memcached Deployment + `memcached` Service, and the noVNC Deployment+Service.

## Prerequisites (Ubuntu; idempotent)

Assumes already installed (by whatever means — distro package, pipx, venv, …): a container engine for
minikube's driver (Docker, or **Podman + `podman-docker`** — with Podman, `minikube start --driver=podman`);
the **OpenStack client** (`openstack`); and the **AWS CLI** (`aws`, for the RGW bucket). This runbook
installs none of those (k3s uses its bundled containerd) — only the KVM/libvirt tooling and small helpers.

**T1 — host (root):**
```
sudo apt-get update
sudo apt-get install -y qemu-kvm libvirt-daemon-system qemu-utils genisoimage curl git jq conntrack
sudo systemctl enable --now libvirtd
```

Install k3s (root), minikube, kubectl, and helm if absent.

**T1 — host (root):**
```
curl -sfL https://get.k3s.io | sudo sh -
curl -Lo /tmp/minikube https://storage.googleapis.com/minikube/releases/latest/minikube-linux-amd64
sudo install /tmp/minikube /usr/local/bin/minikube
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

Make the k3s kubeconfig user-readable and pin **T1** to it (T1 uses this for the rest of the run).

**T1 — host:**
```
mkdir -p ~/.kube
sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/k3s.yaml
sudo chown $(id -u):$(id -g) ~/.kube/k3s.yaml
export KUBECONFIG=~/.kube/k3s.yaml
kubectl get nodes
```

## Setup

### 1. Rook/Ceph + RGW on minikube (T3)

Use the SPEC-014 fixture **`deployments/rook/setup-rook-rgw.sh`** — it stands up Rook Ceph + RGW on a
dedicated `rook` minikube profile (kvm2 + raw OSD disks), creates the `CephObjectStore` + S3 user, and
exposes RGW on a **durable NodePort `30080`** (a *separate* Service the operator won't revert). This
sidesteps every manual-Rook trap: no OSD-disk fuss, no reverting NodePort, and a fresh profile avoids
the stale-`/var/lib/rook` cephx wedge (`handle_auth_bad_method` / mon never reaches quorum). Full
reference: [SPEC-014 HOST_TESTING §3](../014-ceph-rgw-s3-backend/HOST_TESTING.md).

Start clean (`--delete` removes any prior `rook` profile), then bring it up.

**T3 — rook (default kubeconfig; the `rook` context):**
```
unset KUBECONFIG
deployments/rook/setup-rook-rgw.sh --delete 2>/dev/null || true
deployments/rook/setup-rook-rgw.sh
```

Wait for `ROUNDTRIP OK` / `HEALTH_OK`. Capture the endpoint + keys into `/tmp/rgw.env` (so **T1** can
source them) and pre-create the Glance bucket.

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

Desired outcome: `s3 mb` succeeds (or "bucket exists"). RGW is reachable from the k3s host directly at
`$RGW_URL` (both are libvirt VMs on this host), so O3K's hostNetwork pod uses it with **no port-forward**.

### 2. O3K on k3s (real mode) (T1)

Build the o3k image and import it into k3s (k3s uses containerd, not podman; the image bundles
`qemu-img`+`cdrkit`, so it is real-mode-ready).

**T1 — host:**
```
podman build -t docker.io/library/o3k:dev -f build/package/Dockerfile .
podman save docker.io/library/o3k:dev -o /tmp/o3k-dev.tar
sudo k3s ctr images import /tmp/o3k-dev.tar
sudo k3s ctr images ls | grep -i o3k
```

Postgres on the host + node-local storage subdirs. The real-mode O3K pod is `hostNetwork` and the chart
sets **no `dnsPolicy`**, so it uses the host's DNS and can't resolve in-cluster Services — reach Postgres
at `127.0.0.1` (`make db-up` publishes `:5432`; `o3k-migrate` creates the schema on first boot). The
chart's init-container also makes the subdirs, but pre-making them is harmless.

**T1 — host:**
```
make db-up
sudo mkdir -p /var/lib/o3k/images /var/lib/o3k/volumes /var/lib/o3k/instances /var/lib/o3k/cloud-init /var/lib/o3k/dhcp
```

Write `o3k-real-values.yaml` with an **unquoted heredoc** so `$RGW_*` (sourced from T3's file) expand —
no `\`-continuations, no `${}` braces (paste-safe). **Modes are TOP-LEVEL** (`nova`/`neutron`/`glance`/
`cinder`) — nesting under `services:` silently falls back to *stub*. `secrets.jwtSecret`+`adminPassword`
are **required** (validation only checks non-empty, so `admin`/`secret` works). Glance → the RGW NodePort
`$RGW_URL`; DB → host Postgres; AWS creds inline (no pre-created Secret needed).

**T1 — host:**
```
source /tmp/rgw.env
echo "using RGW_URL=$RGW_URL  access=$RGW_ACCESS   (both must be non-empty — else re-run §1 in T3)"
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
```

(No `applyPrivilegedNamespaceLabel` — the chart renders a Namespace object when it's `true`, which
collides with `--create-namespace` ("namespaces o3k already exists"). k3s admits privileged pods by
default, so the label isn't needed. If your cluster enforces restricted PodSecurity, drop
`--create-namespace` and set `podSecurity.applyPrivilegedNamespaceLabel: true` instead — use one, not both.)

Install and wait (repo-relative `charts/o3k` + the values file assume you are at the repo root).

**T1 — host:**
```
helm install o3k charts/o3k -n o3k --create-namespace -f o3k-real-values.yaml
kubectl -n o3k rollout status deploy/o3k-o3k --timeout=300s
kubectl -n o3k get pods
kubectl -n o3k logs deploy/o3k-o3k --tail=40
```

Desired outcome: the O3K pod is Ready (not CrashLoop), the `init-host-storage` init-container completed,
logs show migrations applied + real hypervisor / iptables neutron / **s3 glance** / local cinder, and the
Service `o3k-o3k` exists (ClusterIP). Host prereqs: `libvirtd` up (socket `/var/run/libvirt/libvirt-sock`)
and the O3K API ports (35357/8774/9696/8776/9292/8775) free (tear down the stub compose first).

**Verify the O3K pod can reach RGW — do this BEFORE seeding** (a stale/unreachable endpoint is what
makes `image create` return `503 failed to upload image`).

**T1 — host:**
```
source /tmp/rgw.env
helm -n o3k get values o3k | grep -A1 -iE 'endpoint|s3'
kubectl -n o3k exec deploy/o3k-o3k -- sh -c "wget -qO- $RGW_URL 2>&1 | head -c 120; echo"
```

Desired outcome: the configured `endpoint:` equals your `$RGW_URL` (`http://<rook-ip>:30080`, **not**
`127.0.0.1:8333`), and the `wget` prints **S3 XML** (`<ListAllMyBucketsResult…>` or `<Error><Code>AccessDenied`).
If it's `connection refused` / times out, the pod can't reach the RGW NodePort — a cross-cluster firewall;
apply the two host `iptables` rules from [SPEC-014 HOST_TESTING §8e](../014-ceph-rgw-s3-backend/HOST_TESTING.md)
(`FORWARD … -j ACCEPT` + `POSTROUTING … MASQUERADE` for the minikube /24), then re-check.

### 3. Horizon on k3s (this chart) (T1)

**T1 — host (same shell, `KUBECONFIG=~/.kube/k3s.yaml`):**
```
helm install horizon deployments/horizon-chart -n o3k --set o3k.host=o3k-o3k
kubectl -n o3k rollout status deploy/horizon-o3k-horizon --timeout=300s
kubectl -n o3k get svc horizon-o3k-horizon
kubectl get nodes -o wide
```

The dashboard is at `http://<k3s-node-INTERNAL-IP>:30080/dashboard`. Desired outcome: the login page
renders at the NodePort.

### 4. Seed fixtures (T2)

O3K runs `hostNetwork`, so its API ports are already bound on the host — **no port-forward needed**. The
catalog advertises `endpointHost=o3k-o3k` (for the in-cluster Horizon pod), which the host shell can't
resolve, so map it to loopback (host-only; doesn't affect Horizon's in-cluster DNS), then seed directly.

**T2 — openstack (drives O3K over 127.0.0.1; no kubeconfig):**
```
grep -q ' o3k-o3k' /etc/hosts || echo "127.0.0.1 o3k-o3k" | sudo tee -a /etc/hosts
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
openstack token issue >/dev/null && echo "auth OK"
[ -f /tmp/cirros.img ] || curl -fsSLo /tmp/cirros.img https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image create cirros --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img
openstack image show cirros -c status -f value
[ -f /tmp/o3k_key ] || ssh-keygen -t ed25519 -f /tmp/o3k_key -N ""
openstack keypair create --public-key /tmp/o3k_key.pub verifier-key 2>/dev/null || echo "keypair exists"
```

Desired outcome: `image show cirros … status` = **active** (this proves Glance→RGW works — if it's
`queued` with a 503, revisit the §2 reachability check), keypair `verifier-key` exists (flavors come
pre-seeded by the **binary's** migrations — `migrations/002_seed_data.up.sql` — not by the chart), and
the image landed in the RGW bucket (optional confirm from **T3**:
`source /tmp/rgw.env; aws --endpoint-url $RGW_URL s3 ls s3://images/images/` → an `image-<id>.raw`).

## Test steps (browser → `http://<k3s-node-ip>:30080/dashboard`, admin / secret / Default)

Drive the UI in a browser; the `openstack` cross-checks below run in **T2**. Record each verdict in the
**real** column of the report (authoritative).

- **Sign-in (US1).** Desired: login succeeds and the overview renders. **Expect F2** — the usage panel
  shows `Unable to retrieve limits information. Details 'reserved'` (confirmed in real 2026-08-25; root
  cause and the one-key fix are in the report's F2). The rest of the page still renders.
- **Project switching (US1).** Desired: view re-scopes.
- **Create network (US2).** Networks → Create. Desired: network + subnet listed. Regression-guard:
  the Networks panel renders (Neutron catalog URL has no doubled `/v2.0`).
- **Create volume (US2).** Volumes → Create. Desired: volume listed with a status (node-local backend).
  (F5's stub `localhost:8776` Cinder endpoint does **not** recur in real — the volumev3 catalog is clean
  at `o3k-o3k:8776`; see the report's workflow-4 row. Expect **F8** instead: the Create Volume button
  opens no dialog.)
- **Launch instance (US2).** Instances → Launch Instance; pick `cirros`, a flavor, `verifier-key`,
  the network. Desired: instance reaches ACTIVE and lists. Regression-guard: the Instances panel renders
  (server-detail includes `addresses`).
- **SSH liveness (US2).** From a shell with a route to the VM's address — for a fixed IP on the project
  net you may need a router + external net + the host `iptables` rules per
  [SPEC-014 §8e](../014-ceph-rgw-s3-backend/HOST_TESTING.md).

  **T2 — openstack:**
  ```
  VMIP=$(openstack server show <vm> -f value -c addresses | grep -oE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' | head -1)
  ssh -i /tmp/o3k_key -o StrictHostKeyChecking=no cirros@"$VMIP" id
  ```
  Desired: the connection succeeds — proving the UI-driven image/network/keypair all took and the VM booted.
  (Default SG admits tcp/22; cirros injects the key.)
- **Console tab (US3).** Open the instance's Console tab. Desired (expected): **not-working** — the noVNC
  viewer shows no guest (finding F1). Then confirm the serial log works.

  **T2 — openstack:**
  ```
  openstack console log show <vm>
  ```
  returns boot output.

## Teardown

**T1 — host (o3k side):**
```
export KUBECONFIG=~/.kube/k3s.yaml
helm uninstall horizon o3k -n o3k
kubectl delete namespace o3k
make db-down
```

**T3 — rook (removes the whole `rook` minikube profile):**
```
deployments/rook/setup-rook-rgw.sh --delete
```

(If the O3K chart's pre-delete teardown hook runs, let it complete before deleting the namespace.)
