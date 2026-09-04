# Status & Endpoint Contract

The output half: how a consumer derives lifecycle state and resolves endpoints from a release, using
only Helm + Kubernetes signals. A **documented deterministic recipe** — no shipped executable (per
clarification).

## Status derivation

### Signals

```bash
# 1. Helm release status
helm status <release> -n <ns> -o json | jq -r '.info.status'
#    → pending-install | pending-upgrade | deployed | failed | ...

# 2a. single-node (Deployment) readiness
kubectl -n <ns> get deploy <release>-lightstack \
  -o jsonpath='{.status.updatedReplicas}/{.status.availableReplicas}/{.spec.replicas}'
#    ready ⇔ updatedReplicas == availableReplicas == spec.replicas
#    (equivalently: kubectl rollout status deploy/<release>-lightstack)

# 2b. multi-node (DaemonSet) readiness
kubectl -n <ns> get ds <release>-lightstack \
  -o jsonpath='{.status.numberReady}/{.status.updatedNumberScheduled}/{.status.desiredNumberScheduled}'
#    ready ⇔ numberReady == updatedNumberScheduled == desiredNumberScheduled

# 3. terminal failure (fail fast, before the bound)
kubectl -n <ns> get pods -l app.kubernetes.io/name=lightstack \
  -o jsonpath='{range .items[*]}{.status.phase}{" "}{range .status.containerStatuses[*]}{.state.waiting.reason}{" "}{end}{"\n"}{end}'
#    terminal ⇔ any phase==Failed OR reason ∈ {CrashLoopBackOff, ImagePullBackOff, ErrImagePull}
```

### Mapping (first match wins)

| Helm status | Workload ready | Terminal pod | Wait bound | → State |
|---|---|---|---|---|
| `failed` | — | — | — | **Failed** |
| any | no | yes | — | **Failed** (fail fast) |
| any | no | no | elapsed | **Failed** |
| `deployed` | yes | — | — | **Ready** |
| `pending-*` / `deployed` | no | no | not elapsed | **Provisioning** |

### Readiness gate

The orchestrator drives the bound itself:

```bash
helm upgrade --install <release> <chart> -n <ns> --wait --timeout 10m -f values.yaml
```

`--wait` blocks until workload-ready or the timeout. On timeout/failure Helm exits non-zero and
`.info.status` is `failed` → **Failed**. While `--wait` is in flight (or if the consumer polls
without `--wait`), the recipe above classifies **Provisioning** vs an early **Failed** on a terminal
pod signal. A Ready release's API is genuinely serving — the pod readinessProbe is `GET /v3` on the
Keystone port.

## Endpoint resolution (identity-anchored)

The contract guarantees **one** endpoint and a discovery method for the rest.

1. **Keystone `/v3`** — the anchor:
   - in-cluster: `http://<release>-lightstack.<ns>.svc:35357/v3`
   - advertised (external): `<scheme>://<endpointHost>:35357/v3` where `scheme` = `https` iff
     `tls.enabled`. **`endpointHost` MUST be set** for an external consumer (unset ⇒ the catalog
     advertises `localhost`, unusable off-node).
2. **Authenticate** to Keystone → the token response carries the OpenStack **service catalog**.
3. **Discover** compute / placement / network / volumev3 / image from the catalog. Do NOT hardcode
   the in-cluster ports — the catalog reflects `endpointHost`/scheme and is authoritative.

```bash
export OS_AUTH_URL=<scheme>://<endpointHost>:35357/v3
# ... OS_USERNAME / OS_PASSWORD / domains ...
openstack catalog list          # → identity, compute, placement, network, volumev3, image
```

- The six `<release>-lightstack:<port>` URLs in `NOTES.txt` are **non-authoritative** debugging info.
- **metadata `:8775`** is node-local (VM link-local), not a Service and not in the catalog — excluded.

## Versioning

The contract rides `Chart.version` (SemVer): MAJOR may change it incompatibly; MINOR/PATCH are
backward-compatible. (`appVersion` / `image.tag` is the product version, an independent axis.)
