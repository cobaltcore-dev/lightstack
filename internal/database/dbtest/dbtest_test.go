package dbtest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// TestMigrateFailure pins the remedy line a refused shared PostgreSQL test database gets (SPEC-032
// T032): recognised by the error's type, never by its text, and only where make db-up applies.
func TestMigrateFailure(t *testing.T) {
	const pg = "postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable"
	const remedy = "(the test database: make db-down && make db-up)"
	preSquash := fmt.Errorf("migrate: %w", &database.PreSquashError{Version: 82})
	for _, tc := range []struct {
		name string
		url  string
		err  error
		want bool
	}{
		{"pre-squash PostgreSQL", pg, preSquash, true},
		{"pre-squash SQLite", "sqlite:///tmp/ls-test/t.db", preSquash, false},
		{"stale PostgreSQL", pg, fmt.Errorf("migrate: %w", &database.StaleError{Version: 2}), true},
		{"another failure", pg, errors.New("from before the migration squash, in other words"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := migrateFailure(tc.url, tc.err)
			if got := strings.Contains(msg, remedy); got != tc.want {
				t.Errorf("remedy in %q: %v; want %v", msg, got, tc.want)
			}
			if !strings.Contains(msg, tc.err.Error()) {
				t.Errorf("the message %q drops the error", msg)
			}
		})
	}
}
