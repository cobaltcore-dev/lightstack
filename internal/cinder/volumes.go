package cinder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// Service handles Cinder API endpoints
type Service struct {
	pool       *pgxpool.Pool
	mode       string
	cephPool   string
	cephConf   string
	cephClient *storage.CephClient
	// fetchImageBase puts an image's raw base on this node before a local create-from-image
	// reads it; the image store's EnsureLocalBase once SetImageStore has run, nil before.
	fetchImageBase func(ctx context.Context, imageID, expectedChecksum string) (string, error)
	wg             sync.WaitGroup
	ctx            context.Context
	cancel         context.CancelFunc
}

// NewService creates a new Cinder service
func NewService(pool *pgxpool.Pool, mode, cephPool, cephConf string) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		pool:       pool,
		mode:       mode,
		cephPool:   cephPool,
		cephConf:   cephConf,
		cephClient: storage.NewCephClient(mode, cephPool, cephConf),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Shutdown signals all background goroutines to stop and waits for them.
func (svc *Service) Shutdown() {
	svc.cancel()
	svc.wg.Wait()
}

// RegisterRoutes registers Cinder routes
func (svc *Service) RegisterRoutes(r *gin.RouterGroup) {
	// Version discovery endpoint (no auth required)
	r.GET("/", svc.GetVersions)
	r.GET("/v3", svc.GetVersionV3)

	// Limits endpoint without project_id (extracts from token)
	r.GET("/v3/limits", svc.GetLimitsNoProject)

	// Volume transfers without project_id (extracts from token).
	r.GET("/v3/os-volume-transfer", svc.ListVolumeTransfersNoProject)
	r.GET("/v3/os-volume-transfer/detail", svc.ListVolumeTransfersNoProject)
	r.POST("/v3/os-volume-transfer", svc.CreateVolumeTransfer)
	r.GET("/v3/os-volume-transfer/:id", svc.GetVolumeTransfer)
	r.POST("/v3/os-volume-transfer/:id/accept", svc.AcceptVolumeTransfer)
	r.DELETE("/v3/os-volume-transfer/:id", svc.DeleteVolumeTransfer)

	// Volumes operations without project_id (extracts from token)
	r.POST("/v3/volumes", svc.CreateVolume)
	r.GET("/v3/volumes", svc.ListVolumes)
	r.GET("/v3/volumes/detail", svc.ListVolumesDetail)
	r.GET("/v3/volumes/:id", svc.GetVolume)
	r.PATCH("/v3/volumes/:id", svc.UpdateVolume)
	r.PUT("/v3/volumes/:id", svc.UpdateVolume)
	r.DELETE("/v3/volumes/:id", svc.DeleteVolume)
	r.POST("/v3/volumes/:id/action", svc.VolumeAction)

	// Snapshots list without project_id (extracts from token)
	r.GET("/v3/snapshots", svc.ListSnapshots)
	r.GET("/v3/snapshots/detail", svc.ListSnapshotsDetail)
	r.POST("/v3/snapshots", svc.CreateSnapshot)
	r.GET("/v3/snapshots/:id", svc.GetSnapshot)
	r.PUT("/v3/snapshots/:id", svc.UpdateSnapshot)
	r.PATCH("/v3/snapshots/:id", svc.UpdateSnapshot)
	r.DELETE("/v3/snapshots/:id", svc.DeleteSnapshot)

	// Volume types without project_id (extracts from token)
	r.GET("/v3/types", svc.ListVolumeTypes)
	r.GET("/v3/types/default", svc.GetDefaultVolumeType)
	r.POST("/v3/types", svc.CreateVolumeType)
	r.GET("/v3/types/:id", svc.GetVolumeType)
	r.PUT("/v3/types/:id", svc.UpdateVolumeType)
	r.DELETE("/v3/types/:id", svc.DeleteVolumeType)
	r.POST("/v3/types/:id/action", svc.VolumeTypeAction)
	r.GET("/v3/types/:id/os-volume-type-access", svc.ListVolumeTypeAccess)
	r.GET("/v3/types/:id/extra_specs", svc.ListVolumeTypeExtraSpecs)
	r.POST("/v3/types/:id/extra_specs", svc.CreateVolumeTypeExtraSpecs)
	r.GET("/v3/types/:id/extra_specs/:key", svc.GetVolumeTypeExtraSpecKey)
	r.PUT("/v3/types/:id/extra_specs/:key", svc.UpdateVolumeTypeExtraSpecKey)
	r.DELETE("/v3/types/:id/extra_specs/:key", svc.DeleteVolumeTypeExtraSpecKey)

	// Volume groups without project_id (extracts from token).
	//
	// `groups/detail` is what python-cinderclient requests for
	// groups.list(detailed=True), and therefore what Horizon reaches through
	// api.cinder.group_list. It MUST be registered — when only `groups/:id` existed, gin
	// matched the request as a group literally named "detail" and GetGroup 500'd, which
	// aborted Horizon's Create Volume modal and made the button look inert.
	//
	// Registration *order* is irrelevant: gin's tree splices a static child ahead of an
	// existing wildcard at the same level, so the static route wins however it is added
	// (verified against the vendored gin v1.12.0 in both orders). Only presence matters.
	// What does panic is reusing a *different* param name at a position already claimed by
	// one — ":tenant_id" beside an existing ":id" — which is why every route here says :id.
	//
	// Both views share ListGroups: Lightstack's group objects already carry every field it
	// models, so the detailed view has nothing extra to add.
	r.GET("/v3/groups", svc.ListGroups)
	r.GET("/v3/groups/detail", svc.ListGroups)
	r.POST("/v3/groups", svc.CreateGroup)
	r.GET("/v3/groups/:id", svc.GetGroup)
	r.PUT("/v3/groups/:id", svc.UpdateGroup)
	r.DELETE("/v3/groups/:id", svc.DeleteGroup)

	// QoS Specs without project_id (extracts from token)
	r.GET("/v3/qos-specs", svc.ListQoSSpecs)
	r.POST("/v3/qos-specs", svc.CreateQoSSpec)
	r.GET("/v3/qos-specs/:id", svc.GetQoSSpec)
	r.PUT("/v3/qos-specs/:id", svc.UpdateQoSSpec)
	r.DELETE("/v3/qos-specs/:id", svc.DeleteQoSSpec)

	// Backups without project_id (extracts from token)
	r.GET("/v3/backups", svc.ListBackups)
	r.GET("/v3/backups/detail", svc.ListBackupsDetail)
	r.POST("/v3/backups", svc.CreateBackup)
	r.GET("/v3/backups/:id", svc.GetBackup)
	r.DELETE("/v3/backups/:id", svc.DeleteBackup)
	r.POST("/v3/backups/:id/action", svc.BackupAction)

	// Availability zones without project_id (extracts from token)
	r.GET("/v3/os-availability-zone", svc.ListAvailabilityZones)

	// Volume/snapshot management without project_id (extracts from token).
	r.POST("/v3/os-volume-manage", svc.ManageVolume)
	r.GET("/v3/manageable_volumes", svc.ListManageableVolumes)
	r.POST("/v3/os-snapshot-manage", svc.ManageSnapshot)

	// Volume metadata without project_id (extracts from token)
	r.GET("/v3/volumes/:id/metadata", svc.GetVolumeMetadata)
	r.POST("/v3/volumes/:id/metadata", svc.SetVolumeMetadata)
	r.GET("/v3/volumes/:id/metadata/:key", svc.GetVolumeMetadataKey)
	r.PUT("/v3/volumes/:id/metadata/:key", svc.UpdateVolumeMetadataKey)
	r.DELETE("/v3/volumes/:id/metadata/:key", svc.DeleteVolumeMetadataKey)

	// Snapshot metadata without project_id (extracts from token)
	r.GET("/v3/snapshots/:id/metadata", svc.GetSnapshotMetadata)
	r.POST("/v3/snapshots/:id/metadata", svc.SetSnapshotMetadata)
	r.GET("/v3/snapshots/:id/metadata/:key", svc.GetSnapshotMetadataKey)
	r.PUT("/v3/snapshots/:id/metadata/:key", svc.UpdateSnapshotMetadataKey)
	r.DELETE("/v3/snapshots/:id/metadata/:key", svc.DeleteSnapshotMetadataKey)

	// OS services list without project_id
	r.GET("/v3/os-services", svc.ListServices)

	// API extensions (Horizon's create-volume form probes this)
	r.GET("/v3/extensions", svc.ListExtensions)

	// Quotas without project_id (extracts target project from URL :id).
	// `os-quota-sets` is upstream Cinder's spelling and the one python-cinderclient
	// (and therefore Horizon) requests; `quota-sets` is kept for the callers that
	// already use it. Both resolve to the same handlers. There is deliberately no
	// `/detail` sub-resource: that is Nova's spelling, and block storage exposes usage
	// through `?usage=true` on the show route instead (see GetQuotaSet).
	r.GET("/v3/quota-sets/:id", svc.GetQuotaSet)
	r.GET("/v3/quota-sets/:id/defaults", svc.GetQuotaSetDefaults)
	r.PUT("/v3/quota-sets/:id", svc.UpdateQuotaSet)
	r.DELETE("/v3/quota-sets/:id", svc.DeleteQuotaSet)
	r.GET("/v3/os-quota-sets/:id", svc.GetQuotaSet)
	r.GET("/v3/os-quota-sets/:id/defaults", svc.GetQuotaSetDefaults)
	r.PUT("/v3/os-quota-sets/:id", svc.UpdateQuotaSet)
	r.DELETE("/v3/os-quota-sets/:id", svc.DeleteQuotaSet)

	// Project-scoped forms of everything above. Cinder's canonical upstream path carries
	// the project (`/v3/{admin_project_id}/os-quota-sets/{project_id}`), and while Lightstack's
	// catalog advertises the flat `http://<host>:8776/v3`, clients that build the scoped
	// URL themselves still reach us here.
	//
	// Every resource registered flat above is therefore registered here too, deliberately:
	// this group predates the feature by 43 routes, and the one gap in it — `GET
	// /v3/{project}/volumes`, present as POST but not GET — is filed as a real defect
	// (cloud-storage#536 B2, a 405 that surprised a client). Leaving the new resources out
	// of the group would reproduce exactly that class of bug for groups and quota-sets, so
	// the duplication is the consistent choice, not scope creep.
	v3 := r.Group("/v3/:project_id")
	{
		// Volumes (create, get by ID, update, delete - need project_id in URL)
		v3.POST("/volumes", svc.CreateVolume)
		v3.GET("/volumes/:id", svc.GetVolume)
		v3.PATCH("/volumes/:id", svc.UpdateVolume)
		v3.DELETE("/volumes/:id", svc.DeleteVolume)
		v3.POST("/volumes/:id/action", svc.VolumeAction)

		// Volume metadata
		v3.GET("/volumes/:id/metadata", svc.GetVolumeMetadata)
		v3.POST("/volumes/:id/metadata", svc.SetVolumeMetadata)
		v3.GET("/volumes/:id/metadata/:key", svc.GetVolumeMetadataKey)
		v3.PUT("/volumes/:id/metadata/:key", svc.UpdateVolumeMetadataKey)
		v3.DELETE("/volumes/:id/metadata/:key", svc.DeleteVolumeMetadataKey)

		// Snapshots
		v3.GET("/snapshots", svc.ListSnapshots)
		v3.POST("/snapshots", svc.CreateSnapshot)
		v3.GET("/snapshots/:id", svc.GetSnapshot)
		v3.PUT("/snapshots/:id", svc.UpdateSnapshot)
		v3.PATCH("/snapshots/:id", svc.UpdateSnapshot)
		v3.DELETE("/snapshots/:id", svc.DeleteSnapshot)

		// Snapshot metadata
		v3.GET("/snapshots/:id/metadata", svc.GetSnapshotMetadata)
		v3.POST("/snapshots/:id/metadata", svc.SetSnapshotMetadata)
		v3.GET("/snapshots/:id/metadata/:key", svc.GetSnapshotMetadataKey)
		v3.PUT("/snapshots/:id/metadata/:key", svc.UpdateSnapshotMetadataKey)
		v3.DELETE("/snapshots/:id/metadata/:key", svc.DeleteSnapshotMetadataKey)

		// Volume types
		v3.GET("/types", svc.ListVolumeTypes)
		v3.POST("/types", svc.CreateVolumeType)
		v3.GET("/types/:id", svc.GetVolumeType)
		v3.PUT("/types/:id", svc.UpdateVolumeType)
		v3.DELETE("/types/:id", svc.DeleteVolumeType)
		v3.POST("/types/:id/action", svc.VolumeTypeAction)
		v3.GET("/types/:id/os-volume-type-access", svc.ListVolumeTypeAccess)
		v3.GET("/types/:id/extra_specs", svc.ListVolumeTypeExtraSpecs)
		v3.POST("/types/:id/extra_specs", svc.CreateVolumeTypeExtraSpecs)
		v3.GET("/types/:id/extra_specs/:key", svc.GetVolumeTypeExtraSpecKey)
		v3.PUT("/types/:id/extra_specs/:key", svc.UpdateVolumeTypeExtraSpecKey)
		v3.DELETE("/types/:id/extra_specs/:key", svc.DeleteVolumeTypeExtraSpecKey)

		// Limits
		v3.GET("/limits", svc.GetLimits)

		// Services
		v3.GET("/os-services", svc.ListServices)

		// Volume transfers
		v3.POST("/os-volume-transfer", svc.CreateVolumeTransfer)
		v3.GET("/os-volume-transfer/:id", svc.GetVolumeTransfer)
		v3.POST("/os-volume-transfer/:id/accept", svc.AcceptVolumeTransfer)
		v3.DELETE("/os-volume-transfer/:id", svc.DeleteVolumeTransfer)

		// Volume/Snapshot management
		v3.POST("/os-volume-manage", svc.ManageVolume)
		v3.GET("/manageable_volumes", svc.ListManageableVolumes)
		v3.POST("/os-snapshot-manage", svc.ManageSnapshot)

		// API extensions
		v3.GET("/extensions", svc.ListExtensions)

		// Availability zones. Mirrored because this group's own /extensions advertises
		// `os-availability-zone`: a client resolving the project-scoped catalog URL
		// (internal/keystone/auth.go builds the volumev3 entry as .../v3/{project_id})
		// discovers the capability here and would then 404 on the path it was handed
		// (raised on PR #79). Upstream serves both forms for every resource —
		// cinder/api/v3/router.py's ProjectMapper.resource registers each one with the
		// project_id prefix and then again without it.
		v3.GET("/os-availability-zone", svc.ListAvailabilityZones)

		// Volume groups. Clients that resolve a project-scoped catalog URL
		// (http://host:8776/v3/{project_id}) address these here; `detail` is
		// registered for the same reason as the flat routes above.
		v3.GET("/groups", svc.ListGroups)
		v3.GET("/groups/detail", svc.ListGroups)
		v3.POST("/groups", svc.CreateGroup)
		v3.GET("/groups/:id", svc.GetGroup)
		v3.PUT("/groups/:id", svc.UpdateGroup)
		v3.DELETE("/groups/:id", svc.DeleteGroup)

		// Quotas (both spellings, as above)
		v3.GET("/quota-sets/:id", svc.GetQuotaSet)
		v3.GET("/quota-sets/:id/defaults", svc.GetQuotaSetDefaults)
		v3.PUT("/quota-sets/:id", svc.UpdateQuotaSet)
		v3.DELETE("/quota-sets/:id", svc.DeleteQuotaSet)
		v3.GET("/os-quota-sets/:id", svc.GetQuotaSet)
		v3.GET("/os-quota-sets/:id/defaults", svc.GetQuotaSetDefaults)
		v3.PUT("/os-quota-sets/:id", svc.UpdateQuotaSet)
		v3.DELETE("/os-quota-sets/:id", svc.DeleteQuotaSet)
	}
}

// CreateVolumeRequest represents a volume creation request
type CreateVolumeRequest struct {
	Volume struct {
		Name        string `json:"name"`
		Size        int    `json:"size" binding:"required"`
		Description string `json:"description"`
		VolumeType  string `json:"volume_type"`
		SnapshotID  string `json:"snapshot_id"`
		SourceVolID string `json:"source_volid"`
		ImageRef    string `json:"imageRef"`
	} `json:"volume"`
}

// Volume response constants (F9 — Horizon volume-table compatibility).
const (
	volumeTSLayout = "2006-01-02T15:04:05.000000"
	// defaultVolumeType is upstream Cinder's canonical default-type name, reported
	// when a volume has no explicit type (spec Q2). Kept in sync with GetDefaultVolumeType.
	defaultVolumeType = "__DEFAULT__"
	// volumeBackendHost is a stable, non-null value for the admin-only host attribute;
	// Lightstack does not model per-backend hosts, but Horizon's table dereferences it.
	volumeBackendHost = "lightstack"
)

// volumeRow is the persisted state serializeVolume renders. It is a named struct
// rather than a positional argument list because the fields are mostly strings and
// a transposed pair (tenant/user, name/description) would be silently wrong.
type volumeRow struct {
	ID          string
	Name        string
	Description string
	TenantID    string
	UserID      string
	VolumeType  string
	Status      string
	Size        int
	Bootable    bool
	Attachments []any
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// serializeVolume renders a volume as the full upstream Cinder v3 object, with a
// non-null value for every field Horizon's volume table dereferences (F9,
// FR-001/004/005). It is the single source of truth for the show, list-detail, and
// create responses so they stay consistent. Attributes Lightstack does not model are still
// emitted, with the same values upstream Cinder reports (verified against the v3.65
// api-ref volume response) — present-but-null beats absent, because a template that
// dereferences a missing key raises where null merely renders empty.
func serializeVolume(v volumeRow) gin.H {
	// Defensive default (intentional duplication with CreateVolume): read-back tolerates
	// legacy rows whose volume_type is NULL/empty; create persists the concrete default.
	volumeType := v.VolumeType
	if volumeType == "" {
		volumeType = defaultVolumeType
	}
	attachments := v.Attachments
	if attachments == nil {
		attachments = []any{}
	}
	return gin.H{
		"id":                           v.ID,
		"name":                         v.Name,
		"description":                  v.Description,
		"tenant_id":                    v.TenantID,
		"user_id":                      v.UserID,
		"size":                         v.Size,
		"status":                       v.Status,
		"bootable":                     fmt.Sprintf("%t", v.Bootable),
		"created_at":                   v.CreatedAt.Format(volumeTSLayout),
		"updated_at":                   v.UpdatedAt.Format(volumeTSLayout),
		"attachments":                  attachments,
		"metadata":                     gin.H{},
		"volume_type":                  volumeType,
		"volume_type_id":               volumeType,
		"availability_zone":            "nova",
		"encrypted":                    false,
		"multiattach":                  false,
		"consumes_quota":               true,
		"shared_targets":               false,
		"os-vol-tenant-attr:tenant_id": v.TenantID,
		"os-vol-host-attr:host":        volumeBackendHost,
		// Modelled by upstream Cinder but not by Lightstack; emitted with upstream's own
		// null values so the key is always present for clients that dereference it.
		"consistencygroup_id": nil,
		"migration_status":    nil,
		"replication_status":  nil,
		"snapshot_id":         nil,
		"source_volid":        nil,
	}
}

// volumeSpec is what provisionVolume needs to create one volume.
type volumeSpec struct {
	ID          string
	Name        string
	Description string
	ProjectID   string
	UserID      string
	SizeGB      int
	ImageRef    string // empty for an empty volume
	VolumeType  string
	CreatedAt   time.Time
}

// volumeBackingError marks a failure to provision the backing store, as distinct from a
// failure to record the volume. The first is an external-dependency problem the API answers
// as unavailable, with msg; the second is internal.
type volumeBackingError struct {
	msg string
	err error
}

func (e *volumeBackingError) Error() string { return e.msg + ": " + e.err.Error() }
func (e *volumeBackingError) Unwrap() error { return e.err }

// SetImageStore wires the Glance image store in, so a local-backed create-from-image can
// fetch an RGW/S3-backed image's base first, as Nova's image boot does (SPEC-014).
func (svc *Service) SetImageStore(s *storage.ImageStore) {
	if s == nil {
		svc.fetchImageBase = nil
		return
	}
	svc.fetchImageBase = s.EnsureLocalBase
}

// provisionVolume creates a volume's backing store and records it, reporting whether the
// result is bootable.
//
// CreateVolume and the compute service's boot-from-new-volume path (SPEC-020 FR-025) both
// use it, so there is one place that decides what makes a volume bootable. Synchronous
// (Constitution II): the volume is usable as soon as this returns, which boot-from-volume
// requires. With an ImageRef the volume is a bootable copy of the image's raw base;
// otherwise it is empty (SPEC-009 WS-5 / T040).
func (svc *Service) provisionVolume(ctx context.Context, spec volumeSpec) (bootable bool, err error) {
	var sourceImageID any
	if spec.ImageRef != "" {
		if berr := svc.ensureLocalImageBase(ctx, spec.ImageRef); berr != nil {
			log.Error().Err(berr).Str("operation", "fetch_image_base").Str("image", spec.ImageRef).Msg("failed to fetch the image base for a volume")
			return false, &volumeBackingError{"failed to create volume from image", berr}
		}
		if berr := svc.cephClient.CreateVolumeFromImage(ctx, spec.ID, storage.LocalImagePath(spec.ImageRef), spec.SizeGB); berr != nil {
			log.Error().Err(berr).Str("operation", "create_volume_from_image").Str("image", spec.ImageRef).Msg("failed to create volume from image")
			return false, &volumeBackingError{"failed to create volume from image", berr}
		}
		sourceImageID = spec.ImageRef
		bootable = true
	} else if berr := svc.cephClient.CreateVolume(ctx, spec.ID, spec.SizeGB); berr != nil {
		log.Error().Err(berr).Str("operation", "create_volume_backing").Msg("failed to create volume backing")
		return false, &volumeBackingError{"failed to create volume backing", berr}
	}

	// Status available — the backing is already provisioned. volume_type and description
	// are persisted so they read back verbatim (F9, FR-003); both columns exist (migrations
	// 016 and 012) but were once parsed and discarded.
	if _, derr := svc.pool.Exec(ctx, `
		INSERT INTO volumes (id, name, description, project_id, user_id, size_gb, status, bootable, source_image_id, rbd_pool, rbd_image, volume_type, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`, spec.ID, spec.Name, spec.Description, spec.ProjectID, spec.UserID, spec.SizeGB, "available", bootable,
		sourceImageID, svc.cephPool, "volume-"+spec.ID, spec.VolumeType, spec.CreatedAt, spec.CreatedAt); derr != nil {
		// Roll the backing back (best-effort; the DB error is what the caller sees).
		if cephErr := svc.cephClient.DeleteVolume(ctx, spec.ID); cephErr != nil {
			log.Warn().Err(cephErr).Str("volume_id", spec.ID).Msg("rollback: failed to delete volume backing after DB insert failure")
		}
		log.Error().Err(derr).Str("operation", "create_volume_db").Msg("failed to insert volume into database")
		return false, fmt.Errorf("recording volume %s: %w", spec.ID, derr)
	}
	return bootable, nil
}

// ensureLocalImageBase makes sure the raw base of imageID is on this node before a
// local-backed create-from-image reads it. An RGW/S3-backed image may never have been
// fetched here: Nova's image boot fetches it lazily (SPEC-014), and a volume made from the
// same image needs the same fetch. The other storage modes read no local base.
func (svc *Service) ensureLocalImageBase(ctx context.Context, imageID string) error {
	if svc.fetchImageBase == nil || (svc.mode != "local" && svc.mode != "local,rbd") {
		return nil
	}
	var checksum string
	_ = svc.pool.QueryRow(ctx, "SELECT COALESCE(checksum, '') FROM images WHERE id = $1", imageID).Scan(&checksum)
	_, err := svc.fetchImageBase(ctx, imageID, checksum)
	return err
}

// CreateBootVolumeFromImage provisions a bootable volume from an image for the compute
// service's boot-from-new-volume path (SPEC-020 FR-025). Compute names an image and a size
// and gets a volume id back, with no knowledge of backing stores or volume types.
func (svc *Service) CreateBootVolumeFromImage(ctx context.Context, name, projectID, userID, imageID string, sizeGB int) (string, error) {
	volumeID := uuid.New().String()
	if _, err := svc.provisionVolume(ctx, volumeSpec{
		ID: volumeID, Name: name, ProjectID: projectID, UserID: userID,
		SizeGB: sizeGB, ImageRef: imageID, VolumeType: defaultVolumeType, CreatedAt: time.Now(),
	}); err != nil {
		return "", err
	}
	return volumeID, nil
}

// CreateVolume creates a new volume
func (svc *Service) CreateVolume(c *gin.Context) {
	var req CreateVolumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	userID := c.GetString("user_id")
	volumeID := uuid.New().String()

	if req.Volume.Size < 1 {
		common.SendError(c, common.NewBadRequestError("volume size must be at least 1 GB"))
		return
	}

	// The image must be one the caller can read, and active (SPEC-020 FR-026). Upstream asks
	// Glance with the caller's token, so another project's private image and an id that names
	// no image get the same answer.
	if req.Volume.ImageRef != "" {
		status, found, err := common.VisibleImage(c.Request.Context(), svc.pool, req.Volume.ImageRef, projectID, common.HasRole(c, common.RoleAdmin))
		if err != nil {
			log.Error().Err(err).Str("operation", "create_volume_check_image").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to create volume"))
			return
		}
		if !found {
			common.SendError(c, common.NewBadRequestError("Invalid image identifier or unable to access requested image."))
			return
		}
		if status != "active" {
			common.SendError(c, common.NewBadRequestError(imageNotActive(req.Volume.ImageRef)))
			return
		}
	}

	now := time.Now()
	// Persist the concrete default so volume_type reads back verbatim; serializeVolume
	// repeats this check defensively for legacy NULL rows (intentional duplication).
	volumeType := req.Volume.VolumeType
	if volumeType == "" {
		volumeType = defaultVolumeType
	}

	bootable, err := svc.provisionVolume(c.Request.Context(), volumeSpec{
		ID: volumeID, Name: req.Volume.Name, Description: req.Volume.Description,
		ProjectID: projectID, UserID: userID,
		SizeGB: req.Volume.Size, ImageRef: req.Volume.ImageRef, VolumeType: volumeType,
		CreatedAt: now,
	})
	if err != nil {
		var unavailable *volumeBackingError
		if errors.As(err, &unavailable) {
			common.SendError(c, common.NewServiceUnavailableError(unavailable.msg))
			return
		}
		common.SendError(c, common.NewInternalServerError("failed to create volume"))
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"volume": serializeVolume(volumeRow{
			ID: volumeID, Name: req.Volume.Name, Description: req.Volume.Description,
			TenantID: projectID, UserID: userID, VolumeType: volumeType,
			Status: "available", Size: req.Volume.Size, Bootable: bootable,
			CreatedAt: now, UpdatedAt: now,
		}),
	})
}

// ListVolumes lists all volumes (brief)
func (svc *Service) ListVolumes(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Parse pagination parameters
	limit := 1000
	offset := 0
	if limitParam := c.Query("limit"); limitParam != "" {
		if parsedLimit, err := strconv.Atoi(limitParam); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}
	if offsetParam := c.Query("offset"); offsetParam != "" {
		if parsedOffset, err := strconv.Atoi(offsetParam); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}

	// Marker-based pagination
	var markerCondition string
	var queryArgs []any
	queryArgs = append(queryArgs, projectID)
	argIdx := 2

	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT created_at FROM volumes WHERE id = $1 AND project_id = $2",
			marker, projectID,
		).Scan(&markerCreatedAt)
		if err == nil {
			markerCondition = fmt.Sprintf(" AND created_at < $%d", argIdx)
			queryArgs = append(queryArgs, markerCreatedAt)
			argIdx++
		}
	}

	queryArgs = append(queryArgs, limit, offset)

	rows, err := svc.pool.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT id, name, size_gb
		FROM volumes
		WHERE project_id = $1%s
		ORDER BY created_at DESC, id DESC
		LIMIT $%d OFFSET $%d
	`, markerCondition, argIdx, argIdx+1), queryArgs...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_volumes").Msg("failed to query volumes")
		common.SendError(c, common.NewInternalServerError("failed to list volumes"))
		return
	}
	defer rows.Close()

	var volumes []gin.H
	for rows.Next() {
		var id, name string
		var size int

		if err := rows.Scan(&id, &name, &size); err != nil {
			log.Warn().Err(err).Msg("failed to scan volume row")
			continue
		}

		volumes = append(volumes, gin.H{
			"id":   id,
			"name": name,
			"size": size,
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if volumes == nil {
		volumes = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"volumes": volumes})
}

// ListVolumesDetail lists all volumes (detailed)
func (svc *Service) ListVolumesDetail(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Parse pagination parameters
	limit := 1000
	offset := 0
	if limitParam := c.Query("limit"); limitParam != "" {
		if parsedLimit, err := strconv.Atoi(limitParam); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}
	if offsetParam := c.Query("offset"); offsetParam != "" {
		if parsedOffset, err := strconv.Atoi(offsetParam); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}

	// Marker-based pagination
	var markerCondition string
	var queryArgs []any
	queryArgs = append(queryArgs, projectID)
	argIdx := 2

	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT created_at FROM volumes WHERE id = $1 AND project_id = $2",
			marker, projectID,
		).Scan(&markerCreatedAt)
		if err == nil {
			markerCondition = fmt.Sprintf(" AND v.created_at < $%d", argIdx)
			queryArgs = append(queryArgs, markerCreatedAt)
			argIdx++
		}
	}

	queryArgs = append(queryArgs, limit, offset)

	rows, err := svc.pool.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT v.id, v.name, v.description, v.size_gb, v.status, v.bootable, v.user_id, v.volume_type, v.created_at, v.updated_at
		FROM volumes v
		WHERE v.project_id = $1%s
		ORDER BY v.created_at DESC
		LIMIT $%d OFFSET $%d
	`, markerCondition, argIdx, argIdx+1), queryArgs...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_volumes_detail").Msg("failed to query volumes")
		common.SendError(c, common.NewInternalServerError("failed to list volumes"))
		return
	}
	defer rows.Close()

	var volumes []gin.H
	var ids []string
	for rows.Next() {
		var id, name, status string
		var size int
		var bootable bool
		var userID, volumeType, description sql.NullString
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &description, &size, &status, &bootable, &userID, &volumeType, &createdAt, &updatedAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan volume detail row")
			continue
		}

		volumes = append(volumes, serializeVolume(volumeRow{
			ID: id, Name: name, Description: description.String,
			TenantID: projectID, UserID: userID.String, VolumeType: volumeType.String,
			Status: status, Size: size, Bootable: bootable,
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		}))
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	// Attachments for every listed volume in one query (FR-027).
	attachments, err := volumeAttachments(c.Request.Context(), svc.pool, ids)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_volumes_detail_attachments").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list volumes"))
		return
	}
	for i, id := range ids {
		if a, ok := attachments[id]; ok {
			volumes[i]["attachments"] = a
		}
	}

	if volumes == nil {
		volumes = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"volumes": volumes})
}

// GetVolume returns a single volume
func (svc *Service) GetVolume(c *gin.Context) {
	volumeID := c.Param("id")
	projectID := c.GetString("project_id")

	var id, name, status string
	var size int
	var bootable bool
	var userID, volumeType, description sql.NullString
	var createdAt, updatedAt time.Time

	// Support lookup by ID or name
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, description, size_gb, status, bootable, user_id, volume_type, created_at, updated_at
		FROM volumes
		WHERE project_id = $2 AND ((id::text = $1) OR (name = $1))
	`, volumeID, projectID).Scan(&id, &name, &description, &size, &status, &bootable, &userID, &volumeType, &createdAt, &updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_volume").Str("volume_id", volumeID).Msg("failed to query volume")
		common.SendError(c, common.NewInternalServerError("failed to get volume"))
		return
	}

	attachments, err := volumeAttachments(c.Request.Context(), svc.pool, []string{id})
	if err != nil {
		log.Error().Err(err).Str("operation", "get_volume_attachments").Str("volume_id", volumeID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get volume"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"volume": serializeVolume(volumeRow{
			ID: id, Name: name, Description: description.String,
			TenantID: projectID, UserID: userID.String, VolumeType: volumeType.String,
			Status: status, Size: size, Bootable: bootable, Attachments: attachments[id],
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		}),
	})
}

// imageNotActive is upstream's refusal of an image that is not active yet
// (cinder/volume/volume_utils.py check_image_metadata, raised as InvalidInput).
func imageNotActive(imageID string) string {
	return fmt.Sprintf("Invalid input received: Image %s is not active.", imageID)
}

// errVolumeNotFound: the volume an action names is not in the caller's project.
var errVolumeNotFound = errors.New("volume not found")

// attachRefusal is an os-attach the API answers with status and msg rather than a 500.
type attachRefusal struct {
	status int
	msg    string
}

func (e *attachRefusal) Error() string { return e.msg }

// attachVolume records an os-attach: a row in volume_attachments holding the mountpoint, in
// the table the compute service's attach also writes, so the attachment reads back like
// any other (SPEC-020 FR-027). The volume must be available, and the server one of the
// caller's, or any server for an administrator: the row is what the compute service lists
// for that server.
func (svc *Service) attachVolume(c *gin.Context, volumeID, projectID, instanceID, mountpoint string) {
	ctx := c.Request.Context()
	admin := common.HasRole(c, common.RoleAdmin)
	err := database.WithTx(ctx, svc.pool, func(tx pgx.Tx) error {
		var id, status string
		if err := tx.QueryRow(ctx,
			`SELECT id::text, status FROM volumes WHERE id::text = $1 AND project_id = $2 FOR UPDATE`,
			volumeID, projectID).Scan(&id, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errVolumeNotFound
			}
			return err
		}
		if status != "available" {
			return &attachRefusal{http.StatusBadRequest,
				fmt.Sprintf("Invalid volume: volume status must be available to attach, but the status is %s.", status)}
		}
		var serverOK bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM instances WHERE id = $1 AND (project_id = $2 OR $3))`,
			instanceID, projectID, admin).Scan(&serverOK); err != nil {
			return err
		}
		if !serverOK {
			return &attachRefusal{http.StatusBadRequest, "os-attach.instance_uuid does not name a server in this project"}
		}
		now := time.Now()
		if _, err := tx.Exec(ctx,
			`INSERT INTO volume_attachments (volume_id, instance_id, device, attached_at) VALUES ($1, $2, $3, $4)`,
			id, instanceID, mountpoint, now); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
				return &attachRefusal{http.StatusConflict,
					fmt.Sprintf("mountpoint %s is already in use on server %s", mountpoint, instanceID)}
			}
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE volumes SET attached_to_instance_id = $1, status = 'in-use', updated_at = $2 WHERE id = $3`,
			instanceID, now, id)
		return err
	})

	var refusal *attachRefusal
	switch {
	case err == nil:
		c.Status(http.StatusAccepted)
	case errors.Is(err, errVolumeNotFound):
		common.SendError(c, common.NewNotFoundError("volume"))
	case errors.As(err, &refusal) && refusal.status == http.StatusConflict:
		common.SendError(c, common.NewConflictError(refusal.msg))
	case errors.As(err, &refusal):
		common.SendError(c, common.NewBadRequestError(refusal.msg))
	default:
		log.Error().Err(err).Str("operation", "attach_volume").Str("volume_id", volumeID).Msg("failed to attach volume")
		common.SendError(c, common.NewInternalServerError("failed to attach volume"))
	}
}

// detachVolume clears a volume's attachment for os-detach and os-force_detach: its row in
// volume_attachments and the volume's in-use status together, so the volume and the
// compute service's attachment list agree (SPEC-020 FR-027).
func (svc *Service) detachVolume(c *gin.Context, volumeID, projectID, operation, failure string) {
	ctx := c.Request.Context()
	err := database.WithTx(ctx, svc.pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx,
			`SELECT id::text FROM volumes WHERE id::text = $1 AND project_id = $2 FOR UPDATE`,
			volumeID, projectID).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errVolumeNotFound
			}
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM volume_attachments WHERE volume_id = $1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE volumes SET attached_to_instance_id = NULL, status = 'available', updated_at = $1 WHERE id = $2`,
			time.Now(), id)
		return err
	})
	switch {
	case err == nil:
		c.Status(http.StatusAccepted)
	case errors.Is(err, errVolumeNotFound):
		common.SendError(c, common.NewNotFoundError("volume"))
	default:
		log.Error().Err(err).Str("operation", operation).Str("volume_id", volumeID).Msg(failure)
		common.SendError(c, common.NewInternalServerError(failure))
	}
}

// volumeAttachments reads the attachments of the given volumes, keyed by volume id, in the
// shape upstream Cinder reports them (cinder/api/v2/views/volumes.py _get_attachments,
// SPEC-020 FR-027). Every attachment is a row in volume_attachments, whichever API made it,
// and the row holds the device: the boot volume's /dev/vda, a data volume's first free
// device, or the mountpoint os-attach was given.
//
// Upstream's "id" is the volume's id; the attachment's own id is "attachment_id".
// host_name stays null: upstream shows it to administrators and services only, and
// Lightstack does not record the host.
func volumeAttachments(ctx context.Context, pool *pgxpool.Pool, volumeIDs []string) (map[string][]any, error) {
	result := make(map[string][]any, len(volumeIDs))
	if len(volumeIDs) == 0 {
		return result, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT id::text, volume_id::text, instance_id::text, device, attached_at
		FROM volume_attachments
		WHERE volume_id = ANY($1::uuid[])
		ORDER BY attached_at, device
	`, volumeIDs)
	if err != nil {
		return nil, fmt.Errorf("query volume attachments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, volumeID, serverID, device string
		var attachedAt time.Time
		if err := rows.Scan(&id, &volumeID, &serverID, &device, &attachedAt); err != nil {
			return nil, fmt.Errorf("scan volume attachment: %w", err)
		}
		result[volumeID] = append(result[volumeID], gin.H{
			"id":            volumeID,
			"attachment_id": id,
			"volume_id":     volumeID,
			"server_id":     serverID,
			"host_name":     nil,
			"device":        device,
			"attached_at":   attachedAt.Format(volumeTSLayout),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate volume attachments: %w", err)
	}
	return result, nil
}

// DeleteVolume deletes a volume
func (svc *Service) DeleteVolume(c *gin.Context) {
	volumeID := c.Param("id")
	projectID := c.GetString("project_id")

	// Check if volume is attached (support lookup by ID or name)
	var attachedTo sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT attached_to_instance_id FROM volumes WHERE project_id = $2 AND ((id::text = $1) OR (name = $1))",
		volumeID, projectID,
	).Scan(&attachedTo)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}
	// Any other lookup failure must abort here: falling through would leave attachedTo
	// zero-valued, skip both the attach and snapshot guards, and destroy the backing
	// store below on nothing worse than a transient DB error.
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_volume_lookup").Str("volume_id", volumeID).Msg("failed to look up volume")
		common.SendError(c, common.NewInternalServerError("failed to delete volume"))
		return
	}

	if attachedTo.Valid {
		common.SendError(c, common.NewBadRequestError("volume is attached to an instance"))
		return
	}

	// Check for existing snapshots. The count error is checked separately: a combined
	// `err == nil && snapshotCount > 0` silently skipped this guard whenever the query
	// failed, deleting a volume that still had snapshots and orphaning their backing
	// objects.
	var snapshotCount int
	if err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COUNT(*) FROM snapshots WHERE volume_id = $1", volumeID,
	).Scan(&snapshotCount); err != nil {
		log.Error().Err(err).Str("operation", "delete_volume_snapshot_count").Str("volume_id", volumeID).Msg("failed to count volume snapshots")
		common.SendError(c, common.NewInternalServerError("failed to delete volume"))
		return
	}
	if snapshotCount > 0 {
		common.SendError(c, common.NewOperationConflictError("delete volume", "volume has snapshots; delete snapshots first"))
		return
	}

	// Delete from Ceph
	if err := svc.cephClient.DeleteVolume(c.Request.Context(), volumeID); err != nil {
		log.Error().Err(err).Str("operation", "delete_volume_ceph").Str("volume_id", volumeID).Msg("failed to delete volume from Ceph")
		common.SendError(c, common.NewServiceUnavailableError("failed to delete volume from Ceph"))
		return
	}

	// Delete from database
	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM volumes WHERE id = $1 AND project_id = $2",
		volumeID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_volume_db").Str("volume_id", volumeID).Msg("failed to delete volume from database")
		common.SendError(c, common.NewInternalServerError("failed to delete volume"))
		return
	}

	c.Status(http.StatusNoContent)
}

// VolumeAction performs an action on a volume
func (svc *Service) VolumeAction(c *gin.Context) {
	volumeID := c.Param("id")
	projectID := c.GetString("project_id")

	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Handle attach action. Upstream requires a mountpoint and a server or a host;
	// Lightstack attaches to servers only.
	if attachData, ok := req["os-attach"]; ok {
		attachMap, ok := attachData.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-attach must be an object"))
			return
		}
		instanceID, ok := attachMap["instance_uuid"].(string)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-attach.instance_uuid must be a string"))
			return
		}
		if _, err := uuid.Parse(instanceID); err != nil {
			common.SendError(c, common.NewBadRequestError("os-attach.instance_uuid must be a UUID"))
			return
		}
		mountpoint, _ := attachMap["mountpoint"].(string)
		if mountpoint == "" {
			common.SendError(c, common.NewBadRequestError("os-attach.mountpoint is required"))
			return
		}
		// volume_attachments.device holds 50 characters; upstream allows 255.
		if len(mountpoint) > 50 {
			common.SendError(c, common.NewBadRequestError("os-attach.mountpoint must be at most 50 characters"))
			return
		}
		svc.attachVolume(c, volumeID, projectID, instanceID, mountpoint)
		return
	}

	// Handle detach action
	if _, ok := req["os-detach"]; ok {
		svc.detachVolume(c, volumeID, projectID, "detach_volume", "failed to detach volume")
		return
	}

	// Handle extend action
	if extendData, ok := req["os-extend"]; ok {
		extendMap, ok := extendData.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-extend must be an object"))
			return
		}
		var newSize int

		// Handle different JSON number types
		newSizeVal := extendMap["new_size"]
		switch v := newSizeVal.(type) {
		case float64:
			newSize = int(v)
		case int:
			newSize = v
		case int64:
			newSize = int(v)
		case json.Number:
			parsed, err := v.Int64()
			if err != nil {
				common.SendError(c, common.NewBadRequestError("invalid new_size format"))
				return
			}
			newSize = int(parsed)
		case string:
			parsed, err := strconv.Atoi(v)
			if err != nil {
				common.SendError(c, common.NewBadRequestError("invalid new_size format"))
				return
			}
			newSize = parsed
		default:
			common.SendError(c, common.NewBadRequestError(fmt.Sprintf("invalid new_size type: %T", newSizeVal)))
			return
		}

		// Update volume size
		var currentSize int
		err := svc.pool.QueryRow(c.Request.Context(), `
			SELECT size_gb FROM volumes WHERE id = $1 AND project_id = $2
		`, volumeID, projectID).Scan(&currentSize)
		if err != nil {
			common.SendError(c, common.NewInternalServerError("failed to fetch volume"))
			return
		}
		if newSize <= currentSize {
			common.SendError(c, common.NewBadRequestError(fmt.Sprintf("new_size (%d) must be greater than current size (%d)", newSize, currentSize)))
			return
		}

		_, err = svc.pool.Exec(c.Request.Context(), `
			UPDATE volumes
			SET size_gb = $1, updated_at = $2
			WHERE id = $3 AND project_id = $4
		`, newSize, time.Now(), volumeID, projectID)

		if err != nil {
			log.Error().Err(err).Str("operation", "extend_volume").Str("volume_id", volumeID).Msg("failed to extend volume")
			common.SendError(c, common.NewInternalServerError("failed to extend volume"))
			return
		}

		c.Status(http.StatusAccepted)
		return
	}

	// Handle retype action
	if retypeData, ok := req["os-retype"]; ok {
		retypeMap, ok := retypeData.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-retype must be an object"))
			return
		}
		newType, ok := retypeMap["new_type"].(string)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-retype.new_type must be a string"))
			return
		}

		// Get or create volume type
		var typeID string
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT id FROM volume_types WHERE name = $1",
			newType,
		).Scan(&typeID)

		if errors.Is(err, pgx.ErrNoRows) {
			// Create new volume type if it doesn't exist
			typeID = uuid.New().String()
			_, err = svc.pool.Exec(c.Request.Context(),
				"INSERT INTO volume_types (id, name, description, is_public) VALUES ($1, $2, $3, $4)",
				typeID, newType, "Auto-created volume type", true,
			)
			if err != nil {
				log.Error().Err(err).Str("operation", "retype_volume_create_type").Msg("failed to create volume type")
				common.SendError(c, common.NewInternalServerError("failed to create volume type"))
				return
			}
		}

		// Update volume type
		_, err = svc.pool.Exec(c.Request.Context(), `
			UPDATE volumes
			SET volume_type = $1, updated_at = $2
			WHERE id = $3 AND project_id = $4
		`, newType, time.Now(), volumeID, projectID)

		if err != nil {
			log.Error().Err(err).Str("operation", "retype_volume").Str("volume_id", volumeID).Msg("failed to retype volume")
			common.SendError(c, common.NewInternalServerError("failed to retype volume"))
			return
		}

		c.Status(http.StatusAccepted)
		return
	}

	// Handle unmanage action
	if _, ok := req["os-unmanage"]; ok {
		svc.UnmanageVolume(c, volumeID)
		return
	}

	// Handle update readonly flag action
	if readonlyData, ok := req["os-update_readonly_flag"]; ok {
		readonlyMap, ok := readonlyData.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-update_readonly_flag must be an object"))
			return
		}
		readonly, ok := readonlyMap["readonly"].(bool)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-update_readonly_flag.readonly must be a boolean"))
			return
		}

		// Update volume readonly flag (stored in metadata or separate field)
		// For now, update in volume_metadata table
		_, err := svc.pool.Exec(c.Request.Context(), `
			INSERT INTO volume_metadata (volume_id, meta_key, meta_value)
			VALUES ($1, 'readonly', $2)
			ON CONFLICT (volume_id, meta_key) DO UPDATE SET meta_value = EXCLUDED.meta_value
		`, volumeID, fmt.Sprintf("%t", readonly))

		if err != nil {
			log.Error().Err(err).Str("operation", "update_readonly_flag").Str("volume_id", volumeID).Msg("failed to update readonly flag")
			common.SendError(c, common.NewInternalServerError("failed to update readonly flag"))
			return
		}

		c.Status(http.StatusAccepted)
		return
	}

	// Handle set image metadata action (make volume bootable)
	if imageMetadataData, ok := req["os-set_image_metadata"]; ok {
		imageMetadataMap, ok := imageMetadataData.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-set_image_metadata must be an object"))
			return
		}
		metadata, ok := imageMetadataMap["metadata"].(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-set_image_metadata.metadata must be an object"))
			return
		}

		// Store image metadata to make volume bootable
		for key, value := range metadata {
			valueStr := fmt.Sprintf("%v", value)
			_, err := svc.pool.Exec(c.Request.Context(), `
				INSERT INTO volume_metadata (volume_id, meta_key, meta_value)
				VALUES ($1, $2, $3)
				ON CONFLICT (volume_id, meta_key) DO UPDATE SET meta_value = EXCLUDED.meta_value
			`, volumeID, "volume_image_"+key, valueStr)

			if err != nil {
				log.Error().Err(err).Str("operation", "set_image_metadata").Str("volume_id", volumeID).Msg("failed to set image metadata")
				common.SendError(c, common.NewInternalServerError("failed to set image metadata"))
				return
			}
		}

		c.JSON(http.StatusOK, gin.H{})
		return
	}

	// Handle unset image metadata action
	if unsetImageMetadata, ok := req["os-unset_image_metadata"]; ok {
		unsetMap, ok := unsetImageMetadata.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-unset_image_metadata must be an object"))
			return
		}
		key, ok := unsetMap["key"].(string)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-unset_image_metadata.key must be a string"))
			return
		}

		// Delete metadata entry with prefixed key
		metadataKey := "volume_image_" + key
		_, err := svc.pool.Exec(c.Request.Context(), `
			DELETE FROM volume_metadata
			WHERE volume_id = $1 AND meta_key = $2
		`, volumeID, metadataKey)

		if err != nil {
			log.Error().Err(err).Str("operation", "unset_image_metadata").Str("volume_id", volumeID).Msg("failed to unset image metadata")
			common.SendError(c, common.NewInternalServerError("failed to unset image metadata"))
			return
		}

		c.Status(http.StatusOK)
		return
	}

	// Handle reimage volume action
	if reimageData, ok := req["os-reimage"]; ok {
		reimageMap, ok := reimageData.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-reimage must be an object"))
			return
		}
		imageID, ok := reimageMap["image_id"].(string)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-reimage.image_id must be a string"))
			return
		}
		// The image must be one the caller can read, and active (SPEC-020 FR-026); upstream's
		// answer otherwise is Glance's not-found.
		status, found, err := common.VisibleImage(c.Request.Context(), svc.pool, imageID, projectID, common.HasRole(c, common.RoleAdmin))
		if err != nil {
			log.Error().Err(err).Str("operation", "reimage_check_image").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to reimage volume"))
			return
		}
		if !found {
			common.SendError(c, common.NewNotFoundError("Image "+imageID))
			return
		}
		if status != "active" {
			common.SendError(c, common.NewBadRequestError(imageNotActive(imageID)))
			return
		}

		// Update volume to bootable and store image ID
		_, err = svc.pool.Exec(c.Request.Context(), `
			UPDATE volumes
			SET bootable = true, updated_at = $1
			WHERE id = $2 AND project_id = $3
		`, time.Now(), volumeID, projectID)

		if err != nil {
			log.Error().Err(err).Str("operation", "reimage_volume").Str("volume_id", volumeID).Msg("failed to reimage volume")
			common.SendError(c, common.NewInternalServerError("failed to reimage volume"))
			return
		}

		// Store image_id as metadata
		_, err = svc.pool.Exec(c.Request.Context(), `
			INSERT INTO volume_metadata (volume_id, meta_key, meta_value, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (volume_id, meta_key)
			DO UPDATE SET meta_value = EXCLUDED.meta_value, updated_at = EXCLUDED.updated_at
		`, volumeID, "volume_image_id", imageID, time.Now(), time.Now())

		if err != nil {
			log.Error().Err(err).Str("operation", "reimage_volume_metadata").Str("volume_id", volumeID).Msg("failed to store image metadata")
			common.SendError(c, common.NewInternalServerError("failed to store image metadata"))
			return
		}

		c.Status(http.StatusAccepted)
		return
	}

	// Handle force detach action. Like os-detach it only clears the record; it does not
	// detach anything from the hypervisor.
	if _, ok := req["os-force_detach"]; ok {
		svc.detachVolume(c, volumeID, projectID, "force_detach_volume", "failed to force detach volume")
		return
	}

	// Handle reset status action (admin operation)
	if resetStatusData, ok := req["os-reset_status"]; ok {
		// Verify admin role
		if !common.HasRole(c, common.RoleAdmin) {
			common.SendError(c, common.NewForbiddenError("Policy doesn't allow os-reset_status to be performed."))
			return
		}

		resetStatusMap, ok := resetStatusData.(map[string]any)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-reset_status must be an object"))
			return
		}
		newStatus, ok := resetStatusMap["status"].(string)
		if !ok {
			common.SendError(c, common.NewBadRequestError("os-reset_status.status must be a string"))
			return
		}

		// Admin operation to manually set volume status
		_, err := svc.pool.Exec(c.Request.Context(), `
			UPDATE volumes
			SET status = $1, updated_at = $2
			WHERE id = $3 AND project_id = $4
		`, newStatus, time.Now(), volumeID, projectID)

		if err != nil {
			log.Error().Err(err).Str("operation", "reset_volume_status").Str("volume_id", volumeID).Msg("failed to reset volume status")
			common.SendError(c, common.NewInternalServerError("failed to reset volume status"))
			return
		}

		c.Status(http.StatusAccepted)
		return
	}

	common.SendError(c, common.NewBadRequestError("unknown action"))
}

// Snapshot operations

// CreateSnapshotRequest represents a snapshot creation request
type CreateSnapshotRequest struct {
	Snapshot struct {
		Name        string `json:"name"`
		VolumeID    string `json:"volume_id" binding:"required"`
		Description string `json:"description"`
		Force       bool   `json:"force"`
	} `json:"snapshot"`
}

// CreateSnapshot creates a new snapshot
func (svc *Service) CreateSnapshot(c *gin.Context) {
	var req CreateSnapshotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	snapshotID := uuid.New().String()

	// Get volume info
	var volumeID string
	var size int
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT id, size_gb FROM volumes WHERE id = $1 AND project_id = $2",
		req.Snapshot.VolumeID, projectID,
	).Scan(&volumeID, &size)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}

	// Create snapshot in Ceph
	if err := svc.cephClient.CreateSnapshot(c.Request.Context(), volumeID, snapshotID); err != nil {
		log.Error().Err(err).Str("operation", "create_snapshot_ceph").Str("volume_id", volumeID).Msg("failed to create snapshot in Ceph")
		common.SendError(c, common.NewServiceUnavailableError("failed to create snapshot"))
		return
	}

	// Insert into database
	now := time.Now()
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO snapshots (id, name, description, volume_id, project_id, size_gb, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, snapshotID, req.Snapshot.Name, req.Snapshot.Description, volumeID, projectID, size, "creating", now)

	if err != nil {
		// Rollback: delete from Ceph (best-effort; the DB error is the user-facing one)
		if cephErr := svc.cephClient.DeleteSnapshot(c.Request.Context(), volumeID, snapshotID); cephErr != nil {
			log.Warn().Err(cephErr).Str("snapshot_id", snapshotID).Str("volume_id", volumeID).Msg("rollback: failed to delete snapshot from Ceph after DB insert failure")
		}
		log.Error().Err(err).Str("operation", "create_snapshot_db").Msg("failed to insert snapshot into database")
		common.SendError(c, common.NewInternalServerError("failed to create snapshot"))
		return
	}

	// Update status to available
	svc.wg.Add(1)
	go func() {
		defer svc.wg.Done()
		select {
		case <-time.After(100 * time.Millisecond):
		case <-svc.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(svc.ctx, 5*time.Second)
		defer cancel()
		if _, err := svc.pool.Exec(ctx,
			"UPDATE snapshots SET status = $1 WHERE id = $2",
			"available", snapshotID); err != nil {
			log.Error().Err(err).Str("snapshot_id", snapshotID).Msg("failed to update snapshot status to available")
		}
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"snapshot": gin.H{
			"id":          snapshotID,
			"name":        req.Snapshot.Name,
			"description": req.Snapshot.Description,
			"volume_id":   volumeID,
			"size":        size,
			"status":      "creating",
			"created_at":  now.Format("2006-01-02T15:04:05.000000"),
		},
	})
}

// ListSnapshotsDetail is an alias that works with both URL patterns
func (svc *Service) ListSnapshotsDetail(c *gin.Context) {
	projectID := c.GetString("project_id")

	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, name, COALESCE(description, ''), volume_id, size_gb, status, created_at
		FROM snapshots
		WHERE project_id = $1
		ORDER BY created_at DESC, id DESC
	`, projectID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_snapshots_detail").Msg("failed to query snapshots")
		common.SendError(c, common.NewInternalServerError("failed to list snapshots"))
		return
	}
	defer rows.Close()

	var snapshots []gin.H
	for rows.Next() {
		var id, name, description, volumeID, status string
		var size int
		var createdAt time.Time

		if err := rows.Scan(&id, &name, &description, &volumeID, &size, &status, &createdAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan snapshot row")
			continue
		}

		snapshots = append(snapshots, gin.H{
			"id":          id,
			"name":        name,
			"description": description,
			"volume_id":   volumeID,
			"size":        size,
			"status":      status,
			"created_at":  createdAt.Format("2006-01-02T15:04:05.000000"),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if snapshots == nil {
		snapshots = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"snapshots": snapshots})
}

// GetDefaultVolumeType returns the default volume type
func (svc *Service) GetDefaultVolumeType(c *gin.Context) {
	// Return the default volume type for Horizon compatibility. The name matches
	// what a typeless volume reads back via serializeVolume (F9, spec Q2).
	c.JSON(http.StatusOK, gin.H{
		"volume_type": gin.H{
			"id":          defaultVolumeType,
			"name":        defaultVolumeType,
			"description": "Default volume type",
			"is_public":   true,
		},
	})
}

// ListSnapshots lists all snapshots
func (svc *Service) ListSnapshots(c *gin.Context) {
	projectID := c.GetString("project_id")

	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, name, COALESCE(description, ''), volume_id, size_gb, status, created_at
		FROM snapshots
		WHERE project_id = $1
		ORDER BY created_at DESC, id DESC
	`, projectID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_snapshots").Msg("failed to query snapshots")
		common.SendError(c, common.NewInternalServerError("failed to list snapshots"))
		return
	}
	defer rows.Close()

	var snapshots []gin.H
	for rows.Next() {
		var id, name, description, volumeID, status string
		var size int
		var createdAt time.Time

		if err := rows.Scan(&id, &name, &description, &volumeID, &size, &status, &createdAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan snapshot row")
			continue
		}

		snapshots = append(snapshots, gin.H{
			"id":          id,
			"name":        name,
			"description": description,
			"volume_id":   volumeID,
			"size":        size,
			"status":      status,
			"created_at":  createdAt.Format("2006-01-02T15:04:05.000000"),
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if snapshots == nil {
		snapshots = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"snapshots": snapshots})
}

// GetSnapshot returns a single snapshot
func (svc *Service) GetSnapshot(c *gin.Context) {
	snapshotID := c.Param("id")
	projectID := c.GetString("project_id")

	var id, name, description, volumeID, status string
	var size int
	var createdAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, COALESCE(description, ''), volume_id, size_gb, status, created_at
		FROM snapshots
		WHERE id = $1 AND project_id = $2
	`, snapshotID, projectID).Scan(&id, &name, &description, &volumeID, &size, &status, &createdAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("snapshot"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_snapshot").Str("snapshot_id", snapshotID).Msg("failed to query snapshot")
		common.SendError(c, common.NewInternalServerError("failed to get snapshot"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"snapshot": gin.H{
			"id":          id,
			"name":        name,
			"description": description,
			"volume_id":   volumeID,
			"size":        size,
			"status":      status,
			"created_at":  createdAt.Format("2006-01-02T15:04:05.000000"),
		},
	})
}

// DeleteSnapshot deletes a snapshot
func (svc *Service) DeleteSnapshot(c *gin.Context) {
	snapshotID := c.Param("id")
	projectID := c.GetString("project_id")

	// Get volume ID
	var volumeID string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT volume_id FROM snapshots WHERE id = $1 AND project_id = $2",
		snapshotID, projectID,
	).Scan(&volumeID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("snapshot"))
		return
	}

	// Delete from Ceph
	if err := svc.cephClient.DeleteSnapshot(c.Request.Context(), volumeID, snapshotID); err != nil {
		log.Error().Err(err).Str("operation", "delete_snapshot_ceph").Str("snapshot_id", snapshotID).Msg("failed to delete snapshot from Ceph")
		common.SendError(c, common.NewServiceUnavailableError("failed to delete snapshot"))
		return
	}

	// Delete from database
	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM snapshots WHERE id = $1 AND project_id = $2",
		snapshotID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_snapshot_db").Str("snapshot_id", snapshotID).Msg("failed to delete snapshot from database")
		common.SendError(c, common.NewInternalServerError("failed to delete snapshot"))
		return
	}

	c.Status(http.StatusNoContent)
}

// Volume types

// ListVolumeTypes lists all volume types
func (svc *Service) ListVolumeTypes(c *gin.Context) {
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT id, name, description, is_public, created_at
		FROM volume_types
		ORDER BY name
	`)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_volume_types").Msg("failed to query volume types")
		common.SendError(c, common.NewInternalServerError("failed to list volume types"))
		return
	}
	defer rows.Close()

	var types []gin.H
	for rows.Next() {
		var id, name, description string
		var isPublic bool
		var createdAt time.Time

		if err := rows.Scan(&id, &name, &description, &isPublic, &createdAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan volume type row")
			continue
		}

		types = append(types, gin.H{
			"id":          id,
			"name":        name,
			"description": description,
			"is_public":   isPublic,
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if types == nil {
		types = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"volume_types": types})
}

// GetVolumeType returns a single volume type
func (svc *Service) GetVolumeType(c *gin.Context) {
	typeID := c.Param("id")

	var id, name, description string
	var isPublic bool

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, COALESCE(description, ''), is_public
		FROM volume_types
		WHERE id = $1
	`, typeID).Scan(&id, &name, &description, &isPublic)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume type"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_volume_type").Str("type_id", typeID).Msg("failed to query volume type")
		common.SendError(c, common.NewInternalServerError("failed to get volume type"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"volume_type": gin.H{
			"id":          id,
			"name":        name,
			"description": description,
			"is_public":   isPublic,
		},
	})
}

// UpdateVolume updates volume properties
func (svc *Service) UpdateVolume(c *gin.Context) {
	volumeID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Volume struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		} `json:"volume"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Check volume exists and belongs to project
	var currentName, currentDesc string
	var sizeGB int
	var status string
	var createdAt, updatedAt time.Time
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT name, COALESCE(description, ''), size_gb, status, created_at, updated_at FROM volumes WHERE id = $1 AND project_id = $2",
		volumeID, projectID,
	).Scan(&currentName, &currentDesc, &sizeGB, &status, &createdAt, &updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_volume_check").Str("volume_id", volumeID).Msg("failed to query volume")
		common.SendError(c, common.NewInternalServerError("failed to update volume"))
		return
	}

	// Apply updates
	if req.Volume.Name != nil {
		currentName = *req.Volume.Name
	}
	if req.Volume.Description != nil {
		currentDesc = *req.Volume.Description
	}

	now := time.Now()
	_, err = svc.pool.Exec(c.Request.Context(),
		"UPDATE volumes SET name = $1, description = $2, updated_at = $3 WHERE id = $4",
		currentName, currentDesc, now, volumeID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_volume").Str("volume_id", volumeID).Msg("failed to update volume")
		common.SendError(c, common.NewInternalServerError("failed to update volume"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"volume": gin.H{
			"id":          volumeID,
			"name":        currentName,
			"description": currentDesc,
			"size":        sizeGB,
			"status":      status,
			"created_at":  createdAt.Format("2006-01-02T15:04:05.000000"),
			"updated_at":  now.Format("2006-01-02T15:04:05.000000"),
		},
	})
}

// UpdateSnapshot updates snapshot properties
func (svc *Service) UpdateSnapshot(c *gin.Context) {
	snapshotID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Snapshot struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		} `json:"snapshot"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Check snapshot exists and belongs to project
	var currentName, currentDesc string
	var volumeID string
	var sizeGB int
	var status string
	var createdAt time.Time
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT name, COALESCE(description, ''), volume_id, size_gb, status, created_at FROM snapshots WHERE id = $1 AND project_id = $2",
		snapshotID, projectID,
	).Scan(&currentName, &currentDesc, &volumeID, &sizeGB, &status, &createdAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("snapshot"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_snapshot_check").Str("snapshot_id", snapshotID).Msg("failed to query snapshot")
		common.SendError(c, common.NewInternalServerError("failed to update snapshot"))
		return
	}

	// Apply updates
	if req.Snapshot.Name != nil {
		currentName = *req.Snapshot.Name
	}
	if req.Snapshot.Description != nil {
		currentDesc = *req.Snapshot.Description
	}

	_, err = svc.pool.Exec(c.Request.Context(),
		"UPDATE snapshots SET name = $1, description = $2 WHERE id = $3",
		currentName, currentDesc, snapshotID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_snapshot").Str("snapshot_id", snapshotID).Msg("failed to update snapshot")
		common.SendError(c, common.NewInternalServerError("failed to update snapshot"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"snapshot": gin.H{
			"id":          snapshotID,
			"name":        currentName,
			"description": currentDesc,
			"volume_id":   volumeID,
			"size":        sizeGB,
			"status":      status,
			"created_at":  createdAt.Format("2006-01-02T15:04:05.000000"),
		},
	})
}

// GetVolumeMetadata returns all metadata for a volume
func (svc *Service) GetVolumeMetadata(c *gin.Context) {
	volumeID := c.Param("id")
	projectID := c.GetString("project_id")

	// Check volume exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM volumes WHERE id = $1 AND project_id = $2)",
		volumeID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}

	rows, err := svc.pool.Query(c.Request.Context(),
		"SELECT meta_key, meta_value FROM volume_metadata WHERE volume_id = $1",
		volumeID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_volume_metadata").Str("volume_id", volumeID).Msg("failed to query volume metadata")
		common.SendError(c, common.NewInternalServerError("failed to get volume metadata"))
		return
	}
	defer rows.Close()

	metadata := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			log.Warn().Err(err).Msg("failed to scan metadata row")
			continue
		}
		metadata[key] = value
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"metadata": metadata})
}

// SetVolumeMetadata sets/replaces all metadata for a volume
func (svc *Service) SetVolumeMetadata(c *gin.Context) {
	volumeID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Metadata map[string]string `json:"metadata"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Check volume exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM volumes WHERE id = $1 AND project_id = $2)",
		volumeID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}

	// Delete existing metadata then insert new metadata atomically
	if err = database.WithTx(c.Request.Context(), svc.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c.Request.Context(),
			"DELETE FROM volume_metadata WHERE volume_id = $1",
			volumeID,
		); err != nil {
			return fmt.Errorf("delete_volume_metadata: %w", err)
		}
		for key, value := range req.Metadata {
			if _, err := tx.Exec(c.Request.Context(), `
				INSERT INTO volume_metadata (volume_id, meta_key, meta_value)
				VALUES ($1, $2, $3)
			`, volumeID, key, value); err != nil {
				return fmt.Errorf("insert_volume_metadata: %w", err)
			}
		}
		return nil
	}); err != nil {
		log.Error().Err(err).Str("operation", "set_volume_metadata").Str("volume_id", volumeID).Msg("failed to set volume metadata")
		common.SendError(c, common.NewInternalServerError("failed to set volume metadata"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"metadata": req.Metadata})
}

// GetVolumeMetadataKey returns a single metadata key
func (svc *Service) GetVolumeMetadataKey(c *gin.Context) {
	volumeID := c.Param("id")
	key := c.Param("key")
	projectID := c.GetString("project_id")

	// Check volume exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM volumes WHERE id = $1 AND project_id = $2)",
		volumeID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}

	var value string
	err = svc.pool.QueryRow(c.Request.Context(),
		"SELECT meta_value FROM volume_metadata WHERE volume_id = $1 AND meta_key = $2",
		volumeID, key,
	).Scan(&value)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("metadata key"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_volume_metadata_key").Str("volume_id", volumeID).Msg("failed to query volume metadata key")
		common.SendError(c, common.NewInternalServerError("failed to get metadata key"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"meta": map[string]string{key: value}})
}

// UpdateVolumeMetadataKey updates a single metadata key
func (svc *Service) UpdateVolumeMetadataKey(c *gin.Context) {
	volumeID := c.Param("id")
	key := c.Param("key")
	projectID := c.GetString("project_id")

	var req struct {
		Meta map[string]string `json:"meta"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	value, ok := req.Meta[key]
	if !ok {
		common.SendError(c, common.NewBadRequestError("key not found in request body"))
		return
	}

	// Check volume exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM volumes WHERE id = $1 AND project_id = $2)",
		volumeID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}

	// Upsert metadata
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO volume_metadata (volume_id, meta_key, meta_value, updated_at)
		VALUES ($1, $2, $3, CURRENT_TIMESTAMP)
		ON CONFLICT (volume_id, meta_key)
		DO UPDATE SET meta_value = $3, updated_at = CURRENT_TIMESTAMP
	`, volumeID, key, value)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_volume_metadata_key").Str("volume_id", volumeID).Msg("failed to update volume metadata key")
		common.SendError(c, common.NewInternalServerError("failed to update metadata key"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"meta": map[string]string{key: value}})
}

// DeleteVolumeMetadataKey deletes a single metadata key
func (svc *Service) DeleteVolumeMetadataKey(c *gin.Context) {
	volumeID := c.Param("id")
	key := c.Param("key")
	projectID := c.GetString("project_id")

	// Check volume exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM volumes WHERE id = $1 AND project_id = $2)",
		volumeID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("volume"))
		return
	}

	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM volume_metadata WHERE volume_id = $1 AND meta_key = $2",
		volumeID, key,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_volume_metadata_key").Str("volume_id", volumeID).Msg("failed to delete volume metadata key")
		common.SendError(c, common.NewInternalServerError("failed to delete metadata key"))
		return
	}

	c.Status(http.StatusNoContent)
}

// GetSnapshotMetadata returns all metadata for a snapshot
func (svc *Service) GetSnapshotMetadata(c *gin.Context) {
	snapshotID := c.Param("id")
	projectID := c.GetString("project_id")

	// Check snapshot exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM snapshots WHERE id = $1 AND project_id = $2)",
		snapshotID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("snapshot"))
		return
	}

	rows, err := svc.pool.Query(c.Request.Context(),
		"SELECT meta_key, meta_value FROM snapshot_metadata WHERE snapshot_id = $1",
		snapshotID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_snapshot_metadata").Str("snapshot_id", snapshotID).Msg("failed to query snapshot metadata")
		common.SendError(c, common.NewInternalServerError("failed to get snapshot metadata"))
		return
	}
	defer rows.Close()

	metadata := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			log.Warn().Err(err).Msg("failed to scan metadata row")
			continue
		}
		metadata[key] = value
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"metadata": metadata})
}

// SetSnapshotMetadata sets/replaces all metadata for a snapshot
func (svc *Service) SetSnapshotMetadata(c *gin.Context) {
	snapshotID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Metadata map[string]string `json:"metadata"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Check snapshot exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM snapshots WHERE id = $1 AND project_id = $2)",
		snapshotID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("snapshot"))
		return
	}

	// Delete existing metadata then insert new metadata atomically
	if err = database.WithTx(c.Request.Context(), svc.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(c.Request.Context(),
			"DELETE FROM snapshot_metadata WHERE snapshot_id = $1",
			snapshotID,
		); err != nil {
			return fmt.Errorf("delete_snapshot_metadata: %w", err)
		}
		for key, value := range req.Metadata {
			if _, err := tx.Exec(c.Request.Context(), `
				INSERT INTO snapshot_metadata (snapshot_id, meta_key, meta_value)
				VALUES ($1, $2, $3)
			`, snapshotID, key, value); err != nil {
				return fmt.Errorf("insert_snapshot_metadata: %w", err)
			}
		}
		return nil
	}); err != nil {
		log.Error().Err(err).Str("operation", "set_snapshot_metadata").Str("snapshot_id", snapshotID).Msg("failed to set snapshot metadata")
		common.SendError(c, common.NewInternalServerError("failed to set snapshot metadata"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"metadata": req.Metadata})
}

// GetSnapshotMetadataKey returns a single metadata key
func (svc *Service) GetSnapshotMetadataKey(c *gin.Context) {
	snapshotID := c.Param("id")
	key := c.Param("key")
	projectID := c.GetString("project_id")

	// Check snapshot exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM snapshots WHERE id = $1 AND project_id = $2)",
		snapshotID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("snapshot"))
		return
	}

	var value string
	err = svc.pool.QueryRow(c.Request.Context(),
		"SELECT meta_value FROM snapshot_metadata WHERE snapshot_id = $1 AND meta_key = $2",
		snapshotID, key,
	).Scan(&value)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("metadata key"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_snapshot_metadata_key").Str("snapshot_id", snapshotID).Msg("failed to query snapshot metadata key")
		common.SendError(c, common.NewInternalServerError("failed to get metadata key"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"meta": map[string]string{key: value}})
}

// UpdateSnapshotMetadataKey updates a single metadata key
func (svc *Service) UpdateSnapshotMetadataKey(c *gin.Context) {
	snapshotID := c.Param("id")
	key := c.Param("key")
	projectID := c.GetString("project_id")

	var req struct {
		Meta map[string]string `json:"meta"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	value, ok := req.Meta[key]
	if !ok {
		common.SendError(c, common.NewBadRequestError("key not found in request body"))
		return
	}

	// Check snapshot exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM snapshots WHERE id = $1 AND project_id = $2)",
		snapshotID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("snapshot"))
		return
	}

	// Upsert metadata
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO snapshot_metadata (snapshot_id, meta_key, meta_value, updated_at)
		VALUES ($1, $2, $3, CURRENT_TIMESTAMP)
		ON CONFLICT (snapshot_id, meta_key)
		DO UPDATE SET meta_value = $3, updated_at = CURRENT_TIMESTAMP
	`, snapshotID, key, value)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_snapshot_metadata_key").Str("snapshot_id", snapshotID).Msg("failed to update snapshot metadata key")
		common.SendError(c, common.NewInternalServerError("failed to update metadata key"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"meta": map[string]string{key: value}})
}

// DeleteSnapshotMetadataKey deletes a single metadata key
func (svc *Service) DeleteSnapshotMetadataKey(c *gin.Context) {
	snapshotID := c.Param("id")
	key := c.Param("key")
	projectID := c.GetString("project_id")

	// Check snapshot exists and belongs to project
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM snapshots WHERE id = $1 AND project_id = $2)",
		snapshotID, projectID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("snapshot"))
		return
	}

	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM snapshot_metadata WHERE snapshot_id = $1 AND meta_key = $2",
		snapshotID, key,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_snapshot_metadata_key").Str("snapshot_id", snapshotID).Msg("failed to delete snapshot metadata key")
		common.SendError(c, common.NewInternalServerError("failed to delete metadata key"))
		return
	}

	c.Status(http.StatusNoContent)
}

// ListExtensions lists the Cinder v3 API extensions Lightstack implements. Horizon's
// volume-create form calls extension_supported() while building the dialog, so a
// 404 here raises during form construction and the modal never opens (SPEC-020 F8).
// Only extensions Lightstack genuinely implements are advertised.
func (svc *Service) ListExtensions(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"extensions": []gin.H{
			{
				"alias":       "os-availability-zone",
				"name":        "AvailabilityZones",
				"description": "Describe Availability Zones.",
				"updated":     "2013-06-27T00:00:00+00:00",
				"links":       []gin.H{},
			},
		},
	})
}

// GetLimits returns volume service limits and quotas for the caller's project.
// Limits come from the shared helper (defaults overlaid with cinder_quotas), so this
// view agrees with the quota-set views and reflects an admin's quota override
// (SPEC-020 FR-003) — previously every maxTotal* value here was hardcoded and the
// cinder_quotas table was ignored entirely.
func (svc *Service) GetLimits(c *gin.Context) {
	projectID := c.GetString("project_id")
	// An unscoped token carries no project, and its empty id would reach the uuid-typed
	// queries below — a 500. Answered as the quota routes answer it (QuotaTargetProject).
	if projectID == "" {
		common.SendError(c, common.NewNotFoundError("project"))
		return
	}

	limits, err := resolveCinderQuotaLimits(c.Request.Context(), svc.pool, projectID)
	if err != nil {
		log.Error().Err(err).Str("project_id", projectID).Msg("failed to resolve cinder quota limits")
		common.SendError(c, common.NewInternalServerError("failed to get limits"))
		return
	}

	// Query current usage. A count that fails answers 500, never zero usage.
	var volumesUsed, snapshotsUsed, gigabytesUsed, backupsUsed, backupGigabytesUsed int

	if err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COUNT(*), COALESCE(SUM(size_gb), 0) FROM volumes WHERE project_id = $1 AND status != 'deleted'",
		projectID,
	).Scan(&volumesUsed, &gigabytesUsed); err != nil {
		log.Error().Err(err).Str("project_id", projectID).Msg("failed to query volume usage for limits")
		common.SendError(c, common.NewInternalServerError("failed to get limits"))
		return
	}

	if err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COUNT(*) FROM snapshots WHERE project_id = $1 AND status != 'deleted'",
		projectID,
	).Scan(&snapshotsUsed); err != nil {
		log.Error().Err(err).Str("project_id", projectID).Msg("failed to query snapshot usage for limits")
		common.SendError(c, common.NewInternalServerError("failed to get limits"))
		return
	}

	// volume_backups (migration 022) carries no 'deleted' status — DeleteBackup removes
	// the row — so this counts every row for the project.
	if err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT COUNT(*), COALESCE(SUM(size_gb), 0) FROM volume_backups WHERE project_id = $1",
		projectID,
	).Scan(&backupsUsed, &backupGigabytesUsed); err != nil {
		log.Error().Err(err).Str("project_id", projectID).Msg("failed to query backup usage for limits")
		common.SendError(c, common.NewInternalServerError("failed to get limits"))
		return
	}

	c.JSON(200, gin.H{
		"limits": gin.H{
			"rate": []gin.H{}, // No rate limiting
			"absolute": gin.H{
				"maxTotalVolumes":          limits["volumes"],
				"maxTotalSnapshots":        limits["snapshots"],
				"maxTotalVolumeGigabytes":  limits["gigabytes"],
				"maxTotalBackups":          limits["backups"],
				"maxTotalBackupGigabytes":  limits["backup_gigabytes"],
				"totalVolumesUsed":         volumesUsed,
				"totalSnapshotsUsed":       snapshotsUsed,
				"totalGigabytesUsed":       gigabytesUsed,
				"totalBackupsUsed":         backupsUsed,
				"totalBackupGigabytesUsed": backupGigabytesUsed,
			},
		},
	})
}

// GetLimitsNoProject serves the limits endpoint registered without a project_id in
// the URL. The project always comes from the token, so it simply delegates to
// GetLimits rather than duplicating its body.
func (svc *Service) GetLimitsNoProject(c *gin.Context) {
	svc.GetLimits(c)
}

// ListServices returns list of volume services
func (svc *Service) ListServices(c *gin.Context) {
	// Format: OpenStack timestamp without Z
	now := time.Now().Format("2006-01-02T15:04:05.000000")

	// Return list of volume services for Horizon System Info
	c.JSON(200, gin.H{
		"services": []gin.H{
			{
				"binary":          "cinder-volume",
				"host":            "lightstack-volume-1",
				"zone":            "nova",
				"status":          "enabled",
				"state":           "up",
				"updated_at":      now,
				"disabled_reason": nil,
			},
			{
				"binary":          "cinder-scheduler",
				"host":            "lightstack-controller",
				"zone":            "internal",
				"status":          "enabled",
				"state":           "up",
				"updated_at":      now,
				"disabled_reason": nil,
			},
			{
				"binary":          "cinder-backup",
				"host":            "lightstack-backup-1",
				"zone":            "nova",
				"status":          "enabled",
				"state":           "up",
				"updated_at":      now,
				"disabled_reason": nil,
			},
		},
	})
}

// GetVersions returns the root version discovery response
func (svc *Service) GetVersions(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"versions": []gin.H{
			{
				"id":          "v3.0",
				"status":      "CURRENT",
				"version":     "3.70",
				"min_version": "3.0",
				"updated":     "2021-04-07T00:00:00Z",
				"links": []gin.H{
					{
						"rel":  "self",
						"href": "http://lightstack:8776/v3/",
					},
				},
				"media-types": []gin.H{
					{
						"base": "application/json",
						"type": "application/vnd.openstack.volume+json;version=3",
					},
				},
			},
		},
	})
}

// GetVersionV3 returns the v3 version information
func (svc *Service) GetVersionV3(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version": gin.H{
			"id":          "v3.0",
			"status":      "CURRENT",
			"version":     "3.71",
			"min_version": "3.0",
			"updated":     "2021-04-07T00:00:00Z",
			"links": []gin.H{
				{
					"rel":  "self",
					"href": "http://lightstack:8776/v3/",
				},
			},
			"media-types": []gin.H{
				{
					"base": "application/json",
					"type": "application/vnd.openstack.volume+json;version=3",
				},
			},
		},
	})
}
