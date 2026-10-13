# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Ultimate Project System

**IMPORTANT**: This project uses the Ultimate Project System (Beastmode × Spec-Kit synthesis).

### Workflow & Knowledge Hierarchy
- **L1 (Phase Context)**: `.specify/` and `docs/specs/NNN-*/` - feature specifications and plans
- **L2 (Domain Details)**: `docs/` guides and ADRs - on-demand
- **L3 (Specs & Artifacts)**: `docs/specs/NNN-*/` - feature specifications

### Development Workflow
```
specify → plan → tasks → implement → validate → release
```

### The Nine Articles (Constitution)
Working principles (kept here; no separate constitution file exists):
1. **Library-First** - Features begin as standalone libraries
2. **CLI Interface** - All libraries expose CLI functionality
3. **Test-First** - TDD mandatory (NON-NEGOTIABLE: tests → fail → implement)
4. **Integration Testing** - Real dependencies over mocks
5. **Observability** - Everything inspectable
6. **Versioning** - Semantic versioning
7. **Simplicity** - ≤3 projects, YAGNI principles
8. **Anti-Abstraction** - Use frameworks directly
9. **Integration-First** - Contract tests before implementation

### Available Commands
- `/constitution` - Establish project principles
- `/specify [desc]` - Create feature specification
- `/plan [stack]` - Create implementation plan
- `/tasks` - Generate task list
- `/implement [--parallel]` - Execute with parallel agents (swarm mode)
- `/validate` - Run validation gates
- `/release` - Release feature

### Swarm Mode
Parallel execution enabled for:
- Research: Multiple topics simultaneously
- Implementation: Parallel-safe waves (file isolation checked)
- Retro: Context + Meta walkers together

### Persona
When working on this project, adopt the **deadpan minimalist** persona:
- Short sentences, maximum understatement
- Competent, slightly annoyed at the work (never at the user)

## Project Overview

Lightstack is a lightweight OpenStack implementation in Go, inspired by K3s. It provides a single ~35MB binary that implements all five core OpenStack services (Keystone, Nova, Neutron, Cinder, Glance) with 100% Terraform compatibility.

**Core Philosophy:**
- **Terraform Compatibility First**: Users can use existing Terraform OpenStack provider scripts unchanged
- **UI/CLI Compatibility**: OpenStack CLI works identically; Horizon's dashboard is **usable for resource management**, with its open UI gaps listed in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- **Drop-in Replacement**: Zero modifications needed to migrate from OpenStack to Lightstack
- **Synchronous Operations**: No message queues - operations complete before API returns
- **Fail-Fast Design**: External dependency failures return immediately (< 1 second timeouts)
- **Multi-mode Support**: Each service supports stub mode (development) and real mode (production)

## Build and Development Commands

### Quick Start with Docker Compose

```bash
# Start all services (PostgreSQL + lightstack)
docker compose -f deployments/docker-compose.yml up -d

# Configure environment
source ~/.lightstack-env         # Sets OS_AUTH_URL, OS_USERNAME, etc.

# Test it works
openstack token issue
```

### Building and Running

```bash
# Build the main binary
make build                    # Outputs to bin/lightstack

# Run with configuration
make run                      # Builds and starts with config/lightstack.yaml

# Development with hot reload (requires air)
make dev                      # Auto-restarts on code changes

# Clean build artifacts
make clean
```

### Testing

```bash
# Unit tests
make test                     # Runs all Go unit tests
go test ./internal/nova/...   # Test specific package

# Integration tests
./scripts/test-all.sh                 # Comprehensive test suite
./scripts/test-keystone.sh            # Keystone-specific tests
./test/quick_test.sh                  # Fast integration test suite
./test/integration_test.sh            # Full integration test suite

# Specific feature tests
./test/horizon_compat_test.sh         # Horizon dashboard compatibility
./test/volume_attach_test.sh          # Volume attachment workflow
./test/vxlan_multinode_test.sh        # Multi-node VXLAN networking
```

### Database Operations

```bash
# Start PostgreSQL (Docker)
make db-up                    # Starts postgres:18 container (POSTGRES_VERSION variable)

# Run migrations
make migrate                  # Applies all pending migrations

# Stop PostgreSQL
make db-down                  # Stops and removes container
```

### Code Quality

```bash
# Format code
make fmt                      # Runs go fmt

# Lint code
make lint                     # Runs golangci-lint

# Install development tools
make install-tools            # Installs air, golangci-lint, migrate
```

## Architecture Overview

### Service Structure

Lightstack runs as a single process (`cmd/lightstack/main.go`) that starts seven HTTP servers on different ports:
- **Keystone** (35357): Identity service, JWT-based authentication
- **Nova** (8774): Compute service, VM lifecycle via libvirt
- **Neutron** (9696): Network service, namespace isolation via netlink
- **Cinder** (8776): Block storage, multi-backend support (local/RBD)
- **Glance** (9292): Image service, multi-backend with hybrid failover
- **Placement** (8778): stub — version discovery + empty resource-provider listings (Horizon compatibility)
- **Metadata** (8775): EC2-compatible metadata service (no auth)

Each service is initialized in `main.go` with its configuration and shares:
- **Database connection pool**: Centralized PostgreSQL access
- **Auth middleware**: Shared JWT token validation via `keystone.AuthService`
- **Logging middleware**: Structured JSON logging

### Code Organization

```
internal/                    # Private packages
├── keystone/               # Identity - JWT auth, service catalog
├── nova/                   # Compute - VM lifecycle, flavors, keypairs
├── neutron/                # Network - networks, subnets, ports, security groups
├── cinder/                 # Block storage - volumes, snapshots
├── glance/                 # Images - metadata, multi-backend storage
├── placement/              # Placement stub - version discovery (Horizon)
├── database/               # DB models, migrations (models.go, migrate.go)
├── middleware/             # Auth, logging, CORS, recovery
├── common/                 # Config loading, error handling
├── compute/                # Node registry for multi-node coordination
├── console/                # noVNC console tokens + reconciler (SPEC-021)
├── health/                 # Readiness/liveness checks (SPEC-013)
├── metadata/               # EC2 metadata service
└── teardown/               # `lightstack cleanup` host-state reclamation (SPEC-010)

pkg/                        # Public/reusable packages
├── hypervisor/             # libvirt abstraction (stub + real modes)
├── networking/             # netlink, VXLAN, security groups
└── storage/                # Storage backends (Ceph RBD, S3, local)
```

### Key Architectural Patterns

**Service Mode Pattern**: Each service (Nova, Neutron, Cinder, Glance) supports multiple modes:
- `stub`: Returns fake data, no external dependencies (default for development)
- `real`: Full implementation with external dependencies (libvirt, Ceph, etc.)
- Mode is configured via YAML (`libvirt_mode`, `networking_mode`, `storage_mode`)
- Check mode via `libvirtMode` or `networkingMode` fields in service structs

**Multi-backend Storage Pattern** (Cinder/Glance):
- Storage modes specified as comma-separated strings: `"local"`, `"rbd"`, `"s3"`, `"local,rbd"`, `"local,s3"`, `"rbd,s3"`
- First backend is primary, subsequent are fallback
- Implemented in `pkg/storage/` with `ImageStore` and backend-specific implementations

**Synchronous Operations**:
- All API operations complete before returning (no async state machines)
- VM creation: `DomainDefineXML()` + `DomainCreate()` in single API call
- Database updates happen immediately, no queues

**JWT Authentication**:
- Tokens generated by Keystone (`internal/keystone/auth.go`)
- All services except metadata use `middleware.AuthMiddleware()`
- Token contains `user_id`, `project_id`, `roles` for authorization
- No token database - tokens are stateless (HMAC-SHA256 signed)
- **Two scope kinds exist and only two**: project-scoped and explicitly unscoped. A request for a
  **domain** scope is refused with `400` naming it (SPEC-023) — Lightstack's role assignments bind a user
  to a role within a *project*, so there is no domain-level grant to read. Note this is the
  top-level `scope.domain`; the `scope.project.domain` qualifier every client sends is unaffected.

**Project Isolation**:
- All resources scoped by `project_id` from JWT token
- Per-network L2 isolation comes from one Linux bridge per network, **not** from a namespace per
  project. Nothing creates per-project namespaces (`light-ns-<project>`) any more — bridges, TAPs,
  dnsmasq, the VXLAN devices and the egress NAT rules all live in the namespace Lightstack itself runs in,
  because libvirt has to reach them there (SPEC-024 F10). `NetworkNamespaceManager` survives only so
  teardown can reclaim what older deployments left behind. Router namespaces (`qrouter-*`,
  `RouterManager`) are separate and still live. `BridgeManager`, `TAPDeviceManager` and
  `VXLANManager.AttachToBridge` take **no namespace argument** — do not add one back
- Database queries auto-filter by `project_id`
- **Except for the administrative override** (SPEC-023): a caller holding the `admin` role may act
  on any project's *server*, matching upstream Nova's `admin_or_owner` default. Creation is not
  overridable, and a server addressed by **name** never resolves outside the caller's own project —
  Lightstack accepts a name where upstream accepts only a UUID, so a shared name must not become a way to
  reach the wrong tenant's server.
- Listings never widen implicitly. `?all_tenants` is the opt-in, administrators only; a
  non-administrator passing it is silently ignored, not refused.
- Surfaces belonging to the **deployment** rather than to a project — host aggregates, the
  deployment-wide migration list, the aggregate usage report — are administrator-only. They have no
  `project_id` to filter on, so they take a role gate instead of a scoping predicate.
- Use `common.HasRole` / `common.RoleAdmin` for the role check and `nova.targetInstance` for the
  server lookup; do not re-implement either.

**Dependency Injection Pattern**: Every database-using service receives the process's one `*pgxpool.Pool` as the first parameter of its constructor, for example `nova.NewService(pool, libvirtURI, libvirtMode, cache)` in `internal/nova/handlers.go`. Handlers query through it with `svc.pool.Query(ctx, ...)`. Package-level helpers take the pool as a parameter, e.g. `database.WithTx(ctx, pool, fn)`. There is no process-wide handle. Tests hand services a pool of their own (see `internal/keystone/pool_injection_test.go`). See ADR-001.

## Operating Modes

### Stub Mode (Development - Default)
Safe for macOS and non-Linux systems. No external dependencies required.

**Nova stub mode**:
- Returns fake VM instances, no actual VMs created
- Check: `svc.libvirtMode == "stub"` or `svc.vmManager == nil`
- VMs tracked in database only

**Neutron stub mode**:
- Returns network objects without creating namespaces/bridges
- No iptables rules or actual networking

**Cinder/Glance stub mode**:
- Tracks volumes/images in database only
- No actual storage operations

### Real Mode (Production)
Requires Linux with appropriate dependencies.

**Nova real mode** (`libvirt_mode: real`):
- Requires libvirt + KVM on Linux
- Creates actual VMs via `github.com/digitalocean/go-libvirt`
- XML templates in `pkg/hypervisor/xml_template.go`

**Neutron real mode** (`networking_mode: iptables` or `ebpf`):
- Requires Linux with network namespaces
- Uses `github.com/vishvananda/netlink` for namespace/bridge creation
- iptables via `github.com/coreos/go-iptables` for security groups

**Cinder/Glance real mode** (`storage_mode: local`, `rbd`, `s3`, or hybrid):
- `local`: Host filesystem storage
- `rbd`: Ceph RBD via `github.com/ceph/go-ceph`
- `s3`: AWS S3/MinIO via `github.com/aws/aws-sdk-go-v2`

## Database Schema

PostgreSQL with 35+ tables (see `internal/database/models.go` and `migrations/`, currently 73 up/down pairs). Key tables:

- **users**: User credentials (bcrypt hashed passwords)
- **projects**: Projects/tenants
- **roles**, **role_assignments**: RBAC
- **instances**: VM instances (linked to flavors, images)
- **flavors**: VM flavor definitions
- **networks**, **subnets**, **ports**: Network topology
- **volumes**: Block storage volumes
- **images**: Image metadata (data in storage backend)
- **keypairs**: SSH public keys
- **compute_nodes**: Multi-node registry for VXLAN coordination

Migrations in `migrations/` directory, applied via `golang-migrate/migrate`.

## Testing Guidelines

### Unit Tests
- Located alongside source files as `*_test.go`
- Test database operations with mock connections or test DB
- Test mode detection logic (stub vs real)

### Integration Tests
All integration tests are bash scripts in `test/` directory that:
1. Start Lightstack in background
2. Use OpenStack CLI (`openstack` command) to test workflows
3. Verify responses with `jq` for JSON parsing
4. Clean up resources

**Common test patterns:**
```bash
# Authenticate
openstack token issue

# Test resource lifecycle
openstack server create --flavor m1.small --image cirros test-vm
openstack server show test-vm -f json | jq -r '.status'
openstack server delete test-vm
```

### Running Tests
- Integration tests require Lightstack to be running: `docker compose up -d` or `make run`
- Tests use environment variables from `.env` or `~/.lightstack-env`
- Quick validation: `./test/quick_test.sh` (runs in ~30 seconds)

### Test-First Development (Constitution Article III)
When implementing new features:
1. **Write tests first** - Create `*_test.go` files
2. **Get approval** - Review test strategy
3. **Confirm RED** - Tests must fail initially
4. **Implement** - Write code to make tests pass (GREEN)
5. **Refactor** - Clean up while keeping tests green

This is **NON-NEGOTIABLE** per the project constitution.

## Configuration

Configuration loaded from `config/lightstack.yaml` (or via `--config` flag).

**Critical settings:**
- `database.url`: PostgreSQL connection string
- `keystone.jwt_secret`: MUST change in production (security critical)
- `nova.libvirt_mode`: `stub` (dev) or `real` (prod)
- `neutron.networking_mode`: `stub`, `iptables`, or `ebpf`
- `cinder.storage_mode`: `stub`, `local`, `rbd`, or `local,rbd` (no S3 backend — that is Glance-only)
- `glance.storage_mode`: `stub`, `local`, `rbd`, `s3`, or hybrid (comma-separated)
- `rate_limiting.enabled`: Enable rate limiting (default: false) — **not yet implemented: dead config** (see ADR-002)
- `rate_limiting.auth_rate`: Requests/minute per IP for auth endpoints — **not yet implemented: dead config**
- `rate_limiting.api_rate`: Requests/minute per project for API endpoints — **not yet implemented: dead config**

**Multi-node VXLAN** (advanced):
- Set `neutron.vxlan_enabled: true`
- Configure `compute.node_id` and `compute.tunnel_ip`
- Enables cross-node VM networking via VXLAN overlay

### Default Credentials
Seed data creates:
- **User**: `admin`
- **Password**: `secret`
- **Project**: `default`

*Change these in production environments.*

## Common Patterns and Idioms

### Commit Messages
Follow conventional commits format:
```
<type>(<scope>): <description>

[optional body]
```

**Types**: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`

**Examples**:
- `feat(glance): add S3 backend support for image storage`
- `fix(nova): correct power_state in server detail response`
- `refactor(cinder): simplify volume attachment logic`

### Error Handling
```go
// Always wrap errors with context
if err != nil {
    return fmt.Errorf("failed to create volume: %w", err)
}

// Use helper functions for HTTP errors (internal/common/error_helpers.go)
return common.HandleError(c, err)
```

### Database Queries
```go
// Always use context, the service's injected pool, and prepared statements
ctx := c.Request.Context()
err := svc.pool.QueryRow(ctx,
    "SELECT id FROM instances WHERE project_id = $1",
    projectID,
).Scan(&instanceID)
```

### Mode Detection
```go
// Nova: Check libvirtMode or vmManager
if svc.libvirtMode == "stub" || svc.vmManager == nil {
    // Stub mode: return fake data
}

// Neutron: Check networkingMode
if svc.networkingMode == "stub" {
    // Stub mode: skip netlink operations
}
```

### Microversion Negotiation (Nova)
Nova supports OpenStack microversions. Check request header:
```go
requestedVersion := c.GetHeader("OpenStack-API-Version")
if requestedVersion == "" {
    requestedVersion = c.GetHeader("X-OpenStack-Nova-API-Version")
}
```

## Important Notes

### Security Considerations
- Never commit production JWT secrets
- Database passwords should use environment variables in production
- Token TTL default is 24h (configurable)

### Backwards Compatibility
- All API endpoints must maintain OpenStack API compatibility
- Breaking API changes require OpenStack microversion bumps
- Horizon dashboard compatibility is tested in `test/horizon_compat_test.sh`

### Performance
- Database connection pool default: 20 connections
- libvirt timeouts: 1 second for fail-fast
- Ceph timeouts: 1 second for fail-fast

### Platform Support
- **macOS**: Stub mode only (no libvirt/KVM)
- **Linux**: All modes supported
- **Windows**: Not tested, likely stub mode only

## Troubleshooting Development Issues

**"Failed to connect to libvirt"**: Use stub mode on macOS or ensure libvirt is running on Linux
**"Network namespace not found"**: Requires root/sudo on Linux, use stub mode otherwise
**"Database connection failed"**: Check PostgreSQL is running and connection string is correct
**"Token validation failed"**: Ensure `jwt_secret` matches between Keystone and other services

## Active Technologies
- Go 1.27 + Gin (HTTP framework), pgx (PostgreSQL driver), gophercloud (contract testing), go-libvirt (hypervisor), netlink (networking) (002-horizon-full-compatibility)
- PostgreSQL 18 (primary database) (002-horizon-full-compatibility)
- 007-helm-prereqs-bootstrap-catalog: admin-password bootstrap (bcrypt, `internal/keystone`) + `LIGHTSTACK_ENDPOINT_SCHEME` catalog scheme — no new dependencies, no schema change
- 009-single-node-real-mode: real-mode single node — per-instance qcow2 overlay over a raw base, startup+periodic network reconciler, metadata/cloud-init reachability + `user_data`, north-south (reuse router NAT), block storage (sync attach + boot-from-volume). No new Go deps; image adds `qemu-img`+`genisoimage`; migration 060 (`volumes.source_image_id`); node-local data under `/var/lib/lightstack`.
- 011-ensure-storage-subdirs: real-mode node-local storage subdirs guaranteed to exist — Glance creates its canonical `/var/lib/lightstack/images` (drops the `~/.lightstack` fallback; matches `ceph.go`) + chart initContainer pre-creates all subdirs on the `host-storage` hostPath. No new Go/chart deps, no schema change.
- Helm 3.12+ chart (Go `text/template`); JSON Schema **draft-07**; Markdown docs. No Go code changes. + Helm's built-in `values.schema.json` validation + release-status/pod-readiness reporting; the **helm-unittest** plugin (existing harness). No new Go modules, no chart dependencies. (012-integration-contract)
- N/A — the external PostgreSQL is the product's and is untouched; this feature adds no schema or migration. (012-integration-contract)
- Go 1.27 + Gin (HTTP), pgx/pgxpool (existing `database.HealthCheck`). **No new Go modules.** Chart: Helm 3.12+, JSON Schema draft-07, helm-unittest (existing harness) — no new chart deps. (013-readiness-liveness-probes)
- External PostgreSQL — **no schema, no migration, no persisted data** (readiness is a live `Ping`). (013-readiness-liveness-probes)
- 014-ceph-rgw-s3-backend: Ceph RGW (S3) as Glance backend (validate + implement) + tenant object storage. Existing aws-sdk-go-v2 with a custom `http.Client` (CA pool + connect/first-byte transport timeouts; body streams, no total cap); stdlib `crypto/hmac`+`sha256` for server-side S3 SigV4/SigV2 verification; OpenStack EC2-credentials API reusing the `credentials` table (+ `access_key` col); Nova lazy fetch-on-boot via `storage.EnsureLocalBase` (+ checksum-on-upload, node-local base reclaim). Migration 062 (`credentials.access_key` + `s3` catalog seed); RGW `implicit tenants=s3`. No new Go modules.
- Go 1.27 (module targets 1.21+) + gin-gonic/gin v1.12.0 (HTTP), google/uuid v1.6.0 (`uuid.Validate` — already imported), jackc/pgx/v5 v5.10.0 (`pgx.ErrNoRows`). No new modules. (015-identity-uuid-404)
- PostgreSQL — **no schema change, no migration** (identity id columns stay `uuid`). (015-identity-uuid-404)
- 026-rename-to-lightstack: product rename to `lightstack` across 6,749 occurrences / 482 files / 51 paths. Go 1.27.2, **no dependency change**. Module path becomes `github.com/cobaltcore-dev/lightstack`, matching the repository. Case by role: `Lightstack` prose, `lightstack` identifiers/paths, `LIGHTSTACK_` for the 77 env vars. Three hand-written exceptions: iptables chain prefixes → `LS-SG-`/`LS-NSI-` (the full name overflows the 28-char budget), the catalog host token (migration `052` + `auth.go:684,686` must move together), and the acceptance check written as a character class so it does not match itself. No new tests — verification is the acceptance check plus manual checks.
- PostgreSQL — **no schema change, no new migration**; migration `052` edited in place (five `endpoints.url` rows). Nothing is deployed. (026-rename-to-lightstack)
- Go 1.27 (module targets 1.21+) + gin-gonic/gin (HTTP), jackc/pgx/v5 + pgxpool (PostgreSQL); **no new modules** (017-nova-server-detail-readback)
- PostgreSQL — new migration `063` adds `instances.key_name` (nullable `VARCHAR(255)`); reuses `server_security_groups` (migration 049) and the Neutron `security_groups` table for name→id resolution (017-nova-server-detail-readback)
- No product code. Verification tooling only — Helm 3.12+ (new verification chart; existing `charts/lightstack`), Bash + `python-openstackclient` + `curl` (existing `test/horizon_*.sh`), and the unmodified upstream kolla Horizon image (Flamingo 2025.2, Python/Django — config-only, never patched). + docker-compose (stub pass); k3s (root/privileged) + `charts/lightstack` in real mode; minikube + Rook/Ceph exposing an RGW (S3) endpoint; a new verification Horizon chart (kolla image + `horizon-config` ConfigMap + memcached + `bonigarcia/novnc`). (018-horizon-dashboard-verification)
- No schema change, no migration. Real-mode backends: node-local `/var/lib/lightstack` (VM disks, volumes), Ceph **RGW/S3** for Glance images. External PostgreSQL unchanged. **RBD not used.** (018-horizon-dashboard-verification)
- Go 1.27 (module targets 1.21+) + gin-gonic/gin (HTTP), jackc/pgx/v5 + pgxpool (PostgreSQL); `google/uuid`. **No new modules.** (019-horizon-compat-api-fixes)
- External PostgreSQL. **One** new migration `064_fix_cinder_volume_alias_localhost` (reseeds the `volume` alias `…015` endpoint URL only; s3 `…016` is config-overridden by `BuildServiceCatalog` and deliberately untouched). F9 reuses the existing `volumes.volume_type` column (migration `016`); F4 and F6 add **no** columns. (019-horizon-compat-api-fixes)
- Go 1.27 (module targets 1.21+) + gin-gonic/gin (HTTP), jackc/pgx/v5 + pgxpool (PostgreSQL). **No new modules.** (020-horizon-create-ui-enablement)
- External PostgreSQL. **No migration, no schema change** — reuses `quotas` (migration `007`, row-per-resource, `project_id UUID NOT NULL REFERENCES projects(id)`) and `cinder_quotas` (migration `039`). Neutron has no quota table and does not gain one (limits stay code defaults; usage is counted live). (020-horizon-create-ui-enablement)
- Go 1.27 (module targets 1.21+) + jackc/pgx/v5 + pgxpool (already a dependency), gin-gonic/gin. **No new modules.** (027-database-pool-injection)
- PostgreSQL. **No schema change, no migration.** The pool configuration from `lightstack.yaml` is unchanged. (027-database-pool-injection)
- Go 1.27 (module targets 1.21+) + gin-gonic/gin, jackc/pgx/v5 + pgxpool, digitalocean/go-libvirt (`DomainGetXMLDesc`, `DomainLookupByUUID` → `Domain.ID`, `ConnectGetCapabilities`, all present in the pinned revision `v0.0.0-20260609165003-6254771e63a8`), `encoding/xml`. **No new Go modules.** (021-real-novnc-console)
- PostgreSQL, migration `065_console_auth_tokens`. It is edited in place: nothing is deployed, and test databases are recreated. This amendment adds `domain_id`. Node-local: `<state_path>/console-tokens/` (the projection) and `<state_path>/console-sockets/` (QEMU-owned, 0750, sockets only). (021-real-novnc-console)
- Go 1.27 (module targets 1.21+) + `gin-gonic/gin` (HTTP), `jackc/pgx/v5` + `pgxpool` (PostgreSQL). **No new modules.** (022-project-tenant-isolation)
- External PostgreSQL. **One migration**, `072_groups_domain_id_uuid`: `groups.domain_id` (a `VARCHAR` with no foreign key since `010`) becomes `UUID NOT NULL REFERENCES domains(id)`, alias rows normalised, anything ambiguous stops the migration. Every other fix is behavioural. (022-project-tenant-isolation)
- Go 1.27 (repo pins `go 1.27.2`; module targets 1.21+) + Gin (HTTP), pgx/pgxpool (PostgreSQL), golang-jwt/v5. **No new modules.** Contract tests use the already-pinned `gophercloud v1.14.1`. (023-tenant-scoping-authz)
- PostgreSQL — **no schema change, no migration**. `instance_actions.project_id` is already `UUID NOT NULL`; the change is which value is written to it. (023-tenant-scoping-authz)
- Go 1.27 (module targets 1.21+) + `gin-gonic/gin` (HTTP), `jackc/pgx/v5` + `pgxpool` (PostgreSQL), `vishvananda/netlink` (real-mode bridge/TAP), `google/uuid`. **No new modules.** (024-neutron-api-fidelity-realmode)
- External PostgreSQL. **One** migration `066_neutron_external_networks` adds `networks.external BOOLEAN NOT NULL DEFAULT false` (reversible). The other five defects need no schema change — `subnets.gateway_ip` and `security_group_rules.port_range_min/max` are already nullable; the API just refuses to produce those states. (024-neutron-api-fidelity-realmode)
- Go 1.27 (module targets 1.21+) + `gin-gonic/gin` (HTTP), `jackc/pgx/v5` + `pgxpool` (PostgreSQL), `digitalocean/go-libvirt`, `google/uuid`. **No new modules.** (025-nova-gaps-client-discovery)
- External PostgreSQL. **Two** reversible migrations: `067_instance_fault` (`instances.fault_code`/`fault_message`/`fault_created`, nullable — makes a failed boot readable as upstream's `fault` object) and `068_instance_actions_time_index` (index on `instance_actions(start_time)` for the retention sweep). Two new `NovaConfig` settings: `state_reconcile_interval` (30s) and `instance_action_retention` (90d). (025-nova-gaps-client-discovery)
- Go 1.27 (module targets 1.21+) + `gin-gonic/gin` (HTTP), `jackc/pgx/v5` + `pgxpool` (PostgreSQL), `google/uuid`. **No new modules.** One new shared helper: a `406 Not Acceptable` constructor in `internal/common/errors.go` (the product has none). (025-followup)
- External PostgreSQL. **No schema change, no migration** — every fix is behavioural. One existing column gains a reader: `snapshots.size_gb`, so the `gigabytes` quota counts volume **and** snapshot size as upstream does. (025-followup)

## Recent Changes
- 025-followup: plan — SPEC-025's seven follow-ups (F1–F4, F6–F8), a pseudo-ticket on the same branch, **no new issue**. Same dominant finding as its parent: the shared thing exists and a caller bypasses it (`common.SQLFilter` + identity's filter pattern, `nova.CheckQuota`'s tx+row-lock, `common.BaseURL`'s host precedence, one already-shared quota resolution behind three views). Planning re-derived FR-019 from the code and found **eight** quota call sites across four files where the spec named two — three `INSERT INTO volumes` outside the shared provisioning routine, plus snapshots/backups/extend. Research (Context7, available again) turned up three things clarify could not know: `host` is **role-split** upstream so F7's refusal diverges two-sidedly, image-patch refusals are **403** not 400, and a request with **no** microversion header resolves to upstream's *minimum* where Lightstack uses its *maximum* — a third Article I deviation, pre-existing, which caps US6 to clients that negotiate explicitly. No migration, no new setting; cost sits in negative tests (every defect answers 2xx today) (SPEC-025-followup plan)
- 025-nova-gaps-client-discovery: plan — ten defects in two families (discovery self-links + compute answers that look successful). Dominant finding: **the right shape already exists and one caller ignores it** — `common.BaseURL` (5 services bypass it), `nova.targetInstance` (the action table already passes the resolved id; 17 handlers discard it and re-read the raw param), two accepted reconcilers to copy, and both halves of boot-from-volume. So: broad but shallow, **no new Go modules**, 2 small reversible migrations. Genuinely new: the self-heal of a missing libvirt guest (rebuildable from stored state — the blocker is that the `VMSpec` assembly is inline in a goroutine) and an upstream `fault` field. **B12's root cause stays open on purpose** — the chosen fix is correct whatever it turns out to be (SPEC-025 plan)
- 024-neutron-api-fidelity-realmode: plan — six Neutron defects (B6/B9/B10/B11/F10/F11). B10 is **one** change, not 244: every error path already funnels through `common.SendError`, so marking the Neutron route group converts handlers, auth middleware, panic recovery, unrouted-404 and 405 together. F10 = TAP moves to the **root** namespace (bridge, dnsmasq and SNAT are already there) + stub attach stops trusting a process-local map. B11 = honour a named project for admins, `400` otherwise (upstream `neutron_lib` rule, deliberately unlike #549's compute refusal). F11 = migration `066` + external as a **third** visibility disjunct (not requiring `shared`), with pre-existing gateways grandfathered (SPEC-024 plan)
- 023-tenant-scoping-authz: plan — finish applying the admin override across compute's server operations (upstream `admin_or_owner`, creation excluded) + `all_tenants` opt-in; refuse domain-scoped auth with `400` instead of `201`-with-nothing; close the `os-simple-tenant-usage` cross-tenant leak; honour query filters on 6 identity collections. No new deps, no schema change, no migration (SPEC-023 plan)
- 027-database-pool-injection: plan — remove the process-wide database handle. Every database-using constructor takes the `*pgxpool.Pool` first, and the 20 package-level helpers take it as a parameter. `WithTx(ctx, pool, fn)`. One commit, no API or schema change (SPEC-027 plan)
- 026-rename-to-lightstack: plan — staged rename (8 stages, gates green per stage); spec dir stays **untracked** (it cannot shed its own 70 occurrences); real-mode host validation mandatory (stub mode never reaches the chain namer or the state root) (SPEC-026 plan)
- 014-ceph-rgw-s3-backend: plan — RGW/S3 Glance backend (validate + boot lazy-fetch / timeout rework / TLS-CA) + tenant object storage (EC2-creds API + public self-validating `/v3/s3tokens` + `implicit tenants=s3`); migration 062 (`credentials.access_key` + `s3` catalog) (SPEC-014 plan)
- 011-ensure-storage-subdirs: fix real-mode storage-subdir gap (cloud-storage#510) — Glance no longer falls back to `~/.lightstack`; chart initContainer pre-creates node-local subdirs (SPEC-011 plan)
- v0.7.0 planning: Database DI (ADR-001), rate limiting (ADR-002), Terraform conformance, CI linting, Cinder route redirect, pagination standardization (SPEC-006)
- 002-horizon-full-compatibility: Added Go 1.27 + Gin (HTTP framework), pgx (PostgreSQL driver), gophercloud (contract testing), go-libvirt (hypervisor), netlink (networking)
- 009-single-node-real-mode: real-mode VM disk/network/metadata/north-south + block storage on a single node (SPEC-009 plan)
