# Kubernetes Deployment Guide

Lightstack runs as a single binary serving all OpenStack services, making it straightforward to deploy on Kubernetes. This guide covers manual manifests and Helm chart deployment.

## Prerequisites

- Kubernetes 1.28+
- kubectl configured for your cluster
- Helm 3.x (for Helm deployment)
- PostgreSQL 18+ (external or in-cluster)
- Container image: `ghcr.io/cobaltcore-dev/lightstack:0.6.0`

## Architecture

Lightstack deploys as a single pod running 6 HTTP servers on different ports:

| Service | Port | Purpose |
|---------|------|---------|
| Keystone | 35357 | Identity & authentication |
| Nova | 8774 | Compute |
| Neutron | 9696 | Networking |
| Cinder | 8776 | Block storage |
| Glance | 9292 | Images |
| Metadata | 8775 | EC2-compatible metadata |

All state lives in PostgreSQL. The pod itself is stateless, which makes horizontal scaling and rolling updates straightforward.

---

## Quick Start with Manifests

Apply all resources in order, or combine them into a single file separated by `---`.

### Namespace

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: lightstack
```

### Secret

Sensitive values (database password, JWT secret, admin password) belong in a Secret, not a ConfigMap.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: lightstack-secrets
  namespace: lightstack
type: Opaque
stringData:
  jwt-secret: "replace-with-a-random-256-bit-value"
  admin-password: "replace-with-a-random-admin-password"
  db-password: "replace-with-your-db-password"
```

Generate a strong JWT secret:

```bash
openssl rand -hex 32
```

### ConfigMap

The ConfigMap carries the full `lightstack.yaml` configuration. The database password and JWT secret are injected at runtime via environment variables (the application reads `LIGHTSTACK_DB_URL` and `LIGHTSTACK_JWT_SECRET` when present, overriding the config file values).

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: lightstack-config
  namespace: lightstack
data:
  lightstack.yaml: |
    database:
      url: "postgres://lightstack:$(DB_PASSWORD)@postgres.lightstack.svc.cluster.local:5432/lightstack?sslmode=disable"
      max_connections: 50
      min_connections: 2
      max_conn_lifetime: 1h
      max_conn_idle_time: 10m
      health_check_period: 30s

    keystone:
      port: 35357
      jwt_secret: "overridden-by-env"
      token_ttl: 24h
      admin_user: admin
      admin_password: "replace-with-a-random-admin-password"

    compute:
      node_id: auto
      tunnel_ip: auto
      vxlan_port: 4789
      heartbeat_interval: 30s

    nova:
      port: 8774
      libvirt_uri: "qemu:///system"
      default_flavor: m1.small
      libvirt_mode: stub

    neutron:
      port: 9696
      dhcp_lease_time: 24h
      iptables_enabled: false
      networking_mode: stub
      security_group_mode: stub
      vxlan_enabled: false
      vni_range_start: 1000
      vni_range_end: 10000
      coordination_poll_interval: 1s
      vxlan_mtu: 1450

    cinder:
      port: 8776
      storage_mode: local

    glance:
      port: 9292
      storage_mode: local
      s3_region: us-east-1

    server:
      cors_allowed_origins:
        - "https://horizon.example.com"

    logging:
      level: info
      format: json

    cache:
      enabled: false
```

Key settings to adjust before deploying:

| Setting | Description |
|---------|-------------|
| `database.url` | Point to your PostgreSQL instance |
| `nova.libvirt_mode` | `stub` (dev) or `real` (Linux with KVM) |
| `neutron.networking_mode` | `stub` (dev) or `iptables` (Linux with privileges) |
| `cinder.storage_mode` | `stub`, `local`, `rbd`, or `local,rbd` |
| `glance.storage_mode` | `stub`, `local`, `rbd`, `s3`, or hybrid |
| `server.cors_allowed_origins` | Your actual Horizon URL(s) |

### Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: lightstack
  namespace: lightstack
  labels:
    app: lightstack
spec:
  replicas: 2
  selector:
    matchLabels:
      app: lightstack
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  template:
    metadata:
      labels:
        app: lightstack
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "35357"
        prometheus.io/path: "/metrics"
    spec:
      # Run migrations as an init container so the main container
      # only starts once the schema is current.
      initContainers:
        - name: migrate
          image: ghcr.io/cobaltcore-dev/lightstack:0.6.0
          command: ["/app/lightstack-migrate", "up"]
          env:
            - name: LIGHTSTACK_DB_URL
              value: "postgres://lightstack:$(DB_PASSWORD)@postgres.lightstack.svc.cluster.local:5432/lightstack?sslmode=disable"
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: lightstack-secrets
                  key: db-password
      containers:
        - name: lightstack
          image: ghcr.io/cobaltcore-dev/lightstack:0.6.0
          # Override the default CMD to skip the built-in migration step
          # (init container already ran it).
          command: ["/app/lightstack", "--config", "/app/config/lightstack.yaml"]
          ports:
            - name: keystone
              containerPort: 35357
              protocol: TCP
            - name: nova
              containerPort: 8774
              protocol: TCP
            - name: neutron
              containerPort: 9696
              protocol: TCP
            - name: cinder
              containerPort: 8776
              protocol: TCP
            - name: glance
              containerPort: 9292
              protocol: TCP
            - name: metadata
              containerPort: 8775
              protocol: TCP
          env:
            - name: LIGHTSTACK_JWT_SECRET
              valueFrom:
                secretKeyRef:
                  name: lightstack-secrets
                  key: jwt-secret
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: lightstack-secrets
                  key: db-password
            - name: LIGHTSTACK_DB_URL
              value: "postgres://lightstack:$(DB_PASSWORD)@postgres.lightstack.svc.cluster.local:5432/lightstack?sslmode=disable"
          volumeMounts:
            - name: config
              mountPath: /app/config/lightstack.yaml
              subPath: lightstack.yaml
            - name: storage
              mountPath: /var/lib/lightstack   # = state_path (default); change both together
          livenessProbe:
            httpGet:
              path: /v3
              port: keystone
            initialDelaySeconds: 30
            periodSeconds: 15
            timeoutSeconds: 5
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /v3
              port: keystone
            initialDelaySeconds: 10
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 3
          resources:
            requests:
              cpu: "250m"
              memory: "256Mi"
            limits:
              cpu: "2"
              memory: "1Gi"
      volumes:
        - name: config
          configMap:
            name: lightstack-config
        - name: storage
          # For local storage mode. Replace with a PVC for persistent storage,
          # or remove entirely when using rbd/s3 backends.
          emptyDir: {}
```

> Note: If `cinder.storage_mode` or `glance.storage_mode` is set to `local`, the storage volume above should be a PersistentVolumeClaim, not an `emptyDir`, so image/volume data survives pod restarts.

### PersistentVolumeClaim (local storage mode)

Replace the `emptyDir` volume with this PVC when using `local` storage:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: lightstack-storage
  namespace: lightstack
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: standard
  resources:
    requests:
      storage: 50Gi
```

Then update the Deployment volume entry:

```yaml
volumes:
  - name: storage
    persistentVolumeClaim:
      claimName: lightstack-storage
```

Note that `ReadWriteOnce` limits you to a single replica when using local storage. For multi-replica deployments, use `rbd` or `s3` storage backends.

### Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: lightstack
  namespace: lightstack
  labels:
    app: lightstack
spec:
  type: ClusterIP
  selector:
    app: lightstack
  ports:
    - name: keystone
      port: 35357
      targetPort: keystone
      protocol: TCP
    - name: nova
      port: 8774
      targetPort: nova
      protocol: TCP
    - name: neutron
      port: 9696
      targetPort: neutron
      protocol: TCP
    - name: cinder
      port: 8776
      targetPort: cinder
      protocol: TCP
    - name: glance
      port: 9292
      targetPort: glance
      protocol: TCP
    - name: metadata
      port: 8775
      targetPort: metadata
      protocol: TCP
```

### PodDisruptionBudget

Ensures at least one replica stays available during rolling updates or node drains:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: lightstack-pdb
  namespace: lightstack
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: lightstack
```

---

## Health Checks

Both probes hit the same endpoint:

- **Liveness**: `GET /v3` on port 35357 (Keystone version endpoint)
- **Readiness**: `GET /v3` on port 35357

If Keystone responds, the process is up and all six services are running — they share the same binary and start together. A non-200 response or connection failure signals that the pod should be restarted (liveness) or removed from load balancing (readiness).

---

## Ingress Configuration

Lightstack uses non-standard ports. Two approaches work in practice.

### Option A: Path-based routing (single hostname)

Route all OpenStack API traffic through a single hostname using path prefixes:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: lightstack
  namespace: lightstack
  annotations:
    nginx.ingress.kubernetes.io/rewrite-target: /$2
    nginx.ingress.kubernetes.io/use-regex: "true"
spec:
  ingressClassName: nginx
  rules:
    - host: openstack.example.com
      http:
        paths:
          - path: /identity(/|$)(.*)
            pathType: ImplementationSpecific
            backend:
              service:
                name: lightstack
                port:
                  name: keystone
          - path: /compute(/|$)(.*)
            pathType: ImplementationSpecific
            backend:
              service:
                name: lightstack
                port:
                  name: nova
          - path: /network(/|$)(.*)
            pathType: ImplementationSpecific
            backend:
              service:
                name: lightstack
                port:
                  name: neutron
          - path: /volume(/|$)(.*)
            pathType: ImplementationSpecific
            backend:
              service:
                name: lightstack
                port:
                  name: cinder
          - path: /image(/|$)(.*)
            pathType: ImplementationSpecific
            backend:
              service:
                name: lightstack
                port:
                  name: glance
```

### TLS and the advertised catalog scheme

The Keystone service catalog advertises one URL per service, **including its port**
(e.g. `https://<host>:9292`). To make those URLs `https`, set the `LIGHTSTACK_ENDPOINT_SCHEME=https`
environment variable (default `http`) alongside `LIGHTSTACK_ENDPOINT_HOST`; this rewrites the
scheme of every catalog endpoint and resource self-link at presentation time.

Because the advertised URLs keep their per-service ports, TLS must be terminated
**per service port** — use **Option B (port-based)** below with TLS on each port (one
host/wildcard certificate covers them all). A single-`:443` path-based ingress (Option A)
is *not* compatible with `LIGHTSTACK_ENDPOINT_SCHEME=https`, since the catalog would still point
clients at `https://<host>:<service-port>` rather than `https://<host>/<service-path>`.

### Option B: Port-based routing (separate LoadBalancer services)

The simpler approach when you can allocate multiple external IPs or use a cloud load balancer:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: lightstack-keystone-lb
  namespace: lightstack
spec:
  type: LoadBalancer
  selector:
    app: lightstack
  ports:
    - port: 35357
      targetPort: keystone
---
apiVersion: v1
kind: Service
metadata:
  name: lightstack-nova-lb
  namespace: lightstack
spec:
  type: LoadBalancer
  selector:
    app: lightstack
  ports:
    - port: 8774
      targetPort: nova
```

Repeat for each service. This matches the port layout used in the Docker Compose deployment and requires no path rewriting.

---

## Scaling

Lightstack is stateless — all state lives in PostgreSQL. Scale horizontally by increasing `replicas`:

```bash
kubectl scale deployment lightstack -n lightstack --replicas=3
```

### HorizontalPodAutoscaler

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: lightstack-hpa
  namespace: lightstack
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: lightstack
  minReplicas: 2
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
    - type: Resource
      resource:
        name: memory
        target:
          type: Utilization
          averageUtilization: 80
```

### Connection pooling

With multiple replicas each holding up to 50 database connections, connection count grows linearly. Use [PgBouncer](https://www.pgbouncer.org/) in transaction mode between Lightstack pods and PostgreSQL to cap actual backend connections:

```yaml
# Add to your deployment environment:
- name: LIGHTSTACK_DB_URL
  value: "postgres://lightstack:$(DB_PASSWORD)@pgbouncer.lightstack.svc.cluster.local:5432/lightstack?sslmode=disable"
```

---

## Monitoring

The Deployment manifest above includes Prometheus scrape annotations. Standard metrics to watch:

| Metric | Alert threshold | Notes |
|--------|----------------|-------|
| Pod restarts | > 2 in 5 min | Points to crash loop — check liveness probe |
| Readiness probe failures | Any sustained | Pod receiving traffic but not responding |
| PostgreSQL connection errors | Any | Database availability problem |
| HTTP 5xx rate | > 1% of requests | Application errors |
| Request latency p99 | > 2s | Performance degradation |

Add a `ServiceMonitor` if you use the Prometheus Operator:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: lightstack
  namespace: lightstack
spec:
  selector:
    matchLabels:
      app: lightstack
  endpoints:
    - port: keystone
      path: /metrics
      interval: 30s
```

---

## Helm Chart (Planned)

A full Helm chart is planned for a future release. When available it will live at `deployments/helm/lightstack/` and expose the following `values.yaml` structure:

```yaml
# Intended values.yaml structure (not yet available)
image:
  repository: ghcr.io/cobaltcore-dev/lightstack
  tag: "0.6.0"
  pullPolicy: IfNotPresent

replicaCount: 2

service:
  type: ClusterIP

ingress:
  enabled: false
  className: nginx
  host: openstack.example.com

config:
  nova:
    libvirtMode: stub
  neutron:
    networkingMode: stub
  cinder:
    storageMode: local
  glance:
    storageMode: local
  server:
    corsAllowedOrigins:
      - "https://horizon.example.com"

secrets:
  jwtSecret: ""      # Required — set via --set or external secret
  dbPassword: ""     # Required — set via --set or external secret

database:
  url: ""            # Full DSN, overrides individual fields

storage:
  enabled: true
  size: 50Gi
  storageClass: standard

resources:
  requests:
    cpu: 250m
    memory: 256Mi
  limits:
    cpu: "2"
    memory: 1Gi

autoscaling:
  enabled: false
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70

podDisruptionBudget:
  enabled: true
  minAvailable: 1

cache:
  enabled: false
  redisUrl: ""
```

Until the chart is published, use the manifests in this guide directly or wrap them with Kustomize.

---

## Production Considerations

### PostgreSQL

Use an external, highly available PostgreSQL instance. Options:

- **Patroni** (self-managed HA)
- **Cloud-managed**: AWS RDS, Google Cloud SQL, Azure Database for PostgreSQL
- **In-cluster operators**: CloudNativePG, Zalando Postgres Operator

Minimum version: PostgreSQL 18.

### Secrets management

Do not put the JWT secret or database password in a ConfigMap. Options beyond native Kubernetes Secrets:

- **External Secrets Operator** with AWS Secrets Manager, GCP Secret Manager, or Vault
- **Sealed Secrets** (Bitnami) for GitOps workflows
- **Vault Agent Injector** for direct pod injection

### Storage

For `cinder.storage_mode: local` or `glance.storage_mode: local`:
- Use a PVC backed by a block storage class, not `emptyDir`
- `ReadWriteOnce` limits you to a single pod — use `rbd` or `s3` backends for multi-replica deployments

For `rbd` mode, provide Ceph credentials and `ceph.conf` via a separate Secret and mount them at `/etc/ceph/`.

For `s3` mode, set `glance.s3_bucket`, `glance.s3_region`, and optionally `glance.s3_endpoint` (for MinIO or Ceph RGW).

### Networking mode

`neutron.networking_mode: iptables` requires the pod to run privileged with access to host network namespaces. Only use this on Linux worker nodes with the appropriate node-level permissions. For most Kubernetes deployments, `stub` mode is the right choice unless Lightstack is managing actual VM networking on bare-metal nodes.

### JWT secret rotation

The JWT secret is stateless — all tokens signed with the old secret become invalid immediately when the secret changes. Plan a maintenance window or implement a grace period using two active secrets if zero-downtime rotation is required.

### CORS

Set `server.cors_allowed_origins` to your actual Horizon hostname(s). The default `localhost` entries are for development only and will cause browser-side API failures in production.

### Resource sizing

Starting points based on observed usage:

| Workload | CPU request | Memory request | Replicas |
|----------|-------------|----------------|---------|
| Development | 100m | 128Mi | 1 |
| Small production | 250m | 256Mi | 2 |
| Medium production | 500m | 512Mi | 3–5 |
| Large production | 1000m | 1Gi | 5+ with HPA |
