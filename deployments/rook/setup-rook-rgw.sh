#!/usr/bin/env bash
# setup-rook-rgw.sh — stand up a single-node Rook Ceph cluster with an RGW (S3)
# endpoint on a dedicated minikube profile, for validating Lightstack's Glance S3 backend
# against real Ceph RGW (cloud-storage#480).
#
# Rook is installed via its official Helm chart — nothing from Rook is vendored
# into this repo. Only the small CephCluster / CephObjectStore / user / Service
# CRs below are ours (tuned for a single node so Ceph reaches HEALTH_OK, not a
# degraded state).
#
# Usage:
#   ./setup-rook-rgw.sh            # bring everything up (idempotent-ish)
#   ./setup-rook-rgw.sh --delete   # tear the whole profile down
#
#   # capability B (SPEC-014 #480): wire the RGW's S3 auth to Lightstack Keystone so
#   # tenant EC2 keys validate via /v3/s3tokens. Run AFTER Lightstack is up and reachable
#   # from this cluster (pass Lightstack's Keystone URL — cross-cluster, use a routable IP):
#   LIGHTSTACK_KEYSTONE_URL=http://<lightstack-node-ip>:35357 ./setup-rook-rgw.sh --wire-keystone
#
# Requirements: minikube (kvm2 driver + libvirt), kubectl, helm 3.13+, and
# optionally the aws CLI for the end-to-end S3 smoke test.
set -euo pipefail

# --- config (override via env) ---
PROFILE="${PROFILE:-rook}"
NS="${NS:-rook-ceph}"
STORE="${STORE:-my-store}"
OBJ_USER="${OBJ_USER:-lightstack}"
NODEPORT="${NODEPORT:-30080}"
# Validated pair (manual run 2026-07): Rook v1.20.1 + Ceph v20.2.2 (tentacle).
# If helm can't find the chart version, run:
#   helm search repo rook-release/rook-ceph --versions
ROOK_CHART_VERSION="${ROOK_CHART_VERSION:-v1.20.1}"
CEPH_IMAGE="${CEPH_IMAGE:-quay.io/ceph/ceph:v20.2.2}"
MK_MEMORY="${MK_MEMORY:-8192}"
MK_CPUS="${MK_CPUS:-4}"
MK_DISK="${MK_DISK:-40g}"
MK_EXTRA_DISKS="${MK_EXTRA_DISKS:-2}"   # raw disks -> OSDs; needs a VM driver

# capability-B (RGW -> Lightstack Keystone) wireup — used only by --wire-keystone.
# LIGHTSTACK_KEYSTONE_URL has no default: it is Lightstack's Keystone as reachable FROM this
# cluster (cross-cluster, so a node IP + :35357, not localhost/a cluster DNS name).
LIGHTSTACK_KEYSTONE_URL="${LIGHTSTACK_KEYSTONE_URL:-}"
KS_ADMIN_USER="${LIGHTSTACK_KEYSTONE_ADMIN_USER:-admin}"
KS_ADMIN_PASSWORD="${LIGHTSTACK_KEYSTONE_ADMIN_PASSWORD:-secret}"
KS_ADMIN_PROJECT="${LIGHTSTACK_KEYSTONE_ADMIN_PROJECT:-default}"
KS_ADMIN_DOMAIN="${LIGHTSTACK_KEYSTONE_ADMIN_DOMAIN:-Default}"
KS_ROLES="${LIGHTSTACK_KEYSTONE_ACCEPTED_ROLES:-admin,member,reader,_member_}"

CTX="$PROFILE"                          # minikube names the context after the profile
K="kubectl --context $CTX -n $NS"       # every op is pinned to the rook profile,
                                        # so this never touches your other clusters

log(){ printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die(){ printf '\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }
need(){ command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }

# wire_keystone — point the RGW's S3 auth at Lightstack Keystone (SPEC-014 #480 capability
# B). Injects the rgw keystone options into the rook-config-override ConfigMap
# (preserving the single-node tuning) and restarts the RGW so it re-reads them
# (rgw_keystone_url is startup-only). These are the exact options validated against
# real RGW in the ceph/demo fixture (deployments/rgw-keystone-entrypoint.sh):
#   - admin_user/password/project/domain: RGW must obtain its OWN token before it
#     can POST /v3/s3tokens (without them: "cannot get token for keystone access").
#   - implicit tenants = s3: per-project bucket namespaces (tenant isolation).
#   - token cache size = 0: a revoked EC2 credential is refused on the next request.
# Path-style: clients must use the NodePort IP endpoint (RGW treats an IP host as
# the endpoint, so no rgw_dns_name override is needed here).
# The local RGW user (Glance's, capability A) keeps working — RGW tries local first,
# then Keystone.
wire_keystone(){
  need minikube; need kubectl
  [ -n "$LIGHTSTACK_KEYSTONE_URL" ] || die "set LIGHTSTACK_KEYSTONE_URL=http://<lightstack-node-ip>:35357 (Lightstack Keystone as reachable from the '$PROFILE' cluster)"

  log "Checking Lightstack Keystone reachability from the '$PROFILE' cluster ($LIGHTSTACK_KEYSTONE_URL/v3)"
  $K delete pod ks-probe --ignore-not-found >/dev/null 2>&1 || true
  if $K run ks-probe --image=curlimages/curl:latest --restart=Never --rm -i --quiet \
       -- -m 8 -sf "$LIGHTSTACK_KEYSTONE_URL/v3" >/dev/null 2>&1; then
    echo "  reachable"
  else
    echo "  WARNING: could not reach $LIGHTSTACK_KEYSTONE_URL/v3 from the cluster — fix cross-cluster routing or tenant auth will fail"
  fi

  log "Injecting RGW->Keystone S3 auth into rook-config-override"
  $K apply -f - <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: rook-config-override
data:
  config: |
    [global]
    osd pool default size = 2
    osd crush chooseleaf type = 0
    rgw s3 auth use keystone = true
    rgw keystone url = ${LIGHTSTACK_KEYSTONE_URL}
    rgw keystone api version = 3
    rgw keystone implicit tenants = s3
    rgw keystone accepted roles = ${KS_ROLES}
    rgw keystone verify ssl = false
    rgw keystone admin user = ${KS_ADMIN_USER}
    rgw keystone admin password = ${KS_ADMIN_PASSWORD}
    rgw keystone admin project = ${KS_ADMIN_PROJECT}
    rgw keystone admin domain = ${KS_ADMIN_DOMAIN}
    rgw keystone token cache size = 0
YAML

  log "Restarting RGW to apply (keystone url is startup-only)"
  $K delete pod -l app=rook-ceph-rgw,rook_object_store="$STORE" --ignore-not-found
  for _ in $(seq 1 36); do
    ready=$($K get pod -l app=rook-ceph-rgw,rook_object_store="$STORE" \
      -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c True || true)
    [ "${ready:-0}" -ge 1 ] && { echo "  RGW Ready"; break; }
    sleep 5
  done

  IP=$(minikube -p "$PROFILE" ip)
  cat <<EOF

============================================================
RGW S3 auth wired to lightstack Keystone (capability B).

  RGW endpoint:   http://${IP}:${NODEPORT}   (path-style — use the IP)
  Keystone:       ${LIGHTSTACK_KEYSTONE_URL}
  Validates via:  ${LIGHTSTACK_KEYSTONE_URL}/v3/s3tokens
  implicit tenants = s3   (per-project bucket namespaces)
  token cache = 0         (revocation takes effect immediately)

Tenant flow (run on the lightstack side, then use any S3 client against the endpoint above):
  openstack ec2 credentials create -f value -c access -c secret
  AWS_ACCESS_KEY_ID=<access> AWS_SECRET_ACCESS_KEY=<secret> \\
    <s3 client> --endpoint-url http://${IP}:${NODEPORT} ...   # path-style

Notes:
  - Glance's LOCAL RGW user (${OBJ_USER}) keeps working alongside Keystone.
  - lightstack /v3/s3tokens must be reachable from THIS cluster (cross-cluster routing).
  - Re-run without --wire-keystone to reset, or --delete to tear down.
============================================================
EOF
}

if [ "${1:-}" = "--delete" ]; then
  log "Tearing down minikube profile '$PROFILE'"
  minikube -p "$PROFILE" delete
  exit 0
fi

if [ "${1:-}" = "--wire-keystone" ]; then
  wire_keystone
  exit 0
fi

# --- preflight ---
need minikube; need kubectl; need helm
HAVE_AWS=1; command -v aws >/dev/null 2>&1 || { HAVE_AWS=0; echo "note: aws CLI not found — the S3 smoke test will be skipped"; }

# --- 1) minikube with raw OSD disks ---
if minikube -p "$PROFILE" status >/dev/null 2>&1; then
  log "minikube profile '$PROFILE' already running"
else
  log "Starting minikube profile '$PROFILE' (kvm2, ${MK_EXTRA_DISKS} extra disks)"
  minikube start -p "$PROFILE" --driver=kvm2 \
    --memory="$MK_MEMORY" --cpus="$MK_CPUS" --disk-size="$MK_DISK" \
    --extra-disks="$MK_EXTRA_DISKS"
fi

# --- 2) Rook operator via Helm (nothing vendored) ---
log "Installing Rook operator (chart ${ROOK_CHART_VERSION})"
helm repo add rook-release https://charts.rook.io/release >/dev/null 2>&1 || true
helm repo update rook-release >/dev/null 2>&1 || helm repo update >/dev/null
helm --kube-context "$CTX" upgrade --install rook-ceph rook-release/rook-ceph \
  --namespace "$NS" --create-namespace --version "$ROOK_CHART_VERSION" --wait

# --- 3) single-node ceph.conf overrides (so Ceph reaches HEALTH_OK on one host) ---
# chooseleaf type 0 = replicate across OSDs, not hosts; default pool size 2.
log "Applying rook-config-override (single-node)"
$K apply -f - <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: rook-config-override
data:
  config: |
    [global]
    osd pool default size = 2
    osd crush chooseleaf type = 0
YAML

# --- 4) CephCluster: 1 mon, both extra disks, no oversized resource requests ---
log "Creating CephCluster"
$K apply -f - <<YAML
apiVersion: ceph.rook.io/v1
kind: CephCluster
metadata:
  name: rook-ceph
spec:
  cephVersion:
    image: ${CEPH_IMAGE}
  dataDirHostPath: /var/lib/rook
  mon:
    count: 1
    allowMultiplePerNode: true
  mgr:
    count: 1
  dashboard:
    enabled: false
  storage:
    useAllNodes: true
    useAllDevices: true
YAML

log "Waiting for Ceph HEALTH_OK (several minutes on first run)"
for _ in $(seq 1 60); do
  h=$($K get cephcluster rook-ceph -o jsonpath='{.status.ceph.health}' 2>/dev/null || true)
  echo "  ceph health: ${h:-<pending>}"
  [ "$h" = "HEALTH_OK" ] && break
  sleep 15
done
[ "$($K get cephcluster rook-ceph -o jsonpath='{.status.ceph.health}' 2>/dev/null)" = "HEALTH_OK" ] || \
  die "cluster did not reach HEALTH_OK — inspect: $K get cephcluster rook-ceph; $K get pods"

# --- 5) RGW object store + S3 user (pools sized for a single node) ---
log "Creating CephObjectStore '${STORE}'"
$K apply -f - <<YAML
apiVersion: ceph.rook.io/v1
kind: CephObjectStore
metadata:
  name: ${STORE}
spec:
  metadataPool:
    failureDomain: osd
    replicated: { size: 2 }
  dataPool:
    failureDomain: osd
    replicated: { size: 2 }
  preservePoolsOnDelete: false
  gateway:
    port: 80
    instances: 1
YAML

log "Waiting for object store to be Ready"
for _ in $(seq 1 40); do
  p=$($K get cephobjectstore "$STORE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  echo "  store phase: ${p:-<pending>}"
  [ "$p" = "Ready" ] && break
  sleep 10
done

log "Creating CephObjectStoreUser '${OBJ_USER}'"
$K apply -f - <<YAML
apiVersion: ceph.rook.io/v1
kind: CephObjectStoreUser
metadata:
  name: ${OBJ_USER}
spec:
  store: ${STORE}
  displayName: "Lightstack glance"
YAML

secret="rook-ceph-object-user-${STORE}-${OBJ_USER}"
log "Waiting for user secret (${secret})"
until $K get secret "$secret" >/dev/null 2>&1; do sleep 3; done

# --- 6) durable NodePort endpoint (a separate Service the operator won't revert) ---
log "Exposing RGW on NodePort ${NODEPORT}"
tp=$($K get svc "rook-ceph-rgw-${STORE}" -o jsonpath='{.spec.ports[0].targetPort}')
$K apply -f - <<YAML
apiVersion: v1
kind: Service
metadata:
  name: rgw-${STORE}-ext
spec:
  type: NodePort
  selector:
    app: rook-ceph-rgw
    rook_object_store: ${STORE}
  ports:
  - name: http
    port: 80
    targetPort: ${tp}
    nodePort: ${NODEPORT}
    protocol: TCP
YAML

ACCESS=$($K get secret "$secret" -o jsonpath='{.data.AccessKey}' | base64 -d)
SECRET=$($K get secret "$secret" -o jsonpath='{.data.SecretKey}' | base64 -d)
IP=$(minikube -p "$PROFILE" ip)
RGW_EXT="http://${IP}:${NODEPORT}"

# --- 7) optional S3 round-trip ---
SMOKE="skipped (aws CLI not installed)"
if [ "$HAVE_AWS" = 1 ]; then
  log "S3 round-trip against ${RGW_EXT}"
  export AWS_ACCESS_KEY_ID="$ACCESS" AWS_SECRET_ACCESS_KEY="$SECRET" AWS_DEFAULT_REGION=us-east-1
  aws configure set default.s3.addressing_style path
  tmp="$(mktemp)"; echo "hello-rgw" > "$tmp"; smoke_ok=1
  aws --endpoint-url "$RGW_EXT" s3 mb s3://images >/dev/null 2>&1 || true   # ok if exists
  aws --endpoint-url "$RGW_EXT" s3 cp "$tmp" s3://images/smoke.txt >/dev/null 2>&1 || smoke_ok=0
  aws --endpoint-url "$RGW_EXT" s3 cp s3://images/smoke.txt "${tmp}.back" >/dev/null 2>&1 || smoke_ok=0
  if [ "$smoke_ok" = 1 ] && diff -q "$tmp" "${tmp}.back" >/dev/null 2>&1; then
    SMOKE="ROUNDTRIP OK"
  else
    SMOKE="FAILED — check ${RGW_EXT} reachability/creds"
  fi
  rm -f "$tmp" "${tmp}.back"
fi

cat <<EOF

============================================================
Rook Ceph RGW fixture ready.   S3 smoke test: ${SMOKE}

  S3 endpoint:   ${RGW_EXT}
  Bucket (ex):   images
  Access key:    ${ACCESS}
  Secret key:    ${SECRET}
  Region:        us-east-1  (path-style)

lightstack Glance (storage_mode: s3):
  glance:
    storage_mode: s3
    s3_endpoint: ${RGW_EXT}
    s3_bucket:   images
    s3_region:   us-east-1
  env: AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY  (values above)

Capability B (tenant object storage) — after lightstack is up and reachable:
  LIGHTSTACK_KEYSTONE_URL=http://<lightstack-node-ip>:35357 $0 --wire-keystone

Teardown:  $0 --delete
============================================================
EOF
