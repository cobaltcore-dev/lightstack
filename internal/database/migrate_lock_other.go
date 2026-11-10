//go:build !unix

package database

import (
	"fmt"
	"runtime"
)

// lockMigrations has no implementation off unix: the migration lock is an exclusive flock, which
// only unix provides. SQLite is a single-node, unix deployment; this stub exists so the migrator
// package builds and the lightstack-migrate CLI links on other platforms, rather than running the
// SQLite path there.
func lockMigrations(dbPath string) (unlock func(), err error) {
	return nil, fmt.Errorf("SQLite migrations are supported on unix only, not %s", runtime.GOOS)
}
