package nova

import (
	"os"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
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
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run DB-backed nova tests")
	}
}
