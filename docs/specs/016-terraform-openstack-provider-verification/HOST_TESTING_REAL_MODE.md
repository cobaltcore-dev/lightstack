# Host testing guide — SPEC-016 Terraform compat, REAL MODE

> The **optional real-mode** counterpart to `HOST_TESTING.md`. #481's core deliverable is the
> **stub** run + report; this doc exists so the same Terraform manifests can also be confirmed
> against a **real** Lightstack — VMs that actually boot and real Linux networking. It re-treads ground
> already proven by SPEC-009 (`docs/specs/009-single-node-real-mode/HOST-TESTING.md`); read that
> first — this only adds the Terraform layer.
> Heavier than the stub run, and **not required to close the ticket**.

Same `.tf` manifests as stub (`test/terraform/compat/`), same four AC categories. The point is
**not** new coverage — it's confirming the provider stays drop-in when Lightstack does real work.

**Topology.** Lightstack in **real mode on k3s** — the SPEC-009-proven path: libvirt/KVM, host network
namespaces, and root run **directly on the host node**, so the Lightstack pod is privileged + hostNetwork
(FR-012). Storage is **node-local** (`/var/lib/lightstack`). Two shells:
- `# T1` — cluster/admin (`sudo k3s`, `helm`, `sudo virsh`, image build/import).
- `# T2` — client (`openstack`, `terraform`).

| Validates | Where |
|---|---|
| Real VM boots from the Terraform-created instance (SC-001/005) | §3, §4 |
| Real tenant network + subnet (namespaces/bridges) | §3 |
| Security group → real iptables/ipset chain | §3 |
| Real volume created + attached (node-local) | §4 |
| apply/destroy stay clean end-to-end in real mode | §2, §5 |

---

## 0. Prerequisites — install the tools

Single shell, Ubuntu assumed (`sudo`). Lightstack runs on **k3s**. Real mode runs libvirt/KVM **on the
host** (the k3s node), so the host needs KVM — but **no nested virt**.

```bash
# 1) KVM / libvirt on the host — real-mode lightstack boots its VMs here (k3s runs on the host node)
sudo apt-get update
sudo apt-get install -y qemu-kvm libvirt-daemon-system libvirt-clients bridge-utils virtinst
sudo systemctl enable --now libvirtd
grep -Eq 'vmx|svm' /proc/cpuinfo && echo "CPU virt OK" || echo "CPU virt MISSING — real mode won't work"

# 2) k3s — runs lightstack. --write-kubeconfig-mode 644 lets a non-root kubectl read the kubeconfig.
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--write-kubeconfig-mode=644" sh -
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
sudo k3s kubectl get nodes                        # node Ready

# 3) kubectl
curl -fsSLO "https://dl.k8s.io/release/$(curl -sL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo install -m 0755 kubectl /usr/local/bin/kubectl && rm kubectl

# 4) Helm
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# 5) Terraform (HashiCorp apt repo)
wget -qO- https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /usr/share/keyrings/hashicorp-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/hashicorp-archive-keyring.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main" \
  | sudo tee /etc/apt/sources.list.d/hashicorp.list
sudo apt-get update && sudo apt-get install -y terraform

# 6) OpenStack CLI
pipx install python-openstackclient || pip install --user python-openstackclient

# 7) A container engine to build + save the lightstack image (Docker or Podman — usually already present)
docker version || podman version

# verify
k3s --version && kubectl version --client && helm version && terraform version && openstack --version
```

Then read `docs/specs/009-single-node-real-mode/HOST-TESTING.md` (§1–§15) — the battle-tested
real-mode runbook. This doc assumes its setup and only layers Terraform on top. (No host Go is
needed — the image build is multi-stage and compiles Go inside the builder image.)

---

## 1. Setup

### 1.1 Build the Lightstack image and import it into k3s

Same as stub §1.1 — tag **fully-qualified** so the name matches what the pod requests, and import
into the **`k8s.io`** containerd namespace (plain `k3s ctr images import` lands in `default`, where
pods can't see it → `ErrImageNeverPull`).

```bash
# T1 — repo root (the feature/481 checkout)
docker build -t docker.io/library/lightstack:host-test -f build/package/Dockerfile .
docker save docker.io/library/lightstack:host-test -o /tmp/lightstack-host-test.tar
sudo k3s ctr -n k8s.io images import /tmp/lightstack-host-test.tar
sudo k3s ctr -n k8s.io images ls | grep -i lightstack        # → docker.io/library/lightstack:host-test
```

### 1.2 PostgreSQL (on the host) + install Lightstack in real mode on k3s (local storage — the proven path)

**PostgreSQL runs on the host, NOT in-cluster.** Real-mode Lightstack uses `hostNetwork`, so it shares the
host's network namespace + DNS and **cannot resolve an in-cluster Service name** (`lookup
lightstack-postgres … i/o timeout`). Point it at a **host** Postgres via `127.0.0.1` — the SPEC-009/010
pattern. With no in-cluster DB to pre-create, Helm creates + owns the namespace (`--create-namespace`
+ the chart's privileged PSA label).

```bash
# T1 — repo root
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# PostgreSQL on the host (real-mode lightstack is hostNetwork → reaches it at 127.0.0.1:5432).
# `make db-up` starts the lightstack-postgres container (lightstack/secret/lightstack on :5432).
make db-up

# Install lightstack — Helm creates + OWNS the namespace and stamps the privileged PSA label on it.
# No endpointHost needed: with LIGHTSTACK_ENDPOINT_HOST unset the catalog advertises `localhost`, which
# — because lightstack is hostNetwork — IS this host, so the client reaches everything via 127.0.0.1.
helm install lightstack charts/lightstack -n lightstack --create-namespace \
  --set image.repository=docker.io/library/lightstack --set image.tag=host-test --set image.pullPolicy=Never \
  --set database.url='postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret='host-test-jwt-not-for-prod' --set secrets.adminPassword='Admin-Test-1' \
  --set nova.libvirtMode=real \
  --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables \
  --set cinder.storageMode=local --set glance.storageMode=local \
  --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true \
  --set podSecurity.applyPrivilegedNamespaceLabel=true
kubectl -n lightstack rollout status deploy -l app.kubernetes.io/name=lightstack --timeout=300s
```

With `hostNetwork=true` Lightstack binds the host directly (k3s runs on the host), so there's **no
port-forward** — use `OS_AUTH_URL=http://127.0.0.1:35357/v3` in T2 (catalog host defaults to
`localhost` = this host, so it's self-consistent).

### 1.3 Real cirros image + auth

```bash
# T2  (lightstack is hostNetwork, so 127.0.0.1:35357 on the host reaches Keystone)
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin OS_PASSWORD=Admin-Test-1 OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
# REAL image this time (it must actually boot):
curl -Lo /tmp/cirros.img https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image create cirros --disk-format qcow2 --container-format bare --file /tmp/cirros.img --public
openstack token issue -f value -c id >/dev/null && echo "auth OK"
```

---

## 2–5. Run the Terraform harness (same manifests) + verify real artifacts

```bash
# T2
cd test/terraform/compat
terraform init && terraform apply -auto-approve       # → 9 resources
```

Then confirm Lightstack did **real** work — libvirt runs **on the host** (the k3s node), so `virsh` is
local:

```bash
# T1 — on the host
sudo virsh list --all                                  # the tf-compat-vm domain is running
sudo qemu-img info -U /var/lib/lightstack/instances/*/disk.qcow2 | grep -i backing   # overlay → base image
sudo iptables -S | grep -i LS-SG                       # real security-group chain
# T2
openstack console log show tf-compat-vm | tail -20     # real serial boot output
openstack server show tf-compat-vm -f value -c status  # ACTIVE
```
**Pass** = a libvirt domain exists and is running, its overlay backs onto the base image, an
`LS-SG-*` iptables chain exists, and the console shows a real cirros boot. Then:

```bash
# T2
cd test/terraform/compat && terraform destroy -auto-approve   # → 9 destroyed
# T1
sudo virsh list --all                                          # tf-compat-vm gone
```
**Pass** = destroy removes the domain, the overlay, the netns, and the SG chain (SPEC-010 teardown
territory). Note the same `key_name`/`security_groups` read-back drift as stub (§3 of
`HOST_TESTING.md`) — it's mode-independent.

> **Also run `sc007`** (`cd test/terraform/sc007`): it adds a router + floating IP + FIP
> association (real L3 / NAT) and SSH-verifies key injection + user_data — the real-mode extras the
> compat module omits. See `test/terraform/README.md`.

---

## 6. Teardown

```bash
# T1
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
helm uninstall lightstack -n lightstack             # removes lightstack + the chart-owned namespace (psa-namespace.yaml)
make db-down                          # stop + remove the host lightstack-postgres container

# Image + temp artifacts: the docker-saved tar, the local (podman/docker) image, and the
# k3s-imported copy in containerd. The tar is only an import vehicle (docker can't run it), so all
# three are pure leftover — drop them.
rm -f /tmp/cirros.img /tmp/lightstack-host-test.tar
docker rmi docker.io/library/lightstack:host-test 2>/dev/null || true
sudo k3s ctr -n k8s.io images rm docker.io/library/lightstack:host-test 2>/dev/null || true

cd test/terraform/compat && rm -rf .terraform .terraform.lock.hcl terraform.tfstate*

# Host-local real-mode leftovers: node-local disks + any orphan libvirt domains from failed runs.
# Run only at FULL teardown — this removes ALL lightstack instance-* domains and their disks.
for d in $(sudo virsh list --all --name | grep '^instance-'); do
  sudo virsh destroy "$d" 2>/dev/null; sudo virsh undefine --remove-all-storage "$d" 2>/dev/null
done
sudo rm -rf /var/lib/lightstack/instances/* /var/lib/lightstack/volumes/*

# Full host reset (optional): sudo /usr/local/bin/k3s-uninstall.sh
```
**Pass** = `helm uninstall` succeeds and `kubectl get ns lightstack` is `NotFound`.

---

## 7. Notes / expected rough edges

- **Real-mode libvirt runs on the host** (the k3s node), not inside a VM — so `sudo virsh` on the
  host sees the domains and **no nested virt is needed** (libvirt is native on the host). If Lightstack
  logs `failed to connect to libvirt`, check
  `systemctl status libvirtd` and that the pod is privileged + hostNetwork.
- **`ErrImageNeverPull`** → the image isn't in the `k8s.io` containerd namespace under the exact
  name the pod requests; re-do §1.1 (fully-qualified tag + `-n k8s.io`), then
  `kubectl -n lightstack rollout restart deploy/lightstack-lightstack`.
- **`lookup <svc> … i/o timeout` / DB connect fails at startup** → Lightstack runs `hostNetwork` in real
  mode, so it uses the **host's** DNS and can't resolve in-cluster Service names. Run Postgres on the
  host and point `database.url` at `127.0.0.1:5432` (§1.2) — not an in-cluster `lightstack-postgres` Service.
- **`hostNetwork` port clash** — a prior host Lightstack (or another cluster) already on `:35357` will
  block bind; `pgrep -af lightstack`. See the SPEC-015 troubleshooting note.
- This doc is deliberately terse on the real-mode mechanics it shares with SPEC-009 — when in doubt,
  that HOST-TESTING is the source of truth; here we only add `terraform apply/destroy`.
