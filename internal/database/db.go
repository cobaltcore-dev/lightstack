package database

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Backend identifies the database a process runs on. It is chosen once, at startup,
// from the database URL's scheme (ParseURL), and never changes for the life of the
// process.
type Backend string

const (
	// Postgres is the default backend: pgx and its connection pool, statements passed
	// through unchanged.
	Postgres Backend = "postgres"
	// SQLite keeps all state in one file on the node, for single-node deployments.
	SQLite Backend = "sqlite"
)

// Querier is what DB and Tx have in common: the three ways to run a statement.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Exec(ctx context.Context, sql string, args ...any) (Result, error)
}

// DB is the process's one database, created once by its entry point and lent to every
// component that needs it (ADR-001, amended by ADR-003). Its method set mirrors pgx's
// pool, so a call site changes its types, not its calls.
type DB interface {
	Querier
	Begin(ctx context.Context) (Tx, error)
	Ping(ctx context.Context) error
	Close()
	Backend() Backend
}

// Tx is a transaction begun on a DB.
type Tx interface {
	Querier
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// Rows iterates over a query's result. Close is safe to call more than once and after
// Next has returned false.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// Row is the result of QueryRow. Scan reports ErrNoRows when the query matched nothing.
type Row interface {
	Scan(dest ...any) error
}

// Result reports what an Exec changed.
type Result interface {
	RowsAffected() int64
}

// ErrNoRows is returned by Row.Scan, on every backend, when the query matched nothing.
// Compare with errors.Is: callers commonly wrap it.
var ErrNoRows = errors.New("no rows in result set")

// PoolConfig contains database connection pool configuration
type PoolConfig struct {
	MaxConns          int32         // Maximum number of connections in the pool
	MinConns          int32         // Minimum number of idle connections
	MaxConnLifetime   time.Duration // Maximum lifetime of a connection
	MaxConnIdleTime   time.Duration // Maximum idle time before closing connection
	HealthCheckPeriod time.Duration // How often to check connection health (PostgreSQL only)
}

// DefaultPoolConfig returns sensible defaults for connection pooling
func DefaultPoolConfig() *PoolConfig {
	return &PoolConfig{
		MaxConns:          20,               // Default from config
		MinConns:          2,                // Keep 2 connections warm
		MaxConnLifetime:   1 * time.Hour,    // Recycle connections hourly
		MaxConnIdleTime:   15 * time.Minute, // Close idle connections after 15 min
		HealthCheckPeriod: 1 * time.Minute,  // Check health every minute
	}
}

// Options configures Open.
type Options struct {
	// Pool sizes the connection pool. The zero value means DefaultPoolConfig.
	Pool PoolConfig
	// MustExist makes Open fail, rather than create, when a SQLite database file is
	// missing. Entry points that only read or purge an existing installation set it.
	MustExist bool
	// TxLock chooses how SQLite transactions begin: "immediate" (the default) or
	// "deferred". Only the concurrency tests that compare the two set it.
	TxLock string
}

// Open validates the URL, then opens the backend it selects. A process opens its
// database once and closes it when it exits.
func Open(ctx context.Context, rawURL string, opts Options) (DB, error) {
	backend, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	pool := opts.Pool
	if pool == (PoolConfig{}) {
		pool = *DefaultPoolConfig()
	}
	switch backend {
	case Postgres:
		return openPostgres(ctx, rawURL, pool)
	case SQLite:
		return openSQLite(ctx, rawURL, opts, pool)
	}
	return nil, fmt.Errorf("database backend %q is not supported", backend)
}

// HealthCheck verifies database connectivity through db.
func HealthCheck(ctx context.Context, db DB) error {
	if db == nil {
		return fmt.Errorf("database connection not initialized")
	}
	return db.Ping(ctx)
}
