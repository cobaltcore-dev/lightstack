package keystone

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/database"
)

// testDBAvailable is true when O3K_TEST_DB_URL is set and the schema migrated;
// DB-backed tests skip otherwise (Constitution: real dependencies over mocks).
var testDBAvailable bool

func TestMain(m *testing.M) {
	// Skip DB-backed tests only when no test DB is configured. When
	// O3K_TEST_DB_URL IS set, a connect/migrate failure is fatal — never a
	// silent skip — so a broken Postgres in CI fails the job instead of
	// quietly hiding the DB-backed tests.
	if dbURL := os.Getenv("O3K_TEST_DB_URL"); dbURL != "" {
		ctx := context.Background()
		if err := database.Connect(ctx, dbURL, &database.PoolConfig{
			MaxConns:          5,
			MinConns:          1,
			MaxConnLifetime:   time.Hour,
			MaxConnIdleTime:   15 * time.Minute,
			HealthCheckPeriod: time.Minute,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "O3K_TEST_DB_URL is set but connecting to it failed: %v\n", err)
			os.Exit(1)
		}
		if err := database.MigrateUp(dbURL, "../../migrations"); err != nil {
			fmt.Fprintf(os.Stderr, "O3K_TEST_DB_URL is set but migrations failed: %v\n", err)
			database.Close()
			os.Exit(1)
		}
		testDBAvailable = true
	}
	code := m.Run()
	if testDBAvailable {
		database.Close()
	}
	os.Exit(code)
}

func requireDB(t *testing.T) {
	t.Helper()
	if !testDBAvailable {
		t.Skip("set O3K_TEST_DB_URL to run DB-backed keystone tests")
	}
}

// seededSecretHash is the bcrypt('secret') hash from migration 002_seed_data.
const seededSecretHash = "$2a$10$/DyBs44Yot8zgjC4j.n03Ox/QG94ftrksEXKKvadKcixLbGll0dR2"

// resetAdmin restores the seeded admin (enabled, bcrypt('secret')) and removes
// any test-created users so each bootstrap test starts from a known state.
func resetAdmin(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.DB.Exec(ctx,
		"DELETE FROM role_assignments WHERE user_id IN (SELECT id FROM users WHERE name LIKE 'spec007-%')"); err != nil {
		t.Fatalf("resetAdmin role cleanup: %v", err)
	}
	if _, err := database.DB.Exec(ctx, "DELETE FROM users WHERE name LIKE 'spec007-%'"); err != nil {
		t.Fatalf("resetAdmin user cleanup: %v", err)
	}
	if _, err := database.DB.Exec(ctx,
		"UPDATE users SET password_hash = $1, enabled = true WHERE name = 'admin'", seededSecretHash); err != nil {
		t.Fatalf("resetAdmin: %v", err)
	}
}
