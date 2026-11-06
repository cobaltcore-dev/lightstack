# Lightstack Helm Chart

Deploy [lightstack](https://github.com/cobaltcore-dev/lightstack) — a lightweight, OpenStack-compatible
cloud in a single binary — on Kubernetes, in **single-node** or **multi-node** mode from
one chart.

> **Scope.** **Single-node real mode is host-validated** (SPEC-009, 2026-06-22): VM
> boot/login, metadata + cloud-init, networking surviving restarts, north-south (egress,
> floating IPs, security groups), and block storage all work end-to-end on Linux +
> libvirt — see `docs/specs/009-single-node-real-mode/HOST-TESTING.md`. **Multi-node**
> (cross-node traffic) and routing a floating-IP **pool** to a *hosting* node remain the
> v2 ticket ([issue #52](https://github.com/cobaltcore-dev/lightstack/issues/52) Part B). Stub
> mode (plus optional TLS) remains the zero-dependency default.

## Integration contract (sci-in-a-box)

An orchestrator drives lightstack's lifecycle through this chart — **not** a CRD. The input schema, the
Provisioning/Ready/Failed status conventions, and identity-anchored endpoint resolution are
specified in **[CONTRACT.md](./CONTRACT.md)**.

## Health, readiness & probes (SPEC-013)

Lightstack exposes three **unauthenticated** endpoints on the identity port (35357):

| Path | Purpose | Behavior |
|------|---------|----------|
| `GET /readyz` | readiness | `200` only when the database is reachable **and** all HTTP listeners are bound; `503` + a short reason (e.g. `database unavailable`) otherwise. In real mode it also waits until the console socket directory has been prepared once, which takes the host's libvirt answering (`hypervisor unavailable` until then); later libvirt outages do not affect it, and a host where it never succeeds is restarted by the startup probe. |
| `GET /livez` | liveness | `200` while the process is up; **never** touches the database — a DB outage must not restart the pod. |
| `GET\|HEAD /healthcheck` | OpenStack/oslo alias | alias of `/readyz`; content-negotiates `text/plain` (default) and `application/json`. |

The chart wires `readinessProbe`/`startupProbe` → `/readyz` and `livenessProbe` → `/livez` (identically on the single-node Deployment and the multi-node DaemonSet). Tune timing/thresholds under `probes:` — paths are fixed. Defaults: ~150 s startup grace, ~30 s readiness-failure detection, ~60 s liveness-failure restart.

**Check health the k8s-native way (no custom protocol):**

```bash
helm install lightstack charts/lightstack -f values.yaml --wait      # blocks until the Pod is Ready
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=lightstack --timeout=180s
lightstack healthcheck --ready                                 # CLI probe; exit 0 = ready, 1 = not
```

The machine-readable signal is the **Pod `Ready` condition** and its roll-ups (`Deployment` Available, `--wait`). There is no Lightstack metrics endpoint — scrape readiness via `kube-state-metrics` (`kube_pod_status_ready`).

**Zero-downtime rollouts:** set `drainSeconds` > 0 so Lightstack reports not-ready and waits for endpoint deregistration before stopping; keep `terminationGracePeriodSeconds` above it.

**Security:** these endpoints are unauthenticated. If the identity port is exposed externally, restrict `/readyz`, `/livez`, and `/healthcheck` at your load balancer / Ingress (per OpenStack guidance). They reveal only coarse status + a short reason — never error detail.

## Prerequisites

- Kubernetes **≥ 1.25** (PodSecurity Admission GA) or k3s; Helm **≥ 3.12**.
- A reachable **external PostgreSQL** — the chart deploys no database (FR-006).
- An **lightstack container image** — required, no default (FR-018).
- Real mode only: Linux nodes with a running host `libvirtd` + qemu/KVM, and an image that
  includes `dnsmasq` (the reference Dockerfile does).

## Install

```bash
helm install lightstack ./charts/lightstack \
  --set image.repository=<registry>/lightstack \
  --set image.tag=<tag> \
  --set database.url='postgres://lightstack:<pw>@<host>:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret='<strong-random>' \
  --set secrets.adminPassword='<non-default-admin-pw>'
```

Installation **fails fast** with a clear message if `image.repository`, the token secret,
the admin password, or the database URL is missing — there are no insecure defaults.

Then: `kubectl rollout status deploy/lightstack-lightstack` and `helm test lightstack`.

## Configuration values

Defaults target the single-node stub MVP. "Mode" = where the value applies.

### Image & mode
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `image.repository` | `""` **(required)** | all | container image — no default (FR-018) |
| `image.tag` | `""` → `Chart.appVersion` | all | image tag |
| `image.pullPolicy` | `IfNotPresent` | all | |
| `image.pullSecrets` | `[]` | all | image pull secrets |
| `deploymentMode` | `single-node` | all | `single-node` (Deployment) or `multi-node` (DaemonSet) |
| `replicaCount` | `1` | single-node | stub replicas (real forces 1) |

### Database & secrets (required)
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `database.url` | `""` **(required)** | all | external PostgreSQL URL |
| `database.existingSecret` | `""` | all | Secret holding the DB URL (alternative to `database.url`); injected as `LIGHTSTACK_DB_URL` into the Lightstack pods and the teardown purge Job |
| `database.existingSecretKey` | `url` | all | key within `database.existingSecret` that holds the URL |
| `secrets.jwtSecret` | `""` **(required)** | all | token signing secret |
| `secrets.adminUser` | `admin` | all | admin account name |
| `secrets.adminPassword` | `""` **(required)** | all | admin password (no `admin/secret`) |
| `secrets.existingSecret` | `""` | all | pre-rendered Secret with a `lightstack.yaml` key (GitOps); see [UPGRADING](../../docs/UPGRADING.md) when upgrading an existing one |

> Config (including credentials) is rendered into a **Secret** mounted at the binary's
> `--config` path — the binary has no per-key env override for most settings, so they are
> not injectable via individual env vars. The pod also sets
> `automountServiceAccountToken: false` (lightstack never calls the Kubernetes API), and names its
> node in `LIGHTSTACK_NODE_NAME` (`spec.nodeName`): block-storage creates are recorded under that
> name, so a recreated pod — a new pod name — still sweeps the creates its predecessor left
> unfinished.
>
> Several processes can share that node name — stub replicas on one node, or an old and new
> pod overlapping during a rolling update. Each also records a per-process owner on the
> block-storage reservations it makes and heartbeats it while it runs, so the sweep sets to
> `error` only a create whose owner has stopped; a co-located replica's or a surged pod's live
> create is left alone (FR-033). The sweep never removes a backing: deleting the `error` volume
> or snapshot reclaims it.

### Exposure & TLS
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `endpointHost` | `""` | all | advertised catalog host (`LIGHTSTACK_ENDPOINT_HOST`) |
| `service.type` | `ClusterIP` | all | `ClusterIP`/`NodePort`/`LoadBalancer` |
| `service.annotations` | `{}` | all | e.g. LoadBalancer annotations |
| `ingress.enabled` / `className` / `annotations` / `hosts` | `false` / … | all | external Ingress |
| `tls.enabled` | `false` | all | per-service-port TLS; sets the `https` catalog scheme |
| `tls.secretName` | `""` → `<release>-lightstack-tls` | all | cert Secret; defaults to `<release>-lightstack-tls` when unset |
| `tls.certManagerIssuer` | `""` | all | cert-manager (Cluster)Issuer name; chart emits the annotation so the cert is provisioned |
| `tls.certManagerKind` | `ClusterIssuer` | all | which cert-manager annotation: `ClusterIssuer` or `Issuer` |

### Service modes
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `keystone.rgwCompatTenantIds` | `false` | all | render dash-free project IDs in token responses for Ceph RGW Swift compatibility; uses the existing S3 tenant form |
| `nova.libvirtMode` | `stub` | all | `stub` / `real` |
| `neutron.networkingMode` | `stub` | all | `stub` / `iptables` / `ebpf` |
| `neutron.securityGroupMode` | `stub` | all | `stub` / `iptables` / `ebpf` |
| `cinder.storageMode` | `stub` | all | `stub` / `local` / `rbd` / `local,rbd` |
| `glance.storageMode` | `stub` | all | `stub` / `local` / `rbd` / `s3` / hybrids |

### Real mode (privileged; single-node host-validated — SPEC-009)
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `realMode.enabled` | `false` | real | gate privileged pod + host mounts |
| `realMode.privileged` / `hostNetwork` | `false` | real | host-level access; host networking also renders `dnsPolicy: ClusterFirstWithHostNet` on the Lightstack pod and the purge Job, so in-cluster Service names (`<service>.<namespace>.svc`) resolve |
| `realMode.libvirtSocketPath` | `/var/run/libvirt/libvirt-sock` | real | host libvirt socket; pods mount its directory (hostPath), so a re-created socket stays reachable. The default lets the binary choose `virtqemud-sock`, else `libvirt-sock`; any other value is also the socket it dials. With `secrets.existingSecret`, your own config's `nova.libvirt_uri` must name the same socket — absolute |
| `realMode.hostStoragePath` | `/var/lib/lightstack` | real | node-local VM/volume/image data (hostPath); mounted at the same path and passed to the binary as `state_path` — absolute |
| `realMode.legacyStatePath` | `""` | real | pre-rename root to migrate into `hostStoragePath` once; empty renders nothing ([UPGRADING](../../docs/UPGRADING.md)) |
| `realMode.floatingIpPool` | `""` | real | operator pool routed to nodes (FR-009c) |
| `podSecurity.applyPrivilegedNamespaceLabel` | `false` | real | label the namespace `privileged` (FR-012d) |

**Single-node real-mode recipe.** Needs a Linux node with a running host `libvirtd` +
qemu/KVM and a Lightstack image carrying `qemu-img` + `genisoimage` + `dnsmasq` (the reference
Dockerfile does). Node-local data lives under `/var/lib/lightstack` (hostPath):
```bash
helm install lightstack charts/lightstack \
  --set image.repository=<your-lightstack-image> \
  --set database.url='postgres://lightstack:<pw>@<host>:5432/lightstack?sslmode=disable' \
  --set realMode.enabled=true \
  --set realMode.privileged=true --set realMode.hostNetwork=true \
  --set podSecurity.applyPrivilegedNamespaceLabel=true
```
Then walk `docs/specs/009-single-node-real-mode/HOST-TESTING.md`: create a network, boot a
cirros VM, and exercise metadata/keyless-SSH, egress, floating IPs, security groups, and
volumes (attach, volume-from-image, boot-from-volume).

### Multi-node (e2e deferred — #52)
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `multiNode.nodeSelectorLabel` | `lightstack.io/compute=true` | multi-node | DaemonSet node selector |
| `multiNode.vxlanEnabled` | `true` | multi-node | VXLAN overlay |
| `multiNode.vxlanPort` | `4789` | multi-node | node-to-node UDP |
| `multiNode.minProductVersion` | `""` | multi-node | min product version (FR-010) |

### Cache, retention, observability
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `cache.enabled` | `false` | all | opt-in Redis cache |
| `cache.redisUrl` | `""` | all | **single shared** Redis in multi-node (FR-006a) |
| `dataRetentionPolicy` | `retain` | all | only `retain` is active; the `delete` purge is deferred to issue #53 |
| `logging.level` / `logging.format` | `info` / `json` | all | structured logs (FR-013a) |
| `resources` / `nodeSelector` / `tolerations` / `affinity` | `{}`/… | all | pod scheduling |

## Deployment modes

- **Single-node** → a `Deployment`. Stub may scale (`replicaCount`); real pins one replica
  with `Recreate`.
- **Multi-node** → a `DaemonSet` on nodes labeled `multiNode.nodeSelectorLabel`. Add capacity
  by labeling a node; remove the label to evict. *(Cross-node function gated on #52.)*
- **stub vs real** is independent of node count. Stub needs no privileges (passes `restricted`
  PodSecurity). Real needs host access on the labeled/privileged nodes.

## TLS

Set `tls.enabled=true` with `ingress.enabled=true`, then either supply your own cert Secret
(`tls.secretName`) or name a cert-manager issuer (`tls.certManagerIssuer`). With an issuer,
the chart emits the matching annotation — `cert-manager.io/cluster-issuer` by default, or
`cert-manager.io/issuer` when `tls.certManagerKind: Issuer` — and points it at `tls.secretName`
(defaulting to `<release>-lightstack-tls`), so cert-manager provisions the cert. The chart never
generates certs itself. Either way it advertises `https://` catalog/self-link URLs
(`LIGHTSTACK_ENDPOINT_SCHEME=https`); TLS terminates per service port.

In real mode the console proxy serves plain HTTP, so `tls.enabled` also needs
`nova.consoleProxyBaseUrl`: an `https` origin that terminates TLS in front of the proxy (or set
`consoleProxy.enabled=false`). The chart refuses to render without one.

## Upgrade

```bash
helm upgrade lightstack ./charts/lightstack --reuse-values --set logging.level=debug
```
Config changes roll the pod (a config checksum annotation) — except with
`secrets.existingSecret`, where the chart can't hash a Secret it doesn't render, so roll the
pod yourself (`kubectl rollout restart`, or a tool like Reloader). Single-node real upgrades
use `Recreate` (brief downtime); multi-node uses a DaemonSet rolling update. The external
database and any networked/host storage are untouched — **no data loss** (FR-014, SC-005).

## Uninstall & data retention

```bash
helm uninstall lightstack
```
With the default `dataRetentionPolicy: retain`, this removes all release-owned Kubernetes
resources (Deployment/DaemonSet, Service, Secret, Ingress) — no orphans (SC-006) — and leaves
**all persisted/runtime state untouched**: the external database, networked backends (RBD/S3),
and node-local `hostPath` data on each node.

### Destructive teardown — `dataRetentionPolicy: delete` (SPEC-010 / issue #53)

Set `dataRetentionPolicy: delete` (and supply `teardown.kubectlImage`) to make `helm uninstall`
run a **blocking pre-delete purge** before the release is removed:

```bash
helm upgrade lightstack ./charts/lightstack --reuse-values \
  --set dataRetentionPolicy=delete \
  --set teardown.kubectlImage=registry.example.com/kubectl:1.31
helm uninstall lightstack          # blocks on the purge hooks until they succeed
```

The hooks run in order: **quiesce** (scale the Deployment to 0, then delete the release's PVCs —
which `helm uninstall` does *not* remove by default), then **purge** (`lightstack cleanup`: destroy
libvirt domains; remove all host network state — bridges, SNAT / floating-IP / security-group /
metadata iptables, dnsmasq, namespaces; and remove all node-local storage under `/var/lib/lightstack`).
It finds the domains and networks in the database, through `database.url` or `database.existingSecret`.
The purge is blocking and fail-closed — if it cannot complete, `helm uninstall` aborts; inspect
`kubectl logs job/<release>-lightstack-teardown-purge`.

- **Irreversible on the node.** `delete` destroys the node's local VM disks / images / volumes / ISOs. The external **database is operator-owned and is NOT touched** (read-only for enumeration) — a pristine redeploy requires the operator to provide a clean database.
- **Operator-owned, NOT purged:** networked backends (Ceph RBD / S3). Reclaim those yourself if desired.
- **Escape hatch:** `helm uninstall lightstack --no-hooks` removes the release *without* purging (e.g. the
  hosting node is gone). You are then responsible for any orphaned host state.
- **Single-node only** today; multi-node DaemonSet host purge is tracked in issue #52.

| value | default | applies | purpose |
|-------|---------|---------|---------|
| `dataRetentionPolicy` | `retain` | all | `retain` keeps all state; `delete` runs the irreversible purge above |
| `teardown.kubectlImage` | `""` | `delete` | **required** under `delete`: kubectl image for the quiesce/PVC-delete Job (no implicit pull) |
| `teardown.timeoutSeconds` | `600` | `delete` | blocking-hook bound (purge Job `activeDeadlineSeconds`) |
| `teardown.deletePVCs` | `true` | `delete` | delete release-owned PVCs during teardown |

## Multiple releases

All resources are release-prefixed (`<release>-lightstack-*`), so multiple releases coexist in one
cluster without collision (FR-016).

## OpenShift (real mode)

OpenShift enforces SecurityContextConstraints instead of plain PodSecurity labels.
`podSecurity.applyPrivilegedNamespaceLabel` has no effect there; grant the release's
service account the `privileged` SCC on the target namespace (FR-012d):

```bash
oc adm policy add-scc-to-user privileged -z default -n <namespace>
```

Stub mode needs nothing — it runs under the default `restricted-v2` SCC.

## Floating-IP ingress (real mode, deferred — #52)

Floating IPs stay real, API-compatible OpenStack IP objects: Lightstack programs the DNAT on the
node hosting the VM (FR-009c). Your job as operator is only to **route the pool to the
nodes** (`realMode.floatingIpPool`, e.g. `192.0.2.128/25`):

- **MetalLB (cloud / bare metal):** announce the pool from the Lightstack nodes in L2 mode —

  ```yaml
  apiVersion: metallb.io/v1beta1
  kind: IPAddressPool
  metadata: { name: lightstack-floating, namespace: metallb-system }
  spec:
    addresses: ["192.0.2.128/25"]
  ---
  apiVersion: metallb.io/v1beta1
  kind: L2Advertisement
  metadata: { name: lightstack-floating, namespace: metallb-system }
  spec:
    ipAddressPools: ["lightstack-floating"]
    nodeSelectors:
      - matchLabels: { lightstack.io/compute: "true" }
  ```

- **Routed / L2 subnet (datacenter, lab):** point a static route for the pool at the
  node uplinks (or place the pool in the nodes' L2 segment). No MetalLB needed.

The chart installs neither MetalLB nor host routes; it only carries the pool value and
this documentation. End-to-end floating-IP traffic additionally requires the product
post-condition WS-5 (issue #52).

## Testing

```bash
helm lint ./charts/lightstack -f charts/lightstack/ci/test-values.yaml
charts/lightstack/ci/run-tests.sh                       # lint + template + helm-unittest
./test/helm_singlenode_stub_test.sh              # kind/k3s smoke (needs a cluster)
./test/helm_upgrade_uninstall_test.sh            # day-2 scenario (needs a cluster)
```
