# Lightstack Architecture

**📚 Complete Documentation**: See **[INDEX.md](INDEX.md)** for full documentation index with learning paths.


## Design Philosophy

Lightstack is built on three core principles:

1. **API Compatibility First**: 100% OpenStack API compatibility to work seamlessly with Horizon and OpenStack CLI/SDK
2. **Synchronous Operations**: No async state machines - operations complete before API returns
3. **Fail-Fast Design**: External dependency failures return immediately (< 1 second timeouts)

## System Architecture

### High-Level Overview

```
┌─────────────────────────────────────────────────────────────┐
│                    lightstack Process                        │
├─────────────────────────────────────────────────────────────┤
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐      │
│  │  Keystone    │  │     Nova     │  │   Neutron    │      │
│  │   :35357     │  │    :8774     │  │    :9696     │      │
│  └──────────────┘  └──────────────┘  └──────────────┘      │
│  ┌──────────────┐  ┌──────────────┐                        │
│  │   Cinder     │  │    Glance    │                        │
│  │   :8776      │  │    :9292     │                        │
│  └──────────────┘  └──────────────┘                        │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐  │
│  │           Common Layer                                │  │
│  │  - Database Connection Pool                          │  │
│  │  - JWT Auth Service                                  │  │
│  │  - Middleware (Auth, Logging, CORS)                  │  │
│  └──────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                         ↓
        ┌────────────────┼────────────────┐
        ↓                ↓                ↓
   PostgreSQL       libvirt (KVM)    Ceph (RBD)
   (State DB)      (Compute)         (Storage)
```

## Component Architecture

### 1. Keystone (Identity Service)

**Responsibilities:**
- User authentication (password-based)
- JWT token generation and validation
- Service catalog generation
- User/project/role management

**Technology:**
- JWT tokens (no token database)
- bcrypt password hashing
- PostgreSQL for user/project storage

**Key Design Decisions:**
- **JWT over Fernet**: Stateless tokens, no shared secret rotation needed
- **No token blacklist**: Tokens expire naturally based on TTL
- **Middleware-based auth**: All services share the same auth middleware

### 2. Nova (Compute Service)

**Responsibilities:**
- VM lifecycle management (create, delete, reboot, stop)
- Flavor management
- Hypervisor abstraction
- Microversion negotiation

**Technology:**
- `github.com/digitalocean/go-libvirt` - Pure Go libvirt bindings
- Direct libvirt socket communication (no CGO)
- XML-based VM definitions

**Key Design Decisions:**
- **Synchronous VM creation**: `DomainDefineXML()` + `DomainCreate()` in single API call
- **No async state machine**: VM status transitions happen immediately
- **Hypervisor mocking**: Fake hypervisor stats for Horizon compatibility

### 3. Neutron (Network Service)

**Responsibilities:**
- Virtual network management
- Subnet/CIDR allocation
- Port attachment to VMs
- Security groups (iptables-based)
- DHCP management (dnsmasq)

**Technology:**
- `github.com/vishvananda/netlink` - Linux networking
- `github.com/vishvananda/netns` - Network namespaces
- `github.com/coreos/go-iptables` - iptables rules

**Key Design Decisions:**
- **Namespace isolation**: Per-project network namespaces
- **No VXLAN in v1**: Single-node deployment uses bridges only
- **iptables security groups**: Simple, proven, no eBPF complexity in v1
- **dnsmasq for DHCP**: Standard, well-tested DHCP server

### 4. Cinder (Block Storage Service)

**Responsibilities:**
- Volume lifecycle management
- Volume attachment to VMs
- Snapshot management
- Volume types

**Technology:**
- `github.com/ceph/go-ceph` - Ceph RBD integration
- Direct RBD operations (create, delete, snapshot)

**Key Design Decisions:**
- **Ceph-only**: No local storage backend in v1
- **1-second timeout**: Fast failure if Ceph unavailable
- **Synchronous operations**: Volume creation completes before API returns

### 5. Glance (Image Service)

**Responsibilities:**
- Image metadata management
- Image upload/download
- Image storage (Ceph RBD backed)
- Public/private image visibility

**Technology:**
- `github.com/ceph/go-ceph` - Ceph RBD integration
- Streaming uploads/downloads

**Key Design Decisions:**
- **Ceph-backed storage**: Images stored as RBD snapshots
- **Metadata in PostgreSQL**: Fast queries, RBD only for data
- **Streaming transfers**: No full image buffering in memory

## Data Flow

### VM Creation Flow

```
1. User: openstack server create --flavor m1.small --image cirros test-vm
                    ↓
2. Nova API: POST /v2.1/servers
                    ↓
3. Validate token (JWT signature check)
                    ↓
4. Query PostgreSQL: Fetch flavor, image metadata
                    ↓
5. Query Neutron: Create ports, get network config
                    ↓
6. Generate libvirt XML with RBD backing
                    ↓
7. libvirt.DomainDefineXML() + DomainCreate()
                    ↓
8. Insert instance into PostgreSQL
                    ↓
9. Return HTTP 202 with instance details
```

### Authentication Flow

```
1. User: curl -X POST /v3/auth/tokens (with credentials)
                    ↓
2. Keystone: Validate username/password against PostgreSQL
                    ↓
3. bcrypt.CompareHashAndPassword()
                    ↓
4. If scoped: Fetch project and roles from PostgreSQL
                    ↓
5. Generate JWT token (HS256 signature)
                    ↓
6. Build service catalog dynamically
                    ↓
7. Return token in X-Subject-Token header + body
```

### Rate Limiting Architecture

Lightstack implements two-tier rate limiting as Gin middleware (see [ADR-002](adr/ADR-002-rate-limiting-architecture.md)):

```
  REQUEST
    |
    v
  Authenticated?
   / \
 NO   YES
  |     |
  v     v
 IP-based         Project-based
 rate.Limiter     rate.Limiter
 (auth endpoints) (all other endpoints)
  |                |
  v                v
 sync.Map          sync.Map
 [IP -> limiter]   [project_id -> limiter]
  |                |
  +-- evict stale entries every 10 min
  +-- cap: 10,000 entries max
  +-- overflow: allow traffic (degrade open)
```

**Technology**: `golang.org/x/time/rate` (Go standard library)

**Key Design Decisions:**
- **Two-tier, not per-endpoint**: IP-based for unauthenticated auth endpoints (`POST /v3/auth/tokens`), project-based for all authenticated endpoints
- **In-process, not Redis**: Single-binary architecture avoids external dependencies
- **Degrade open**: When entry cap (10K) is exceeded, new IPs/projects are allowed through rather than blocked
- **Configurable**: Enable/disable and adjust rates via `rate_limiting` block in `lightstack.yaml`

### Dependency Injection Pattern

Services receive their database through constructors rather than importing a global variable
(see [ADR-001](adr/ADR-001-database-dependency-injection.md) and
[ADR-003](adr/ADR-003-pluggable-database-backend.md)):

```
  main.go
    |
    +-- database.Open(ctx, url, opts) -> db database.DB   (defer db.Close())
    +-- database.MigrateUp(url, migrationsPath)
    |
    +-- health.DatabaseCheck(db)
    +-- keystone.BootstrapAdmin(ctx, db, cfg.Keystone)
    +-- keystone.NewAuthService(db, jwtSecret, ttl, cache)
    +-- keystone.NewService(db, authService, cache)
    +-- nova.NewService(db, libvirtURI, mode, cache)
    +-- neutron.NewService(db, mode, cache)
    +-- neutron.NewVXLANCoordinator(db, vxlanManager, nodeRegistry, ...)
    +-- compute.NewNodeRegistry(db, nodeID, tunnelIP, heartbeat)
    +-- cinder.NewService(db, mode, cephPool, cephConf, nodeName)
    |     +-- SweepInterruptedReservations(ctx)   (before the listeners open)
    +-- glance.NewService(db, mode, cephPool, cephConf, s3opts, cache)
    +-- metadata.NewService(db, bindAddr)
```

**Key Design Decisions:**
- **One contract, two backends**: `database.DB` is an interface over the method set the code calls
  on pgx — `Query`, `QueryRow`, `Exec`, `Begin` — with a PostgreSQL implementation (pgxpool,
  unchanged) and a SQLite implementation (`modernc.org/sqlite`, pure Go). The URL's scheme picks one
  at startup; services never ask which.
- **Same database, all services**: DI is for testability and explicit dependencies, not service isolation
- **WithTx()**: Transaction helper takes the database: `WithTx(ctx, db, fn)`
- **Helpers**: Package-level functions that query take the database as a parameter; the cleanup
  job opens and closes one of its own

### Database Backends

| | PostgreSQL | SQLite |
|---|---|---|
| `database.url` | `postgres://…` | `sqlite:///absolute/path/lightstack.db` |
| Deployment | single or multi-node | single node (VXLAN refused) |
| Driver | pgx / pgxpool | `modernc.org/sqlite` through `database/sql` |
| Concurrency | row and advisory locks | one writer; transactions begin `IMMEDIATE`, 5 s busy wait |
| SQL | as written | translated on the way in (casts, `= ANY`, `FOR UPDATE`, …) |

Callers write PostgreSQL syntax once; `internal/database`'s package documentation lists what both
backends run, and `TestSQLConstructs` fails the build on anything else. Ids, timestamps, JSON and
lists are text on SQLite, bound and scanned through the same Go types as on PostgreSQL.

**Migrations**: one `migrations/` folder serves both. Each backend reads it through a view —
`.postgres`/`.sqlite` files are used by that backend only and replace the plain file of their
number, and SQLite starts at a baseline generated from PostgreSQL's schema at 073
([migrations/README.md](../migrations/README.md)). `TestSchemaParity` compares the two schemas.

**No transaction around external work**: volume and snapshot creates reserve their row
(`creating`, under the node's name), create the backing store outside any transaction, then
finalise. At startup each node sets the reservations it left behind to `error` and reclaims their
backing. On SQLite, where one transaction holds the whole file, this is what keeps an image copy
from stopping every project's writes.

### Request Lifecycle

Full request path through Lightstack middleware stack:

```
  Client
    |
    v
  Gin Router (port-specific: 35357, 8774, 9696, etc.)
    |
    v
  CORS Middleware (configurable origins)
    |
    v
  Rate Limit Middleware (IP or project-based)
    |
    v
  Auth Middleware (JWT validation, extracts user/project/roles)
    |     (skipped for: metadata service, version endpoints)
    v
  Handler (service method on struct with injected pool)
    |
    v
  svc.pool.Query(ctx, ...) -> PostgreSQL or SQLite
    |
    v
  Response (structured error format via common.SendError on failure)
```

## Database Schema

### Core Tables

- **users**: User credentials and metadata
- **projects**: Project (tenant) metadata
- **roles**: Role definitions (admin, member, reader)
- **role_assignments**: User-project-role mappings
- **instances**: VM instances
- **flavors**: VM flavor definitions
- **networks**: Virtual networks
- **subnets**: IP subnets
- **ports**: Network ports (attached to VMs)
- **volumes**: Cinder volumes
- **images**: Glance image metadata

### Indexes

Key indexes for performance:
- `instances(project_id)` - Fast per-project filtering
- `ports(device_id)` - Fast port lookup by VM
- `role_assignments(user_id, project_id)` - Fast role checks

## Security

### Authentication

- **Password hashing**: bcrypt (cost factor 10)
- **Token signing**: HS256 (HMAC-SHA256)
- **Token TTL**: 24 hours default (configurable)

### Authorization

- **Middleware-based**: All protected endpoints check X-Auth-Token
- **Role-based access**: RequireRole() middleware for privileged ops
- **Project scoping**: Automatic filtering by project_id from token

### Network Security

- **Namespace isolation**: Projects cannot see each other's networks
- **Security groups**: Per-VM iptables rules
- **Default deny**: All traffic blocked unless explicitly allowed

## Performance Characteristics

### Target Metrics

- **API latency**: < 10ms for read operations
- **VM creation**: < 2 seconds (depends on libvirt)
- **Volume creation**: < 1 second (fail-fast if Ceph down)
- **Token validation**: < 1ms (JWT signature check)

### Connection Pooling

- **PostgreSQL**: 20 connections default (configurable)
- **libvirt**: Connection pooling with max 10 connections
- **Ceph**: Connection per operation (stateless)

### Concurrency

- **Goroutines per request**: Each HTTP request runs in separate goroutine
- **Database queries**: All queries use context for cancellation
- **Netlink operations**: Mutex per namespace to prevent races

## Deployment Models

### Single-Node (v1)

```
┌────────────────────────────────┐
│       lightstack Host          │
│                                │
│  ┌──────────────────────────┐ │
│  │  lightstack Binary       │ │
│  │  (All services)          │ │
│  └──────────────────────────┘ │
│                                │
│  ┌──────────────────────────┐ │
│  │  PostgreSQL              │ │
│  └──────────────────────────┘ │
│                                │
│  ┌──────────────────────────┐ │
│  │  libvirt + KVM           │ │
│  └──────────────────────────┘ │
└────────────────────────────────┘
          ↓
    Ceph Cluster
```

### Multi-Node (v2 - Future)

```
┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
│  Control Node   │  │  Compute Node 1 │  │  Compute Node 2 │
│                 │  │                 │  │                 │
│  - Keystone     │  │  - Nova Agent   │  │  - Nova Agent   │
│  - Nova API     │  │  - libvirt      │  │  - libvirt      │
│  - Neutron API  │  │  - Neutron Agnt │  │  - Neutron Agnt │
│  - Cinder API   │  │  - VXLAN        │  │  - VXLAN        │
│  - Glance API   │  │                 │  │                 │
│  - PostgreSQL   │  │                 │  │                 │
└─────────────────┘  └─────────────────┘  └─────────────────┘
          ↓                  ↓                    ↓
                      Ceph Cluster
```

## Observability (Future)

### Metrics (Prometheus)

- API request latency (p50, p95, p99)
- Database connection pool stats
- libvirt operation latency
- Ceph operation latency
- Active VMs/volumes/networks count

### Logging (Structured JSON)

```json
{
  "timestamp": "2024-03-06T12:34:56Z",
  "level": "info",
  "service": "nova",
  "method": "POST",
  "path": "/v2.1/servers",
  "status": 202,
  "duration_ms": 1842,
  "user_id": "abc123",
  "project_id": "def456"
}
```

### Tracing (OpenTelemetry)

- Distributed traces across services
- Span per database query
- Span per libvirt/Ceph operation

## Failure Modes

### PostgreSQL Down

- **Impact**: All API operations fail
- **Response**: HTTP 503 Service Unavailable
- **Mitigation**: Database replication in production

### Ceph Down

- **Impact**: Volume/image operations fail
- **Response**: HTTP 503 within 1 second (fast fail)
- **Mitigation**: Ceph cluster redundancy

### libvirt Down

- **Impact**: VM operations fail
- **Response**: HTTP 503
- **Mitigation**: Multi-node deployment with live migration

## Future Enhancements

### v2.0 (Multi-node)

- VXLAN overlay networks
- Floating IPs (external network access)
- Live migration
- Placement API (resource scheduling)
- eBPF security groups (kernel-space filtering)

### v3.0 (Production)

- High availability (multi-master)
- Heat (orchestration)
- Swift (object storage)
- Ceilometer (telemetry)
- Multi-region support

## References

- [OpenStack API Reference](https://docs.openstack.org/api-ref/)
- [Keystone v3 API](https://docs.openstack.org/api-ref/identity/v3/)
- [Nova v2.1 API](https://docs.openstack.org/api-ref/compute/)
- [Neutron v2.0 API](https://docs.openstack.org/api-ref/network/v2/)
