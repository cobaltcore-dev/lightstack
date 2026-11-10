package database

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMigrationHelperProcess is not a test: TestTwoMigratorsShareOneFile runs the test
// binary again with it selected, as a second process migrating the same file.
func TestMigrationHelperProcess(t *testing.T) {
	url := os.Getenv("LS_MIGRATE_HELPER_URL")
	if url == "" {
		t.Skip("helper process for TestTwoMigratorsShareOneFile")
	}
	if err := MigrateUp(url, "../../migrations"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestMigrationLockCreatesDirectory pins FR-011 for lightstack-migrate, whose lock is the
// first thing to touch a fresh database's directory: it is created 0750, and a missing
// parent — the state root — 0755, because QEMU must traverse it to reach its disks.
func TestMigrationLockCreatesDirectory(t *testing.T) {
	pinUmask(t)
	root := filepath.Join(t.TempDir(), "state")
	dir := filepath.Join(root, "db")
	unlock, err := lockMigrations(filepath.Join(dir, "lightstack.db"))
	if err != nil {
		t.Fatalf("lockMigrations: %v", err)
	}
	unlock()
	for p, want := range map[string]os.FileMode{root: 0o755, dir: 0o750} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o; want %o", p, got, want)
		}
	}
}

// TestTwoMigratorsShareOneFile pins SC-007 (FR-030): two processes migrating one fresh
// SQLite file at once both succeed, and leave one schema, migrated once — in 20 of 20
// runs. golang-migrate's SQLite driver locks within its own process only.
func TestTwoMigratorsShareOneFile(t *testing.T) {
	if testing.Short() {
		t.Skip("starts 40 processes")
	}
	newest := newestMigration(t)
	for run := range 20 {
		path := filepath.Join(t.TempDir(), "db", "lightstack.db")
		url := "sqlite://" + path
		procs := make([]*exec.Cmd, 2)
		outs := make([][]byte, 2)
		errs := make([]error, 2)
		done := make(chan int, 2)
		for i := range procs {
			procs[i] = exec.Command(os.Args[0], "-test.run=^TestMigrationHelperProcess$", "-test.count=1")
			procs[i].Env = append(os.Environ(), "LS_MIGRATE_HELPER_URL="+url)
		}
		for i := range procs {
			go func() {
				outs[i], errs[i] = procs[i].CombinedOutput()
				done <- i
			}()
		}
		<-done
		<-done
		for i := range procs {
			if errs[i] != nil {
				t.Fatalf("run %d: migrator %d failed: %v\n%s", run+1, i+1, errs[i], outs[i])
			}
		}
		db, err := openSQLiteHandle(context.Background(), path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1})
		if err != nil {
			t.Fatal(err)
		}
		var integrity string
		var versions int
		var version uint
		if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT count(*), max(version) FROM schema_migrations`).Scan(&versions, &version); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
		if integrity != "ok" || versions != 1 || version != newest {
			t.Fatalf("run %d: integrity %q, %d schema_migrations rows at version %d; want ok, 1, %d",
				run+1, integrity, versions, version, newest)
		}
	}
}

// newestMigration is the highest version in the migrations folder.
func newestMigration(t *testing.T) uint {
	t.Helper()
	entries, err := os.ReadDir("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	var newest uint
	for _, e := range entries {
		if f, ok := parseMigrationFile(e.Name()); ok && uint(f.version) > newest {
			newest = uint(f.version)
		}
	}
	return newest
}
