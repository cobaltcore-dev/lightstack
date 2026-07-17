# Host testing guide — SPEC-016 Terraform OpenStack-provider compatibility (stub)

End-to-end validation of [cloud-storage#481](https://github.com/cobaltcore-dev/cloud-storage/issues/481):
drive the **unmodified** Terraform OpenStack provider against a chart-deployed Lightstack and
confirm `terraform apply` + `terraform destroy` both succeed for #481's four resource
categories — **compute instances, networks, security groups, volumes**. Object storage is
out of scope (Lightstack exposes no Swift API; see lightstack#15).

**Topology.** Lightstack in **stub mode** (the chart default), deployed by the **SPEC-008 Helm
chart** on **k3s**, with one external PostgreSQL. Stub mode answers the question #481 asks —
*is the provider API-compatible with Lightstack?* — without libvirt/KVM. Real VMs and real Linux
networking are validated separately in `HOST_TESTING_REAL_MODE.md`.
The `.tf` manifests live in `test/terraform/compat/` and are identical for both modes.

**Everything runs in a single shell** on the host (the API port-forward in §1.4 runs in the
background). Ubuntu is assumed; commands use `sudo` where needed.

| Validates (AC) | Where |
|---|---|
| Representative `.tf` manifests + apply/destroy harness run against Lightstack | §2–§4 |
| Networks — `openstack_networking_network_v2` / `subnet_v2` | §3, §4 |
| Security groups — `openstack_networking_secgroup_v2` / `secgroup_rule_v2` | §3, §4 |
| Compute instances — `openstack_compute_keypair_v2` / `compute_instance_v2` / `volume_attach_v2` | §3, §4 |
| Volumes — `openstack_blockstorage_volume_v3` | §3, §4 |
| Per-resource pass/fail evidence for the report | §5 (paste back) |

---

## 0. Prerequisites — install the tools (k3s first)

Install **k3s first**: it brings up the cluster *and* the containerd that the image import in
§1.1 loads into. Then the client tools. Skip any step whose tool is already present — the final
line verifies them all.

```bash
# 1) k3s (cluster + containerd). --write-kubeconfig-mode 644 lets a non-root kubectl read it.
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--write-kubeconfig-mode=644" sh -
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
sudo k3s kubectl get nodes                        # node Ready

# 2) kubectl (standalone, so `kubectl …` works without the `k3s` prefix)
curl -fsSLO "https://dl.k8s.io/release/$(curl -sL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo install -m 0755 kubectl /usr/local/bin/kubectl && rm kubectl

# 3) Helm
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# 4) Terraform (HashiCorp apt repo)
wget -qO- https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" \
  | sudo tee /etc/apt/sources.list.d/hashicorp.list
sudo apt-get update && sudo apt-get install -y terraform

# 5) OpenStack CLI
pipx install python-openstackclient || pip install --user python-openstackclient

# 6) A container engine to build + save the lightstack image (Docker or Podman — usually already there)
docker version || podman version

# verify everything resolves
k3s --version && kubectl version --client && helm version && terraform version && openstack --version
```
**Pass** = every version prints and `kubectl get nodes` shows a `Ready` node. (No host Go is
needed — the image build is multi-stage and compiles Go inside the builder image.)

---

## 1. Setup — build, deploy (stub), expose

### 1.1 Build the Lightstack image from *this branch* and import it into k3s

Tag the image **fully-qualified** (`docker.io/library/…`) so both Docker and Podman store the
exact name the pod requests — a bare `-t lightstack:host-test` makes Podman store `localhost/lightstack:…`,
which won't match — and import into the **`k8s.io`** containerd namespace, the one the kubelet
pulls from. Plain `k3s ctr images import` lands in the `default` namespace, where pods can't see
it → `ErrImageNeverPull`.

```bash
# repo root — must be the feature/481 checkout so the image is what we test
git rev-parse --abbrev-ref HEAD                       # feature/481-…
docker build -t docker.io/library/lightstack:host-test -f build/package/Dockerfile .
docker save docker.io/library/lightstack:host-test -o /tmp/lightstack-host-test.tar
sudo k3s ctr -n k8s.io images import /tmp/lightstack-host-test.tar
sudo k3s ctr -n k8s.io images ls | grep -i lightstack        # → docker.io/library/lightstack:host-test
```
> `docker` here may be Podman (Docker-CLI-compatible) — the commands are identical. If you
> already built a bare `localhost/lightstack:host-test`, retag instead of rebuilding:
> `docker tag localhost/lightstack:host-test docker.io/library/lightstack:host-test`.

**Pass** = `sudo k3s ctr -n k8s.io images ls` lists `docker.io/library/lightstack:host-test`.

### 1.2 Deploy an external PostgreSQL (the chart deploys none — FR-006)

```bash
kubectl create namespace lightstack
kubectl -n lightstack apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: lightstack-postgres }
spec:
  replicas: 1
  selector: { matchLabels: { app: lightstack-postgres } }
  template:
    metadata: { labels: { app: lightstack-postgres } }
    spec:
      containers:
        - name: postgres
          image: postgres:18.4-alpine
          env:
            - { name: POSTGRES_DB, value: lightstack }
            - { name: POSTGRES_USER, value: lightstack }
            - { name: POSTGRES_PASSWORD, value: secret }
          ports: [{ containerPort: 5432 }]
---
apiVersion: v1
kind: Service
metadata: { name: lightstack-postgres }
spec:
  selector: { app: lightstack-postgres }
  ports: [{ port: 5432, targetPort: 5432 }]
EOF
kubectl -n lightstack rollout status deploy/lightstack-postgres --timeout=120s
```
**Pass** = the postgres deployment reports `successfully rolled out`.

### 1.3 Install Lightstack via Helm (stub-mode defaults)

```bash
# repo root
helm install lightstack charts/lightstack -n lightstack \
  --set image.repository=docker.io/library/lightstack --set image.tag=host-test --set image.pullPolicy=Never \
  --set database.url='postgres://lightstack:secret@lightstack-postgres:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret='host-test-jwt-not-for-prod' \
  --set secrets.adminPassword='Admin-Test-1' \
  --set endpointHost=localhost
# libvirtMode / networkingMode / securityGroupMode / cinder.storageMode / glance.storageMode
# all default to "stub" — no override needed.
kubectl -n lightstack rollout status deploy -l app.kubernetes.io/name=lightstack --timeout=180s
```
**Pass** = the Lightstack deployment rolls out and its pod becomes `Ready` (readiness is DB-gated
`/readyz`, so Ready also proves the DB connection).

### 1.4 Expose the API (port-forward all 6 catalog ports, in the background)

`endpointHost=localhost` makes the service catalog advertise `http://localhost:<port>`, so the
client must reach every service port on localhost — not just Keystone. The port-forward runs in
the background, so the rest of this guide continues in the same shell.

```bash
SVC=$(kubectl -n lightstack get svc -l app.kubernetes.io/name=lightstack -o jsonpath='{.items[0].metadata.name}')
kubectl -n lightstack port-forward "svc/$SVC" 35357:35357 8774:8774 8778:8778 9696:9696 8776:8776 9292:9292 > /tmp/lightstack-pf.log 2>&1 &
PF_PID=$!
sleep 3
curl -sf http://localhost:35357/v3 -o /dev/null -w 'keystone %{http_code}\n'
```
**Pass** = `keystone 200`. The port-forward runs in the background; if the curl fails, check
`/tmp/lightstack-pf.log`. `PF_PID` lets §6 stop exactly this process. (One line, no `\` continuation,
and `$!` only ever bare — inside double quotes, `!` is zsh's history-expansion character and
trips interactive paste.)

### 1.5 Authenticate + create the prerequisite Glance image

```bash
cat > /tmp/lightstack-481-env.sh <<'EOF'
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=Admin-Test-1
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
export OS_REGION_NAME=RegionOne
EOF
source /tmp/lightstack-481-env.sh
openstack token issue -f value -c id >/dev/null && echo "auth OK"
# stub Glance stores metadata only — a tiny dummy file is fine (no VM boots).
head -c 1048576 /dev/urandom > /tmp/fake-cirros.img
openstack image create --disk-format qcow2 --container-format bare \
  --file /tmp/fake-cirros.img --public cirros -f value -c name -c status
openstack flavor list -f value -c Name        # m1.tiny … m1.xlarge are seeded
```
**Pass** = `auth OK`, the image prints `cirros` / `active`, and the flavor list includes
`m1.small`.

---

## 2. Test — `terraform init` + `apply`

```bash
cd "$(git rev-parse --show-toplevel)/test/terraform/compat"
source /tmp/lightstack-481-env.sh
terraform init
terraform apply -auto-approve
```
**Pass** = `Apply complete! Resources: 9 added, 0 changed, 0 destroyed.` with the five
outputs (`instance_id`, `network_id`, `subnet_id`, `secgroup_id`, `volume_id`) populated, and
**no** provider modification. The 9 resources are: network, subnet, security group + 2 rules,
keypair, instance, volume, volume-attach.

---

## 3. Test — verify each resource landed (and the known read-back drift)

```bash
source /tmp/lightstack-481-env.sh
openstack server  show tf-compat-vm  -f value -c status            # ACTIVE
openstack volume  show tf-compat-vol -f value -c status            # in-use (attach worked)
openstack network show tf-compat-net -f value -c status            # ACTIVE
openstack security group rule list tf-compat-sg -f value | wc -l   # ≥ 2 (ssh + icmp; +defaults)
openstack server show tf-compat-vm -f json | \
  python3 -c 'import sys,json;d=json.load(sys.stdin);print("addresses:",d.get("addresses"))'
# Known deviation — re-plan is NOT idempotent (document, do not "fix" here):
terraform plan -no-color | grep -E 'No changes|must be replaced|forces replacement' | head
```
**Pass** = server `ACTIVE`, volume `in-use`, network `ACTIVE`, ≥2 SG rules, `addresses`
populated with a fixed IP. **Expected deviation:** `terraform plan` reports the instance
`must be replaced` because `key_pair … forces replacement` — Lightstack's Nova server-detail omits
`key_name` (and `security_groups`), so the provider can't read them back. Create/destroy still
work; only a second `apply` on the same state churns the instance. Record whatever you see.

---

## 4. Test — `terraform destroy` (clean teardown)

```bash
cd "$(git rev-parse --show-toplevel)/test/terraform/compat"
source /tmp/lightstack-481-env.sh
terraform destroy -auto-approve
echo "residue (each count must be 0; an error line here is a FAIL, not a 0):"
printf '  server         %s\n' "$(openstack server list -f value -c Name | grep -c tf-compat)"
printf '  network        %s\n' "$(openstack network list -f value -c Name | grep -c tf-compat)"
printf '  security group %s\n' "$(openstack security group list -f value -c Name | grep -c tf-compat)"
printf '  volume         %s\n' "$(openstack volume list -f value -c Name | grep -c tf-compat)"
printf '  keypair        %s\n' "$(openstack keypair list -f value -c Name | grep -c tf-compat)"
```
**Pass** = `Destroy complete! Resources: 9 destroyed.` and every residue count is `0`.

---

## 5. Collect results (paste these back)

Paste the output of these so the report can be written from evidence:

```bash
source /tmp/lightstack-481-env.sh
echo "=== versions ==="; terraform version | head -1; openstack --version
echo "=== helm release ==="; helm -n lightstack list
echo "=== pods ==="; kubectl -n lightstack get pods
cd "$(git rev-parse --show-toplevel)/test/terraform/compat"
terraform apply -auto-approve -no-color > /tmp/lightstack-481-apply.log 2>&1; apply_rc=$?
echo "=== apply tail (exit $apply_rc) ==="; tail -20 /tmp/lightstack-481-apply.log
echo "=== drift ==="; terraform plan -no-color 2>&1 | grep -E 'No changes|Plan:|forces replacement' | head
terraform destroy -auto-approve -no-color > /tmp/lightstack-481-destroy.log 2>&1; destroy_rc=$?
echo "=== destroy tail (exit $destroy_rc) ==="; tail -15 /tmp/lightstack-481-destroy.log
```
Both `exit` values must be `0`; a non-zero one is a **FAIL** even if the tail looks clean, so
paste the full log (`/tmp/lightstack-481-apply.log`, `/tmp/lightstack-481-destroy.log`) in that case.
Also note anything that **failed** — which `openstack_*` resource, the provider error, and the
Lightstack pod log line (`kubectl -n lightstack logs -l app.kubernetes.io/name=lightstack --tail=50`).

---

## 6. Teardown

```bash
kill "$PF_PID" 2>/dev/null || pkill -f 'port-forward.*35357'   # stop the background port-forward
cd "$(git rev-parse --show-toplevel)/test/terraform/compat" && rm -rf .terraform .terraform.lock.hcl terraform.tfstate*
helm uninstall lightstack -n lightstack
kubectl delete namespace lightstack          # removes lightstack + the test PostgreSQL (all data)
rm -f /tmp/lightstack-host-test.tar /tmp/fake-cirros.img /tmp/lightstack-481-env.sh /tmp/lightstack-pf.log /tmp/lightstack-481-apply.log /tmp/lightstack-481-destroy.log
# Full reset (optional): uninstall k3s entirely
#   /usr/local/bin/k3s-uninstall.sh
```
**Pass** = `helm uninstall` succeeds and `kubectl get ns lightstack` is `NotFound`. Because the
namespace holds both Lightstack and the test PostgreSQL, deleting it drops all test data — no manual
resource cleanup needed.

---

## 7. Troubleshooting

- **Pod `ErrImageNeverPull` / `ImagePullBackOff`** → the image isn't in the `k8s.io` containerd
  namespace under the exact name the pod requests. Two usual causes: (a) it was imported into the
  `default` namespace — re-import with `sudo k3s ctr -n k8s.io images import …`; (b) Podman stored
  it as `localhost/lightstack:host-test` — retag to `docker.io/library/lightstack:host-test`, re-save,
  re-import (§1.1). Verify `sudo k3s ctr -n k8s.io images ls | grep lightstack`, then
  `kubectl -n lightstack rollout restart deploy/lightstack-lightstack`.
- **Pod not Ready, `/readyz` failing** → Lightstack can't reach PostgreSQL. Check
  `kubectl -n lightstack logs -l app.kubernetes.io/name=lightstack --tail=50` and that §1.2's `lightstack-postgres`
  service is up; the `database.url` host must be `lightstack-postgres` (in-cluster DNS).
- **`terraform apply` hangs creating the instance / volume** → a catalog port isn't
  forwarded. §1.4 must forward **all six** ports; `endpointHost=localhost` means the client
  follows `http://localhost:<port>` for every service.
- **`openstack token issue` → 401** → `OS_PASSWORD` must equal the `secrets.adminPassword` you
  set in §1.3 (`Admin-Test-1` here), not the built-in `secret`.
- **`Could not resolve … cirros`** on apply → the §1.5 image create was skipped or landed in
  another project; re-run it as admin and confirm `openstack image list` shows `cirros`
  `active`.
- **`terraform plan` shows the instance replacing every time** → expected (see §3); it is a
  read-back gap in Lightstack's Nova server detail, not a harness bug.
