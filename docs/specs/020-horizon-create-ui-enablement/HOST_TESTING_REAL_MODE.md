# Host testing guide — Horizon create/write UI enablement F2/F3/F7/F8 (real mode, authoritative)

Validates the SPEC-020 fixes (cloud-storage#533) in **real mode**: Lightstack on a root/privileged **k3s**
cluster (real libvirt/KVM, node-local Cinder) with **Rook/Ceph on minikube** serving Glance over **RGW
(S3)**, and the verification Horizon chart (`deployments/horizon-chart/`) on k3s. The lightstack image is **built
from this branch**, so it carries the quota/limits/capability changes. This is the graded, authoritative
pass; `HOST_TESTING.md` is the portable stub pass.

The stack bring-up (Rook, the `charts/lightstack` real values, the Horizon chart) is **identical to the SPEC-018
and SPEC-019 real passes** — reused verbatim. Consult
[SPEC-019 real mode](../019-horizon-compat-api-fixes/HOST_TESTING_REAL_MODE.md) and
[SPEC-018 real mode](../018-horizon-dashboard-verification/HOST_TESTING_REAL_MODE.md) for the deep
Rook/firewall notes; §1 below is the condensed path. The **only real difference is §2**: each finding must
be **fixed**, through the **browser**, against a **stock** Horizon.

**Run all commands from the repo root.** Shell blocks are zsh-copy-paste-safe. Two clusters on one host
(k3s + minikube); estimated setup ~45–75 min on a fresh host.

**Validates**

| Finding | Success criterion | Section |
|---|---|---|
| API surface | Every create-flow quota/limits/capability endpoint 200s; limits agree across views — SC-005 | 2.1 |
| F2 | Overview usage/limits widget renders, no "Unable to retrieve limits information" — SC-004 | 2.2 |
| F3 | Project panel offers **Create Network**; network created and listed once — SC-003 | 2.3 |
| F8 | **Create Volume** dialog opens; volume created on the node-local backend — SC-002 | 2.4 |
| F7 | **Launch Instance** present; Networks step selectable; VM **boots** from the RGW image — SC-001 | 2.5 |
| Authz | No cross-project quota read/write; writes admin-only; phantom project 404s — SC-007 | 2.6 |
| Stock | Horizon config/policy unmodified — FR-018 | 2.7 |

**Beyond #533** — §2.8–§2.10 are **not** SPEC-020 acceptance criteria. They adapt the SPEC-014 end-to-end
flow (`../014-ceph-rgw-s3-backend/HOST_TESTING.md` §8c–§10c) to run through **Horizon** wherever Horizon can
drive it, so the create-UI work is exercised against something a user would actually do next: SSH into the
VM the dashboard built, and use the tenant's own S3 bucket from inside it. They validate SPEC-009
(cloud-init/SSH, north-south) and SPEC-014 (RGW, tenant object storage), not F2/F3/F7/F8. Skip them if you
only need the graded pass.

| Extra | What it proves | Section | Horizon-driven? |
|---|---|---|---|
| SSH | Key created **in Horizon** is injected via config-drive; `ssh cirros@<ip>` works | 2.8 | ✅ keypair + launch |
| Egress | Guest reaches the internet and the minikube RGW | 2.9 | ❌ CLI — see below |
| Tenant S3 | Guest round-trips an object through RGW, validated by Lightstack `/v3/s3tokens` | 2.10 | ❌ CLI |

**What Horizon cannot do here, and why.** Lightstack models **no external networks**: `router:external` is
hard-coded `false` on every network object (`internal/neutron/network.go:527,709,814`) and
`GET /v2.0/networks?router:external=true` deliberately returns `[]` (`networkListDecision`, `network.go:563`). Horizon's router
**Set Gateway** dialog populates its dropdown from exactly that filter, so it is empty and the gateway
cannot be attached from the UI. The CLI path works because `UpdateRouter` accepts
`external_gateway_info` directly (`internal/neutron/router.go:68`). Horizon also exposes **no** UI for EC2
credentials. Both are findings for the compatibility report, not defects introduced here.

## Shells

Three shells, each pinned to a context (every block is labelled `Tn — …`):
- **T1 — host + k3s** (`KUBECONFIG=~/.kube/k3s.yaml`): image build, `k3s ctr`, `helm`, `kubectl`.
- **T2 — OpenStack CLI** (drives Lightstack over `127.0.0.1`; no kubeconfig).
- **T3 — Rook/minikube** (default kubeconfig, `rook` context).

Cross-shell values (RGW endpoint + keys) pass **T3 → T1** via `/tmp/rgw.env`.

## 0. Prerequisites

This runbook installs **k3s** if absent, plus lightweight helpers. The other heavy/privileged runtimes stay
**preconditions** it only verifies: a container engine (Docker, or **Podman + `podman-docker`**),
**minikube**, `kubectl`, `helm`, **libvirt/KVM** (active `libvirtd`), the **OpenStack client**, and the
**AWS CLI**. If any are missing on a bare host, install them per the SPEC-018 real-pass Prerequisites.

**T1 — host (root):**
```bash
command -v k3s >/dev/null || curl -sfL https://get.k3s.io | sudo sh -
sudo systemctl is-active k3s || sudo systemctl start k3s
mkdir -p ~/.kube
sudo k3s kubectl config view --raw | sudo tee ~/.kube/k3s.yaml >/dev/null
sudo chown "$USER" ~/.kube/k3s.yaml
chmod 600 ~/.kube/k3s.yaml
sudo apt-get update
sudo apt-get install -y jq curl
command -v minikube >/dev/null || echo "install minikube first"
command -v helm >/dev/null || echo "install helm first"
command -v openstack >/dev/null || echo "install python-openstackclient first"
command -v aws >/dev/null || echo "install awscli first"
systemctl is-active libvirtd >/dev/null || echo "libvirtd is not active — real mode needs KVM"
echo "versions:"; { docker --version || podman --version; }; k3s --version | head -1; minikube version --short; helm version --short; kubectl version --client -o yaml | head -2; openstack --version; jq --version
```

**Pass =** every tool reports a version, `libvirtd` is active, and `~/.kube/k3s.yaml` exists.

## 1. Setup steps

### 1.1 T3 — Rook/Ceph + RGW on minikube

Reuses the existing script unchanged. It provisions the CephObjectStore, the CephObjectStoreUser `lightstack` on
store `my-store`, and the **`images`** bucket, on minikube profile `rook` with the RGW on NodePort `30080`.

The script **prints** the endpoint and keys but does **not** write `/tmp/rgw.env` — the second block does
that, and it is what every later step sources. Same capture as the SPEC-019 real pass.

**T3 — Rook/minikube:**
```bash
./deployments/rook/setup-rook-rgw.sh
```

**Pass =** the script ends with its own verification (`ROUNDTRIP OK` / `HEALTH_OK`). If it fails, stop here
and consult the SPEC-014 Rook notes — nothing below works without a working RGW.

Now capture the endpoint and keys for T1:

**T3 — Rook/minikube:**
```bash
ROOK_IP=$(minikube -p rook ip)
RGW_ACCESS=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-lightstack -o jsonpath='{.data.AccessKey}' | base64 -d)
RGW_SECRET=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-lightstack -o jsonpath='{.data.SecretKey}' | base64 -d)
printf 'export RGW_URL=%s\nexport RGW_ACCESS=%s\nexport RGW_SECRET=%s\n' "http://$ROOK_IP:30080" "$RGW_ACCESS" "$RGW_SECRET" > /tmp/rgw.env
cat /tmp/rgw.env
AWS_ACCESS_KEY_ID=$RGW_ACCESS AWS_SECRET_ACCESS_KEY=$RGW_SECRET AWS_DEFAULT_REGION=us-east-1 aws --endpoint-url "http://$ROOK_IP:30080" s3 ls
```

**Pass =** `/tmp/rgw.env` holds a non-empty `RGW_URL` matching the endpoint the script printed, and `s3 ls`
lists `images`. The variables are `RGW_URL` / `RGW_ACCESS` / `RGW_SECRET` — every later step uses those
names.

### 1.2 T1 — build the lightstack image from this branch and import it into k3s

The image must come from the working tree so it carries the SPEC-020 handlers.

Tag it **fully qualified**. Podman prefixes an unqualified local build with `localhost/`, so `-t lightstack:spec020`
lands in containerd as `localhost/lightstack:spec020` while the chart's `image.repository=lightstack` resolves to
`docker.io/library/lightstack:spec020` — a mismatch that shows up as `ImagePullBackOff` under
`pullPolicy: Never`, with no useful message.

**T1 — host + k3s:**
```bash
export KUBECONFIG=~/.kube/k3s.yaml
podman build -f build/package/Dockerfile -t docker.io/library/lightstack:spec020 .
podman save docker.io/library/lightstack:spec020 -o /tmp/lightstack-spec020.tar
sudo k3s ctr images import /tmp/lightstack-spec020.tar
sudo k3s ctr images ls | grep lightstack:spec020
```

**Pass =** `docker.io/library/lightstack:spec020` is listed in the k3s image store.

### 1.3 T1 — host Postgres and node-local storage dirs

The real-mode pod runs `hostNetwork` with no `dnsPolicy`, so it reaches Postgres at `127.0.0.1`. The
node-local directories are the ones Glance/Cinder/Nova write into (SPEC-011); §2.4 inspects
`/var/lib/lightstack/volumes` directly.

**T1 — host + k3s:**
```bash
make db-up
sudo mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp
```

**Pass =** the `lightstack-postgres` container is up on `:5432`.

### 1.4 T1 — deploy Lightstack in real mode via Helm

Real mode: libvirt/KVM compute, node-local volumes, Glance on RGW. Use a **values file**, not `--set`:
`extraEnv` is a list and cannot be expressed with `--set`, and the RGW credentials must arrive that way —
the chart does **not** read a `rgw-s3-creds` Secret (that name appears only in a values.yaml comment).

Every mode is explicit. `realMode.enabled=true` alone is not enough: the chart's defaults leave
`privileged`, `hostNetwork`, `nova.libvirtMode`, `neutron.networkingMode`, `neutron.securityGroupMode` and
`cinder.storageMode` at their stub values, so the pass would silently run stub compute/networking/storage
and §2.4/§2.5 could never succeed. `secrets.jwtSecret` and `secrets.adminPassword` are required — the chart
fails the render without them. `endpointHost` must be the in-cluster Service DNS so the catalog is
resolvable by the Horizon pod (SPEC-018 research U3).

The heredoc is **unquoted** so `$RGW_*` and `$(openssl …)` expand.

**T1 — host + k3s:**
```bash
source /tmp/rgw.env
echo "using RGW_URL=$RGW_URL  access=$RGW_ACCESS   (both must be non-empty — else re-run 1.1 in T3)"
cat > lightstack-real-values.yaml <<EOF
image: { repository: lightstack, tag: spec020, pullPolicy: Never }
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
  s3: { endpoint: "$RGW_URL", bucket: images, region: us-east-1 }
extraEnv:
  - { name: AWS_ACCESS_KEY_ID, value: "$RGW_ACCESS" }
  - { name: AWS_SECRET_ACCESS_KEY, value: "$RGW_SECRET" }
EOF
helm upgrade --install lightstack charts/lightstack -n lightstack --create-namespace -f lightstack-real-values.yaml --wait --timeout 10m
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=300s
kubectl -n lightstack logs deploy/lightstack-lightstack --tail=40 | grep -iE 'migrat|glance|hypervisor'
```

**Pass =** the lightstack pod is Ready and the logs show migrations applied, **real** hypervisor, **s3** glance and
local cinder. If it crash-loops, check `kubectl -n lightstack logs deploy/lightstack-lightstack` for the DB or RGW connection.

Verify the pod can reach RGW **before** seeding — a stale endpoint makes `image create` 503:

**T1 — host + k3s:**
```bash
source /tmp/rgw.env
kubectl -n lightstack exec deploy/lightstack-lightstack -- sh -c "wget -qO- $RGW_URL 2>&1 | head -c 120; echo"
```

**Pass =** S3 XML (`<ListAllMyBucketsResult…>` or `<Error><Code>AccessDenied`). Connection-refused means the
minikube RGW is unreachable from k3s — apply the two host iptables rules from SPEC-014 §8e and re-check.

### 1.5 T1 — deploy the verification Horizon chart

Stock kolla Horizon (Flamingo 2025.2) plus memcached and noVNC, pointed at the Lightstack Service. **Nothing in
this chart may be modified to make a control appear** (FR-018).

**T1 — host + k3s:**
```bash
helm upgrade --install horizon deployments/horizon-chart -n lightstack --set lightstack.host=lightstack-lightstack --wait --timeout 10m
kubectl -n lightstack get pods -l app.kubernetes.io/name=lightstack-horizon
kubectl -n lightstack get svc -o wide | grep -E 'horizon|NodePort'
kubectl get nodes -o wide
```

**Pass =** the Horizon pod is `Running`. The dashboard is at
`http://<k3s-node-INTERNAL-IP>:30080/dashboard` — use the node's `INTERNAL-IP` from `get nodes -o wide`.

### 1.6 T2 — authenticate and create fixtures

The launch wizard needs an image (RGW-backed), a flavor, and a network with a subnet.

The chart sets `endpointHost=lightstack-lightstack` so the catalog is resolvable by the Horizon pod. The `openstack` CLI
follows that same catalog, and Lightstack's version documents advertise the same host (`common.BaseURL` lets
`LIGHTSTACK_ENDPOINT_HOST` outrank the request Host header), so `OS_*_ENDPOINT_OVERRIDE` is discarded during
discovery. Map the name to loopback; teardown removes the line. The entry is tagged so teardown leaves any
pre-existing `lightstack-lightstack` alias from an earlier pass alone.

No port-forward is needed: the pod runs `hostNetwork` (§1.4), so every Lightstack port is already on host
loopback.

`OS_COMPUTE_API_VERSION=2.1` is required, not cosmetic: Lightstack's `GET /v2.1` version object omits the `links`
array upstream Nova returns, so keystoneauth's microversion negotiation indexes an empty list and dies with
`list index out of range`. Pinning the version skips that negotiation. (`server list` and the Glance/Neutron
commands are unaffected — only the negotiating paths crash.) Tracked as a follow-up alongside the
`common.BaseURL` self-link defect.

**T2 — OpenStack CLI:**
```bash
grep -q "spec020-hosttest" /etc/hosts || echo "127.0.0.1 lightstack-lightstack spec020-hosttest" | sudo tee -a /etc/hosts
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
export OS_COMPUTE_API_VERSION=2.1
openstack token issue -f value -c project_id
openstack flavor list -f value -c Name >/dev/null && echo "nova reachable via the catalog"
curl -sL -o /tmp/cirros.img http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img cirros
openstack network create spec020-net
openstack subnet create --network spec020-net --subnet-range 10.44.0.0/24 spec020-sub
openstack image list -f value -c Name -c Status; openstack network list -f value -c Name
```

**Pass =** the token prints a project id, `cirros` is `active` (stored in RGW), and `spec020-net` is listed
**exactly once**.

## 2. Test steps

Sections 2.1, 2.6 and 2.7 are terminal-only. Sections 2.2–2.5 are **browser** checks against stock Horizon
at the NodePort URL from 1.5 — sign in as `admin` / `secret`, domain `Default`.

### 2.1 T2 — API surface: every create-flow endpoint resolves (SC-005)

The probe walks exactly the endpoints Horizon's create flows load, then replays the create requests
Horizon sends for SC-001 – SC-004 and removes what it created (FR-028). In real mode its launch boots a real
guest from a placeholder image, so the guest boots no OS; ACTIVE is all the check needs. The pod is
`hostNetwork`, so `127.0.0.1` reaches the real-mode deployment directly.

**T2 — OpenStack CLI:**
```bash
KEYSTONE_URL=http://127.0.0.1:35357 NOVA_URL=http://127.0.0.1:8774 NEUTRON_URL=http://127.0.0.1:9696 CINDER_URL=http://127.0.0.1:8776 GLANCE_URL=http://127.0.0.1:9292 ./test/horizon_create_ui_test.sh
```

**Expect:** `Failed: 0`, of about 85 checks — the same as the stub pass, now against real backends. Before this
feature the same script reported `Passed: 15  Failed: 11`. If the total differs, check whether probe checks
were added or removed before treating it as a failure — `Failed: 0` is the criterion that matters.

### 2.2 F2 — Project Overview limits widget (SC-004)

In the browser, open **Project → Overview**.

**Expect:** the usage/limits panel renders compute, block-storage and network usage; the error
**"Unable to retrieve limits information" must NOT appear**. Reload once to confirm it is stable.

### 2.3 F3 — Create Network from the Project panel (SC-003)

In the browser, open **Project → Network → Networks** and create `ui-net` with subnet `ui-sub` /
`10.45.0.0/24` through the dialog. Then confirm in T2.

**T2 — OpenStack CLI:**
```bash
openstack network list -f value -c Name | grep -c '^ui-net$'
openstack subnet list --network ui-net -f value -c Name
```

**Expect:** a **Create Network** button on the *Project* panel (not only under Admin); the grep prints `1`
(no duplicate row) and `ui-sub` is listed.

### 2.4 F8 — Create Volume dialog, node-local backend (SC-002)

In the browser, open **Project → Volumes** and click **Create Volume**. Create a 1 GiB volume `ui-vol`.
Then confirm in T2 and on the node.

**T2 — OpenStack CLI:**
```bash
openstack volume list -f value -c Name -c Status
openstack volume show ui-vol -f json | jq -r '{type:.type, az:.availability_zone, encrypted:.encrypted}'
```

**T1 — host + k3s:**
```bash
sudo ls -l /var/lib/lightstack/volumes | tail -5
```

**Expect:** the dialog **opens** (before this feature the button did nothing), populated with a volume type
and an availability zone; `ui-vol` becomes `available`; `volume show` reports non-null type / AZ /
`encrypted` (the SPEC-019 F9 fix still holding); and a `volume-<uuid>.qcow2` file exists under
`/var/lib/lightstack/volumes`.

The reported `type` is whichever type the modal offered — with the seeded data that is **`ceph-rbd`**, which
is only a *name*: `cinder.storageMode` is `local` (§1.4) and the qcow2 under `/var/lib/lightstack/volumes` is the
proof. `__DEFAULT__` is what `GET /v3/types/default` returns, not what a created volume carries. Misleading
name, tracked separately.

### 2.5 F7 — Launch an instance from the dashboard, real VM (SC-001)

In the browser, open **Project → Compute → Instances** and use **Launch Instance**: source image `cirros`,
any flavor, and on the **Networks** step select `spec020-net`. Leave *Create New Volume* at its default
(**Yes**). Name it `ui-vm` and launch. Then confirm in T2 and on the hypervisor.

**T2 — OpenStack CLI:**
```bash
openstack server list -f value -c Name -c Status -c Networks
openstack volume list -f value -c ID -c Size -c Status -c "Attached to"
openstack server show ui-vm -f value -c volumes_attached
openstack console log show ui-vm 2>/dev/null | tail -20
```

**T1 — host + k3s:**
```bash
sudo virsh list --name | grep '^instance-' || echo "no lightstack libvirt domain"
for d in $(sudo virsh list --name | grep '^instance-'); do sudo virsh domblklist "$d"; done
```

**Expect:** a **Launch Instance** button on the Instances panel; the **Networks step renders
`spec020-net` as a selectable row** (before this feature the table was empty and Launch stayed disabled);
`ui-vm` reaches `ACTIVE` with an address on `spec020-net`; a **real libvirt domain** exists; and the
console log shows a genuine cirros boot. With *Create New Volume* on, the wizard asks for a boot volume
(FR-025). Expect one unnamed volume of the wizard's *Volume Size*, `in-use` and attached to `ui-vm` on
`/dev/vda`, and `server show` listing it under `volumes_attached` (FR-027). The domain's `vda` is `/var/lib/lightstack/volumes/volume-<id>.qcow2`, not a per-instance overlay under
`instances/`. That proves the instance booted from a volume Cinder copied out of the RGW-backed image,
after fetching the image's base onto the node. A `503 could not create the boot volume` is that copy
failing; the lightstack log names the cause.

Do **not** grep libvirt for the server name: Lightstack names domains `instance-<hex>` from the domain UUID, not
after the Nova server, and it does not populate `OS-EXT-SRV-ATTR:instance_name`. A `rook` domain in the same
list is minikube's — ignore it.

The guest's cloud-init may report `checking http://169.254.169.254/... request failed` and
`no interfaces have a carrier`. That is the metadata/L2 path, not the launch path, and it does not affect
this section's criteria — but note it, because it shares a root cause with the port-attach defect in
Troubleshooting.

### 2.6 T2 — Authorization: scoping, admin-only writes, phantom projects (SC-007)

**T2 — OpenStack CLI:**
```bash
TOKEN=$(openstack token issue -f value -c id)
PROJECT=$(openstack token issue -f value -c project_id)
echo "admin real project:"; curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" "http://127.0.0.1:8776/v3/quota-sets/$PROJECT"
echo "nova quota-set detail (want 200):"; curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" "http://127.0.0.1:8774/v2.1/os-quota-sets/$PROJECT/detail"
echo "neutron quota project scope:"; curl -s -H "X-Auth-Token: $TOKEN" "http://127.0.0.1:9696/v2.0/quotas/$PROJECT" | jq -r '.quota.project_id'
echo "phantom targets on ALL THREE services (want 404 each):"
for ref in 11111111-1111-1111-1111-111111111111 00000000-0000-0000-0000-000000000000 no-such-project; do
  printf '  %-22s nova:%s neutron:%s cinder:%s\n' "$ref" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TOKEN" "http://127.0.0.1:8774/v2.1/os-quota-sets/$ref")" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TOKEN" "http://127.0.0.1:9696/v2.0/quotas/$ref")" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TOKEN" "http://127.0.0.1:8776/v3/quota-sets/$ref")"
done
```

**Expect:** `200`, `200`, the project id echoed back, and `404` in **all nine** cells of the phantom-target
grid (SC-007a) — every service, all three malformed/unknown reference shapes.

Then the pre-existing quota suites, which are **not** run by `make test`. They assume an **exclusive**
project: delete any instance left over from §2.5 first, or `TEST 5` fails because the pre-existing VM already
consumes part of the quota it sets.

```bash
openstack server list -f value -c Name -c ID
openstack server delete ui-vm 2>/dev/null || true
./test/quota_test.sh
./test/quota_admin_test.sh
```

**Expect:** `ALL QUOTA TESTS PASSED` / `All tests passed - Quota admin check working correctly`.

**`make test-contract` is deliberately NOT part of the real pass** — run it in the stub pass
(`HOST_TESTING.md` §2.6), where it is green. The gophercloud nova suite assumes stub-mode semantics: server
create returning an instantly-`ACTIVE` instance. Under real libvirt the boots take seconds to tens of
seconds, so its wait loops expire, actions answer `409 instance must be active` instead of `202`/`400`, and
the accumulated 30-second waits blow the suite's own `-timeout 5m`. Those are harness assumptions, not Lightstack
regressions. Making that suite real-mode-aware is separate work; the SPEC-019 real pass omits it for the
same reason.

### 2.7 T1 — Confirm Horizon stayed stock (FR-018)

Per **FR-018a**, correcting an outright misconfiguration in Lightstack's *own* deployment artifacts is allowed and
is not a workaround. This feature has exactly one: `CSRF_COOKIE_HTTPONLY`, which was `True`. Horizon's
AngularJS layer reads the CSRF token from `document.cookie` and has no DOM fallback, so a write on that path
is sent with no `X-CSRFToken` and Django rejects it. Upstream Horizon does not set it. What stays barred is
any policy relaxation, feature toggle, or override added to make a control appear.

**Both copies must change.** `deployments/horizon-chart/files/local_settings` is *generated* from
`deployments/horizon-config/local_settings` by `make horizon-chart-sync`, and **real mode reads the chart
copy**. The first fix touched only the compose copy, so the real pass ran with `True` and Project → Compute →
Key Pairs → **Delete Key Pair** silently did nothing — Django logged `Forbidden (CSRF token missing.)`. Both
files are therefore expected to differ from the base here.

**T1 — host + k3s:**
"Stayed stock" is a property of the **running deployment**, so these checks read that — deliberately **no
`git diff`**. A diff against a base commit breaks the moment the branch is rebased, squashed or merged, and a
bare `git diff` breaks the moment the work is committed; neither answers whether Horizon is stock, and both
fail in ways that look like a pass. Real mode can do better than the stub runbook here: the *deployed* image
and the *live* ConfigMap are both inspectable.

```bash
# 1. The pod really is running upstream's image -- not a local build, not a patched layer.
kubectl -n lightstack get pod -l app.kubernetes.io/name=lightstack-horizon -o jsonpath='{.items[*].spec.containers[*].image}'; echo
kubectl -n lightstack get pod -l app.kubernetes.io/name=lightstack-horizon -o jsonpath='{.items[*].status.containerStatuses[*].imageID}'; echo

# 2. Horizon enforces the image's own policies: standard per-service mapping at the image's
#    own path, no policy file shipped by us, no POLICY_CHECK_FUNCTION override.
kubectl -n lightstack get cm horizon-lightstack-horizon-config -o jsonpath='{.data.local_settings}' | grep -nE '^(POLICY_FILES_PATH|POLICY_CHECK_FUNCTION)'
find deployments/horizon-config deployments/horizon-chart/files -type f -name '*policy*' | grep . && echo "POLICY FILE SHIPPED - not stock" || echo "no policy files shipped"

# 3. No feature toggle flipped to force a control to render.
kubectl -n lightstack get cm horizon-lightstack-horizon-config -o jsonpath='{.data.local_settings}' | grep -nE '^LAUNCH_INSTANCE_(LEGACY|NG)_ENABLED'

# 4. The one FR-018a correction -- in the LIVE ConfigMap, which is what the pod reads, and in
#    both on-disk copies.
kubectl -n lightstack get cm horizon-lightstack-horizon-config -o jsonpath='{.data.local_settings}' | grep -c 'CSRF_COOKIE_HTTPONLY = False'
grep -c 'CSRF_COOKIE_HTTPONLY = False' deployments/horizon-config/local_settings deployments/horizon-chart/files/local_settings

# 5. Does regenerating the chart copy change it? Snapshot, regenerate, compare. Any output is
#    drift in SOME setting, which is the class of bug F14 was -- grepping only for
#    CSRF_COOKIE_HTTPONLY would miss it.
cp deployments/horizon-chart/files/local_settings /tmp/lightstack-chart-ls.before
make horizon-chart-sync >/dev/null
diff -u /tmp/lightstack-chart-ls.before deployments/horizon-chart/files/local_settings && echo "chart copy is in sync"

# 6. And no override was injected at install time.
helm -n lightstack get values horizon
```

**Expect:** the image is `quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble` and its `imageID` resolves to
the upstream registry (`quay.io/openstack.kolla/horizon@sha256:...`), never a `localhost/` or `docker.io/library`
build; `POLICY_FILES_PATH` is the image's own `/etc/openstack-dashboard` with no `POLICY_CHECK_FUNCTION` line;
`no policy files shipped`; `LAUNCH_INSTANCE_LEGACY_ENABLED = False` with `LAUNCH_INSTANCE_NG_ENABLED = True`
(Horizon's own defaults); the live ConfigMap reports `1` and **both** on-disk copies report `1`; `chart copy is
in sync` prints; and the Helm values contain no policy or `local_settings` override.

**The live ConfigMap is the authoritative one.** If it says `True` while the files say `False`, the release was
never upgraded after the sync — that is F14's exact failure mode, and it is invisible to any check that only
reads the repository.

Any policy override, shipped policy file, other flipped toggle, or non-upstream image means the workflow it
enabled is a **documented finding**, not a fix.

If the two files disagree, the chart copy is stale — run `make horizon-chart-sync`, then `helm upgrade` the
horizon release and restart its pod so the ConfigMap is re-read.

### 2.8 SSH into the dashboard-built VM, with a key created in Horizon

Adapted from SPEC-014 §8c/§8d, but the network, subnet, keypair and instance all come from **Horizon**.
Nothing here needs the metadata service: Lightstack writes the key into a **config-drive** (cloud-init ISO)
precisely so a VM provisions without it (`internal/nova/handlers.go:679-708`). If §2.5's console log showed
`169.254.169.254 request failed`, that does **not** predict a failure here.

Generate a **throwaway** key for this pass, then import its public half through the dashboard. A dedicated
path deliberately, not `~/.ssh/id_rsa`: the runbook must never generate over, or hand a VM, the operator's
own key, and teardown can delete this one without hesitating.

**T2 — OpenStack CLI:**
```bash
[ -f ~/.ssh/id_rsa_lightstack_spec020.pub ] || ssh-keygen -t rsa -b 2048 -N '' -C 'lightstack-spec020-hosttest' -f ~/.ssh/id_rsa_lightstack_spec020
cat ~/.ssh/id_rsa_lightstack_spec020.pub
```

**Pass =** a public key line is printed. The `[ -f ... ] ||` guard makes a re-run idempotent rather than
prompting to overwrite.

In the browser, open **Project → Compute → Key Pairs**:

1. **Import Public Key** — name `ui-key`, key type SSH, paste the `id_rsa_lightstack_spec020.pub` line above. This is
   the key §2.8 uses, because the matching private key is already on the host at a known path.
2. Then also click **Create Key Pair** — name `ui-gen` — to exercise the *generate* path separately. Horizon
   must offer a private-key download; that only works if Lightstack returns `private_key`, which it does when no
   public key is supplied (`internal/nova/keypairs.go:138-149`). Delete `ui-gen` afterwards; it is a
   capability check, not a fixture.

Now launch a second instance the same way as §2.5 — **Project → Compute → Instances → Launch Instance** —
source image `cirros`, any flavor, network `spec020-net`, and on the **Key Pair** step select **`ui-key`**.
Name it `ssh-vm`. Leave *Create New Volume* at its default, so it boots from a new volume as `ui-vm` does.

**T2 — OpenStack CLI:**
```bash
openstack keypair list -f value -c Name
openstack server show ssh-vm -f value -c key_name
VMIP=$(openstack server show ssh-vm -f value -c addresses | grep -oE '10\.44\.0\.[0-9]+' | head -1)
echo "VMIP=$VMIP"
ssh -i ~/.ssh/id_rsa_lightstack_spec020 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null cirros@"$VMIP" id
```

**Expect:** `ui-key` (and `ui-gen` until you delete it) listed; `server show` reports `key_name = ui-key`
— the read-back added by #524; `id` prints `uid=1000(cirros)`. That is the whole chain: Horizon keypair →
`key_name` on the create → config-drive ISO → cloud-init → `authorized_keys`.

If SSH times out but the instance is ACTIVE, the key is not the suspect — check L2 first. Confirm the guest
got a lease and the tap is on the bridge:

**T1 — host + k3s:**
```bash
sudo virsh list --name | grep '^instance-'
ip -br link show type bridge | grep '^br-'
sudo ip netns list
```

`ip netns list` showing **only `cni-*` namespaces** (those are k3s's own) with no `light-ns-<project>` entry,
alongside present `br-*` bridges, is the known port-attach defect described in Troubleshooting — the bridge lives in the root namespace while `CreatePort` attaches inside a project
namespace that is never created. Nova builds the domain's interface itself, which is why an instance can
reach ACTIVE with an address while explicit `port create` fails.

### 2.9 Give the guest egress, and reach the RGW from inside it

**CLI-only** — see the note under *Validates*: Horizon's Set Gateway dropdown is empty because Lightstack reports
no external networks. Adapted from SPEC-014 §8e.

**T2 — OpenStack CLI:**
```bash
openstack network create ext-net
openstack subnet create ext-sub --network ext-net --subnet-range 172.24.4.0/24 --no-dhcp
openstack router create r1
openstack router set r1 --external-gateway ext-net
openstack router add subnet r1 spec020-sub
ssh -i ~/.ssh/id_rsa_lightstack_spec020 -o StrictHostKeyChecking=no cirros@"$VMIP" 'ping -c2 1.1.1.1'
```

**Expect:** two ping replies. Now check the guest can reach the minikube RGW, which is the part that usually
needs host plumbing:

**T2 — OpenStack CLI:**
```bash
source /tmp/rgw.env
ssh -i ~/.ssh/id_rsa_lightstack_spec020 -o StrictHostKeyChecking=no cirros@"$VMIP" "wget -qO- $RGW_URL | head -c 40"
```

**Expect:** S3 XML. On `connection refused` — while `ping 1.1.1.1` still works, because internet egress
takes a different path — libvirt is rejecting new inbound to its minikube network. Open it on the k3s host
(these are two libvirt networks talking to each other, not an Lightstack change; teardown removes them):

**T1 — host (root):**
```bash
source /tmp/rgw.env
MK_NET="${RGW_URL#http://}"
MK_NET="${MK_NET%:*}"
MK_NET="${MK_NET%.*}.0/24"
echo "minikube subnet: $MK_NET"
sudo iptables -I FORWARD 1 -d "$MK_NET" -j ACCEPT
sudo iptables -t nat -A POSTROUTING -d "$MK_NET" ! -s "$MK_NET" -j MASQUERADE
```

Re-run the guest `wget`; it should return S3 XML.

### 2.10 Tenant S3 round-trip from inside the VM

**CLI-only** — Horizon has no EC2-credentials UI. Adapted from SPEC-014 §9/§10. This needs the RGW wired to
Lightstack's Keystone so it can validate tenant S3 signatures.

**T3 — Rook/minikube:**
```bash
LIGHTSTACK_KEYSTONE_URL=http://$(kubectl --kubeconfig ~/.kube/k3s.yaml get nodes -o jsonpath='{.items[0].status.addresses[0].address}'):35357 ./deployments/rook/setup-rook-rgw.sh --wire-keystone
```

Mint EC2 credentials for a tenant user.

**T2 — OpenStack CLI:**
```bash
openstack project create tenant-a >/dev/null
PID=$(openstack project list -f value -c ID -c Name | awk '$2=="tenant-a"{print $1}')
MEMBER_RID=$(openstack role list -f value -c ID -c Name | awk '$2=="member"{print $1}')
ALICE_UID=$(openstack user create --project "$PID" --password 'Tenant-A-1' alice -f value -c id)
openstack role add --project "$PID" --user "$ALICE_UID" "$MEMBER_RID"
cat > ~/.lightstack-alice <<EOF
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=alice OS_PASSWORD='Tenant-A-1'
export OS_PROJECT_NAME=tenant-a OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3 OS_COMPUTE_API_VERSION=2.1
EOF
( source ~/.lightstack-alice; openstack ec2 credentials create -f json > /tmp/alice-ec2.json )
TA_ACCESS=$(jq -r .access /tmp/alice-ec2.json)
TA_SECRET=$(jq -r .secret /tmp/alice-ec2.json)
echo "tenant access=$TA_ACCESS"
```

**Expect:** an access/secret pair. `project create` sends no `domain_id` here, so it does not hit F13 (the
500 is specific to a non-UUID `domain_id` such as `default`).

`cirros` has no `sftp-server` and no DNS, so stream `mc` in over ssh rather than `scp` or an in-guest
download.

**T2 — OpenStack CLI:**
```bash
[ -f /tmp/mc ] || { wget -qO /tmp/mc https://dl.min.io/client/mc/release/linux-amd64/mc && chmod +x /tmp/mc; }
echo "hello-from-tenant-$(date -u +%s)" > /tmp/payload.txt
ssh -i ~/.ssh/id_rsa_lightstack_spec020 -o StrictHostKeyChecking=no cirros@"$VMIP" 'cat > /tmp/mc && chmod +x /tmp/mc' < /tmp/mc
ssh -i ~/.ssh/id_rsa_lightstack_spec020 -o StrictHostKeyChecking=no cirros@"$VMIP" 'cat > /tmp/payload.txt' < /tmp/payload.txt
```

Round-trip an object from inside the guest. `cirros` is busybox, so drive it with `sh -s`.

**T2 — OpenStack CLI:**
```bash
source /tmp/rgw.env
ssh -i ~/.ssh/id_rsa_lightstack_spec020 -o StrictHostKeyChecking=no cirros@"$VMIP" sh -s <<EOF
set -e
/tmp/mc alias set rgw "$RGW_URL" "$TA_ACCESS" "$TA_SECRET" --api S3v4
/tmp/mc mb rgw/alice-bucket || true
/tmp/mc cp /tmp/payload.txt rgw/alice-bucket/payload.txt
/tmp/mc cp rgw/alice-bucket/payload.txt /tmp/pulled.txt
cmp -s /tmp/payload.txt /tmp/pulled.txt && echo "IN-VM S3 ROUNDTRIP OK"
EOF
```

**Expect:** `IN-VM S3 ROUNDTRIP OK`. Confirm Lightstack did the validating, not RGW alone:

**T1 — host + k3s:**
```bash
kubectl -n lightstack logs deploy/lightstack-lightstack | grep -iE 's3token|ec2' | tail
```

**Expect:** `/v3/s3tokens` entries. Then check revocation is immediate — the fixture sets the RGW token
cache to 0:

**T2 — OpenStack CLI:**
```bash
( source ~/.lightstack-alice; openstack ec2 credentials delete "$TA_ACCESS" )
ssh -i ~/.ssh/id_rsa_lightstack_spec020 -o StrictHostKeyChecking=no cirros@"$VMIP" "/tmp/mc ls rgw/alice-bucket 2>&1 | grep -qiE 'denied|forbidden|error|not exist|invalid' && echo 'REVOKED OK'"
```

**Expect:** `REVOKED OK`.

## 3. Teardown steps

Removes the fixtures, both releases, the namespace, the Rook stack, and the temp files.

The dashboard-launched servers boot from volumes that outlive them (*Delete Volume on Instance Delete*
defaults to **No**). Each volume's id is read from the server's `volumes_attached` (FR-027) before the server
is deleted; the boot volume is the first one attached. A volume made from `cirros` keeps
the image from being deleted, so the image goes last, after both servers' volumes.

**T2 — OpenStack CLI:**
```bash
UIVOL=$(openstack server show ui-vm -f json -c volumes_attached 2>/dev/null | jq -r '.volumes_attached[0].id // empty')
openstack server delete ui-vm 2>/dev/null || true
openstack volume delete "$UIVOL" 2>/dev/null || true
openstack volume delete ui-vol 2>/dev/null || true
openstack network delete ui-net 2>/dev/null || true
rm -f /tmp/cirros.img
sudo sed -i "/spec020-hosttest/d" /etc/hosts
```

If you ran the §2.8–§2.10 extras, remove their fixtures too. The router must release the subnet and gateway
before either network can be deleted, and `spec020-net` is deleted here rather than above because the router
holds its subnet.

**T2 — OpenStack CLI (only if §2.8–§2.10 were run):**
```bash
SSHVOL=$(openstack server show ssh-vm -f json -c volumes_attached 2>/dev/null | jq -r '.volumes_attached[0].id // empty')
openstack server delete ssh-vm 2>/dev/null || true
openstack volume delete "$SSHVOL" 2>/dev/null || true
openstack keypair delete ui-key 2>/dev/null || true
openstack keypair delete ui-gen 2>/dev/null || true
openstack router unset r1 --external-gateway 2>/dev/null || true
openstack router remove subnet r1 spec020-sub 2>/dev/null || true
openstack router delete r1 2>/dev/null || true
openstack network delete ext-net 2>/dev/null || true
( source ~/.lightstack-alice 2>/dev/null && openstack ec2 credentials delete "$(jq -r .access /tmp/alice-ec2.json)" ) 2>/dev/null || true
openstack user delete alice 2>/dev/null || true
openstack project delete tenant-a 2>/dev/null || true
rm -f /tmp/mc /tmp/payload.txt /tmp/alice-ec2.json ~/.lightstack-alice
rm -f ~/.ssh/id_rsa_lightstack_spec020 ~/.ssh/id_rsa_lightstack_spec020.pub
ssh-keygen -R "$VMIP" 2>/dev/null || true
```

The key pair is throwaway and scoped to this pass, so it goes with the fixtures. `ssh-keygen -R` drops the
guest's host key, which would otherwise collide the next time the same fixed IP is handed out.

**T2 — OpenStack CLI:**
```bash
openstack network delete spec020-net 2>/dev/null || true
openstack image delete cirros 2>/dev/null || true
```

**T1 — host + k3s:**
```bash
export KUBECONFIG=~/.kube/k3s.yaml
helm -n lightstack uninstall horizon --ignore-not-found || true
helm -n lightstack uninstall lightstack --ignore-not-found || true
kubectl delete namespace lightstack --ignore-not-found
make db-down
sudo virsh list --all | grep -iE 'ui-vm|ssh-vm|instance' || echo "no leftover domains"
sudo rm -f /tmp/lightstack-spec020.tar
rm -f lightstack-real-values.yaml
kubectl -n lightstack get all 2>/dev/null || echo "namespace gone"
```

If §2.9 needed the two host iptables rules, remove them — they are host plumbing, not Lightstack state:

**T1 — host (root, only if §2.9's rules were added):**
```bash
source /tmp/rgw.env 2>/dev/null || true
MK_NET="${RGW_URL#http://}"
MK_NET="${MK_NET%:*}"
MK_NET="${MK_NET%.*}.0/24"
sudo iptables -D FORWARD -d "$MK_NET" -j ACCEPT 2>/dev/null || true
sudo iptables -t nat -D POSTROUTING -d "$MK_NET" ! -s "$MK_NET" -j MASQUERADE 2>/dev/null || true
```

**T3 — Rook/minikube:**
```bash
minikube delete --profile rook || true
rm -f /tmp/rgw.env
```

**Pass =** the `lightstack` namespace is gone, no leftover libvirt domain, and the minikube `rook` profile is
deleted. Optionally `sudo /usr/local/bin/k3s-uninstall.sh` to remove k3s itself.

## Troubleshooting

**lightstack pod crash-loops** — `kubectl -n lightstack logs deploy/lightstack-lightstack`. Usual causes: the RGW endpoint in
`/tmp/rgw.env` is not reachable from k3s (minikube IP changed after a restart — re-run 1.1 and 1.3), or the
external Postgres is down.

**Horizon 500s on a panel** — read the traceback before concluding anything:
`kubectl -n lightstack exec deploy/horizon-lightstack-horizon -- sh -c 'tail -80 /var/log/apache2/*error*'`.

**Wizard shows a network but no selectable row** — check that `project_id` is present and every subnet id
resolves: `openstack network list -f json | jq '.[0]'` then `openstack subnet list -f value -c ID`. That is
the F7 data-shape guard (contract `neutron-network-selectability.md`).

**Instance stays BUILD / never boots** — libvirt is the usual culprit: `systemctl status libvirtd`,
`sudo virsh list --all`, and `kubectl -n lightstack logs deploy/lightstack-lightstack | grep -i libvirt`. Real mode needs a
genuinely privileged k3s (rootless containers cannot reach `/var/run/libvirt/libvirt-sock`).

**`POST /v2.0/ports` returns 500 `failed to attach TAP to bridge`** — pre-existing real-mode defect, not
caused by this feature. `CreateNetwork` creates the bridge in the **root** namespace (`network.go:489`,
deliberately, "for libvirt access"), but `CreatePort` attaches the TAP **inside a project namespace**
(`ports.go:98`, `light-ns-<project>`) that is never created. Confirm with:

```bash
ip -br link show type bridge | grep '^br-'
sudo ip netns list
```

The bridge is present in the root namespace and `ip netns list` is **empty** — hence
`Device does not exist`. Dashboard instance launch is unaffected (Nova builds the domain's interface itself
rather than going through `CreatePort`), which is why §2.5 passes while `port create` fails. This also breaks
`test/contract/neutron`'s `TestPortTopologyData_Contract` and is the likely root of the guest's
metadata/`no carrier` messages in §2.5.

**A Horizon control silently does nothing — no toast, no traceback** (seen on Project → Compute → Key Pairs →
**Delete Key Pair**). Check the *apache* log, not Lightstack's: Lightstack never sees the request.

```bash
POD=$(kubectl -n lightstack get pods -l app.kubernetes.io/name=lightstack-horizon -o name | head -1)
kubectl -n lightstack exec "$POD" -- sh -c 'cat /var/log/apache2/*error* | grep -i csrf | tail -5'
kubectl -n lightstack exec "$POD" -- sh -c 'cat /var/log/apache2/*access* | grep -iE "DELETE|POST" | tail -10'
```

`Forbidden (CSRF token missing.)` means the chart's `local_settings` still has
`CSRF_COOKIE_HTTPONLY = True` — run `make horizon-chart-sync`, `helm upgrade` the horizon release, restart the
pod. The access-log tell is a `200` with a **non-empty body** on a `DELETE` (a successful Horizon ajax delete
is `204` empty), and no corresponding request in `kubectl -n lightstack logs deploy/lightstack-lightstack`. Reading Lightstack's log alone
shows nothing at all, which is easy to misread as the click never registering.

**§2.8: `ssh` times out although the instance is ACTIVE with an address** — the key is not the suspect;
config-drive injection does not depend on the metadata service. Run §2.8's L2 block. An empty
`ip netns list` beside present `br-*` bridges is the port-attach defect above.

**§2.8: Horizon's Create Key Pair offers no download** — Lightstack only returns `private_key` when the request
carries no `public_key` (`internal/nova/keypairs.go:138-149`). Check
`kubectl -n lightstack logs deploy/lightstack-lightstack | grep -i keypair`; an *Import* that reached the generate branch, or the
reverse, points at the wizard sending the wrong field.

**§2.10: `mc` reports SignatureDoesNotMatch or InvalidAccessKeyId** — RGW is not validating against Lightstack.
Re-run the §2.10 `--wire-keystone` step and confirm the RGW restarted with keystone config:
`kubectl --context rook -n rook-ceph logs -l app=rook-ceph-rgw --tail=40 | grep -i keystone`. A
`cannot get token for keystone access` line means the RGW's own admin credentials did not take.

**Known unrelated failures** (pre-existing, tracked separately, not caused by this feature):
`POST /v3/projects` returns 500 when `domain_id` is a non-UUID such as `default` (F13), and
`test/horizon_compat_test.sh`'s "Cinder - List volumes" check fails
because `GET /v3/{project_id}/volumes` is not registered (405). Both reproduce on the base commit.
