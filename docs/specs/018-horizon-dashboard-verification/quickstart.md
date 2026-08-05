# Quickstart: Verify Horizon dashboard compatibility

**Feature**: SPEC-018 | **Date**: 2026-07-22

Condensed deploy→verify for both passes. The full, copy-paste host runbooks are `HOST_TESTING.md` (stub) and `HOST_TESTING_REAL_MODE.md` (real), created during implementation. Resolved wiring values come from `research.md`.

## Pass A — portable (stub), laptop-grade

```bash
export LIGHTSTACK_JWT_SECRET="dev-secret-change-me"
docker compose -f deployments/docker-compose-horizon.yml up -d --build   # or: scripts/start_validation.sh
# wait for lightstack healthy (curl http://localhost:35357/v3) and Horizon (http://localhost/dashboard/auth/login/)
```

Browser → `http://localhost/dashboard` → sign in `admin` / `secret` → walk the six workflows.
Backend-dependent items (console, volume attach) → record `not-exercised` (stub has no real VM/volume).
Supporting evidence: `test/horizon_dashboard_access_test.sh`, `test/horizon_compat_test.sh`.

## Pass B — graded (real), authoritative

**Prereqs**: a Linux host; **k3s running as root**; **minikube** with **Rook/Ceph**; `helm`, `kubectl`, `openstack`, an SSH keypair.

1. **Rook/Ceph (minikube)** — `CephObjectStore` (RGW) + `CephObjectStoreUser`; expose RGW off-cluster (NodePort / `minikube service`); pre-create bucket `lightstack-images`; capture `AccessKey`/`SecretKey`.
2. **Lightstack (k3s)** — create Secret `rgw-s3-creds` (accessKey/secretKey), then install the chart in real mode:
   ```yaml
   # values (excerpt) — see research.md U2/U3
   realMode: { enabled: true, privileged: true, hostNetwork: true }
   service: { type: ClusterIP }
   endpointHost: "<release>-lightstack"          # catalog → in-cluster DNS (Horizon pod resolves it)
   nova:    { libvirtMode: real }
   neutron: { networkingMode: iptables, securityGroupMode: iptables }
   cinder:  { storageMode: local }
   glance:  { storageMode: "s3", s3: { endpoint: "http://<minikube-ip>:<nodeport>", bucket: "lightstack-images", region: "us-east-1" } }
   extraEnv:
     - { name: AWS_ACCESS_KEY_ID,     valueFrom: { secretKeyRef: { name: rgw-s3-creds, key: accessKey } } }
     - { name: AWS_SECRET_ACCESS_KEY, valueFrom: { secretKeyRef: { name: rgw-s3-creds, key: secretKey } } }
   ```
   Seed an image (into the RGW-backed Glance), a flavor, and an SSH keypair.
3. **Horizon (k3s, new verification chart)** — `helm install` the verification Horizon chart (kolla image + `horizon-config` ConfigMap + memcached + novnc), with `local_settings` repointed: `OPENSTACK_KEYSTONE_URL=http://<release>-lightstack:35357/v3`. Dashboard via **NodePort**.
4. **Verify** — browser → Horizon NodePort → sign in → six workflows. For launch-instance, **SSH into the VM** with the selected key. Console tab → expected `not-working` (F1); check serial `console log show` instead.

## Record

Fill `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` from `contracts/report-template.md`; verdicts per `contracts/workflow-matrix.md`; reconcile/supersede `docs/HORIZON_FULL_COMPATIBILITY_REPORT.md`.

## Gotchas (from research)

- **Lightstack stays ClusterIP** — do **not** NodePort it (catalog advertises fixed ports 35357…; NodePort range 30000–32767 won't match). CORS is not involved (server-side proxy).
- **RGW over HTTP** — avoids the chart's missing CA-mount hook; path-style addressing means an `IP:port` endpoint is fine.
- **Bucket must pre-exist** — Lightstack never creates it.
- **Console URL is hardcoded `localhost:6080`** — port-forward the novnc Service if you want to reproduce the (blank) console; verdict is `not-working` regardless.
