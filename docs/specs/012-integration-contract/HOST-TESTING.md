# Host testing — Lightstack integration contract on k3s: inputs → status → endpoints → published doc

End-to-end validation of the **integration contract (#474)** on real k3s: prove the chart **rejects
invalid configuration before execution** (AC1), that an orchestrator can **derive Provisioning /
Ready / Failed** and **resolve endpoints from the Keystone catalog** (AC2), and that the contract
**ships with the chart** (AC3).

```
clean slate → offline contract gate → install k3s → build+import image (this worktree) →
external Postgres → AC1 reject-before-execution → AC2 Ready (+ forced Failed) → AC2 catalog → AC3 packaged
```

> **Mode: STUB.** The contract is mode-agnostic — the values schema, the Provisioning/Ready/Failed
> derivation, and the Keystone service catalog behave identically in stub and real mode. So this test
> runs in **stub mode**: zero host deps (no libvirt/KVM/nested-virt). Real-mode VM e2e is a *different*
> concern, already covered by `docs/specs/009-single-node-real-mode/HOST-TESTING.md` and
> `docs/specs/011-ensure-storage-subdirs/HOST-TESTING.md`. The contract holds the same either way.

> **Run two shells:** **T1** = host + `kubectl`/`helm`. **T2** = the OpenStack CLI
> (`source ~/.lightstack-ct-env`, used only in §8).
>
> **Build + deploy from THIS worktree** (`/home/fnk/code/sap/ora/lightstack/feature/474`): the chart here
> carries the completed `values.schema.json`, the published `CONTRACT.md`, and the rejection/drift
> gates in `ci/run-tests.sh`.

---

## 0. Prerequisites (one-time)

```bash
podman --version || docker --version ; go version       # container build + Go 1.26+
helm version                                             # helm 3.12+ (v4 fine)
kubectl version --client
jq --version
# OpenStack client in T2:  pipx install python-openstackclient  (or apt install python3-openstackclient)
# psql (optional, to poke Postgres):  apt install postgresql-client
```
> No libvirt, KVM, or nested virtualization required — stub mode runs under the `restricted`
> PodSecurity standard with no host access.

---

## 1. Clean slate

```bash
helm uninstall lightstack -n lightstack --no-hooks 2>/dev/null || true
kubectl wait --for=delete ns/lightstack --timeout=120s 2>/dev/null || true
kubectl delete ns lightstack --ignore-not-found 2>/dev/null || true
podman rm -f lightstack-postgres 2>/dev/null; docker rm -f lightstack-postgres 2>/dev/null; true
```
Verify clean: `kubectl get ns lightstack 2>&1 | grep -q NotFound && echo "clean: ns gone"`.

---

## 2. Offline contract gate (run first — no cluster needed)

The chart's own gate validates the contract without k3s: lint, the schema-rejection fixtures, the
schema↔values drift gate, the unittest suites, and that `CONTRACT.md` packages.

```bash
cd /home/fnk/code/sap/ora/lightstack/feature/474
charts/lightstack/ci/run-tests.sh
```
**Pass** = ends with `chart tests OK` — every `ci/invalid-values/*` is rejected, `valid-complete`
renders, `key coverage OK (… top-level keys)`, all unittest suites pass, `CONTRACT.md packaged OK`.
This already proves AC1/AC3 offline; §6–§9 prove them on a live cluster.

---

## 3. Install k3s

```bash
curl -sfL https://get.k3s.io | sh -
mkdir -p ~/.kube && sudo install -m 600 -o "$USER" /etc/rancher/k3s/k3s.yaml ~/.kube/config
export KUBECONFIG=~/.kube/config
kubectl get nodes                                # Ready
```
> Stub mode needs no host privileges, so root vs rootless k3s does not matter here. The standard
> `get.k3s.io` install is simplest.

---

## 4. Build the image (this worktree) and import it into k3s

k3s has its own containerd; podman/docker images aren't visible until imported. **Tag with the full
`docker.io/library/` name** or `image.repository=lightstack` won't match (see §11).

```bash
cd /home/fnk/code/sap/ora/lightstack/feature/474
podman build -t docker.io/library/lightstack:dev -f build/package/Dockerfile .   # docker: identical flags
podman save  docker.io/library/lightstack:dev -o /tmp/lightstack-dev.tar
sudo k3s ctr images import /tmp/lightstack-dev.tar
sudo k3s ctr images ls | grep -i lightstack             # → docker.io/library/lightstack:dev
```

---

## 5. External Postgres (the chart deploys none — FR-006)

```bash
cd /home/fnk/code/sap/ora/lightstack/feature/474
make db-up                                       # lightstack-postgres on :5432 (lightstack/secret/lightstack)
HOSTIP=$(ip route get 1.1.1.1 | awk '{print $7; exit}'); echo "HOSTIP=$HOSTIP"
DBURL="postgres://lightstack:secret@${HOSTIP}:5432/lightstack?sslmode=disable"
```
> **Stub mode is NOT `hostNetwork`** (unlike the real-mode docs), so the pod's `127.0.0.1` is the pod,
> not the host. Use the **node/host IP** (`$HOSTIP`) in the DB URL so the pod reaches the published
> `:5432`. Lightstack auto-migrates the schema on startup.

---

## 6. AC1 — invalid configuration is rejected BEFORE execution

`helm install`/`upgrade` validates against `values.schema.json` before creating anything. A typo'd
top-level key aborts the install with **nothing deployed**:

```bash
cd /home/fnk/code/sap/ora/lightstack/feature/474
helm install lightstack charts/lightstack -n lightstack --create-namespace \
  --set image.repository=lightstack --set image.tag=dev \
  --set database.url="$DBURL" --set secrets.jwtSecret=x --set secrets.adminPassword=x \
  --set databse.url=oops                              # ← misspelled "database"
# → Error: ... Additional property databse is not allowed
helm list -n lightstack                                      # EMPTY — the release was never created
kubectl get all -n lightstack 2>/dev/null | grep -q . && echo "BUG: resources created" || echo "ok: nothing deployed"
```

Run the rest of the rejection matrix fast with `--dry-run` (schema validation still fires):

```bash
for bad in "service.type=Foo" "glance.storageMode=nfs" "replicaCount=0"; do
  if helm install lightstack charts/lightstack -n lightstack --dry-run \
       --set image.repository=lightstack --set database.url="$DBURL" \
       --set secrets.jwtSecret=x --set secrets.adminPassword=x --set "$bad" >/dev/null 2>&1
  then echo "BUG: accepted $bad"; else echo "rejected: $bad"; fi
done
# missing required image:
helm install lightstack charts/lightstack -n lightstack --dry-run \
  --set database.url="$DBURL" --set secrets.jwtSecret=x --set secrets.adminPassword=x >/dev/null 2>&1 \
  && echo "BUG: accepted missing image" || echo "rejected: missing image"
```
**Pass** = the typo install errors with `helm list` empty, and every matrix line prints `rejected:`.

---

## 7. AC2 — derive Provisioning → Ready (and a forced Failed)

Install a valid stub release with the readiness gate the contract documents
(`CONTRACT.md` §2):

```bash
helm install lightstack charts/lightstack -n lightstack --create-namespace --wait --timeout 5m \
  --set image.repository=lightstack --set image.tag=dev --set image.pullPolicy=Never \
  --set database.url="$DBURL" \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" \
  --set secrets.adminPassword='Host-Admin-1' \
  --set endpointHost="$HOSTIP"
```
Derive the state exactly as an orchestrator would:
```bash
helm status lightstack -n lightstack -o json | jq -r '.info.status'                 # → deployed
kubectl -n lightstack get deploy lightstack-lightstack \
  -o jsonpath='{.status.updatedReplicas}/{.status.availableReplicas}/{.spec.replicas}{"\n"}'   # → 1/1/1
```
**Ready** ⇔ `deployed` **and** `updated == available == desired`. (Readiness is real: the pod's
probe is `GET /v3` on Keystone, so Ready means the API answers.)

Now force **Failed** and confirm the recipe catches it *fast* (terminal pod, before the timeout):
```bash
helm upgrade lightstack charts/lightstack -n lightstack --reuse-values --set image.tag=nope --wait --timeout 90s &
sleep 25
kubectl -n lightstack get pods -l app.kubernetes.io/instance=lightstack \
  -o jsonpath='{range .items[*]}{.status.phase}{" "}{range .status.containerStatuses[*]}{.state.waiting.reason}{"\n"}{end}{end}'
# → ErrImageNeverPull  ⇒ terminal ⇒ FAILED per CONTRACT.md §2 (no need to wait out the 90s)
wait                                                # the helm upgrade exits non-zero
helm status lightstack -n lightstack -o json | jq -r '.info.status'                 # → failed / pending-upgrade
```
Recover:
```bash
helm rollback lightstack -n lightstack
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=120s           # back to Ready
```
**Pass** = Ready derived from `deployed`+`1/1/1`; the bad-tag upgrade surfaces a terminal pod reason
(Failed) and helm reports a non-`deployed` status; rollback returns to Ready.

---

## 8. AC2 — resolve endpoints from the Keystone catalog (identity-anchored)

The contract guarantees **one** endpoint (Keystone `/v3`); the rest come from the service catalog.
Port-forward the front-door Service and authenticate (**T2**):

```bash
kubectl -n lightstack port-forward svc/lightstack-lightstack 35357:35357 >/tmp/pf.log 2>&1 &  PF=$!; sleep 2
cat > ~/.lightstack-ct-env <<'EOF'
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD='Host-Admin-1'                 # ← MUST match §7 secrets.adminPassword
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF
source ~/.lightstack-ct-env
openstack token issue >/dev/null && echo "auth OK (Keystone anchor)"

# the other five services are DISCOVERED from the catalog (not enumerated by the consumer):
openstack catalog list -f value -c Name                       # identity compute placement network volumev3 image
# catalog URLs honor endpointHost (§7 set it to $HOSTIP), NOT the in-cluster ClusterIP:
openstack catalog show compute -f value -c endpoints | grep -o "http://[^/]*"   # → http://<HOSTIP>:8774
# metadata is node-local and intentionally NOT advertised:
openstack catalog list -f value -c Name | grep -qi metadata && echo "BUG: metadata advertised" || echo "ok: metadata excluded"
kill "$PF"
```
**Pass** = `token issue` succeeds against the Keystone anchor; `catalog list` returns the six
services; the compute URL host is `$HOSTIP` (the advertised host, proving the `endpointHost`
coupling, FR-012a); `metadata` is absent.

> Leave `endpointHost` unset on a separate install and the compute URL becomes `http://localhost:8774`
> — usable only in-cluster. That is exactly why the contract requires `endpointHost` for external
> consumers.

---

## 9. AC3 — the contract ships with the chart

```bash
cd /home/fnk/code/sap/ora/lightstack/feature/474
helm package charts/lightstack -d /tmp >/dev/null
tar tzf /tmp/lightstack-*.tgz | grep 'lightstack/CONTRACT.md' && echo "ok: CONTRACT.md packaged"
grep -n 'CONTRACT.md' charts/lightstack/README.md            # README links it
```
**Pass** = `CONTRACT.md` is inside the packaged `.tgz` and the README links it.

---

## 10. Cleanup / reset between runs

```bash
helm uninstall lightstack -n lightstack --no-hooks 2>/dev/null || true   # chart manages the lightstack namespace…
kubectl wait --for=delete ns/lightstack --timeout=120s 2>/dev/null || true   # …so WAIT before any reinstall
podman rm -f lightstack-postgres 2>/dev/null || true
# wipe the cluster entirely:  /usr/local/bin/k3s-uninstall.sh
```
> Stub mode leaves **no** host network/storage state — there is nothing under `/var/lib/lightstack` to clean
> (that is a real-mode concern).

---

## 11. Troubleshooting

- **Pod `ErrImageNeverPull` (`image "lightstack:dev" is not present with pull policy of Never`)** → the image
  isn't in k3s containerd under the expected name. Redo §4 with the `docker.io/library/lightstack:dev` tag
  (a bare `podman build -t lightstack:dev` becomes `localhost/lightstack:dev`). Check:
  `sudo k3s ctr -n k8s.io images ls | grep lightstack`.
- **Pod `CrashLoopBackOff`, log: DB connection refused/timeout** → in **stub mode the pod is not
  `hostNetwork`**, so `127.0.0.1` is the pod. Use `$HOSTIP` in `database.url` (§5). Confirm the
  Postgres container publishes `0.0.0.0:5432` and the host firewall allows the pod CIDR.
- **`INSTALLATION FAILED: … namespace lightstack … is being terminated`** → the chart manages the `lightstack`
  namespace; a back-to-back install races the uninstall. `kubectl wait --for=delete ns/lightstack
  --timeout=120s` first.
- **`401 "Unrecognized schema in response body"` / auth fails** → `OS_PASSWORD` ≠ §7
  `secrets.adminPassword`; re-source `~/.lightstack-ct-env`.
- **`openstack` can't connect on `127.0.0.1:35357`** → the port-forward died; restart
  `kubectl -n lightstack port-forward svc/lightstack-lightstack 35357:35357` and check `/tmp/pf.log`.
- **`catalog show compute` host is `localhost`, not `$HOSTIP`** → `endpointHost` wasn't set on this
  release; reinstall with `--set endpointHost=$HOSTIP` (this is the FR-012a coupling, working as
  documented).
- **A schema rejection in §6 is *accepted* (`BUG:` printed)** → the running chart predates the
  schema completion; build/deploy from this worktree (`feature/474`) and re-run §2's gate.
