# Host testing — Lightstack readiness/liveness on k3s: probes → Pod `Ready` → traffic gating

End-to-end validation of the **readiness/liveness feature (cloud-storage#475 / SPEC-013)** on real
k3s: prove Lightstack exposes an **honest** readiness signal (`/readyz` is `503` until the database is
reachable and all listeners are up), that **Kubernetes does the heartbeating** (readiness gates
Service traffic, liveness restarts a wedged process but a DB outage does **not**), that the
machine-readable signal is the **Pod `Ready` condition**, and that the OpenStack `/healthcheck` alias
behaves per oslo.

```
clean slate → offline gate (go test + helm unittest + ci) → install k3s → build+import image
(this worktree) → external Postgres → US1 honest readiness gates traffic → US2 no crash-loop on a
DB outage → US3 native Pod-Ready consumption + /healthcheck → US4 startup grace + rollout drain →
US5 diagnosis (verbose, CLI, logs)
```

> **Mode: STUB.** The health endpoints gate only on the database + the HTTP listeners (Q1) — they
> behave identically in stub and real mode, and the DB is required in **both** modes. So this runs in
> **stub mode**: zero host deps (no libvirt/KVM/nested-virt). Real-mode VM e2e is a separate concern
> (`docs/specs/009-single-node-real-mode/HOST-TESTING.md`).

> **Run two shells:** **T1** = host + `kubectl`/`helm`. **T2** = a port-forward + `curl` (and
> optionally the OpenStack CLI for §8). Keep a `kubectl get pod -n lightstack -w` running in a third pane —
> you will watch `READY` and `RESTARTS` flip live.

> **Build + deploy from THIS worktree** (`/home/fnk/code/sap/ora/lightstack/feature/475`): only this branch
> carries the `internal/health` package, the `/readyz`·`/livez`·`/healthcheck` wiring, and the chart
> `probes:` block.

Defaults you will observe (chart `values.yaml` `probes:`): readiness `10s × 3 ≈ 30s` to leave the
Service after the DB drops; liveness `20s × 3 ≈ 60s` to restart a wedged process; startup
`5s × 30 ≈ 150s` boot grace before CrashLoopBackOff.

---

## 0. Prerequisites (one-time)

```bash
podman --version || docker --version ; go version       # container build + Go 1.26+
helm version                                             # helm 3.12+ (v4 fine)
helm plugin list | grep -i unittest || helm plugin install https://github.com/helm-unittest/helm-unittest
kubectl version --client
jq --version
# optional (§8 catalog check): pipx install python-openstackclient
```
> No libvirt/KVM/nested-virt — stub mode runs under the `restricted` PodSecurity standard.

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

## 2. Offline gate (run first — no cluster needed)

The unit tests and the chart gate prove most of the contract without k3s.

```bash
cd /home/fnk/code/sap/ora/lightstack/feature/475
go test -race ./internal/health/... ./cmd/lightstack/...        # readyz/livez/healthcheck/drain/CLI + Checker
go vet ./... && gofmt -l internal/health cmd/lightstack         # vet clean; gofmt prints nothing
charts/lightstack/ci/run-tests.sh                               # helm lint + render + schema rejection + unittest
```
**Pass** = `go test` reports `ok` for both packages; `gofmt -l` prints nothing; `run-tests.sh` ends
with `chart tests OK` (the `health probes wiring (SPEC-013)` suite passes and `ci/invalid-values/bad-probes.yaml`
is rejected). This proves the handler semantics + the probe wiring offline; §6–§10 prove them live.

---

## 3. Install k3s

```bash
curl -sfL https://get.k3s.io | sh -
mkdir -p ~/.kube && sudo install -m 600 -o "$USER" /etc/rancher/k3s/k3s.yaml ~/.kube/config
export KUBECONFIG=~/.kube/config
kubectl get nodes                                # Ready
```

---

## 4. Build the image (this worktree) and import it into k3s

```bash
cd /home/fnk/code/sap/ora/lightstack/feature/475
podman build -t docker.io/library/lightstack:dev -f build/package/Dockerfile .   # docker: identical flags
podman save  docker.io/library/lightstack:dev -o /tmp/lightstack-dev.tar
sudo k3s ctr images import /tmp/lightstack-dev.tar
sudo k3s ctr -n k8s.io images ls | grep -i lightstack  # → docker.io/library/lightstack:dev
```

---

## 5. External Postgres (the chart deploys none — FR-006)

```bash
cd /home/fnk/code/sap/ora/lightstack/feature/475
make db-up                                       # lightstack-postgres on :5432 (lightstack/secret/lightstack)
HOSTIP=$(ip route get 1.1.1.1 | awk '{print $7; exit}'); echo "HOSTIP=$HOSTIP"
DBURL="postgres://lightstack:secret@${HOSTIP}:5432/lightstack?sslmode=disable"
```
> **Stub mode is NOT `hostNetwork`**, so the pod's `127.0.0.1` is the pod, not the host. Use the
> **node/host IP** (`$HOSTIP`) so the pod reaches the published `:5432`. We stop/start this container
> below to simulate a DB outage — use `podman stop/start` (not `make db-down`, which deletes it).

---

## 6. US1 — honest readiness gates traffic (SC-001, SC-002, FR-004)

Install a valid stub release; `--wait` blocks until the pod is **Ready**, which (unlike the old `/v3`
probe) now means the DB is genuinely reachable:

```bash
helm install lightstack charts/lightstack -n lightstack --create-namespace --wait --timeout 5m \
  --set image.repository=lightstack --set image.tag=dev --set image.pullPolicy=Never \
  --set database.url="$DBURL" \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" \
  --set secrets.adminPassword='Host-Admin-1' \
  --set endpointHost="$HOSTIP"

kubectl -n lightstack get pod -l app.kubernetes.io/instance=lightstack          # READY 1/1, RESTARTS 0
```

Open a port-forward in **T2** and confirm the endpoints directly:
```bash
kubectl -n lightstack port-forward svc/lightstack-lightstack 35357:35357 >/tmp/pf.log 2>&1 &  PF=$!; sleep 2
curl -s -o /dev/null -w 'readyz=%{http_code}\n' localhost:35357/readyz   # → 200
curl -s -o /dev/null -w 'livez=%{http_code}\n'  localhost:35357/livez    # → 200
```

Confirm the pod is a **ready** endpoint of the Service (receiving traffic). Use the
`discovery.k8s.io/v1` **EndpointSlice** — the legacy `kubectl get endpoints` still works but warns
"v1 Endpoints is deprecated" on k8s ≥1.33, and EndpointSlice shows the readiness flip more precisely
(the endpoint stays listed; its `ready` condition is what kube-proxy routes on):
```bash
kubectl -n lightstack get endpointslices -l kubernetes.io/service-name=lightstack-lightstack \
  -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]} ready={.conditions.ready}{"\n"}{end}'   # → <pod IP> ready=true
```

**Now drop the database** (T1) and watch readiness flip and traffic stop — *without a restart*:
```bash
podman stop lightstack-postgres
sleep 35                                                            # readiness 10s × 3 ≈ 30s
kubectl -n lightstack get pod -l app.kubernetes.io/instance=lightstack            # READY 0/1, RESTARTS still 0
curl -s -w ' (%{http_code})\n' localhost:35357/readyz               # → "database unavailable (503)"
curl -s -o /dev/null -w 'livez=%{http_code}\n' localhost:35357/livez # → 200 (liveness independent of DB)
kubectl -n lightstack get endpointslices -l kubernetes.io/service-name=lightstack-lightstack \
  -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]} ready={.conditions.ready}{"\n"}{end}'   # → <pod IP> ready=false (kube-proxy stops routing)
```

**Restore** and confirm recovery:
```bash
podman start lightstack-postgres
sleep 15
kubectl -n lightstack get pod -l app.kubernetes.io/instance=lightstack           # READY 1/1 again, RESTARTS still 0
kubectl -n lightstack get endpointslices -l kubernetes.io/service-name=lightstack-lightstack \
  -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]} ready={.conditions.ready}{"\n"}{end}'   # → <pod IP> ready=true again
```

**Pass** = Ready only after the DB is reachable; a DB outage flips `/readyz` to `503 "database
unavailable"` within ~30s and the EndpointSlice endpoint's `ready` condition goes `false` (kube-proxy
stops routing to it); `/livez` stays `200`; recovery flips `ready` back to `true`. **`RESTARTS` never
increments** (that is US2).

---

## 7. US2 — a DB outage must NOT restart the process (SC-009); a wedge must (SC-004)

The critical guarantee: losing the database flips readiness but **never** restarts (no crash loop),
because liveness is `/livez` (process-up only).

```bash
podman stop lightstack-postgres
# watch for ~2 minutes — well past the liveness window (20s × 3 ≈ 60s):
for i in $(seq 1 8); do
  kubectl -n lightstack get pod -l app.kubernetes.io/instance=lightstack \
    -o jsonpath='{range .items[*]}READY={.status.containerStatuses[0].ready} RESTARTS={.status.containerStatuses[0].restartCount}{"\n"}{end}'
  sleep 15
done
podman start lightstack-postgres                                          # restore
```
**Pass** = throughout the outage `READY=false` but `RESTARTS=0` — the DB outage never triggers a
restart loop.

**Optional — prove the liveness → restart mechanism (SC-004).** You **cannot** authentically freeze
Lightstack with `kill -STOP 1`: Lightstack is **PID 1** of the container's PID namespace, and the kernel **drops
`SIGSTOP`/`SIGKILL` sent to PID 1 from inside its own namespace** (the init-process protection — the
same reason `kill -9 1` can't kill a container from within). The signal is silently ignored and Lightstack
keeps serving, so no restart happens. Instead, point liveness at a failing path and watch the kubelet
restart the container, then revert:

```bash
# Make liveness fail (startup still gates on /readyz, which stays 200 with the DB up):
kubectl -n lightstack patch deploy lightstack-lightstack --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/livenessProbe/httpGet/path","value":"/__force-fail"}]'
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=120s        # new pod starts + becomes Ready (readiness=/readyz)
# its liveness now 404s → fails 3× (≈60s) → the kubelet restarts the container IN PLACE:
kubectl -n lightstack get pod -l app.kubernetes.io/instance=lightstack -w        # RESTARTS climbs 0 → 1 → … ; Ctrl-C

# Revert to the real liveness path:
kubectl -n lightstack patch deploy lightstack-lightstack --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/livenessProbe/httpGet/path","value":"/livez"}]'
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=120s
```
> A genuine internal deadlock would make `/livez` stop responding, which the kubelet treats exactly
> like the 404 above — restart after `failureThreshold`. We inject the failure via the probe path
> because PID 1 can't be frozen from inside the container. (The `livenessProbe → /livez` wiring itself
> is also guarded offline by `probes_test`.)
**Pass** = `RESTARTS` increments only under the forced liveness failure, and returns to a stable
`0`-restart Ready pod after the revert.

---

## 8. US3 — operators consume the native Pod `Ready` condition (SC-007); `/healthcheck` (FR-014)

An orchestrator gates on Kubernetes-native signals — **no custom protocol**:

```bash
# --wait already blocked on Ready in §6. The condition is directly readable:
kubectl -n lightstack get pod -l app.kubernetes.io/instance=lightstack \
  -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}{"\n"}'        # → True
kubectl -n lightstack wait --for=condition=Ready pod -l app.kubernetes.io/instance=lightstack --timeout=60s && echo "wait: Ready"
```

Force a **never-Ready** release and confirm `--wait` / `kubectl wait` fail (Failed):
```bash
helm upgrade lightstack charts/lightstack -n lightstack --reuse-values --set image.tag=nope --wait --timeout 90s &
sleep 25
kubectl -n lightstack get pods -l app.kubernetes.io/instance=lightstack \
  -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.state.waiting.reason}{"\n"}{end}{end}'  # → ErrImageNeverPull
wait                                                                # helm upgrade exits non-zero
helm rollback lightstack -n lightstack && kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=120s    # back to Ready
```

The OpenStack-compatible `/healthcheck` alias (T2 port-forward from §6):
```bash
curl -s            localhost:35357/healthcheck            # → OK            (text/plain default)
curl -s -I         localhost:35357/healthcheck | head -1  # → HTTP/1.1 200  (HEAD, no body)
curl -s -H 'Accept: application/json' localhost:35357/healthcheck   # → {"detailed":false,"reasons":["OK"]}
```

**Pass** = the Pod `Ready` condition is `True` and `kubectl wait` succeeds when serving; a bad-image
upgrade never becomes Ready (`--wait` fails, rollback recovers); `/healthcheck` answers GET (text),
HEAD (status only), and negotiates JSON.

---

## 9. US4 — resilient startup (SC-005) and zero-downtime rollout (SC-006)

**Slow / absent DB at boot is tolerated within the startup grace (~150s), not crash-looped:**
```bash
helm uninstall lightstack -n lightstack --wait 2>/dev/null; kubectl wait --for=delete ns/lightstack --timeout=120s 2>/dev/null || true
podman stop lightstack-postgres                                            # DB down at boot
helm install lightstack charts/lightstack -n lightstack --create-namespace \
  --set image.repository=lightstack --set image.tag=dev --set image.pullPolicy=Never \
  --set database.url="$DBURL" --set secrets.jwtSecret=x --set secrets.adminPassword='Host-Admin-1'
sleep 60
kubectl -n lightstack get pod -l app.kubernetes.io/instance=lightstack           # Running, READY 0/1, RESTARTS 0 (startup grace)
podman start lightstack-postgres                                          # bring the DB up WITHIN the grace
kubectl -n lightstack wait --for=condition=Ready pod -l app.kubernetes.io/instance=lightstack --timeout=120s && echo "came up after slow DB"
```
> Leave the DB down past ~150s instead and the pod enters `CrashLoopBackOff` (startupProbe exhausted)
> — the intended outcome for an unmet boot-time dependency (Q6).

**Zero-downtime rollout** (needs ≥2 replicas + a drain; default `drainSeconds:0`):
```bash
helm upgrade lightstack charts/lightstack -n lightstack --reuse-values --set replicaCount=2 --set drainSeconds=5 --wait --timeout 5m
# hammer the front door during a rolling restart (T2 keeps the port-forward up):
( for i in $(seq 1 3000); do curl -s -o /dev/null -w '%{http_code}\n' localhost:35357/v3; done ) >/tmp/codes.txt &
kubectl -n lightstack rollout restart deploy/lightstack-lightstack
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=180s
wait; echo "response codes:"; sort /tmp/codes.txt | uniq -c
```
**Pass** = a slow-but-eventual DB connect at boot does not restart the pod (becomes Ready once the DB
returns); with `replicaCount=2 drainSeconds=5`, the rolling restart shows **only `200`/`300`** codes
(old pods report not-ready and drain before terminating — no dropped requests).

---

## 10. US5 — diagnosing an unhealthy instance (SC-008, FR-010/FR-011/FR-015)

With the DB down (`podman stop lightstack-postgres`), the failing component is visible and queryable:
```bash
curl -s localhost:35357/readyz                          # → database unavailable
curl -s 'localhost:35357/readyz?verbose'                # → [failed] database
kubectl -n lightstack logs -l app.kubernetes.io/instance=lightstack | grep 'readiness transition' | tail -3   # → ready=false reason=...
```
The `lightstack healthcheck` CLI (from the host against the port-forward, or via `kubectl exec` if the image
has a shell):
```bash
go run ./cmd/lightstack healthcheck --ready --addr http://localhost:35357 ; echo "exit=$?"   # DB down → exit 1
podman start lightstack-postgres; sleep 15
go run ./cmd/lightstack healthcheck --ready --addr http://localhost:35357 ; echo "exit=$?"   # DB up → exit 0
kill "$PF" 2>/dev/null || true
```
**Pass** = `/readyz?verbose` names `database`; a `readiness transition` log line is emitted on each
flip; `lightstack healthcheck --ready` exits `1` when not-ready and `0` when ready.

---

## 11. Cleanup / reset between runs

```bash
helm uninstall lightstack -n lightstack --no-hooks 2>/dev/null || true
kubectl wait --for=delete ns/lightstack --timeout=120s 2>/dev/null || true   # chart owns the ns — WAIT before reinstall
podman rm -f lightstack-postgres 2>/dev/null || true
# wipe the cluster entirely:  /usr/local/bin/k3s-uninstall.sh
```
> Stub mode leaves no host network/storage state — nothing under `/var/lib/lightstack` to clean.

---

## 12. Troubleshooting

- **Pod `ErrImageNeverPull`** → the image isn't in k3s containerd under `docker.io/library/lightstack:dev`.
  Redo §4 (a bare `podman build -t lightstack:dev` becomes `localhost/lightstack:dev`). Check
  `sudo k3s ctr -n k8s.io images ls | grep lightstack`.
- **Pod stuck `0/1`, never Ready, `/readyz` 503 `database unavailable`** → the pod cannot reach
  Postgres. In **stub mode the pod is not `hostNetwork`**, so `127.0.0.1` is the pod — use `$HOSTIP`
  in `database.url` (§5), confirm `lightstack-postgres` publishes `0.0.0.0:5432`, and that the DB container
  is running (`podman ps`). This is readiness working *correctly* — it refuses to go Ready until the
  DB answers.
- **`RESTARTS` climbing during a DB outage** → liveness is mis-pointed at a DB-gated path. It must be
  `/livez` (process-up). Verify: `kubectl -n lightstack get pod <pod> -o jsonpath='{.spec.containers[0].livenessProbe.httpGet.path}'`
  → `/livez`. (The offline `probes_test` guards this.)
- **`curl localhost:35357/readyz` connection refused** → the port-forward died; restart
  `kubectl -n lightstack port-forward svc/lightstack-lightstack 35357:35357` and check `/tmp/pf.log`.
- **EndpointSlice `ready` not flipping when NotReady** → check the slice/label.
  `kubectl -n lightstack get endpointslices -l kubernetes.io/service-name=lightstack-lightstack -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]} ready={.conditions.ready}{"\n"}{end}'`;
  the endpoint stays listed but `conditions.ready` flips `true→false` within ~30s of the DB drop
  (kube-proxy excludes not-ready endpoints). The legacy `kubectl get endpoints lightstack-lightstack` still works
  but warns "v1 Endpoints is deprecated" on k8s ≥1.33.
- **`kill -STOP 1` in §7 did nothing (no restart)** → expected. Lightstack is PID 1, and the kernel drops
  `SIGSTOP`/`SIGKILL` sent to PID 1 from inside its own namespace (init-process protection). Use the
  liveness-path-patch method in §7 to prove the restart mechanism instead.
- **Rollout in §9 shows non-200 codes** → with a single replica there is no overlap; set
  `replicaCount=2` (done above) and `drainSeconds=5`, and ensure `terminationGracePeriodSeconds`
  (default 30) exceeds `drainSeconds`.
- **A schema rejection in §2 is *accepted*** → the running chart predates SPEC-013; build/run from
  this worktree (`feature/475`).
