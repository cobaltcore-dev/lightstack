package console

import (
	"os"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
)

// testDBAvailable is true when LIGHTSTACK_TEST_DB_URL is set and the schema migrated.
// Article IV wants real databases over mocks, so the token store is exercised
// against a real database (either backend); without one, the DB-backed tests skip
// (the pure projection and parsing tests still run).
//
// Mirrors internal/nova/maindb_test.go: when LIGHTSTACK_TEST_DB_URL IS set, a
// connect/migrate failure is fatal rather than a silent skip, so a broken test
// database in CI fails the job instead of quietly hiding coverage.
var testDBAvailable bool

// testPool is this package's test pool: opened by TestMain when LIGHTSTACK_TEST_DB_URL
// is set, and handed to the stores the tests construct (SPEC-027).
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
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run DB-backed console-token tests")
	}
}
