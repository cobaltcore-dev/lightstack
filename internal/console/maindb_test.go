package console

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDBAvailable is true when LIGHTSTACK_TEST_DB_URL is set and the schema migrated.
// Article IV wants real databases over mocks, so the token store is exercised
// against a real Postgres; without one, the DB-backed tests skip (the pure
// projection and parsing tests still run).
//
// Mirrors internal/nova/maindb_test.go: when LIGHTSTACK_TEST_DB_URL IS set, a
// connect/migrate failure is fatal rather than a silent skip, so a broken
// Postgres in CI fails the job instead of quietly hiding coverage.
var testDBAvailable bool

// testPool is this package's test pool: opened by TestMain when LIGHTSTACK_TEST_DB_URL
// is set, and handed to the stores the tests construct (SPEC-027).
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if dbURL := os.Getenv("LIGHTSTACK_TEST_DB_URL"); dbURL != "" {
		ctx := context.Background()
		var err error
		if testPool, err = database.Connect(ctx, dbURL, &database.PoolConfig{
			MaxConns:          5,
			MinConns:          1,
			MaxConnLifetime:   time.Hour,
			MaxConnIdleTime:   15 * time.Minute,
			HealthCheckPeriod: time.Minute,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "LIGHTSTACK_TEST_DB_URL is set but connecting to it failed: %v\n", err)
			os.Exit(1)
		}
		if err := database.MigrateUp(dbURL, "../../migrations"); err != nil {
			fmt.Fprintf(os.Stderr, "LIGHTSTACK_TEST_DB_URL is set but migrations failed: %v\n", err)
			testPool.Close()
			os.Exit(1)
		}
		testDBAvailable = true
	}
	code := m.Run()
	if testDBAvailable {
		testPool.Close()
	}
	os.Exit(code)
}

func requireDB(t *testing.T) {
	t.Helper()
	if !testDBAvailable {
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run DB-backed console-token tests")
	}
}
