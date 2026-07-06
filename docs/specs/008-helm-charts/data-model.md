# Phase 1 Data Model: SPEC-008 Helm Charts

The chart introduces **no database schema**. Its data model is the **`values.yaml` configuration surface** — the sole customization interface (FR-017, SC-007) — plus the spec's deployment entities mapped to Kubernetes objects.

**How values reach the product:** the chart renders the full `o3k.yaml` from these values into a **Secret** mounted at the binary's `--config` path (research R2). The binary reads almost everything from that file; it honors **only four** environment overrides — `O3K_DB_URL`, `O3K_JWT_SECRET` (`internal/common/config.go` `LoadConfig`) and `O3K_ENDPOINT_HOST`, `O3K_ENDPOINT_SCHEME` (request-time). In the tables below, **"Sets"** names the `o3k.yaml` key unless an env var is shown; there is **no** generic `O3K_<SECTION>_<KEY>` layer (see research R2).

## Configuration values (`values.yaml`)

### Core & image
| Value | Type | Default | Mode | Sets |
|-------|------|---------|------|------|
| `image.repository` | string | **required, no default** | all | container image (FR-018) — install `fail`s if empty |
| `image.tag` | string | `""` → chart `appVersion` | all | image tag |
| `image.pullPolicy` | string | `IfNotPresent` | all | — |
| `image.pullSecrets` | list | `[]` | all | `imagePullSecrets` |
| `deploymentMode` | `single-node`\|`multi-node` | `single-node` | all | workload-kind selector (FR-002) |
| `replicaCount` | int | `1` | single-node | Deployment replicas (stub only; real forced to 1 — FR-003b) |

### Database — required, external (FR-006)
| Value | Type | Default | Mode | Sets |
|-------|------|---------|------|------|
| `database.url` | string (secret) | `""` | all | `database.url` in `o3k.yaml`, or env `O3K_DB_URL`; install `fail`s if neither this nor `existingSecret` set |
| `database.existingSecret` / `database.existingSecretKey` | string | `""` / `url` | all | env `O3K_DB_URL` from a pre-created Secret |

### Secrets & install gates (FR-007/007a/007b/018)
| Value | Type | Default | Mode | Sets |
|-------|------|---------|------|------|
| `secrets.jwtSecret` | string (secret) | `""` | all | `keystone.jwt_secret`, or env `O3K_JWT_SECRET` — **required**, no insecure default |
| `secrets.adminUser` | string | `admin` | all | `keystone.admin_user` (file only — no env override) |
| `secrets.adminPassword` | string (secret) | `""` | all | `keystone.admin_password` (file only — no env override) — **required**; no `admin/secret` (relies on SPEC-007 WS-2) |
| `secrets.existingSecret` | string | `""` | all | pre-created Secret supplying the rendered config / `O3K_DB_URL` / `O3K_JWT_SECRET` (GitOps) |

> Because the binary has **no** `O3K_KEYSTONE_ADMIN_PASSWORD` override, the admin password is carried in the rendered `o3k.yaml`; the chart therefore renders that file into a **Secret** (not a ConfigMap) so credentials never sit in plaintext (research R2).

### Exposure & TLS (FR-008/008a/008b/009d)
| Value | Type | Default | Mode | Sets |
|-------|------|---------|------|------|
| `endpointHost` | string | `""` | all | env `O3K_ENDPOINT_HOST` — advertised catalog host |
| `service.type` | `ClusterIP`\|`NodePort`\|`LoadBalancer` | `ClusterIP` | all | front-door Service (7 ports incl. placement 8778) |
| `service.annotations` | map | `{}` | all | e.g. LoadBalancer annotations |
| `ingress.enabled` / `className` / `annotations` / `hosts` | — | `false` / … | all | Ingress for external exposure |
| `tls.enabled` | bool | `false` | all | per-service-port TLS; sets env `O3K_ENDPOINT_SCHEME=https` (FR-008b) |
| `tls.secretName` / `tls.certManagerIssuer` | string | `""` | all | operator- or cert-manager-supplied cert (chart generates none) |

### Service modes (FR-011, FR-012) — all set in `o3k.yaml` (no env overrides)
| Value | Type | Default | Mode | Sets |
|-------|------|---------|------|------|
| `nova.libvirtMode` | `stub`\|`real` | `stub` | all | `nova.libvirt_mode` |
| `neutron.networkingMode` | `stub`\|`iptables`\|`ebpf` | `stub` | all | `neutron.networking_mode` |
| `neutron.securityGroupMode` | `stub`\|`iptables`\|`ebpf` | `stub` | all | `neutron.security_group_mode` |
| `cinder.storageMode` | `stub`\|`local`\|`rbd`\|… | `stub` | all | `cinder.storage_mode` (+ `cinder.ceph_pool`/`ceph_conf`) |
| `glance.storageMode` | `stub`\|`local`\|`rbd`\|`s3`\|… | `stub` | all | `glance.storage_mode` (+ ceph/s3 settings) |

### Real mode (FR-012a/c/d/f) and real-mode networking (FR-009a/b/c)
| Value | Type | Default | Mode | Sets |
|-------|------|---------|------|------|
| `realMode.enabled` | bool | `false` | single/multi | gates privileged pod + host mounts |
| `realMode.privileged` | bool | `false` | real | `securityContext.privileged` |
| `realMode.hostNetwork` | bool | `false` | real | `hostNetwork` |
| `realMode.libvirtSocketPath` | string | `/var/run/libvirt/libvirt-sock` | real | hostPath mount (host libvirtd) |
| `realMode.hostStoragePath` | string | `/var/lib/o3k` | real | hostPath for node-local VM/volume/image data (FR-012a) |
| `realMode.floatingIpPool` | string | `""` | **real mode (single-node real & multi-node real)** | operator-supplied floating-IP pool routed to the node(s) via MetalLB/L2; the product programs DNAT on the hosting node (FR-009c). Egress (FR-009b) needs no value — automatic masquerade out the node uplink. |
| `podSecurity.applyPrivilegedNamespaceLabel` | bool | `false` | real | namespace `pod-security.kubernetes.io/enforce=privileged` (FR-012d) |

### Multi-node (FR-004/005/005a/009/010/012e)
| Value | Type | Default | Mode | Sets |
|-------|------|---------|------|------|
| `multiNode.nodeSelectorLabel` | string | `o3k.io/compute=true` | multi-node | DaemonSet `nodeSelector` (FR-005a) |
| `multiNode.vxlanEnabled` | bool | `true` (multi) | multi-node | `neutron.vxlan_enabled` in `o3k.yaml` (file only — no env) |
| `multiNode.vxlanPort` | int | `4789` | multi-node | `compute.vxlan_port`; node-to-node UDP (FR-009) |
| `multiNode.minProductVersion` | string | `""` | multi-node | documented version gate (FR-010); filled when WS-1 ships |

### Cache — optional, off (FR-006a) — set in `o3k.yaml`
| Value | Type | Default | Mode | Sets |
|-------|------|---------|------|------|
| `cache.enabled` | bool | `false` | all | `cache.enabled` |
| `cache.redisUrl` | string (secret) | `""` | all | `cache.redis_url` (single shared Redis required in multi-node) |

### Scheduling, resources, observability
| Value | Type | Default | Mode | Sets |
|-------|------|---------|------|------|
| `resources` | map | `{}` | all | container requests/limits |
| `nodeSelector` / `tolerations` / `affinity` | — | — | all | pod scheduling |
| `logging.level` / `logging.format` | string | `info` / `json` | all | `logging.level` / `logging.format` (FR-013a) |
| `rateLimiting.enabled` (+ rates) | bool | `false` | all | `rate_limiting.*` |

## Deployment entities (spec → Kubernetes)

| Spec entity | Kubernetes representation |
|-------------|---------------------------|
| Release | the Helm release; all resources `{{ .Release.Name }}-o3k-*` (FR-016) |
| Deployment Mode | `deploymentMode` value → Deployment (single-node) vs DaemonSet (multi-node) (FR-002) |
| O3K Instance | one Pod running the single binary (Deployment replica or DaemonSet pod) |
| Node Identity | per-pod `node_id`/`tunnel_ip` (auto) registered in `compute_nodes` |
| Shared State Store | external PostgreSQL (**not** deployed by the chart) |
| Configuration Values | `values.yaml` → rendered `o3k.yaml` in a Secret (+ the 4 env overrides) |
| Secrets | the rendered-config Secret, or `existingSecret` |
| Endpoint Host | `endpointHost` → env `O3K_ENDPOINT_HOST`; resolves to the Service/Ingress |
| Node-Local Data | `hostPath` volumes on the hosting node (real mode) |
| Startup Migration | in-process `MigrateUp` (golang-migrate lock) — no Job |

## Lifecycle (FR-014)

```
install  → render manifests from values → apply; instances migrate on startup, self-register, report ready
           (single-node stub fully functional now; real-mode & multi-node functional once the
            post-conditions land — issue #52)
upgrade  → values/image change re-renders; external DB + networked/host storage untouched (no data loss);
           single-node real = Recreate (brief downtime); multi-node = DaemonSet rolling update
uninstall→ all release workloads/Services removed; persisted data (external DB, hostPath/PVC, networked
           backends) retained (default) — or, under dataRetentionPolicy=delete, the SPEC-010 blocking
           pre-delete hook quiesces, then purges host + node-local storage + release PVCs; the
           operator-owned DB is read-only (never reset) and RBD/S3 stay operator-owned (issue #53)
```

## Validation rules (template-enforced)

- **Required** (else `fail` with a clear message): `image.repository`; a token signing secret; an admin password; a database URL — each satisfiable by value **or** `existingSecret` (FR-007/007a/018).
- **Multi-node real**: `glance.storageMode ∈ {rbd, s3}` **and** `cinder.storageMode == rbd`; local backends `fail`/hard-warn (FR-012e).
- **Cache + multi-node**: a single shared `cache.redisUrl` (per-pod Redis invalid → warn — FR-006a).
- **TLS**: `tls.enabled` ⇒ `O3K_ENDPOINT_SCHEME=https` + Ingress path + a referenced certificate (else surface misconfig — Edge Cases).
- **Real mode**: `realMode.enabled` ⇒ privileged + hostNetwork + host mounts; otherwise none are emitted (stub stays `restricted`-clean).
