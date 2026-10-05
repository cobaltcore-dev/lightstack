package nova

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
)

// A start the hypervisor refused because the console socket directory is not QEMU's
// names the directory: the operator has to fix it on the host, and a bare "failed to
// start server" would send them looking at libvirt (SPEC-021 FR-017a). Any other
// failure keeps the generic message.
func TestStartRefusal(t *testing.T) {
	notReady := fmt.Errorf("%w: /var/lib/lightstack/console-sockets: no DAC base label",
		hypervisor.ErrConsoleSocketDirNotReady)

	got := startRefusal(notReady, "failed to start server")
	if got.StatusCode != 500 {
		t.Errorf("status = %d; want 500", got.StatusCode)
	}
	if !strings.Contains(got.Message, "/var/lib/lightstack/console-sockets") {
		t.Errorf("message %q does not name the directory", got.Message)
	}

	other := startRefusal(errors.New("libvirt: domain not found"), "failed to start server")
	if other.Message != "failed to start server" {
		t.Errorf("message = %q; a libvirt error must keep the generic message", other.Message)
	}
}

// With no hypervisor at all there is nothing to prepare; saying so beats a nil deref.
func TestPrepareConsoleSocketDirWithoutAHypervisor(t *testing.T) {
	svc := NewService(testPool, "", "real", nil)
	if err := svc.PrepareConsoleSocketDir(t.Context(), t.TempDir()); !errors.Is(err, errNoHypervisor) {
		t.Errorf("err = %v; want errNoHypervisor", err)
	}
}
