package keystone

import (
	"context"
	"os"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
)

// testDBAvailable is true when LIGHTSTACK_TEST_DB_URL is set and the schema migrated;
// DB-backed tests skip otherwise (Constitution: real dependencies over mocks).
var testDBAvailable bool

// testPool is this package's test pool: opened by TestMain when LIGHTSTACK_TEST_DB_URL
// is set, and handed to the services the tests construct (SPEC-027).
var testPool database.DB

func TestMain(m *testing.M) {
	// Skip DB-backed tests only when no test DB is configured. A configured database
	// that cannot be opened or migrated fails the run instead (dbtest.Open), so a broken
	// test database in CI fails the job rather than quietly hiding the DB-backed tests.
	testPool, testDBAvailable = dbtest.Open("../../migrations")
	code := m.Run()
	dbtest.Close(testPool)
	os.Exit(code)
}

func requireDB(t *testing.T) {
	t.Helper()
	if !testDBAvailable {
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run DB-backed keystone tests")
	}
}

// seededSecretHash is the bcrypt('secret') hash from migration 002_seed_data.
const seededSecretHash = "$2a$10$/DyBs44Yot8zgjC4j.n03Ox/QG94ftrksEXKKvadKcixLbGll0dR2"

// resetAdmin restores the seeded admin (enabled, bcrypt('secret')) and removes
// any test-created users so each bootstrap test starts from a known state.
func resetAdmin(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		"DELETE FROM role_assignments WHERE user_id IN (SELECT id FROM users WHERE name LIKE 'spec007-%')"); err != nil {
		t.Fatalf("resetAdmin role cleanup: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM users WHERE name LIKE 'spec007-%'"); err != nil {
		t.Fatalf("resetAdmin user cleanup: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		"UPDATE users SET password_hash = $1, enabled = true WHERE name = 'admin'", seededSecretHash); err != nil {
		t.Fatalf("resetAdmin: %v", err)
	}
}
