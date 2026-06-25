# O3K Helm Chart

Deploy [O3K](https://github.com/cobaltcore-dev/o3k) — a lightweight, OpenStack-compatible
cloud in a single binary — on Kubernetes, in **single-node** or **multi-node** mode from
one chart.

> **Scope.** **Single-node real mode is host-validated** (SPEC-009, 2026-06-22): VM
> boot/login, metadata + cloud-init, networking surviving restarts, north-south (egress,
> floating IPs, security groups), and block storage all work end-to-end on Linux +
> libvirt — see `docs/specs/009-single-node-real-mode/HOST-TESTING.md`. **Multi-node**
> (cross-node traffic) and routing a floating-IP **pool** to a *hosting* node remain the
> v2 ticket ([issue #52](https://github.com/cobaltcore-dev/o3k/issues/52) Part B). Stub
> mode (plus optional TLS) remains the zero-dependency default.

## Prerequisites

- Kubernetes **≥ 1.25** (PodSecurity Admission GA) or k3s; Helm **≥ 3.12**.
- A reachable **external PostgreSQL** — the chart deploys no database (FR-006).
- An **O3K container image** — required, no default (FR-018).
- Real mode only: Linux nodes with a running host `libvirtd` + qemu/KVM, and an image that
  includes `dnsmasq` (the reference Dockerfile does).

## Install

```bash
helm install o3k ./charts/o3k \
  --set image.repository=<registry>/o3k \
  --set image.tag=<tag> \
  --set database.url='postgres://o3k:<pw>@<host>:5432/o3k?sslmode=disable' \
  --set secrets.jwtSecret='<strong-random>' \
  --set secrets.adminPassword='<non-default-admin-pw>'
```

Installation **fails fast** with a clear message if `image.repository`, the token secret,
the admin password, or the database URL is missing — there are no insecure defaults.

Then: `kubectl rollout status deploy/o3k-o3k` and `helm test o3k`.

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
| `database.existingSecret` | `""` | all | Secret holding the DB URL (alternative to `database.url`); injected as `O3K_DB_URL` |
| `database.existingSecretKey` | `url` | all | key within `database.existingSecret` that holds the URL |
| `secrets.jwtSecret` | `""` **(required)** | all | token signing secret |
| `secrets.adminUser` | `admin` | all | admin account name |
| `secrets.adminPassword` | `""` **(required)** | all | admin password (no `admin/secret`) |
| `secrets.existingSecret` | `""` | all | pre-rendered Secret with an `o3k.yaml` key (GitOps) |

> Config (including credentials) is rendered into a **Secret** mounted at the binary's
> `--config` path — the binary has no per-key env override for most settings, so they are
> not injectable via individual env vars. The pod also sets
> `automountServiceAccountToken: false` (O3K never calls the Kubernetes API).

### Exposure & TLS
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `endpointHost` | `""` | all | advertised catalog host (`O3K_ENDPOINT_HOST`) |
| `service.type` | `ClusterIP` | all | `ClusterIP`/`NodePort`/`LoadBalancer` |
| `service.annotations` | `{}` | all | e.g. LoadBalancer annotations |
| `ingress.enabled` / `className` / `annotations` / `hosts` | `false` / … | all | external Ingress |
| `tls.enabled` | `false` | all | per-service-port TLS; sets the `https` catalog scheme |
| `tls.secretName` | `""` → `<release>-o3k-tls` | all | cert Secret; defaults to `<release>-o3k-tls` when unset |
| `tls.certManagerIssuer` | `""` | all | cert-manager (Cluster)Issuer name; chart emits the annotation so the cert is provisioned |
| `tls.certManagerKind` | `ClusterIssuer` | all | which cert-manager annotation: `ClusterIssuer` or `Issuer` |

### Service modes
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `nova.libvirtMode` | `stub` | all | `stub` / `real` |
| `neutron.networkingMode` | `stub` | all | `stub` / `iptables` / `ebpf` |
| `neutron.securityGroupMode` | `stub` | all | `stub` / `iptables` / `ebpf` |
| `cinder.storageMode` | `stub` | all | `stub` / `local` / `rbd` / `local,rbd` |
| `glance.storageMode` | `stub` | all | `stub` / `local` / `rbd` / `s3` / hybrids |

### Real mode (privileged; single-node host-validated — SPEC-009)
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `realMode.enabled` | `false` | real | gate privileged pod + host mounts |
| `realMode.privileged` / `hostNetwork` | `false` | real | host-level access |
| `realMode.libvirtSocketPath` | `/var/run/libvirt/libvirt-sock` | real | host libvirtd socket (hostPath) |
| `realMode.hostStoragePath` | `/var/lib/o3k` | real | node-local VM/volume/image data (hostPath) |
| `realMode.floatingIpPool` | `""` | real | operator pool routed to nodes (FR-009c) |
| `podSecurity.applyPrivilegedNamespaceLabel` | `false` | real | label the namespace `privileged` (FR-012d) |

**Single-node real-mode recipe.** Needs a Linux node with a running host `libvirtd` +
qemu/KVM and an o3k image carrying `qemu-img` + `genisoimage` + `dnsmasq` (the reference
Dockerfile does). Node-local data lives under `/var/lib/o3k` (hostPath):
```bash
helm install o3k charts/o3k \
  --set image.repository=<your-o3k-image> \
  --set database.url='postgres://o3k:<pw>@<host>:5432/o3k?sslmode=disable' \
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
| `multiNode.nodeSelectorLabel` | `o3k.io/compute=true` | multi-node | DaemonSet node selector |
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
(defaulting to `<release>-o3k-tls`), so cert-manager provisions the cert. The chart never
generates certs itself. Either way it advertises `https://` catalog/self-link URLs
(`O3K_ENDPOINT_SCHEME=https`); TLS terminates per service port.

## Upgrade

```bash
helm upgrade o3k ./charts/o3k --reuse-values --set logging.level=debug
```
Config changes roll the pod (a config checksum annotation) — except with
`secrets.existingSecret`, where the chart can't hash a Secret it doesn't render, so roll the
pod yourself (`kubectl rollout restart`, or a tool like Reloader). Single-node real upgrades
use `Recreate` (brief downtime); multi-node uses a DaemonSet rolling update. The external
database and any networked/host storage are untouched — **no data loss** (FR-014, SC-005).

## Uninstall & data retention

```bash
helm uninstall o3k
```
With the default `dataRetentionPolicy: retain`, this removes all release-owned Kubernetes
resources (Deployment/DaemonSet, Service, Secret, Ingress) — no orphans (SC-006) — and leaves
**all persisted/runtime state untouched**: the external database, networked backends (RBD/S3),
and node-local `hostPath` data on each node.

### Destructive teardown — `dataRetentionPolicy: delete` (SPEC-010 / issue #53)

Set `dataRetentionPolicy: delete` (and supply `teardown.kubectlImage`) to make `helm uninstall`
run a **blocking pre-delete purge** before the release is removed:

```bash
helm upgrade o3k ./charts/o3k --reuse-values \
  --set dataRetentionPolicy=delete \
  --set teardown.kubectlImage=registry.example.com/kubectl:1.31
helm uninstall o3k          # blocks on the purge hooks until they succeed
```

The hooks run in order: **quiesce** (scale the Deployment to 0, then delete the release's PVCs —
which `helm uninstall` does *not* remove by default), then **purge** (`o3k cleanup`: destroy
libvirt domains; remove all host network state — bridges, SNAT / floating-IP / security-group /
metadata iptables, dnsmasq, namespaces; and remove all node-local storage under `/var/lib/o3k`).
The purge is blocking and fail-closed — if it cannot complete, `helm uninstall` aborts; inspect
`kubectl logs job/<release>-o3k-teardown-purge`.

- **Irreversible on the node.** `delete` destroys the node's local VM disks / images / volumes / ISOs. The external **database is operator-owned and is NOT touched** (read-only for enumeration) — a pristine redeploy requires the operator to provide a clean database.
- **Operator-owned, NOT purged:** networked backends (Ceph RBD / S3). Reclaim those yourself if desired.
- **Escape hatch:** `helm uninstall o3k --no-hooks` removes the release *without* purging (e.g. the
  hosting node is gone). You are then responsible for any orphaned host state.
- **Single-node only** today; multi-node DaemonSet host purge is tracked in issue #52.

| value | default | applies | purpose |
|-------|---------|---------|---------|
| `dataRetentionPolicy` | `retain` | all | `retain` keeps all state; `delete` runs the irreversible purge above |
| `teardown.kubectlImage` | `""` | `delete` | **required** under `delete`: kubectl image for the quiesce/PVC-delete Job (no implicit pull) |
| `teardown.timeoutSeconds` | `600` | `delete` | blocking-hook bound (purge Job `activeDeadlineSeconds`) |
| `teardown.deletePVCs` | `true` | `delete` | delete release-owned PVCs during teardown |

## Multiple releases

All resources are release-prefixed (`<release>-o3k-*`), so multiple releases coexist in one
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

Floating IPs stay real, API-compatible OpenStack IP objects: O3K programs the DNAT on the
node hosting the VM (FR-009c). Your job as operator is only to **route the pool to the
nodes** (`realMode.floatingIpPool`, e.g. `192.0.2.128/25`):

- **MetalLB (cloud / bare metal):** announce the pool from the O3K nodes in L2 mode —

  ```yaml
  apiVersion: metallb.io/v1beta1
  kind: IPAddressPool
  metadata: { name: o3k-floating, namespace: metallb-system }
  spec:
    addresses: ["192.0.2.128/25"]
  ---
  apiVersion: metallb.io/v1beta1
  kind: L2Advertisement
  metadata: { name: o3k-floating, namespace: metallb-system }
  spec:
    ipAddressPools: ["o3k-floating"]
    nodeSelectors:
      - matchLabels: { o3k.io/compute: "true" }
  ```

- **Routed / L2 subnet (datacenter, lab):** point a static route for the pool at the
  node uplinks (or place the pool in the nodes' L2 segment). No MetalLB needed.

The chart installs neither MetalLB nor host routes; it only carries the pool value and
this documentation. End-to-end floating-IP traffic additionally requires the product
post-condition WS-5 (issue #52).

## Testing

```bash
helm lint ./charts/o3k -f charts/o3k/ci/test-values.yaml
charts/o3k/ci/run-tests.sh                       # lint + template + helm-unittest
./test/helm_singlenode_stub_test.sh              # kind/k3s smoke (needs a cluster)
./test/helm_upgrade_uninstall_test.sh            # day-2 scenario (needs a cluster)
```
