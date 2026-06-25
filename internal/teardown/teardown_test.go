package teardown

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
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
	if err := execute(context.Background(), d, Inventory{DomainIDs: []string{"d1"}}, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"compute:d1", "network", "storage"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// FR-008: a missing / already-deleted domain is success, not a failure.
func TestExecute_AbsentDomainIsSuccess(t *testing.T) {
	d := okDeps()
	d.deleteDomain = func(context.Context, string) error { return errors.New("domain not found") }
	if err := execute(context.Background(), d, Inventory{DomainIDs: []string{"gone"}}, Options{}); err != nil {
		t.Fatalf("absent domain must not fail teardown: %v", err)
	}
}

// FR-016: a real failure in a class fails the whole teardown (fail-closed).
func TestExecute_FailClosedOnNetworkError(t *testing.T) {
	d := okDeps()
	d.purgeNetwork = func() error { return errors.New("iptables: permission denied") }
	err := execute(context.Background(), d, Inventory{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("expected fail-closed error naming the network class, got %v", err)
	}
}

func TestExecute_FailClosedOnStorageError(t *testing.T) {
	d := okDeps()
	d.purgeStorage = func() error { return errors.New("read-only file system") }
	err := execute(context.Background(), d, Inventory{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("expected fail-closed error naming the storage class, got %v", err)
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
	if err := execute(context.Background(), d, Inventory{DomainIDs: []string{"d1"}}, Options{DryRun: true}); err != nil {
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
