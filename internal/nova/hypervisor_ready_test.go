package nova

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
)

// SPEC-031 FR-017, FR-018a: real-mode readiness waits for the hypervisor. This package cannot
// produce a prepared manager — the fake libvirtd lives in pkg/hypervisor's tests, which cover
// the prepared and sticky cases (ConsoleDirPrepared) — so it pins the cases it can reach.
func TestHypervisorReady(t *testing.T) {
	svc := NewService(nil, "", "real", nil)
	if err := svc.HypervisorReady(context.Background()); err == nil {
		t.Error("ready with no hypervisor wired in")
	}

	dir := t.TempDir()
	mgr, err := hypervisor.StartVMManager(filepath.Join(dir, "missing-sock"), "real", filepath.Join(dir, "cs"))
	if err != nil {
		t.Fatalf("StartVMManager: %v", err)
	}
	t.Cleanup(mgr.Close)
	svc.vmManager = mgr
	if err := svc.HypervisorReady(context.Background()); err == nil {
		t.Error("ready while libvirt cannot be reached")
	}
}
