package nova

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/hypervisor"
	"github.com/cobaltcore-dev/o3k/pkg/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// volumeDiskXML builds the libvirt disk XML for a volume: a local-mode volume is
// a file (its backing qcow2 lives on the node), otherwise an RBD network disk
// (SPEC-009 WS-5).
func volumeDiskXML(volumeID, device, rbdPool, rbdImage string) string {
	if path := storage.LocalVolumePath(volumeID); fileExists(path) {
		return hypervisor.GenerateDiskXML(hypervisor.DiskSpec{Device: device, Type: "file", Source: path})
	}
	return hypervisor.GenerateDiskXML(hypervisor.DiskSpec{
		Device: device, Type: "network", Source: rbdPool + "/" + rbdImage, Protocol: "rbd",
	})
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// VolumeAttachment represents a volume attached to an instance
type VolumeAttachment struct {
	ID         string    `json:"id"`
	VolumeID   string    `json:"volumeId"`
	InstanceID string    `json:"serverId"`
	Device     string    `json:"device"`
	AttachedAt time.Time `json:"attachedAt"`
}

// AttachVolumeRequest represents a volume attachment request
type AttachVolumeRequest struct {
	VolumeAttachment struct {
		VolumeID string `json:"volumeId" binding:"required"`
		Device   string `json:"device"` // Optional, auto-assigned if not provided
	} `json:"volumeAttachment"`
}

// AttachVolume attaches a volume to an instance
func (svc *Service) AttachVolume(c *gin.Context) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	var req AttachVolumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify instance exists and get libvirt domain ID
	var libvirtDomainID string
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT libvirt_domain_id FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&libvirtDomainID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// Verify volume exists and is available
	var volumeStatus string
	var attachedToInstance any
	err = database.DB.QueryRow(c.Request.Context(),
		"SELECT status, attached_to_instance_id FROM volumes WHERE id = $1 AND project_id = $2",
		req.VolumeAttachment.VolumeID, projectID,
	).Scan(&volumeStatus, &attachedToInstance)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}

	if volumeStatus != "available" {
		common.SendError(c, common.NewBadRequestError(fmt.Sprintf("volume status is %s, must be available", volumeStatus)))
		return
	}

	if attachedToInstance != nil {
		common.SendError(c, common.NewBadRequestError("volume is already attached to an instance"))
		return
	}

	// Auto-assign device if not provided
	device := req.VolumeAttachment.Device
	if device == "" {
		device, err = svc.getNextAvailableDevice(c.Request.Context(), instanceID)
		if err != nil {
			log.Error().Err(err).Str("operation", "get_next_device").Msg("device assignment error")
			common.SendError(c, common.NewInternalServerError("failed to assign device"))
			return
		}
	}

	// Create attachment record
	attachmentID := uuid.New().String()
	now := time.Now()

	_, err = database.DB.Exec(c.Request.Context(), `
		INSERT INTO volume_attachments (id, volume_id, instance_id, device, attached_at)
		VALUES ($1, $2, $3, $4, $5)
	`, attachmentID, req.VolumeAttachment.VolumeID, instanceID, device, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_volume_attachment").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create volume attachment"))
		return
	}

	// Update volume status
	_, err = database.DB.Exec(c.Request.Context(), `
		UPDATE volumes
		SET attached_to_instance_id = $1, status = $2, updated_at = $3
		WHERE id = $4
	`, instanceID, "in-use", now, req.VolumeAttachment.VolumeID)

	if err != nil {
		// Rollback attachment (best-effort; the update error is the
		// user-facing one)
		if _, rbErr := database.DB.Exec(c.Request.Context(), "DELETE FROM volume_attachments WHERE id = $1", attachmentID); rbErr != nil {
			log.Warn().Err(rbErr).Str("attachment_id", attachmentID).Msg("rollback: failed to delete volume attachment after volume update failure")
		}
		log.Error().Err(err).Str("operation", "update_volume_status").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update volume status"))
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
		_ = database.DB.QueryRow(ctx,
			"SELECT rbd_pool, rbd_image FROM volumes WHERE id = $1",
			req.VolumeAttachment.VolumeID).Scan(&rbdPool, &rbdImage)
		diskXML := volumeDiskXML(req.VolumeAttachment.VolumeID, device, rbdPool, rbdImage)
		if err := svc.vmManager.AttachDevice(ctx, libvirtDomainID, diskXML); err != nil {
			log.Error().Err(err).Str("volume_id", req.VolumeAttachment.VolumeID).Str("libvirt_domain", libvirtDomainID).Msg("failed to attach volume to VM; reverting")
			if _, e := database.DB.Exec(c.Request.Context(), "DELETE FROM volume_attachments WHERE id = $1", attachmentID); e != nil {
				log.Warn().Err(e).Str("attachment_id", attachmentID).Msg("revert: delete attachment")
			}
			if _, e := database.DB.Exec(c.Request.Context(),
				"UPDATE volumes SET attached_to_instance_id = NULL, status = 'available', updated_at = $1 WHERE id = $2",
				time.Now(), req.VolumeAttachment.VolumeID); e != nil {
				log.Warn().Err(e).Str("volume_id", req.VolumeAttachment.VolumeID).Msg("revert: reset volume status")
			}
			common.SendError(c, common.NewInternalServerError("failed to attach volume to the instance"))
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"volumeAttachment": gin.H{
			"id":         attachmentID,
			"volumeId":   req.VolumeAttachment.VolumeID,
			"serverId":   instanceID,
			"device":     device,
			"attachedAt": now.Format(time.RFC3339),
		},
	})
}

// DetachVolume detaches a volume from an instance
func (svc *Service) DetachVolume(c *gin.Context) {
	instanceID := c.Param("id")
	volumeID := c.Param("volume_id")
	projectID := c.GetString("project_id")

	// Verify instance exists and get libvirt domain ID
	var libvirtDomainID string
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT libvirt_domain_id FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&libvirtDomainID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// Get attachment details
	var attachmentID, device string
	err = database.DB.QueryRow(c.Request.Context(),
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
		err := database.DB.QueryRow(ctx,
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

	// Delete attachment record
	_, err = database.DB.Exec(c.Request.Context(),
		"DELETE FROM volume_attachments WHERE id = $1",
		attachmentID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_volume_attachment").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete volume attachment"))
		return
	}

	// Update volume status
	now := time.Now()
	_, err = database.DB.Exec(c.Request.Context(), `
		UPDATE volumes
		SET attached_to_instance_id = NULL, status = $1, updated_at = $2
		WHERE id = $3
	`, "available", now, volumeID)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_volume_status_detach").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update volume status"))
		return
	}

	c.Status(http.StatusAccepted)
}

// ListVolumeAttachments lists all volumes attached to an instance
func (svc *Service) ListVolumeAttachments(c *gin.Context) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	// Verify instance exists
	var exists bool
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM instances WHERE id = $1 AND project_id = $2)",
		instanceID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// List attachments
	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT id, volume_id, device, attached_at
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
		var id, volumeID, device string
		var attachedAt time.Time

		if err := rows.Scan(&id, &volumeID, &device, &attachedAt); err != nil {
			continue
		}

		attachments = append(attachments, gin.H{
			"id":         id,
			"volumeId":   volumeID,
			"serverId":   instanceID,
			"device":     device,
			"attachedAt": attachedAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
	}

	if attachments == nil {
		attachments = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"volumeAttachments": attachments})
}

// getNextAvailableDevice finds the next available device name (vdb, vdc, etc.)
func (svc *Service) getNextAvailableDevice(ctx context.Context, instanceID string) (string, error) {
	// Query existing devices
	rows, err := database.DB.Query(ctx,
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
			continue
		}
		usedDevices[device] = true
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
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
