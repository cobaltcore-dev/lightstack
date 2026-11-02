# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Lightstack is a lightweight OpenStack implementation in Go, inspired by K3s. It provides a single ~35MB binary that implements the five core OpenStack services (Keystone, Nova, Neutron, Cinder, Glance), a Placement stub, and an EC2-compatible metadata service. Terraform compatibility is **validated for one topology**, not literally 100% — see the qualifiers below.

**Core Philosophy:**
- **Terraform Compatibility First**: The unmodified Terraform OpenStack provider is validated for compute, networks, security groups, and volumes on the sc007 topology; see `docs/compatibility/TERRAFORM_COMPATIBILITY_REPORT.md` and `docs/blueprint.md` §6 for the scope, the port-id workaround, and the object-storage exclusion
- **UI/CLI Compatibility**: OpenStack CLI works identically; Horizon's dashboard is **usable for resource management**, with its open UI gaps listed in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`
- **Drop-in Replacement**: Migrating from OpenStack is designed to be low-friction, but the Terraform path is not literally zero-modification — it needs a one-line port-id workaround, is validated on one real-mode (non-CI) topology, and does not serve Terraform's Swift-based object storage; see `docs/blueprint.md` §6 and the [Terraform compatibility report](docs/compatibility/TERRAFORM_COMPATIBILITY_REPORT.md)
- **Synchronous Operations**: No message queues - operations complete before API returns
- **Fail-Fast Design**: External dependency failures return immediately (< 1 second timeouts)
- **Multi-mode Support**: Each service supports stub mode (development) and real mode (production)

## Project Principles

These principles govern how features are designed and reviewed. They are referenced throughout the specs (`docs/specs/README.md`) and ADRs (`docs/adr/`):

1. **Library-First** - Features begin as standalone libraries
2. **CLI Interface** - All libraries expose CLI functionality
3. **Test-First** - TDD mandatory: write tests, confirm they fail, then implement
4. **Integration Testing** - Real dependencies over mocks
5. **Observability** - Everything inspectable
6. **Versioning** - Semantic versioning
7. **Simplicity** - YAGNI principles
8. **Anti-Abstraction** - Use frameworks directly
9. **Integration-First** - Contract tests before implementation

**SPEC-000 is priority zero**: OpenStack API compliance. Upstream OpenStack behavior is the reference implementation — never invent semantics. Every specification must comply with SPEC-000 (see `docs/specs/SPEC-000-COMPLIANCE-ADDENDUM.md`).

## Development Workflow

Significant changes follow the spec pattern (see `docs/specs/README.md`):

```
specify → plan → tasks → implement → validate
```

- Each feature lives in `docs/specs/NNN-name/` containing `spec.md`, `plan.md`, `tasks.md`, and usually `contracts/` and `checklists/` subdirectories. Read the relevant spec before changing a feature it covers.
- **API-surface changes need contract tests** in `test/contract/` (gophercloud-based). If you add or change an endpoint's behavior, add or extend a contract test there.
- **Every commit needs a DCO sign-off**: `Signed-off-by: Name <name@sap.com>` (use `git commit -s`). The "SAP Sign-off Check" CI job rejects PR commits missing a valid `@sap.com` trailer.
- **Comprehensive review-fix loops are the norm**. Reviews are documented under `docs/review/`; findings are worked until the report is CLEAN. Expect to iterate.
- Commit messages follow the conventional commits format (see below).

## Build and Development Commands

### Quick Start with Docker Compose

```bash
# Start all services (PostgreSQL + lightstack)
docker compose -f deployments/docker-compose.yml up -d

# Configure environment (OS_AUTH_URL, OS_USERNAME, etc.)
source ~/.lightstack-env

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
# Unit tests (excludes test/contract — use 'make test-contract' for those)
make test

# Test a specific package
go test ./internal/nova/...

# DB-backed unit tests: set LIGHTSTACK_TEST_DB_URL to an existing scratch
# database the test may MIGRATE (the harness connects and migrates; it does
# not create the database — see internal/common/maindb_test.go; e.g. from
# 'make db-up', then create lightstack_test once). When unset, DB-backed
# tests skip instead of failing.
LIGHTSTACK_TEST_DB_URL=postgres://lightstack:secret@localhost:5432/lightstack_test make test

# Contract tests (gophercloud; requires a running Lightstack — the target
# refuses to start otherwise)
make test-contract

# Integration tests (bash suites driving the OpenStack CLI; requires a
# running Lightstack)
make test-integration

# Helm chart tests (lint + template + unittest; no cluster, no running
# Lightstack; requires helm + the helm-unittest plugin — 'make install-tools')
make chart-test
```

### Database Operations

```bash
# Start PostgreSQL (Docker)
make db-up                    # Starts a postgres container (POSTGRES_VERSION, default 18)

# Run migrations
make migrate                  # Applies all pending migrations via golang-migrate

# Stop PostgreSQL
make db-down                  # Stops and removes container
```

### Code Quality

```bash
# Format code
make fmt                      # Runs go fmt

# Lint code
make lint                     # Runs pinned golangci-lint (built from the tools/ module)

# Install development tools
make install-tools            # Installs air, golangci-lint, migrate, helm, helm-unittest
```

## Architecture Overview

### Service Structure

Lightstack runs as a single process (`cmd/lightstack/main.go`) that starts seven HTTP listeners on different ports:
- **Keystone** (35357): Identity service, JWT-based authentication
- **Nova** (8774): Compute service, VM lifecycle via libvirt
- **Neutron** (9696): Network service, namespace isolation via netlink
- **Cinder** (8776): Block storage, multi-backend support (local/RBD)
- **Glance** (9292): Image service, multi-backend with hybrid failover
- **Placement** (8778): Stub of the placement service
- **Metadata** (8775): EC2-compatible metadata service (no auth)

Each service is initialized in `main.go` with its configuration and shares:
- **Database connection pool**: Centralized PostgreSQL access
- **Auth middleware**: Shared JWT token validation via `keystone.AuthService`
- **Logging middleware**: Structured JSON logging

The binary also dispatches subcommands: `lightstack cleanup` (SPEC-010 teardown), `lightstack healthcheck` (node health probes), and `lightstack console-tokens list|purge` (SPEC-021 noVNC token inspection). A separate `cmd/lightstack-migrate` binary applies migrations.

### Code Organization

```
cmd/lightstack/              # Main binary (server + cleanup/console-tokens/healthcheck subcommands)
cmd/lightstack-migrate/      # Standalone migration binary
internal/                    # Private packages
├── keystone/               # Identity - JWT auth, service catalog
├── nova/                   # Compute - VM lifecycle, flavors, keypairs
├── neutron/                # Network - networks, subnets, ports, security groups
├── cinder/                 # Block storage - volumes, snapshots
├── glance/                 # Images - metadata, multi-backend storage
├── placement/              # Placement stub
├── metadata/               # EC2 metadata service
├── database/               # DB models, transactions (WithTx), migrate
├── middleware/             # Auth, logging, CORS, recovery
├── common/                 # Config, errors, BaseURL, scheme, pagination, SQL builder
├── compute/                # Node registry for multi-node coordination
├── console/                # noVNC console tokens (SPEC-021)
├── health/                 # Health/readiness endpoints
└── teardown/               # SPEC-010 release teardown

pkg/                        # Public/reusable packages
├── hypervisor/             # libvirt abstraction (stub + real modes)
├── networking/             # netlink, VXLAN, security groups
├── storage/                # Storage backends (Ceph RBD, S3, local)
├── cache/                  # Caching
└── paths/                  # state_path resolution (node-local state root)

test/                       # Test suites
├── contract/               # gophercloud contract tests (per-service subdirs)
├── terraform/              # Terraform provider conformance
└── *.sh                    # Bash integration suites (OpenStack CLI + jq)
```

### Key Architectural Patterns

**Service Mode Pattern**: Each service (Nova, Neutron, Cinder, Glance) supports multiple modes:
- `stub`: Returns fake data, no external dependencies (default for development)
- `real`: Full implementation with external dependencies (libvirt, Ceph, etc.)
- Mode is configured via YAML (`libvirt_mode`, `networking_mode`, `storage_mode`)
- Check mode via `libvirtMode` or `networkingMode` fields in service structs

**Multi-backend Storage Pattern** (Cinder/Glance):
- **Cinder**: `stub`, `local`, `rbd`, `local,rbd` only. S3 is **not** a Cinder backend.
- **Glance**: `stub`, `local`, `rbd`, `s3`, `local,rbd`, `local,s3` only. `rbd,s3` is **rejected at startup** (Glance has no RBD backend, so the S3 replica would never be written).
- Storage modes are specified as comma-separated strings; the first backend is primary, subsequent ones are fallback
- Implemented in `pkg/storage/` with `ImageStore` and backend-specific implementations; see `docs/STORAGE_MODES.md` for the authoritative matrix

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
- XML templates in `pkg/hypervisor/`

**Neutron real mode** (`networking_mode: iptables` or `ebpf`):
- Requires Linux with network namespaces
- Uses `github.com/vishvananda/netlink` for namespace/bridge creation
- iptables via `github.com/coreos/go-iptables` for security groups

**Cinder/Glance real mode** (`storage_mode` varies by service):
- **Cinder** (`local`, `rbd`, or `local,rbd`):
  - `local`: Host filesystem storage (node-local, under the `state_path` root)
  - `rbd`: Ceph RBD via `github.com/ceph/go-ceph`
- **Glance** (`local`, `rbd`, `s3`, `local,rbd`, or `local,s3`):
  - `local`: Host filesystem storage (node-local, under the `state_path` root)
  - `rbd`: Ceph RBD via `github.com/ceph/go-ceph`
  - `s3`: AWS S3/MinIO/Ceph RGW via `github.com/aws/aws-sdk-go-v2` (Glance only; `rbd,s3` is rejected at startup)

## Database Schema

PostgreSQL, 70+ tables. Key tables:

- **users**: User credentials (bcrypt hashed passwords)
- **projects**, **domains**, **roles**, **role_assignments**: Identity and RBAC
- **instances**: VM instances (linked to flavors, images)
- **flavors**: VM flavor definitions
- **networks**, **subnets**, **ports**, **security_groups**: Network topology
- **volumes**, **snapshots**: Block storage
- **images**: Image metadata (data in storage backend)
- **keypairs**: SSH public keys
- **compute_nodes**: Multi-node registry for VXLAN coordination

Migrations live in `migrations/` as numbered `NNN_name.up.sql` / `NNN_name.down.sql` pairs, applied via `golang-migrate/migrate`. Never edit an applied migration in a way that changes its meaning for deployed databases — add a new one.

## Testing Guidelines

### Unit Tests
- Located alongside source files as `*_test.go`
- DB-backed unit tests connect to the database in `LIGHTSTACK_TEST_DB_URL` (see `internal/common/maindb_test.go` for the TestMain pattern); they skip when it is unset
- Test mode detection logic (stub vs real)

### Contract Tests (`test/contract/`)
- Go tests using gophercloud (pinned v1.14.1) as a stand-in for any OpenStack client
- Require a running Lightstack (start the compose stack or `make run` first)
- **Every API-surface change needs a contract test here** — this is the SPEC-000 compliance gate CI runs

### Integration Tests
Bash scripts in `test/` that:
1. Start with a running Lightstack (compose stack or `make run`)
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

### Test-First Development
When implementing new features:
1. **Write tests first** - Create `*_test.go` files / contract tests
2. **Confirm RED** - Tests must fail initially
3. **Implement** - Write code to make tests pass (GREEN)
4. **Refactor** - Clean up while keeping tests green

## Configuration

Configuration loaded from `config/lightstack.yaml` (or via `--config` flag).

**Critical settings:**
- `database.url`: PostgreSQL connection string
- `keystone.jwt_secret`: MUST change in production (security critical)
- `keystone.admin_password`: overrides the seeded `admin`/`secret` default — set it for any exposed deployment
- `nova.libvirt_mode`: `stub` (dev) or `real` (prod)
- `neutron.networking_mode`: `stub`, `iptables`, or `ebpf`
- `cinder.storage_mode`: `stub`, `local`, `rbd`, or `local,rbd` (Cinder has no S3 backend)
- `glance.storage_mode`: `stub`, `local`, `rbd`, `s3`, `local,rbd`, or `local,s3` (`rbd,s3` is rejected at startup)
- `state_path`: node-local state root (default `/var/lib/lightstack`; resolved via `pkg/paths`)

**Dead config — do not rely on it**: the `rate_limiting.*` block (`enabled`, `auth_rate`, `api_rate`, …) appears in `config/lightstack.yaml` and in ADR-002's design, but **no code reads it** — there is no `RateLimiting` field in the `Config` struct (`internal/common/config.go`) and no rate-limiting middleware. Treat it as unimplemented until a spec lands it.

**Multi-node VXLAN** (advanced):
- Set `neutron.vxlan_enabled: true`
- Configure `compute.node_id` and `compute.tunnel_ip`
- Enables cross-node VM networking via VXLAN overlay

### Default Credentials
Seed data creates:
- **User**: `admin`
- **Password**: `secret`
- **Project**: `default`

*Change these in production environments.* (Setting `keystone.admin_password` neutralizes the seeded default.)

## Common Patterns and Idioms

### Commit Messages
Follow conventional commits format:
```
<type>(<scope>): <description>

[optional body]

Signed-off-by: Name <name@sap.com>
```

**Types**: `feat`, `fix`, `refactor`, `docs`, `test`, `chore`

**DCO**: every commit must carry the `Signed-off-by:` trailer (`git commit -s`); CI's "SAP Sign-off Check" job enforces an `@sap.com` address on every PR commit.

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
// Always use context, the service's injected pool, and parameterized statements
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
- Never commit production JWT secrets or any credentials
- Database passwords should use environment variables in production
- Token TTL default is 24h (configurable)
- Tenant isolation and admin-only surfaces are security-critical — see SPEC-022 (`docs/specs/022-project-tenant-isolation/`) and SPEC-023 (`docs/specs/023-tenant-scoping-authz/`)

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

## Where to Look Next

- `README.md` / `STATUS.md` / `ROADMAP.md` — project status and direction
- `docs/CONTRIBUTING.md` — contribution process
- `docs/specs/README.md` and `docs/specs/NNN-*/` — feature specifications
- `docs/adr/` — architecture decision records
- `docs/review/` — comprehensive review reports
- `CHANGELOG.md` — user-visible changes per release
