# Quickstart: Deploy Lightstack with the Helm chart

Validated path is **single-node, stub mode** (the SPEC-008 MVP). Real-mode and multi-node steps are documented but only become fully functional once the product post-conditions (issue #52) land.

## Prerequisites

- A Kubernetes cluster ≥ 1.25 (or k3s / a local kind cluster) and `helm` ≥ 3.12.
- An external PostgreSQL reachable from the cluster (the chart deploys **no** database).
- A Lightstack container image reference (required — the chart ships no default).

## 1. Install (single-node, stub)

```bash
helm install lightstack ./charts/lightstack \
  --set image.repository=<your-registry>/lightstack \
  --set image.tag=<tag> \
  --set database.url='postgres://lightstack:<pw>@<host>:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret='<random-strong-secret>' \
  --set secrets.adminPassword='<non-default-admin-password>'
# Omit any required value → install fails fast with a clear message (no insecure default).
```

## 2. Wait & verify health

```bash
kubectl rollout status deploy/lightstack-lightstack        # ready within ~5 min (SC-001)
kubectl get svc lightstack-lightstack                       # seven ports; ClusterIP by default
```

## 3. Authenticate & run a basic lifecycle (stub)

```bash
kubectl port-forward svc/lightstack-lightstack 35357:35357 &      # or use endpointHost when exposed
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin OS_PASSWORD='<the password above>'
export OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default

openstack token issue                                # succeeds with configured pw; 'secret' is rejected
openstack flavor list
openstack server create --flavor m1.small --image cirros demo   # stub
openstack server list && openstack server delete demo
```

## 4. Built-in verification (FR-015)

```bash
helm test lightstack        # runs the identity-check Job; reports success
```

## 5. Optional TLS (per-service-port)

```bash
helm upgrade lightstack ./charts/lightstack --reuse-values \
  --set tls.enabled=true \
  --set ingress.enabled=true \
  --set tls.secretName=<cert-secret> \
  --set endpointHost=lightstack.example.com
# Catalog + self-links then advertise https://lightstack.example.com:<port> (SPEC-007 WS-3).
```

## 6. Real mode (single-node) — host prerequisites apply

```bash
helm upgrade lightstack ./charts/lightstack --reuse-values \
  --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true \
  --set nova.libvirtMode=real --set neutron.networkingMode=iptables \
  --set podSecurity.applyPrivilegedNamespaceLabel=true
```
Requires Linux nodes with a running host `libvirtd` + qemu/KVM and a Lightstack image that includes `dnsmasq` (FR-012f).

> **Note:** booted VMs are fully usable (cloud-init/login, internet egress, floating IPs, networking surviving restarts) only once the post-conditions WS-4/WS-5/WS-6 ship (issue #52). Until then, real-mode resources deploy but VM usability is not guaranteed.

## 7. Multi-node — DEFERRED (issue #52)

```bash
kubectl label node <node> lightstack.io/compute=true
helm upgrade lightstack ./charts/lightstack --reuse-values --set deploymentMode=multi-node
```
The DaemonSet schedules one instance per labeled node and they self-register. **Cross-node VM lifecycle and traffic (SC-003/SC-004) are not functional until WS-1 ships** (issue #52); multi-node is render/lint-validated now, e2e-validated later.

## Uninstall

```bash
helm uninstall lightstack     # removes all release workloads/Services
# Persisted data (external DB, hostPath node-local data, networked backends) is retained
# unless you remove it explicitly per the documented retention choice (FR-014).
# With dataRetentionPolicy=delete, a blocking pre-delete hook purges host state, node-local
# storage, and release PVCs (SPEC-010 / issue #53); the external DB and RBD/S3 stay
# operator-owned (the DB is read-only — never reset). See docs/specs/010-helm-teardown/quickstart.md.
```

## Regression gate

```bash
helm lint ./charts/lightstack
helm template lightstack ./charts/lightstack            # + per-mode permutations render cleanly
# kind/k3s smoke: install single-node stub, then ./test/quick_test.sh against the release
```
