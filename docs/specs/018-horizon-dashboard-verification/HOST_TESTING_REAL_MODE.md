# HOST_TESTING — Real pass (graded, authoritative)

The graded pass. Real-mode Lightstack on a **root/privileged k3s** cluster (real libvirt/KVM VMs) with
**Rook/Ceph on minikube** serving image storage over **RGW (S3)**, and the verification Horizon chart
(`deployments/horizon-chart/`) on the same k3s cluster. **No RBD** — volumes are node-local.

Two clusters on one host: **k3s** (Lightstack + Horizon) and **minikube** (Rook/Ceph). Run all commands from the
repo root. Shell blocks are zsh-safe. Estimated end-to-end setup: **~45–75 min** on a fresh host (SC-002
real-pass reference; no fixed bound).

## Shells

Three shells, each pinned to a fixed context — **each code block carries a bold `Tn — …` label just above
it** saying which shell to run it in:

- **T1 — host + k3s** (the Lightstack side). `KUBECONFIG=~/.kube/k3s.yaml`; run from the repo root. Does
  `docker build`, `sudo k3s ctr`, `make db-up`, and `helm`/`kubectl` against **k3s**.
- **T2 — OpenStack CLI** (drives Lightstack). The `OS_*` env → `http://127.0.0.1:35357/v3` (Lightstack is hostNetwork,
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

Assumes already installed (by whatever means — distro package, pipx, venv, …): a **Docker-compatible
container engine**, i.e. `docker` on `PATH` — Docker, or **Podman + `podman-docker`** (which provides the
`docker` shim; with Podman also pass `minikube start --driver=podman`). It is used for two things: minikube's
driver, and building the Lightstack image in §2. Also: the **OpenStack client** (`openstack`) and the **AWS
CLI** (`aws`, for the RGW bucket).

**This runbook installs none of those, and deliberately does not install a container engine** — engine setup
is the reader's, since the choice affects rootless/driver behaviour the runbook cannot predict. The
`apt-get` below is only KVM/libvirt tooling and small helpers (k3s brings its own containerd). Check with
`docker version` before starting; if that fails, stop and install an engine first.

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
source them) and pre-create the Glance bucket. The file is pre-created **mode 600** — a plain `>` under a
typical `umask` leaves it `644`/`664` in a world-traversable `/tmp` — and the block below prints only the
endpoint plus a present/missing check rather than the keys themselves.

Note that `setup-rook-rgw.sh` has already printed both keys in its completion banner above; that is
deliberate, since it is how you read them off for ad-hoc `aws` commands. They are scratch credentials for
a `CephObjectStoreUser` the fixture creates inside the throwaway `rook` minikube profile, and teardown
(`--delete`) removes the profile and the cluster with them — so treat this whole procedure's output as
lab-only, not as something safe to paste unredacted from a real deployment.

**T3 — rook:**
```
ROOK_IP=$(minikube -p rook ip)
RGW_ACCESS=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-lightstack -o jsonpath='{.data.AccessKey}' | base64 -d)
RGW_SECRET=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-lightstack -o jsonpath='{.data.SecretKey}' | base64 -d)
install -m 600 /dev/null /tmp/rgw.env
printf 'export RGW_URL=%s\nexport RGW_ACCESS=%s\nexport RGW_SECRET=%s\n' "http://$ROOK_IP:30080" "$RGW_ACCESS" "$RGW_SECRET" > /tmp/rgw.env
source /tmp/rgw.env
echo "wrote /tmp/rgw.env (mode 600) with RGW_URL=$RGW_URL"
test -n "$RGW_ACCESS" -a -n "$RGW_SECRET" && echo "RGW keys captured" || echo "RGW keys MISSING — check the CephObjectStoreUser secret above"
AWS_ACCESS_KEY_ID=$RGW_ACCESS AWS_SECRET_ACCESS_KEY=$RGW_SECRET AWS_DEFAULT_REGION=us-east-1 aws --endpoint-url "$RGW_URL" s3 mb s3://lightstack-images 2>/dev/null || echo "bucket exists"
```

Desired outcome: `s3 mb` succeeds (or "bucket exists"). RGW is reachable from the k3s host directly at
`$RGW_URL` (both are libvirt VMs on this host), so Lightstack's hostNetwork pod uses it with **no port-forward**.

### 2. Lightstack on k3s (real mode) (T1)

Build the lightstack image and import it into k3s (k3s uses its own containerd, so the image has to be
handed over as a tar rather than pulled from the engine's local store; the image bundles `qemu-img`+`cdrkit`,
so it is real-mode-ready). `docker` here is whichever engine you installed — under Podman the
`podman-docker` shim resolves it, and `docker save` writes the same docker-archive `k3s ctr` expects.

**T1 — host:**
```
docker build -t docker.io/library/lightstack:dev -f build/package/Dockerfile .
docker save docker.io/library/lightstack:dev -o /tmp/lightstack-dev.tar
sudo k3s ctr images import /tmp/lightstack-dev.tar
sudo k3s ctr images ls | grep -i lightstack
```

Postgres on the host + node-local storage subdirs. The real-mode Lightstack pod is `hostNetwork` and the chart
sets **no `dnsPolicy`**, so it uses the host's DNS and can't resolve in-cluster Services — reach Postgres
at `127.0.0.1` (`make db-up` publishes `:5432`; `lightstack-migrate` creates the schema on first boot). The
chart's init-container also makes the subdirs, but pre-making them is harmless.

**T1 — host:**
```
make db-up
sudo mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp
```

Write `lightstack-real-values.yaml` with an **unquoted heredoc** so `$RGW_*` (sourced from T3's file) expand —
no `\`-continuations, no `${}` braces (paste-safe). **Modes are TOP-LEVEL** (`nova`/`neutron`/`glance`/
`cinder`) — nesting under `services:` silently falls back to *stub*. `secrets.jwtSecret`+`adminPassword`
are **required** (validation only checks non-empty, so `admin`/`secret` works). Glance → the RGW NodePort
`$RGW_URL`; DB → host Postgres. The **AWS keys go in a `rgw-s3-creds` Secret**, not in the values file —
Helm persists supplied values in its `sh.helm.release.v1.*` release Secret, and `quickstart.md` + T012 +
`charts/lightstack/values.yaml` all specify the Secret form. The namespace is created up front so the
Secret can exist before `helm install` (hence no `--create-namespace` below); both commands are
`apply`-shaped, so re-running the runbook is safe. The keys are passed via `--from-env-file` and a
600-mode file rather than `--from-literal`, which would put the secret in the command line where any
local user can read it out of `ps` (and where the shell records it in history).

**T1 — host:**
```
source /tmp/rgw.env
echo "using RGW_URL=$RGW_URL  (must be non-empty — else re-run §1 in T3)"
test -n "$RGW_ACCESS" -a -n "$RGW_SECRET" && echo "RGW keys present" || echo "RGW keys MISSING — re-run §1 in T3"
kubectl create namespace lightstack --dry-run=client -o yaml | kubectl apply -f -
install -m 600 /dev/null /tmp/rgw-creds.env
printf 'accessKey=%s\nsecretKey=%s\n' "$RGW_ACCESS" "$RGW_SECRET" > /tmp/rgw-creds.env
kubectl -n lightstack create secret generic rgw-s3-creds --from-env-file=/tmp/rgw-creds.env --dry-run=client -o yaml | kubectl apply -f -
cat > lightstack-real-values.yaml <<EOF
image: { repository: lightstack, tag: dev, pullPolicy: Never }
deploymentMode: single-node
database:
  url: postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable
secrets:
  jwtSecret: $(openssl rand -hex 16)
  adminUser: admin
  adminPassword: secret
endpointHost: lightstack-lightstack
service:
  type: ClusterIP
realMode: { enabled: true, privileged: true, hostNetwork: true }
nova: { libvirtMode: real }
neutron: { networkingMode: iptables, securityGroupMode: iptables }
cinder: { storageMode: local }
glance:
  storageMode: s3
  s3: { endpoint: "$RGW_URL", bucket: lightstack-images, region: us-east-1 }
extraEnv:
  - { name: AWS_ACCESS_KEY_ID, valueFrom: { secretKeyRef: { name: rgw-s3-creds, key: accessKey } } }
  - { name: AWS_SECRET_ACCESS_KEY, valueFrom: { secretKeyRef: { name: rgw-s3-creds, key: secretKey } } }
EOF
```

(No `applyPrivilegedNamespaceLabel` — the chart renders a Namespace object when it's `true`, which
collides with the namespace created above ("namespaces lightstack already exists"). k3s admits privileged
pods by default, so the label isn't needed. If your cluster enforces restricted PodSecurity, you need the
label instead: skip the `kubectl create namespace` above, set `podSecurity.applyPrivilegedNamespaceLabel:
true`, and create the `rgw-s3-creds` Secret after the install — use one namespace owner, not both.)

Install and wait (repo-relative `charts/lightstack` + the values file assume you are at the repo root).

**T1 — host:**
```
helm install lightstack charts/lightstack -n lightstack -f lightstack-real-values.yaml
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=300s
kubectl -n lightstack get pods
kubectl -n lightstack logs deploy/lightstack-lightstack --tail=40
```

Desired outcome: the Lightstack pod is Ready (not CrashLoop), the `init-host-storage` init-container completed,
logs show migrations applied + real hypervisor / iptables neutron / **s3 glance** / local cinder, and the
Service `lightstack-lightstack` exists (ClusterIP). Host prereqs: `libvirtd` up (socket `/var/run/libvirt/libvirt-sock`)
and the Lightstack API ports (35357/8774/9696/8776/9292/8775) free (tear down the stub compose first).

**Verify the Lightstack pod can reach RGW — do this BEFORE seeding** (a stale/unreachable endpoint is what
makes `image create` return `503 failed to upload image`).

**T1 — host:**
```
source /tmp/rgw.env
helm -n lightstack get values lightstack | grep -A1 -iE 'endpoint|s3'
kubectl -n lightstack exec deploy/lightstack-lightstack -- sh -c "wget -qO- $RGW_URL 2>&1 | head -c 120; echo"
```

Desired outcome: the configured `endpoint:` equals your `$RGW_URL` (`http://<rook-ip>:30080`, **not**
`127.0.0.1:8333`), and the `wget` prints **S3 XML** (`<ListAllMyBucketsResult…>` or `<Error><Code>AccessDenied`).
If it's `connection refused` / times out, the pod can't reach the RGW NodePort — a cross-cluster firewall;
apply the two host `iptables` rules from [SPEC-014 HOST_TESTING §8e](../014-ceph-rgw-s3-backend/HOST_TESTING.md)
(`FORWARD … -j ACCEPT` + `POSTROUTING … MASQUERADE` for the minikube /24), then re-check.

### 3. Horizon on k3s (this chart) (T1)

**T1 — host (same shell, `KUBECONFIG=~/.kube/k3s.yaml`):**
```
helm install horizon deployments/horizon-chart -n lightstack --set lightstack.host=lightstack-lightstack
kubectl -n lightstack rollout status deploy/horizon-lightstack-horizon --timeout=300s
kubectl -n lightstack get svc horizon-lightstack-horizon
kubectl get nodes -o wide
```

The dashboard is at `http://<k3s-node-INTERNAL-IP>:30080/dashboard`. Desired outcome: the login page
renders at the NodePort.

### 4. Seed fixtures (T2)

Lightstack runs `hostNetwork`, so its API ports are already bound on the host — **no port-forward needed**. The
catalog advertises `endpointHost=lightstack-lightstack` (for the in-cluster Horizon pod), which the host shell can't
resolve, so map it to loopback (host-only; doesn't affect Horizon's in-cluster DNS), then seed directly.

Seed **all five** fixtures here — image, keypair, network, subnet, and a second project. The dashboard
cannot create the network (finding **F3**: the Project panel has no Create button) and the seed data ships
only the `default` project, so the "Create network", "Project switching" and "Launch instance" steps below
are untestable until these exist.

**T2 — openstack (drives Lightstack over 127.0.0.1; no kubeconfig):**
```
grep -q ' lightstack-lightstack' /etc/hosts || echo "127.0.0.1 lightstack-lightstack" | sudo tee -a /etc/hosts
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
openstack token issue >/dev/null && echo "auth OK"
```

Download the image and check its **size**, not mere existence — a half-finished download is still a file,
and uploading one fails with a 503 plus a `qemu-img convert qcow2->raw: I/O error` in the Lightstack log that
reads exactly like a Glance defect and is not one:

```
CIRROS_URL=https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
CIRROS_BYTES=21430272
if [ "$(stat -c %s /tmp/cirros.img 2>/dev/null || echo 0)" -eq "$CIRROS_BYTES" ]; then echo "cirros already complete"; else curl -fsSL -o /tmp/cirros.img "$CIRROS_URL"; fi
stat -c %s /tmp/cirros.img
```

Desired outcome: `21430272`. Anything else means the download is incomplete — re-run the block.

```
openstack image create cirros --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img
openstack image show cirros -c status -f value
[ -f /tmp/lightstack_key ] || ssh-keygen -t ed25519 -f /tmp/lightstack_key -N ""
openstack keypair create --public-key /tmp/lightstack_key.pub verifier-key 2>/dev/null || echo "keypair exists"
openstack network create test-net -f value -c id
openstack subnet create --network test-net --subnet-range 10.0.0.0/24 test-subnet -f value -c cidr
openstack project create --domain Default verify-two -f value -c id
openstack role add --project verify-two --user admin admin
openstack project list -f value -c Name
```

Desired outcome: `image show cirros … status` = **active** (this proves Glance→RGW works — if it's
`queued` with a 503, first re-check the image size above, then the §2 reachability check), keypair
`verifier-key` exists (flavors come pre-seeded by the **binary's** migrations —
`migrations/002_seed_data.up.sql` — not by the chart), a `10.0.0.0/24` subnet on `test-net`,
`role add` printing nothing (that is success), and `project list` showing **both** `default` and
`verify-two`. Without the `role add` the second project exists but Horizon's switcher will not offer it.
Optionally confirm the image landed in the RGW bucket (from **T3**:
`source /tmp/rgw.env; aws --endpoint-url $RGW_URL s3 ls s3://lightstack-images/images/`).

## Test steps (browser → `http://<k3s-node-ip>:30080/dashboard`, admin / secret / Default)

Drive the UI in a browser; the `openstack` cross-checks below run in **T2**. Record each verdict in the
**real** column of the report (authoritative).

- **Sign-in (US1).** Desired: login succeeds and the overview renders. **Expect F2** — the usage panel
  shows `Unable to retrieve limits information. Details 'reserved'` (confirmed in real 2026-08-25; root
  cause and the one-key fix are in the report's F2). The rest of the page still renders.
- **Project switching (US1).** Desired: the switcher lists **both** `default` and `verify-two` (seeded
  above), and selecting the other one **re-scopes the view** — the resource panels should go empty, since
  the fixtures belong to `default`. Re-scoping is the acceptance check, not merely that the switcher
  renders.
- **Create network (US2).** Networks → Create. Desired: network + subnet listed. Regression-guard:
  the Networks panel renders (Neutron catalog URL has no doubled `/v2.0`).
- **Create volume (US2).** Volumes → Create. Desired: volume listed with a status (node-local backend).
  (F5's stub `localhost:8776` Cinder endpoint does **not** recur in real — the volumev3 catalog is clean
  at `lightstack-lightstack:8776`; see the report's workflow-4 row. Expect **F8** instead: the Create Volume button
  opens no dialog.)
- **Launch instance (US2).** Instances → Launch Instance; pick `cirros`, a flavor, `verifier-key`, and
  `test-net`. Desired: instance reaches ACTIVE and lists. Regression-guard: the Instances panel renders
  (server-detail includes `addresses`). Known failure (**F7**): the Networks step renders no selectable
  rows — confirm in the browser console as `[ngRepeat:dupes] … Duplicate key: <network-id>`, which is F4
  breaking the table. Also expect the Security Groups step to be empty (**F10**). If the UI cannot
  complete, launch from **T2** instead so the SSH and console steps below stay exercisable:
  ```
  openstack server create --image cirros --flavor m1.small --key-name verifier-key --network test-net vm1
  openstack server show vm1 -c status -c power_state -c addresses -c key_name -c security_groups
  ```
- **SSH liveness (US2).** From a shell with a route to the VM's address — for a fixed IP on the project
  net you may need a router + external net + the host `iptables` rules per
  [SPEC-014 §8e](../014-ceph-rgw-s3-backend/HOST_TESTING.md).

  **T2 — openstack:**
  ```
  VMIP=$(openstack server show vm1 -f value -c addresses | grep -oE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' | head -1)
  ssh -i /tmp/lightstack_key -o StrictHostKeyChecking=no cirros@"$VMIP" id
  ```
  Desired: the connection succeeds — proving the UI-driven image/network/keypair all took and the VM booted.
  (Default SG admits tcp/22; cirros injects the key.)
- **Console tab (US3).** Open the instance's Console tab. Desired (expected): **not-working** — the noVNC
  viewer shows no guest (finding F1). Then confirm the serial log works.

  **T2 — openstack:**
  ```
  openstack console log show vm1
  ```
  returns boot output.

## Teardown

**T1 — host (lightstack side):**
```
export KUBECONFIG=~/.kube/k3s.yaml
helm uninstall horizon lightstack -n lightstack
kubectl delete namespace lightstack
make db-down
```

**T3 — rook (removes the whole `rook` minikube profile):**
```
deployments/rook/setup-rook-rgw.sh --delete
```

(If the Lightstack chart's pre-delete teardown hook runs, let it complete before deleting the namespace.)
