package hypervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
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

// PrepareInstanceDir creates the per-instance directory and, in real mode, makes it owned
// by the user libvirt runs QEMU as (cached from capabilities at startup). QEMU opens each
// guest's serial console.log — the <log file> chardev — itself, so the directory must be
// writable by that user. Older libvirt opened that log as root in the daemon, which masked
// the gap; the modular virtqemud does not, so a root-owned directory makes the domain fail
// to start ("console.log: Permission denied"). Lightstack keeps write access to stage the
// overlay because root is exempt from the directory's mode. Stub mode — or real mode before
// capabilities have been read — just creates the directory. Idempotent.
func (m *VMManager) PrepareInstanceDir(instanceID string) error {
	dir := InstanceDir(instanceID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if m.mode != "real" {
		return nil
	}
	owner := m.qemuOwner.Load()
	if owner == nil {
		// Capabilities were never read (startup prep failed); guest starts are already
		// refused by consoleDirReady, so creating the directory without the chown is
		// harmless — it never gets used.
		return nil
	}
	if err := os.Chown(dir, owner.uid, owner.gid); err != nil {
		return fmt.Errorf("making instance dir %s QEMU's (%d:%d): %w", dir, owner.uid, owner.gid, err)
	}
	return nil
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
	overlay := OverlayPath(instanceID)
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

// OverlayPath returns the per-instance qcow2 overlay StageOverlay creates, without
// creating it. Needed to rebuild a guest's definition from stored state after its
// domain has gone: the disk is still there, only libvirt's record of it is missing
// (SPEC-025 FR-025a).
func OverlayPath(instanceID string) string {
	return filepath.Join(InstanceDir(instanceID), "disk.qcow2")
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
