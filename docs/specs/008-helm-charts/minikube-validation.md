# Minikube Validation Runbook — SPEC-008 Helm Chart

The full, reproducible setup behind the SPEC-008 done-line: **single-node, stub mode (+ optional TLS)**
on a local minikube cluster. This is the heavy-setup companion to [`quickstart.md`](./quickstart.md)
— the quickstart assumes a cluster, an external Postgres, and an image already exist; this document
builds all three from nothing.

> Validated 2026-06-12 on minikube (kvm2 driver). Result log at the end of this file.
> Real-mode and multi-node paths are render/lint-clean but their e2e success criteria are
> **deferred to issue #52**; the uninstall **purge** (`dataRetentionPolicy: delete`) is **deferred to issue #53**.

## What this validates

| Check | Criteria |
|-------|----------|
| Nothing → working single-node cloud, endpoints healthy < 5 min | SC-001 |
| OpenStack CLI works unchanged against the release | SC-002 (stub subset) |
| Configured admin password works; default `secret` is rejected | FR-007a |
| Built-in identity verification (`helm test`) | FR-015 |
| Install fails fast on a missing required value | FR-007/018 |
| `helm uninstall` leaves no release workloads | SC-006 |
| Pod execs `/app/lightstack` (not bare `args`); no empty `env:` block | regression for the two folded `fix(helm)` changes |

## Host prerequisites

| Tool | Version | Notes |
|------|---------|-------|
| minikube | ≥ 1.32 | kvm2 driver used for the validated run; `docker` driver is fine for stub |
| kubectl | ≥ 1.25 | |
| helm | ≥ 3.12 | |
| helm-unittest plugin | latest | `helm plugin install https://github.com/helm-unittest/helm-unittest` |
| docker | any | builds the Lightstack image |
| openstack CLI | any recent | the auth/lifecycle assertions; `pip install python-openstackclient` |
| libvirt + kvm2 | host | only if you also exercise the (deferred) real-mode leg |

```bash
# kvm2 driver one-time check (Linux):
virsh list --all >/dev/null && echo "libvirt OK"
```

---

## Step 1 — Start minikube

```bash
minikube start --profile lightstack --driver=kvm2 --cpus=4 --memory=6g --kubernetes-version=v1.31.0
kubectl config use-context lightstack
kubectl get nodes        # Ready within ~1 min
```

> `docker` driver works identically for stub mode: `minikube start --profile lightstack --driver=docker`.

## Step 2 — Build the Lightstack image and load it into the cluster

The chart ships **no default image** (FR-018), so we build one and side-load it (no registry needed).

```bash
# from the repo root
docker build -f build/package/Dockerfile -t lightstack:dev .
minikube --profile lightstack image load lightstack:dev
minikube --profile lightstack image ls | grep lightstack          # confirm: docker.io/library/lightstack:dev
```

> **If `docker` is actually podman** (the CLI prints *"Emulate Docker CLI using podman"*, or the
> build tags the image `localhost/lightstack:dev`), `minikube image load lightstack:dev` fails with
> `docker.io/library/lightstack:dev: image not known` — podman stored a different name than minikube looks
> for. Give it a docker-normalized name, export, and load the **tarball** instead:
>
> ```bash
> podman tag localhost/lightstack:dev docker.io/library/lightstack:dev
> podman save -o /tmp/lightstack-dev.tar docker.io/library/lightstack:dev
> minikube --profile Lightstack image load /tmp/lightstack-dev.tar
> minikube --profile Lightstack image ls | grep Lightstack          # → docker.io/library/lightstack:dev
> ```
>
> Or build straight inside the node — no host daemon involved, works for docker *or* podman:
> `minikube --profile lightstack image build -t lightstack:dev -f build/package/Dockerfile .`

The image puts the binary at `/app/lightstack` and bundles `iproute2 iptables ipset dnsmasq` (FR-012f).
Because we load a fixed tag locally, set `image.pullPolicy=Never` at install so Kubernetes never
reaches for a registry.

## Step 3 — Provide PostgreSQL (in-cluster, ephemeral)

The chart deploys **no** database (FR-006); it needs a reachable, empty Postgres. The Lightstack binary runs
its own startup migration (golang-migrate, FR-012b), so the DB only has to exist and be empty.

```bash
kubectl create namespace lightstack
kubectl -n lightstack apply -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: { name: postgres }
spec:
  replicas: 1
  selector: { matchLabels: { app: postgres } }
  template:
    metadata: { labels: { app: postgres } }
    spec:
      containers:
        - name: postgres
          image: postgres:18
          env:
            - { name: POSTGRES_DB, value: lightstack }
            - { name: POSTGRES_USER, value: lightstack }
            - { name: POSTGRES_PASSWORD, value: secret }
          ports: [{ containerPort: 5432 }]
          readinessProbe:
            exec: { command: ["pg_isready", "-U", "lightstack"] }
            initialDelaySeconds: 5
---
apiVersion: v1
kind: Service
metadata: { name: postgres }
spec:
  selector: { app: postgres }
  ports: [{ port: 5432, targetPort: 5432 }]
YAML
kubectl -n lightstack rollout status deploy/postgres --timeout=120s
```

DB URL the chart will use (same namespace):
`postgres://lightstack:secret@postgres.lightstack.svc.cluster.local:5432/lightstack?sslmode=disable`

> **Alternative — host Postgres:** run `make db-up` (Docker `postgres:18` on the host) and point the
> chart at `postgres://lightstack:secret@host.minikube.internal:5432/lightstack?sslmode=disable`. Ephemeral data is
> fine; this is a validation cluster.

## Step 4 — Static gates (no cluster needed, but run them here)

```bash
helm lint ./charts/lightstack -f charts/lightstack/ci/test-values.yaml
charts/lightstack/ci/run-tests.sh        # lint + per-mode template permutations + helm-unittest
```
Expected: lint clean, every permutation renders, `helm unittest` GREEN (6 suites / 24 tests).

## Step 5 — Install the chart (single-node, stub)

```bash
helm install lightstack ./charts/lightstack -n lightstack \
  --set image.repository=lightstack \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --set database.url='postgres://lightstack:secret@postgres.lightstack.svc.cluster.local:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" \
  --set secrets.adminPassword='Smoke-Admin-1'
```

## Step 6 — Health + built-in verification

```bash
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=300s   # SC-001: ready < 5 min
kubectl -n lightstack get svc lightstack-lightstack                                # seven ports, ClusterIP
helm test lightstack -n lightstack                                          # FR-015 identity-check Job → success
```

If the pod crash-loops with **exit 127 / ContainerCannotRun**, the chart is execing the wrong
entrypoint — that is the exact regression the `command:` fix prevents (see Troubleshooting).

## Step 7 — Authenticate and run a stub lifecycle (SC-002 subset)

After `token issue`, the CLI reads the **service catalog** and follows it to each service — compute
`8774`, image `9292`, network `9696`, block storage `8776`, placement `8778`. The catalog advertises
`http://localhost:<port>` (it echoes the host you authenticated against), so **every** API port must
be forwarded — not just identity `35357`, or the first catalog-following call fails with
*connection refused*.

```bash
kubectl -n lightstack port-forward svc/lightstack-lightstack \
  35357:35357 8774:8774 8778:8778 9696:9696 8776:8776 9292:9292 >/dev/null 2>&1 &
PF=$!; sleep 4
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default

OS_PASSWORD='Smoke-Admin-1' openstack token issue            # PASS: configured password works
OS_PASSWORD='secret'        openstack token issue            # HTTP 401 expected: default rejected (FR-007a)

OS_PASSWORD='Smoke-Admin-1' openstack flavor list            # m1.* flavors are seeded

# Stub glance seeds no image — create one (stub mode stores no real bits):
head -c 1024 /dev/zero > /tmp/fake.img
OS_PASSWORD='Smoke-Admin-1' openstack image create cirros \
  --disk-format qcow2 --container-format bare --public --file /tmp/fake.img

OS_PASSWORD='Smoke-Admin-1' openstack server create --flavor m1.small --image cirros demo --wait
OS_PASSWORD='Smoke-Admin-1' openstack server list           # demo → ACTIVE
OS_PASSWORD='Smoke-Admin-1' openstack server delete demo
OS_PASSWORD='Smoke-Admin-1' openstack image delete cirros
kill $PF 2>/dev/null || true
```

## Step 8 — Fail-fast gate (negative test, FR-007/018)

```bash
# Omit a required value → install must fail with a clear message, render nothing:
helm install bad ./charts/lightstack -n lightstack \
  --set image.repository=lightstack --set image.tag=dev \
  --set database.url='postgres://x' --set secrets.jwtSecret=x
# Expected: error about the missing admin password (no insecure default). Nothing is created.
```

## Step 9 — Day-2: upgrade + uninstall (US3)

```bash
# value change takes effect, data/workloads survive the upgrade:
helm upgrade lightstack ./charts/lightstack -n lightstack --reuse-values --set logging.level=debug
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=300s

# uninstall removes every release workload (SC-006):
helm uninstall lightstack -n lightstack
kubectl -n lightstack get all        # nothing prefixed lightstack-lightstack remains
```

> The default `dataRetentionPolicy: retain` means uninstall removes only Kubernetes objects;
> the external Postgres is untouched. The active **purge** path (`delete`) is **deferred to issue #53**.

## Step 10 — Teardown

```bash
minikube delete --profile lightstack
```

---

## Fast path — the scripted harness

Steps 5–9 are also wrapped by the committed test scripts (they create their own `lightstack-smoke`
namespace). Run them once Steps 1–3 are up and `LIGHTSTACK_DB_URL` points at a reachable Postgres:

```bash
export LIGHTSTACK_IMAGE_REPO=lightstack LIGHTSTACK_IMAGE_TAG=dev
export LIGHTSTACK_DB_URL='postgres://lightstack:secret@postgres.lightstack.svc.cluster.local:5432/lightstack?sslmode=disable'
./test/helm_singlenode_stub_test.sh        # install → helm test → auth (pw + reject secret) → stub lifecycle → uninstall
./test/helm_upgrade_uninstall_test.sh      # value change + upgrade, then clean uninstall
```
> The scripts set `image.pullPolicy` via the chart default; if you side-loaded a fixed tag, pass
> `LIGHTSTACK_IMAGE_TAG=dev` and ensure the image is loaded (Step 2) so `IfNotPresent` uses the local copy.

## Result log — 2026-06-12 (minikube / kvm2)

| Stage | Result |
|-------|--------|
| `helm lint` + 17/17 template permutation matrix (11 renders, 6 gate rejections) | PASS (T035) |
| `helm unittest` — 6 suites / 24 tests | GREEN (T035) |
| Install → rollout → `helm test` | PASS (T034) |
| Configured admin password accepted; `secret` rejected | PASS (T034) |
| Stub server create/list/delete | PASS (T034) |
| `helm uninstall` — no residual workloads | PASS (T034) |
| Optional TLS leg | available via `minikube addons enable ingress` (not in the smoke run) |

> **2026-06-15 re-validation:** a fresh run exposed that Step 7 / the smoke script forwarded only the
> identity port `35357`, so every catalog-following call (compute `8774`, image `9292`, …) was
> connection-refused; and stub glance seeds no image, so `server create --image cirros` had nothing to
> boot. Both the runbook (Step 7) and `test/helm_singlenode_stub_test.sh` now forward all API ports and
> create an image first — the script then passes with a real compute+image lifecycle (not identity-only).

Two real defects were surfaced and fixed during the original run, now folded into the chart history:
- **command/args:** the image declares a CMD and no ENTRYPOINT, so `args: ["--config", …]` exec'd
  `--config` directly → CrashLoopBackOff (exit 127). The chart now sets the full `command: ["/app/lightstack", …]`.
- **empty env block:** the pod spec rendered an `env:` key with no entries when neither `endpointHost`
  nor TLS was set. The block is now gated on having content.

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| Pod `CrashLoopBackOff`, exit 127, "ContainerCannotRun" | Wrong entrypoint — confirm `command: ["/app/lightstack", "--config", "/app/config/lightstack.yaml"]` renders (`helm template … \| grep -A1 command`). |
| `minikube image load` → `docker.io/library/lightstack:dev: image not known` | `docker` is podman; the image is tagged `localhost/lightstack:dev`. Retag → `podman save` → `minikube image load <tar>` (see Step 2 note). |
| `ImagePullBackOff` for `lightstack:dev` | Image not loaded into this profile, or `pullPolicy` not `Never`. Re-run the Step 2 load and confirm `minikube -p lightstack image ls \| grep lightstack`. |
| Pod stuck `0/1`, logs show DB connection refused | Postgres not ready / wrong URL host. `kubectl -n lightstack get pods -l app=postgres`; verify the FQDN and `sslmode=disable`. |
| `helm test` job fails | Pod not ready yet, or identity port not serving — check `kubectl -n lightstack logs deploy/lightstack-lightstack`. |
| After `token issue`, `flavor`/`server`/`image` calls → `Connection refused` on `:8774`/`:9292`/… | Only `35357` was forwarded. The catalog advertises `localhost:<port>` for each service — forward **all** API ports (Step 7). |
| `server create --image cirros` → image not found | Stub glance seeds no image. Create one first (Step 7: `openstack image create cirros …`). |
| `openstack token issue` hangs | The `port-forward` died — restart it and re-export `OS_AUTH_URL`. |
