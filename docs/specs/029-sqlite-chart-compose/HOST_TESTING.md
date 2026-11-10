# Host testing guide — SQLite backend, part 2: Helm chart & Docker Compose (stub mode)

Validates cloud-storage#599 (SPEC-029) end-to-end by hand: Lightstack running on **SQLite** — one node-local file, no PostgreSQL — through the **Helm chart** and through **Docker Compose**, plus the chart's render-time guards and safe teardown. This proves the spec's success criteria SC-001 … SC-007. Topology: a single Ubuntu host, **stub mode** (no libvirt/KVM needed — the SQLite storage, guards and teardown behave identically in stub and real mode).

**Run all commands from the repo root.** Paths below are repo-relative (`charts/lightstack`, `deployments/…`); host paths (`/var/lib/lightstack`, `/tmp/…`, `~/.kube`) are not.

**Validates:**

| Acceptance criterion (spec) | Section |
|---|---|
| Chart renders SQLite storage: hostPath db dir, no PVC, `Recreate`, `DB_BACKEND=sqlite` | 2.1 |
| Chart refuses SQLite: path outside root / no node pin / multi-node / replicas>1 / Secret | 2.1 |
| PostgreSQL release renders unchanged (only `DB_BACKEND=postgres` added) | 2.1 |
| Teardown purge Job pinned like the Deployment | 2.1 |
| SQLite via Docker Compose: migrates, starts, serves; no PostgreSQL | 2.2 |
| SQLite database survives `down`/`up` (Compose) | 2.2 |
| SQLite via the chart on k3s: migrates, starts, serves | 2.3 |
| Database survives pod restart and `helm upgrade` | 2.3 |
| `delete` purges and removes `db/` last; `retain` keeps `db/`, reinstall resumes | 2.3 |

Sections **2.1** (helm only) and **2.2** (Docker Compose) run on any host. Section **2.3** needs single-node **k3s**, which Setup **installs** (§1.2) and Teardown **removes** (§3) — skip §1.2 and §2.3 if you don't want k3s on this host.

---

## 0. Prerequisites

Assumes **Docker or Podman** is already installed and usable (used to build the image and run Compose). This block verifies that and installs the lightweight CLIs the tests invoke (`jq`, `python3-openstackclient`, `helm`). k3s itself is installed later, in §1.2. Re-running this block is a no-op.

```bash
sudo apt-get update -qq && sudo apt-get install -y jq curl python3-openstackclient
command -v helm >/dev/null || curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
echo "container engine:" && { docker --version 2>/dev/null || podman --version 2>/dev/null || echo "MISSING — install docker or podman first"; }
echo "versions:" && helm version --short && jq --version && openstack --version 2>&1 | head -1
```

**Pass =** `helm`, `jq`, `openstack` print versions and a container engine (Docker or Podman) prints its version.

---

## 1. Setup steps

### 1.1 Shared client environment

The OpenStack client credentials are the seeded defaults (`admin` / `secret`, project `default`, domain `Default`). Both the Compose stack (2.2) and the port-forwarded k3s stack (2.3) expose Keystone on `localhost:35357`, so this one environment serves both. `KUBECONFIG` points at the private kubeconfig copy that §1.2 writes (harmless until then). Export this in each client shell you open.

```bash
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
export KUBECONFIG=$HOME/.kube/lightstack-hosttest.yaml
echo "client env set for $OS_AUTH_URL"
```

**Pass =** prints `client env set for http://localhost:35357/v3`.

### 1.2 Install single-node k3s (only if you will run section 2.3)

Installs a single-node **k3s** and gives your user a private (mode 0600) copy of its kubeconfig, then waits for the node to be `Ready`. Idempotent — skips the install if k3s is already present, and in that case records nothing, so §3 leaves a pre-existing cluster untouched (it uninstalls k3s only when this run installed it). It installs a system service via the official installer (needs `sudo`). **Skip this stage if you are not running section 2.3.** (The system kubeconfig stays root-only — k3s's default — so the admin credentials are not world-readable; the private copy below is the one `KUBECONFIG` points at.)

```bash
command -v k3s >/dev/null || { curl -sfL https://get.k3s.io | sudo sh -s - && touch /tmp/ls-k3s-installed-by-hosttest; }  # marker: §3 uninstalls k3s only if this run installed it
for i in $(seq 1 30); do sudo k3s kubectl get node >/dev/null 2>&1 && break; sleep 2; done
sudo k3s kubectl wait --for=condition=Ready node --all --timeout=120s
mkdir -p "$HOME/.kube"
sudo install -m 600 -o "$(id -un)" -g "$(id -gn)" /etc/rancher/k3s/k3s.yaml "$HOME/.kube/lightstack-hosttest.yaml"
export KUBECONFIG=$HOME/.kube/lightstack-hosttest.yaml
kubectl get nodes
```

**Pass =** `kubectl wait` reports the node `condition met` and `kubectl get nodes` shows one `Ready` node. (Re-export the §1.1 env — which sets `KUBECONFIG` — in each section 2.3 shell.)

---

## 2. Test steps

### 2.1 Chart render-time behaviour (helm only, no cluster)

Renders the chart with SQLite and PostgreSQL values and asserts the storage, strategy, backend marker, guards and teardown pinning from the terminal — all in one block (three `### check` groups). No cluster or container engine needed.

```bash
echo "### check 1: SQLite storage, Recreate, backend marker (expect no PersistentVolumeClaim)"
helm template r charts/lightstack --set image.repository=lightstack --set secrets.jwtSecret=j --set secrets.adminPassword=secret --set database.url=sqlite:///var/lib/lightstack/db/lightstack.db --set 'nodeSelector.kubernetes\.io/hostname=node-a' | grep -E 'strategy:|type: Recreate|LIGHTSTACK_DB_BACKEND|value: "?sqlite|name: sqlite-db|path: /var/lib/lightstack/db|PersistentVolumeClaim'
echo "### check 2: unsafe SQLite configs refused (expect 5 reasons, in order)"
for args in "--set database.url=sqlite:///tmp/x.db --set nodeSelector.k=v" "--set database.url=sqlite:///var/lib/lightstack/db/lightstack.db" "--set database.url=sqlite:///var/lib/lightstack/db/lightstack.db --set deploymentMode=multi-node --set nodeSelector.k=v" "--set database.url=sqlite:///var/lib/lightstack/db/lightstack.db --set replicaCount=2 --set nodeSelector.k=v" "--set database.url=sqlite:///var/lib/lightstack/db/lightstack.db --set database.existingSecret=s --set nodeSelector.k=v"; do eval helm template r charts/lightstack --set image.repository=lightstack --set secrets.jwtSecret=j --set secrets.adminPassword=secret $args >/dev/null 2>/tmp/ls-err.txt; grep -oE "must lie under realMode.hostStoragePath|requires a node pin|requires deploymentMode=single-node|requires replicaCount|cannot be combined with database.existingSecret" /tmp/ls-err.txt || echo "NO REFUSAL (unexpected)"; done
echo "### check 3: PostgreSQL unchanged + purge Job pinned like the Deployment"
helm template r charts/lightstack --set image.repository=lightstack --set secrets.jwtSecret=j --set secrets.adminPassword=secret --set database.url=postgres://lightstack@db/lightstack --set dataRetentionPolicy=delete --set teardown.kubectlImage=alpine/k8s:1.31.0 --set 'nodeSelector.kubernetes\.io/hostname=node-b' | grep -E 'kind: Deployment|kind: Job|LIGHTSTACK_DB_BACKEND|value: "?postgres|hostname: node-b|strategy:'
rm -f /tmp/ls-err.txt
```

**Expect:**
- **check 1** — `strategy:` + `type: Recreate`, `LIGHTSTACK_DB_BACKEND` value `sqlite`, a `sqlite-db` volume **and** mount at `path: /var/lib/lightstack/db`, and **no** `PersistentVolumeClaim` line.
- **check 2** — exactly these five reasons, in order: `must lie under realMode.hostStoragePath`, `requires a node pin`, `requires deploymentMode=single-node`, `requires replicaCount`, `cannot be combined with database.existingSecret`. No `NO REFUSAL` line.
- **check 3** — `LIGHTSTACK_DB_BACKEND` value `postgres`; `hostname: node-b` appears under **both** `kind: Deployment` and the purge `kind: Job` (same node pin); no `strategy:` line for the PostgreSQL stub. (A second, un-pinned `kind: Job` is the kubectl quiesce hook — expected.)

### 2.2 Docker Compose on SQLite

Builds and starts the stub stack from the SQLite Compose file — no PostgreSQL container — then authenticates, creates a resource, and proves the database survives a restart and a `down`/`up`. One block (five `### ` stages); it re-exports the §1.1 client env so it runs standalone. The first run builds the image (a few minutes).

```bash
echo "### up: build + start the SQLite stack (first run builds the image, a few minutes)"
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
docker compose -f deployments/docker-compose-sqlite.yml up -d --build && touch /tmp/ls-compose-started-by-hosttest  # marker, only after a successful up: §3 removes this stack and its volumes only if this run started it
echo "waiting for Keystone…" && for i in $(seq 1 90); do curl -sf http://localhost:35357/v3 >/dev/null 2>&1 && break; sleep 2; done
echo "### no PostgreSQL + the database file is on the named volume"
docker compose -f deployments/docker-compose-sqlite.yml ps --services | sort | tr '\n' ' '; echo
docker compose -f deployments/docker-compose-sqlite.yml exec -T lightstack ls -l /var/lib/lightstack/db
echo "### authenticate and create a network"
openstack token issue -f value -c id | head -c 12; echo " …token issued"
openstack network create hosttest-net -f value -c id
openstack network list -f value -c Name | grep hosttest-net
echo "### survives a restart"
docker compose -f deployments/docker-compose-sqlite.yml restart lightstack
for i in $(seq 1 60); do curl -sf http://localhost:35357/v3 >/dev/null 2>&1 && break; sleep 2; done
openstack network list -f value -c Name | grep hosttest-net && echo "SURVIVED restart"
echo "### survives down/up on the named volume"
docker compose -f deployments/docker-compose-sqlite.yml down
docker compose -f deployments/docker-compose-sqlite.yml up -d
for i in $(seq 1 60); do curl -sf http://localhost:35357/v3 >/dev/null 2>&1 && break; sleep 2; done
openstack network list -f value -c Name | grep hosttest-net && echo "SURVIVED down/up"
```

**Expect:**
- **up** — the stack builds and starts; Keystone answers on `localhost:35357` within ~2 min.
- **no PostgreSQL** — the service list is `lightstack redis` (no `postgres`); the `db` directory holds `lightstack.db` (and, in WAL mode, `lightstack.db-wal` / `lightstack.db-shm`).
- **authenticate** — a token id prints; `hosttest-net` is created and listed (stub-mode Neutron tracks it in the SQLite database).
- **survives a restart** — `hosttest-net` is still listed; `SURVIVED restart` prints (state read back from the file, not recreated).
- **survives down/up** — `hosttest-net` is still present; `SURVIVED down/up` prints (`down` keeps the named volume, so the same database returns).

### 2.3 Helm chart on SQLite (single-node k3s)

Uses the k3s installed in §1.2, and first stops the §2.2 Compose stack so its localhost ports do not shadow the port-forward (otherwise the readiness `curl` would pass against Compose and the checks below would exercise the Compose database, not the k3s release). **One block** runs the whole k3s path: build + import the image, install pinned to the node (with `endpointHost: localhost` so the service catalog is reachable through the port-forward), create a resource, prove it survives a pod restart and a `helm upgrade`, then that `delete` purges `db/` and `retain` keeps it across a reinstall. A `pf` helper (re)starts the `kubectl port-forward` after each pod recreation — the forward drops when its pod is deleted. It installs under a dedicated test state root (`/var/lib/lightstack-hosttest`), never the real `/var/lib/lightstack`, so the delete-retention test's purge hook can only remove this run's own database. The node's `kubelet` creates `/var/lib/lightstack-hosttest/db` on the host; inspecting it uses `sudo`. Expect several minutes (image build + repeated rollouts).

```bash
echo "### stop the 2.2 Compose stack so its ports don't shadow the k3s port-forward (localhost:35357 etc.)"
docker compose -f deployments/docker-compose-sqlite.yml down 2>/dev/null || true
echo "### build + import the image into k3s (Docker or Podman; localhost/ ref matches both)"
CE=docker; command -v docker >/dev/null 2>&1 || CE=podman
$CE build -t localhost/lightstack:hosttest -f build/package/Dockerfile .
$CE save localhost/lightstack:hosttest | sudo k3s ctr images import -
sudo k3s ctr images ls -q | grep localhost/lightstack:hosttest
echo "### install pinned to the node (endpointHost=localhost so the catalog is reachable via port-forward)"
export KUBECONFIG=$HOME/.kube/lightstack-hosttest.yaml
export OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
cat > /tmp/ls-sqlite-values.yaml <<YAML
image:
  repository: localhost/lightstack
  tag: hosttest
  pullPolicy: Never
endpointHost: localhost
# A dedicated test state root, NOT the real /var/lib/lightstack, so this run creates and later
# removes only its own node-local state — and the delete-retention test's purge hook can never
# touch a pre-existing deployment's database.
realMode:
  hostStoragePath: /var/lib/lightstack-hosttest
secrets:
  jwtSecret: hosttest-jwt
  adminPassword: secret
database:
  url: sqlite:///var/lib/lightstack-hosttest/db/lightstack.db
nodeSelector:
  kubernetes.io/hostname: $NODE
dataRetentionPolicy: delete
teardown:
  kubectlImage: alpine/k8s:1.31.0
YAML
helm install ls charts/lightstack -f /tmp/ls-sqlite-values.yaml && touch /tmp/ls-helm-installed-by-hosttest  # marker, only after a successful install: §3 uninstalls release ls only if this run installed it
kubectl rollout status deployment/ls-lightstack --timeout=180s
echo "### no PVC, strategy Recreate, db file on the node"
kubectl get pvc -l app.kubernetes.io/instance=ls --no-headers 2>/dev/null | wc -l | xargs echo "PVC count (expect 0):"
kubectl get deploy ls-lightstack -o jsonpath='{.spec.strategy.type}'; echo " <- strategy (expect Recreate)"
sudo ls -l /var/lib/lightstack-hosttest/db
pf() { kill $(cat /tmp/ls-pf.pid 2>/dev/null) 2>/dev/null; sleep 1; kubectl port-forward svc/ls-lightstack 35357 8774 8778 9696 8776 9292 > /tmp/ls-pf.log 2>&1 & echo $! > /tmp/ls-pf.pid; for i in $(seq 1 30); do curl -sf http://localhost:35357/v3 >/dev/null 2>&1 && break; sleep 1; done; }
echo "### create a network through the API"
pf
openstack token issue -f value -c id | head -c 12; echo " …token issued"
openstack network create k3s-net -f value -c id
openstack network list -f value -c Name | grep k3s-net && echo created
echo "### survives a pod restart (Recreate)"
kubectl delete pod -l app.kubernetes.io/instance=ls
kubectl rollout status deployment/ls-lightstack --timeout=180s
pf
openstack network list -f value -c Name | grep k3s-net && echo "SURVIVED pod restart"
echo "### survives a helm upgrade"
helm upgrade ls charts/lightstack -f /tmp/ls-sqlite-values.yaml
kubectl rollout status deployment/ls-lightstack --timeout=180s
pf
openstack network list -f value -c Name | grep k3s-net && echo "SURVIVED helm upgrade"
echo "### delete retention: the purge removes db/ last"
kill $(cat /tmp/ls-pf.pid 2>/dev/null) 2>/dev/null || true
helm uninstall ls
sleep 5
sudo ls -ld /var/lib/lightstack-hosttest/db 2>&1 | grep -q 'No such file' && echo "db/ removed (delete policy)" || { echo "db/ still present:"; sudo ls -l /var/lib/lightstack-hosttest/db; }
echo "### retain keeps db/, reinstall resumes"
helm install ls charts/lightstack -f /tmp/ls-sqlite-values.yaml --set dataRetentionPolicy=retain
kubectl rollout status deployment/ls-lightstack --timeout=180s
pf
openstack network create retain-net -f value -c id
helm uninstall ls
sleep 3
sudo ls -l /var/lib/lightstack-hosttest/db/lightstack.db && echo "db/ retained"
helm install ls charts/lightstack -f /tmp/ls-sqlite-values.yaml --set dataRetentionPolicy=retain
kubectl rollout status deployment/ls-lightstack --timeout=180s
pf
openstack network list -f value -c Name | grep retain-net && echo "RESUMED from retained db"
kill $(cat /tmp/ls-pf.pid 2>/dev/null) 2>/dev/null || true
unset -f pf 2>/dev/null
```

**Expect** (each `### ` stage prints its own result):
- **build + import** — `localhost/lightstack:hosttest` appears in the containerd image list.
- **install** — the release installs and the Deployment becomes available (~3 min).
- **no PVC / Recreate / db file** — `PVC count (expect 0): 0`; strategy `Recreate`; `/var/lib/lightstack-hosttest/db` holds `lightstack.db`.
- **create** — a token id prints; `k3s-net` is created and `created` prints.
- **survives a pod restart** — `k3s-net` still listed; `SURVIVED pod restart`.
- **survives a helm upgrade** — `k3s-net` still listed; `SURVIVED helm upgrade`.
- **delete retention** — `db/ removed (delete policy)` (the pre-delete purge Job ran `lightstack cleanup` on the pinned node).
- **retain** — `db/ retained` after the retain uninstall, then `RESUMED from retained db` after the reinstall.

---

## 3. Teardown steps

Removes everything **this run** created — the k3s it installed in §1.2, the `ls` Helm release and Compose stack it started in §2.3/§2.2, and the dedicated test state root `/var/lib/lightstack-hosttest` — and leaves the host as it was. Each destructive step is gated on a marker the creating section wrote, so a pre-existing k3s, a pre-existing release named `ls`, or an unrelated Compose stack is left untouched; the test state root is this run's own path, never the real `/var/lib/lightstack`, so it is removed outright. Idempotent.

```bash
export KUBECONFIG=$HOME/.kube/lightstack-hosttest.yaml
CE=docker; command -v docker >/dev/null 2>&1 || CE=podman
kill $(cat /tmp/ls-pf.pid) 2>/dev/null || true
# Uninstall the Helm release and remove the Compose stack (with its volumes) only when this run
# created them (§2.3 / §2.2 markers), so a pre-existing release named ls or an unrelated Compose
# stack from the same file is left untouched.
if [ -f /tmp/ls-helm-installed-by-hosttest ]; then helm uninstall ls 2>/dev/null || true; fi
if [ -f /tmp/ls-compose-started-by-hosttest ]; then docker compose -f deployments/docker-compose-sqlite.yml down -v 2>/dev/null || true; fi
# Uninstall k3s only when this run installed it (§1.2 marker), so a pre-existing cluster is left
# untouched. The database and all node-local state live under the dedicated test root, which is
# this run's own, so it is removed outright — the real /var/lib/lightstack is never touched.
if [ -f /tmp/ls-k3s-installed-by-hosttest ] && [ -x /usr/local/bin/k3s-uninstall.sh ]; then sudo /usr/local/bin/k3s-uninstall.sh; fi
sudo rm -rf /var/lib/lightstack-hosttest
rm -f /tmp/ls-sqlite-values.yaml /tmp/ls-pf.pid /tmp/ls-pf.log /tmp/ls-err.txt "$HOME/.kube/lightstack-hosttest.yaml" /tmp/ls-k3s-installed-by-hosttest /tmp/ls-helm-installed-by-hosttest /tmp/ls-compose-started-by-hosttest
$CE rmi localhost/lightstack:hosttest 2>/dev/null || true
echo "teardown done"
```

**Pass =** `teardown done` prints; `command -v k3s` finds nothing (k3s removed), `docker compose -f deployments/docker-compose-sqlite.yml ps` is empty, and `/var/lib/lightstack-hosttest` is gone.

> `k3s-uninstall.sh` is k3s's official uninstaller — it stops and removes the service, its data and its embedded containerd (so the imported image goes with it); it runs **only when this run installed k3s** (the §1.2 marker), so a pre-existing cluster is left alone. The `helm uninstall ls` and `docker compose … down -v` before it run only when this run created the release / started the stack (the §2.3 / §2.2 markers), and are best-effort (harmless if you finished on the `retain` install, or if they are already gone). The `sudo rm -rf /var/lib/lightstack-hosttest` removes this run's dedicated test state root — the database and any local backing it created under a test-specific path, never the real `/var/lib/lightstack`. To also remove the CLIs §0 added: `sudo apt-get remove -y python3-openstackclient jq` and `sudo rm -f /usr/local/bin/helm`.

---

## Troubleshooting

- **`openstack` calls hang on k3s** — the `kubectl port-forward` in T1 dropped (it dies when its target pod is deleted). Re-run T1's block, then retry the client command in T2.
- **Pod `Pending` / rejected** — the namespace enforces a `baseline`/`restricted` Pod Security level, which forbids the hostPath volume SQLite needs. Use a namespace without PSA enforcement (vanilla k3s `default` has none).
- **`ImagePullBackOff` on k3s** — the image was not imported into k3s's containerd, or its ref does not match. Re-run the `$CE save localhost/lightstack:hosttest | sudo k3s ctr images import -` step and confirm `sudo k3s ctr images ls -q | grep localhost/lightstack:hosttest`; the values file pins `image.repository: localhost/lightstack` with `pullPolicy: Never`, which must equal the imported ref exactly.
- **Compose `lightstack` never healthy** — check `docker compose -f deployments/docker-compose-sqlite.yml logs lightstack`; a first run compiles the binary in the image build, which can take several minutes before the service starts.
- **`helm uninstall` → `timed out waiting for the condition`, then reinstall → `cannot re-use a name that is still in use`** — the pre-delete hook Jobs did not complete, so the release never uninstalled. Almost always the **quiesce Job's `teardown.kubectlImage` is not pullable** (e.g. a non-existent tag like `rancher/kubectl:v1.31`). Check `kubectl get pods -l app.kubernetes.io/instance=ls` for `ImagePullBackOff`. Use a valid image — the chart's example `alpine/k8s:1.31.0` — then recover the stuck release and retry: `helm uninstall ls --no-hooks || kubectl delete secret -l owner=helm,name=ls --ignore-not-found`, `kubectl delete job -l app.kubernetes.io/instance=ls --ignore-not-found`, `sudo rm -rf /var/lib/lightstack-hosttest`.
- **`db/ still present` after a delete uninstall** — the purge Job could not reach the node (node pin mismatch) or the pre-delete hook timed out; inspect `kubectl get jobs` and the purge pod's logs before the release finished uninstalling.
