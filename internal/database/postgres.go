package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The PostgreSQL implementation is a thin wrapper over pgx's pool. Statements pass
// through untouched; the only translation is "no rows", which pgx reports with its own
// sentinel. This is the only file outside tests that imports pgx.

func openPostgres(ctx context.Context, connString string, pc PoolConfig) (DB, error) {
	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse database URL: %w", err)
	}

	config.MaxConns = pc.MaxConns
	config.MinConns = pc.MinConns
	config.MaxConnLifetime = pc.MaxConnLifetime
	config.MaxConnIdleTime = pc.MaxConnIdleTime
	config.HealthCheckPeriod = pc.HealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Test connection
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	return &pgDB{pool: pool}, nil
}

type pgDB struct{ pool *pgxpool.Pool }

func (d *pgDB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := d.pool.Query(ctx, sql, args...)
	// pgx returns usable (erroring) rows together with the error; keep that shape so
	// a caller that defers Close before checking err does not dereference nil.
	return pgRows{rows}, err
}

func (d *pgDB) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return pgRow{d.pool.QueryRow(ctx, sql, args...)}
}

func (d *pgDB) Exec(ctx context.Context, sql string, args ...any) (Result, error) {
	tag, err := d.pool.Exec(ctx, sql, args...)
	return pgResult{tag}, err
}

func (d *pgDB) Begin(ctx context.Context) (Tx, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return pgTx{tx}, nil
}

func (d *pgDB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }
func (d *pgDB) Close()                         { d.pool.Close() }

// InUse is the number of connections checked out of the pool, for the test harness's
// connection-leak check (dbtest.Close). A handler that leaked a Rows holds one.
func (d *pgDB) InUse() int       { return int(d.pool.Stat().AcquiredConns()) }
func (d *pgDB) Backend() Backend { return Postgres }

type pgTx struct{ tx pgx.Tx }

func (t pgTx) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	return pgRows{rows}, err
}

func (t pgTx) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return pgRow{t.tx.QueryRow(ctx, sql, args...)}
}

func (t pgTx) Exec(ctx context.Context, sql string, args ...any) (Result, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	return pgResult{tag}, err
}

func (t pgTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t pgTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

type pgRows struct{ rows pgx.Rows }

func (r pgRows) Next() bool             { return r.rows.Next() }
func (r pgRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r pgRows) Err() error             { return r.rows.Err() }
func (r pgRows) Close()                 { r.rows.Close() }

type pgRow struct{ row pgx.Row }

func (r pgRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoRows
	}
	return err
}

type pgResult struct{ tag pgconn.CommandTag }

func (r pgResult) RowsAffected() int64 { return r.tag.RowsAffected() }

// pgCode returns err's SQLSTATE when it is (or wraps) a PostgreSQL error.
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
