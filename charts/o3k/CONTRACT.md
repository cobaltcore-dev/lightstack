# O3K Integration Contract

The interface an orchestrator (e.g. **sci-in-a-box**) uses to drive an o3k instance's lifecycle
**through Helm — not a CRD**. It has two halves:

- **Input contract** — the values you supply (`values.schema.json`), validated structurally before
  anything is deployed.
- **Output/status contract** — how you derive lifecycle state (Provisioning / Ready / Failed) and
  resolve service endpoints from the running release.

This document ships with the chart and is versioned with it (see [Versioning](#versioning--compatibility)).

---

## 1. Inputs (the values schema)

Every input is declared, typed, and described in [`values.schema.json`](./values.schema.json). Helm
validates values against it on `install`, `upgrade`, `lint`, and `template`, and **rejects
structurally invalid input before any workload is created**. See [`README.md`](./README.md) for the
full value reference.

Guarantees:

- **Documented + typed.** Every input has a description and a structural rule (`type` / `enum` /
  `minimum` / `minLength`).
- **Required.** `image.repository` is required (no implicit image). At render time the chart also
  fails fast if `database.url`, `secrets.jwtSecret`, or `secrets.adminPassword` is missing — unless
  the corresponding `existingSecret` is supplied — and if `teardown.kubectlImage` is unset under
  `dataRetentionPolicy: delete`.
- **Unknown keys are rejected.** `additionalProperties: false` applies to the top level and typed
  objects, so a misspelled key (e.g. `databse:`) fails validation. Kubernetes-native passthrough
  maps stay open: `service.annotations`, `ingress.annotations`, `resources`, `nodeSelector`,
  `tolerations`, `affinity`.
- **`version` = `image.tag`.** The product version you deploy is `image.tag` (defaults to the
  chart's `appVersion`). It is an open-ended registry string (no enum). `multiNode.minProductVersion`
  is a *separate, advisory-only* compatibility floor (not enforced; tied to deferred multi-node, #52).
- **Credentials are validated for presence only.** The schema does not enforce length/complexity;
  use strong secrets (guidance, not enforcement). Values provided via `existingSecret` are never
  seen or validated by the chart.
- **Cross-field combinations.** Release-breaking/unsafe combinations are rejected (e.g. multi-node +
  real mode with `local` glance/cinder backends). Benign capacity/topology preferences are advisory
  — real mode pins to a single replica; multi-node + cache without a shared Redis warns in the notes.

Invalid input fails like this:

```console
$ helm template o3k charts/o3k --set databse.url=x        # misspelled key
Error: values don't meet the specifications of the schema(s) ... Additional property databse is not allowed
```

## 2. Lifecycle status

Derive exactly one of **Provisioning / Ready / Failed** from Helm release status + pod readiness.
The orchestrator owns the bounded wait:

```bash
helm upgrade --install o3k charts/o3k -n o3k --create-namespace --wait --timeout 10m -f values.yaml
```

### Signals

```bash
# (1) Helm release status
helm status o3k -n o3k -o json | jq -r '.info.status'        # deployed | failed | pending-* | ...

# (2a) single-node (Deployment) readiness — ready when updated == available == desired
kubectl -n o3k get deploy o3k-o3k \
  -o jsonpath='{.status.updatedReplicas}/{.status.availableReplicas}/{.spec.replicas}'
#   (equivalently: kubectl -n o3k rollout status deploy/o3k-o3k)

# (2b) multi-node (DaemonSet) readiness — ready when numberReady == updated == desired
kubectl -n o3k get ds o3k-o3k \
  -o jsonpath='{.status.numberReady}/{.status.updatedNumberScheduled}/{.status.desiredNumberScheduled}'

# (3) terminal failure (fail fast, before the timeout)
kubectl -n o3k get pods -l app.kubernetes.io/name=o3k \
  -o jsonpath='{range .items[*]}{.status.phase}{" "}{range .status.containerStatuses[*]}{.state.waiting.reason}{" "}{end}{"\n"}{end}'
#   terminal ⇔ phase==Failed OR reason ∈ {CrashLoopBackOff, ImagePullBackOff, ErrImagePull}
```

### Mapping (first match wins)

| Helm status | Workload ready | Terminal pod | Wait bound | → State |
|-------------|----------------|--------------|------------|---------|
| `failed`    | —              | —            | —          | **Failed** |
| any         | no             | yes          | —          | **Failed** (fail fast) |
| any         | no             | no           | elapsed    | **Failed** |
| `deployed`  | yes            | —            | —          | **Ready** |
| `pending-*` / `deployed` | no | no          | not elapsed | **Provisioning** |

A **Ready** release's API is genuinely serving: the pod readiness probe is `GET /v3` on the Keystone
port, so readiness implies the identity API answers. `--wait` blocks until ready or the timeout; on
timeout/failure Helm exits non-zero and `.info.status` becomes `failed`.

## 3. Endpoints

Endpoint resolution is **identity-anchored**: the contract guarantees one entry point and a discovery
method for the rest.

1. **Keystone `/v3`** (the anchor):
   - in-cluster: `http://<release>-o3k.<namespace>.svc:35357/v3`
   - advertised (external): `<scheme>://<endpointHost>:35357/v3`, where `scheme` is `https` when
     `tls.enabled`. **Set `endpointHost`** for any external consumer — unset, the service catalog
     advertises `localhost`, which is unusable off-node.
2. **Authenticate** to Keystone — the token response carries the OpenStack **service catalog**.
3. **Discover** compute, placement, network, volumev3, and image from the catalog (do not hardcode
   the in-cluster ports — the catalog reflects `endpointHost`/scheme and is authoritative).

```bash
export OS_AUTH_URL=<scheme>://<endpointHost>:35357/v3
# OS_USERNAME / OS_PASSWORD / OS_PROJECT_NAME / domains ...
openstack catalog list      # identity, compute, placement, network, volumev3, image
```

- The six `<release>-o3k:<port>` URLs printed in `NOTES.txt` are **non-authoritative** debugging
  convenience, not a guarantee.
- The **metadata** endpoint (`:8775`) is node-local (reached by VMs over link-local), not a Service,
  not in the catalog — excluded.

API versions (OpenStack Flamingo): Identity v3, Compute v2.1, Network v2.0, Block Storage v3,
Image v2, Placement v1.0.

## Versioning & compatibility

This contract rides the **chart version** (`Chart.yaml: version`, SemVer):

- **MAJOR** bump — the contract may change incompatibly (inputs removed/retyped, status/endpoint
  conventions changed).
- **MINOR / PATCH** — backward compatible.

The product version (`appVersion` / `image.tag`) is an independent axis and does not signal contract
changes.

> Scope note: multi-node cross-node behavior (VM placement across nodes, cross-node tenant traffic)
> is deferred (issue #52). The status and endpoint conventions still apply to what is deployed; the
> chart documents the deferred-scope caveat rather than misreporting health.
