package cinder

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// statusAwaitingTransfer is the state a volume holds while a transfer of it is pending, from create
// until the transfer is accepted or cancelled — upstream's transfer_create, transfer_accept and
// transfer_destroy (cinder/db/api.py). Accept moves a volume only out of this state, so a volume can
// have one pending transfer, and a transfer moves it once.
//
// Nothing held the volume before: create left it available, accept moved it with an unconditional
// UPDATE, and nothing re-checked that the sender still owned it. A sender could create two transfers,
// hand one over, and take the volume back with the other; two accepts of one transfer into different
// projects both succeeded, the last one keeping the volume.
const statusAwaitingTransfer = "awaiting-transfer"

var (
	errVolumeNotAvailable        = errors.New("volume is not available")
	errVolumeNotAwaitingTransfer = errors.New("volume is not awaiting transfer")
	errTransferSizeChanged       = errors.New("volume size changed during the accept")
	errSnapshotsNotAvailable     = errors.New("a snapshot of the volume is not available")
)

// snapshotNotAvailableError refuses a transfer one of whose snapshots is not available. A volume's
// snapshots go with it (see AcceptVolumeTransfer), and upstream refuses the transfer unless every
// one of them is available (cinder/transfer/api.py create).
type snapshotNotAvailableError struct{ snapshotID string }

func (e *snapshotNotAvailableError) Error() string {
	return "snapshot " + e.snapshotID + " is not available"
}

// refuseAwaitingTransfer answers an operation on a volume awaiting transfer, as upstream's
// InvalidVolume does, and reports whether it did. The sender has offered the volume as it is: until
// the transfer is accepted or cancelled it may not be changed, consumed, copied or overwritten.
// Reading it, renaming it and its metadata stay allowed, as upstream.
func refuseAwaitingTransfer(c *gin.Context, volumeRef, status string) bool {
	if status != statusAwaitingTransfer {
		return false
	}
	common.SendError(c, common.NewBadRequestError(
		fmt.Sprintf("Invalid volume: volume %s is awaiting transfer; accept or cancel the transfer first.", volumeRef)))
	return true
}

// CreateVolumeTransfer handles POST /v3/os-volume-transfer
func (svc *Service) CreateVolumeTransfer(c *gin.Context) {
	projectID := c.GetString("project_id")

	var req struct {
		Transfer struct {
			VolumeID string  `json:"volume_id" binding:"required"`
			Name     *string `json:"name"`
		} `json:"transfer" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify volume exists and belongs to project
	var volumeStatus string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT status FROM volumes WHERE id = $1 AND project_id = $2",
		req.Transfer.VolumeID, projectID,
	).Scan(&volumeStatus)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "create_transfer").Msg("failed to query volume")
		common.SendError(c, common.NewInternalServerError("failed to create transfer"))
		return
	}

	// Volume must be available
	if volumeStatus != "available" {
		common.SendError(c, common.NewBadRequestError("volume must be available for transfer"))
		return
	}

	// Generate auth key
	authKeyBytes := make([]byte, 16)
	rand.Read(authKeyBytes)
	authKey := hex.EncodeToString(authKeyBytes)

	transferID := uuid.New().String()
	now := time.Now()
	ctx := c.Request.Context()

	// The volume enters awaiting-transfer in the transaction that records the transfer, and only
	// from available: the check above can race a concurrent create, and this conditional update is
	// what lets one of them through.
	err = database.WithTx(ctx, svc.pool, func(tx pgx.Tx) error {
		tag, uerr := tx.Exec(ctx,
			`UPDATE volumes SET status = $1, updated_at = $2 WHERE id = $3 AND project_id = $4 AND status = 'available'`,
			statusAwaitingTransfer, now, req.Transfer.VolumeID, projectID)
		if uerr != nil {
			return fmt.Errorf("holding the volume for transfer: %w", uerr)
		}
		if tag.RowsAffected() == 0 {
			return errVolumeNotAvailable
		}
		var notReady string
		serr := tx.QueryRow(ctx,
			`SELECT id::text FROM snapshots WHERE volume_id = $1 AND status <> 'available' ORDER BY created_at, id LIMIT 1`,
			req.Transfer.VolumeID).Scan(&notReady)
		if serr == nil {
			return &snapshotNotAvailableError{snapshotID: notReady}
		}
		if !errors.Is(serr, pgx.ErrNoRows) {
			return fmt.Errorf("checking the volume's snapshots: %w", serr)
		}
		if _, ierr := tx.Exec(ctx, `
			INSERT INTO volume_transfers (id, volume_id, name, source_project_id, auth_key, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, transferID, req.Transfer.VolumeID, req.Transfer.Name, projectID, authKey, now, now); ierr != nil {
			return fmt.Errorf("recording the transfer: %w", ierr)
		}
		return nil
	})
	if errors.Is(err, errVolumeNotAvailable) {
		common.SendError(c, common.NewBadRequestError("volume must be available for transfer"))
		return
	}
	var notReady *snapshotNotAvailableError
	if errors.As(err, &notReady) {
		// Upstream's wording (InvalidSnapshot).
		common.SendError(c, common.NewBadRequestError(
			fmt.Sprintf("Invalid snapshot: snapshot: %s status must be available", notReady.snapshotID)))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "create_transfer").Msg("failed to create transfer")
		common.SendError(c, common.NewInternalServerError("failed to create transfer"))
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"transfer": gin.H{
			"id":         transferID,
			"volume_id":  req.Transfer.VolumeID,
			"name":       req.Transfer.Name,
			"auth_key":   authKey,
			"created_at": now.Format(time.RFC3339),
		},
	})
}

// ListVolumeTransfers handles GET /v3/os-volume-transfer
func (svc *Service) ListVolumeTransfers(c *gin.Context) {
	projectID := c.GetString("project_id")

	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, volume_id, name, created_at
		FROM volume_transfers
		WHERE source_project_id = $1 AND accepted = false
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_transfers").Msg("failed to query transfers")
		common.SendError(c, common.NewInternalServerError("failed to list transfers"))
		return
	}
	defer rows.Close()

	transfers := []gin.H{}
	for rows.Next() {
		var id, volumeID string
		var name *string
		var createdAt time.Time

		if err := rows.Scan(&id, &volumeID, &name, &createdAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan transfer row")
			continue
		}

		transfers = append(transfers, gin.H{
			"id":         id,
			"volume_id":  volumeID,
			"name":       name,
			"created_at": createdAt.Format(time.RFC3339),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"transfers": transfers})
}

// ListVolumeTransfersNoProject is an alias for routes without project_id in URL
func (svc *Service) ListVolumeTransfersNoProject(c *gin.Context) {
	svc.ListVolumeTransfers(c)
}

// GetVolumeTransfer handles GET /v3/os-volume-transfer/:id
func (svc *Service) GetVolumeTransfer(c *gin.Context) {
	transferID := c.Param("id")
	projectID := c.GetString("project_id")

	var volumeID string
	var name *string
	var createdAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT volume_id, name, created_at
		FROM volume_transfers
		WHERE id = $1 AND source_project_id = $2 AND accepted = false
	`, transferID, projectID).Scan(&volumeID, &name, &createdAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("transfer"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_transfer").Msg("failed to query transfer")
		common.SendError(c, common.NewInternalServerError("failed to get transfer"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"transfer": gin.H{
			"id":         transferID,
			"volume_id":  volumeID,
			"name":       name,
			"created_at": createdAt.Format(time.RFC3339),
		},
	})
}

// DeleteVolumeTransfer handles DELETE /v3/os-volume-transfer/:id
func (svc *Service) DeleteVolumeTransfer(c *gin.Context) {
	transferID := c.Param("id")
	projectID := c.GetString("project_id")

	// Cancelling gives the volume back as available, in the transaction that deletes the transfer —
	// upstream's transfer_destroy. Only out of awaiting-transfer: a volume an administrator has
	// already taken out of it keeps its status, and the transfer still goes.
	ctx := c.Request.Context()
	err := database.WithTx(ctx, svc.pool, func(tx pgx.Tx) error {
		var volumeID string
		if derr := tx.QueryRow(ctx,
			"DELETE FROM volume_transfers WHERE id = $1 AND source_project_id = $2 AND accepted = false RETURNING volume_id::text",
			transferID, projectID,
		).Scan(&volumeID); derr != nil {
			return derr
		}
		_, uerr := tx.Exec(ctx,
			"UPDATE volumes SET status = 'available', updated_at = $1 WHERE id = $2 AND status = $3",
			time.Now(), volumeID, statusAwaitingTransfer)
		return uerr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("transfer"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_transfer").Msg("failed to delete transfer")
		common.SendError(c, common.NewInternalServerError("failed to delete transfer"))
		return
	}

	c.Status(http.StatusAccepted)
}

// AcceptVolumeTransfer handles POST /v3/os-volume-transfer/:id/accept
func (svc *Service) AcceptVolumeTransfer(c *gin.Context) {
	transferID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Accept struct {
			AuthKey string `json:"auth_key" binding:"required"`
		} `json:"accept" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Get transfer details and verify auth key
	var volumeID, storedAuthKey, sourceProjectID string
	var name *string

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT volume_id, name, auth_key, source_project_id
		FROM volume_transfers
		WHERE id = $1 AND accepted = false
	`, transferID).Scan(&volumeID, &name, &storedAuthKey, &sourceProjectID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("transfer"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "accept_transfer").Msg("failed to query transfer")
		common.SendError(c, common.NewInternalServerError("failed to accept transfer"))
		return
	}

	// Verify auth key
	if req.Accept.AuthKey != storedAuthKey {
		common.SendError(c, common.NewBadRequestError("invalid auth key"))
		return
	}

	// Accepting a transfer PROVISIONS NOTHING, yet the accepting project's count and
	// gigabytes rise by the volume's size — indistinguishable from a create on the
	// receiving side (SPEC-025-followup FR-019). Without this, a project at its limit could
	// still acquire storage from any project willing to hand some over.
	//
	// The refusal obligation is the mirror image of a create's (FR-019a): there is nothing
	// to roll back, so what matters is that nothing is LOST. The transfer stays unaccepted
	// and the volume stays with its sender, which is why this check sits before both
	// updates rather than between them.
	var transferSize int
	if err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT size_gb FROM volumes WHERE id = $1", volumeID).Scan(&transferSize); err != nil {
		log.Error().Err(err).Str("operation", "accept_transfer").Msg("failed to read transferred volume size")
		common.SendError(c, common.NewInternalServerError("failed to accept transfer"))
		return
	}
	// The volume's snapshots go with it, as upstream's transfer_accept moves them by default:
	// they hold the volume's data, and left behind they gave the sender a copy of what it had
	// handed over, counted against its own quota. So the recipient is charged for them too —
	// their count and their size, as upstream's _handle_snapshot_quota charges.
	var snapCount, snapGB int
	if err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COUNT(*), COALESCE(SUM(size_gb), 0) FROM snapshots WHERE volume_id = $1", volumeID,
	).Scan(&snapCount, &snapGB); err != nil {
		log.Error().Err(err).Str("operation", "accept_transfer").Msg("failed to read the transferred volume's snapshots")
		common.SendError(c, common.NewInternalServerError("failed to accept transfer"))
		return
	}
	// Both UPDATEs run under the same lock as the check, so an accept is all-or-nothing
	// and is serialised against a concurrent accept into the same project. Previously the
	// check committed first and the two writes followed unprotected: four concurrent
	// accepts against a one-volume allowance all succeeded, and the project ended up
	// holding four.
	//
	// That lock is the ACCEPTING project's, so it cannot order two accepts into different
	// projects; the volume's own state does. It leaves awaiting-transfer, for the accepting
	// project and user, in one conditional statement — upstream's transfer_accept — so only the
	// first accept finds it there: a concurrent one waits on the row and then matches nothing, and
	// so does a volume an administrator has taken out of the state.
	userID := c.GetString("user_id")
	// withVolumeQuota's checks, plus the snapshots that come along: the per-volume ceiling on the
	// volume alone, then count before size, as quotaWant asks.
	err = checkPerVolumeCeiling(c.Request.Context(), svc.pool, projectID, transferSize)
	if err == nil {
		err = withCinderQuota(c.Request.Context(), svc.pool, projectID, []quotaWant{
			{"volumes", 1},
			{"snapshots", snapCount},
			{"gigabytes", transferSize + snapGB},
		}, func(tx pgx.Tx) error {
			return acceptTransferWrite(c, tx, transferID, volumeID, projectID, userID, transferSize, snapCount)
		})
	}

	if err != nil {
		if sendQuotaError(c, err) {
			return
		}
		switch {
		case errors.Is(err, errVolumeNotAwaitingTransfer):
			// Upstream's wording (transfer_accept's InvalidVolume).
			common.SendError(c, common.NewBadRequestError(fmt.Sprintf(
				"Invalid volume: Transfer %s: Volume id %s expected in awaiting-transfer state.", transferID, volumeID)))
		case errors.Is(err, errSnapshotsNotAvailable):
			// Upstream's InvalidSnapshot, for a snapshot that stopped being available.
			common.SendError(c, common.NewBadRequestError(fmt.Sprintf(
				"Invalid snapshot: Transfer %s: a snapshot of volume %s is not in the expected available state.", transferID, volumeID)))
		case errors.Is(err, errTransferSizeChanged):
			common.SendError(c, common.NewConflictError("the volume changed while the transfer was being accepted; retry"))
		default:
			log.Error().Err(err).Str("operation", "accept_transfer").Msg("failed to accept transfer")
			common.SendError(c, common.NewInternalServerError("failed to accept transfer"))
		}
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"transfer": gin.H{
			"id":        transferID,
			"volume_id": volumeID,
			"name":      name,
		},
	})
}

// acceptTransferWrite moves the volume, its snapshots and the transfer row to the accepting project,
// inside the quota transaction AcceptVolumeTransfer opened.
func acceptTransferWrite(c *gin.Context, tx pgx.Tx, transferID, volumeID, projectID, userID string, transferSize, snapCount int) error {
	ctx := c.Request.Context()
	var size int
	err := tx.QueryRow(ctx, `
		UPDATE volumes
		   SET status = 'available', project_id = $1, user_id = COALESCE(NULLIF($2, '')::uuid, user_id), updated_at = $3
		 WHERE id = $4 AND status = $5
		RETURNING size_gb`,
		projectID, userID, time.Now(), volumeID, statusAwaitingTransfer,
	).Scan(&size)
	if errors.Is(err, pgx.ErrNoRows) {
		return errVolumeNotAwaitingTransfer
	}
	if err != nil {
		return fmt.Errorf("transferring volume ownership: %w", err)
	}
	// The quota was checked against the size read before this transaction. Awaiting
	// transfer holds it still, but only the row read under the lock can confirm that.
	if size != transferSize {
		return errTransferSizeChanged
	}
	// Each snapshot moves only out of available, as upstream's transfer_accept moves it, and
	// all of them or none: one that stopped being available — or a count that changed since
	// the quota was checked — fails the accept rather than splitting the volume from its data.
	tag, err := tx.Exec(ctx,
		"UPDATE snapshots SET project_id = $1 WHERE volume_id = $2 AND status = 'available'",
		projectID, volumeID)
	if err != nil {
		return fmt.Errorf("transferring snapshots: %w", err)
	}
	if tag.RowsAffected() != int64(snapCount) {
		return errSnapshotsNotAvailable
	}
	if _, err := tx.Exec(ctx,
		"UPDATE volume_transfers SET accepted = true, destination_project_id = $1 WHERE id = $2",
		projectID, transferID,
	); err != nil {
		return fmt.Errorf("marking transfer accepted: %w", err)
	}
	return nil
}
