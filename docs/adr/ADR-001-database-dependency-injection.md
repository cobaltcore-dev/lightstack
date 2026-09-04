# ADR-001: Database Dependency Injection

**Status**: Accepted
**Date**: 2026-04-20
**Deciders**: Lightstack Core Team
**Spec**: [SPEC-006](../specs/006-v070-foundation/README.md)

## Context

Lightstack uses a global mutable database pool (`var DB *pgxpool.Pool` in `internal/database/db.go:11`) accessed by 661 references across 53 files. This pattern:

- Makes unit testing without a running PostgreSQL instance impossible
- Couples all service packages to the `database` package
- Prevents future service isolation (required for modular architecture per SPEC-001)
- Was identified as finding H-3 (High severity) in the April 2026 codebase review

The project constitution (Article IX: Integration-First) prefers real database integration tests over mocks, but the current global prevents even basic constructor-level testing.

## Decision

**Direct `*pgxpool.Pool` injection through service constructors.**

Each service struct gains a `pool *pgxpool.Pool` field. Handler methods use `svc.pool.Query()` instead of `database.DB.Query()`. All services share the same pool instance (DI is for architecture and testability, not for isolation).

### Migration Strategy

1. **Order**: metadata (8 refs) → compute (3 refs) → glance (53 refs) → keystone (85 refs) → cinder (116 refs) → neutron (184 refs) → nova (212 refs)
2. **Transition**: Global `database.DB` stays alive during migration. `main.go` passes the same pool value to each `NewService()` constructor.
3. **Completion**: After all 661 references are migrated, remove `var DB *pgxpool.Pool`. The compiler catches any missed references.
4. **WithTx**: The `database.WithTx()` helper (`internal/database/tx.go`) gains a pool parameter: `WithTx(ctx, pool, fn)`.
5. **Lint**: golangci-lint `unused` rule disabled during migration to avoid false positives on partially-wired parameters.

### Constructor Pattern (After)

```go
type Service struct {
    pool        *pgxpool.Pool
    libvirtMode string
    // ... other fields
}

func NewService(pool *pgxpool.Pool, libvirtURI, mode string, cache *cache.Cache) *Service {
    return &Service{pool: pool, libvirtURI: libvirtURI, ...}
}

func (svc *Service) ListServers(c *gin.Context) {
    rows, err := svc.pool.Query(c.Request.Context(), ...)
}
```

## Alternatives Considered

### Interface-based injection
Define a `DB` interface with `QueryRow`, `Query`, `Exec`, `Begin` methods. Pass the interface instead of the concrete pool.

**Rejected because**: Premature abstraction (Constitution Article VIII: Anti-Abstraction). The interface adds boilerplate to all 661 call sites without immediate benefit. Can be extracted later if mock-based testing becomes a priority.

### Per-service connection pools
Each service gets its own pool with independent connection limits.

**Rejected because**: Unnecessary complexity. Lightstack is a single-binary deployment where all services share the same PostgreSQL instance. Per-service pools add configuration burden and connection overhead without isolation benefit.

### Repository pattern
Wrap all database access in repository structs with typed methods.

**Rejected because**: Over-engineering for the current scale. The handlers directly construct SQL queries with pgx, which is idiomatic Go. A repository layer would duplicate every query method without adding value.

## Consequences

### Positive
- Unit tests can pass a test pool (or in-memory DB) to service constructors
- Service packages no longer import `database` package for the global
- Constructor signatures make dependencies explicit
- Prepares codebase for modular architecture (SPEC-001)

### Negative
- 661 mechanical changes across 53 files (high blast radius)
- Every `NewService()` call in `cmd/lightstack/main.go` gains a parameter
- Must coordinate with lint config (staged `unused` disable)
- `database.DB` global temporarily coexists with injected pools during migration

### Neutral
- No performance impact (same pool, different access path)
- No API changes (internal refactoring only)
- No database schema changes
