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
		deleteDomain:  func(context.Context, string) error { return nil },
		deleteProject: func(string) error { return nil },
		deleteRouter:  func(string) error { return nil },
		purgeStorage:  func() error { return nil },
		resetDB:       func() error { return nil },
	}
}

func fullInventory() Inventory {
	return Inventory{
		DomainIDs:  []string{"d1"},
		ProjectIDs: []string{"p1"},
		RouterIDs:  []string{"r1"},
	}
}

// FR-019 / research R3: compute → network → storage → DB-reset-last.
func TestExecute_PurgesInDependencyOrder_ResetsDBLast(t *testing.T) {
	var order []string
	d := deps{
		deleteDomain:  func(_ context.Context, id string) error { order = append(order, "compute:"+id); return nil },
		deleteProject: func(id string) error { order = append(order, "net-project:"+id); return nil },
		deleteRouter:  func(id string) error { order = append(order, "net-router:"+id); return nil },
		purgeStorage:  func() error { order = append(order, "storage"); return nil },
		resetDB:       func() error { order = append(order, "db-reset"); return nil },
	}
	if err := execute(context.Background(), d, fullInventory(), Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := []string{"compute:d1", "net-project:p1", "net-router:r1", "storage", "db-reset"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// FR-008: a missing / already-deleted artifact is success, not a failure.
func TestExecute_AbsentArtifactIsSuccess(t *testing.T) {
	d := okDeps()
	d.deleteDomain = func(context.Context, string) error { return errors.New("domain not found") }
	if err := execute(context.Background(), d, Inventory{DomainIDs: []string{"gone"}}, Options{}); err != nil {
		t.Fatalf("absent artifact must not fail teardown: %v", err)
	}
}

// FR-016: a real failure in a required class fails the whole teardown (fail-closed).
func TestExecute_FailClosedOnRealError(t *testing.T) {
	d := okDeps()
	d.purgeStorage = func() error { return errors.New("permission denied") }
	err := execute(context.Background(), d, Inventory{}, Options{})
	if err == nil || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("expected fail-closed error naming the storage class, got %v", err)
	}
}

// --dry-run enumerates/logs only: no deletes, no reset.
func TestExecute_DryRunDeletesNothing(t *testing.T) {
	touched := false
	d := okDeps()
	d.deleteDomain = func(context.Context, string) error { touched = true; return nil }
	d.deleteProject = func(string) error { touched = true; return nil }
	d.purgeStorage = func() error { touched = true; return nil }
	d.resetDB = func() error { touched = true; return nil }
	if err := execute(context.Background(), d, fullInventory(), Options{DryRun: true}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if touched {
		t.Fatal("dry-run must not invoke any delete or the DB reset")
	}
}

// C9: an unreachable DB (resetDB == nil) is ignored, not a failure.
func TestExecute_DBUnreachableSkipsResetWithoutFailing(t *testing.T) {
	d := okDeps()
	d.resetDB = nil
	if err := execute(context.Background(), d, Inventory{ProjectIDs: []string{"p1"}}, Options{}); err != nil {
		t.Fatalf("DB-unreachable must not fail teardown: %v", err)
	}
}

func TestIsAbsent(t *testing.T) {
	absent := []string{"domain not found", "no such file or directory", "namespace does not exist", "volume already deleted"}
	for _, s := range absent {
		if !isAbsent(errors.New(s)) {
			t.Errorf("%q should be treated as absent", s)
		}
	}
	if isAbsent(errors.New("permission denied")) {
		t.Error("a real error (permission denied) must not be treated as absent")
	}
}

// C9 fallback: namespace names map back to the project/router ids the delete
// methods expect.
func TestClassifyNetns(t *testing.T) {
	inv := classifyNetns([]string{"light-ns-proj-a", "qrouter-abc12345678", "light-ns-proj-b", "lo", "kube-system"})
	if !reflect.DeepEqual(inv.ProjectIDs, []string{"proj-a", "proj-b"}) {
		t.Fatalf("project ids = %v", inv.ProjectIDs)
	}
	if !reflect.DeepEqual(inv.RouterIDs, []string{"abc12345678"}) {
		t.Fatalf("router ids = %v", inv.RouterIDs)
	}
}

func TestParseNetnsList(t *testing.T) {
	got := parseNetnsList("light-ns-x (id: 0)\nqrouter-y\n\n")
	want := []string{"light-ns-x", "qrouter-y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseNetnsList = %v, want %v", got, want)
	}
}
