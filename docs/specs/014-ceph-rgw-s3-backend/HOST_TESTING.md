# Host testing guide — SPEC-014 Ceph RGW (S3) end-to-end

Full two-cluster validation of **both** RGW capabilities on real infrastructure:

- **A — Glance image backend on RGW:** Lightstack (on k3s, real mode) stores images in
  Ceph RGW and **boots a VM by lazily fetching + checksum-verifying** the image
  base on the node (SPEC-014 FR-013).
- **B — tenant object storage:** a tenant mints S3 credentials from lightstack, and RGW
  authenticates their S3 requests via Lightstack `/v3/s3tokens` — proven by putting and
  pulling a file to an S3 bucket **from inside the guest VM** (FR-005/006/007).

```
Rook RGW on minikube  ←──S3 (glance backend)──  lightstack on k3s (real mode)
        ▲                                              │
        └────────  s3tokens (rgw keystone)  ───────────┘
image → boot VM (lazy fetch) → SSH in → in-guest: put/pull to S3 (roundtrip)
```

**Topology.** Two local clusters on one host: **k3s** runs Lightstack (real mode, so it
needs real root + KVM + host netns — rootless won't work, same constraint as
SPEC-009/010); **minikube** runs Rook+Ceph+RGW (kvm2 + extra disks). They must
reach each other on the host's libvirt network (see §1). Reference for the k3s
half: [SPEC-010 HOST-TESTING.md](../010-helm-teardown/HOST-TESTING.md).

**Run three shells** — every code block's first line says which one:
- `# T1 - host` — the host shell: `kubectl`(k3s)/`helm`, `go`/`podman` build, `ssh`, `sudo` (real-mode root).
- `# T2 - openstack` — the OpenStack CLI (`OS_*` from `~/.lightstack-env`; Lightstack is hostNetwork so `127.0.0.1:35357` works).
- `# T3 - rook` — the minikube/Rook side: `kubectl --context rook`, `minikube`, `setup-rook-rgw.sh`, and the `RGW_*` vars.
>
Cross-shell values are handed over explicitly (e.g. the RGW creds via `/tmp/rgw-creds.env` in §3 → sourced in T1/T2), so keep each shell's env as its tag implies.

| Validates | Where |
|---|---|
| FR-001/002 image round-trip stored in RGW | §8b |
| FR-013/017 boot from an RGW image on a node with no local base (lazy fetch + checksum) | §8c–§8e |
| FR-005/007 tenant mints revocable EC2 creds | §10a |
| FR-006/008 RGW authenticates the tenant via Lightstack `/v3/s3tokens`; per-project isolation | §10c–§10d |
| SC-003 in-guest S3 put/pull round-trip | §10c |
| FR-015 HTTPS/CA (optional) | §15 |

---

## 0. Prerequisites (one-time)

Host packages + libvirt/KVM (needed by **both** clusters), minikube + helm for the
Rook side, and the OpenStack CLI:
```bash
# T1 - host (root)
sudo apt install -y qemu-utils genisoimage dnsmasq iproute2 iptables postgresql-client curl
systemctl status libvirtd ; kvm-ok                 # libvirt + KVM (needed by BOTH clusters)
minikube version ; helm version                    # helm 3.13+ ; minikube drives the Rook side
# OpenStack CLI in T2:  pipx install python-openstackclient
```
**Both** clusters use libvirt/KVM. Lightstack runs on **k3s** in real mode (needs real root);
Rook runs on **minikube** with the **kvm2** driver + `--extra-disks` for Ceph OSDs.

### 0a. Install k3s (lightstack runs here, real mode)

k3s is single-binary Kubernetes; the installer sets it up as a systemd service and
symlinks `kubectl`/`ctr`. Lightstack in real mode runs `hostNetwork`+`privileged` (§7) — the
k3s node **is this host**, so the pod sees the host's libvirt/KVM directly.
```bash
# T1 - host (root)
# K3S_KUBECONFIG_MODE=644 makes the admin kubeconfig readable by your normal user;
# k3s writes /etc/rancher/k3s/k3s.yaml as root-only 0600 otherwise. Fine on a
# single-user test host (it exposes cluster-admin to any local user).
curl -sfL https://get.k3s.io | K3S_KUBECONFIG_MODE="644" sh -
sudo systemctl status k3s --no-pager | head -3 # active (running)
sudo k3s kubectl get nodes                     # → one node, STATUS Ready (give it ~30s)
```
Point `kubectl`/`helm` at k3s in your **T1** shell (used from §4 onward):
```bash
# T1 - host
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml    # now 0644 → readable without sudo
kubectl get nodes ; helm version
```
**Already installed without the flag?** `k3s.yaml` is `0600 root` — fix it in place:
`sudo chmod 644 /etc/rancher/k3s/k3s.yaml` (transient: k3s rewrites it as 0600 on
restart). To persist, add `K3S_KUBECONFIG_MODE=644` to `/etc/systemd/system/k3s.service.env`
and `sudo systemctl restart k3s`, or just run `kubectl`/`helm` via `sudo -E` / `sudo k3s kubectl`.
>
`kubectl`/`helm` run as your normal user (644 kubeconfig); only `sudo k3s ctr images
import` (§4) and `sudo mkdir /var/lib/lightstack` (§6) need root. Keep the k3s `KUBECONFIG`
**only** in T1 — the Rook side (**T3**) uses `kubectl --context rook` from minikube's
default `~/.kube/config`, so don't export the k3s KUBECONFIG there.
Deeper host/real-mode reference: [SPEC-010 §0](../010-helm-teardown/HOST-TESTING.md).

---

## 1. Clean slate + cross-cluster networking

Reset per SPEC-010 §1 (k3s/lightstack host state) and tear any prior Rook profile:
```bash
# T1 - host
deployments/rook/setup-rook-rgw.sh --delete 2>/dev/null || true   # remove the `rook` minikube profile
# k3s/lightstack reset: see SPEC-010 §1 (VMs, netns, br-*, lightstack iptables, /var/lib/lightstack, lightstack-postgres)
```
**Cross-cluster reachability is the load-bearing prerequisite** — both directions:
- Lightstack (k3s node) → RGW at `http://<minikube-ip>:30080` (glance backend, §7).
- RGW (minikube pod) → Lightstack Keystone at `http://<k3s-node-ip>:35357` (`s3tokens`, §9).

Both clusters are libvirt VMs on the same host, so their IPs are usually mutually
routable via the host. You'll capture and verify the exact IPs in §3/§7/§9.

---

## 2. Local gates first (no cluster needed)

```bash
# T1 - host
cd /home/fnk/code/sap/ora/lightstack/feature/480
go build ./...
go vet ./internal/... ./pkg/...
go test -race ./pkg/storage/ ./internal/keystone/ ./internal/common/   # SPEC-014 unit tests
go test ./internal/... ./pkg/...                                        # full unit sweep
helm lint charts/lightstack --set image.repository=lightstack --set secrets.jwtSecret=x \
  --set secrets.adminPassword=x --set database.url=postgres://x
```
**Pass** = build/vet/unit green, helm lint clean. (Skip `make test` here — its
`test/contract/*` suites need a running lightstack; run those in §8/§10.)

---

## 3. Stand up Rook Ceph RGW on minikube (T3)

Run the fixture — it brings up minikube (kvm2 + 2 OSD disks), installs Rook, creates
the CephObjectStore + an S3 user, exposes RGW on NodePort **30080**, and prints the
endpoint + keys:
```bash
# T3 - rook
deployments/rook/setup-rook-rgw.sh
```
Capture the outputs (also re-derivable any time):
```bash
# T3 - rook
export ROOK_IP=$(minikube -p rook ip)
export RGW_URL="http://${ROOK_IP}:30080"
export RGW_ACCESS=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-lightstack -o jsonpath='{.data.AccessKey}' | base64 -d)
export RGW_SECRET=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-lightstack -o jsonpath='{.data.SecretKey}' | base64 -d)
echo "RGW_URL=$RGW_URL  access=$RGW_ACCESS  secret_len=${#RGW_SECRET}"   # secret_len must be non-zero
```
**Hand these to T1** — §7's `helm install` runs in **T1**, whose kubeconfig is k3s
(no `rook` context), so `RGW_ACCESS`/`RGW_SECRET` can't be re-derived there the same
way. Persist them for T1 to source (this shell is T3):
```bash
# T3 - rook
cat > /tmp/rgw-creds.env <<EOF
export ROOK_IP='$ROOK_IP' RGW_URL='$RGW_URL'
export RGW_ACCESS='$RGW_ACCESS' RGW_SECRET='$RGW_SECRET'
EOF
```
(Or fetch them from T1 directly: prefix the two `kubectl` lines with
`KUBECONFIG=$HOME/.kube/config` — that's where minikube keeps the `rook` context.)
**Pass** = the script ends with `ROUNDTRIP OK` and `ceph status` is `HEALTH_OK`. This
RGW admin user (`RGW_ACCESS`/`RGW_SECRET`) is what lightstack's **Glance backend** uses in §7.

Pre-create the Glance bucket (lightstack does NOT create buckets):
```bash
# T3 - rook (RGW creds are set in this shell)
AWS_ACCESS_KEY_ID=$RGW_ACCESS AWS_SECRET_ACCESS_KEY=$RGW_SECRET AWS_DEFAULT_REGION=us-east-1 \
  aws --endpoint-url "$RGW_URL" s3 mb s3://images 2>/dev/null || echo "bucket exists"
```

---

## 4. Build the Lightstack image and import it into k3s (T1)

```bash
# T1 - host
cd /home/fnk/code/sap/ora/lightstack/feature/480
podman build -t docker.io/library/lightstack:dev -f build/package/Dockerfile .
podman save  docker.io/library/lightstack:dev -o /tmp/lightstack-dev.tar
sudo k3s ctr images import /tmp/lightstack-dev.tar
sudo k3s ctr images ls | grep -i lightstack        # → docker.io/library/lightstack:dev
```
(Details/gotchas — full-name tag, kubeconfig — are SPEC-010 §3/§4.)

---

## 5. External Postgres (T1)

```bash
# T1 - host
make db-up      # lightstack-postgres on :5432 (lightstack/secret/lightstack); lightstack auto-migrates on startup
```

---

## 6. Node-local storage subdirs (real mode)

```bash
# T1 - host (root)
sudo mkdir -p /var/lib/lightstack/{images,volumes,instances,cloud-init,dhcp}
```

---

## 7. Helm deploy Lightstack on k3s — REAL mode + Glance S3 backend (T1)

Point Glance at the minikube RGW and give the pod the RGW admin creds (env). Real
mode is `hostNetwork`, so `127.0.0.1` inside the pod is the host (Postgres), and the
node can reach `$ROOK_IP:30080`.

**In T1**, load the §3 RGW creds and *fail fast if any is empty* — empty values
deploy Glance against no endpoint yet still log "Glance initialized in s3 mode":
```bash
# T1 - host
source /tmp/rgw-creds.env      # from §3 (T3); or re-export RGW_URL/RGW_ACCESS/RGW_SECRET
: "${RGW_URL:?set it}" "${RGW_ACCESS:?set it}" "${RGW_SECRET:?set it}"
```
```bash
# T1 - host
helm install lightstack-rt charts/lightstack -n lightstack --create-namespace \
  --set image.repository=lightstack --set image.tag=dev --set image.pullPolicy=Never \
  --set database.url='postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" \
  --set secrets.adminPassword='Rgw-Admin-1' \
  --set nova.libvirtMode=real \
  --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables \
  --set cinder.storageMode=local \
  --set glance.storageMode=s3 \
  --set glance.s3.endpoint="$RGW_URL" \
  --set glance.s3.bucket=images \
  --set glance.s3.region=us-east-1 \
  --set-string 'extraEnv[0].name=AWS_ACCESS_KEY_ID'     --set-string "extraEnv[0].value=$RGW_ACCESS" \
  --set-string 'extraEnv[1].name=AWS_SECRET_ACCESS_KEY' --set-string "extraEnv[1].value=$RGW_SECRET" \
  --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true \
  --set podSecurity.applyPrivilegedNamespaceLabel=true

kubectl -n lightstack rollout status deploy/lightstack-rt-lightstack --timeout=300s
```
**AWS creds:** the chart must inject `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` into
the pod (Glance reads them from the environment, not `lightstack.yaml`). If your chart has no
`extraEnv` passthrough, add the two vars to the Lightstack Secret/Deployment, or set them in a
mounted `lightstack.yaml`'s process env — the pod simply needs them present.
Verify Glance init: `kubectl -n lightstack logs deploy/lightstack-rt-lightstack | grep -i 'Glance initialized in s3'`.

Confirm the pod can reach RGW:
```bash
# T1 - host
kubectl -n lightstack exec deploy/lightstack-rt-lightstack -- sh -c "wget -qO- $RGW_URL | head -c 80; echo"   # S3 XML = reachable
```

---

## 8. Capability A — image on RGW, then boot a VM from it (T2)

```bash
# T2 - openstack  (lightstack is hostNetwork, so 127.0.0.1:35357 on the host reaches Keystone)
cat > ~/.lightstack-env <<EOF
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD='Rgw-Admin-1'
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF
source ~/.lightstack-env
source /tmp/rgw-creds.env   # RGW_URL/ACCESS/SECRET (from §3) — used by the aws + in-guest checks below
openstack token issue >/dev/null && echo "auth OK"
# FR-009: the `s3` service must advertise the RGW endpoint (not migration 062's seeded
# placeholder). Look it up by type and assert the URL equals the configured RGW — the
# `catalog list -c Name | grep s3` form silently misses it (the service is named `rgw`).
S3URL=$(openstack catalog show s3 -f json | jq -r '.endpoints[] | select(.interface=="public").url')
[ "$S3URL" = "$RGW_URL" ] && echo "s3 catalog -> $S3URL ✓" || echo "MISMATCH: s3 catalog '$S3URL' != RGW_URL '$RGW_URL'"
```

### 8b. Create an image — it lands in RGW
```bash
# T2 - openstack  (RGW_* were sourced in the §8 auth block)
[ -f cirros.img ] || wget -O cirros.img https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image create cirros --disk-format qcow2 --container-format bare --file cirros.img
IMG=$(openstack image show cirros -f value -c id)
# verify the object is in the RGW bucket (uses RGW creds from /tmp/rgw-creds.env):
AWS_ACCESS_KEY_ID=$RGW_ACCESS AWS_SECRET_ACCESS_KEY=$RGW_SECRET AWS_DEFAULT_REGION=us-east-1 \
  aws --endpoint-url "$RGW_URL" s3 ls s3://images/images/     # → image-<IMG>.raw
openstack image show cirros -c checksum -f value              # non-empty (FR-002)
```
**Pass** = an `image-<id>.raw` object exists in RGW and the image has a checksum.

### 8c. Boot a VM (lazy fetch from RGW on this node)
```bash
# T2 - openstack
[ -f ~/.ssh/id_rsa.pub ] || ssh-keygen -t rsa -N '' -f ~/.ssh/id_rsa
openstack keypair create --public-key ~/.ssh/id_rsa.pub k1
openstack network create net1
openstack subnet create sub1 --network net1 --subnet-range 10.0.0.0/24
openstack server create --image cirros --flavor m1.small --network net1 --key-name k1 vm1 --wait
openstack server show vm1 -c status -f value      # ACTIVE  (Nova lazily fetched + verified the base)
```
Confirm the lazy fetch happened (base materialized on the node, fetched from RGW):
```bash
# T1 - host  ($IMG lives in T2; here just list the dir)
kubectl -n lightstack logs deploy/lightstack-rt-lightstack | grep -iE 'materialize|EnsureLocalBase|fetch' | tail
sudo ls -la /var/lib/lightstack/images/       # an image-<id>.raw is present (fetched + normalized from RGW)
```
**Pass** = `vm1` is `ACTIVE` and the raw base exists on the node (it was pulled from
RGW, not pre-placed). A checksum mismatch would have failed the boot to `ERROR`.

### 8d. SSH into the VM
```bash
# T2 - openstack
VMIP=$(openstack server show vm1 -f value -c addresses | grep -oE '10\.0\.0\.[0-9]+' | head -1)
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null cirros@"$VMIP" id
```

### 8e. Give the VM internet egress (needed to fetch `mc` + reach RGW in §10)
```bash
# T2 - openstack
openstack network create ext-net
openstack subnet create ext-sub --network ext-net --subnet-range 172.24.4.0/24 --no-dhcp
openstack router create r1 ; openstack router set r1 --external-gateway ext-net
openstack router add subnet r1 sub1
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no cirros@"$VMIP" 'ping -c2 1.1.1.1'   # egress works
```
**Guest → RGW reachability** is the tricky bit: the guest egresses via lightstack's NAT out
the k3s node, then must reach `$ROOK_IP:30080` (the minikube RGW). On a single host
this usually routes; verify from inside the guest before §10:
```bash
# T2 - openstack
ssh -i ~/.ssh/id_rsa cirros@"$VMIP" "wget -qO- $RGW_URL | head -c 40"   # S3 XML = reachable
```
If it's **`connection refused`** (the guest's `ping 1.1.1.1` still works — internet is a
different egress path), libvirt is REJECTing new inbound to its minikube network. Open
it on the k3s host — allow forwarding into the minikube subnet and masquerade so replies
route back (`ROOK_IP` comes from `/tmp/rgw-creds.env`; `source` it in T1 if unset):
```bash
# T1 - host (root)
MK_NET="${ROOK_IP%.*}.0/24"                       # e.g. 192.168.39.0/24
sudo iptables -I FORWARD 1 -d "$MK_NET" -j ACCEPT
sudo iptables -t nat -A POSTROUTING -d "$MK_NET" ! -s "$MK_NET" -j MASQUERADE
```
Re-run the guest `wget` above — it should now return S3 XML. These two rules are host
plumbing between two libvirt networks (not a Lightstack change); remove them at teardown (§11).

---

## 9. Wire RGW → Lightstack Keystone (enables tenant S3 auth) (T3)

So RGW validates tenant S3 requests against lightstack. The fixture's `--wire-keystone` mode
injects the **complete, validated** RGW→Keystone config into `rook-config-override`
and restarts the RGW: keystone url + api v3 + `implicit tenants=s3` + **admin creds**
(RGW authenticates itself to obtain a token *before* it can call `/v3/s3tokens` —
without them it fails `cannot get token for keystone access`) + `token cache size 0`
(prompt revocation). The admin password must match §7's `secrets.adminPassword`.
```bash
# T3 - rook  (read K3S_NODE_IP from the k3s kubeconfig — RGW must reach lightstack's node, NOT minikube's;
#             a bare `kubectl get node` here would return the minikube node)
K3S_NODE_IP=$(KUBECONFIG=/etc/rancher/k3s/k3s.yaml kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
echo "K3S_NODE_IP=$K3S_NODE_IP"   # sanity: lightstack's node IP (reachable from the minikube VM)
LIGHTSTACK_KEYSTONE_URL="http://${K3S_NODE_IP}:35357" LIGHTSTACK_KEYSTONE_ADMIN_PASSWORD='Rgw-Admin-1' \
  deployments/rook/setup-rook-rgw.sh --wire-keystone
```
Confirm the RGW pod can reach Lightstack Keystone (reverse cross-cluster direction):
```bash
# T3 - rook
kubectl --context rook -n rook-ceph exec deploy/rook-ceph-rgw-my-store-a -- \
  curl -s -o /dev/null -w '%{http_code}\n' "http://${K3S_NODE_IP}:35357/v3"   # 200
```
How it works: RGW authenticates itself to Lightstack with the admin creds (to obtain a
token), then POSTs each tenant S3 signature to lightstack's **public** `/v3/s3tokens`, which
recomputes the signature from the stored secret and returns the user/project/roles.
Both cross-cluster directions must work: lightstack→RGW (§7) and RGW→lightstack (the curl check above).

---

## 10. Capability B — tenant user mints S3 creds and uses the bucket from the VM (T2)

### 10a. Create a tenant project + user + EC2 creds
> **lightstack identity bug (known):** `GET /v3/{projects,users,roles}/:id` returns **500**
> (not 404) on a non-UUID id, which breaks the openstack CLI's by-**name** resolution
> (`--project tenant-a` → `GET /v3/projects/tenant-a` → 500). Work around it by addressing
> resources **by ID** — the `list` endpoints work. (Tracked: *Identity API returns 500
> instead of 404 for non-UUID ids*.)
```bash
# T2 - openstack  (resolve by ID; name lookups 500)
openstack project create tenant-a >/dev/null
PID=$(openstack project list -f value -c ID -c Name | awk '$2=="tenant-a"{print $1}')
MEMBER_RID=$(openstack role list -f value -c ID -c Name | awk '$2=="member"{print $1}')
ALICE_UID=$(openstack user create --project "$PID" --password 'Tenant-A-1' alice -f value -c id)
openstack role add --project "$PID" --user "$ALICE_UID" "$MEMBER_RID"
# authenticate AS alice and mint S3 creds:
cat > ~/.lightstack-alice <<EOF
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=alice OS_PASSWORD='Tenant-A-1'
export OS_PROJECT_NAME=tenant-a OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF
( source ~/.lightstack-alice
  openstack ec2 credentials create -f json > /tmp/alice-ec2.json )
TA_ACCESS=$(jq -r .access /tmp/alice-ec2.json); TA_SECRET=$(jq -r .secret /tmp/alice-ec2.json)
echo "tenant access=$TA_ACCESS"
```
**Pass** = an access/secret pair is returned, scoped to `tenant-a` (FR-005).

### 10b. Push `mc` + a test file into the VM
`cirros` ships no `sftp-server`, so `scp` fails (`/usr/libexec/sftp-server: not found`) —
stream over `ssh + cat` instead. And its resolver is DHCP-only (lightstack dnsmasq has DNS
disabled), so fetch `mc` on the **host** and push the binary in (don't download it in-guest):
```bash
# T2 - host
[ -f /tmp/mc ] || { wget -qO /tmp/mc https://dl.min.io/client/mc/release/linux-amd64/mc && chmod +x /tmp/mc; }
echo "hello-from-tenant-$(date -u +%s)" > /tmp/payload.txt
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no cirros@"$VMIP" 'cat > /tmp/mc && chmod +x /tmp/mc' < /tmp/mc
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no cirros@"$VMIP" 'cat > /tmp/payload.txt'            < /tmp/payload.txt
```

### 10c. Inside the VM — round-trip a file to S3 via RGW (validated by lightstack)
`cirros` uses busybox (no `bash`, no `aws-cli`), so drive it with `sh -s` and the static
`mc` pushed in §10b:
```bash
# T2 - openstack  (RGW_URL / TA_ACCESS / TA_SECRET expand from this shell)
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no cirros@"$VMIP" sh -s <<EOF
set -e
/tmp/mc alias set rgw "$RGW_URL" "$TA_ACCESS" "$TA_SECRET" --api S3v4
/tmp/mc mb rgw/alice-bucket || true          # tenant's own bucket (per-project namespace)
/tmp/mc cp /tmp/payload.txt rgw/alice-bucket/payload.txt   # PUT  → RGW → lightstack /v3/s3tokens validates
/tmp/mc cp rgw/alice-bucket/payload.txt /tmp/pulled.txt    # PULL
cmp -s /tmp/payload.txt /tmp/pulled.txt && echo "IN-VM S3 ROUNDTRIP OK"
EOF
```
**Pass** = `IN-VM S3 ROUNDTRIP OK`. That is the whole chain: guest → RGW → Lightstack
`/v3/s3tokens` (signature recomputed from alice's stored secret) → allow → object
stored under `tenant-a`'s namespace. Confirm Lightstack validated it:
```bash
# T1 - host
kubectl -n lightstack logs deploy/lightstack-rt-lightstack | grep -iE 's3token|ec2' | tail
```

### 10d. Isolation + revocation
```bash
# T2 - openstack
# revoke → subsequent requests fail (token cache is 0, so revocation is immediate)
( source ~/.lightstack-alice; openstack ec2 credentials delete "$TA_ACCESS" )
ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no cirros@"$VMIP" \
  "/tmp/mc ls rgw/alice-bucket 2>&1 | grep -qiE 'denied|forbidden|error|not exist|invalid' && echo 'REVOKED OK'"
# cross-project isolation is also covered host-side by test/tenant_s3_rgw_test.sh
```
**Pass** = after `ec2 credentials delete`, the VM's S3 access is rejected (FR-007).

---

## 11. Teardown / reset

**Between runs** — drop Lightstack + data, keep the k3s/minikube clusters:
```bash
# T1 - host  (setup-rook-rgw.sh --delete drives minikube; fine from this shell)
# lightstack side (k3s)
helm uninstall lightstack-rt -n lightstack ; kubectl delete ns lightstack --ignore-not-found
# Rook side (minikube)
deployments/rook/setup-rook-rgw.sh --delete
make db-down
# §8e guest→minikube rules, if you added them (subnet = that run's minikube /24):
sudo iptables -D FORWARD -d 192.168.39.0/24 -j ACCEPT 2>/dev/null || true
sudo iptables -t nat -D POSTROUTING -d 192.168.39.0/24 ! -s 192.168.39.0/24 -j MASQUERADE 2>/dev/null || true
# host real-mode residue: SPEC-010 §9d / SPEC-009 reset (VMs, netns, br-*, iptables, /var/lib/lightstack)
```

**Full teardown** — also remove the clusters themselves:
```bash
# T1 - host (root)
sudo /usr/local/bin/k3s-uninstall.sh    # stops k3s + removes its service, binary, containerd, and state
minikube -p rook delete                 # (setup-rook-rgw.sh --delete already does this)
```
The k3s server installer drops `k3s-uninstall.sh` in `/usr/local/bin`; an agent-only
install uses `k3s-agent-uninstall.sh`. After it, `kubectl`/`ctr` symlinks and
`/etc/rancher/k3s` are gone — re-run §0a to reinstall.

---

## 15. Troubleshooting

- **`vm1` boots to `ERROR`** → check `kubectl -n lightstack logs deploy/lightstack-rt-lightstack | grep -iE 'materialize|checksum|fetch'`. Causes: pod can't reach `$RGW_URL` (§7 wget check), bucket/creds wrong, or a checksum mismatch (fail-closed by design).
- **Glance not in s3 mode** → `... | grep 'Glance initialized in'` must say `s3`; if `local`, the `glance.storageMode`/env didn't apply.
- **`s3tokens` 401 from RGW** → the RGW pod can't reach `:35357` (§9 curl check), or the RGW keystone config didn't apply (`kubectl --context rook -n rook-ceph exec deploy/rook-ceph-rgw-my-store-a -- cat /etc/ceph/ceph.conf | grep keystone` after restart).
- **Guest can't reach RGW** (§8e / §10c) — `connection refused` while the guest's `ping 1.1.1.1` works → libvirt REJECTs new inbound to its minikube network (the ICMP "unreachable" is relayed back as `from 10.0.0.1`). Fix on the k3s host (see §8e): `sudo iptables -I FORWARD 1 -d <minikube/24> -j ACCEPT` + `sudo iptables -t nat -A POSTROUTING -d <minikube/24> ! -s <minikube/24> -j MASQUERADE`. Host plumbing between two libvirt networks, not a Lightstack bug.
- **`mc` won't run on cirros** → it's a static amd64 Go binary and normally runs; if not, boot an Ubuntu cloud image guest instead and `apt install awscli` (needs a larger flavor).
- **TLS RGW (FR-015)**: if you front RGW with HTTPS + an internal CA, add `--set glance.s3.caBundle=/etc/lightstack/rgw-ca.pem` (mount the CA into the pod) or, for a throwaway test, `--set glance.s3.insecureSkipVerify=true`.
```
