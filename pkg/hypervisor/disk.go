package hypervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/cobaltcore-dev/o3k/pkg/paths"
)

// InstanceDir returns the per-instance directory that holds an instance's
// qcow2 overlay disk (SPEC-009 WS-0).
func InstanceDir(instanceID string) string {
	return filepath.Join(paths.InstancesDir(), instanceID)
}

// CloudInitISOPath returns the per-instance cloud-init config-drive ISO path
// (SPEC-009 WS-4).
func CloudInitISOPath(instanceID string) string {
	return filepath.Join(paths.CloudInitDir(), instanceID+".iso")
}

// EnsureInstanceDir creates the per-instance directory (idempotent). The domain
// XML writes the serial console to <InstanceDir>/console.log, so this directory
// must exist before the domain starts. Image boots get it as a side effect of
// StageOverlay; boot-from-volume skips StageOverlay and must create it
// explicitly (SPEC-009 WS-5).
func EnsureInstanceDir(instanceID string) error {
	return os.MkdirAll(InstanceDir(instanceID), 0o755)
}

// EnsureInstanceDir creates the per-instance directory (idempotent). The domain
// XML writes the serial console to <InstanceDir>/console.log, so this directory
// must exist before the domain starts. Image boots get it as a side effect of
// StageOverlay; boot-from-volume skips StageOverlay and must create it
// explicitly (SPEC-009 WS-5).
func EnsureInstanceDir(instanceID string) error {
	return os.MkdirAll(InstanceDir(instanceID), 0o755)
}

// RemoveInstanceDir removes an instance's per-instance disk directory and its
// contents (the overlay). Best-effort: a missing directory is not an error, so
// it is safe to call in stub mode where no overlay was ever staged.
func RemoveInstanceDir(instanceID string) error {
	return os.RemoveAll(InstanceDir(instanceID))
}

// StageOverlay creates the per-instance qcow2 overlay backed by the image's raw
// base and returns the overlay path (SPEC-009 WS-0). Real mode only — it shells
// out to qemu-img, which the reference image installs. The backing format is
// pinned to raw (-F raw) so a later Glance delete cannot silently break the
// chain; the image-in-use guard (FR-004) blocks that delete anyway.
func StageOverlay(instanceID, rawBasePath string) (string, error) {
	if err := EnsureInstanceDir(instanceID); err != nil {
		return "", fmt.Errorf("create instance dir: %w", err)
	}
	overlay := filepath.Join(InstanceDir(instanceID), "disk.qcow2")
	// qemu-img create -f qcow2 -b <raw base> -F raw <overlay>
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", "-b", rawBasePath, "-F", "raw", overlay)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("qemu-img create overlay (base %s): %w: %s", rawBasePath, err, string(out))
	}
	// NOTE: the host qemu user must be able to read the overlay; on hosts where
	// libvirt runs qemu as a non-root user, ownership/permissions are handled by
	// the deployment (privileged pod) — validated on the host (quickstart).
	return overlay, nil
}

// ConsoleLogPath returns the per-instance serial console log file. libvirt is
// configured (in the domain XML) to capture the VM's serial output here so
// os-getConsoleOutput works (SPEC-009 WS-0).
func ConsoleLogPath(instanceID string) string {
	return filepath.Join(InstanceDir(instanceID), "console.log")
}

// ReadConsoleLog returns the instance's captured serial console output. When
// length > 0, only the last length bytes are returned (Nova's os-getConsoleOutput
// semantics). It errors if no console log exists yet (e.g. the VM has not booted).
func ReadConsoleLog(instanceID string, length int) (string, error) {
	data, err := os.ReadFile(ConsoleLogPath(instanceID))
	if err != nil {
		return "", err
	}
	out := string(data)
	if length > 0 && len(out) > length {
		out = out[len(out)-length:]
	}
	return out, nil
}
