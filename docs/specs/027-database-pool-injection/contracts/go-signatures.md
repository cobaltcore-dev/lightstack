# Contract: Go signatures (before → after)

**Feature**: `027-database-pool-injection` | **Date**: 2026-10-01

The **HTTP API does not change.** Status codes, headers and bodies stay the same (FR-005), and the contract suite verifies it (SC-003). This file pins the one interface that does change: the internal Go API between the process entrypoint, the services and their helpers. Implementation and review both work against this list.

`pool` is always `*pgxpool.Pool`. Every parameter not shown is unchanged.

## `internal/database`

```go
// deleted
var DB *pgxpool.Pool
func Close()
func Stats() *pgxpool.Stat
func ConnectSimple(ctx context.Context, connString string, maxConns int) error

// changed
func Connect(ctx context.Context, connString string, poolConfig *PoolConfig) (*pgxpool.Pool, error) // was: error, and it set DB
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error           // was: WithTx(ctx, fn)
func HealthCheck(ctx context.Context, pool *pgxpool.Pool) error                                // was: HealthCheck(ctx); the nil guard and its message are kept

// unchanged: PoolConfig, DefaultPoolConfig, CreateMigrator, GetMigrationStatus, MigrateUp,
// MigrateDown, MigrateToVersion, MigrateReset, ForceMigrationVersion, ValidateMigrations
```

## Constructors

```go
func keystone.NewAuthService(pool, jwtSecret string, tokenTTL time.Duration, cacheInstance *cache.Cache) *AuthService
func keystone.NewService(pool, authService *AuthService, cacheInstance *cache.Cache) *Service
func nova.NewService(pool, libvirtURI, libvirtMode string, cacheInstance *cache.Cache) *Service
func neutron.NewService(pool, mode string, cacheInstance *cache.Cache) *Service
func cinder.NewService(pool, mode, cephPool, cephConf string) *Service
func glance.NewService(pool, mode, cephPool, cephConf string, s3opts storage.S3Options, cacheInstance *cache.Cache) (*Service, error)
func metadata.NewService(pool, bindAddr string) *Service
func compute.NewNodeRegistry(pool, nodeID, tunnelIP string, heartbeatInterval time.Duration) (*NodeRegistry, error)
func neutron.NewVXLANCoordinator(pool, vxlanManager *networking.VXLANManager, nodeRegistry *compute.NodeRegistry,
	nsManager *networking.NetworkNamespaceManager, pollInterval time.Duration, vniRangeStart, vniRangeEnd int) *VXLANCoordinator
func health.DatabaseCheck(pool) Check

// unchanged
func neutron.NewNetworkReconciler(svc *Service, pollInterval time.Duration) *NetworkReconciler // reads r.svc.pool
func placement.NewService() *Service                                                            // no database access
```

## Exported helpers

```go
func common.ResolveProjectRef(ctx context.Context, pool, ref string) (string, *OpenStackError)
func common.QuotaTargetProject(c *gin.Context, pool) (string, *OpenStackError)
func keystone.BuildServiceCatalog(pool, projectID string, cacheInstance *cache.Cache, s3Endpoint string) []CatalogEntry
func keystone.BootstrapAdmin(ctx context.Context, pool, cfg common.KeystoneConfig) error
func neutron.EnsureDefaultSecurityGroup(ctx context.Context, pool, projectID string) (string, error) // via WithTx
func nova.CheckQuota(c *gin.Context, pool, resource string, requestedAmount int) error                // via WithTx
```

## Package-private helpers

```go
// cinder
func cinderQuotaUsage(ctx context.Context, pool, projectID string) (map[string]int, error)
func resolveCinderQuotaLimits(ctx context.Context, pool, projectID string) (map[string]int, error)
// keystone
func createAdmin(ctx context.Context, pool, name, hash string) error
func grantAdminRole(ctx context.Context, pool, userID string) error
func seedCustomAdmin(ctx context.Context, pool, adminUser, password, hash string) error
func seedDefaultAdmin(ctx context.Context, pool, password, hash string) error
// neutron
func listSecurityGroupRows(ctx context.Context, pool, projectID string) ([]securityGroupRow, error)
func securityGroupOwnedByCaller(ctx context.Context, pool, sgID, projectID string) (bool, error)
func securityGroupRulesByGroup(ctx context.Context, pool, sgIDs []string) (map[string][]gin.H, error)
func queryStrings(ctx context.Context, pool, query string) ([]string, error)
// nova
func securityGroupNamesForServer(ctx context.Context, pool, serverID string) ([]string, error)
func securityGroupNamesForServers(ctx context.Context, pool, serverIDs []string) (map[string][]string, error)
func allocateNextIP(ctx context.Context, pool, networkID, cidr string) (string, error)
func countUsage(ctx context.Context, pool, resource, query, arg string) (int, error)
func resolveQuotaLimits(ctx context.Context, pool, projectID string) (map[string]int, map[string]struct{}, error)
// teardown
func enumerateDomainIDs(ctx context.Context, pool) ([]string, error)
```

## Invariants checked at review

1. `git grep -n 'database\.DB\b' -- '*.go'` is empty (FR-001 / SC-001).
2. In every database-using constructor above, `pool` is the first parameter (FR-002).
3. No function obtains a pool any way other than a parameter or its receiver's field (FR-003 / SC-002).
4. Only owners call `pool.Close()` (data-model.md, Rules).
5. The owners pass the real pool to every holder. No database-using construction in `cmd/lightstack/main.go` or `internal/teardown/teardown.go` receives `nil`. A `nil` there compiles, and it would only fail at runtime, on a real-mode-only path.
