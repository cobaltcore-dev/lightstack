package nova

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDBAvailable is true when LIGHTSTACK_TEST_DB_URL is set and the schema migrated;
// DB-backed nova tests skip otherwise (Constitution: real dependencies over mocks).
//
// Nova had no DB test harness before this feature (only boot_from_volume_test.go,
// a pure unit test). This mirrors internal/keystone/maindb_test.go so nova handler
// tests can run against a real Postgres. See docs/specs/017-nova-server-detail-readback
// (research.md D9): CI's unit-tests job must export LIGHTSTACK_TEST_DB_URL or these skip.
var testDBAvailable bool

// testPool is this package's test pool: opened by TestMain when LIGHTSTACK_TEST_DB_URL
// is set, and handed to the services the tests construct (SPEC-027).
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	// Skip DB-backed tests only when no test DB is configured. When LIGHTSTACK_TEST_DB_URL
	// IS set, a connect/migrate failure is fatal — never a silent skip — so a broken
	// Postgres in CI fails the job instead of quietly hiding the DB-backed tests.
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
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run DB-backed nova tests")
	}
}
