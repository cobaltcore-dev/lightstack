package database

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Test helpers for the PostgreSQL-backed schema tests: the admin URL and scratch databases.
// The introspection itself lives in introspect_postgres.go.

// adminPostgresURL returns the PostgreSQL test database URL, or skips.
func adminPostgresURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("LIGHTSTACK_TEST_DB_URL")
	if !strings.HasPrefix(u, "postgres://") && !strings.HasPrefix(u, "postgresql://") {
		t.Skip("needs LIGHTSTACK_TEST_DB_URL set to a postgres:// URL")
	}
	return u
}

// scratchPostgres creates an empty database next to the test database, returns its URL
// and drops it when the test ends.
func scratchPostgres(t *testing.T, adminURL, name string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		t.Fatalf("drop scratch database: %v", err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
