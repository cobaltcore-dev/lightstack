package nova

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
)

// Suspend suspends a running instance (saves RAM to disk)
// guestDomain reads a scanned libvirt_domain_id, treating NULL as "no guest recorded".
//
// The column is nullable with no default and the create INSERT omits it, so EVERY server
// has it NULL until a guest is defined — forever in stub mode, and in real mode until the
// goroutine that calls libvirt writes it back. Scanning it into a plain string made pgx
// refuse the row outright ("cannot scan NULL into *string"), so an action against a
// perfectly ordinary server answered 500 (cloud-storage#547, found by the R10 contract
// run on the real-mode host).
//
// lifecycleAction has always handled this, by scanning into `any`. A pointer says the same
// thing with the nullability visible in the type, and gives the three handlers below one
// place to agree on what NULL means.
func guestDomain(scanned *string) string {
	if scanned == nil {
		return ""
	}
	return *scanned
}

func (svc *Service) SuspendInstance(c *gin.Context, instanceID, projectID string) {

	// Get instance and libvirt domain ID
	var domainScan *string
	var status string
	var powerState int
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(libvirt_domain_id, ''), status, power_state FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&domainScan, &status, &powerState)
	libvirtDomainID := guestDomain(domainScan)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Can only suspend an active instance
	if status != "ACTIVE" {
		common.SendError(c, common.NewInvalidStateError(fmt.Sprintf("cannot suspend instance in %s state", status)))
		return
	}

	// Suspending takes the guest's console away, so outstanding links must not
	// survive it (SPEC-021 FR-014).
	if !svc.revokeConsoleTokens(c, instanceID, "instance suspended") {
		return
	}

	// Update status to SUSPENDED
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE instances
		SET status = $1, power_state = $2, task_state = $3, updated_at = $4
		WHERE id = $5
	`, "SUSPENDED", 4, "", time.Now(), instanceID) // power_state 4 = SUSPENDED

	if err != nil {
		log.Error().Err(err).Str("operation", "suspend_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to suspend instance"))
		return
	}

	// Suspend VM in libvirt (asynchronously)
	if svc.vmManager != nil && libvirtDomainID != "" {
		svc.wg.Add(1)
		go func() {
			defer svc.wg.Done()
			ctx, cancel := context.WithTimeout(svc.ctx, 30*time.Second)
			defer cancel()

			if err := svc.vmManager.SuspendVM(ctx, libvirtDomainID); err != nil {
				svc.markActionFailed(instanceID, "suspend", err)
			}
		}()
	}

	c.Status(http.StatusAccepted)
}

// Resume resumes a suspended instance
func (svc *Service) ResumeInstance(c *gin.Context, instanceID, projectID string) {

	// Get instance and libvirt domain ID
	var domainScan *string
	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(libvirt_domain_id, ''), status FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&domainScan, &status)
	libvirtDomainID := guestDomain(domainScan)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Can only resume a suspended instance
	if status != "SUSPENDED" {
		common.SendError(c, common.NewInvalidStateError(fmt.Sprintf("cannot resume instance in %s state", status)))
		return
	}

	// Resuming can bring the guest back on a different console socket than it had
	// before (SPEC-021 FR-014).
	if !svc.revokeConsoleTokens(c, instanceID, "instance resumed") {
		return
	}

	// Update status to ACTIVE
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE instances
		SET status = $1, power_state = $2, task_state = $3, updated_at = $4
		WHERE id = $5
	`, "ACTIVE", 1, "", time.Now(), instanceID) // power_state 1 = RUNNING

	if err != nil {
		log.Error().Err(err).Str("operation", "resume_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to resume instance"))
		return
	}

	// Resume VM in libvirt (asynchronously)
	if svc.vmManager != nil && libvirtDomainID != "" {
		svc.wg.Add(1)
		go func() {
			defer svc.wg.Done()
			ctx, cancel := context.WithTimeout(svc.ctx, 30*time.Second)
			defer cancel()

			if err := svc.vmManager.ResumeVM(ctx, libvirtDomainID); err != nil {
				svc.markActionFailed(instanceID, "resume", err)
			}
		}()
	}

	c.Status(http.StatusAccepted)
}

// markActionFailed records an action that libvirt failed after its 202 — suspend, resume,
// shelve: the cause in the log, the only place it can still be named (e.g. a console socket
// directory that is not ready, FR-017a), and the server in ERROR, so the record never claims a
// state the guest is not in. A failed rollback is logged too: the record then disagrees with
// libvirt and needs an operator.
func (svc *Service) markActionFailed(instanceID, action string, err error) {
	log.Error().Err(err).Str("instance_id", instanceID).Msg(action + " failed in libvirt")
	ctx, cancel := context.WithTimeout(svc.ctx, 5*time.Second)
	defer cancel()
	if _, dbErr := svc.pool.Exec(ctx,
		"UPDATE instances SET status = $1, task_state = $2 WHERE id = $3",
		"ERROR", "", instanceID); dbErr != nil {
		log.Error().Err(dbErr).Str("instance_id", instanceID).Msg("failed to mark instance ERROR after " + action + " failure")
	}
}

// Shelve shelves an instance (shutdown and save disk image)
func (svc *Service) ShelveInstance(c *gin.Context, instanceID, projectID string) {

	// Get instance
	var domainScan *string
	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(libvirt_domain_id, ''), status FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&domainScan, &status)
	libvirtDomainID := guestDomain(domainScan)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Can only shelve an active or stopped instance
	if status != "ACTIVE" && status != "SHUTOFF" {
		common.SendError(c, common.NewInvalidStateError(fmt.Sprintf("cannot shelve instance in %s state", status)))
		return
	}

	// Shelving stops the guest and takes its console away, so outstanding links
	// must not survive it (SPEC-021 FR-014).
	if !svc.revokeConsoleTokens(c, instanceID, "instance shelved") {
		return
	}

	// Create snapshot before shelving. instance_snapshots has
	// UNIQUE(instance_id), so a stale row from a crashed shelving — or from
	// an unshelve path that predates snapshot consumption — would fail this
	// insert with 500. Reclaim any leftover first: at this point the instance
	// is ACTIVE/SHUTOFF and about to be SHELVED, so no live snapshot of it
	// can exist (SPEC-031: shelve → unshelve → shelve was a 500).
	snapshotName := fmt.Sprintf("shelved-%s-%d", instanceID[:8], time.Now().Unix())

	_, err = svc.pool.Exec(c.Request.Context(), `
		DELETE FROM instance_snapshots WHERE instance_id = $1
	`, instanceID)
	if err != nil {
		log.Error().Err(err).Str("operation", "reclaim_shelve_snapshot").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create snapshot"))
		return
	}

	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO instance_snapshots (id, instance_id, snapshot_name, flavor_id, created_at)
		SELECT gen_random_uuid(), $1, $2, flavor_id, $3
		FROM instances WHERE id = $1
	`, instanceID, snapshotName, time.Now())

	if err != nil {
		log.Error().Err(err).Str("operation", "create_shelve_snapshot").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create snapshot"))
		return
	}

	// Update status to SHELVED
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE instances
		SET status = $1, power_state = $2, task_state = $3, updated_at = $4
		WHERE id = $5
	`, "SHELVED", 0, "", time.Now(), instanceID) // power_state 0 = NO STATE

	if err != nil {
		log.Error().Err(err).Str("operation", "shelve_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to shelve instance"))
		return
	}

	// Stop and undefine VM in libvirt (asynchronously)
	if svc.vmManager != nil && libvirtDomainID != "" {
		svc.wg.Add(1)
		go func() {
			defer svc.wg.Done()
			// Wide enough for the guest's whole grace period plus the forced
			// power-off: a stop now waits for the transition rather than firing a
			// request at the guest and returning, so a ceiling below the grace
			// period would cancel the wait and hard-kill every slow guest.
			grace := svc.guestShutdownGrace()
			ctx, cancel := context.WithTimeout(svc.ctx, grace+common.LifecycleResponseSlack)
			defer cancel()

			// A guest that could not be stopped is not shelved.
			if err := svc.vmManager.StopVM(ctx, libvirtDomainID, grace); err != nil {
				svc.markActionFailed(instanceID, "shelve", err)
			}
			// In real mode, would also snapshot disk and delete instance
		}()
	}

	c.Status(http.StatusAccepted)
}

// Unshelve restores a shelved instance
func (svc *Service) UnshelveInstance(c *gin.Context, instanceID, projectID string) {

	// Get instance
	var domainScan *string
	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COALESCE(libvirt_domain_id, ''), status FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&domainScan, &status)
	libvirtDomainID := guestDomain(domainScan)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Can only unshelve a shelved instance
	if status != "SHELVED" {
		common.SendError(c, common.NewInvalidStateError(fmt.Sprintf("cannot unshelve instance in %s state", status)))
		return
	}

	// Restart the guest shelve stopped — BEFORE the record claims ACTIVE. The
	// state reconciler exists to correct records that disagree with the
	// hypervisor, so a record written ACTIVE over a stopped guest was flipped
	// back to SHUTOFF within one 30s sweep: `server unshelve` observably did
	// nothing (SPEC-031). Starting first also wins the reconciler's
	// compare-and-set by construction: a SHELVED row is never a reconcile
	// candidate, and once the guest is up no verdict disagrees with ACTIVE.
	// A domain that has gone missing since the shelve self-heals the way
	// os-start does (FR-025) rather than answering 202 over nothing.
	healedDomain := ""
	if svc.vmManager != nil && libvirtDomainID != "" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), common.LifecycleResponseSlack)
		defer cancel()
		if err := svc.vmManager.StartVM(ctx, libvirtDomainID); err != nil {
			if !errors.Is(err, hypervisor.ErrDomainNotFound) {
				sendLifecycleError(c, "unshelve", err)
				return
			}
			var herr error
			healedDomain, herr = svc.reviveGuest(ctx, instanceID)
			if herr != nil {
				// The record must not go on claiming the server runs (FR-025c).
				svc.persistRunState(c, instanceID, projectID, "ERROR", 0, "")
				log.Error().Err(herr).Str("instance_id", instanceID).
					Msg("unshelve self-heal failed; the server has no guest and could not be given one")
				common.SendError(c, common.NewOperationConflictError("unshelve",
					"the server's guest is absent and could not be re-created"))
				return
			}
			svc.recordSelfHeal(c, instanceID, projectID, healedDomain)
		}
	}

	// Update status to ACTIVE. A self-healed guest runs under a NEW domain
	// identifier — the row must name it, or the reconciler selects the row,
	// asks about the dead domain, and flips ACTIVE back to SHUTOFF within one
	// sweep: the very bug this restart exists to fix, reborn on the self-heal
	// path. persistRunState does the same for os-start's heal (FR-025).
	err = database.WithTx(c.Request.Context(), svc.pool, func(tx database.Tx) error {
		if healedDomain != "" {
			_, err = tx.Exec(c.Request.Context(), `
			UPDATE instances
			SET status = $1, power_state = $2, task_state = $3, updated_at = $4, libvirt_domain_id = $5
			WHERE id = $6
		`, "ACTIVE", 1, "", time.Now(), healedDomain, instanceID)
		} else {
			_, err = tx.Exec(c.Request.Context(), `
			UPDATE instances
			SET status = $1, power_state = $2, task_state = $3, updated_at = $4
			WHERE id = $5
		`, "ACTIVE", 1, "", time.Now(), instanceID)
		}

		if err != nil {
			return err
		}
		_, err = tx.Exec(c.Request.Context(), "DELETE FROM instance_snapshots WHERE instance_id = $1", instanceID)
		return err
	})

	if err != nil {
		log.Error().Err(err).Str("operation", "unshelve_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to unshelve instance"))
		return
	}

	// In real mode, would restore from snapshot and start VM
	// For stub mode, just update DB

	c.Status(http.StatusAccepted)
}

// ResizeInstanceAction handles resize from the action API.
//
// Takes the resolved reference like every other dispatched action (SPEC-025 FR-029):
// it used to read the raw path parameter, so a server addressed by name reached a
// uuid-typed query and failed the cast.
func (svc *Service) ResizeInstanceAction(c *gin.Context, resizeData any, instanceID, projectID string) {

	// Parse the already-parsed action body
	resizeMap, ok := resizeData.(map[string]any)
	if !ok {
		common.SendError(c, common.NewBadRequestError("invalid resize request"))
		return
	}

	flavorRef, ok := resizeMap["flavorRef"].(string)
	if !ok {
		common.SendError(c, common.NewBadRequestError("flavorRef is required"))
		return
	}

	svc.resizeInstance(c, instanceID, projectID, flavorRef)
}

// resizeInstance is the shared resize logic
func (svc *Service) resizeInstance(c *gin.Context, instanceID, projectID, flavorRef string) {

	// Get instance
	var status, currentFlavorID string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT status, flavor_id FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&status, &currentFlavorID)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Can only resize a stopped instance (for simplicity)
	if status != "SHUTOFF" && status != "ACTIVE" {
		common.SendError(c, common.NewInvalidStateError(fmt.Sprintf("cannot resize instance in %s state", status)))
		return
	}

	// Verify new flavor exists
	var newFlavorID string
	err = svc.pool.QueryRow(c.Request.Context(),
		"SELECT id FROM flavors WHERE id::text = $1 OR name = $1 LIMIT 1",
		flavorRef,
	).Scan(&newFlavorID)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("flavor"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Don't allow resizing to same flavor
	if newFlavorID == currentFlavorID {
		common.SendError(c, common.NewBadRequestError("new flavor is same as current flavor"))
		return
	}

	// Update instance with new flavor
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE instances
		SET flavor_id = $1, status = $2, task_state = $3, updated_at = $4
		WHERE id = $5
	`, newFlavorID, "VERIFY_RESIZE", "resize_prep", time.Now(), instanceID)

	if err != nil {
		log.Error().Err(err).Str("operation", "resize_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to resize instance"))
		return
	}

	// In real mode, would rebuild VM with new flavor
	// For stub mode, auto-confirm after 5 seconds
	svc.wg.Add(1)
	go func() {
		defer svc.wg.Done()
		select {
		case <-time.After(5 * time.Second):
		case <-svc.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(svc.ctx, 5*time.Second)
		defer cancel()
		if _, err := svc.pool.Exec(ctx,
			"UPDATE instances SET status = $1, task_state = $2 WHERE id = $3",
			"ACTIVE", "", instanceID); err != nil {
			log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to auto-confirm resize")
		}
	}()

	c.Status(http.StatusAccepted)
}

// ConfirmResize confirms a resize operation
func (svc *Service) ConfirmResizeInstance(c *gin.Context, instanceID, projectID string) {

	// Update status to ACTIVE
	result, err := svc.pool.Exec(c.Request.Context(), `
		UPDATE instances
		SET status = $1, task_state = $2, updated_at = $3
		WHERE id = $4 AND project_id = $5 AND status = $6
	`, "ACTIVE", "", time.Now(), instanceID, projectID, "VERIFY_RESIZE")

	if err != nil {
		log.Error().Err(err).Str("operation", "confirm_resize").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to confirm resize"))
		return
	}

	rowsAffected := result.RowsAffected()
	if rowsAffected == 0 {
		common.SendError(c, common.NewInvalidStateError("instance not in resize-verify state"))
		return
	}

	c.Status(http.StatusNoContent)
}

// RevertResize reverts a resize operation
func (svc *Service) RevertResizeInstance(c *gin.Context, instanceID, projectID string) {

	// Get instance
	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT status FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&status)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	if status != "VERIFY_RESIZE" {
		common.SendError(c, common.NewInvalidStateError("instance not in resize-verify state"))
		return
	}

	// In real mode, would revert to old flavor
	// For stub mode, just set back to ACTIVE
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE instances
		SET status = $1, task_state = $2, updated_at = $3
		WHERE id = $4
	`, "ACTIVE", "", time.Now(), instanceID)

	if err != nil {
		log.Error().Err(err).Str("operation", "revert_resize").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to revert resize"))
		return
	}

	c.Status(http.StatusAccepted)
}

// EvacuateInstance handles evacuating an instance from a failed host.
//
// Administrator-only, and therefore NOT scoped to the administrator's own
// project: evacuating the instances of a failed host is the entire purpose, and
// those instances belong to tenants. Before SPEC-023 this gated on the admin
// role and then looked the instance up `AND project_id = $2` against the
// caller's project, so it answered 404 for every instance the administrator did
// not personally own — unusable for the situation it exists to handle
// (FR-008a).
func (svc *Service) EvacuateInstance(c *gin.Context, instanceID, _ string) {

	// Admin-only operation - check roles
	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("Policy doesn't allow evacuate to be performed"))
		return
	}

	instanceUUID, _, err := targetInstance(c, svc.pool, instanceID)
	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE instances SET status = $1, task_state = $2, updated_at = $3
		WHERE id = $4
	`, "ACTIVE", "evacuating", time.Now(), instanceUUID)

	if err != nil {
		log.Error().Err(err).Str("operation", "evacuate_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to evacuate instance"))
		return
	}

	c.Status(http.StatusOK)
}

// MigrateInstance handles cold migration
func (svc *Service) MigrateInstance(c *gin.Context, instanceID, projectID string) {

	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT status FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&status)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	if status != "ACTIVE" && status != "SHUTOFF" {
		common.SendError(c, common.NewInvalidStateError(fmt.Sprintf("cannot migrate instance in %s state", status)))
		return
	}

	// In stub mode, simulate hosts (compute-1, compute-2)
	// In real mode, would query compute_nodes table for available hosts
	currentHost := "compute-1"
	destHost := "compute-2"

	// Create migration record
	migrationID := uuid.New().String()
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO server_migrations (id, server_uuid, source_node, dest_node, migration_type, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'migration', 'running', $5, $5)
	`, migrationID, instanceID, currentHost, destHost, time.Now())

	if err != nil {
		log.Error().Err(err).Str("operation", "create_migration_record").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create migration"))
		return
	}

	// Update instance to set migrating state
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE instances SET task_state = 'migrating', updated_at = $1
		WHERE id = $2
	`, time.Now(), instanceID)

	if err != nil {
		log.Error().Err(err).Str("operation", "migrate_instance_update").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to start migration"))
		return
	}

	// Background goroutine to complete migration after 5 seconds (stub mode)
	svc.wg.Add(1)
	go func() {
		defer svc.wg.Done()
		select {
		case <-time.After(5 * time.Second):
		case <-svc.ctx.Done():
			return
		}

		ctx, cancel := context.WithTimeout(svc.ctx, 5*time.Second)
		defer cancel()

		// Clear task_state and mark migration as complete
		if _, err := svc.pool.Exec(ctx, `
			UPDATE instances SET task_state = NULL, updated_at = $1
			WHERE id = $2
		`, time.Now(), instanceID); err != nil {
			log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to clear task_state after migration")
		}

		if _, err := svc.pool.Exec(ctx, `
			UPDATE server_migrations SET status = 'completed', updated_at = $1
			WHERE id = $2
		`, time.Now(), migrationID); err != nil {
			log.Error().Err(err).Str("migration_id", migrationID).Msg("failed to mark migration as completed")
		}
	}()

	c.Status(http.StatusAccepted)
}

// LiveMigrateInstance handles live migration
func (svc *Service) LiveMigrateInstance(c *gin.Context, instanceID, projectID string) {

	// Validate that action data exists (already parsed by ServerAction)
	if _, exists := c.Get("action_data"); !exists {
		common.SendError(c, common.NewBadRequestError("missing os-migrateLive data"))
		return
	}

	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT status FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&status)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	if status != "ACTIVE" {
		common.SendError(c, common.NewInvalidStateError("instance must be active"))
		return
	}

	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE instances SET task_state = $1, updated_at = $2
		WHERE id = $3
	`, "migrating", time.Now(), instanceID)

	if err != nil {
		log.Error().Err(err).Str("operation", "live_migrate_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to start live migration"))
		return
	}

	c.Status(http.StatusAccepted)
}

// securityGroupNamesForServers returns, for each server, the groups its ports carry, in one query:
// one name per port and group, ports in creation order and then by id, a port's groups by name.
// Duplicates stay, as upstream reports them. A group with an empty name is reported by its id,
// upstream's fallback. A server whose ports carry no group has no entry.
//
// The ports are the only list of a server's groups and the one its filter is built from, so a server
// reports exactly what filters it (SPEC-033 FR-014, FR-015; research R10). A server's ports are those
// naming it that are its project's, as the address map reads them: device_id is any port creator's to
// set (SPEC-033 T180). A port may carry another project's group, which an administrator put there by id
// (FR-004); only an administrator's report names it, as upstream skips a group its caller cannot list
// (T262).
func securityGroupNamesForServers(ctx context.Context, pool database.DB, serverIDs []string, admin bool) (map[string][]string, error) {
	result := make(map[string][]string, len(serverIDs))
	if len(serverIDs) == 0 {
		return result, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT p.device_id::text, COALESCE(NULLIF(sg.name, ''), sg.id::text)
		FROM ports p
		JOIN instances i ON i.id = p.device_id AND i.project_id = p.project_id
		JOIN port_security_groups psg ON psg.port_id = p.id
		JOIN security_groups sg ON sg.id = psg.security_group_id AND (sg.project_id = i.project_id OR $2)
		WHERE p.device_id = ANY($1::uuid[])
		ORDER BY p.created_at, p.id, sg.name
	`, serverIDs, admin)
	if err != nil {
		return nil, fmt.Errorf("query the security groups of the servers' ports: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid, name string
		if err := rows.Scan(&sid, &name); err != nil {
			return nil, fmt.Errorf("scan a port's security group: %w", err)
		}
		result[sid] = append(result[sid], name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate the security groups of the servers' ports: %w", err)
	}
	return result, nil
}

// formatSecurityGroups renders names in Nova's [{"name": ...}] shape. It returns nil for none, and
// the caller then leaves security_groups out: upstream omits the key when no port carries a group,
// and never invents default (SPEC-033 FR-015).
func formatSecurityGroups(names []string) []gin.H {
	if len(names) == 0 {
		return nil
	}
	out := make([]gin.H, 0, len(names))
	for _, n := range names {
		out = append(out, gin.H{"name": n})
	}
	return out
}

// AddSecurityGroup adds a security group to every port of a server (SPEC-033 FR-001, research R4).
func (svc *Service) AddSecurityGroup(c *gin.Context, instanceID, projectID string) {
	svc.changeServerSecurityGroup(c, instanceID, projectID, true)
}

// RemoveSecurityGroup removes a security group from every port of a server that carries it (SPEC-033
// FR-002, research R4).
func (svc *Service) RemoveSecurityGroup(c *gin.Context, instanceID, projectID string) {
	svc.changeServerSecurityGroup(c, instanceID, projectID, false)
}

// serverGroupChange is what one addSecurityGroup or removeSecurityGroup stored: the group, the ports
// it changed, and the list it left on each, which the undo compares against (research R5).
type serverGroupChange struct {
	serverGroupRequest
	groupID string
	ports   []string            // the ports changed, in id order
	left    map[string][]string // port → the group ids this change left on it
}

// serverGroupRequest is an add or a remove of a security group on a server: the server, its project, the
// group as named — a name or an id — and whether the caller is an administrator.
type serverGroupRequest struct {
	instanceID, projectID, ref string
	add, admin                 bool
}

// changeServerSecurityGroup adds or removes a group on a server's ports. The ports are the only list
// of a server's groups and the one their firewall is built from (research R1), so the change is
// stored on them and then converged on the ports' nodes before the answer (R32). When a node cannot
// apply it, the change is undone, compare-and-set, and the request answers 500 (FR-008, R5).
func (svc *Service) changeServerSecurityGroup(c *gin.Context, instanceID, projectID string, add bool) {
	data, _ := c.Get("action_data")
	body, _ := data.(map[string]any)
	// Upstream's refusals, before any lookup (nova's security_groups _parse; SPEC-033 T238).
	ref, named := body["name"].(string)
	if !named {
		common.SendError(c, common.NewBadRequestError("Security group not specified"))
		return
	}
	if strings.TrimSpace(ref) == "" {
		common.SendError(c, common.NewBadRequestError("Security group name cannot be empty"))
		return
	}

	var change *serverGroupChange
	err := database.WithTx(c.Request.Context(), svc.pool, func(tx database.Tx) error {
		var err error
		change, err = storeServerGroupChange(c.Request.Context(), tx, serverGroupRequest{instanceID: instanceID,
			projectID: projectID, ref: ref, add: add, admin: common.HasRole(c, common.RoleAdmin)})
		return err
	})
	var refusal *common.OpenStackError
	if errors.As(err, &refusal) {
		common.SendError(c, refusal)
		return
	}
	if err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("store a security group change")
		common.SendError(c, common.NewInternalServerError("failed to change the server's security groups"))
		return
	}

	// Stored. From here the change is applied or undone, whether or not the client is still there.
	ctx := context.WithoutCancel(c.Request.Context())
	if err := svc.applyServerGroupChange(ctx, instanceID, change); err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Strs("port_ids", change.ports).
			Msg("this node could not apply the security group change; the change was not applied")
		common.SendError(c, common.NewInternalServerError("failed to apply the security group change"))
		return
	}
	c.Status(http.StatusAccepted)
}

// storeServerGroupChange is the change's transaction. It locks the server, then its ports in id order
// (research R3); resolves the group — a name in the server's project, which for an administrator acting
// on a tenant's server is the tenant's (constitution register, entry 5), an id in any project for an
// administrator, as upstream (FR-004, research R52); checks; and stores the change on the ports. A
// refusal is returned as the *common.OpenStackError to answer with, and rolls back.
func storeServerGroupChange(ctx context.Context, tx database.Tx, req serverGroupRequest) (*serverGroupChange, error) {
	instanceID, projectID, ref := req.instanceID, req.projectID, req.ref
	var one int
	err := tx.QueryRow(ctx, "SELECT 1 FROM instances WHERE id = $1 AND project_id = $2 FOR UPDATE",
		instanceID, projectID).Scan(&one)
	if errors.Is(err, database.ErrNoRows) {
		return nil, common.NewNotFoundError("instance") // deleted since the request found it
	}
	if err != nil {
		return nil, fmt.Errorf("lock instance %s: %w", instanceID, err)
	}
	// The server's ports are those naming it that are its project's: device_id is any port creator's to
	// set (SPEC-033 T180).
	ports, err := lockPorts(ctx, tx, "device_id = $1 AND project_id = $2", instanceID, projectID)
	if err != nil {
		return nil, err
	}
	groupID, err := neutron.ResolveSecurityGroup(ctx, tx, ref, projectID, req.admin)
	if errors.Is(err, neutron.ErrSecurityGroupNotFound) {
		return nil, common.NewNotFoundError("security group")
	}
	var ambiguous *neutron.AmbiguousSecurityGroupError
	if errors.As(err, &ambiguous) {
		return nil, common.NewConflictError(ambiguous.Error()) // upstream Nova's NoUniqueMatch (FR-026)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve security group %q: %w", ref, err)
	}
	if len(ports) == 0 {
		return nil, common.NewItemNotFoundError(fmt.Sprintf("instance_id %s could not be found as device id on any ports", instanceID))
	}

	change := &serverGroupChange{serverGroupRequest: req, groupID: groupID, left: map[string][]string{}}
	if req.add {
		err = addGroupToPorts(ctx, tx, change, ports)
	} else {
		err = removeGroupFromPorts(ctx, tx, change, ports)
	}
	if err != nil {
		return nil, err
	}
	for _, p := range change.ports {
		if change.left[p], err = neutron.PortSecurityGroupIDs(ctx, tx, p); err != nil {
			return nil, fmt.Errorf("read the groups of port %s: %w", p, err)
		}
	}
	return change, nil
}

// addGroupToPorts adds the group to every port. When any port already carries it, the whole add is
// refused with 400 and nothing changes: upstream's own outcome when it reaches such a port, without
// the ports it changed before (research R4).
func addGroupToPorts(ctx context.Context, tx database.Tx, change *serverGroupChange, ports []string) error {
	for _, p := range ports {
		groups, err := neutron.PortSecurityGroupIDs(ctx, tx, p)
		if err != nil {
			return fmt.Errorf("read the groups of port %s: %w", p, err)
		}
		if slices.Contains(groups, change.groupID) {
			return common.NewBadRequestError(fmt.Sprintf("Security group %s is already associated with the instance %s", change.ref, change.instanceID))
		}
	}
	for _, p := range ports {
		err := neutron.AddPortSecurityGroup(ctx, tx, p, change.groupID)
		if errors.Is(err, neutron.ErrSecurityGroupNotFound) {
			return common.NewNotFoundError("security group") // deleted since it was resolved
		}
		if errors.Is(err, neutron.ErrSecurityGroupConflict) {
			// A port would carry stateful and stateless groups. Upstream Nova raises
			// SecurityGroupConnectionStateConflict, 409, with the network service's reason (FR-051).
			return common.NewConflictError(err.Error())
		}
		if err != nil {
			return fmt.Errorf("add security group %s to port %s: %w", change.groupID, p, err)
		}
	}
	change.ports = ports
	return nil
}

// removeGroupFromPorts removes the group from every port that carries it; 404 when none does. The
// server's last group may go, as upstream allows: its ports then carry none, and a floating IP on
// them admits replies only (SPEC-033 FR-005).
func removeGroupFromPorts(ctx context.Context, tx database.Tx, change *serverGroupChange, ports []string) error {
	for _, p := range ports {
		removed, err := neutron.RemovePortSecurityGroup(ctx, tx, p, change.groupID)
		if err != nil {
			return fmt.Errorf("remove security group %s from port %s: %w", change.groupID, p, err)
		}
		if removed {
			change.ports = append(change.ports, p)
		}
	}
	if len(change.ports) == 0 {
		return common.NewItemNotFoundError(fmt.Sprintf("Security group %s not associated with the instance %s", change.ref, change.instanceID))
	}
	return nil
}

// lockPorts locks the ports matching where ($1 bound to arg) in id order, the order every change to a
// port's groups takes them in (research R3), and returns their ids.
func lockPorts(ctx context.Context, tx database.Tx, where string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, "SELECT id FROM ports WHERE "+where+" ORDER BY id FOR UPDATE", args...)
	if err != nil {
		return nil, fmt.Errorf("lock ports: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("lock ports: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// applyServerGroupChange converges the firewall of the changed ports, and of the ports whose rules
// expand the group, on their nodes. When that fails, it undoes the change, converges them again from
// what the database then holds, and returns the failure. Without a network service there is nothing
// to converge.
func (svc *Service) applyServerGroupChange(ctx context.Context, instanceID string, change *serverGroupChange) error {
	if svc.neutronSvc == nil {
		return nil
	}
	err := svc.neutronSvc.RefreshPortFilters(ctx, change.ports, []string{change.groupID})
	if err == nil {
		return nil
	}
	if uerr := svc.undoServerGroupChange(ctx, instanceID, change); uerr != nil {
		log.Error().Err(uerr).Str("instance_id", instanceID).
			Msg("undo a security group change; the reconciliation pass applies what the ports hold")
	}
	if rerr := svc.neutronSvc.RefreshPortFilters(ctx, change.ports, []string{change.groupID}); rerr != nil {
		log.Error().Err(rerr).Str("instance_id", instanceID).
			Msg("rebuild the filters after an undo; the reconciliation pass converges them")
	}
	return err
}

// undoServerGroupChange undoes a stored change in a transaction of its own, taking the same locks in
// the same order, on each port whose list is still the one the change left: a port another request
// has changed since keeps that change (research R5).
func (svc *Service) undoServerGroupChange(ctx context.Context, instanceID string, change *serverGroupChange) error {
	return database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, "SELECT 1 FROM instances WHERE id = $1 FOR UPDATE", instanceID).Scan(&one); err != nil && !errors.Is(err, database.ErrNoRows) {
			return fmt.Errorf("lock instance %s: %w", instanceID, err)
		}
		// The ports still there: one deleted since has nothing to put back, and walking it would hit the
		// foreign key and roll the whole undo back (SPEC-033 T207).
		locked, err := lockPorts(ctx, tx, "id = ANY($1::uuid[])", change.ports)
		if err != nil {
			return err
		}
		if !change.add {
			// A removed group may have been deleted since; then there is nothing to put back.
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM security_groups WHERE id = $1)",
				change.groupID).Scan(&exists); err != nil {
				return fmt.Errorf("read security group %s: %w", change.groupID, err)
			}
			if !exists {
				return nil
			}
		}
		for _, p := range locked {
			now, err := neutron.PortSecurityGroupIDs(ctx, tx, p)
			if err != nil {
				return fmt.Errorf("read the groups of port %s: %w", p, err)
			}
			if !slices.Equal(now, change.left[p]) {
				log.Info().Str("port_id", p).Msg("undo: another change reached the port since; it keeps that change")
				continue
			}
			if change.add {
				_, err = neutron.RemovePortSecurityGroup(ctx, tx, p, change.groupID)
			} else {
				err = neutron.AddPortSecurityGroup(ctx, tx, p, change.groupID)
			}
			if err != nil {
				return fmt.Errorf("undo on port %s: %w", p, err)
			}
		}
		return nil
	})
}

// ChangePassword changes the admin password for an instance
func (svc *Service) ChangePassword(c *gin.Context, instanceID, projectID string) {

	// Get action data from context
	actionData, exists := c.Get("action_data")
	if !exists {
		common.SendError(c, common.NewBadRequestError("missing changePassword data"))
		return
	}

	changePassMap, ok := actionData.(map[string]any)
	if !ok {
		common.SendError(c, common.NewBadRequestError("invalid changePassword data"))
		return
	}

	adminPass, ok := changePassMap["adminPass"].(string)
	if !ok || adminPass == "" {
		common.SendError(c, common.NewBadRequestError("missing adminPass"))
		return
	}

	// Verify instance exists
	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT status FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&status)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Can only change password on ACTIVE instances
	if status != "ACTIVE" {
		common.SendError(c, common.NewInvalidStateError(fmt.Sprintf("cannot change password for instance in %s state", status)))
		return
	}

	// Validate password strength (minimum 8 characters)
	if len(adminPass) < 8 {
		common.SendError(c, common.NewBadRequestError("password must be at least 8 characters"))
		return
	}

	// Hash password using bcrypt
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(adminPass), bcrypt.DefaultCost)
	if err != nil {
		log.Error().Err(err).Str("operation", "hash_password").Msg("bcrypt error")
		common.SendError(c, common.NewInternalServerError("failed to hash password"))
		return
	}

	// Update admin password hash in database
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE instances
		SET admin_password_hash = $1, updated_at = $2
		WHERE id = $3
	`, string(hashedPassword), time.Now(), instanceID)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_password").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to update password"))
		return
	}

	// In stub mode, just return success
	// In real mode, would use libvirt guest agent or cloud-init to inject password
	c.Status(http.StatusAccepted)
}

// RestoreInstance answers `restore` as upstream does at its default reclaim_instance_interval
// of 0, where deletion is immediate (SPEC-025-followup FR-035). The dispatcher has already
// answered 404 for a server that no longer exists; one that does exist is live, never
// soft-deleted, so upstream refuses it with 409. No state here lets the action succeed, but
// upstream keeps the route at every setting, so a client gets its answer, not "unknown action".
func (svc *Service) RestoreInstance(c *gin.Context, instanceID string) {
	common.SendError(c, common.NewInvalidStateError(
		fmt.Sprintf("Instance %s is in an invalid state for 'restore'", instanceID)))
}

// CreateBackupAction creates a backup image of an instance
func (svc *Service) CreateBackupAction(c *gin.Context, instanceID, projectID string) {

	// Get action data from context
	actionData, exists := c.Get("action_data")
	if !exists {
		common.SendError(c, common.NewBadRequestError("missing createBackup data"))
		return
	}

	backupMap, ok := actionData.(map[string]any)
	if !ok {
		common.SendError(c, common.NewBadRequestError("invalid createBackup data"))
		return
	}

	backupName, ok := backupMap["name"].(string)
	if !ok || backupName == "" {
		common.SendError(c, common.NewBadRequestError("missing backup name"))
		return
	}
	// The backup name becomes the glance image's name, whose schema (and
	// column) cap at 255 — over-long names answer 400 upstream, not a 500
	// from the VARCHAR overflow.
	if utf8.RuneCountInString(backupName) > 255 {
		common.SendError(c, common.NewBadRequestError("backup name too long (max 255 characters)"))
		return
	}

	backupType, _ := backupMap["backup_type"].(string)
	rotation, _ := backupMap["rotation"].(float64)

	if backupType == "" {
		backupType = "daily"
	}
	if rotation == 0 {
		rotation = 7 // default rotation
	}

	// Verify instance exists
	var existingInstanceID string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT id FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&existingInstanceID)

	if errors.Is(err, database.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("database error")
		common.SendError(c, common.NewInternalServerError("database error"))
		return
	}

	// Generate new UUID for backup image
	backupImageID := uuid.New().String()

	// Create backup image in the images table. The image is named EXACTLY
	// what the caller asked for — upstream Nova passes `name` through
	// verbatim — so clients can look the image up by name afterwards
	// (OSC's `server backup create` polls for it; the previous
	// `{name}-{type}-{timestamp}` suffix broke that lookup). Rotation finds
	// its candidates through the image_tags below, not through the name.
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO images (id, name, project_id, status, visibility, disk_format, container_format, created_at, updated_at)
		VALUES ($1, $2, $3, 'active', 'private', 'qcow2', 'bare', $4, $4)
	`, backupImageID, backupName, projectID, time.Now())

	if err != nil {
		log.Error().Err(err).Str("operation", "create_backup_image").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create backup image"))
		return
	}

	// Add tags to identify this as a backup. Failures are logged but
	// don't abort: the backup image already exists and is usable;
	// missing tags only break rotation, not the snapshot itself.
	if _, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO image_tags (image_id, tag) VALUES ($1, 'backup'), ($1, $2), ($1, $3)
	`, backupImageID, fmt.Sprintf("backup_type:%s", backupType), fmt.Sprintf("source_server:%s", instanceID)); err != nil {
		log.Warn().Err(err).Str("image_id", backupImageID).Msg("failed to tag backup image; rotation may not work")
	}

	// Implement rotation: delete old backups of same type for this server
	// Query all backup images for this server with same backup_type
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT DISTINCT i.id, i.created_at
		FROM images i
		JOIN image_tags t1 ON i.id = t1.image_id AND t1.tag = 'backup'
		JOIN image_tags t2 ON i.id = t2.image_id AND t2.tag = $1
		JOIN image_tags t3 ON i.id = t3.image_id AND t3.tag = $2
		WHERE i.project_id = $3
		ORDER BY i.created_at DESC
	`, fmt.Sprintf("backup_type:%s", backupType), fmt.Sprintf("source_server:%s", instanceID), projectID)

	if err == nil {
		defer rows.Close()

		var backupIDs []string
		for rows.Next() {
			var id string
			var createdAt time.Time
			if err := rows.Scan(&id, &createdAt); err != nil {
				log.Warn().Err(err).Msg("failed to scan backup image row; skipping")
				continue
			}
			backupIDs = append(backupIDs, id)
		}
		// Deliberately non-fatal: the backup image is already created and this
		// handler still returns 202 below. Aborting here would report failure
		// for an operation that succeeded; only rotation is skipped.
		if err := rows.Err(); err != nil {
			log.Warn().Err(err).Msg("backup rotation: iterating backup images failed; skipping rotation")
			backupIDs = nil // the next backup rotates; never act on a partial listing
		}

		// Delete backups beyond rotation limit. A failed delete leaves a
		// stale image row; logged but not fatal — the new backup still
		// succeeded.
		if len(backupIDs) > int(rotation) {
			oldBackups := backupIDs[int(rotation):]
			for _, oldID := range oldBackups {
				if _, err := svc.pool.Exec(c.Request.Context(), "DELETE FROM images WHERE id = $1", oldID); err != nil {
					log.Warn().Err(err).Str("image_id", oldID).Msg("failed to delete rotated backup image")
				}
			}
		}
	}

	// The image is identified by the Location header at every microversion (OpenStack
	// pattern), and additionally in the BODY from 2.45 (SPEC-025-followup FR-039).
	c.Header("Location", fmt.Sprintf("/v2/images/%s", backupImageID))

	// This is the gate the comment here always claimed and the code never had: it read
	// "Microversion 2.45+ returns image_id in response body" while returning the body
	// unconditionally, so a client negotiating 2.1 received a field upstream introduced 44
	// versions later. The audit's first finding was a documented gate that did not exist.
	if !wantsCreateImageIDInBody(c) {
		c.Status(http.StatusAccepted)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"image_id": backupImageID,
	})
}

// ResetStateAction resets instance state (admin operation).
//
// Administrator-only, and therefore NOT scoped to the administrator's own
// project — an operator resets the state of a *tenant's* stuck instance, which
// is the only reason the operation exists. Before SPEC-023 the existence check
// was scoped `AND project_id = $2` to the caller while the UPDATE below was not,
// so the check was the only thing narrowing it and an administrator got 404 for
// every instance they did not own (FR-008a).
func (svc *Service) ResetStateAction(c *gin.Context, instanceID, _ string) {

	// Admin-only operation - check roles
	if !common.HasRole(c, common.RoleAdmin) {
		common.SendError(c, common.NewForbiddenError("Policy doesn't allow os-resetState to be performed"))
		return
	}

	// Get action data from context
	actionData, exists := c.Get("action_data")
	if !exists {
		common.SendError(c, common.NewBadRequestError("missing os-resetState data"))
		return
	}

	resetMap, ok := actionData.(map[string]any)
	if !ok {
		common.SendError(c, common.NewBadRequestError("invalid os-resetState data"))
		return
	}

	state, ok := resetMap["state"].(string)
	if !ok || state == "" {
		common.SendError(c, common.NewBadRequestError("missing state"))
		return
	}

	// Resolve the instance across projects — the admin gate above already
	// decided who may do this, and scoping the lookup to the caller would
	// contradict it.
	instanceUUID, _, ok := resolveServerOr404(c, svc.pool, instanceID)
	if !ok {
		return
	}

	// Upstream accepts exactly two states. Its api-ref gives os-resetState.state as
	// "The state to set, 'active' or 'error'", and refuses anything else.
	//
	// This mapped those two and PASSED EVERYTHING ELSE THROUGH UNCHANGED into
	// instances.status — the column every listing, every server show, the ?status= filter
	// and Horizon's status column read. `{"state":"banana"}` was answered 202 and became a
	// permanent, advertised server status. `{"state":"shutoff"}` stored a lowercase value
	// that only a case-folding comparison could find again, which is why the listing
	// filter's UPPER(status) is load-bearing rather than gratuitous: it exists to cope with
	// what this handler used to write.
	//
	// Matched case-INSENSITIVELY, deliberately laxer than upstream's enum: a caller that
	// sent "ACTIVE" had it stored by the old pass-through, so refusing it now would remove
	// behaviour rather than add correctness. Erring toward acceptance is the direction that
	// cannot break a caller.
	//
	// Folding first and comparing once means the value that gets stored IS the value that
	// was checked: if the guard passes, statusUpper is "ACTIVE" or "ERROR" by construction,
	// so there is no branch in which a non-canonical status can reach the UPDATE.
	statusUpper := strings.ToUpper(state)
	if statusUpper != "ACTIVE" && statusUpper != "ERROR" {
		common.SendError(c, common.NewInvalidValueError("state", state, "active or error"))
		return
	}

	_, err := svc.pool.Exec(c.Request.Context(), `
		UPDATE instances
		SET status = $1, updated_at = $2
		WHERE id = $3
	`, statusUpper, time.Now(), instanceUUID)

	if err != nil {
		log.Error().Err(err).Str("operation", "reset_state").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to reset instance state"))
		return
	}

	c.Status(http.StatusAccepted)
}

// ResetNetworkAction resets instance network
func (svc *Service) ResetNetworkAction(c *gin.Context, instanceID, projectID string) {

	// Verify instance exists
	var existsCheck bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM instances WHERE id = $1 AND project_id = $2)",
		instanceID, projectID,
	).Scan(&existsCheck)

	if err != nil || !existsCheck {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// In stub mode, just return success
	// In real mode, would reset network interfaces via libvirt/netlink
	c.Status(http.StatusAccepted)
}
