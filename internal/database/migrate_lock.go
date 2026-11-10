//go:build unix

package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

// lockMigrations takes the migration lock of the SQLite database at dbPath — an exclusive
// flock on <dbPath>.migrate-lock — and holds it until unlock is called (FR-030, research
// R12). golang-migrate's SQLite driver locks within its own process only, so two
// processes migrating one file at once — two server starts, or lightstack-migrate beside
// a server — would both apply the same migration. The lock is polled every 100 ms for as
// long as golang-migrate waits for its own, then the error names the file.
func lockMigrations(dbPath string) (unlock func(), err error) {
	lockPath := dbPath + ".migrate-lock"
	if err := makeDatabaseDir(filepath.Dir(lockPath)); err != nil {
		return nil, fmt.Errorf("the migration lock %s: %w", lockPath, err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the migration lock %s: %w", lockPath, err)
	}
	if err := waitFlock(f, lockPath, migrate.DefaultLockTimeout); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// waitFlock takes an exclusive flock on f, polling every 100 ms up to timeout, then returning an
// error naming path. path is used only for the message.
func waitFlock(f *os.File, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("take the migration lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another migration holds %s; gave up after %s", path, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
