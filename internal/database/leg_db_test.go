package database_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// openLeg opens a database on the backend the test leg selects through
// LIGHTSTACK_TEST_DB_URL: the PostgreSQL test database itself, or a fresh SQLite file
// in the test's temporary directory. Without the variable the test skips.
func openLeg(t *testing.T) database.DB {
	t.Helper()
	url := os.Getenv("LIGHTSTACK_TEST_DB_URL")
	switch {
	case strings.HasPrefix(url, "postgres://"), strings.HasPrefix(url, "postgresql://"):
	case strings.HasPrefix(url, "sqlite://"):
		url = "sqlite://" + filepath.Join(t.TempDir(), "leg.db")
	default:
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run the database-backed tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := database.Open(ctx, url, database.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}
