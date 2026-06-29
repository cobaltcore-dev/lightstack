# Phase 1 Data Model: Integration Contract

The "entities" here are the conceptual model of the contract: the **input value set** (what the
schema validates), the **lifecycle state** (what the status recipe derives), and the **endpoint
set** (what the orchestrator resolves).

## 1. Input value set — schema inventory

Every top-level group the chart reads. **Validation** = the structural rule the schema enforces;
**Required** = whether absence is rejected (R = required, C = conditional/render-time guard, — = optional);
**AP** = `additionalProperties: false` applies (✓) or the object stays open (open).

| Group / key | Type | Validation | Req | Notes |
|---|---|---|---|---|
| `image.repository` | string | `minLength: 1` | R | no implicit image (FR-018) |
| `image.tag` | string | type + description (no enum) | — | **the "version" input**; defaults to `appVersion` |
| `image.pullPolicy` | string | enum `Always/IfNotPresent/Never` | — | |
| `image.pullSecrets` | array | type | — | |
| `deploymentMode` | string | enum `single-node/multi-node` | — | topology profile |
| `replicaCount` | integer | `minimum: 1` | — | real mode auto-pins to 1 (advisory) |
| `database.url` | string | type + description | C | required unless `existingSecret` (render-time) |
| `database.existingSecret` | string | type | — | Secret holding the DB URL |
| `database.existingSecretKey` | string | type | — | default `url` |
| `secrets.jwtSecret` | string | type (presence-only) | C | required unless `existingSecret`; **no strength check** |
| `secrets.adminUser` | string | type | — | default `admin` |
| `secrets.adminPassword` | string | type (presence-only) | C | required unless `existingSecret`; **no strength check** |
| `secrets.existingSecret` | string | type | — | pre-rendered `o3k.yaml` Secret (GitOps) |
| `endpointHost` | string | type | — | advertised catalog host; **required for external consumers** (R6/FR-012a) |
| `service.type` | string | enum `ClusterIP/NodePort/LoadBalancer` | — | AP ✓ |
| `service.annotations` | object | open map | — | open |
| `ingress.*` | object | typed where fixed; `annotations` open | — | enabled/className/hosts typed |
| `tls.enabled` | boolean | type | — | |
| `tls.secretName` | string | type | — | |
| `tls.certManagerIssuer` | string | type | — | |
| `tls.certManagerKind` | string | enum `ClusterIssuer/Issuer` | — | |
| `nova.libvirtMode` | string | enum `stub/real` | — | |
| `neutron.networkingMode` | string | enum `stub/iptables/ebpf` | — | |
| `neutron.securityGroupMode` | string | enum `stub/iptables/ebpf` | — | |
| `cinder.storageMode` | string | enum `stub/local/rbd/local,rbd` | — | |
| `glance.storageMode` | string | enum `stub/local/rbd/s3/local,rbd/local,s3/rbd,s3` | — | |
| `realMode.enabled` | boolean | type | — | gates privileged/host mounts |
| `realMode.privileged` / `hostNetwork` | boolean | type | — | |
| `realMode.libvirtSocketPath` / `hostStoragePath` | string | type | — | |
| `realMode.floatingIpPool` | string | type | — | |
| `podSecurity.applyPrivilegedNamespaceLabel` | boolean | type | — | |
| `multiNode.nodeSelectorLabel` | string | type | — | |
| `multiNode.vxlanEnabled` | boolean | type | — | |
| `multiNode.vxlanPort` | integer | `minimum/maximum` (port range) | — | |
| `multiNode.minProductVersion` | string | type + description | — | **advisory-only** floor; NOT "the version" (#52) |
| `cache.enabled` | boolean | type | — | |
| `cache.redisUrl` | string | type | — | single shared Redis in multi-node (advisory, NOTES warns) |
| `dataRetentionPolicy` | string | enum `retain/delete` | — | |
| `teardown.kubectlImage` | string | type | C | required under `delete` (render-time); AP ✓ already |
| `teardown.timeoutSeconds` | integer | `minimum: 1` | — | |
| `teardown.deletePVCs` | boolean | type | — | |
| `logging.level` | string | enum `debug/info/warn/error` | — | |
| `logging.format` | string | enum `json/text` | — | |
| `resources` / `nodeSelector` / `tolerations` / `affinity` | object/array | open | — | k8s passthrough — open |

**Coverage invariant (drift gate, R5)**: the set of top-level keys in `values.yaml` ≡ the set of
properties in `values.schema.json`. A key in one but not the other fails the gate.

## 2. Lifecycle state

**States**: `Provisioning`, `Ready`, `Failed`. Derived (not persisted) from `helm status` + pod
readiness at observation time; the orchestrator owns the bounded wait (`--wait --timeout`).

**Derivation** (first matching row wins):

| Helm `.info.status` | Workload readiness | Terminal pod signal | Bound | → State |
|---|---|---|---|---|
| `failed` | any | any | any | **Failed** |
| any | not ready | present (CrashLoopBackOff / ImagePullBackOff / ErrImagePull / pod Failed) | any | **Failed** |
| any | not ready | none | elapsed | **Failed** |
| `deployed` | ready | — | — | **Ready** |
| `pending-install` / `pending-upgrade` / `deployed` | not ready | none | not elapsed | **Provisioning** |

**Workload-ready, per topology** (R4):
- single-node Deployment: `updatedReplicas == spec.replicas` ∧ `availableReplicas == spec.replicas`.
- multi-node DaemonSet: `numberReady == desiredNumberScheduled` ∧ `updatedNumberScheduled == desiredNumberScheduled`.

**Transitions**: `Provisioning → Ready` (workload becomes ready), `Provisioning → Failed` (terminal
signal or bound elapsed). An `upgrade` re-enters `Provisioning` until the new pods are ready. `Ready`
is asserted only while `deployed` ∧ workload-ready holds.

**Note**: "ready" is meaningful because the readinessProbe hits Keystone `/v3` — a Ready release's
API is actually serving. Multi-node cross-node behavior is out of scope (#52); the state still
derives correctly for what is scheduled, and the contract documents the caveat.

## 3. Endpoint set

| Element | Value |
|---|---|
| **Anchor (guaranteed)** | Keystone `/v3`: in-cluster `http://<release>-o3k:35357/v3`, or advertised `<scheme>://<endpointHost>:35357/v3` when set |
| **Discovered** | compute / placement / network / volumev3 / image — from the service catalog in the Keystone token (honors `endpointHost` + scheme) |
| **Non-authoritative** | the six in-cluster `<release>-o3k:<port>` addresses listed in `NOTES.txt` (debugging convenience) |
| **Excluded** | metadata `:8775` — node-local, reached by VMs via link-local; not a Service, not in the catalog |
| **Coupling** | catalog correctness requires `endpointHost` set (else `localhost`); scheme follows `tls.enabled` |

API versions advertised (constitution / Flamingo): Identity v3, Compute v2.1, Network v2.0,
Block Storage v3, Image v2, Placement v1.0.

## 4. Contract document

| Attribute | Value |
|---|---|
| Artifact | `charts/o3k/CONTRACT.md` (ships in the package; SPDX header) |
| Sections | inputs (→ schema), status derivation (→ status-contract), endpoints (→ identity-anchored), versioning (rides `Chart.version`) |
| Discoverability | linked from `charts/o3k/README.md` |
| Versioning | rides `Chart.version` SemVer; MAJOR may break, MINOR/PATCH compatible (R7) |
| Drift control | schema↔values coverage gate + rejection tests keep it honest (SC-005) |
