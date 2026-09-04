# Quickstart — driving Lightstack through the integration contract

The end-to-end loop an orchestrator (sci-in-a-box) follows: **validate inputs → install with a bound
→ derive Ready → resolve endpoints**. Mirrors the spec's user stories and acceptance scenarios.

## 1. Inputs are rejected before anything deploys (US1)

```bash
# Unknown/misspelled key → rejected by the schema (additionalProperties)
helm template r charts/lightstack --set databse.url=x          # FAILS: additional property

# Bad enum → rejected
helm template r charts/lightstack --set service.type=Foo       # FAILS: not one of ClusterIP/NodePort/LoadBalancer

# Missing required image → rejected
helm template r charts/lightstack                               # FAILS: image.repository required

# Missing credential (render-time guard) → rejected
helm template r charts/lightstack --set image.repository=ghcr.io/lightstack/lightstack \
  --set database.url=postgres://lightstack@db/lightstack               # FAILS: jwtSecret is required
```

A complete, valid value set passes validation and proceeds.

## 2. Install with a readiness bound (US2)

```bash
helm upgrade --install lightstack charts/lightstack -n lightstack --create-namespace --wait --timeout 10m \
  --set image.repository=<registry>/lightstack --set image.tag=<version> \
  --set database.url='postgres://lightstack:<pw>@<host>:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret='<strong-random>' --set secrets.adminPassword='<admin-pw>' \
  --set endpointHost=<reachable-host>          # REQUIRED for an external consumer (catalog correctness)
```

## 3. Derive lifecycle state (US2)

Apply the `status-contract.md` recipe:

```bash
helm status lightstack -n lightstack -o json | jq -r '.info.status'         # deployed?
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=0      # ready? (single-node)
# Failed-fast check: any pod CrashLoopBackOff / ImagePullBackOff → Failed without waiting out --timeout
```

- `deployed` + workload ready ⇒ **Ready**
- still rolling, no terminal pod, bound not elapsed ⇒ **Provisioning**
- `failed`, or a terminal pod, or bound elapsed ⇒ **Failed**

## 4. Resolve endpoints (US2)

```bash
export OS_AUTH_URL=http://<endpointHost>:35357/v3   # https if tls.enabled
export OS_USERNAME=admin OS_PASSWORD=<admin-pw> \
       OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default
openstack token issue        # authenticates against the anchor (Keystone /v3)
openstack catalog list       # discovers compute/placement/network/volumev3/image
```

The orchestrator only needs the **Keystone endpoint**; everything else comes from the catalog. The
node-local metadata endpoint is intentionally absent.

## 5. The published contract (US3)

```bash
helm package charts/lightstack && tar tzf lightstack-*.tgz | grep CONTRACT.md   # CONTRACT.md ships
grep -n CONTRACT charts/lightstack/README.md                            # README links it
```

## Validate the contract (maintainer)

```bash
charts/lightstack/ci/run-tests.sh        # helm lint + template permutations + helm unittest
                                  #  + schema-rejection fixtures + schema↔values coverage gate
```
