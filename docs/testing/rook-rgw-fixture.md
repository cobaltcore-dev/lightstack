# Ceph RGW (S3) fixture via Rook on minikube

A standalone, single-node **Ceph RGW** endpoint for validating lightstack's Glance image
storage against a real S3/RGW backend (cloud-storage#480) — and, later, tenant
object-storage. This is the manual / deployment-parity fixture; CI uses the
lighter `ceph/demo` docker-compose instead (RGW can't run on GitHub runners).

Ceph is brought up in a **non-degraded (`HEALTH_OK`)** state on a single node,
which needs deliberate tuning (see [Single-node HEALTH_OK](#single-node-health_ok)).

## What's here (and what isn't)

- **Nothing from Rook is vendored.** The operator is installed from Rook's
  official Helm chart (`rook-release/rook-ceph`); only the small `CephCluster` /
  `CephObjectStore` / user / `Service` CRs are ours, tuned for one node.
- **Licensing:** Lightstack and Rook are both Apache-2.0. Because we reference the chart
  at runtime and copy no Rook files, there is no attribution/NOTICE obligation.
  Container images (Ceph = LGPL-2.1, Rook = Apache-2.0) are pulled at runtime,
  not redistributed.

## Prerequisites

- **minikube** with the **kvm2** driver (libvirt) — needed for `--extra-disks`
  (the docker driver can't hand Rook raw OSD devices).
- **kubectl**, **helm 3.13+**.
- **aws** CLI (optional) — only for the end-to-end S3 smoke test.

## Quick start

```bash
deployments/rook/setup-rook-rgw.sh
```

The script is idempotent-ish and pins all operations to a dedicated `rook`
minikube profile (context `rook`), so it never touches your other clusters
(e.g. the k3s cluster running lightstack). It:

1. starts the `rook` minikube profile (kvm2, 2 extra disks → 2 OSDs);
2. installs the Rook operator via Helm;
3. applies the single-node config override, `CephCluster`, `CephObjectStore`,
   S3 user, and a durable NodePort service;
4. waits for `HEALTH_OK` and prints the S3 endpoint + credentials + the Lightstack
   Glance config snippet.

Tear it all down:

```bash
deployments/rook/setup-rook-rgw.sh --delete
```

Pin a different Rook/Ceph version via env (validated pair is the default):

```bash
ROOK_CHART_VERSION=v1.20.1 CEPH_IMAGE=quay.io/ceph/ceph:v20.2.2 deployments/rook/setup-rook-rgw.sh
```

If Helm can't find the chart version: `helm search repo rook-release/rook-ceph --versions`.

## Single-node HEALTH_OK

The stock Rook cluster uses `failureDomain: host` + replica `size: 3` — on one
node that can't be satisfied and Ceph sits in permanent `HEALTH_WARN`. The
fixture avoids that with:

- **2 raw OSD disks** (`minikube --extra-disks=2`), so `size: 2` is satisfiable.
- a `rook-config-override` ConfigMap applied before the cluster:
  ```ini
  [global]
  osd pool default size = 2
  osd crush chooseleaf type = 0   # replicate across OSDs, not hosts
  ```
- `mon.count: 1`, `allowMultiplePerNode: true`.
- `CephObjectStore` metadata/data pools set to `failureDomain: osd`, `size: 2`.

If any pool is still undersized (rare — e.g. the auto-created `.mgr` pool on an
older Ceph), deploy the Rook toolbox and force every pool onto an OSD rule:

```bash
kubectl -n rook-ceph exec -it deploy/rook-ceph-tools -- bash -c '
  ceph osd crush rule create-replicated osd-rule default osd 2>/dev/null || true
  for p in $(ceph osd pool ls); do ceph osd pool set $p crush_rule osd-rule; ceph osd pool set $p size 2; done
  ceph status'
```

## Using it from Lightstack

The script prints the endpoint (`http://<minikube-ip>:30080`), bucket, and keys.
Point Glance at it:

```yaml
glance:
  storage_mode: s3
  s3_endpoint: http://<minikube-ip>:30080
  s3_bucket: images
  s3_region: us-east-1
```
with `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` in lightstack's environment (lightstack reads
S3 credentials from the AWS default chain, not from `lightstack.yaml`).

**Cross-cluster note:** for Lightstack running on a *separate* k3s cluster to reach the
RGW, the k3s node must route to the minikube VM IP. When both are libvirt VMs on
the same host this usually works; otherwise add MetalLB or a static IP.

## Tenant object storage — capability B (RGW → Lightstack Keystone)

The steps above give the **Glance image backend** (capability A): Glance writes
image data to RGW using the local RGW user's keys. **Capability B** lets *tenants*
use their own S3 keys — RGW validates each S3 signature against lightstack's Keystone
(`POST /v3/s3tokens`), so a VM/user with a lightstack-minted EC2 credential can talk to
RGW directly. Local (Glance) and Keystone auth coexist: RGW tries the local user
first, then Keystone.

Run this **after** Lightstack is up and its Keystone is reachable *from the rook cluster*,
passing lightstack's Keystone URL (cross-cluster → a routable node IP, not `localhost`):

```bash
LIGHTSTACK_KEYSTONE_URL=http://<lightstack-node-ip>:35357 deployments/rook/setup-rook-rgw.sh --wire-keystone
```

It injects the RGW keystone options into `rook-config-override` and restarts the
RGW (the keystone URL is startup-only). The options — validated against real RGW in
the CI `ceph/demo` fixture (`deployments/rgw-keystone-entrypoint.sh`) — are:

```ini
[global]
rgw s3 auth use keystone   = true
rgw keystone url           = http://<lightstack-node-ip>:35357
rgw keystone api version   = 3
rgw keystone implicit tenants = s3          # per-project bucket namespaces (isolation)
rgw keystone accepted roles = admin,member,reader
rgw keystone verify ssl    = false
rgw keystone admin user    = admin          # RGW must fetch its OWN token before it
rgw keystone admin password = secret        #   can call /v3/s3tokens — without these
rgw keystone admin project = default        #   it fails "cannot get token for
rgw keystone admin domain  = Default        #   keystone access"
rgw keystone token cache size = 0           # revoked EC2 key refused on next request
```

Override any of them via env: `LIGHTSTACK_KEYSTONE_ADMIN_USER/PASSWORD/PROJECT/DOMAIN`,
`LIGHTSTACK_KEYSTONE_ACCEPTED_ROLES`.

Then, on the **lightstack** side, mint a tenant credential and use it with any S3 client
against the RGW NodePort endpoint (path-style — use the IP):

```bash
openstack ec2 credentials create -f value -c access -c secret   # -> ACCESS SECRET
AWS_ACCESS_KEY_ID=<ACCESS> AWS_SECRET_ACCESS_KEY=<SECRET> \
  aws --endpoint-url http://<minikube-ip>:30080 s3 mb s3://tenant-bucket   # path-style
```

**Gotchas (learned against real RGW):**

- **Bidirectional cross-cluster routing.** Capability A needs Lightstack → RGW
  (`:30080`); capability B *additionally* needs RGW → Lightstack Keystone (`:35357`).
  The `--wire-keystone` step runs a best-effort reachability probe and warns if
  the second direction is broken.
- **Path-style only.** Use the NodePort **IP** endpoint; RGW treats an IP host as
  the endpoint, so buckets stay in the path. (A hostname endpoint would need
  `rgw dns name` set, or RGW mis-reads the host as the bucket.)
- **lightstack IDs are dash-free in `/v3/s3tokens`.** RGW tenant names reject `-`; Lightstack
  already returns keystone-canonical (32-hex) project IDs, so nothing to do here.
- **aws CLI vs RGW errors.** Some aws-cli builds crash on RGW's empty error
  `<Message>`; `boto3` / `s3cmd` / `mc` don't. Prefer those for negative tests.

> **Rook-native alternative (not used here):** Rook's `CephObjectStore` has a
> `spec.auth.keystone` block (url, `acceptedRoles`, `implicitTenants`,
> `tokenCacheSize`, `serviceUserSecretName`). It is the idiomatic Rook path and
> lets the operator manage the restart + service-user secret. This fixture instead
> sets the raw ceph options via `rook-config-override` because those are the exact
> options validated end-to-end against real RGW; switch to `spec.auth.keystone`
> once you've validated its field mapping on your Rook version.

## Manual walkthrough (for understanding)

The script automates these steps; run them by hand to learn the moving parts.

```bash
# 1) minikube with 2 raw disks
minikube start -p rook --driver=kvm2 --memory=8192 --cpus=4 --disk-size=40g --extra-disks=2

# 2) Rook operator (Helm — replaces crds.yaml/common.yaml/operator.yaml/csi-operator.yaml)
helm repo add rook-release https://charts.rook.io/release
helm upgrade --install rook-ceph rook-release/rook-ceph \
  -n rook-ceph --create-namespace --version v1.20.1 --wait

# 3) config override, 4) CephCluster, 5) CephObjectStore + user, 6) NodePort
#    — see the CR bodies in deployments/rook/setup-rook-rgw.sh

# health + endpoint
kubectl -n rook-ceph get cephcluster rook-ceph -o jsonpath='{.status.ceph.health}'; echo
kubectl -n rook-ceph get cephobjectstore my-store -o jsonpath='{.status.phase}'; echo
```

> **Manifest path caveat (if you skip Helm):** Rook v1.20 split the Ceph CSI
> Operator CRDs into `csi-operator.yaml`. Applying `crds.yaml`+`common.yaml`+
> `operator.yaml` alone fails with *"no matches for kind OperatorConfig/Driver in
> version csi.ceph.io/v1"*. Apply `csi-operator.yaml` first. The Helm chart
> bundles its own CRDs, so the quick-start above avoids this.
