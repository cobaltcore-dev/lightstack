# Contract: the `internal/database` Go API

Every service, background worker, helper and database-backed test reaches the database through these types (FR-006, FR-007). No pgx type appears outside `internal/database/postgres*.go`, except in five PostgreSQL-inspecting tests. Method sets mirror pgx's, so a call site changes its **types**, not its calls.

## Types

```go
package database

type Backend string

const (
	Postgres Backend = "postgres"
	SQLite   Backend = "sqlite"
)

// Querier is what DB and Tx share.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Exec(ctx context.Context, sql string, args ...any) (Result, error)
}

// DB is the process's one database, created once by its entry point and lent to every
// component (ADR-001, amended by ADR-003).
type DB interface {
	Querier
	Begin(ctx context.Context) (Tx, error)
	Ping(ctx context.Context) error
	Close()
	Backend() Backend
}

type Tx interface {
	Querier
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

type Row interface{ Scan(dest ...any) error }

type Result interface{ RowsAffected() int64 }

// ErrNoRows is returned by Row.Scan on both backends when the query matched nothing.
var ErrNoRows = errors.New("no rows in result set")
```

## Functions

```go
// URL handling — the checks of contracts/configuration-and-startup.md §1–2.
func ParseURL(raw string) (Backend, error)
func CheckDeclaredBackend(declared string, b Backend) error // declared = os.Getenv("LIGHTSTACK_DB_BACKEND")
func RedactURL(raw string) string                          // for messages and logs

type Options struct {
	Pool      PoolConfig // today's PoolConfig; HealthCheckPeriod is PostgreSQL-only
	MustExist bool       // SQLite: never create a missing file (cleanup, console-tokens)
	TxLock    string     // SQLite: how transactions begin, "immediate" (the default, decided by T080/T081) or "deferred"; for the concurrency tests
}

// Open validates the URL again, then opens the backend it selects. One call per process.
func Open(ctx context.Context, url string, opts Options) (DB, error)

// WithTx keeps its shape; only the types change.
func WithTx(ctx context.Context, db DB, fn func(tx Tx) error) error

func HealthCheck(ctx context.Context, db DB) error // Ping, for /readyz

// Describe returns the fields of the startup log line (FR-013a).
func Describe(ctx context.Context, db DB, url string) (Description, error)

// Error classes, true for either backend's error (wrapped errors included).
func IsUniqueViolation(err error) bool     // 23505 | SQLITE_CONSTRAINT_UNIQUE, _PRIMARYKEY
func IsForeignKeyViolation(err error) bool // 23503 | SQLITE_CONSTRAINT_FOREIGNKEY
func IsNotNullViolation(err error) bool    // 23502 | SQLITE_CONSTRAINT_NOTNULL
func IsCheckViolation(err error) bool      // 23514 | SQLITE_CONSTRAINT_CHECK
func IsBusy(err error) bool                // 40001, 40P01, 55P03 | SQLITE_BUSY*, SQLITE_LOCKED*
```

## Migrations

```go
// Each opens its own connection for the run and selects the backend from dbURL.
func MigrateUp(dbURL, migrationsPath string) error
func MigrateDown(dbURL, migrationsPath string) error
func MigrateToVersion(dbURL, migrationsPath string, v uint) error   // SQLite: v below the baseline refused
func MigrateReset(dbURL, migrationsPath string) error
func ForceMigrationVersion(dbURL, migrationsPath string, v int) error // SQLite: v below the baseline refused
func GetMigrationStatus(dbURL, migrationsPath string) (*MigrationInfo, error)
func ValidateMigrations(migrationsPath string) error // validates the PostgreSQL view and the SQLite view
```

## Call-site changes (before → after)

| Before | After | Count (production) |
|---|---|---|
| `*pgxpool.Pool` in fields, constructors, helpers | `database.DB` — identifiers such as `pool` and `svc.pool` keep their names | 61 in 28 files |
| `pgx.Tx` in closures and helpers | `database.Tx` | 29 in 10 files |
| `pgx.ErrNoRows` | `database.ErrNoRows` | 165 |
| `common.IsUniqueViolation`, inline `*pgconn.PgError` 23505 check | `database.IsUniqueViolation` (`internal/common/pgerror.go` deleted, no alias) | 3 callers + 1 inline |
| `pgx.CollectRows(rows, pgx.RowTo[string])` | scan loop | 1 |
| `svc.pool.Begin(ctx)` → `pgx.Tx` | `svc.pool.Begin(ctx)` → `database.Tx` | 1 (`DeleteServer`) |
| `database.Connect(ctx, url, cfg)` | `database.Open(ctx, url, database.Options{Pool: *cfg})` | 3 production, 10 tests + benchmark |
| `testPool.Stat().AcquiredConns()` | removed; the test asserts through the behaviour it guards | 1 test |

No transitional aliases (`Connect`, `IsUniqueViolation` in `common`, re-exported pgx names): the old names are deleted, and the compiler finds every caller.
