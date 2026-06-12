# O3K Helm Chart

Deploy [O3K](https://github.com/cobaltcore-dev/o3k) — a lightweight, OpenStack-compatible
cloud in a single binary — on Kubernetes, in **single-node** or **multi-node** mode from
one chart.

> **Scope.** The validated deliverable is **single-node, stub mode (plus optional TLS)** —
> the MVP. Real-mode and multi-node templates are authored and render/lint-clean, but their
> end-to-end behavior (VM login, floating IPs, cross-node traffic, networking surviving
> restarts) depends on product post-conditions tracked in
> [issue #52](https://github.com/cobaltcore-dev/o3k/issues/52) and is **not yet functional**.

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
| `database.existingSecret` | `""` | all | Secret providing the DB URL instead |
| `secrets.jwtSecret` | `""` **(required)** | all | token signing secret |
| `secrets.adminUser` | `admin` | all | admin account name |
| `secrets.adminPassword` | `""` **(required)** | all | admin password (no `admin/secret`) |
| `secrets.existingSecret` | `""` | all | pre-rendered Secret with an `o3k.yaml` key (GitOps) |

> Config (including credentials) is rendered into a **Secret** mounted at the binary's
> `--config` path — the binary has no per-key env override for most settings, so they are
> not injectable via individual env vars.

### Exposure & TLS
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `endpointHost` | `""` | all | advertised catalog host (`O3K_ENDPOINT_HOST`) |
| `service.type` | `ClusterIP` | all | `ClusterIP`/`NodePort`/`LoadBalancer` |
| `service.annotations` | `{}` | all | e.g. LoadBalancer annotations |
| `ingress.enabled` / `className` / `annotations` / `hosts` | `false` / … | all | external Ingress |
| `tls.enabled` | `false` | all | per-service-port TLS; sets the `https` catalog scheme |
| `tls.secretName` / `tls.certManagerIssuer` | `""` | all | operator/cert-manager cert (chart generates none) |

### Service modes
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `nova.libvirtMode` | `stub` | all | `stub` / `real` |
| `neutron.networkingMode` | `stub` | all | `stub` / `iptables` / `ebpf` |
| `neutron.securityGroupMode` | `stub` | all | `stub` / `iptables` / `ebpf` |
| `cinder.storageMode` | `stub` | all | `stub` / `local` / `rbd` / `local,rbd` |
| `glance.storageMode` | `stub` | all | `stub` / `local` / `rbd` / `s3` / hybrids |

### Real mode (privileged; e2e deferred — #52)
| Key | Default | Mode | Purpose |
|-----|---------|------|---------|
| `realMode.enabled` | `false` | real | gate privileged pod + host mounts |
| `realMode.privileged` / `hostNetwork` | `false` | real | host-level access |
| `realMode.libvirtSocketPath` | `/var/run/libvirt/libvirt-sock` | real | host libvirtd socket (hostPath) |
| `realMode.hostStoragePath` | `/var/lib/o3k` | real | node-local VM/volume/image data (hostPath) |
| `realMode.floatingIpPool` | `""` | real | operator pool routed to nodes (FR-009c) |
| `podSecurity.applyPrivilegedNamespaceLabel` | `false` | real | label the namespace `privileged` (FR-012d) |

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
| `dataRetentionPolicy` | `retain` | all | `retain`/`delete` node-local data on uninstall |
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

Set `tls.enabled=true` with `ingress.enabled=true` and a cert (`tls.secretName` or
`tls.certManagerIssuer`). The chart advertises `https://` catalog/self-link URLs
(`O3K_ENDPOINT_SCHEME=https`). TLS terminates per service port; the chart generates no certs.

## Upgrade

```bash
helm upgrade o3k ./charts/o3k --reuse-values --set logging.level=debug
```
Config changes roll the pod (a config checksum annotation). Single-node real upgrades use
`Recreate` (brief downtime); multi-node uses a DaemonSet rolling update. The external
database and any networked/host storage are untouched — **no data loss** (FR-014, SC-005).

## Uninstall & data retention

```bash
helm uninstall o3k
```
Removes all release-owned resources (Deployment/DaemonSet, Service, Secret, Ingress) — no
orphans (SC-006). **Not** removed: the external database, networked backends (RBD/S3), and
**node-local `hostPath` data** on each node (not Helm-managed). `dataRetentionPolicy`
documents intent; clean node-local data manually when `delete`.

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
