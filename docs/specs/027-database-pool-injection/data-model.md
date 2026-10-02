# Data Model: Inject the database pool through service constructors (ADR-001)

**Feature**: `027-database-pool-injection` | **Date**: 2026-10-01

No persisted data changes: there is no schema change, no migration and no new table or column. The "model" here is who owns the one database pool, and who holds a reference to it.

## Pool

| Attribute | Value |
|---|---|
| Type | `*pgxpool.Pool` (concrete; no interface — FR-007) |
| Created by | `database.Connect(ctx, url, cfg)`, which parses, configures, pings and returns the pool |
| Configuration | `database.PoolConfig` from `lightstack.yaml`, unchanged (FR-004) |
| Count per process | exactly one |
| Closed by | its owner, once, via `pool.Close()` |

### Owners (create and close)

| Owner | Where | Lifetime |
|---|---|---|
| The server process | `cmd/lightstack/main.go` | From startup to `main` returning, after every listener's `Shutdown` (`defer pool.Close()`) |
| The `cleanup` job | `internal/teardown/teardown.go` `Run` | From `Connect` to `Run` returning. Absent when the database is unreachable, which selects the host-only path |
| A test package | the `TestMain` of cinder, glance, keystone, metadata, neutron and nova | The test binary's run, and only when `LIGHTSTACK_TEST_DB_URL` is set |
| The Keystone demonstrator | `internal/keystone/pool_injection_test.go` | One test (`t.Cleanup`) |
| A benchmark | `test/benchmark/database_bench_test.go` | One benchmark function |

### Holders (receive and never close)

| Holder | Field | Set by | References converted |
|---|---|---|---|
| `nova.Service` | `pool` | `nova.NewService(pool, …)` | 209 + 5 in helpers |
| `neutron.Service` | `pool` | `neutron.NewService(pool, …)` | 174 + 4 in helpers |
| `neutron.NetworkReconciler` | — (reads `r.svc.pool`) | its `svc` | 4 |
| `neutron.VXLANCoordinator` | `pool` | `neutron.NewVXLANCoordinator(pool, …)` | 6 |
| `cinder.Service` | `pool` | `cinder.NewService(pool, …)` | 111 + 4 in helpers |
| `keystone.Service` | `pool` | `keystone.NewService(pool, …)` | 80 |
| `keystone.AuthService` | `pool` | `keystone.NewAuthService(pool, …)` | 16 + 1 in `BuildServiceCatalog` |
| `glance.Service` | `pool` | `glance.NewService(pool, …)` | 54 |
| `metadata.Service` | `pool` | `metadata.NewService(pool, …)` | 10 |
| `compute.NodeRegistry` | `pool` | `compute.NewNodeRegistry(pool, …)` | 3 |
| `health.Check` (database) | captured in `Probe` | `health.DatabaseCheck(pool)` | — (used `database.HealthCheck`) |

The remaining production references are in parameters rather than fields. `keystone.BootstrapAdmin` and its four helpers hold 7, `teardown.enumerateDomainIDs` 1, and `common.ResolveProjectRef` 1.

Total: 667 in methods + 23 in package functions = **690** (research R12).

### Rules

- A holder stores the pool it was given and never replaces it.
- A holder never closes the pool. Only an owner does.
- A nil pool is allowed for database-free code paths (stub-mode unit tests). A path that queries through a nil pool panics, as the nil handle does today. It never fails silently.
- In production every holder has the same pool (FR-004). Cross-service calls — Nova→Neutron port allocation, Nova→Glance image store — use the callee's own pool, which is that same pool.
- Placement holds no pool, because it has no database access.

## Lifecycle

```
owner: database.Connect ──► pool ──► constructors (holders) ──► requests / background work
                                                        │
owner: shutdown (drain → stop goroutines → srv.Shutdown) ──► pool.Close()   [once]
```

The `cleanup` job follows the same shape inside `teardown.Run`. When `Connect` fails, the job has no pool and no holders, and it takes the host-only path, as today.
