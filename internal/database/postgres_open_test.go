package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// TestOpenPostgresUnreachable: a well-formed postgres URL whose server cannot be reached
// fails the open at the Ping, rather than returning a pool that errors on first use. No
// server is required — the port is closed.
func TestOpenPostgresUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := database.Open(ctx, "postgres://lightstack:secret@127.0.0.1:1/lightstack?sslmode=disable",
		database.Options{Pool: *database.DefaultPoolConfig()})
	if err == nil {
		t.Fatal("open of an unreachable PostgreSQL succeeded; want a ping failure")
	}
}
