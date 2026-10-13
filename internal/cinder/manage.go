package cinder

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// ManageVolume handles POST /v3/os-volume-manage
func (svc *Service) ManageVolume(c *gin.Context) {
	projectID := c.GetString("project_id")

	var req struct {
		Volume struct {
			Host       string            `json:"host" binding:"required"`
			Ref        map[string]any    `json:"ref" binding:"required"`
			Name       string            `json:"name"`
			VolumeType string            `json:"volume_type"`
			Metadata   map[string]string `json:"metadata"`
		} `json:"volume" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Create volume from existing storage
	volumeID := uuid.New()

	// Default size for managed volumes (would query backend in real implementation)
	sizeGB := 1

	// Importing an unmanaged backing store adds a volume to the project, so it consumes
	// quota exactly as a create does (SPEC-025-followup FR-019). A third of the four
	// volume-adding routes, and one the spec did not know about.
	err := withVolumeQuota(c.Request.Context(), svc.pool, projectID, sizeGB, func(tx database.Tx) error {
		_, ierr := tx.Exec(c.Request.Context(), `
		INSERT INTO volumes (id, name, size_gb, status, project_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, volumeID, req.Volume.Name, sizeGB, "available", projectID, time.Now(), time.Now())
		return ierr
	})

	if err != nil {
		if sendQuotaError(c, err) {
			return
		}
		log.Error().Err(err).Str("operation", "manage_volume").Msg("failed to insert managed volume")
		common.SendError(c, common.NewInternalServerError("failed to manage volume"))
		return
	}

	volume := map[string]any{
		"id":                volumeID.String(),
		"name":              req.Volume.Name,
		"size":              sizeGB,
		"status":            "available",
		"volume_type":       req.Volume.VolumeType,
		"host":              req.Volume.Host,
		"availability_zone": "nova",
		"created_at":        time.Now().Format(time.RFC3339),
	}

	c.JSON(http.StatusAccepted, gin.H{"volume": volume})
}

// ListManageableVolumes handles GET /v3/manageable_volumes
func (svc *Service) ListManageableVolumes(c *gin.Context) {
	host := c.Query("host")

	if host == "" {
		common.SendError(c, common.NewBadRequestError("host parameter is required"))
		return
	}

	// In stub mode, return empty list
	// In real mode, would query storage backend for unmanaged volumes
	manageableVolumes := []map[string]any{}

	c.JSON(http.StatusOK, gin.H{"manageable-volumes": manageableVolumes})
}

// UnmanageVolume handles volume action: os-unmanage
func (svc *Service) UnmanageVolume(c *gin.Context, volumeID string) {
	projectID := c.GetString("project_id")

	// Verify volume exists and belongs to project
	var id uuid.UUID
	var status string

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, status
		FROM volumes
		WHERE id = $1 AND project_id = $2
	`, volumeID, projectID).Scan(&id, &status)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}

	// Cannot unmanage attached volumes
	if status == "in-use" {
		common.SendError(c, common.NewBadRequestError("cannot unmanage volume in use"))
		return
	}
	// A volume still being created is not the caller's to remove (FR-036a): deleting its
	// row mid-create cancels another request's in-flight create and strands its backing.
	if status == "creating" {
		common.SendError(c, common.NewBadRequestError("Invalid volume: Volume status must be available or error or error_restoring or error_extending or error_managing and must not be migrating, attached, belong to a group, have snapshots, awaiting a transfer, or be disassociated from snapshots after volume transfer."))
		return
	}

	// Remove from database (in real implementation, would leave on backend)
	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM volumes WHERE id = $1 AND project_id = $2",
		volumeID, projectID,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "unmanage_volume").Msg("failed to delete volume")
		common.SendError(c, common.NewInternalServerError("failed to unmanage volume"))
		return
	}

	c.Status(http.StatusAccepted)
}

// ManageSnapshot handles POST /v3/os-snapshot-manage
func (svc *Service) ManageSnapshot(c *gin.Context) {
	projectID := c.GetString("project_id")

	var req struct {
		Snapshot struct {
			VolumeID string            `json:"volume_id" binding:"required"`
			Ref      map[string]any    `json:"ref" binding:"required"`
			Name     string            `json:"name"`
			Metadata map[string]string `json:"metadata"`
		} `json:"snapshot" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// A malformed volume id is a volume that does not exist — 404, as upstream and the SQLite
	// backend (TEXT ids) answer — not the PostgreSQL 22P02 the raw lookup would surface as a 500.
	if _, err := uuid.Parse(req.Snapshot.VolumeID); err != nil {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}
	// Verify the volume exists and read its status.
	var volumeStatus string
	err := svc.pool.QueryRow(c.Request.Context(),
		`SELECT status FROM volumes WHERE id = $1 AND project_id = $2`,
		req.Snapshot.VolumeID, projectID).Scan(&volumeStatus)
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "manage_snapshot").Msg("failed to look up the volume")
		common.SendError(c, common.NewInternalServerError("failed to manage snapshot"))
		return
	}
	// A snapshot cannot be imported for a volume whose own backing is still being created
	// (FR-036a).
	if volumeStatus == "creating" {
		common.SendError(c, common.NewBadRequestError(
			fmt.Sprintf("Invalid volume: Volume %s status must be available, but current status is: creating.", req.Snapshot.VolumeID)))
		return
	}

	// Create snapshot from existing storage
	snapshotID := uuid.New()

	// Default size (would query backend in real implementation)
	sizeGB := 1

	// Importing an unmanaged snapshot adds a snapshot to the project, so it consumes the
	// snapshot count and — since FR-026 counts snapshot size toward the allowance — its
	// gigabytes too.
	//
	// THIS SITE WAS NOT IN THE PLAN. The spec named two volume-adding routes, planning
	// re-derived four, and T074's re-grep of the finished code found this ninth
	// quota-consuming site. It is the reason FR-019 requires the list be re-derived rather
	// than trusted: every enumeration of these sites so far has been short by one.
	err = withCinderQuota(c.Request.Context(), svc.pool, projectID,
		[]quotaWant{{"snapshots", 1}, {"gigabytes", sizeGB}},
		func(tx database.Tx) error {
			_, ierr := tx.Exec(c.Request.Context(), `
		INSERT INTO snapshots (id, name, volume_id, size_gb, status, project_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, snapshotID, req.Snapshot.Name, req.Snapshot.VolumeID, sizeGB, "available", projectID, time.Now())
			return ierr
		})

	if err != nil {
		if sendQuotaError(c, err) {
			return
		}
		log.Error().Err(err).Str("operation", "manage_snapshot").Msg("failed to insert managed snapshot")
		common.SendError(c, common.NewInternalServerError("failed to manage snapshot"))
		return
	}

	snapshot := map[string]any{
		"id":         snapshotID.String(),
		"name":       req.Snapshot.Name,
		"volume_id":  req.Snapshot.VolumeID,
		"size":       sizeGB,
		"status":     "available",
		"created_at": time.Now().Format(time.RFC3339),
	}

	c.JSON(http.StatusAccepted, gin.H{"snapshot": snapshot})
}
