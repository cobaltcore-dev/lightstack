package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/cobaltcore-dev/o3k/pkg/paths"
	"github.com/rs/zerolog/log"
)

// CephClient manages storage operations (RBD, local, or both)
type CephClient struct {
	mode        string // "stub", "rbd", "local", or "local,rbd"
	pool        string
	confFile    string
	timeout     time.Duration
	mu          sync.Mutex
	stubVolumes map[string]*stubVolume // For stub mode
	localPath   string                 // For local mode
	conn        any                    //nolint:unused // Ceph RADOS connection — used by the ceph build tag
	ioctx       any                    //nolint:unused // RADOS IO context for pool — used by the ceph build tag
}

// stubVolume represents a simulated volume
type stubVolume struct {
	id        string
	sizeGB    int
	createdAt time.Time
}

// resolveLocalPath returns the node-local storage directory for subdir (e.g.
// "volumes"). It prefers the host storage path (/var/lib/o3k — the chart's
// hostPath mount) and falls back to ~/.o3k for non-containerized runs, mirroring
// pkg/storage/image_store.go (FR-020 / SPEC-009). Co-locating Cinder and Glance
// node-local data under the same base keeps it persistent across pod restarts
// (SPEC-008 FR-012a) and reachable by the teardown storage purge (SPEC-009 FR-005).
func resolveLocalPath(subdir string) string {
	return resolveLocalPathWithBase("/var/lib/o3k", subdir)
}

// resolveLocalPathWithBase is the testable core of resolveLocalPath.
func resolveLocalPathWithBase(base, subdir string) string {
	if fi, err := os.Stat(base); err == nil && fi.IsDir() {
		return filepath.Join(base, subdir)
	}
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".o3k", subdir)
}

// LocalVolumePath is the backing file for a local-mode volume (SPEC-009 WS-5).
func LocalVolumePath(volumeID string) string {
	return filepath.Join(LocalVolumeDir(), "volume-"+volumeID+".qcow2")
}

// RemoveLocalVolume deletes a local-mode volume's backing file, tolerant of an
// already-absent file. Used by the Nova delete path for delete-on-termination
// volumes (SPEC-009 WS-5 / FR-023); RBD backing cleanup routes through Cinder.
func RemoveLocalVolume(volumeID string) error {
	if err := os.Remove(LocalVolumePath(volumeID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// NewCephClient creates a new storage client
func NewCephClient(mode, pool, confFile string) *CephClient {
	// Node-local volumes live under the host storage path (mirroring Glance),
	// not a bare ~/.o3k, so they persist and the teardown purge can find them.
	localPath := resolveLocalPath("volumes")

	client := &CephClient{
		mode:        mode,
		pool:        pool,
		confFile:    confFile,
		timeout:     1 * time.Second, // Fail-fast: 1 second timeout
		stubVolumes: make(map[string]*stubVolume),
		localPath:   localPath,
	}

	// Create local storage directory if needed
	if mode == "local" || mode == "local,rbd" {
		_ = os.MkdirAll(client.localPath, 0755)
	}

	// Initialize Ceph connection if RBD mode
	if mode == "rbd" || mode == "local,rbd" {
		if err := client.initCephConnection(); err != nil {
			log.Warn().Err(err).Msg("failed to initialize Ceph connection; running in degraded mode")
		}
	}

	return client
}

// CreateVolume creates a storage volume
func (c *CephClient) CreateVolume(ctx context.Context, volumeID string, sizeGB int) error {
	switch c.mode {
	case "stub":
		return c.createVolumeStub(volumeID, sizeGB)
	case "local":
		return c.createVolumeLocal(ctx, volumeID, sizeGB)
	case "rbd":
		return c.createVolumeRBD(ctx, volumeID, sizeGB)
	case "local,rbd":
		// Create in local first, then RBD
		if err := c.createVolumeLocal(ctx, volumeID, sizeGB); err != nil {
			return fmt.Errorf("failed to create local volume: %w", err)
		}
		if err := c.createVolumeRBD(ctx, volumeID, sizeGB); err != nil {
			// Rollback local creation (best-effort; the RBD error is
			// the user-facing one)
			if rbErr := c.deleteVolumeLocal(ctx, volumeID); rbErr != nil {
				log.Warn().Err(rbErr).Str("volume_id", volumeID).Msg("rollback: failed to delete local volume after RBD create failure")
			}
			return fmt.Errorf("failed to create RBD volume: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported storage mode: %s", c.mode)
	}
}

// createVolumeStub simulates volume creation
func (c *CephClient) createVolumeStub(volumeID string, sizeGB int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.stubVolumes[volumeID]; exists {
		return fmt.Errorf("volume %s already exists", volumeID)
	}

	c.stubVolumes[volumeID] = &stubVolume{
		id:        volumeID,
		sizeGB:    sizeGB,
		createdAt: time.Now(),
	}

	return nil
}

// createVolumeLocal creates a real local qcow2 volume backing file via qemu-img
// (SPEC-009 WS-5). A previous stub only truncated a sparse file, which libvirt
// could not attach as a qcow2 disk.
func (c *CephClient) createVolumeLocal(ctx context.Context, volumeID string, sizeGB int) error {
	volumePath := LocalVolumePath(volumeID)
	if _, err := os.Stat(volumePath); err == nil {
		return fmt.Errorf("volume %s already exists", volumeID)
	}
	cmd := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", volumePath, fmt.Sprintf("%dG", sizeGB))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create %s (%dG): %w, output: %s", volumePath, sizeGB, err, out)
	}
	return nil
}

// CreateVolumeFromImage provisions a bootable volume from a (raw) base image.
// Local mode copies the base into the volume's qcow2 and grows it to sizeGB;
// pure-RBD CoW clone is validated separately under the ceph build tag (SPEC-009
// WS-5 / T040).
func (c *CephClient) CreateVolumeFromImage(ctx context.Context, volumeID, imageRawPath string, sizeGB int) error {
	switch c.mode {
	case "stub":
		return nil
	case "local", "local,rbd":
		return c.createVolumeFromImageLocal(ctx, volumeID, imageRawPath, sizeGB)
	case "rbd":
		return fmt.Errorf("volume-from-image on pure RBD requires the ceph build tag; use storage_mode local or local,rbd")
	default:
		return fmt.Errorf("unsupported storage mode: %s", c.mode)
	}
}

func (c *CephClient) createVolumeFromImageLocal(ctx context.Context, volumeID, imageRawPath string, sizeGB int) error {
	volumePath := LocalVolumePath(volumeID)
	if _, err := os.Stat(volumePath); err == nil {
		return fmt.Errorf("volume %s already exists", volumeID)
	}
	if _, err := os.Stat(imageRawPath); err != nil {
		return fmt.Errorf("source image %s not found: %w", imageRawPath, err)
	}
	// Copy the raw base into a qcow2 volume, then grow to the requested size.
	if out, err := exec.CommandContext(ctx, "qemu-img", "convert", "-f", "raw", "-O", "qcow2", imageRawPath, volumePath).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert %s -> %s: %w, output: %s", imageRawPath, volumePath, err, out)
	}
	if out, err := exec.CommandContext(ctx, "qemu-img", "resize", volumePath, fmt.Sprintf("%dG", sizeGB)).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img resize %s %dG: %w, output: %s", volumePath, sizeGB, err, out)
	}
	return nil
}

// deleteVolumeLocal deletes a local volume (tolerant of an already-absent file).
func (c *CephClient) deleteVolumeLocal(ctx context.Context, volumeID string) error {
	if err := os.Remove(LocalVolumePath(volumeID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DeleteVolume deletes a storage volume
func (c *CephClient) DeleteVolume(ctx context.Context, volumeID string) error {
	switch c.mode {
	case "stub":
		return c.deleteVolumeStub(volumeID)
	case "local":
		return c.deleteVolumeLocal(ctx, volumeID)
	case "rbd":
		return c.deleteVolumeRBD(ctx, volumeID)
	case "local,rbd":
		// Delete both (best effort)
		localErr := c.deleteVolumeLocal(ctx, volumeID)
		rbdErr := c.deleteVolumeRBD(ctx, volumeID)
		if localErr != nil && rbdErr != nil {
			return fmt.Errorf("failed to delete: local=%v, rbd=%v", localErr, rbdErr)
		}
		return nil
	default:
		return fmt.Errorf("unsupported storage mode: %s", c.mode)
	}
}

// deleteVolumeStub simulates volume deletion
func (c *CephClient) deleteVolumeStub(volumeID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.stubVolumes[volumeID]; !exists {
		return fmt.Errorf("volume %s not found", volumeID)
	}

	delete(c.stubVolumes, volumeID)
	return nil
}

// CreateSnapshot creates a snapshot of a volume
func (c *CephClient) CreateSnapshot(ctx context.Context, volumeID, snapshotID string) error {
	if c.mode == "rbd" || c.mode == "local,rbd" {
		return c.CreateSnapshotRBD(ctx, volumeID, snapshotID)
	}
	return nil // Stub behavior for non-RBD modes
}

// DeleteSnapshot deletes a snapshot
func (c *CephClient) DeleteSnapshot(ctx context.Context, volumeID, snapshotID string) error {
	if c.mode == "rbd" || c.mode == "local,rbd" {
		return c.DeleteSnapshotRBD(ctx, volumeID, snapshotID)
	}
	return nil // Stub behavior for non-RBD modes
}

// VolumeExists checks if a volume exists
func (c *CephClient) VolumeExists(ctx context.Context, volumeID string) (bool, error) {
	switch c.mode {
	case "stub":
		c.mu.Lock()
		defer c.mu.Unlock()
		_, exists := c.stubVolumes[volumeID]
		return exists, nil
	case "local":
		return c.volumeExistsLocal(volumeID)
	case "rbd":
		return c.volumeExistsRBD(ctx, volumeID)
	case "local,rbd":
		// Check local first (faster)
		if exists, _ := c.volumeExistsLocal(volumeID); exists {
			return true, nil
		}
		return c.volumeExistsRBD(ctx, volumeID)
	default:
		return false, fmt.Errorf("unsupported storage mode: %s", c.mode)
	}
}

// volumeExistsLocal checks if a local volume exists
func (c *CephClient) volumeExistsLocal(volumeID string) (bool, error) {
	volumePath := filepath.Join(c.localPath, "volume-"+volumeID+".qcow2")
	_, err := os.Stat(volumePath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// GetVolumeSize gets the size of a volume
func (c *CephClient) GetVolumeSize(ctx context.Context, volumeID string) (int, error) {
	if c.mode == "rbd" || c.mode == "local,rbd" {
		return c.GetVolumeSizeRBD(ctx, volumeID)
	}
	return 0, nil // Stub behavior
}

// GetRBDPath returns the RBD path for a volume (for libvirt attachment)
func (c *CephClient) GetRBDPath(volumeID string) string {
	return fmt.Sprintf("rbd:%s/volume-%s", c.pool, volumeID)
}

// Health checks if Ceph cluster is accessible
func (c *CephClient) Health(ctx context.Context) error {
	if c.mode == "rbd" || c.mode == "local,rbd" {
		return c.HealthRBD(ctx)
	}
	return nil // Stub behavior
}
