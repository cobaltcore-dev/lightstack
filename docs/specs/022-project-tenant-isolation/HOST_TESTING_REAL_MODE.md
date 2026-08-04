# Host testing guide — Project/tenant lifecycle & isolation fixes (real mode)

The real-mode counterpart of [HOST_TESTING.md](./HOST_TESTING.md): the same eight checks for
[SPEC-022](./spec.md) / [cloud-storage#545](https://github.com/cobaltcore-dev/cloud-storage/issues/545),
run against **o3k on k3s via Helm** with **Horizon** deployed, Nova on **libvirt**, Neutron on
**iptables + network namespaces**, and Glance backed by **Ceph RGW (S3)** from a single-node
**Rook** cluster on minikube.

**Two things here are stronger than the stub run, not merely equivalent:**

- **§2.4** — in stub mode `applyDHCPForNetwork` returns immediately (`internal/neutron/reconciler.go:232`),
  so FR-010's "refuse **before** any address-assignment work" is unobservable. In real mode it starts
  dnsmasq and assigns a bridge address, so the ordering guarantee becomes a thing you can actually see.
- **§2.5** — Cinder writes real qcow2 backing files, so the project-scoped routes are exercised against
  volumes that physically exist.

**Run all commands from the repo root.** Blocks use repo-relative paths and never `cd`.

## Shells

Three terminals, two clusters. Every block is labelled with the shell it belongs to, and blocks are
merged as far as the shell boundaries allow — so a section showing two labels needs two pastes and no
more.

| Shell | Role | Environment it holds |
|---|---|---|
| **T1** | the k3s host — image build, Helm, node-local state, `make` targets | `KUBECONFIG=~/.kube/k3s.yaml` |
| **T2** | the client — OpenStack CLI, `curl`, `boto3` | `OS_*`, `AT`, `KS`, `NEU`, `CIN`, tenant ids and tokens |
| **T3** | the Rook cluster — minikube profile `rook` | `--context rook`, `ROOK_IP`, `RGW_*` |

**Two hand-off files carry state between shells**, because a variable exported in one terminal does not
exist in another — and a check that silently reads an empty variable is worse than one that fails:

| File | Written by | Read by |
|---|---|---|
| `/tmp/rgw-creds.env` | §1.1 (T3) | §1.2 (T1) |
| `/tmp/spec022-env.sh` | §1.3 (T2), appended by §2.3 | §2.4, §2.6 (T1) |

### Block and shell map

| § | Blocks | Shell order |
|---|---|---|
| 0 Prerequisites | 1 | T1 |
| 1 Setup | 3 | T3 → T1 → T2 |
| 2.1 · 2.2 · 2.3 | 1 each | T2 |
| 2.4 cross-tenant subnet write 🔒 | 4 | T2 → T1 → T2 → T1 |
| 2.4a owner may inspect/remove a foreign subnet | 2 | T2 |
| 2.5 volume routes | 2 | T2 → T1 |
| 2.6 multi-tenancy suite | 2 | T2 → T1 |
| 2.7 Horizon · 2.8 gates | 1 each | T1 |
| 3 Teardown | 4 | T2 → T1 → T3 → T1 (optional) |

**21 blocks total.** Where a section still has more than one, it is because the shell must change
mid-step, not because the commands could not be joined.

> **Not yet executed end to end.** Every command comes from a runbook validated on this host
> (`docs/specs/021-real-novnc-console/HOST_TESTING_REAL_MODE.md` for the k3s/Helm/Horizon half,
> `docs/specs/014-ceph-rgw-s3-backend/HOST_TESTING.md` for the Rook/RGW half) or from a script in this
> repo. §2's expectations are the values a run on this host actually produced.

## Validates

| Acceptance criterion | Section |
|---|---|
| US1 / SC-001, SC-002 — a tenant can be created | [2.1](#21-b5p1--tenant-creation-with-the-default-domain-alias) |
| US1 / SC-001a, FR-001a — client-side `--domain` resolution | [2.2](#22-b5--domain-retrieval-by-alias-and-the-name-filter) |
| US2 / SC-003, SC-004, SC-004a, SC-004b — no cross-tenant subnet write 🔒 | [2.4](#24-b8--the-cross-tenant-subnet-write-real-mode-) |
| US2 / FR-013a — the network owner may inspect and remove a foreign subnet on their network | [2.4a](#24a-fr-013a--the-network-owner-may-inspect-and-remove-what-an-admin-put-on-their-network) |
| US3 / SC-005, SC-005a — both volume URL forms agree | [2.5](#25-b2--the-project-scoped-volume-routes-real-storage) |
| US4 / SC-006, SC-006a — the suite reports on something real | [2.6](#26-us4--the-multi-tenancy-suite) |
| SC-008 — no regression; Horizon works | [2.7](#27-sc-008--horizon), [2.8](#28-sc-008--the-full-gate-set) |

---

## 0. Prerequisites

Heavy or privileged preconditions are **verified, not installed**: a container engine, `helm`,
`minikube` with the **kvm2** driver, and libvirt/KVM. k3s is installed if absent — it is the thing under
test. Everything else is a lightweight CLI the steps invoke.

**Shell T1** — one paste:

```bash
sudo apt-get update -qq
sudo apt-get install -y curl jq make python3-openstackclient python3-boto3 libvirt-clients qemu-kvm postgresql-client
sudo systemctl enable --now libvirtd
command -v docker >/dev/null || command -v podman >/dev/null || echo "MISSING: install docker or podman first"
command -v helm >/dev/null || echo "MISSING: install helm 3.13+ first"
command -v minikube >/dev/null || echo "MISSING: install minikube with the kvm2 driver first"
command -v aws >/dev/null || echo "OPTIONAL: aws CLI absent; boto3 covers every S3 step in this guide"
command -v k3s >/dev/null || curl -sfL https://get.k3s.io | sudo sh -
sudo systemctl is-active k3s >/dev/null || sudo systemctl start k3s
mkdir -p ~/.kube
sudo k3s kubectl config view --raw | sudo tee ~/.kube/k3s.yaml >/dev/null
sudo chown "$USER" ~/.kube/k3s.yaml
chmod 600 ~/.kube/k3s.yaml
export KUBECONFIG=~/.kube/k3s.yaml
echo "--- versions ---"
echo "engine:   $(docker --version 2>/dev/null || podman --version 2>/dev/null)"; echo "helm:     $(helm version --short)"; echo "minikube: $(minikube version --short)"; echo "k3s node: $(kubectl get nodes --no-headers | awk '{print $1, $2}')"; echo "libvirt:  $(virsh --version 2>/dev/null)"; echo "osc:      $(openstack --version 2>&1 | head -1)"; echo "boto3:    $(python3 -c 'import boto3; print(boto3.__version__)')"
```

**Pass =** no `MISSING:` line, the k3s node is `Ready`, and every other line prints a version. Keep
`KUBECONFIG` exported in T1 — every later `kubectl`/`helm` call needs it.

---

## 1. Setup steps

Three pastes: Rook (T3) → everything on the k3s host (T1) → authenticate (T2). The T1 block is long;
`echo` markers show which stage failed.

### 1.1 Rook Ceph + RGW on minikube — **Shell T3**

The repo's fixture brings up a dedicated minikube profile (`rook`, kvm2, 2 raw disks → OSDs), installs
Rook via its official chart, creates the `CephObjectStore` and an S3 user, and exposes RGW on NodePort
**30080**. Nothing from Rook is vendored — only the single-node-tuned CRs. First run takes 10–20 minutes.

```bash
deployments/rook/setup-rook-rgw.sh
echo "--- capture endpoint + keys for T1 ---"
export ROOK_IP=$(minikube -p rook ip)
export RGW_URL="http://${ROOK_IP}:30080"
export RGW_ACCESS=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-o3k -o jsonpath='{.data.AccessKey}' | base64 -d)
export RGW_SECRET=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-o3k -o jsonpath='{.data.SecretKey}' | base64 -d)
echo "RGW_URL=$RGW_URL access=$RGW_ACCESS secret_len=${#RGW_SECRET}"
printf 'export ROOK_IP=%s RGW_URL=%s RGW_ACCESS=%s RGW_SECRET=%s\n' "$ROOK_IP" "$RGW_URL" "$RGW_ACCESS" "$RGW_SECRET" > /tmp/rgw-creds.env
echo "--- pre-create the Glance bucket (o3k never creates buckets) ---"
AWS_ACCESS_KEY_ID=$RGW_ACCESS AWS_SECRET_ACCESS_KEY=$RGW_SECRET python3 -c 'import boto3,os;from botocore.client import Config;c=boto3.client("s3",endpoint_url=os.environ["RGW_URL"],region_name="us-east-1",config=Config(s3={"addressing_style":"path"},signature_version="s3v4"));
b=[x["Name"] for x in c.list_buckets()["Buckets"]];
c.create_bucket(Bucket="images") if "images" not in b else None;print("buckets:",[x["Name"] for x in c.list_buckets()["Buckets"]])'
```

**Pass =** the script ends with `ROUNDTRIP OK`, `ceph status` is `HEALTH_OK`, `secret_len` is non-zero,
and `buckets: ['images']`.

### 1.2 Image, database, o3k and Horizon — **Shell T1**

One paste covering four stages: build and import the image; start Postgres and the node-local
directories; deploy o3k with every service real and Glance on the RGW; deploy Horizon.

Real mode runs `hostNetwork`, so `127.0.0.1` inside the pod is the host — that is how it reaches this
Postgres. `endpointHost` must be the node IP, or the service catalog advertises `localhost` and Horizon
breaks after login.

```bash
export KUBECONFIG=~/.kube/k3s.yaml
source /tmp/rgw-creds.env
export NODE_IP=$(hostname -I | awk '{print $1}')
: "${RGW_URL:?set it}" "${RGW_ACCESS:?set it}" "${RGW_SECRET:?set it}"
echo "NODE_IP=$NODE_IP RGW_URL=$RGW_URL"
echo "--- 1: image ---"
docker build -f build/package/Dockerfile -t docker.io/library/o3k:spec022 .
rm -f /tmp/o3k-spec022.tar
docker save docker.io/library/o3k:spec022 -o /tmp/o3k-spec022.tar
sudo k3s ctr images import /tmp/o3k-spec022.tar
sudo k3s ctr images ls | grep o3k:spec022
echo "--- 2: node-local storage + database ---"
sudo mkdir -p /var/lib/o3k/images /var/lib/o3k/volumes /var/lib/o3k/instances /var/lib/o3k/cloud-init /var/lib/o3k/dhcp /var/lib/o3k/console-tokens
docker inspect o3k-postgres >/dev/null 2>&1 || make db-up
export PGPASSWORD=secret
for i in $(seq 1 30); do psql -h localhost -U o3k -d postgres -tAc 'select 1' >/dev/null 2>&1 && { echo "postgres ready after ${i}s"; break; }; sleep 1; done
psql -h localhost -U o3k -d postgres -tAc "select 1 from pg_database where datname='o3k_test'" | grep -q 1 || psql -h localhost -U o3k -d postgres -c 'CREATE DATABASE o3k_test;'
echo "--- 3: o3k, real mode, Glance on RGW ---"
kubectl get ns o3k >/dev/null 2>&1 || kubectl create ns o3k
kubectl label ns o3k pod-security.kubernetes.io/enforce=privileged --overwrite
helm upgrade --install o3k charts/o3k --namespace o3k --set image.repository=o3k --set image.tag=spec022 --set image.pullPolicy=Never --set deploymentMode=single-node --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true --set nova.libvirtMode=real --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables --set cinder.storageMode=local --set glance.storageMode=s3 --set glance.s3.endpoint="$RGW_URL" --set glance.s3.bucket=images --set glance.s3.region=us-east-1 --set-string 'extraEnv[0].name=AWS_ACCESS_KEY_ID' --set-string "extraEnv[0].value=$RGW_ACCESS" --set-string 'extraEnv[1].name=AWS_SECRET_ACCESS_KEY' --set-string "extraEnv[1].value=$RGW_SECRET" --set endpointHost=$NODE_IP --set secrets.jwtSecret=changeme --set secrets.adminPassword=secret --set database.url='postgres://o3k:secret@127.0.0.1:5432/o3k?sslmode=disable'
kubectl -n o3k rollout status deploy/o3k-o3k --timeout=300s
echo "--- 4: horizon ---"
helm upgrade --install horizon charts/horizon --namespace o3k
kubectl -n o3k rollout status deploy/horizon-o3k-horizon --timeout=300s
echo "--- read-back: every service must be real ---"
kubectl -n o3k get secret o3k-o3k-config -o jsonpath='{.data.o3k\.yaml}' | base64 -d | grep -E '_mode:'
kubectl -n o3k logs deploy/o3k-o3k | grep -E 'Migrations applied|initialized in'
kubectl -n o3k exec deploy/o3k-o3k -- sh -c "wget -qO- $RGW_URL | head -c 60; echo"
echo "Horizon: http://$NODE_IP:30080"
```

**Pass**, reading down the output:

- `docker.io/library/o3k:spec022` in the k3s image store, then `postgres ready after Ns`
- both rollouts complete
- the mode read-back shows **all five real**: `libvirt_mode: "real"`, `networking_mode: "iptables"`,
  `security_group_mode: "iptables"`, `storage_mode: "local"` (Cinder), `storage_mode: "s3"` (Glance)
- `Migrations applied successfully` with `version: 65`, and `Glance initialized in s3 mode`
- the pod gets S3 XML back from the RGW

`realMode.enabled=true` on its own does **not** make the services real — each `*Mode` flag is required
independently, which is why the read-back is the check that matters, not the rollout.

The namespace is created and labelled before Helm rather than via `--create-namespace`: on a re-run that
flag fails with `namespaces "o3k" already exists`, marking the release `failed` even though every
resource applied — and it silently skips the privileged PSA label real mode needs on a cluster that
enforces Pod Security Admission.

Horizon's NodePort is **30080**, the same number the RGW uses, but on a different cluster and node IP.
`$NODE_IP:30080` is Horizon (k3s); `$ROOK_IP:30080` is the RGW (minikube).

### 1.3 Authenticate — **Shell T2**

Also writes the hand-off file the T1 blocks in §2.4 and §2.6 read, so those never run against an empty
variable.

```bash
export NODE_IP=$(hostname -I | awk '{print $1}')
export OS_AUTH_URL=http://$NODE_IP:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
export AT=$(openstack token issue -f value -c id)
export P=00000000-0000-0000-0000-000000000002
export KS=http://$NODE_IP:35357/v3
export NEU=http://$NODE_IP:9696/v2.0
export CIN=http://$NODE_IP:8776/v3
printf 'export NODE_IP=%s AT=%s P=%s KS=%s NEU=%s CIN=%s\n' "$NODE_IP" "$AT" "$P" "$KS" "$NEU" "$CIN" > /tmp/spec022-env.sh
echo "token length: ${#AT}"
openstack catalog list -f value -c Name -c Endpoints | head -3
```

**Pass =** a token length over 300, and the catalog endpoints show `$NODE_IP` — **not** `localhost`. A
`localhost` catalog means `endpointHost` was not set in §1.2.


---

## 2. Test steps

Blocks are **T2** unless labelled otherwise. Each step names the value the defect produced *before* this
feature, so a pass is unambiguous.

### 2.1 B5/P1 — tenant creation with the default-domain alias

The headline defect: `domain_id: "default"` — what every OpenStack client sends, because upstream
Keystone's Default domain literally carries that id — reached a `uuid` column and surfaced as a generic
`500`.

```bash
openstack project create --domain default probe -f value -c id
curl -s -o /dev/null -w 'user create with alias:   %{http_code}\n' -X POST $KS/users -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"user":{"name":"probe-u","password":"x","domain_id":"default"}}'
curl -s -o /dev/null -w 'group create with alias:  %{http_code}\n' -X POST $KS/groups -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"group":{"name":"probe-g","domain_id":"default"}}'
curl -s -o /dev/null -w 'project, omitted domain:  %{http_code}\n' -X POST $KS/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-omitted"}}'
curl -s -X POST $KS/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-bad","domain_id":"11111111-1111-1111-1111-111111111111"}}' | jq -c .
curl -s -o /dev/null -w 'non-identifier domain:    %{http_code}\n' -X POST $KS/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe-bad2","domain_id":"not-a-domain"}}'
curl -s -o /dev/null -w 'duplicate name:           %{http_code}\n' -X POST $KS/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"probe","domain_id":"default"}}'
openstack project show probe -f value -c domain_id
```

**Expect:** a UUID for `probe` (was `500`); `201`, `201`, `201`; a
`{"badRequest":{"code":400,"message":"specified domain not found or disabled"}}` body naming the domain;
`non-identifier domain: 400`; `duplicate name: 409` (was `500`); and the stored UUID
`00000000-0000-0000-0000-000000000100`, **not** the string `default`.

That last line is the quiet one: the alias is accepted on input and never echoed back, which is what
keeps the identity contract suite green (A2/FR-007).

> **Known unrelated gap.** `openstack user create --domain default …` and `openstack domain show default`
> print `'links'` and exit non-zero even though the API returns `201`/`200` and the resource *is*
> created — python-openstackclient's display code expects a `links` field that o3k's identity
> `users`/`domains` responses omit. Pre-existing and unrelated: `openstack domain show` with a real UUID,
> a path this feature never touches, fails identically. That is why this section uses `curl` for those
> and the CLI only for `project create`, whose response does carry `links`. Observation O8.

### 2.2 B5 — domain retrieval by alias, and the name filter

Standard clients resolve a `--domain` argument client-side *before* sending any creation request: they
retrieve the domain by the given reference, then fall back to a name lookup. Both legs were broken —
retrieval `404`d and the list ignored every filter.

```bash
curl -s -o /dev/null -w 'GET /v3/domains/default:  %{http_code}\n' -H "X-Auth-Token: $AT" $KS/domains/default
echo "?name=Default   -> $(curl -s -H "X-Auth-Token: $AT" "$KS/domains?name=Default" | jq '.domains|length')"
echo "?name=default   -> $(curl -s -H "X-Auth-Token: $AT" "$KS/domains?name=default" | jq '.domains|length')"
echo "?name=zzz-nope  -> $(curl -s -H "X-Auth-Token: $AT" "$KS/domains?name=zzz-nope" | jq '.domains|length')"
echo "unfiltered      -> $(curl -s -H "X-Auth-Token: $AT" $KS/domains | jq '.domains|length')"
curl -s -H "X-Auth-Token: $AT" $KS/domains/not-a-uuid | jq -c .
curl -s -o /dev/null -w 'PATCH not-a-uuid:         %{http_code}\n' -X PATCH $KS/domains/not-a-uuid -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"domain":{"name":"x"}}'
```

**Expect:** `200` (was `404`); `1`, `0` (names are case-sensitive), **`0`** (was `1`), `1`; the verbatim
`{"itemNotFound":{"code":404,"message":"Could not find domain: not-a-uuid."}}`; and
`PATCH not-a-uuid: 404`.

`?name=zzz-nope -> 0` is the discriminator. With one domain in the system, `?name=Default -> 1` is
ambiguous — an ignored filter returns `1` too. Asking for a name that cannot exist and getting `0` is
what proves filtering happens. The `PATCH` line shows the sibling routes still get their 404 from the
middleware: the exception is one route, one method, one alias.

### 2.3 Two tenants with genuine member tokens

**Read this before pasting.** `GET /v3/roles?name=…` **ignores the filter** and returns every role, so
`.roles[0].id` is `admin` — and there is no `_member_` role: o3k seeds `admin`, `member`, `reader`. Get
this wrong and both tenants are administrators, who legitimately bypass the check in §2.4, so it would
report a breach that does not exist.

```bash
echo "--- clear any fixture of the same name from a previous run ---"
for n in net-a net-a-shared net-b; do id=$(curl -s -H "X-Auth-Token: $AT" $NEU/networks | jq -r ".networks[]|select(.name==\"$n\")|.id"); test -n "$id" && curl -s -o /dev/null -X DELETE "$NEU/networks/$id" -H "X-Auth-Token: $AT"; done
for n in user-a user-b; do id=$(curl -s -H "X-Auth-Token: $AT" $KS/users | jq -r ".users[]|select(.name==\"$n\")|.id"); test -n "$id" && curl -s -o /dev/null -X DELETE "$KS/users/$id" -H "X-Auth-Token: $AT"; done
for n in tenant-a tenant-b; do id=$(curl -s -H "X-Auth-Token: $AT" $KS/projects | jq -r ".projects[]|select(.name==\"$n\")|.id"); test -n "$id" && curl -s -o /dev/null -X DELETE "$KS/projects/$id" -H "X-Auth-Token: $AT"; done
echo "--- create the fixtures ---"
mkp() { curl -s -X POST "$KS/$1" -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d "$2"; }
export PA=$(mkp projects '{"project":{"name":"tenant-a","domain_id":"default"}}' | jq -r '.project.id')
export PB=$(mkp projects '{"project":{"name":"tenant-b","domain_id":"default"}}' | jq -r '.project.id')
export UA=$(mkp users '{"user":{"name":"user-a","password":"pw-a","domain_id":"default"}}' | jq -r '.user.id')
export UB=$(mkp users '{"user":{"name":"user-b","password":"pw-b","domain_id":"default"}}' | jq -r '.user.id')
export MEMBER=$(curl -s -H "X-Auth-Token: $AT" $KS/roles | jq -r '.roles[] | select(.name=="member") | .id')
echo "PA=$PA PB=$PB UA=$UA UB=$UB member=$MEMBER"
curl -s -o /dev/null -w 'role assign A: %{http_code}\n' -X PUT "$KS/projects/$PA/users/$UA/roles/$MEMBER" -H "X-Auth-Token: $AT"
curl -s -o /dev/null -w 'role assign B: %{http_code}\n' -X PUT "$KS/projects/$PB/users/$UB/roles/$MEMBER" -H "X-Auth-Token: $AT"
tok() { curl -i -s -X POST $KS/auth/tokens -H 'Content-Type: application/json' -d "{\"auth\":{\"identity\":{\"methods\":[\"password\"],\"password\":{\"user\":{\"name\":\"$1\",\"password\":\"$2\",\"domain\":{\"name\":\"Default\"}}}},\"scope\":{\"project\":{\"id\":\"$3\"}}}}" | grep -i '^X-Subject-Token:' | awk '{print $2}' | tr -d '\r'; }
export TA=$(tok user-a pw-a "$PA")
export TB=$(tok user-b pw-b "$PB")
python3 -c 'import base64,json,sys;t=sys.argv[1];
print("B roles: NO TOKEN - user-b could not authenticate; check the ids above") if t.count(".")<2 else print("B roles:",json.loads(base64.urlsafe_b64decode(t.split(".")[1]+"="*(-len(t.split(".")[1])%4)))["roles"])' "$TB"
echo "--- networks: two owned by A (one shared), one by B ---"
export NA=$(curl -s -X POST $NEU/networks -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d '{"network":{"name":"net-a"}}' | jq -r '.network.id')
export NS=$(curl -s -X POST $NEU/networks -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d '{"network":{"name":"net-a-shared","shared":true}}' | jq -r '.network.id')
export NB=$(curl -s -X POST $NEU/networks -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"network":{"name":"net-b"}}' | jq -r '.network.id')
echo "NA=$NA NS=$NS NB=$NB"
echo "--- control: real neutron must actually be doing host-level work ---"
for id in "$NA" "$NS" "$NB"; do printf '  br-%s: %s\n' "${id:0:8}" "$(ip -o link show "br-${id:0:8}" >/dev/null 2>&1 && echo PRESENT || echo absent)"; done
echo "  dhcp configs so far (must be 0, no subnets yet): $(ls -1 /var/lib/o3k/dhcp/configs/ 2>/dev/null | wc -l)"
echo "--- guard: every id must have resolved ---"
BAD=""; for v in PA PB UA UB MEMBER TA TB NA NS NB; do val=$(eval echo \$$v); case "$val" in ""|null) BAD="$BAD $v";; esac; done
if [ -n "$BAD" ]; then echo "SETUP FAILED:$BAD is empty or null. Most likely a fixture of the same name survived a previous run and creation returned 409. Clear it and re-run this block - do NOT continue to 2.4."; else printf 'export NA=%s NS=%s NB=%s PA=%s PB=%s\n' "$NA" "$NS" "$NB" "$PA" "$PB" >> /tmp/spec022-env.sh; echo "PASS: every fixture id resolved, and written to /tmp/spec022-env.sh"; fi
```

**Expect:** five UUIDs, `member=00000000-0000-0000-0000-000000000004`, both assignments `204`,
`B roles: ['member']`, three network UUIDs, all three bridges `PRESENT`, **0** DHCP configs, and
`PASS: every fixture id resolved`.

Two ways this step can lie, both guarded:

- **`B roles: ['admin']`** — stop. An administrator legitimately bypasses the check in §2.4, so it
  would report a breach that does not exist.
- **`SETUP FAILED: … is empty or null`** — a fixture of the same name survived an earlier run, so
  creation returned `409` and `jq` yielded `null`. Everything downstream would then run against
  non-existent tenants and report on nothing. The block clears its own fixtures first for exactly this
  reason; the guard catches the case where a deletion did not take.

The bridge check is the control that keeps §2.4 honest: it proves neutron really is creating host state,
so "no DHCP config for tenant A" afterwards means *refused*, not *nothing was going to happen anyway*.

### 2.4 B8 — the cross-tenant subnet write (real mode) 🔒

Four pastes, alternating T2 and T1, because the host-side checks have to interleave with the API calls.

**Shell T2** — the entitlement matrix:

```bash
sub() { curl -s -o /dev/null -w '%{http_code}' -X POST $NEU/subnets -H "X-Auth-Token: $1" -H 'Content-Type: application/json' -d "{\"subnet\":{\"name\":\"$2\",\"network_id\":\"$3\",\"cidr\":\"$4\",\"ip_version\":4}}"; }
printf '%-38s %s   want 404\n' "foreign PRIVATE network" "$(sub "$TB" intruder-priv "$NA" 10.199.1.0/24)"
printf '%-38s %s   want 403\n' "foreign SHARED network" "$(sub "$TB" intruder-shar "$NS" 10.199.2.0/24)"
printf '%-38s %s   want 404\n' "nonexistent network id" "$(sub "$TB" ghost 11111111-1111-1111-1111-111111111111 10.199.3.0/24)"
printf '%-38s %s   want 400\n' "malformed network id" "$(sub "$TB" bad not-a-uuid 10.199.4.0/24)"
printf '%-38s %s   want 201\n' "tenant B on its OWN network" "$(sub "$TB" ok-own "$NB" 10.199.5.0/24)"
```

**Expect:** exactly `404, 403, 404, 400, 201`. Before this feature: `201, 201, 500, 500, 201`.

`404` for the private network and `403` for the shared one is the deliberate distinction (FR-010a): a
network the caller cannot see must not be confirmed to exist, but a *shared* network is already visible
in their own network list, so `404` there would be a detectable lie. A shared network grants the right to
*attach to* it, never to define its address ranges — which is why this handler is stricter than its
`CreatePort` neighbour. The last row matters as much as the first two: a handler that refused everything
would pass the refusal checks (SC-004).

**Shell T1** — the ordering guarantee, the residue check and the audit trail, in one paste:

```bash
export KUBECONFIG=~/.kube/k3s.yaml
test -r /tmp/spec022-env.sh && source /tmp/spec022-env.sh
CFG=/var/lib/o3k/dhcp/configs
BAD=""; for v in NA NS NB; do val=$(eval echo \$$v); case "$val" in ""|null) BAD="$BAD $v";; esac; done
if [ -n "$BAD" ]; then echo "CANNOT JUDGE:$BAD is empty or null - /tmp/spec022-env.sh is missing or stale. Re-run 2.3 first. Every verdict below would be a FALSE PASS, because 'test -f' on an empty filename always fails."; elif [ ! -r "$CFG" ]; then echo "CANNOT READ $CFG - re-run under sudo. An unreadable directory is NOT an empty one, and treating it as empty would read as a PASS."; else
echo "--- dhcp configs, resolved to network names ---"
for f in $(ls -1 "$CFG" 2>/dev/null); do printf '  %s -> %s\n' "${f%.conf}" "$(docker exec o3k-postgres psql -U o3k -d o3k -tAc "select name from networks where id = '${f%.conf}'" 2>/dev/null | tr -d ' ')"; done
echo "--- networks with a live dnsmasq ---"
DNS=$(pgrep -a dnsmasq 2>/dev/null | grep -oE '[0-9a-f]{8}-[0-9a-f-]{27}' | sort -u)
test -n "$DNS" && echo "$DNS" || echo "  (none - o3k may not be running; the config files above are the durable record)"
echo "--- verdict: DHCP ordering ---"
test -f "$CFG/$NA.conf" && echo "  FAIL: tenant A's PRIVATE network has DHCP state - a REFUSED write started address assignment" || echo "  PASS: no DHCP state for tenant A's private network"
test -f "$CFG/$NS.conf" && echo "  FAIL: tenant A's SHARED network has DHCP state - a REFUSED write started address assignment" || echo "  PASS: no DHCP state for tenant A's shared network"
test -f "$CFG/$NB.conf" && echo "  PASS: tenant B's own network has DHCP state, as its 201 requires" || echo "  NOTE: no DHCP config for tenant B's network - did the 201 really succeed?"
fi
echo "--- verdict: no residue in the data ---"
docker exec o3k-postgres psql -U o3k -d o3k -tAc "select s.name || '|' || s.cidr || '|' || n.name from subnets s join networks n on n.id = s.network_id order by s.created_at" 2>/dev/null
echo "--- verdict: audit records (want exactly 2, one per refusal) ---"
kubectl -n o3k logs deploy/o3k-o3k 2>/dev/null | grep -c 'refused cross-tenant subnet write'
kubectl -n o3k logs deploy/o3k-o3k 2>/dev/null | grep 'refused cross-tenant subnet write' | jq -c '{caller_project, network_id, network_owner, network_shared}'
```

**Expect:** one config resolving to `net-b`; a live dnsmasq for that same network; three `PASS` lines; a
single subnet row `ok-own|10.199.5.0/24|net-b`; and **2** audit records naming caller, network and owner
— one with `network_shared: false` (the `404`), one with `true` (the `403`). Nothing is logged for the
legitimate `201` or for the `404`/`400`, which targeted no existing network.

A live dnsmasq for net-b while tenant A's two networks have none is the whole point: neutron is
demonstrably spawning DHCP for a legitimate subnet and spawned none for either refused one. Stub mode
cannot produce this evidence — `applyDHCPForNetwork` returns immediately there.

No `sudo` on the reads: `/var/lib/o3k/dhcp/configs` is `drwxr-xr-x`, and `sudo … 2>/dev/null` would turn
a sudo-password failure into an empty listing, which the verdict lines would report as a `PASS`. The `-r`
guard covers deployments where the directory really is root-only. The dnsmasq listing captures to a
variable before testing it, because `… | sort -u || echo "(none)"` never fires — `sort` exits 0 on empty
input.

**Back in Shell T2** — the control, and the victim's own views:

```bash
printf '%-38s %s   want 201\n' "tenant A on its own network" "$(sub "$TA" a-own "$NA" 10.199.6.0/24)"
sleep 3
echo "tenant A subnet names: $(curl -s -H "X-Auth-Token: $TA" $NEU/subnets | jq -c '[.subnets[].name]')"
echo "intruder subnets:      $(curl -s -H "X-Auth-Token: $TA" $NEU/subnets | jq '[.subnets[]|select(.name|startswith("intruder"))]|length')"
```

**Expect:** `201`, then `["a-own"]` and `intruder subnets: 0` (SC-003). The same network that refused
tenant B twice accepts its owner — which is what separates "correctly scoped" from "broken".

**Shell T1** — confirm the legitimate subnet did start DHCP:

```bash
test -r /tmp/spec022-env.sh && source /tmp/spec022-env.sh
ls -1 /var/lib/o3k/dhcp/configs/ 2>/dev/null
sudo ip netns list 2>/dev/null | grep -vE '^cni-' | head
case "$NA" in ""|null) echo "CANNOT JUDGE: NA is empty or null - re-run 2.3; a bare 'test -f' here would print a FALSE FAIL";; *) test -f /var/lib/o3k/dhcp/configs/$NA.conf && echo "PASS: tenant A's own subnet started DHCP on its network" || echo "FAIL: tenant A's legitimate subnet did NOT start DHCP - the check is refusing too much";; esac
```

**Expect:** **two** config files now, and `PASS`. This is the other direction — proof the check does not
refuse too much.

### 2.4a FR-013a — the network owner may inspect and remove what an admin put on their network

FR-012 lets an administrator add a subnet to any network, and the row is attributed to the *caller* —
so tenant A ends up with a subnet on `net-a` that belongs to the admin project. Tenant A's listing shows
it. Until this fix, tenant A got `404` fetching it by id and could not delete it: visible, uninspectable,
undeletable. Upstream grants the network's owner both, so this restores parity rather than inventing a
rule. Added after the first host pass — this section has **not** been run on a real stack yet.

**Shell T2** — admin plants the subnet, then tenant A exercises it:

```bash
export SID=$(curl -s -X POST $NEU/subnets -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d "{\"subnet\":{\"name\":\"ok-admin\",\"network_id\":\"$NA\",\"cidr\":\"10.199.7.0/24\",\"ip_version\":4}}" | jq -r '.subnet.id')
echo "admin subnet id: $SID"
echo "owner sees it listed:  $(curl -s -H "X-Auth-Token: $TA" $NEU/subnets | jq '[.subnets[]|select(.name=="ok-admin")]|length')   want 1"
printf '%-34s %s   want 200\n' "owner fetches it by id" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TA" $NEU/subnets/$SID)"
printf '%-34s %s   want 200\n' "owner renames it" "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d '{"subnet":{"name":"renamed-by-owner"}}' $NEU/subnets/$SID)"
echo "rename actually stored:    $(curl -s -H "X-Auth-Token: $TA" $NEU/subnets/$SID | jq -r '.subnet.name')   want renamed-by-owner"
echo "tenant_id on the by-id read: $(curl -s -H "X-Auth-Token: $TA" $NEU/subnets/$SID | jq -r '.subnet.tenant_id')   want the ADMIN project, not $PA"
echo "tenant_id in the listing:    $(curl -s -H "X-Auth-Token: $TA" $NEU/subnets | jq -r '.subnets[]|select(.id=="'$SID'")|.tenant_id')   want the same"
printf '%-34s %s   want 404\n' "tenant B fetches it" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TB" $NEU/subnets/$SID)"
printf '%-34s %s   want 404\n' "tenant B renames it" "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "X-Auth-Token: $TB" -H 'Content-Type: application/json' -d '{"subnet":{"name":"hijacked"}}' $NEU/subnets/$SID)"
printf '%-34s %s   want 404\n' "tenant B deletes it" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "X-Auth-Token: $TB" $NEU/subnets/$SID)"
printf '%-34s %s   want 204\n' "owner deletes it" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "X-Auth-Token: $TA" $NEU/subnets/$SID)"
echo "gone from the listing: $(curl -s -H "X-Auth-Token: $TA" $NEU/subnets | jq '[.subnets[]|select(.name=="renamed-by-owner")]|length')   want 0"
```

**Expect:** `1`, then `200`, `200`, `renamed-by-owner`, the admin project id, then `404, 404, 404, 204`,
then `0`. Before this fix every status was `404` and the subnet survived.

Three lines carry more weight than their status codes. **`rename actually stored`** — a `200` that changes
nothing was the first version of this fix, because the entitlement query and the `UPDATE` disagreed
about scope; the same trap sat in `DELETE`. And **both `tenant_id` lines** — all three handlers
used to report the caller's project, which was right only while each could return nothing but the
caller's own rows. The listing line is here because the first host run passed the by-id read and still
had the listing wrong; the unit tests were green throughout, because none of them read `tenant_id` back
through a `GET`.

Tenant B's three `404`s are the control: widening this to the *network owner* must not widen it to
everyone. And the shared network keeps read-only semantics — upstream puts `rule:shared` in `get_subnet`
and not in `update_subnet`/`delete_subnet`:

```bash
export SSID=$(curl -s -X POST $NEU/subnets -H "X-Auth-Token: $TA" -H 'Content-Type: application/json' -d "{\"subnet\":{\"name\":\"shared-sub\",\"network_id\":\"$NS\",\"cidr\":\"10.199.8.0/24\",\"ip_version\":4}}" | jq -r '.subnet.id')
printf '%-34s %s   want 200\n' "B reads a shared net subnet" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TB" $NEU/subnets/$SSID)"
printf '%-34s %s   want 404\n' "B deletes a shared net subnet" "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "X-Auth-Token: $TB" $NEU/subnets/$SSID)"
curl -s -o /dev/null -w 'owner cleans up: %{http_code}\n' -X DELETE -H "X-Auth-Token: $TA" $NEU/subnets/$SSID
```

**Expect:** `200`, `404`, `204`.

### 2.5 B2 — the project-scoped volume routes (real storage)

Cinder is in `local` mode here, so volumes get real qcow2 backing files under `/var/lib/o3k/volumes`.

**Shell T2**:

```bash
for u in volumes volumes/detail snapshots/detail types/default backups/detail; do printf '%-18s %s\n' "$u" "$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $AT" "$CIN/$P/$u")"; done
curl -s -o /dev/null -w 'create a volume: %{http_code}\n' -X POST "$CIN/$P/volumes" -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"volume":{"name":"probe-vol","size":1}}'
curl -s -H "X-Auth-Token: $AT" $CIN/volumes | jq -S '.volumes' > /tmp/o3k-flat.json
curl -s -H "X-Auth-Token: $AT" "$CIN/$P/volumes" | jq -S '.volumes' > /tmp/o3k-scoped.json
diff -q /tmp/o3k-flat.json /tmp/o3k-scoped.json && echo "IDENTICAL" || echo "DIFFER"
echo "B naming its own project:   $(curl -s -H "X-Auth-Token: $TB" "$CIN/$PB/volumes" | jq '.volumes|length')"
echo "B naming the admin project: $(curl -s -H "X-Auth-Token: $TB" "$CIN/$P/volumes" | jq '.volumes|length')"
```

**Expect:** `volumes 200` (was `405`), `volumes/detail 200` (was a by-id `404`), `snapshots/detail 200`
(was `500`), `types/default 200` (was `500`), and **`backups/detail 404` — unchanged by design**
(FR-016a: it has no by-identifier sibling in that group, so it was never shadowed). Then
`create a volume: 202`, **`IDENTICAL`** (SC-005), and both tenant-B counts equal — naming a foreign
project never widens scope (FR-015).

**Shell T1** — the backing file really exists:

```bash
ls -la /var/lib/o3k/volumes/ | head
```

**Expect:** a `volume-<uuid>.qcow2` file. This is the step that returned `503 failed to create volume
backing` in the stub runbook, where the directory does not exist.

### 2.6 US4 — the multi-tenancy suite

The suite reported `Passed: 0 / Failed: 5` against `PROJECT1=null` before this feature: its setup could
not create a tenant (B5), nothing guarded that, and six sections ran against tenants that did not exist.

**Shell T2** — clear the §2.3 fixtures so the suite's fixed names are free:

```bash
for s in $(curl -s -H "X-Auth-Token: $AT" $NEU/subnets | jq -r '.subnets[].id'); do curl -s -o /dev/null -X DELETE "$NEU/subnets/$s" -H "X-Auth-Token: $AT"; done
for n in "$NA" "$NS" "$NB"; do curl -s -o /dev/null -X DELETE "$NEU/networks/$n" -H "X-Auth-Token: $AT"; done
for u in "$UA" "$UB"; do curl -s -o /dev/null -X DELETE "$KS/users/$u" -H "X-Auth-Token: $AT"; done
for p in "$PA" "$PB"; do curl -s -o /dev/null -X DELETE "$KS/projects/$p" -H "X-Auth-Token: $AT"; done
echo "fixtures removed"
```

**Shell T1** — run it twice, then prove the guards fire and that it recovers. The suite hardcodes
`localhost`, which works because `realMode.hostNetwork=true` makes the pod bind the host's ports.

```bash
source /tmp/spec022-env.sh
echo "===== RUN 1 ====="
make test-multi-tenant 2>&1 | tail -6
echo "===== RUN 2 (no manual cleanup in between) ====="
make test-multi-tenant 2>&1 | tail -6
echo "===== GUARDS FIRE: pre-create test-project-1 so the suite's own create hits the new 409 ====="
curl -s -o /dev/null -w 'pre-create: %{http_code}\n' -X POST $KS/projects -H "X-Auth-Token: $AT" -H 'Content-Type: application/json' -d '{"project":{"name":"test-project-1","domain_id":"default"}}'
make test-multi-tenant > /tmp/o3k-guard.log 2>&1; echo "exit code: $?"
grep -E 'SETUP FAILED|Refusing' /tmp/o3k-guard.log
echo "passes reported: $(grep -c '✓' /tmp/o3k-guard.log)"
echo "===== RECOVERY: clear the deliberate leftover ====="
STALE=$(curl -s -H "X-Auth-Token: $AT" $KS/projects | jq -r '.projects[]|select(.name=="test-project-1")|.id')
curl -s -o /dev/null -X DELETE "$KS/projects/$STALE" -H "X-Auth-Token: $AT"
make test-multi-tenant 2>&1 | tail -6
```

**Expect:** `Passed: 13`, `Failed: 0`, `All multi-tenancy tests passed!` on runs 1 and 2 (SC-006,
SC-006a); then `pre-create: 201`, `exit code: 2`,
`SETUP FAILED: could not create project test-project-1 (got 'null')` with
`Refusing to run isolation checks…`, and **`passes reported: 0`** (FR-017); then `Passed: 13`,
`Failed: 0` again.

This block sources `/tmp/spec022-env.sh` because `$KS` and `$AT` live in T2. Without it the `curl` calls
here would send an empty token, the pre-create would `401`, and the guard test would appear to pass for
entirely the wrong reason.

Two consecutive clean runs is the point: teardown now runs from an `EXIT` trap, so a crashed run cannot
leave a fixed-name project behind to block the next one.

### 2.7 SC-008 — Horizon

**Shell T1**:

```bash
bash test/horizon_compat_test.sh > /tmp/o3k-horizon.log 2>&1; echo "exit code: $?"
grep -E 'Total Passed|Total Failed' /tmp/o3k-horizon.log
grep -E 'Cinder - List volumes|Listed .* volumes' /tmp/o3k-horizon.log
```

**Expect:** `exit code: 0`, `Total Passed: 18`, `Total Failed: 0`, both volume lines `[PASS]`. Before
this feature the suite exited 1 after 5 passes, dead at `[FAIL] Cinder - List volumes` — it calls
`GET /v3/{project}/volumes` (the `405`) and `GET /v3/{project}/volumes/detail` (the shadowed route), and
it could not report more than one failure because `set -e` plus `((FAILED++))` killed it at the first.

Then the browser: open `http://$NODE_IP:30080` and log in as `admin` / `secret`, domain `Default`.

**Expect:**
- login succeeds and the Project dashboard renders (a `localhost` catalog breaks this — see §1.3)
- **Project → Volumes** lists volumes without a `500` — the tab the shadowed `volumes/detail` route broke
- **Project → Network → Networks** renders, and each network's detail page lists its subnets
- **Identity → Projects** lists `probe` from §2.1 — a project created with the `default` alias

### 2.8 SC-008 — the full gate set

**Shell T1**:

```bash
export O3K_TEST_DB_URL='postgres://o3k:secret@localhost:5432/o3k_test?sslmode=disable'
echo "make test failures: $(make test 2>&1 | grep -cE '^FAIL')"
echo "lint: $(make lint 2>&1 | tail -1)"
make test-integration 2>&1 | grep -E 'Passed:|Failed:' | tail -2
go test ./test/contract/keystone/... ./test/contract/neutron/... ./test/contract/cinder/... 2>&1 | tail -4
go test ./test/contract/glance/... 2>&1 | tail -2
echo "--- scope: no migration, no new dependency ---"
git status --short migrations/ go.mod go.sum
echo "(nothing between the two markers means clean)"
```

**Expect:** `0` failing packages, `0 issues.`, `Passed: 28` / `Failed: 0`, three `ok` lines, and — unlike
the stub runbook — `ok` for Glance too, because it has a real RGW backend here rather than stub mode
returning zero-filled bytes. And no output from `git status`.
## 3. Teardown steps

**Shell T2** — the client-side fixtures:

```bash
for n in probe probe-omitted probe-bad probe-bad2 tenant-a tenant-b; do id=$(curl -s -H "X-Auth-Token: $AT" $KS/projects | jq -r ".projects[]|select(.name==\"$n\")|.id"); test -n "$id" && curl -s -o /dev/null -X DELETE "$KS/projects/$id" -H "X-Auth-Token: $AT"; done
for n in probe-u user-a user-b; do id=$(curl -s -H "X-Auth-Token: $AT" $KS/users | jq -r ".users[]|select(.name==\"$n\")|.id"); test -n "$id" && curl -s -o /dev/null -X DELETE "$KS/users/$id" -H "X-Auth-Token: $AT"; done
for n in probe-g; do id=$(curl -s -H "X-Auth-Token: $AT" $KS/groups | jq -r ".groups[]|select(.name==\"$n\")|.id"); test -n "$id" && curl -s -o /dev/null -X DELETE "$KS/groups/$id" -H "X-Auth-Token: $AT"; done
rm -f /tmp/spec022-env.sh /tmp/o3k-flat.json /tmp/o3k-scoped.json
echo "client fixtures removed"
```

**Shell T1** — uninstall the charts, then purge the node-local and host state. The chart's pre-delete
hook does the resource purge (SPEC-010), so let it finish before removing directories.

```bash
export KUBECONFIG=~/.kube/k3s.yaml
helm uninstall horizon -n o3k 2>/dev/null || true
helm uninstall o3k -n o3k 2>/dev/null || true
kubectl delete ns o3k --ignore-not-found --timeout=180s
sudo virsh list --all --name 2>/dev/null | grep '^instance-' | while read -r d; do sudo virsh destroy "$d" 2>/dev/null || true; sudo virsh undefine "$d" 2>/dev/null || true; done
sudo pkill -f 'dnsmasq.*o3k' 2>/dev/null || true
sudo ip netns list 2>/dev/null | awk '{print $1}' | grep -E '^(qdhcp|qrouter)' | while read -r n; do sudo ip netns delete "$n" 2>/dev/null || true; done
for b in $(ip -o link show type bridge 2>/dev/null | awk -F': ' '{print $2}' | grep -E '^br-[0-9a-f]{8}$'); do echo "deleting bridge $b"; sudo ip link set "$b" down 2>/dev/null || true; sudo ip link delete "$b" 2>/dev/null || true; done
sudo iptables -S 2>/dev/null | grep -oE 'O3K-[A-Za-z0-9_-]+' | sort -u | while read -r c; do sudo iptables -F "$c" 2>/dev/null || true; sudo iptables -X "$c" 2>/dev/null || true; done
sudo rm -rf /var/lib/o3k
make db-down 2>/dev/null || true
rm -f /tmp/o3k-spec022.tar /tmp/rgw-creds.env /tmp/o3k-guard.log /tmp/o3k-horizon.log
echo "--- residue check ---"
echo "o3k processes:  $(pgrep -cx o3k 2>/dev/null)"
echo "o3k namespaces: $(kubectl get ns --no-headers 2>/dev/null | grep -c '^o3k')"
echo "o3k bridges:    $(ip -o link show type bridge 2>/dev/null | awk -F': ' '{print $2}' | grep -cE '^br-[0-9a-f]{8}$')"
echo "O3K chains:     $(sudo iptables -S 2>/dev/null | grep -c O3K)"
echo "/var/lib/o3k:   $(test -d /var/lib/o3k && echo PRESENT || echo gone)"
```

**Pass =** all four counts `0` and `/var/lib/o3k: gone`.

**Bridges and iptables chains are host state that outlives the pod, the namespace and the data
directory** — deleting the deployment does not remove them, and a stale `br-<id>` will still be sitting
there on the next run. They are matched as `br-` plus exactly **8 hex** characters, which is how o3k
names them (`internal/neutron/reconciler.go:158`). Docker's user-defined bridges use **12** hex and
`virbr0`/`docker0` do not match at all, so nothing outside o3k is touched.

**Shell T3** — tear down the whole minikube profile. The one step that removes a VM, so it is last and
separate:

```bash
deployments/rook/setup-rook-rgw.sh --delete
minikube profile list 2>/dev/null | grep -c rook
```

**Expect:** `0` — the `rook` profile is gone. Other minikube profiles and clusters are untouched; every
command here was pinned to `--context rook` or `--profile rook`.

**Shell T1, optional** — only if this host did not have k3s before:

```bash
/usr/local/bin/k3s-uninstall.sh
```

---

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Catalog endpoints say `localhost` | `endpointHost` was not set in §1.2. Horizon breaks after login and host-side clients cannot follow the catalog. Re-run the `helm upgrade` with `--set endpointHost=$NODE_IP`. |
| Helm release shows `failed` but pods are `Running` | `--create-namespace` against an existing namespace. §1.2 creates and labels the namespace first, which avoids it; the resources apply either way. |
| `Glance initialized in s3 mode` but uploads fail | `RGW_ACCESS`/`RGW_SECRET` were empty at install time — the `: "${VAR:?}"` guard in §1.2 exists for this. Also check the pod can reach the RGW (the `wget` line). |
| Every service says `stub` in the mode read-back | `realMode.enabled=true` alone does not make services real. Each `nova.libvirtMode`, `neutron.networkingMode`, `cinder.storageMode`, `glance.storageMode` must be set explicitly. |
| §2.4 returns `201` where `404`/`403` is expected | The tenant token carries `admin`. Re-read §2.3 — `?name=` is ignored on `/v3/roles` and `_member_` does not exist. The `python3` decode must print `['member']`. |
| §2.4's T1 verdict prints all `PASS` with no configs listed | `/tmp/spec022-env.sh` was not sourced, or the config directory is unreadable. The `-r` guard distinguishes the two; an empty listing is not the same as an empty directory. |
| §2.5 gives `503 failed to create volume backing` | `/var/lib/o3k/volumes` is missing or not writable by the pod. Re-run §1.2's stage 2. |
| §2.6's guard test shows `pre-create: 401` | `$KS`/`$AT` are empty because `/tmp/spec022-env.sh` was not sourced in T1. |
| `make test-multi-tenant` cannot reach `localhost:35357` | The suite hardcodes `localhost`. It needs `realMode.hostNetwork=true` so the pod binds host ports — check §1.2. |
| Rook stuck, `ceph status` not `HEALTH_OK` | Single-node Ceph needs the tuned CRs the fixture applies and ≥2 raw disks (`MK_EXTRA_DISKS`). Check `kubectl --context rook -n rook-ceph get pods` for OSDs; re-run the fixture. |
