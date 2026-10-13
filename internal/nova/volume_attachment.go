package nova

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// volumeDisk is a volume as a guest disk at device: a local-mode volume is a file (its
// backing qcow2 lives on the node), otherwise an RBD network disk (SPEC-009 WS-5).
//
// The one selection every path that gives a guest a volume makes — attach, boot from a
// volume, and self-heal — so none of them can pick a different backing for the same volume.
func volumeDisk(volumeID, device, rbdPool, rbdImage string) hypervisor.DiskSpec {
	if path := storage.LocalVolumePath(volumeID); fileExists(path) {
		return hypervisor.DiskSpec{Device: device, Type: "file", Source: path}
	}
	return hypervisor.DiskSpec{Device: device, Type: "network", Source: rbdPool + "/" + rbdImage, Protocol: "rbd"}
}

// volumeDiskXML builds the libvirt disk XML for a volume (see volumeDisk).
func volumeDiskXML(volumeID, device, rbdPool, rbdImage string) string {
	return hypervisor.GenerateDiskXML(volumeDisk(volumeID, device, rbdPool, rbdImage))
}

// rootDevice is where a server booted from a volume has that volume: the boot path records
// the attachment there, and self-heal looks for it there.
const rootDevice = "/dev/vda"

// volumeRootDiskPath is a volume as the guest's root disk, in VMSpec.ImagePath's terms: the
// node-local file, or rbd:pool/image (see volumeDisk).
func volumeRootDiskPath(volumeID, rbdPool, rbdImage string) string {
	d := volumeDisk(volumeID, rootDevice, rbdPool, rbdImage)
	if d.Type == "file" {
		return d.Source
	}
	return "rbd:" + d.Source
}

// fileExists reports whether a node-local file is present.
//
// A package variable rather than a plain function so paths under /var/lib/lightstack can be
// simulated in tests. paths.Base is a hardcoded constant with no override —
// deliberately, since SPEC-011 removed the old home-directory fallback — so a non-root
// test run cannot stage a real backing file, and the alternative was to leave the
// boot-from-volume and self-heal paths covered only by skipped tests.
var fileExists = func(p string) bool { _, err := os.Stat(p); return err == nil }

// VolumeAttachment represents a volume attached to an instance
type VolumeAttachment struct {
	ID         string    `json:"id"`
	VolumeID   string    `json:"volumeId"`
	InstanceID string    `json:"serverId"`
	Device     string    `json:"device"`
	AttachedAt time.Time `json:"attachedAt"`
}

// novaAttachRefusal is a client-facing attach refusal (wrong volume state,
// already attached) carried out of the attach transaction so the handler can
// answer with the right status instead of a generic 500. Mirrors cinder's
// attachRefusal.
type novaAttachRefusal struct {
	status  int
	message string
}

func (e *novaAttachRefusal) Error() string { return e.message }

var errNovaVolumeNotFound = errors.New("volume not found")
var errNovaAttachmentNotFound = errors.New("volume attachment not found")

// AttachVolumeRequest represents a volume attachment request
type AttachVolumeRequest struct {
	VolumeAttachment struct {
		VolumeID string `json:"volumeId" binding:"required"`
		Device   string `json:"device"` // Optional, auto-assigned if not provided
	} `json:"volumeAttachment"`
}

// AttachVolume attaches a volume to an instance
func (svc *Service) AttachVolume(c *gin.Context) {
	instanceID, projectID, authorized := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !authorized {
		return
	}

	var req AttachVolumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify instance exists and get libvirt domain ID
	var libvirtDomainID string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(libvirt_domain_id, '') FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&libvirtDomainID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("volume attachment: instance lookup failed")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Verify volume exists and is available, create the attachment and flip
	// the volume to in-use — all in one transaction with the volume row
	// locked (review 2026-10-13 N-9): the old check-then-act let the loser of
	// a concurrent double-attach surface as a 500 from the UNIQUE constraint,
	// and a failure between the INSERT and the UPDATE left the volume row
	// inconsistent with its attachment. Mirrors cinder's attach CAS.
	var device string
	attachmentID := uuid.NewString()
	var now time.Time
	err = database.WithTx(c.Request.Context(), svc.pool, func(tx pgx.Tx) error {
		var volumeStatus string
		var attachedToInstance any
		if err := tx.QueryRow(c.Request.Context(),
			"SELECT status, attached_to_instance_id FROM volumes WHERE id = $1 AND project_id = $2 FOR UPDATE",
			req.VolumeAttachment.VolumeID, projectID,
		).Scan(&volumeStatus, &attachedToInstance); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNovaVolumeNotFound
			}
			return err
		}
		if volumeStatus != "available" {
			return &novaAttachRefusal{http.StatusBadRequest,
				fmt.Sprintf("volume status is %s, must be available", volumeStatus)}
		}
		if attachedToInstance != nil {
			return &novaAttachRefusal{http.StatusConflict, "volume is already attached to an instance"}
		}

		// Auto-assign device if not provided
		device = req.VolumeAttachment.Device
		if device == "" {
			device, err = svc.getNextAvailableDevice(c.Request.Context(), instanceID)
			if err != nil {
				return fmt.Errorf("device assignment: %w", err)
			}
		}

		now = time.Now()
		if _, err := tx.Exec(c.Request.Context(), `
			INSERT INTO volume_attachments (id, volume_id, instance_id, device, attached_at)
			VALUES ($1, $2, $3, $4, $5)
		`, attachmentID, req.VolumeAttachment.VolumeID, instanceID, device, now); err != nil {
			return err
		}
		if _, err := tx.Exec(c.Request.Context(), `
			UPDATE volumes
			SET attached_to_instance_id = $1, status = $2, updated_at = $3
			WHERE id = $4
		`, instanceID, "in-use", now, req.VolumeAttachment.VolumeID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		var refusal *novaAttachRefusal
		if errors.As(err, &refusal) {
			if refusal.status == http.StatusConflict {
				common.SendError(c, common.NewConflictError(refusal.message))
			} else {
				common.SendError(c, common.NewBadRequestError(refusal.message))
			}
			return
		}
		if errors.Is(err, errNovaVolumeNotFound) {
			common.SendError(c, common.NewNotFoundError("volume"))
			return
		}
		log.Error().Err(err).Str("operation", "attach_volume_tx").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to attach volume"))
		return
	}

	// Attach the volume to the running VM synchronously (fold the libvirt call
	// into the request path, fail-fast): on libvirt failure, revert the DB
	// attachment so the API never reports a success libvirt did not perform
	// (SPEC-009 WS-5 / T039).
	if svc.vmManager != nil && libvirtDomainID != "" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()
		var rbdPool, rbdImage string
		_ = svc.pool.QueryRow(ctx,
			"SELECT rbd_pool, rbd_image FROM volumes WHERE id = $1",
			req.VolumeAttachment.VolumeID).Scan(&rbdPool, &rbdImage)
		diskXML := volumeDiskXML(req.VolumeAttachment.VolumeID, device, rbdPool, rbdImage)
		if err := svc.vmManager.AttachDevice(ctx, libvirtDomainID, diskXML); err != nil {
			log.Error().Err(err).Str("volume_id", req.VolumeAttachment.VolumeID).Str("libvirt_domain", libvirtDomainID).Msg("failed to attach volume to VM; reverting")
			if _, e := svc.pool.Exec(c.Request.Context(), "DELETE FROM volume_attachments WHERE id = $1", attachmentID); e != nil {
				log.Warn().Err(e).Str("attachment_id", attachmentID).Msg("revert: delete attachment")
			}
			if _, e := svc.pool.Exec(c.Request.Context(),
				"UPDATE volumes SET attached_to_instance_id = NULL, status = 'available', updated_at = $1 WHERE id = $2",
				time.Now(), req.VolumeAttachment.VolumeID); e != nil {
				log.Warn().Err(e).Str("volume_id", req.VolumeAttachment.VolumeID).Msg("revert: reset volume status")
			}
			common.SendError(c, common.NewInternalServerError("failed to attach volume to the instance"))
			return
		}
	}

	// Recorded against the owning project once all of it has happened, so the
	// tenant sees an administrator's change in their own history (SPEC-023 FR-007a).
	recordInstanceAction(c, svc.pool, instanceID, projectID, "attach_volume")

	c.JSON(http.StatusOK, gin.H{
		"volumeAttachment": gin.H{
			// Nova convention: the attachment id IS the volume id — clients (CLI,
			// gophercloud, Terraform) read back and detach by this id. The internal
			// row uuid stays in the DB (SPEC-009 / SC-007).
			"id":         req.VolumeAttachment.VolumeID,
			"volumeId":   req.VolumeAttachment.VolumeID,
			"serverId":   instanceID,
			"device":     device,
			"attachedAt": now.Format(time.RFC3339),
		},
	})
}

// GetVolumeAttachment returns a single volume attachment. Per the Nova API the
// attachment id IS the volume id; the Terraform provider reads the attachment
// back by that id after create (SPEC-009 / SC-007).
func (svc *Service) GetVolumeAttachment(c *gin.Context) {
	instanceID, _, authorized := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !authorized {
		return
	}
	volumeID := c.Param("volume_id")

	var device string
	var attachedAt time.Time
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT device, attached_at FROM volume_attachments WHERE volume_id = $1 AND instance_id = $2",
		volumeID, instanceID,
	).Scan(&device, &attachedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume attachment"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_volume_attachment").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get volume attachment"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"volumeAttachment": gin.H{
			"id":         volumeID,
			"volumeId":   volumeID,
			"serverId":   instanceID,
			"device":     device,
			"attachedAt": attachedAt.Format(time.RFC3339),
		},
	})
}

// DetachVolume detaches a volume from an instance
func (svc *Service) DetachVolume(c *gin.Context) {
	instanceID, projectID, authorized := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !authorized {
		return
	}
	volumeID := c.Param("volume_id")

	// Verify instance exists and get libvirt domain ID
	var libvirtDomainID string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(libvirt_domain_id, '') FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&libvirtDomainID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("volume attachment: instance lookup failed")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Get attachment details
	var attachmentID, device string
	err = svc.pool.QueryRow(c.Request.Context(),
		"SELECT id, device FROM volume_attachments WHERE volume_id = $1 AND instance_id = $2",
		volumeID, instanceID,
	).Scan(&attachmentID, &device)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume attachment"))
		return
	}

	// Detach from hypervisor first
	if svc.vmManager != nil && libvirtDomainID != "" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()

		// Get volume details for disk XML
		var rbdPool, rbdImage string
		err := svc.pool.QueryRow(ctx,
			"SELECT rbd_pool, rbd_image FROM volumes WHERE id = $1",
			volumeID,
		).Scan(&rbdPool, &rbdImage)

		if err == nil {
			diskXML := volumeDiskXML(volumeID, device, rbdPool, rbdImage)

			if err := svc.vmManager.DetachDevice(ctx, libvirtDomainID, diskXML); err != nil {
				log.Error().Err(err).Str("operation", "detach_disk_libvirt").Msg("libvirt error")
				common.SendError(c, common.NewInternalServerError("failed to detach disk"))
				return
			}
		}
	}

	// Delete the attachment and release the volume in one transaction (review
	// 2026-10-13 N-10): as two statements, a failure after the DELETE left the
	// volume stranded in-use with no attachment row — unfixable through the
	// API, since detach could no longer find the attachment to remove.
	now := time.Now()
	err = database.WithTx(c.Request.Context(), svc.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(c.Request.Context(),
			"DELETE FROM volume_attachments WHERE id = $1",
			attachmentID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNovaAttachmentNotFound
		}
		_, err = tx.Exec(c.Request.Context(), `
			UPDATE volumes
			SET attached_to_instance_id = NULL, status = $1, updated_at = $2
			WHERE id = $3
		`, "available", now, volumeID)
		return err
	})
	if err != nil {
		if errors.Is(err, errNovaAttachmentNotFound) {
			common.SendError(c, common.NewNotFoundError("volume attachment"))
			return
		}
		log.Error().Err(err).Str("operation", "detach_volume_tx").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to detach volume"))
		return
	}

	recordInstanceAction(c, svc.pool, instanceID, projectID, "detach_volume")
	c.Status(http.StatusAccepted)
}

// ListVolumeAttachments lists all volumes attached to an instance
func (svc *Service) ListVolumeAttachments(c *gin.Context) {
	instanceID, _, authorized := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !authorized {
		return
	}

	// List attachments
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT volume_id, device, attached_at
		FROM volume_attachments
		WHERE instance_id = $1
		ORDER BY attached_at
	`, instanceID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_volume_attachments").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list volume attachments"))
		return
	}
	defer rows.Close()

	var attachments []gin.H
	for rows.Next() {
		var volumeID, device string
		var attachedAt time.Time

		if err := rows.Scan(&volumeID, &device, &attachedAt); err != nil {
			log.Warn().Err(err).Str("operation", "ListVolumeAttachments").Msg("failed to scan row")
			continue
		}

		attachments = append(attachments, gin.H{
			"id":         volumeID,
			"volumeId":   volumeID,
			"serverId":   instanceID,
			"device":     device,
			"attachedAt": attachedAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list volume attachments"))
		return
	}

	if attachments == nil {
		attachments = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"volumeAttachments": attachments})
}

// volumesAttachedForServers reads the volumes attached to each server, keyed by server id,
// for os-extended-volumes:volumes_attached on server show and detail (SPEC-020 FR-027).
// Every attachment is a row in volume_attachments, the boot volume's included, so the list
// is what upstream reports: {id, delete_on_termination} per volume, in the order they were
// attached. delete_on_termination is part of the entry from microversion 2.3; Lightstack
// answers every request with its current microversion.
func volumesAttachedForServers(ctx context.Context, pool *pgxpool.Pool, serverIDs []string) (map[string][]gin.H, error) {
	result := make(map[string][]gin.H, len(serverIDs))
	if len(serverIDs) == 0 {
		return result, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT instance_id::text, volume_id::text, delete_on_termination
		FROM volume_attachments
		WHERE instance_id = ANY($1::uuid[])
		ORDER BY attached_at, device
	`, serverIDs)
	if err != nil {
		return nil, fmt.Errorf("query attached volumes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var serverID, volumeID string
		var dot bool
		if err := rows.Scan(&serverID, &volumeID, &dot); err != nil {
			return nil, fmt.Errorf("scan attached volume: %w", err)
		}
		result[serverID] = append(result[serverID], gin.H{"id": volumeID, "delete_on_termination": dot})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attached volumes: %w", err)
	}
	return result, nil
}

// attachedVolumes renders a server's attached volumes, an empty list rather than null when
// it has none, as upstream does.
func attachedVolumes(vols []gin.H) []gin.H {
	if vols == nil {
		return []gin.H{}
	}
	return vols
}

// getNextAvailableDevice finds the next available device name (vdb, vdc, etc.)
func (svc *Service) getNextAvailableDevice(ctx context.Context, instanceID string) (string, error) {
	// Query existing devices
	rows, err := svc.pool.Query(ctx,
		"SELECT device FROM volume_attachments WHERE instance_id = $1",
		instanceID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	usedDevices := make(map[string]bool)
	for rows.Next() {
		var device string
		if err := rows.Scan(&device); err != nil {
			log.Warn().Err(err).Str("operation", "getNextAvailableDevice").Msg("failed to scan row")
			continue
		}
		usedDevices[device] = true
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate volume attachments for instance %s: %w", instanceID, err)
	}

	// Find first available device (vdb, vdc, ..., vdz)
	for i := 'b'; i <= 'z'; i++ {
		device := fmt.Sprintf("/dev/vd%c", i)
		if !usedDevices[device] {
			return device, nil
		}
	}

	return "", fmt.Errorf("no available device slots")
}
