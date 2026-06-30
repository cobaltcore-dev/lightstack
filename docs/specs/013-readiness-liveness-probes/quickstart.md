# Quickstart: Verifying readiness/liveness

Walkthrough mapping each guarantee to an observable check. Run after implementation; doubles as the acceptance script.

## Local (binary, stub mode — macOS/Linux dev)

```bash
make build && ./bin/o3k --config config/o3k.yaml &

# livez: up regardless of deps
curl -fsS localhost:35357/livez            # → 200 "ok"

# readyz: 200 once DB reachable + all listeners bound
curl -isS localhost:35357/readyz           # → 200 "ok"

# OpenStack-compatible alias, content negotiation
curl -isS localhost:35357/healthcheck                          # → 200 "OK" (text)
curl -isS -H 'Accept: application/json' localhost:35357/healthcheck   # → 200 {"detailed":false,"reasons":["OK"]}
curl -IsS localhost:35357/healthcheck                          # HEAD → 200, no body

# CLI subcommand
./bin/o3k healthcheck --ready ; echo "exit=$?"   # → exit 0 (ready) / 1 (not)
```

**DB-down (the core fix, US1/SC-001):** point `database.url` at a dead host (or stop PostgreSQL) →
```bash
curl -isS localhost:35357/readyz     # → 503 "database unavailable"  (NOT 200 — this is the bug being fixed)
curl -fsS localhost:35357/livez      # → 200  (liveness independent of DB, US2)
```
**Never-hangs (SC-003):** with the DB host black-holed (packets dropped), `/readyz` still returns within ~1 s (503), not hanging.

## Kubernetes (the real consumption path)

```bash
helm install o3k charts/o3k -f my-values.yaml --wait     # blocks until Pod Ready (US3)
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=o3k --timeout=180s
```

- **Honest readiness (US1, SC-002):** `kubectl delete pod <postgres>` (or NetworkPolicy-block the DB) →
  ```bash
  kubectl get pod -l app.kubernetes.io/name=o3k        # READY 0/1 within ~30 s
  kubectl get endpointslices                           # o3k pod IP removed → no traffic
  ```
  Restore the DB → pod returns to `1/1` and is re-added. The container is **not** restarted (RESTARTS unchanged) — runtime outage ≠ crash loop (US2/SC-009).

- **Slow DB at boot (SC-005):** start o3k before the DB; within the ~150 s startup grace it is not restarted; once the DB accepts connections it goes Ready.
- **DB never up at boot (Q6):** leave the DB down past the startup grace → `CrashLoopBackOff` (expected for an unmet boot dependency).
- **Wedged process (SC-004):** liveness failure → kubelet restarts within ~60 s.
- **Zero-downtime rollout (SC-006):** `kubectl rollout restart deploy/o3k` under `hey`/`wrk` load → no dropped requests (old pod flips not-ready and drains before terminating).
- **Native signal (AC4):** `kubectl get pod -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'` is the consumed truth — no custom o3k status call.

## Chart tests
```bash
charts/o3k/ci/run-tests.sh          # helm-unittest: probe wiring (Deployment + DaemonSet), defaults, overrides, schema rejects bad probe values
```

## Gates (Principle I — no regression)
```bash
make test          # Go unit (incl. internal/health, -race)
make lint && make build
./test/readyz_test.sh
# existing OpenStack contract/integration tests still green (new paths don't shadow Keystone routes)
```
