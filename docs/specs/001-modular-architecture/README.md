# SPEC-001: Modular Architecture Transformation

**Status**: Draft
**Version**: 1.0
**Created**: 2026-03-09
**Priority**: Critical
**Compliance**: Must pass [SPEC-000](../000-api-compliance/README.md) at every phase

## Overview

Transform Lightstack from a monolithic single-binary system into a truly modular OpenStack alternative where each service (Keystone, Nova, Neutron, Cinder, Glance, Barbican, Designate, Octavia) is independently deployable, developable, and testable.

**Critical Requirement**: Every milestone must maintain 100% OpenStack API compatibility per SPEC-000.

## Goals

1. **Modular Independence**: Each OpenStack service runs as separate binary/library
2. **Fail-Early Architecture**: Service failures propagate immediately (< 1s), no queue hiding
3. **100% API Compatibility**: Full SPEC-000 compliance at every phase (NON-NEGOTIABLE)
4. **Library-First**: Each service begins as standalone library (Constitution Article I)
5. **Observable State**: Real-time state visibility, no asynchronous queues
6. **Horizon Integration**: Native compatibility with Horizon 2025.2 dashboard

## Non-Goals

- Kubernetes/container orchestration (future phase)
- Multi-region support (future phase)
- High-availability clustering (future phase)
- Message queue integration (explicitly rejected per fail-early principle)

## Architecture Principles

### Fail-Early Strategy

```
Service Dependency Failure → Immediate Error (< 1s) → HTTP Error Response
NO: Service → Queue → Async Worker → Maybe Fail Later
YES: Service → External Dep (timeout 1s) → Fail Immediately
```

**Rationale**: Operators must know immediately when dependencies fail. Queuing operations hides failures and creates debugging nightmares.

### Service Independence

Each service must:
- Run as standalone binary with `--service <name>` flag
- Have independent configuration file
- Manage own database schema (migrations)
- Expose OpenStack-compatible REST API
- Support both stub and real modes
- Include CLI tool for operations

### Current State vs Target State

**Current (Monolithic)**:
```
cmd/lightstack/main.go → Single Process
  ├─ Keystone HTTP Server (port 35357)
  ├─ Nova HTTP Server (port 8774)
  ├─ Neutron HTTP Server (port 9696)
  ├─ Cinder HTTP Server (port 8776)
  ├─ Glance HTTP Server (port 9292)
  └─ Metadata HTTP Server (port 8775)

Shared: database.DB, middleware, config
```

**Target (Modular)**:
```
cmd/
├─ lightstack-keystone/main.go → Standalone binary
├─ lightstack-nova/main.go → Standalone binary
├─ lightstack-neutron/main.go → Standalone binary
├─ lightstack-cinder/main.go → Standalone binary
├─ lightstack-glance/main.go → Standalone binary
├─ lightstack-barbican/main.go → Standalone binary (NEW)
├─ lightstack-designate/main.go → Standalone binary (NEW)
├─ lightstack-octavia/main.go → Standalone binary (NEW)
└─ lightstack-metadata/main.go → Standalone binary

Each service:
- Independent config: /etc/lightstack/<service>.yaml
- Independent database schema
- Service-to-service communication via OpenStack APIs
- CLI tool: lightstack-<service>-cli
```

## Technical Design

### Service Communication

Services communicate via OpenStack APIs, not internal interfaces.

```go
// NO: Direct internal calls
novaService.neutronClient.CreatePort(...)

// YES: OpenStack API client
neutronClient := gophercloud.NewServiceClient(...)
neutronClient.Create(ports.CreateOpts{...})
```

**Benefits**:
- Enforces API contracts
- Services can be deployed separately
- Enables version negotiation
- Testable with real OpenStack clients

### Authentication Flow

```
1. Client → Keystone: POST /v3/auth/tokens (password)
2. Keystone → Client: JWT token + service catalog
3. Client → Nova: GET /v2.1/servers (X-Auth-Token: JWT)
4. Nova → Keystone: Validate token (internal fast path or API)
5. Nova → Client: Server list
```

**Token Validation Options**:
- **Option A (Fast)**: Shared JWT secret, local validation (current)
- **Option B (Secure)**: Keystone API call per request (network overhead)
- **Option C (Hybrid)**: Local JWT validation + periodic catalog refresh

Recommend **Option C** with 5-minute catalog cache.

### Database Strategy

**Option 1: Shared Database, Service Schemas**
```
PostgreSQL: lightstack
├─ keystone_users
├─ keystone_tokens
├─ nova_instances
├─ nova_flavors
├─ neutron_networks
├─ cinder_volumes
└─ glance_images
```

**Option 2: Database Per Service**
```
PostgreSQL:
├─ lightstack_keystone (users, tokens, projects)
├─ lightstack_nova (instances, flavors)
├─ lightstack_neutron (networks, ports)
├─ lightstack_cinder (volumes)
└─ lightstack_glance (images)
```

**Recommendation**: Option 1 initially (simpler migration), Option 2 for production scale.

### Configuration Management

Each service has independent config file:

```yaml
# /etc/lightstack/nova.yaml
service:
  name: nova
  bind: 0.0.0.0:8774
  mode: real  # or stub

database:
  url: postgresql://localhost/lightstack
  schema_prefix: nova_

keystone:
  url: http://localhost:35357
  jwt_secret: shared-secret-for-validation
  catalog_cache_ttl: 300

hypervisor:
  libvirt_uri: qemu:///system
  timeout: 1000  # milliseconds (fail-early)

logging:
  level: info
  format: json
```

### Library Structure

Each service becomes a library:

```
pkg/
├─ keystone/
│  ├─ service.go        # Service interface
│  ├─ auth.go           # Authentication
│  ├─ catalog.go        # Service catalog
│  └─ cli/              # CLI tool
├─ nova/
│  ├─ service.go
│  ├─ compute.go
│  ├─ flavors.go
│  └─ cli/
├─ neutron/
│  ├─ service.go
│  ├─ networks.go
│  ├─ ports.go
│  └─ cli/
... (other services)

cmd/
├─ lightstack-keystone/main.go    # Binary entry point
├─ lightstack-nova/main.go
└─ ... (other services)

internal/
└─ shared/
   ├─ database/         # DB utilities
   ├─ middleware/       # Auth, logging
   └─ config/           # Config loading
```

## Migration Path

### Phase 1: Library Extraction (Weeks 1-3)
1. Extract `internal/keystone` → `pkg/keystone` as library
2. Create `cmd/lightstack-keystone/main.go` standalone binary
3. Add CLI tool `cmd/lightstack-keystone-cli/main.go`
4. Write contract tests against OpenStack API
5. Validate Horizon compatibility

Repeat for Nova, Neutron, Cinder, Glance.

### Phase 2: Service Separation (Weeks 4-6)
1. Replace internal calls with OpenStack API clients
2. Separate configuration files per service
3. Independent database migrations
4. Docker Compose with separate containers
5. Integration testing with separate services

### Phase 3: New Services (Weeks 7-12)
1. Implement Barbican (key management)
2. Implement Designate (DNS)
3. Implement Octavia (load balancing)
4. Each follows library-first pattern

### Phase 4: Production Readiness (Weeks 13-16)
1. Database per service option
2. TLS/mTLS between services
3. Service discovery (etcd/consul)
4. Helm charts for Kubernetes
5. Performance testing at scale

## Testing Strategy

### Contract Tests (Constitution Article IX)

Each service must have contract tests:

```go
// test/contract/keystone_test.go
func TestKeystoneAuthAPI(t *testing.T) {
    // Use actual OpenStack client
    provider := openstack.NewClient("http://localhost:35357")

    // Test password auth
    token, err := tokens.Create(provider, authOpts)
    require.NoError(t, err)

    // Validate token structure
    assert.NotEmpty(t, token.ID)
    assert.Contains(t, token.Catalog, "nova")
}
```

### Integration Tests

Each service has integration tests using real dependencies:

```bash
# test/integration/nova_test.sh
#!/bin/bash
# Start real libvirt, real database
# Test full VM lifecycle
# Clean up resources
```

### Validation Gates

Before merging any phase:
1. **SPEC-000 Compliance Tests Pass** (100% - NON-NEGOTIABLE)
   - All contract tests pass (OpenStack clients)
   - Terraform provider tests pass
   - OpenStack CLI tests pass
   - SDK compatibility tests pass (Python, Go, Java)
   - Schema validation passes
   - Error response validation passes
2. All integration tests pass
3. Horizon dashboard workflow passes
4. No performance regression (< 5% overhead)
5. Zero API breaking changes

## Success Criteria

- [ ] Each service runs as independent binary
- [ ] Each service has CLI tool
- [ ] All services pass contract tests with openstack-client
- [ ] Horizon 2025.2 dashboard works with all services
- [ ] Fail-early behavior: dependency failures < 1s response
- [ ] No message queues or async state hiding
- [ ] Documentation updated for modular deployment
- [ ] Docker Compose reference deployment
- [ ] Migration guide from monolithic to modular

## Dependencies

- Gophercloud library for service-to-service communication
- OpenStack client CLI for validation
- Horizon 2025.2 dashboard for UI validation
- PostgreSQL for shared database
- Docker Compose for orchestration

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Performance overhead from HTTP calls | Medium | Use local JWT validation, cache service catalog |
| Increased deployment complexity | High | Provide Docker Compose and systemd templates |
| Service version skew | Medium | Require semantic versioning, API version negotiation |
| Database migration conflicts | Low | Use schema prefixes, independent migration paths |
| Breaking existing deployments | High | Maintain backward-compatible monolithic binary option |

## Open Questions

1. Should monolithic binary remain as deployment option?
   - **Recommendation**: Yes, via `cmd/lightstack-all/main.go` wrapper
2. Service discovery mechanism?
   - **Recommendation**: Static config initially, etcd/consul later
3. TLS between services?
   - **Recommendation**: Optional, document reverse proxy pattern

## References

- OpenStack API specifications
- Gophercloud documentation
- Constitution Article I (Library-First)
- Constitution Article III (Test-First)
- Constitution Article IX (Integration-First)
