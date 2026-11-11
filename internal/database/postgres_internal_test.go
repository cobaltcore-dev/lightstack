package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPostgresPoolSettingsReachPgx pins FR-003: the pool settings from the
// configuration reach pgx's pool exactly as before, and both PostgreSQL URL schemes
// open the same backend.
func TestPostgresPoolSettingsReachPgx(t *testing.T) {
	url := os.Getenv("LIGHTSTACK_TEST_DB_URL")
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		t.Skip("set LIGHTSTACK_TEST_DB_URL to a postgres:// URL to run the PostgreSQL contract tests")
	}
	want := PoolConfig{
		MaxConns:          7,
		MinConns:          2,
		MaxConnLifetime:   42 * time.Minute,
		MaxConnIdleTime:   3 * time.Minute,
		HealthCheckPeriod: 17 * time.Second,
	}
	for _, scheme := range []string{"postgres://", "postgresql://"} {
		t.Run(scheme, func(t *testing.T) {
			u := scheme + url[strings.Index(url, "://")+3:]
			db, err := Open(context.Background(), u, Options{Pool: want})
			if err != nil {
				t.Fatalf("open %s: %v", scheme, err)
			}
			defer db.Close()
			if db.Backend() != Postgres {
				t.Fatalf("Backend = %q; want %q", db.Backend(), Postgres)
			}
			cfg := db.(*pgDB).pool.Config()
			got := PoolConfig{
				MaxConns:          cfg.MaxConns,
				MinConns:          cfg.MinConns,
				MaxConnLifetime:   cfg.MaxConnLifetime,
				MaxConnIdleTime:   cfg.MaxConnIdleTime,
				HealthCheckPeriod: cfg.HealthCheckPeriod,
			}
			if got != want {
				t.Fatalf("pgxpool config = %+v; want %+v", got, want)
			}
		})
	}
}
