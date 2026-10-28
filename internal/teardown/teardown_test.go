package teardown

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

func okDeps() deps {
	return deps{
		deleteDomain:  func(context.Context, string) error { return nil },
		purgeNetwork:  func() error { return nil },
		purgeStorage:  func() error { return nil },
		purgeDatabase: func() error { return nil },
	}
}

// Order: compute → network → storage → database (research R3). The node-local SQLite db/
// is removed last, after every other class purged cleanly.
func TestExecute_PurgesInOrder(t *testing.T) {
	var order []string
	d := deps{
		deleteDomain:  func(_ context.Context, id string) error { order = append(order, "compute:"+id); return nil },
		purgeNetwork:  func() error { order = append(order, "network"); return nil },
		purgeStorage:  func() error { order = append(order, "storage"); return nil },
		purgeDatabase: func() error { order = append(order, "database"); return nil },
	}
	if err := execute(context.Background(), d, Inventory{DomainIDs: []string{"d1"}}, Options{}, true); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"compute:d1", "network", "storage", "database"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// SPEC-029 FR-013: the enumeration connection is closed AFTER the network purge (which
// queries the DB) and BEFORE the SQLite db/ directory is removed, so that directory is
// never unlinked while a connection holds -wal/-shm.
func TestExecute_ClosesDBBeforeDatabasePurge(t *testing.T) {
	var order []string
	d := deps{
		deleteDomain:  func(context.Context, string) error { return nil },
		purgeNetwork:  func() error { order = append(order, "network"); return nil },
		purgeStorage:  func() error { order = append(order, "storage"); return nil },
		purgeDatabase: func() error { order = append(order, "database"); return nil },
		closeDB:       func() { order = append(order, "closeDB") },
	}
	if err := execute(context.Background(), d, Inventory{}, Options{}, true); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"network", "closeDB", "storage", "database"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// SPEC-029 FR-012 (outcome): the unreachable-DB path (closeDB nil) completes and
// returns success — the uninstall is not blocked when cleanup cannot reach the database.
func TestExecute_UnreachableDBStillSucceeds(t *testing.T) {
	d := okDeps() // closeDB nil, as Run leaves it on the unreachable branch
	if err := execute(context.Background(), d, Inventory{}, Options{}, false); err != nil {
		t.Fatalf("execute on the unreachable-DB path should succeed, got: %v", err)
	}
}

// FR-008: a missing / already-deleted domain is success, not a failure.
func TestExecute_AbsentDomainIsSuccess(t *testing.T) {
	d := okDeps()
	d.deleteDomain = func(context.Context, string) error { return errors.New("domain not found") }
	if err := execute(context.Background(), d, Inventory{DomainIDs: []string{"gone"}}, Options{}, true); err != nil {
		t.Fatalf("absent domain must not fail teardown: %v", err)
	}
}

// FR-016: a real failure in a class fails the whole teardown (fail-closed).
func TestExecute_FailClosedOnNetworkError(t *testing.T) {
	d := okDeps()
	d.purgeNetwork = func() error { return errors.New("iptables: permission denied") }
	err := execute(context.Background(), d, Inventory{}, Options{}, true)
	if err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("expected fail-closed error naming the network class, got %v", err)
	}
}

func TestExecute_FailClosedOnStorageError(t *testing.T) {
	d := okDeps()
	d.purgeStorage = func() error { return errors.New("read-only file system") }
	err := execute(context.Background(), d, Inventory{}, Options{}, true)
	if err == nil || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("expected fail-closed error naming the storage class, got %v", err)
	}
}

// A domain-enumeration failure (DB reachable, query errored) fails the compute class
// rather than reporting a false "complete" (fail-closed).
func TestExecute_FailClosedOnEnumError(t *testing.T) {
	d := okDeps()
	inv := Inventory{enumErr: errors.New(`query: relation "instances" does not exist`)}
	err := execute(context.Background(), d, inv, Options{}, true)
	if err == nil || !strings.Contains(err.Error(), "compute") {
		t.Fatalf("expected fail-closed error naming the compute class, got %v", err)
	}
}

// When any class fails, the uninstall will be retried, so the SQLite database (the retry's
// inventory) must be LEFT in place: purgeDatabase must not run. Otherwise the retry opens
// MustExist, fails, falls back to the DB-unreachable path, and reports best-effort success
// with domains or network rules still present (SPEC-029 FR-013).
func TestExecute_PreservesDatabaseWhenAClassFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(d *deps, inv *Inventory)
	}{
		{"compute enumeration", func(_ *deps, inv *Inventory) { inv.enumErr = errors.New(`query: no such table: instances`) }},
		{"compute delete", func(d *deps, _ *Inventory) {
			d.deleteDomain = func(context.Context, string) error { return errors.New("permission denied") }
		}},
		{"network", func(d *deps, _ *Inventory) {
			d.purgeNetwork = func() error { return errors.New("iptables: permission denied") }
		}},
		{"storage", func(d *deps, _ *Inventory) {
			d.purgeStorage = func() error { return errors.New("read-only file system") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPurged := false
			d := okDeps()
			d.purgeDatabase = func() error { dbPurged = true; return nil }
			inv := Inventory{DomainIDs: []string{"d1"}}
			tc.mut(&d, &inv)
			if err := execute(context.Background(), d, inv, Options{}, true); err == nil {
				t.Fatal("expected a fail-closed error")
			}
			if dbPurged {
				t.Fatal("the SQLite database must be preserved for the retry when a class fails")
			}
		})
	}
}

// --dry-run enumerates/logs only: no deletes, no network/storage purge.
func TestExecute_DryRunDoesNothing(t *testing.T) {
	touched := false
	d := deps{
		deleteDomain:  func(context.Context, string) error { touched = true; return nil },
		purgeNetwork:  func() error { touched = true; return nil },
		purgeStorage:  func() error { touched = true; return nil },
		purgeDatabase: func() error { touched = true; return nil },
	}
	if err := execute(context.Background(), d, Inventory{DomainIDs: []string{"d1"}}, Options{DryRun: true}, true); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if touched {
		t.Fatal("dry-run must not invoke any delete / network / storage / database purge")
	}
}

func TestIsAbsent(t *testing.T) {
	for _, s := range []string{"domain not found", "no such file or directory", "namespace does not exist", "chain already removed"} {
		if !isAbsent(errors.New(s)) {
			t.Errorf("%q should be treated as absent", s)
		}
	}
	if isAbsent(errors.New("permission denied")) {
		t.Error("a real error (permission denied) must not be treated as absent")
	}
}

func TestParseNetnsList(t *testing.T) {
	got := parseNetnsList("light-ns-x (id: 0)\nqrouter-y\n\n")
	want := []string{"light-ns-x", "qrouter-y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseNetnsList = %v, want %v", got, want)
	}
}

// The uninstall purge removes every node-local tree Lightstack writes, the console's
// two included: token files are credential-shaped, and both directories are recreated
// with the right owner at the next start (SPEC-021 FR-017b). Anything else under the
// root is not Lightstack's to remove.
func TestPurgeLocalStorage(t *testing.T) {
	root := t.TempDir()
	paths.SetBase(root)
	t.Cleanup(func() { paths.SetBase(paths.DefaultBase) })

	ours := []string{
		paths.ImagesDir(), paths.VolumesDir(), paths.InstancesDir(), paths.CloudInitDir(),
		paths.DHCPDir(), paths.ConsoleTokenDir(), paths.ConsoleSocketDir(),
	}
	// The SQLite db/ directory is NOT removed by purgeLocalStorage — purgeLocalDatabase
	// removes it separately, and only on a fully clean run (SPEC-029 FR-013). It must survive.
	keep := []string{filepath.Join(root, "not-lightstacks"), paths.DBDir()}
	for _, dir := range append(append([]string{}, ours...), keep...) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := purgeLocalStorage(); err != nil {
		t.Fatalf("purgeLocalStorage: %v", err)
	}
	for _, dir := range ours {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived the purge", dir)
		}
	}
	for _, dir := range keep {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("purgeLocalStorage removed %s, which it must not: %v", dir, err)
		}
	}
}

// purgeLocalDatabase removes the directory of the database the URL names — the default db/
// AND a non-default subdirectory — not a fixed db/, so a custom database.url is not left behind
// (SPEC-029 FR-013/FR-004). It leaves other node-local trees, removes nothing for PostgreSQL,
// and never removes the state root itself.
func TestPurgeLocalDatabase(t *testing.T) {
	root := t.TempDir()
	paths.SetBase(root)
	t.Cleanup(func() { paths.SetBase(paths.DefaultBase) })

	write := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gone := func(p string) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", p)
		}
	}
	kept := func(p string) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should have been kept: %v", p, err)
		}
	}

	// The canonical db/ directory: the database file and its sidecars go, and the now-empty dir
	// with them, so a delete-policy teardown leaves nothing behind there.
	db := paths.DBDir()
	for _, f := range []string{"lightstack.db", "lightstack.db-wal", "lightstack.db-shm"} {
		write(filepath.Join(db, f))
	}
	if err := purgeLocalDatabase("sqlite://" + filepath.Join(db, "lightstack.db")); err != nil {
		t.Fatalf("purgeLocalDatabase(db): %v", err)
	}
	gone(filepath.Join(db, "lightstack.db"))
	gone(db)

	// A custom directory under the root: the database files go, but an unrelated sibling and the
	// directory itself are kept — the URL owns the file, not everything beside it.
	custom := filepath.Join(root, "custom")
	write(filepath.Join(custom, "lightstack.db"))
	write(filepath.Join(custom, "lightstack.db-wal"))
	customSibling := filepath.Join(custom, "notes.txt")
	write(customSibling)
	if err := purgeLocalDatabase("sqlite://" + filepath.Join(custom, "lightstack.db")); err != nil {
		t.Fatalf("purgeLocalDatabase(custom): %v", err)
	}
	gone(filepath.Join(custom, "lightstack.db"))
	gone(filepath.Join(custom, "lightstack.db-wal"))
	kept(customSibling)
	kept(custom)

	// A shared directory OUTSIDE the state root (the bare binary accepts one): the target database
	// goes, another database and the directory are untouched.
	shared := t.TempDir()
	write(filepath.Join(shared, "lightstack.db"))
	otherDB := filepath.Join(shared, "other.db")
	write(otherDB)
	if err := purgeLocalDatabase("sqlite://" + filepath.Join(shared, "lightstack.db")); err != nil {
		t.Fatalf("purgeLocalDatabase(shared): %v", err)
	}
	gone(filepath.Join(shared, "lightstack.db"))
	kept(otherDB)
	kept(shared)

	// A database directly in the state root: only its files go; the root and its siblings stay.
	write(filepath.Join(root, "lightstack.db"))
	rootSibling := filepath.Join(root, "keep-root.txt")
	write(rootSibling)
	if err := purgeLocalDatabase("sqlite://" + filepath.Join(root, "lightstack.db")); err != nil {
		t.Fatalf("purgeLocalDatabase(root db): %v", err)
	}
	gone(filepath.Join(root, "lightstack.db"))
	kept(rootSibling)
	kept(root)

	// A db/ that still holds another file: the database files go, the stray file and the dir stay
	// (os.Remove is non-recursive).
	write(filepath.Join(db, "lightstack.db"))
	stray := filepath.Join(db, "keep.me")
	write(stray)
	if err := purgeLocalDatabase("sqlite://" + filepath.Join(db, "lightstack.db")); err != nil {
		t.Fatalf("purgeLocalDatabase(db with stray): %v", err)
	}
	gone(filepath.Join(db, "lightstack.db"))
	kept(stray)
	kept(db)

	// An already-absent database is success and removes nothing (the custom sibling still stands).
	if err := purgeLocalDatabase("sqlite://" + filepath.Join(custom, "lightstack.db")); err != nil {
		t.Fatalf("purgeLocalDatabase on an absent database should be nil, got: %v", err)
	}
	kept(customSibling)

	// PostgreSQL has no node-local database: a no-op that touches nothing.
	pgSentinel := filepath.Join(root, "pgcheck", "sentinel")
	write(pgSentinel)
	if err := purgeLocalDatabase("postgres://u@h/db"); err != nil {
		t.Fatalf("purgeLocalDatabase(postgres): %v", err)
	}
	kept(pgSentinel)
}
