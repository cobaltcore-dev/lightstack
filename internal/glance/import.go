package glance

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// StageImageData stages image data before import
func (svc *Service) StageImageData(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	// Verify image exists and is owned by project
	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT status FROM images WHERE id = $1 AND project_id = $2",
		imageID, projectID,
	).Scan(&status)

	if errors.Is(err, database.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"message": "Image not found"})
		return
	}

	if err != nil {
		log.Error().Err(err).Str("image_id", imageID).Msg("image staging database error")
		c.JSON(http.StatusInternalServerError, gin.H{"message": "Failed to stage image data"})
		return
	}

	// Can only stage to queued images
	if status != "queued" {
		c.JSON(http.StatusConflict, gin.H{"message": "Image is not in queued state"})
		return
	}

	// Consume staging data with bounded memory. The staging backend is currently
	// a stub, so only its byte count is retained; the upload cap can be 10 GiB.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, svc.uploadCap)
	stagedSize, err := io.Copy(io.Discard, c.Request.Body)
	if err != nil {
		// Over the cap is 413, upstream's answer to image_size_cap (round 2
		// of this review — it used to read as a 400 read failure).
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"message": "Image data exceeds the maximum upload size"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"message": "Failed to read image data"})
		return
	}

	// Update image status to uploading
	result, err := svc.pool.Exec(c.Request.Context(), `
		UPDATE images
		SET status = $1, size_bytes = $2, updated_at = $3
		WHERE id = $4 AND status = 'queued'
	`, "uploading", stagedSize, time.Now(), imageID)

	if err != nil {
		log.Error().Err(err).Str("image_id", imageID).Msg("image staging database error")
		c.JSON(http.StatusInternalServerError, gin.H{"message": "Failed to stage image data"})
		return
	}
	if result.RowsAffected() == 0 {
		c.JSON(http.StatusConflict, gin.H{"message": "Image is not in queued state"})
		return
	}

	c.Status(http.StatusNoContent)
}

// ImportImage imports staged image data
func (svc *Service) ImportImage(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	// Parse import method from body
	var req struct {
		Method struct {
			Name string `json:"name"`
		} `json:"method"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"message": "Invalid request body"})
		return
	}

	// Verify image exists
	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT status FROM images WHERE id = $1 AND project_id = $2",
		imageID, projectID,
	).Scan(&status)

	if errors.Is(err, database.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"message": "Image not found"})
		return
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
		return
	}

	// Import from staging to active storage
	// In stub mode, just mark as active
	// In real mode, would move from staging to backend storage
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE images
		SET status = $1, updated_at = $2
		WHERE id = $3
	`, "active", time.Now(), imageID)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"message": err.Error()})
		return
	}

	c.Status(http.StatusAccepted)
}

// GetImageImportInfo returns available import methods for an image
func (svc *Service) GetImageImportInfo(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	// Verify image exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM images WHERE id = $1 AND project_id = $2)",
		imageID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"message": "Image not found"})
		return
	}

	// Return available import methods
	c.JSON(http.StatusOK, gin.H{
		"import-methods": gin.H{
			"description": "Import methods available.",
			"type":        "array",
			"value": []string{
				"glance-direct",
				"web-download",
			},
		},
	})
}
