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
		deleteDomain: func(context.Context, string) error { return nil },
		purgeNetwork: func() error { return nil },
		purgeStorage: func() error { return nil },
	}
}

// Order: compute → network → storage (research R3). No DB reset (operator-owned).
func TestExecute_PurgesInOrder(t *testing.T) {
	var order []string
	d := deps{
		deleteDomain: func(_ context.Context, id string) error { order = append(order, "compute:"+id); return nil },
		purgeNetwork: func() error { order = append(order, "network"); return nil },
		purgeStorage: func() error { order = append(order, "storage"); return nil },
	}
	if err := execute(context.Background(), d, Inventory{DomainIDs: []string{"d1"}}, Options{}, true); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"compute:d1", "network", "storage"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// SPEC-029 FR-013: the enumeration connection is closed AFTER the network purge (which
// queries the DB) and BEFORE the storage purge, so the SQLite db/ directory is never
// unlinked while a connection holds -wal/-shm.
func TestExecute_ClosesDBBeforeStoragePurge(t *testing.T) {
	var order []string
	d := deps{
		deleteDomain: func(context.Context, string) error { return nil },
		purgeNetwork: func() error { order = append(order, "network"); return nil },
		purgeStorage: func() error { order = append(order, "storage"); return nil },
		closeDB:      func() { order = append(order, "closeDB") },
	}
	if err := execute(context.Background(), d, Inventory{}, Options{}, true); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"network", "closeDB", "storage"}
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

// --dry-run enumerates/logs only: no deletes, no network/storage purge.
func TestExecute_DryRunDoesNothing(t *testing.T) {
	touched := false
	d := deps{
		deleteDomain: func(context.Context, string) error { touched = true; return nil },
		purgeNetwork: func() error { touched = true; return nil },
		purgeStorage: func() error { touched = true; return nil },
	}
	if err := execute(context.Background(), d, Inventory{DomainIDs: []string{"d1"}}, Options{DryRun: true}, true); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if touched {
		t.Fatal("dry-run must not invoke any delete / network / storage purge")
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
		paths.DBDir(), // SPEC-029 FR-013: the SQLite db directory is purged too (last)
	}
	keep := filepath.Join(root, "not-lightstacks")
	for _, dir := range append(ours, keep) {
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
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the purge removed %s, which is not Lightstack's: %v", keep, err)
	}
}
