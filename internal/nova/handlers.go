package nova

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/middleware"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// Service handles Nova API endpoints
type Service struct {
	libvirtURI  string
	libvirtMode string
	vmManager   *hypervisor.VMManager
	cache       *cache.Cache
	imageStore  *storage.ImageStore // lazy fetch-on-boot of S3/RGW-backed image bases (SPEC-014)
	neutronSvc  NeutronService      // For port allocation
	wg          sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
}

const (
	novaMinVersion     = "2.1"
	novaCurrentVersion = "2.90"
)

// NeutronService defines the interface for Neutron operations Nova needs
type NeutronService interface {
	AllocatePortForInstance(ctx context.Context, networkID, projectID, instanceID string, securityGroupIDs []string) (any, error)
}

// NewService creates a new Nova service
func NewService(libvirtURI, libvirtMode string, cacheInstance *cache.Cache) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		libvirtURI:  libvirtURI,
		libvirtMode: libvirtMode,
		cache:       cacheInstance,
		neutronSvc:  nil, // Set via SetNeutronService after initialization
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Shutdown signals all background goroutines to stop and waits for them.
func (svc *Service) Shutdown() {
	svc.cancel()
	svc.wg.Wait()
}

// SetImageStore wires the Glance image store into Nova so real-mode boots can
// lazily materialize an S3/RGW-backed image's raw base on this node before
// staging the overlay (SPEC-014 FR-013).
func (svc *Service) SetImageStore(s *storage.ImageStore) {
	svc.imageStore = s
}

// reclaimImageBaseIfUnused removes the node-local raw base for imageID once no
// instance or image-backed volume references it, bounding the lazily-fetched
// image cache (SPEC-014 FR-016). Best-effort; only meaningful in real mode.
// The refcount is cluster-wide (instances has no host column): exact on a single
// node, and safely conservative on multi-node (never evicts a base still in use).
func (svc *Service) reclaimImageBaseIfUnused(imageID string) {
	if imageID == "" || svc.libvirtMode != "real" {
		return
	}
	var inUse bool
	if err := database.DB.QueryRow(svc.ctx, `
		SELECT EXISTS(
			SELECT 1 FROM instances WHERE image_id = $1
			UNION ALL
			SELECT 1 FROM volumes WHERE source_image_id = $1)`, imageID).Scan(&inUse); err != nil {
		log.Warn().Err(err).Str("image_id", imageID).Msg("cache eviction: refcount query failed; keeping base")
		return
	}
	if inUse {
		return
	}
	base := storage.LocalImagePath(imageID)
	if err := os.Remove(base); err != nil && !os.IsNotExist(err) {
		log.Warn().Err(err).Str("path", base).Msg("cache eviction: failed to remove unused image base")
		return
	}
	log.Info().Str("image_id", imageID).Msg("cache eviction: reclaimed unused local image base")
}

// SetNeutronService sets the Neutron service reference (called after both services are created)
func (svc *Service) SetNeutronService(neutron NeutronService) {
	svc.neutronSvc = neutron
}

// InitHypervisor initializes the hypervisor connection
func (svc *Service) InitHypervisor() error {
	vmManager, err := hypervisor.NewVMManager(svc.libvirtURI, svc.libvirtMode)
	if err != nil {
		return fmt.Errorf("failed to initialize hypervisor: %w", err)
	}
	svc.vmManager = vmManager
	return nil
}

// RegisterRoutes registers Nova routes
func (svc *Service) RegisterRoutes(r *gin.RouterGroup) {
	// Version discovery
	r.GET("/", svc.ListVersions)
	r.GET("/v2.1", svc.GetVersion)

	v21 := r.Group("/v2.1")
	{
		// Servers (instances)
		v21.GET("/servers", svc.ListServers)
		v21.GET("/servers/detail", svc.ListServersDetail)
		v21.POST("/servers", svc.CreateServer)
		v21.GET("/servers/:id", svc.GetServer)
		v21.PATCH("/servers/:id", svc.UpdateServer)
		v21.PUT("/servers/:id", svc.UpdateServer) // OpenStack also supports PUT for updates
		v21.DELETE("/servers/:id", svc.DeleteServer)
		v21.POST("/servers/:id/action", svc.ServerAction)
		v21.GET("/servers/:id/diagnostics", svc.GetServerDiagnostics)
		v21.GET("/servers/:id/os-instance-actions", svc.ListInstanceActions)
		v21.GET("/servers/:id/os-instance-actions/:request_id", svc.GetInstanceAction)

		// Server metadata
		v21.GET("/servers/:id/metadata", svc.GetServerMetadata)
		v21.POST("/servers/:id/metadata", svc.UpdateServerMetadata)
		v21.PUT("/servers/:id/metadata", svc.ResetServerMetadata)

		// Server tags
		v21.GET("/servers/:id/tags", svc.ListServerTags)
		v21.PUT("/servers/:id/tags", svc.ReplaceServerTags)
		v21.DELETE("/servers/:id/tags", svc.DeleteAllServerTags)
		v21.PUT("/servers/:id/tags/:tag", svc.AddServerTag)
		v21.DELETE("/servers/:id/tags/:tag", svc.DeleteServerTag)

		// Flavors
		v21.GET("/flavors", svc.ListFlavors)
		v21.GET("/flavors/detail", svc.ListFlavorsDetail)
		v21.POST("/flavors", svc.CreateFlavor)
		v21.GET("/flavors/:id", svc.GetFlavor)
		v21.DELETE("/flavors/:id", svc.DeleteFlavor)

		// Flavor extra specs
		v21.GET("/flavors/:id/os-extra_specs", svc.GetFlavorExtraSpecs)
		v21.POST("/flavors/:id/os-extra_specs", svc.CreateFlavorExtraSpecs)
		v21.GET("/flavors/:id/os-extra_specs/:key", svc.GetFlavorExtraSpecKey)
		v21.PUT("/flavors/:id/os-extra_specs/:key", svc.UpdateFlavorExtraSpecKey)
		v21.DELETE("/flavors/:id/os-extra_specs/:key", svc.DeleteFlavorExtraSpecKey)

		// Flavor actions and access
		v21.POST("/flavors/:id/action", svc.FlavorAction)
		v21.GET("/flavors/:id/os-flavor-access", svc.GetFlavorAccess)

		// Images (proxy to Glance)
		v21.GET("/images", svc.ListImages)
		v21.GET("/images/detail", svc.ListImagesDetail)

		// Keypairs
		v21.GET("/os-keypairs", svc.ListKeypairs)
		v21.POST("/os-keypairs", svc.CreateKeypair)
		v21.GET("/os-keypairs/:id", svc.GetKeypair)
		v21.DELETE("/os-keypairs/:id", svc.DeleteKeypair)

		// Hypervisors (for Horizon compatibility)
		v21.GET("/os-hypervisors", svc.ListHypervisors)
		v21.GET("/os-hypervisors/detail", svc.ListHypervisorsDetail)
		v21.GET("/os-hypervisors/statistics", svc.GetHypervisorStatistics)

		// Availability zones
		v21.GET("/os-availability-zone", svc.ListAvailabilityZones)
		v21.GET("/os-availability-zone/detail", svc.ListAvailabilityZonesDetail)

		// Limits (quotas and usage)
		v21.GET("/limits", svc.GetLimits)

		// Services (compute service status)
		v21.GET("/os-services", svc.ListServices)

		// Tenant usage (usage statistics)
		v21.GET("/os-simple-tenant-usage", svc.ListTenantUsage)
		v21.GET("/os-simple-tenant-usage/:id", svc.GetTenantUsage)

		// Server groups
		v21.GET("/os-server-groups", svc.ListServerGroups)
		v21.POST("/os-server-groups", svc.CreateServerGroup)
		v21.GET("/os-server-groups/:id", svc.GetServerGroup)
		v21.DELETE("/os-server-groups/:id", svc.DeleteServerGroup)

		// Server migrations
		v21.GET("/os-migrations", svc.ListMigrations)
		v21.GET("/servers/:id/migrations", svc.ListServerMigrations)
		v21.GET("/servers/:id/migrations/:migration_id", svc.GetServerMigration)
		v21.DELETE("/servers/:id/migrations/:migration_id", svc.DeleteServerMigration)
		v21.POST("/servers/:id/migrations/:migration_id/action", svc.ServerMigrationAction)

		// Volume attachments
		v21.GET("/servers/:id/os-volume_attachments", svc.ListVolumeAttachments)
		v21.GET("/servers/:id/os-volume_attachments/:volume_id", svc.GetVolumeAttachment)
		v21.POST("/servers/:id/os-volume_attachments", svc.AttachVolume)
		v21.DELETE("/servers/:id/os-volume_attachments/:volume_id", svc.DetachVolume)

		// Interface attachments (network hot-plug)
		v21.GET("/servers/:id/os-interface", svc.ListInterfaceAttachments)
		v21.POST("/servers/:id/os-interface", svc.AttachInterface)
		v21.DELETE("/servers/:id/os-interface/:port_id", svc.DetachInterface)

		// Quotas
		v21.GET("/os-quota-sets/:id", svc.GetQuotaSet)
		v21.PUT("/os-quota-sets/:id", svc.UpdateQuotaSet)
		v21.GET("/os-quota-sets/:id/defaults", svc.GetQuotaSetDefaults)
		v21.GET("/os-quota-sets/:id/detail", svc.GetQuotaSetDetail)

		// Console access
		v21.POST("/servers/:id/remote-consoles", svc.GetRemoteConsole)

		// Host aggregates
		v21.GET("/os-aggregates", svc.ListAggregates)
		v21.POST("/os-aggregates", svc.CreateAggregate)
		v21.GET("/os-aggregates/:id", svc.GetAggregate)
		v21.PUT("/os-aggregates/:id", svc.UpdateAggregate)
		v21.DELETE("/os-aggregates/:id", svc.DeleteAggregate)
		v21.POST("/os-aggregates/:id/action", svc.AggregateAction)
	}
}

// ListVersions returns available API versions
func (svc *Service) ListVersions(c *gin.Context) {
	c.JSON(200, gin.H{
		"versions": []gin.H{
			{
				"id":          "v2.1",
				"status":      "CURRENT",
				"version":     novaCurrentVersion,
				"min_version": novaMinVersion,
				"links": []gin.H{
					{"rel": "self", "href": fmt.Sprintf("%s/v2.1", common.BaseURL(c, 8774))},
				},
			},
		},
	})
}

// GetVersion returns version details
func (svc *Service) GetVersion(c *gin.Context) {
	c.Header("OpenStack-API-Version", "compute "+novaCurrentVersion)
	c.Header("OpenStack-API-Minimum-Version", "compute "+novaMinVersion)
	c.JSON(200, gin.H{
		"version": gin.H{
			"id":          "v2.1",
			"status":      "CURRENT",
			"version":     novaCurrentVersion,
			"min_version": novaMinVersion,
		},
	})
}

// CreateServerRequest represents a server creation request
type CreateServerRequest struct {
	Server struct {
		Name      string `json:"name" binding:"required"`
		FlavorRef string `json:"flavorRef" binding:"required"`
		ImageRef  string `json:"imageRef"`
		Networks  []struct {
			UUID string `json:"uuid"`
		} `json:"networks"`
		KeyName        string `json:"key_name"`
		SecurityGroups []struct {
			Name *string `json:"name"` // pointer: {} (absent) is ignored like upstream; "" is rejected
		} `json:"security_groups"`
		UserData             string `json:"user_data"`
		BlockDeviceMappingV2 []struct {
			BootIndex           any    `json:"boot_index"`
			UUID                string `json:"uuid"`
			SourceType          string `json:"source_type"`
			DestinationType     string `json:"destination_type"`
			DeleteOnTermination bool   `json:"delete_on_termination"`
		} `json:"block_device_mapping_v2"`
	} `json:"server"`
}

// isRootBootIndex reports whether a block_device_mapping_v2 boot_index marks the root
// disk. boot_index 0 (or unset) is the root. Normalize across the representations the
// JSON decoder yields: gin sets EnableDecoderUseNumber, so a numeric 0 arrives as
// json.Number, not float64 — match on the rendered value to cover number/string/nil.
func isRootBootIndex(bootIndex any) bool {
	switch fmt.Sprintf("%v", bootIndex) {
	case "0", "<nil>", "":
		return true
	}
	return false
}

// bootVolumeID returns the root boot-from-volume id and its delete-on-termination
// flag from a block_device_mapping_v2 (source_type=volume at boot_index 0), or
// ("", false) if the VM boots from an image (SPEC-009 WS-5 / T041, FR-023).
func bootVolumeID(req *CreateServerRequest) (string, bool) {
	for _, bdm := range req.Server.BlockDeviceMappingV2 {
		if bdm.SourceType != "volume" || bdm.UUID == "" {
			continue
		}
		if isRootBootIndex(bdm.BootIndex) {
			return bdm.UUID, bdm.DeleteOnTermination
		}
	}
	return "", false
}

// bootImageRefFromBDM returns the image id carried by a root block_device_mapping_v2
// entry with source_type=image, or "" if there is none.
//
// Horizon's Launch Instance wizard sends the image this way — not in imageRef —
// whenever its "Create New Volume" toggle is on, which is the upstream default:
//
//	{"source_id": "", "block_device_mapping_v2":
//	  [{"source_type": "image", "destination_type": "volume", "uuid": "<image>", "boot_index": "0"}]}
//
// Lightstack used to skip that entry (bootVolumeID only matches source_type=volume) and then
// reject the empty imageRef, leaving the instance in ERROR — so a dashboard launch
// failed on the wizard's default setting.
//
// LIMITATION: Lightstack boots the image directly rather than first materializing a volume
// from it, so volume_size and delete_on_termination on this entry are ignored and no
// volume appears in Cinder. That is enough for the instance to reach ACTIVE, which is
// what the wizard's default path needs; true boot-from-new-volume is tracked
// separately. An explicit imageRef and a real boot volume both take precedence — see
// the caller in CreateServer.
func bootImageRefFromBDM(req *CreateServerRequest) string {
	for _, bdm := range req.Server.BlockDeviceMappingV2 {
		if bdm.SourceType != "image" || bdm.UUID == "" {
			continue
		}
		if isRootBootIndex(bdm.BootIndex) {
			return bdm.UUID
		}
	}
	return ""
}

// markInstanceError flags an instance ERROR (best-effort), used from the
// VM-creation goroutine's failure paths. Uses svc.ctx so it still runs even if
// the request context was cancelled.
func (svc *Service) markInstanceError(instanceID string) {
	if _, err := database.DB.Exec(svc.ctx,
		"UPDATE instances SET status = 'ERROR', updated_at = $1 WHERE id = $2",
		time.Now(), instanceID); err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark instance ERROR")
	}
}

// CreateServer creates a new server instance
func (svc *Service) CreateServer(c *gin.Context) {
	logger := middleware.GetLogger(c)
	start := time.Now()

	var req CreateServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.Warn().Err(err).Msg("Invalid request body for server creation")
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Adopt the image from a root block_device_mapping_v2 (source_type=image) when the
	// caller sent no imageRef — Horizon's launch wizard does this by default. Done once,
	// here, so every downstream reader (the instances row, the libvirt boot path, the
	// response body) sees one image source. A real boot volume still wins: only fall
	// back to the image entry when there is no root source_type=volume BDM.
	if req.Server.ImageRef == "" {
		if rootVol, _ := bootVolumeID(&req); rootVol == "" {
			if imageRef := bootImageRefFromBDM(&req); imageRef != "" {
				req.Server.ImageRef = imageRef
			}
		}
	}

	projectID := c.GetString("project_id")
	userID := c.GetString("user_id")

	// Decode user_data once (OpenStack/Terraform send it base64-encoded). Reused
	// for both the metadata service (T027) and config-drive (T028). Fall back to
	// the raw string if it is not valid base64.
	decodedUserData := req.Server.UserData
	if decodedUserData != "" {
		if decoded, derr := base64.StdEncoding.DecodeString(decodedUserData); derr == nil {
			decodedUserData = string(decoded)
		}
	}

	middleware.LogOperationStart(c, "create", "server", req.Server.Name)

	// Fetch flavor (support lookup by UUID or name)
	var flavor database.Flavor
	queryStart := time.Now()
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT id, name, vcpus, ram_mb, disk_gb FROM flavors WHERE id::text = $1 OR name = $1 LIMIT 1",
		req.Server.FlavorRef,
	).Scan(&flavor.ID, &flavor.Name, &flavor.VCPUs, &flavor.RAMMB, &flavor.DiskGB)
	middleware.LogDatabaseQuery(c, "SELECT flavor", time.Since(queryStart), err)

	if errors.Is(err, pgx.ErrNoRows) {
		logger.Warn().Str("flavor_ref", req.Server.FlavorRef).Msg("Flavor not found")
		middleware.LogOperationEnd(c, "create", "server", req.Server.Name, time.Since(start), err)
		common.SendError(c, common.NewNotFoundError("flavor"))
		return
	}
	if err != nil {
		logger.Error().Err(err).Msg("Failed to query flavor")
		middleware.LogOperationEnd(c, "create", "server", req.Server.Name, time.Since(start), err)
		log.Error().Err(err).Str("operation", "query_flavor").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to query flavor"))
		return
	}

	// Check quotas before creating instance
	if err := CheckQuota(c, "instances", 1); err != nil {
		if _, ok := err.(*QuotaExceededError); ok {
			common.SendError(c, common.NewQuotaExceededError("instances"))
			return
		}
		log.Error().Err(err).Str("operation", "check_quota_instances").Msg("quota check error")
		common.SendError(c, common.NewInternalServerError("failed to check quota"))
		return
	}

	// Check cores quota
	if err := CheckQuota(c, "cores", flavor.VCPUs); err != nil {
		if _, ok := err.(*QuotaExceededError); ok {
			common.SendError(c, common.NewQuotaExceededError("cores"))
			return
		}
		log.Error().Err(err).Str("operation", "check_quota_cores").Msg("quota check error")
		common.SendError(c, common.NewInternalServerError("failed to check quota"))
		return
	}

	// Check RAM quota
	if err := CheckQuota(c, "ram", flavor.RAMMB); err != nil {
		if _, ok := err.(*QuotaExceededError); ok {
			common.SendError(c, common.NewQuotaExceededError("ram"))
			return
		}
		log.Error().Err(err).Str("operation", "check_quota_ram").Msg("quota check error")
		common.SendError(c, common.NewInternalServerError("failed to check quota"))
		return
	}

	// FR-002a: validate the keypair exists for this user before creating anything,
	// so an unknown key_name fails fast with a Nova-compatible 400 (matching real
	// Nova v2.1 strict input validation) instead of silently booting a keyless VM.
	if req.Server.KeyName != "" {
		var kpExists bool
		if err := database.DB.QueryRow(c.Request.Context(),
			"SELECT EXISTS(SELECT 1 FROM keypairs WHERE user_id = $1 AND name = $2)",
			userID, req.Server.KeyName,
		).Scan(&kpExists); err != nil {
			log.Error().Err(err).Str("operation", "validate_keypair").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to validate key_name"))
			return
		}
		if !kpExists {
			common.SendError(c, common.NewBadRequestError("Invalid key_name provided."))
			return
		}
	}

	// Resolve + validate security groups supplied at create (FR-005). Unknown
	// non-default names fail fast with 400 (FR-005a); the project `default` group is
	// ensured on demand (FR-006a). Done before the INSERT so a bad request creates nothing.
	var sgIDs []string
	for _, sg := range req.Server.SecurityGroups {
		// Upstream Nova: an item without "name" ({}) is filtered out, but an empty
		// name fails schema validation (minLength 1) with 400. Mirror both.
		if sg.Name == nil {
			continue
		}
		name := *sg.Name
		if name == "" {
			common.SendError(c, common.NewBadRequestError("Invalid security group name provided."))
			return
		}
		// Resolve by id OR name. The field is spelled "name", but with Neutron as the
		// network backend upstream Nova matches each entry against both the name and
		// the UUID of the caller's security groups — and Horizon's launch wizard relies
		// on it, pushing securityGroup.id whenever Neutron is enabled
		// (launch-instance-model.service.js::setFinalSpecSecurityGroups). Matching on
		// name alone rejected every dashboard launch with
		// "Security group <uuid> not found." once the SG list started populating.
		// id::text keeps a non-UUID identifier from erroring the comparison, as in
		// neutron's GetSecurityGroup. An id match wins, so a group perversely named
		// after another's UUID cannot shadow it.
		var sgID string
		sgErr := database.DB.QueryRow(c.Request.Context(),
			`SELECT id FROM security_groups
			 WHERE (id::text = $1 OR name = $1) AND project_id = $2
			 ORDER BY (id::text = $1) DESC
			 LIMIT 1`,
			name, projectID,
		).Scan(&sgID)
		if errors.Is(sgErr, pgx.ErrNoRows) {
			if name == "default" {
				if sgID, sgErr = neutron.EnsureDefaultSecurityGroup(c.Request.Context(), projectID); sgErr != nil {
					log.Error().Err(sgErr).Str("operation", "ensure_default_sg").Msg("database error")
					common.SendError(c, common.NewInternalServerError("failed to ensure default security group"))
					return
				}
			} else {
				common.SendError(c, common.NewBadRequestError("Security group "+name+" not found."))
				return
			}
		} else if sgErr != nil {
			log.Error().Err(sgErr).Str("operation", "resolve_security_group").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to resolve security group"))
			return
		}
		sgIDs = append(sgIDs, sgID)
	}

	// No group resolved → the instance gets the project's default group, which
	// OpenStack guarantees exists (Neutron creates it lazily on first use). Ensure it
	// here so the port allocated at boot can attach it and the read-back reflects a
	// real association rather than a synthesized one (FR-006 / FR-006a).
	if len(sgIDs) == 0 {
		sgID, sgErr := neutron.EnsureDefaultSecurityGroup(c.Request.Context(), projectID)
		if sgErr != nil {
			log.Error().Err(sgErr).Str("operation", "ensure_default_sg").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to ensure default security group"))
			return
		}
		sgIDs = append(sgIDs, sgID)
	}

	// Generate instance ID
	instanceID := uuid.New().String()

	// Create instance record in database
	now := time.Now()

	// Handle NULL image_id (for volume-backed instances)
	var imageID any
	if req.Server.ImageRef != "" {
		imageID = req.Server.ImageRef
	} else {
		imageID = nil
	}

	// Persist key_name (empty → NULL) so server-detail can read it back (FR-002).
	var keyName any
	if req.Server.KeyName != "" {
		keyName = req.Server.KeyName
	}

	// Create the instance and its security-group associations atomically: if any
	// association fails, the instance INSERT rolls back too, so we never leave an
	// instance without the security groups it was created with (FR-005; Constitution
	// II, Synchronous Operations — "database updates happen within the same
	// transaction as the operation"). Associations cascade on delete
	// (server_security_groups FKs instances ON DELETE CASCADE).
	queryStart = time.Now()
	err = database.WithTx(c.Request.Context(), func(tx pgx.Tx) error {
		if _, txErr := tx.Exec(c.Request.Context(), `
			INSERT INTO instances (id, name, project_id, user_id, flavor_id, image_id, status, power_state, created_at, updated_at, key_name)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, instanceID, req.Server.Name, projectID, userID, flavor.ID, imageID, "BUILD", 0, now, now, keyName); txErr != nil {
			return fmt.Errorf("insert instance: %w", txErr)
		}
		for _, sgID := range sgIDs {
			if _, txErr := tx.Exec(c.Request.Context(), `
				INSERT INTO server_security_groups (server_id, security_group_id, created_at)
				VALUES ($1, $2, $3) ON CONFLICT DO NOTHING
			`, instanceID, sgID, now); txErr != nil {
				return fmt.Errorf("associate security group %s: %w", sgID, txErr)
			}
		}
		return nil
	})
	middleware.LogDatabaseQuery(c, "INSERT instance and security groups", time.Since(queryStart), err)

	if err != nil {
		logger.Error().Err(err).Str("instance_id", instanceID).Msg("Failed to create instance in database")
		middleware.LogOperationEnd(c, "create", "server", instanceID, time.Since(start), err)
		log.Error().Err(err).Str("operation", "insert_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create instance"))
		return
	}

	logger.Info().Str("instance_id", instanceID).Str("flavor", flavor.Name).Msg("Instance record created")

	// Persist cloud-init user_data so the metadata service serves it raw to
	// cloud-init (SPEC-009 WS-4 / T027).
	if decodedUserData != "" {
		if _, err := database.DB.Exec(c.Request.Context(),
			`INSERT INTO instance_userdata (instance_id, userdata) VALUES ($1, $2)
			 ON CONFLICT (instance_id) DO UPDATE SET userdata = EXCLUDED.userdata`,
			instanceID, decodedUserData); err != nil {
			logger.Warn().Err(err).Str("instance_id", instanceID).Msg("failed to persist user_data")
		}
	}

	// Log instance action
	requestID := uuid.New().String()
	if _, err := database.DB.Exec(c.Request.Context(), `
		INSERT INTO instance_actions (instance_id, action, request_id, user_id, project_id, start_time, message)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, instanceID, "create", requestID, userID, projectID, now, "Instance created"); err != nil {
		log.Debug().Err(err).Str("instance_id", instanceID).Msg("failed to log instance action (non-critical)")
	}

	// Create VM asynchronously (or synchronously if libvirt is available)
	if svc.vmManager != nil {
		logger.Info().Str("instance_id", instanceID).Msg("Starting VM creation via libvirt")
		svc.wg.Add(1)
		go func() {
			defer svc.wg.Done()
			// Recover from panics in goroutine
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Str("instance_id", instanceID).Msg("PANIC in VM creation goroutine")
					if _, err := database.DB.Exec(context.Background(),
						"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3",
						"ERROR", time.Now(), instanceID); err != nil {
						log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark instance ERROR after VM-creation panic")
					}
				}
			}()

			ctx, cancel := context.WithTimeout(svc.ctx, 30*time.Second)
			defer cancel()

			log.Debug().Str("instance_id", instanceID).Msg("VM creation goroutine started")
			libvirtStart := time.Now()

			// Allocate ports from Neutron for requested networks
			log.Debug().Str("instance_id", instanceID).Int("network_count", len(req.Server.Networks)).Msg("Allocating ports")
			var networks []hypervisor.NetworkConfig
			if svc.neutronSvc != nil && len(req.Server.Networks) > 0 {
				for _, network := range req.Server.Networks {
					log.Debug().Str("network_id", network.UUID).Msg("Allocating port for network")
					// sgIDs: the groups resolved and validated above, so the port is
					// filtered by the groups the server reports (cloud-storage#579).
					portInfoRaw, err := svc.neutronSvc.AllocatePortForInstance(ctx, network.UUID, projectID, instanceID, sgIDs)
					if err != nil {
						log.Error().Err(err).
							Str("instance_id", instanceID).
							Str("network_id", network.UUID).
							Msg("Failed to allocate port from Neutron")
						// Continue with empty networks rather than failing
						continue
					}

					// Type assert to neutron.PortInfo
					if portInfo, ok := portInfoRaw.(*neutron.PortInfo); ok {
						networks = append(networks, hypervisor.NetworkConfig{
							PortID:     portInfo.ID,
							MACAddress: portInfo.MAC,
							BridgeName: fmt.Sprintf("br-%s", portInfo.NetworkID[:8]),
						})
					}
				}
			}

			// Build a config-drive (cloud-init ISO) when a key and/or user_data is
			// supplied, so the VM is provisioned even without metadata-service
			// reachability (SPEC-009 WS-4 / T028).
			var cloudInit *hypervisor.CloudInitConfig
			var sshKey string
			if req.Server.KeyName != "" {
				if err := database.DB.QueryRow(ctx,
					"SELECT public_key FROM keypairs WHERE user_id = $1 AND name = $2",
					userID, req.Server.KeyName,
				).Scan(&sshKey); err != nil {
					logger.Warn().Err(err).
						Str("key_name", req.Server.KeyName).
						Msg("Failed to fetch SSH key for cloud-init")
				}
			}
			if sshKey != "" || decodedUserData != "" {
				cloudInit = hypervisor.NewCloudInitConfig(req.Server.Name, sshKey, decodedUserData)
				isoPath, err := hypervisor.GenerateCloudInitISO(instanceID, cloudInit)
				if err != nil {
					logger.Error().Err(err).
						Str("instance_id", instanceID).
						Msg("Failed to generate cloud-init ISO")
					// Continue without cloud-init rather than failing
				} else {
					logger.Info().
						Str("instance_id", instanceID).
						Str("iso_path", isoPath).
						Msg("Cloud-init ISO generated successfully")
				}
			}

			// Generate VM XML
			log.Debug().Str("instance_id", instanceID).Msg("Generating VM XML")
			// Root disk: boot-from-volume (the bootable volume's backing IS the
			// root disk) or, for an image boot, a per-instance qcow2 overlay over
			// the image's raw base so each VM is isolated (SPEC-009 WS-0/WS-5).
			var imagePath string
			if rootVol, rootDOT := bootVolumeID(&req); rootVol != "" {
				// Validate the boot volume before using it as the root disk: it must
				// exist, belong to this project, and be available + bootable
				// (SPEC-009 WS-5) — otherwise the VM would boot against an
				// unvalidated/foreign volume or a missing backing file.
				var volStatus, volProject, volAttached, rbdPool, rbdImage string
				var volBootable bool
				if err := database.DB.QueryRow(ctx,
					"SELECT status, COALESCE(bootable, false), project_id, COALESCE(attached_to_instance_id::text, ''), COALESCE(rbd_pool, ''), COALESCE(rbd_image, '') FROM volumes WHERE id = $1",
					rootVol).Scan(&volStatus, &volBootable, &volProject, &volAttached, &rbdPool, &rbdImage); err != nil {
					log.Error().Err(err).Str("instance_id", instanceID).Str("volume_id", rootVol).Msg("boot-from-volume: root volume not found")
					svc.markInstanceError(instanceID)
					return
				}
				if volProject != projectID || volStatus != "available" || !volBootable || volAttached != "" {
					log.Error().Str("instance_id", instanceID).Str("volume_id", rootVol).
						Str("status", volStatus).Bool("bootable", volBootable).Bool("owned", volProject == projectID).
						Msg("boot-from-volume: root volume not eligible (need available + bootable + owned + unattached)")
					svc.markInstanceError(instanceID)
					return
				}
				// Root disk = the bootable volume's backing, selected exactly like the
				// attach path (volumeDiskXML): if the node-local qcow2 exists it's a
				// local volume; otherwise it's RBD (rbd:pool/image, which GenerateVMXML
				// renders as a network disk). Do NOT key off rbd_pool/rbd_image — Lightstack
				// sets those defaults on every volume, local ones included, so they
				// can't distinguish the backend (SPEC-009 WS-5). Local is the validated
				// v1 mode; the RBD branch shares the (Ceph-untested) attach RBD path.
				if localPath := storage.LocalVolumePath(rootVol); fileExists(localPath) {
					imagePath = localPath
				} else {
					imagePath = "rbd:" + rbdPool + "/" + rbdImage
				}
				// boot-from-volume skips StageOverlay, so create the per-instance
				// directory the serial console.log lives in — otherwise libvirt
				// fails to start the domain (SPEC-009 WS-5).
				if err := hypervisor.EnsureInstanceDir(instanceID); err != nil {
					log.Error().Err(err).Str("instance_id", instanceID).Msg("boot-from-volume: failed to create instance dir")
					if _, dbErr := database.DB.Exec(svc.ctx,
						"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3",
						"ERROR", time.Now(), instanceID); dbErr != nil {
						log.Error().Err(dbErr).Str("instance_id", instanceID).Msg("failed to mark instance ERROR after instance-dir failure")
					}
					return
				}
				// Record the attachment first, then mark the volume in-use. Both must
				// succeed: a half-write means the VM boots but the DB is inconsistent
				// (no attachment row → FR-023 can't release/delete the volume on
				// server delete), so treat any failure as fatal.
				bvNow := time.Now()
				if _, e := database.DB.Exec(ctx,
					"INSERT INTO volume_attachments (id, volume_id, instance_id, device, delete_on_termination, attached_at) VALUES ($1, $2, $3, $4, $5, $6)",
					uuid.New().String(), rootVol, instanceID, "/dev/vda", rootDOT, bvNow); e != nil {
					log.Error().Err(e).Str("instance_id", instanceID).Str("volume_id", rootVol).Msg("boot-from-volume: failed to record root attachment")
					svc.markInstanceError(instanceID)
					return
				}
				if _, e := database.DB.Exec(ctx,
					"UPDATE volumes SET attached_to_instance_id = $1, status = 'in-use', updated_at = $2 WHERE id = $3",
					instanceID, bvNow, rootVol); e != nil {
					log.Error().Err(e).Str("instance_id", instanceID).Str("volume_id", rootVol).Msg("boot-from-volume: failed to mark volume in-use")
					svc.markInstanceError(instanceID)
					return
				}
			} else {
				parsedImageUUID, err := uuid.Parse(req.Server.ImageRef)
				if err != nil {
					log.Error().Err(err).Str("image_ref", req.Server.ImageRef).Str("instance_id", instanceID).Msg("Invalid imageRef — not a valid UUID")
					if _, dbErr := database.DB.Exec(svc.ctx,
						"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3",
						"ERROR", time.Now(), instanceID); dbErr != nil {
						log.Error().Err(dbErr).Str("instance_id", instanceID).Msg("failed to mark instance ERROR after invalid imageRef")
					}
					return
				}
				imageUUID := parsedImageUUID.String()
				imagePath = storage.LocalImagePath(imageUUID)
				if svc.libvirtMode == "real" {
					// Lazy fetch-on-boot: materialize the raw base from the image
					// backend (S3/RGW, or any node lacking a local copy) and verify
					// it against the image checksum before staging the overlay
					// (SPEC-014 FR-013/FR-017). No-op when the base already exists.
					if svc.imageStore != nil {
						var checksum string
						_ = database.DB.QueryRow(svc.ctx,
							"SELECT COALESCE(checksum, '') FROM images WHERE id = $1", imageUUID).Scan(&checksum)
						if _, ferr := svc.imageStore.EnsureLocalBase(svc.ctx, imageUUID, checksum); ferr != nil {
							log.Error().Err(ferr).Str("instance_id", instanceID).Str("image_id", imageUUID).Msg("Failed to materialize image base for boot")
							if _, dbErr := database.DB.Exec(svc.ctx,
								"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3",
								"ERROR", time.Now(), instanceID); dbErr != nil {
								log.Error().Err(dbErr).Str("instance_id", instanceID).Msg("failed to mark instance ERROR after image fetch failure")
							}
							return
						}
					}
					overlay, stageErr := hypervisor.StageOverlay(instanceID, storage.LocalImagePath(imageUUID))
					if stageErr != nil {
						log.Error().Err(stageErr).Str("instance_id", instanceID).Str("image_id", imageUUID).Msg("Failed to stage per-instance disk overlay")
						if _, dbErr := database.DB.Exec(svc.ctx,
							"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3",
							"ERROR", time.Now(), instanceID); dbErr != nil {
							log.Error().Err(dbErr).Str("instance_id", instanceID).Msg("failed to mark instance ERROR after overlay staging failure")
						}
						return
					}
					imagePath = overlay
				}
			}

			spec := hypervisor.VMSpec{
				UUID:      instanceID,
				Name:      fmt.Sprintf("instance-%s", instanceID[:8]),
				VCPUs:     flavor.VCPUs,
				MemoryMB:  flavor.RAMMB,
				DiskGB:    flavor.DiskGB,
				ImagePath: imagePath,
				Networks:  networks,
				CloudInit: cloudInit,
			}

			log.Debug().Str("image_path", spec.ImagePath).Int("network_count", len(spec.Networks)).Msg("VM spec prepared")
			xml := hypervisor.GenerateVMXML(spec)

			// Create VM
			log.Debug().Str("instance_id", instanceID).Msg("Calling CreateVM")
			libvirtUUID, err := svc.vmManager.CreateVM(ctx, xml)

			if err != nil {
				log.Error().Err(err).Str("instance_id", instanceID).Msg("Failed to create VM via libvirt")
				// Update instance status to ERROR
				dbCtx, dbCancel := context.WithTimeout(svc.ctx, 5*time.Second)
				defer dbCancel()
				if _, dbErr := database.DB.Exec(dbCtx,
					"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3",
					"ERROR", time.Now(), instanceID); dbErr != nil {
					log.Error().Err(dbErr).Str("instance_id", instanceID).Msg("failed to mark instance ERROR after libvirt create failure")
				}
				return
			}

			log.Debug().Str("libvirt_uuid", libvirtUUID).Msg("VM created successfully")

			logger.Info().
				Str("instance_id", instanceID).
				Str("libvirt_uuid", libvirtUUID).
				Dur("duration", time.Since(libvirtStart)).
				Msg("VM created successfully via libvirt")

			// Update instance with libvirt UUID
			dbCtx, dbCancel := context.WithTimeout(svc.ctx, 5*time.Second)
			defer dbCancel()
			if _, err := database.DB.Exec(dbCtx, `
				UPDATE instances
				SET status = $1, power_state = $2, libvirt_domain_id = $3, launched_at = $4, updated_at = $5
				WHERE id = $6
			`, "ACTIVE", 1, libvirtUUID, time.Now(), time.Now(), instanceID); err != nil {
				log.Error().Err(err).Str("instance_id", instanceID).Str("libvirt_uuid", libvirtUUID).Msg("failed to mark instance ACTIVE after libvirt create")
			}
		}()
	} else {
		logger.Debug().Msg("Stub mode: skipping libvirt VM creation")
		// In stub mode, immediately set instance to ACTIVE so Terraform polling succeeds
		if _, err := database.DB.Exec(c.Request.Context(), `
			UPDATE instances SET status = $1, power_state = $2, launched_at = $3, updated_at = $4 WHERE id = $5
		`, "ACTIVE", 1, time.Now(), time.Now(), instanceID); err != nil {
			log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark stub instance ACTIVE")
		}
	}

	middleware.LogOperationEnd(c, "create", "server", instanceID, time.Since(start), nil)

	// Return instance details
	c.JSON(http.StatusAccepted, gin.H{
		"server": gin.H{
			"id":        instanceID,
			"name":      req.Server.Name,
			"status":    "BUILD",
			"tenant_id": projectID,
			"user_id":   userID,
			"created":   now.Format(time.RFC3339),
			"updated":   now.Format(time.RFC3339),
			"flavor":    gin.H{"id": flavor.ID},
			"image":     gin.H{"id": req.Server.ImageRef},
			"metadata":  gin.H{},
			"adminPass": common.GeneratePassword(16),
		},
	})
}

// ListServers lists all servers (brief)
func (svc *Service) ListServers(c *gin.Context) {
	projectID := c.GetString("project_id")

	// Parse pagination parameters
	limit := 1000 // Default limit
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
	if markerParam := c.Query("marker"); markerParam != "" {
		// Marker-based pagination: get offset of marker UUID.
		// Scan failures (including no-rows) leave offset at zero, which
		// returns the full page — the same fallback we used before.
		var markerOffset int
		if err := database.DB.QueryRow(c.Request.Context(),
			`SELECT ROW_NUMBER() OVER (ORDER BY created_at DESC) - 1
			 FROM instances WHERE project_id = $1 AND id = $2`,
			projectID, markerParam,
		).Scan(&markerOffset); err != nil {
			log.Warn().Err(err).Str("marker", markerParam).Msg("failed to resolve pagination marker; ignoring")
		}
		if markerOffset > 0 {
			offset = markerOffset
		}
	}

	rows, err := database.DB.Query(c.Request.Context(),
		"SELECT id, name FROM instances WHERE project_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3",
		projectID, limit, offset,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_servers").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list servers"))
		return
	}
	defer rows.Close()

	var servers []gin.H
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Warn().Err(err).Msg("failed to scan server row")
			continue
		}
		servers = append(servers, gin.H{
			"id":   id,
			"name": name,
			"links": []gin.H{
				{"rel": "self", "href": fmt.Sprintf("%s/v2.1/servers/%s", common.BaseURL(c, 8774), id)},
			},
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list servers"))
		return
	}

	if servers == nil {
		servers = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"servers": servers})
}

// ListServersDetail lists all servers (detailed)
func (svc *Service) ListServersDetail(c *gin.Context) {
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
		err := database.DB.QueryRow(c.Request.Context(),
			"SELECT created_at FROM instances WHERE id = $1 AND project_id = $2",
			marker, projectID,
		).Scan(&markerCreatedAt)
		if err == nil {
			markerCondition = fmt.Sprintf(" AND i.created_at < $%d", argIdx)
			queryArgs = append(queryArgs, markerCreatedAt)
			argIdx++
		}
	}

	queryArgs = append(queryArgs, limit, offset)

	rows, err := database.DB.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT i.id, i.name, i.status, i.power_state, i.project_id, i.user_id,
		       i.flavor_id, i.image_id, i.created_at, i.updated_at, i.launched_at,
		       i.key_name,
		       f.vcpus, f.ram_mb, f.disk_gb, f.name as flavor_name
		FROM instances i
		LEFT JOIN flavors f ON i.flavor_id = f.id
		WHERE i.project_id = $1%s
		ORDER BY i.created_at DESC
		LIMIT $%d OFFSET $%d
	`, markerCondition, argIdx, argIdx+1), queryArgs...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_servers_detail").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list servers"))
		return
	}
	defer rows.Close()

	var servers []gin.H
	var ids []string
	for rows.Next() {
		var id, name, status, rowProjectID, userID, flavorID, flavorName string
		var powerState, vcpus, ramMB, diskGB int
		var createdAt, updatedAt time.Time
		var launchedAt sql.NullTime
		var imageID, keyName sql.NullString

		if err := rows.Scan(&id, &name, &status, &powerState, &rowProjectID, &userID,
			&flavorID, &imageID, &createdAt, &updatedAt, &launchedAt, &keyName,
			&vcpus, &ramMB, &diskGB, &flavorName); err != nil {
			log.Warn().Err(err).Msg("failed to scan server detail row")
			continue
		}

		// Get addresses for this instance
		addresses, addrErr := svc.getInstanceAddresses(c.Request.Context(), id, rowProjectID)
		if addrErr != nil {
			log.Error().Err(addrErr).Str("operation", "list_servers_detail_addresses").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to list servers"))
			return
		}

		entry := gin.H{
			"id":                     id,
			"name":                   name,
			"status":                 status,
			"tenant_id":              rowProjectID,
			"user_id":                userID,
			"created":                createdAt.Format(time.RFC3339),
			"updated":                updatedAt.Format(time.RFC3339),
			"addresses":              addresses,
			"OS-EXT-STS:power_state": powerState,
			"flavor": gin.H{
				"id":    flavorID,
				"vcpus": vcpus,
				"ram":   ramMB,
				"disk":  diskGB,
			},
		}
		// key_name: always present; null when the instance has no keypair (FR-003).
		if keyName.Valid {
			entry["key_name"] = keyName.String
		} else {
			entry["key_name"] = nil
		}
		if imageID.Valid {
			entry["image"] = gin.H{"id": imageID.String}
		} else {
			entry["image"] = ""
		}
		if launchedAt.Valid {
			entry["OS-SRV-USG:launched_at"] = launchedAt.Time.Format(time.RFC3339)
		} else {
			entry["OS-SRV-USG:launched_at"] = nil
		}
		servers = append(servers, entry)
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list servers"))
		return
	}

	// Attach security_groups to every entry in one batched query (no N+1, FR-007).
	sgMap, err := securityGroupNamesForServers(c.Request.Context(), ids)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_servers_detail_sgs").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list security groups"))
		return
	}
	for _, s := range servers {
		sid, _ := s["id"].(string)
		s["security_groups"] = formatSecurityGroups(sgMap[sid])
	}

	if servers == nil {
		servers = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"servers": servers})
}

// getInstanceAddresses retrieves network addresses for an instance from ports
func (svc *Service) getInstanceAddresses(ctx context.Context, instanceID, projectID string) (gin.H, error) {
	addresses := gin.H{}

	// Query ports for this instance
	rows, err := database.DB.Query(ctx, `
		SELECT p.network_id, p.fixed_ips, n.name
		FROM ports p
		JOIN networks n ON p.network_id = n.id
		WHERE p.device_id = $1 AND p.project_id = $2
	`, instanceID, projectID)

	if err != nil {
		return nil, fmt.Errorf("query ports for instance %s: %w", instanceID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var networkID, networkName string
		var fixedIPsJSON []byte

		if err := rows.Scan(&networkID, &fixedIPsJSON, &networkName); err != nil {
			log.Warn().Err(err).Msg("failed to scan network row")
			continue
		}

		// Parse fixed IPs
		var fixedIPs []map[string]any
		if err := json.Unmarshal(fixedIPsJSON, &fixedIPs); err != nil {
			continue
		}

		// Build address list for this network
		var addressList []gin.H
		for _, ipInfo := range fixedIPs {
			if ipAddr, ok := ipInfo["ip_address"].(string); ok {
				addressList = append(addressList, gin.H{
					"addr":            ipAddr,
					"version":         4,
					"OS-EXT-IPS:type": "fixed",
				})
			}
		}

		if len(addressList) > 0 {
			addresses[networkName] = addressList
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ports for instance %s: %w", instanceID, err)
	}

	return addresses, nil
}

// GetServer returns details for a single server
func (svc *Service) GetServer(c *gin.Context) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	var id, name, status string
	var userID, flavorID, imageID, keyName sql.NullString
	var powerState int
	var createdAt, updatedAt time.Time

	// Try to find by ID first, then by name
	// Use separate conditions to avoid type mismatch when id is UUID and param might be a name
	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT id, name, status, power_state, project_id,
		       user_id::text, flavor_id::text, image_id::text,
		       created_at, updated_at, key_name
		FROM instances
		WHERE project_id = $2 AND (
			(id::text = $1) OR (name = $1)
		)
	`, instanceID, projectID).Scan(&id, &name, &status, &powerState, &projectID, &userID, &flavorID, &imageID, &createdAt, &updatedAt, &keyName)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_server").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get server"))
		return
	}

	// Get addresses from ports
	addresses, addrErr := svc.getInstanceAddresses(c.Request.Context(), id, projectID)
	if addrErr != nil {
		log.Error().Err(addrErr).Str("operation", "get_server_addresses").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get server"))
		return
	}

	// Build response with nullable fields
	response := gin.H{
		"id":                     id,
		"name":                   name,
		"status":                 status,
		"tenant_id":              projectID,
		"created":                createdAt.Format(time.RFC3339),
		"updated":                updatedAt.Format(time.RFC3339),
		"addresses":              addresses,
		"OS-EXT-STS:power_state": powerState,
	}

	if userID.Valid {
		response["user_id"] = userID.String
	}
	if flavorID.Valid {
		response["flavor"] = gin.H{"id": flavorID.String}
	}
	if imageID.Valid {
		response["image"] = gin.H{"id": imageID.String}
	}

	// key_name: always present; null when the instance has no keypair (FR-001).
	if keyName.Valid {
		response["key_name"] = keyName.String
	} else {
		response["key_name"] = nil
	}

	// security_groups: current membership by name; [{"name":"default"}] when none (FR-004/FR-006).
	sgNames, err := securityGroupNamesForServer(c.Request.Context(), id)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_server_sgs").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get security groups"))
		return
	}
	response["security_groups"] = formatSecurityGroups(sgNames)

	c.JSON(http.StatusOK, gin.H{"server": response})
}

// DeleteServer deletes a server
func (svc *Service) DeleteServer(c *gin.Context) {
	logger := middleware.GetLogger(c)
	start := time.Now()

	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	middleware.LogOperationStart(c, "delete", "server", instanceID)

	// Get the instance UUID + libvirt domain ID (support lookup by ID or name)
	var instanceUUID string
	var libvirtDomainID sql.NullString
	var instanceImageID sql.NullString
	queryStart := time.Now()
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT id, libvirt_domain_id, image_id FROM instances WHERE project_id = $2 AND ((id::text = $1) OR (name = $1))",
		instanceID, projectID,
	).Scan(&instanceUUID, &libvirtDomainID, &instanceImageID)
	middleware.LogDatabaseQuery(c, "SELECT libvirt_domain_id", time.Since(queryStart), err)

	if errors.Is(err, pgx.ErrNoRows) {
		logger.Warn().Str("instance_id", instanceID).Msg("Instance not found")
		middleware.LogOperationEnd(c, "delete", "server", instanceID, time.Since(start), err)
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// Delete VM from libvirt (if available)
	if svc.vmManager != nil && libvirtDomainID.Valid && libvirtDomainID.String != "" {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()

		libvirtStart := time.Now()
		if err := svc.vmManager.DeleteVM(ctx, libvirtDomainID.String); err != nil {
			logger.Error().Err(err).Str("libvirt_domain_id", libvirtDomainID.String).Msg("Failed to delete VM from libvirt")
			middleware.LogExternalService(c, "libvirt", "delete_vm", time.Since(libvirtStart), err)
			middleware.LogOperationEnd(c, "delete", "server", instanceID, time.Since(start), err)
			log.Error().Err(err).Str("operation", "delete_vm_libvirt").Msg("libvirt error")
			common.SendError(c, common.NewInternalServerError("failed to delete server"))
			return
		}
		middleware.LogExternalService(c, "libvirt", "delete_vm", time.Since(libvirtStart), nil)
		logger.Info().Str("libvirt_domain_id", libvirtDomainID.String).Msg("VM deleted from libvirt")
	}

	// SPEC-009 FR-023: release (or, for delete-on-termination, delete) the volumes
	// attached to this instance, drop its auto-allocated neutron ports, and delete
	// the instance row — all in ONE transaction so a mid-sequence failure can't
	// leave split state (e.g. a volume deleted while the server still exists). The
	// attachment rows are read+resolved before the instance delete so the cascade
	// can't drop them first. Filesystem cleanup (volume backing, overlay dir) is
	// best-effort and runs only after the transaction commits.
	ctx := c.Request.Context()
	tx, txErr := database.DB.Begin(ctx)
	if txErr != nil {
		logger.Error().Err(txErr).Str("instance_id", instanceUUID).Msg("delete: begin tx failed")
		middleware.LogOperationEnd(c, "delete", "server", instanceID, time.Since(start), txErr)
		common.SendError(c, common.NewInternalServerError("failed to delete server"))
		return
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed

	rows, vaErr := tx.Query(ctx,
		"SELECT volume_id, delete_on_termination FROM volume_attachments WHERE instance_id = $1", instanceUUID)
	if vaErr != nil {
		logger.Error().Err(vaErr).Str("instance_id", instanceUUID).Msg("delete: query volume attachments failed")
		common.SendError(c, common.NewInternalServerError("failed to delete server"))
		return
	}
	type attachedVol struct {
		id  string
		dot bool
	}
	var attached []attachedVol
	for rows.Next() {
		var a attachedVol
		if scanErr := rows.Scan(&a.id, &a.dot); scanErr == nil {
			attached = append(attached, a)
		}
	}
	// Closed eagerly, not deferred: the transaction below acquires its own pooled
	// connection, and a deferred close would hold this cursor open across all of it. The
	// rows are already drained into `attached`, so there is nothing left to read.
	rows.Close()
	if rErr := rows.Err(); rErr != nil {
		logger.Error().Err(rErr).Str("instance_id", instanceUUID).Msg("delete: iterate volume attachments failed")
		common.SendError(c, common.NewInternalServerError("failed to delete server"))
		return
	}

	// Resolve each attachment in the tx; collect delete-on-termination volume ids
	// so their backing files are removed after commit (filesystem ops can't be
	// transactional).
	var dotVolumes []string
	for _, a := range attached {
		if a.dot {
			if _, e := tx.Exec(ctx, "DELETE FROM volumes WHERE id = $1", a.id); e != nil {
				logger.Error().Err(e).Str("volume_id", a.id).Msg("delete: drop delete-on-termination volume failed")
				common.SendError(c, common.NewInternalServerError("failed to delete server"))
				return
			}
			dotVolumes = append(dotVolumes, a.id)
		} else {
			if _, e := tx.Exec(ctx,
				"UPDATE volumes SET status = 'available', attached_to_instance_id = NULL, updated_at = $1 WHERE id = $2",
				time.Now(), a.id); e != nil {
				logger.Error().Err(e).Str("volume_id", a.id).Msg("delete: release volume failed")
				common.SendError(c, common.NewInternalServerError("failed to delete server"))
				return
			}
		}
	}
	if _, e := tx.Exec(ctx, "DELETE FROM volume_attachments WHERE instance_id = $1", instanceUUID); e != nil {
		logger.Error().Err(e).Str("instance_id", instanceUUID).Msg("delete: remove volume attachments failed")
		common.SendError(c, common.NewInternalServerError("failed to delete server"))
		return
	}

	// Delete the instance's auto-allocated neutron ports (Nova owns them). Without
	// this the port lingers after the server is gone, and deleting its network then
	// fails with "network has active ports" — which breaks `terraform destroy`
	// (SPEC-009 / SC-007) and leaks ports.
	if _, e := tx.Exec(ctx, "DELETE FROM ports WHERE device_id = $1", instanceUUID); e != nil {
		logger.Error().Err(e).Str("instance_id", instanceUUID).Msg("delete: remove instance ports failed")
		common.SendError(c, common.NewInternalServerError("failed to delete server"))
		return
	}

	// Delete the instance row (support lookup by ID or name).
	queryStart = time.Now()
	_, err = tx.Exec(ctx,
		"DELETE FROM instances WHERE project_id = $2 AND ((id::text = $1) OR (name = $1))",
		instanceID, projectID,
	)
	middleware.LogDatabaseQuery(c, "DELETE instance", time.Since(queryStart), err)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to delete instance from database")
		log.Error().Err(err).Str("operation", "delete_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete server"))
		return
	}

	if err := tx.Commit(ctx); err != nil {
		logger.Error().Err(err).Str("instance_id", instanceUUID).Msg("delete: commit failed")
		middleware.LogOperationEnd(c, "delete", "server", instanceID, time.Since(start), err)
		common.SendError(c, common.NewInternalServerError("failed to delete server"))
		return
	}

	// Post-commit, best-effort filesystem cleanup: delete-on-termination volume
	// backings and the per-instance overlay directory (a missing dir is fine — e.g.
	// stub mode never staged one). SPEC-009 WS-0/WS-5/FR-005/FR-023.
	for _, id := range dotVolumes {
		if e := storage.RemoveLocalVolume(id); e != nil {
			logger.Warn().Err(e).Str("volume_id", id).Msg("delete: remove local volume backing (post-commit)")
		}
	}
	if rmErr := hypervisor.RemoveInstanceDir(instanceUUID); rmErr != nil {
		logger.Warn().Err(rmErr).Str("instance_id", instanceUUID).Msg("failed to remove per-instance disk directory")
	}

	// Reclaim the node-local image base once nothing references it, so lazily
	// fetched bases don't accumulate unbounded (SPEC-014 FR-016).
	if instanceImageID.Valid {
		svc.reclaimImageBaseIfUnused(instanceImageID.String)
	}

	logger.Info().Str("instance_id", instanceID).Msg("Instance deleted successfully")
	middleware.LogOperationEnd(c, "delete", "server", instanceID, time.Since(start), nil)

	c.Status(http.StatusNoContent)
}

// ServerAction performs an action on a server
func (svc *Service) ServerAction(c *gin.Context) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warn().Err(err).Str("operation", "server_action_bind").Msg("Failed to bind JSON")
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	log.Debug().Str("instance_id", instanceID).Str("project_id", projectID).Interface("action", req).Msg("ServerAction")

	// Handle console actions first (don't require libvirt)
	if vncConsole, ok := req["os-getVNCConsole"]; ok {
		svc.GetVNCConsoleAction(c, vncConsole)
		return
	}
	if consoleOutput, ok := req["os-getConsoleOutput"]; ok {
		svc.GetConsoleOutputAction(c, consoleOutput)
		return
	}
	if serialConsole, ok := req["os-getSerialConsole"]; ok {
		svc.GetSerialConsoleAction(c, serialConsole)
		return
	}
	if spiceConsole, ok := req["os-getSPICEConsole"]; ok {
		svc.GetSPICEConsoleAction(c, spiceConsole)
		return
	}
	if rdpConsole, ok := req["os-getRDPConsole"]; ok {
		svc.GetRDPConsoleAction(c, rdpConsole)
		return
	}

	// Handle advanced actions that don't require libvirt in all cases
	if _, ok := req["suspend"]; ok {
		svc.SuspendInstance(c)
		return
	} else if _, ok := req["resume"]; ok {
		svc.ResumeInstance(c)
		return
	} else if _, ok := req["shelve"]; ok {
		svc.ShelveInstance(c)
		return
	} else if _, ok := req["shelveOffload"]; ok {
		svc.ShelveInstance(c) // Same as shelve for now
		return
	} else if _, ok := req["unshelve"]; ok {
		svc.UnshelveInstance(c)
		return
	} else if resizeData, ok := req["resize"]; ok {
		svc.ResizeInstanceAction(c, resizeData)
		return
	} else if _, ok := req["confirmResize"]; ok {
		svc.ConfirmResizeInstance(c)
		return
	} else if _, ok := req["revertResize"]; ok {
		svc.RevertResizeInstance(c)
		return
	} else if rebuildData, ok := req["rebuild"]; ok {
		svc.RebuildInstanceAction(c, rebuildData)
		return
	} else if rescueData, ok := req["rescue"]; ok {
		svc.RescueInstanceAction(c, rescueData)
		return
	} else if createImageData, ok := req["createImage"]; ok {
		svc.CreateImageAction(c, createImageData)
		return
	} else if _, ok := req["pause"]; ok {
		svc.PauseInstanceAction(c)
		return
	} else if _, ok := req["unpause"]; ok {
		svc.UnpauseInstanceAction(c)
		return
	} else if _, ok := req["lock"]; ok {
		svc.LockInstanceAction(c)
		return
	} else if _, ok := req["unlock"]; ok {
		svc.UnlockInstanceAction(c)
		return
	} else if _, ok := req["forceDelete"]; ok {
		svc.ForceDeleteInstanceAction(c)
		return
	} else if _, ok := req["evacuate"]; ok {
		svc.EvacuateInstance(c)
		return
	} else if _, ok := req["migrate"]; ok {
		svc.MigrateInstance(c)
		return
	} else if liveMigrate, ok := req["os-migrateLive"]; ok {
		// Pass the parsed live migrate data through context
		c.Set("action_data", liveMigrate)
		svc.LiveMigrateInstance(c)
		return
	} else if addSG, ok := req["addSecurityGroup"]; ok {
		c.Set("action_data", addSG)
		svc.AddSecurityGroup(c)
		return
	} else if removeSG, ok := req["removeSecurityGroup"]; ok {
		c.Set("action_data", removeSG)
		svc.RemoveSecurityGroup(c)
		return
	} else if changePass, ok := req["changePassword"]; ok {
		c.Set("action_data", changePass)
		svc.ChangePassword(c)
		return
	} else if _, ok := req["restore"]; ok {
		svc.RestoreInstance(c)
		return
	} else if createBackup, ok := req["createBackup"]; ok {
		c.Set("action_data", createBackup)
		svc.CreateBackupAction(c)
		return
	} else if resetState, ok := req["os-resetState"]; ok {
		c.Set("action_data", resetState)
		svc.ResetStateAction(c)
		return
	} else if _, ok := req["os-resetNetwork"]; ok {
		svc.ResetNetworkAction(c)
		return
	}

	// Get libvirt domain ID for remaining actions (support lookup by ID or name)
	var libvirtDomainID any
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT libvirt_domain_id FROM instances WHERE project_id = $2 AND ((id::text = $1) OR (name = $1))",
		instanceID, projectID,
	).Scan(&libvirtDomainID)

	if errors.Is(err, pgx.ErrNoRows) {
		log.Warn().Str("instance_id", instanceID).Msg("Instance not found in ServerAction")
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "server_action_query").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to query instance"))
		return
	}

	libvirtDomainStr := ""
	if libvirtDomainID != nil {
		libvirtDomainStr = fmt.Sprintf("%v", libvirtDomainID)
	}

	log.Debug().
		Bool("libvirt_valid", libvirtDomainStr != "").
		Str("libvirt_domain", libvirtDomainStr).
		Bool("vm_manager", svc.vmManager != nil).
		Str("mode", svc.libvirtMode).
		Msg("ServerAction state")

	// In stub mode, just update database status (don't call vmManager even if it exists)
	if svc.libvirtMode == "stub" || libvirtDomainStr == "" {
		// Handle actions in stub mode by updating database only
		if _, ok := req["reboot"]; ok {
			// Just mark as rebooting then active
			if _, err := database.DB.Exec(c.Request.Context(),
				"UPDATE instances SET status = $1, updated_at = $2 WHERE (id::text = $3 OR name = $3) AND project_id = $4",
				"REBOOT", time.Now(), instanceID, projectID); err != nil {
				log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark stub instance REBOOT")
			}
			svc.wg.Add(1)
			go func() {
				defer svc.wg.Done()
				select {
				case <-time.After(1 * time.Second):
				case <-svc.ctx.Done():
					return
				}
				ctx, cancel := context.WithTimeout(svc.ctx, 5*time.Second)
				defer cancel()
				if _, err := database.DB.Exec(ctx,
					"UPDATE instances SET status = $1, updated_at = $2 WHERE (id::text = $3 OR name = $3) AND project_id = $4",
					"ACTIVE", time.Now(), instanceID, projectID); err != nil {
					log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark stub instance ACTIVE after reboot")
				}
			}()
		} else if _, ok := req["os-stop"]; ok {
			if _, err := database.DB.Exec(c.Request.Context(),
				"UPDATE instances SET status = $1, power_state = $2, updated_at = $3 WHERE (id::text = $4 OR name = $4) AND project_id = $5",
				"SHUTOFF", 4, time.Now(), instanceID, projectID); err != nil {
				log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark stub instance SHUTOFF")
			}
		} else if _, ok := req["os-start"]; ok {
			if _, err := database.DB.Exec(c.Request.Context(),
				"UPDATE instances SET status = $1, power_state = $2, updated_at = $3 WHERE (id::text = $4 OR name = $4) AND project_id = $5",
				"ACTIVE", 1, time.Now(), instanceID, projectID); err != nil {
				log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark stub instance ACTIVE on start")
			}
		} else {
			common.SendError(c, common.NewBadRequestError("unknown action"))
			return
		}
		c.Status(http.StatusAccepted)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	// Handle different actions with real libvirt
	if _, ok := req["reboot"]; ok {
		if err := svc.vmManager.RebootVM(ctx, libvirtDomainStr); err != nil {
			log.Error().Err(err).Str("operation", "reboot_vm").Msg("libvirt error")
			common.SendError(c, common.NewInternalServerError("failed to reboot server"))
			return
		}
	} else if _, ok := req["os-stop"]; ok {
		if err := svc.vmManager.StopVM(ctx, libvirtDomainStr); err != nil {
			log.Error().Err(err).Str("operation", "stop_vm").Msg("libvirt error")
			common.SendError(c, common.NewInternalServerError("failed to stop server"))
			return
		}
	} else if _, ok := req["os-start"]; ok {
		if err := svc.vmManager.StartVM(ctx, libvirtDomainStr); err != nil {
			log.Error().Err(err).Str("operation", "start_vm").Msg("libvirt error")
			common.SendError(c, common.NewInternalServerError("failed to start server"))
			return
		}
	} else {
		common.SendError(c, common.NewBadRequestError("unknown action"))
		return
	}

	c.Status(http.StatusAccepted)
}

// ListFlavors lists all flavors (brief)
func (svc *Service) ListFlavors(c *gin.Context) {
	rows, err := database.DB.Query(c.Request.Context(),
		"SELECT id, name FROM flavors WHERE is_public = true ORDER BY ram_mb",
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_flavors").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list flavors"))
		return
	}
	defer rows.Close()

	var flavors []gin.H
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			log.Warn().Err(err).Msg("failed to scan flavor row")
			continue
		}
		flavors = append(flavors, gin.H{
			"id":   id,
			"name": name,
			"links": []gin.H{
				{"rel": "self", "href": fmt.Sprintf("%s/v2.1/flavors/%s", common.BaseURL(c, 8774), id)},
			},
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list flavors"))
		return
	}

	if flavors == nil {
		flavors = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"flavors": flavors})
}

// ListFlavorsDetail lists all flavors (detailed)
func (svc *Service) ListFlavorsDetail(c *gin.Context) {
	ctx := c.Request.Context()

	// Parse pagination parameters
	marker := c.Query("marker")
	limitStr := c.Query("limit")

	// Build query with pagination support
	query := "SELECT id, name, vcpus, ram_mb, disk_gb, is_public FROM flavors WHERE is_public = true"
	var args []any
	argIndex := 1

	// Add marker filter using created_at cursor (avoids broken UUID lexicographic ordering)
	if marker != "" {
		var markerTime any
		lookupErr := database.DB.QueryRow(ctx,
			"SELECT created_at FROM flavors WHERE id = $1", marker).Scan(&markerTime)
		if lookupErr == nil {
			query += fmt.Sprintf(" AND created_at > $%d", argIndex)
			args = append(args, markerTime)
			argIndex++
		}
		// If the marker flavor is not found, ignore the marker and return from the start.
	}

	query += " ORDER BY created_at, id"

	// Add limit
	if limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil && limit > 0 {
			query += fmt.Sprintf(" LIMIT $%d", argIndex)
			args = append(args, limit)
		}
	}

	rows, err := database.DB.Query(ctx, query, args...)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_flavors_detail").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list flavors"))
		return
	}
	defer rows.Close()

	var flavors []gin.H
	for rows.Next() {
		var id, name string
		var vcpus, ramMB, diskGB int
		var isPublic bool

		if err := rows.Scan(&id, &name, &vcpus, &ramMB, &diskGB, &isPublic); err != nil {
			log.Warn().Err(err).Msg("failed to scan flavor detail row")
			continue
		}

		flavors = append(flavors, gin.H{
			"id":                         id,
			"name":                       name,
			"vcpus":                      vcpus,
			"ram":                        ramMB,
			"disk":                       diskGB,
			"OS-FLV-EXT-DATA:ephemeral":  0,
			"OS-FLV-DISABLED:disabled":   false,
			"os-flavor-access:is_public": isPublic,
			"rxtx_factor":                1.0,
			"swap":                       "",
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list flavors"))
		return
	}

	if flavors == nil {
		flavors = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"flavors": flavors})
}

// GetFlavor returns a single flavor
func (svc *Service) GetFlavor(c *gin.Context) {
	flavorID := c.Param("id")
	ctx := c.Request.Context()

	// Validate ID is not empty
	if flavorID == "" {
		common.SendError(c, common.NewBadRequestError("Flavor ID cannot be empty"))
		return
	}

	// Try cache first
	if svc.cache != nil {
		cacheKey := "flavor:" + flavorID
		var cached gin.H
		if err := svc.cache.Get(ctx, cacheKey, &cached); err == nil {
			// Cache hit
			c.JSON(http.StatusOK, gin.H{"flavor": cached})
			return
		}
	}

	// Cache miss - query database
	var id, name string
	var vcpus, ramMB, diskGB int
	var isPublic bool

	// Support lookup by UUID or name
	err := database.DB.QueryRow(ctx,
		"SELECT id, name, vcpus, ram_mb, disk_gb, is_public FROM flavors WHERE id::text = $1 OR name = $1 LIMIT 1",
		flavorID,
	).Scan(&id, &name, &vcpus, &ramMB, &diskGB, &isPublic)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("flavor"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_flavor").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get flavor"))
		return
	}

	flavor := gin.H{
		"id":                         id,
		"name":                       name,
		"vcpus":                      vcpus,
		"ram":                        ramMB,
		"disk":                       diskGB,
		"OS-FLV-EXT-DATA:ephemeral":  0,
		"OS-FLV-DISABLED:disabled":   false,
		"os-flavor-access:is_public": isPublic,
		"rxtx_factor":                1.0,
		"swap":                       "",
	}

	// Store in cache (24 hour TTL - flavors rarely change)
	if svc.cache != nil {
		cacheKey := "flavor:" + id
		svc.cache.TrySet(ctx, cacheKey, flavor, 24*time.Hour)
	}

	c.JSON(http.StatusOK, gin.H{"flavor": flavor})
}

// ListImages - stub (proxy to Glance)
func (svc *Service) ListImages(c *gin.Context) {
	c.JSON(200, gin.H{"images": []gin.H{}})
}

// ListImagesDetail - stub (proxy to Glance)
func (svc *Service) ListImagesDetail(c *gin.Context) {
	c.JSON(200, gin.H{"images": []gin.H{}})
}

// ListHypervisors lists hypervisors (mock for Horizon)
func (svc *Service) ListHypervisors(c *gin.Context) {
	c.JSON(200, gin.H{"hypervisors": []gin.H{
		{
			"id":                  1,
			"hypervisor_hostname": "lightstack-node-1",
			"state":               "up",
			"status":              "enabled",
			"hypervisor_type":     "QEMU",
			"hypervisor_version":  2012000,
			"vcpus":               16,
			"memory_mb":           32768,
			"local_gb":            1000,
			"vcpus_used":          0,
			"memory_mb_used":      0,
			"local_gb_used":       0,
			"free_disk_gb":        900,
			"free_ram_mb":         28672,
			"running_vms":         0,
		},
	}})
}

// ListHypervisorsDetail lists hypervisors with details (mock for Horizon)
func (svc *Service) ListHypervisorsDetail(c *gin.Context) {
	cpuInfoJSON := `{"arch":"x86_64","model":"Skylake-Server-IBRS","vendor":"Intel","features":[],"topology":{"cores":8,"threads":2,"sockets":1}}`

	c.JSON(200, gin.H{"hypervisors": []gin.H{
		{
			"id":                  1,
			"hypervisor_hostname": "lightstack-node-1",
			"state":               "up",
			"status":              "enabled",
			"vcpus":               16,
			"memory_mb":           32768,
			"local_gb":            1000,
			"vcpus_used":          0,
			"memory_mb_used":      0,
			"local_gb_used":       0,
			"free_disk_gb":        900,
			"free_ram_mb":         28672,
			"hypervisor_type":     "QEMU",
			"hypervisor_version":  2012000,
			"running_vms":         0,
			"cpu_info":            cpuInfoJSON,
		},
	}})
}

// GetHypervisorStatistics returns aggregated hypervisor statistics (for Horizon)
func (svc *Service) GetHypervisorStatistics(c *gin.Context) {
	// Count running instances. Scan failures degrade to zero (the stats
	// response still ships) and are logged for diagnosis.
	var runningVMs int
	if err := database.DB.QueryRow(c.Request.Context(),
		"SELECT COUNT(*) FROM instances WHERE power_state = 1",
	).Scan(&runningVMs); err != nil {
		log.Warn().Err(err).Msg("failed to count running instances for hypervisor statistics")
	}

	// Return aggregated stats
	c.JSON(200, gin.H{
		"hypervisor_statistics": gin.H{
			"count":                1,
			"current_workload":     0,
			"disk_available_least": 800,
			"free_disk_gb":         900,
			"free_ram_mb":          28672,
			"local_gb":             1000,
			"local_gb_used":        100,
			"memory_mb":            32768,
			"memory_mb_used":       4096,
			"running_vms":          runningVMs,
			"vcpus":                16,
			"vcpus_used":           runningVMs * 2, // Assume 2 vCPUs per VM
		},
	})
}

// ListAvailabilityZones lists availability zones
func (svc *Service) ListAvailabilityZones(c *gin.Context) {
	// Query distinct availability zones from host_aggregates
	rows, err := database.DB.Query(c.Request.Context(),
		"SELECT DISTINCT availability_zone FROM host_aggregates WHERE availability_zone IS NOT NULL AND availability_zone != ''")
	if err != nil {
		log.Error().Err(err).Str("operation", "list_availability_zones").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to query availability zones"))
		return
	}
	defer rows.Close()

	var zones []gin.H
	for rows.Next() {
		var zoneName string
		if err := rows.Scan(&zoneName); err != nil {
			log.Warn().Err(err).Msg("failed to scan availability zone row")
			continue
		}
		zones = append(zones, gin.H{
			"zoneName":  zoneName,
			"zoneState": gin.H{"available": true},
			"hosts":     nil,
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list availability zones"))
		return
	}

	// Fallback to "nova" if no aggregates exist
	if len(zones) == 0 {
		zones = append(zones, gin.H{
			"zoneName":  "nova",
			"zoneState": gin.H{"available": true},
			"hosts":     nil,
		})
	}

	c.JSON(200, gin.H{"availabilityZoneInfo": zones})
}

// ListAvailabilityZonesDetail lists availability zones with host details
func (svc *Service) ListAvailabilityZonesDetail(c *gin.Context) {
	// Query availability zones with hosts from host_aggregates
	rows, err := database.DB.Query(c.Request.Context(),
		"SELECT availability_zone, hosts FROM host_aggregates WHERE availability_zone IS NOT NULL AND availability_zone != ''")
	if err != nil {
		log.Error().Err(err).Str("operation", "list_availability_zones_detail").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to query availability zones"))
		return
	}
	defer rows.Close()

	// Build zone map: zone_name -> []host
	zoneHosts := make(map[string][]string)
	for rows.Next() {
		var zoneName string
		var hosts []string
		if err := rows.Scan(&zoneName, &hosts); err != nil {
			log.Warn().Err(err).Msg("failed to scan availability zone host row")
			continue
		}
		zoneHosts[zoneName] = append(zoneHosts[zoneName], hosts...)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list availability zones"))
		return
	}

	var zones []gin.H
	if len(zoneHosts) == 0 {
		// Fallback to "nova" with default host
		zones = append(zones, gin.H{
			"zoneName":  "nova",
			"zoneState": gin.H{"available": true},
			"hosts": gin.H{
				"lightstack-compute-1": gin.H{
					"nova-compute": gin.H{
						"active":    true,
						"available": true,
					},
				},
			},
		})
	} else {
		// Build response for each zone
		for zoneName, hosts := range zoneHosts {
			zoneHostsMap := gin.H{}
			for _, host := range hosts {
				if host != "" {
					zoneHostsMap[host] = gin.H{
						"nova-compute": gin.H{
							"active":    true,
							"available": true,
						},
					}
				}
			}
			zones = append(zones, gin.H{
				"zoneName":  zoneName,
				"zoneState": gin.H{"available": true},
				"hosts":     zoneHostsMap,
			})
		}
	}

	c.JSON(200, gin.H{"availabilityZoneInfo": zones})
}

// GetLimits returns compute limits and quota information
func (svc *Service) GetLimits(c *gin.Context) {
	ctx := c.Request.Context()
	projectID := c.GetString("project_id")
	// An unscoped token carries no project, and its empty id would reach the uuid-typed
	// queries below — a 500. Answered as the quota routes answer it (QuotaTargetProject).
	if projectID == "" {
		common.SendError(c, common.NewNotFoundError("project"))
		return
	}
	// Resolve limits through the shared helper (package defaults overlaid with the
	// quotas table) so this view and the quota-set views always report the same value
	// for a resource, and an admin's quota override is reflected here too (FR-003).
	// This handler previously carried its own default map with instances/cores/ram at
	// 100/200/512000 — ten times the quota-set defaults it silently disagreed with.
	// Resolved before usage, as the quota-set views do.
	quotas, _, err := resolveQuotaLimits(ctx, projectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_limits").Msg("failed to resolve quota limits")
		common.SendError(c, common.NewInternalServerError("failed to get limits"))
		return
	}

	// Query current usage from database
	var instancesUsed, coresUsed, ramUsed int
	if err := database.DB.QueryRow(ctx,
		`SELECT
			COUNT(*),
			COALESCE(SUM(f.vcpus), 0),
			COALESCE(SUM(f.ram_mb), 0)
		FROM instances i
		LEFT JOIN flavors f ON i.flavor_id = f.id
		WHERE i.project_id = $1`,
		projectID,
	).Scan(&instancesUsed, &coresUsed, &ramUsed); err != nil {
		log.Error().Err(err).Str("project_id", projectID).Msg("failed to query limits usage")
		common.SendError(c, common.NewInternalServerError("failed to get limits"))
		return
	}

	// The remaining used-counters were hardcoded 0 while GetQuotaSet counted the same
	// resources for real, so the Overview widget and the quota-set view disagreed about
	// usage for floating IPs, security groups and server groups (FR-004; the same class
	// of drift FR-003 fixed for the limits themselves). Counted here rather than shared
	// with GetQuotaSet because that view reports a wider resource set. A count that fails
	// answers 500, never zero usage.
	var floatingIPsUsed, securityGroupsUsed, serverGroupsUsed int
	for _, q := range []struct {
		label string
		sql   string
		dst   *int
	}{
		{"floating_ips", `SELECT COUNT(*) FROM floating_ips WHERE project_id = $1`, &floatingIPsUsed},
		{"security_groups", `SELECT COUNT(*) FROM security_groups WHERE project_id = $1`, &securityGroupsUsed},
		{"server_groups", `SELECT COUNT(*) FROM server_groups WHERE project_id = $1`, &serverGroupsUsed},
	} {
		if err := database.DB.QueryRow(ctx, q.sql, projectID).Scan(q.dst); err != nil {
			log.Error().Err(err).Str("resource", q.label).Str("project_id", projectID).
				Msg("failed to query limits usage")
			common.SendError(c, common.NewInternalServerError("failed to get limits"))
			return
		}
	}

	// Return limits response
	c.JSON(200, gin.H{
		"limits": gin.H{
			"rate": []gin.H{}, // No rate limiting implemented
			"absolute": gin.H{
				// Quota limits from the quotas table (with defaults)
				"maxTotalInstances":     quotas["instances"],
				"maxTotalCores":         quotas["cores"],
				"maxTotalRAMSize":       quotas["ram"],
				"maxTotalKeypairs":      quotas["keypairs"],
				"maxServerMeta":         128,
				"maxPersonality":        5,
				"maxPersonalitySize":    10240,
				"maxServerGroups":       quotas["server_groups"],
				"maxServerGroupMembers": quotas["server_group_members"],
				"maxTotalFloatingIps":   quotas["floatingip"],
				"maxSecurityGroups":     quotas["security_groups"],
				"maxSecurityGroupRules": quotas["security_group_rules"],
				"maxImageMeta":          128,

				// Current usage
				"totalInstancesUsed":      instancesUsed,
				"totalCoresUsed":          coresUsed,
				"totalRAMUsed":            ramUsed,
				"totalFloatingIpsUsed":    floatingIPsUsed,
				"totalSecurityGroupsUsed": securityGroupsUsed,
				"totalServerGroupsUsed":   serverGroupsUsed,
			},
		},
	})
}

// ListServices returns list of compute services
func (svc *Service) ListServices(c *gin.Context) {
	// Format: "2006-01-02T15:04:05.000000" (without Z, microseconds)
	now := time.Now().Format("2006-01-02T15:04:05.000000")

	// Return list of compute services for Horizon System Info panel
	c.JSON(200, gin.H{
		"services": []gin.H{
			{
				"id":              1,
				"binary":          "nova-compute",
				"host":            "lightstack-compute-1",
				"zone":            "nova",
				"status":          "enabled",
				"state":           "up",
				"updated_at":      now,
				"disabled_reason": nil,
				"forced_down":     false,
			},
			{
				"id":              2,
				"binary":          "nova-scheduler",
				"host":            "lightstack-controller",
				"zone":            "internal",
				"status":          "enabled",
				"state":           "up",
				"updated_at":      now,
				"disabled_reason": nil,
				"forced_down":     false,
			},
			{
				"id":              3,
				"binary":          "nova-conductor",
				"host":            "lightstack-controller",
				"zone":            "internal",
				"status":          "enabled",
				"state":           "up",
				"updated_at":      now,
				"disabled_reason": nil,
				"forced_down":     false,
			},
		},
	})
}

// GetServerMetadata returns metadata for a server (GET /v2.1/servers/:id/metadata)
func (svc *Service) GetServerMetadata(c *gin.Context) {
	serverID := c.Param("id")

	// Check if server exists
	var exists bool
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM instances WHERE id = $1)",
		serverID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// Fetch metadata
	rows, err := database.DB.Query(c.Request.Context(),
		"SELECT meta_key, meta_value FROM instance_metadata WHERE instance_id = $1",
		serverID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_server_metadata").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to fetch metadata"))
		return
	}
	defer rows.Close()

	metadata := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			log.Error().Err(err).Str("operation", "scan_metadata").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to read metadata"))
			return
		}
		metadata[key] = value
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to get server metadata"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"metadata": metadata})
}

// UpdateServerMetadata updates/merges server metadata (POST /v2.1/servers/:id/metadata)
func (svc *Service) UpdateServerMetadata(c *gin.Context) {
	serverID := c.Param("id")

	var req struct {
		Metadata map[string]string `json:"metadata" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Check if server exists
	var exists bool
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM instances WHERE id = $1)",
		serverID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// Upsert each metadata key-value pair
	for key, value := range req.Metadata {
		_, err := database.DB.Exec(c.Request.Context(),
			`INSERT INTO instance_metadata (instance_id, meta_key, meta_value)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (instance_id, meta_key)
			 DO UPDATE SET meta_value = $3, created_at = CURRENT_TIMESTAMP`,
			serverID, key, value,
		)
		if err != nil {
			log.Error().Err(err).Str("operation", "upsert_metadata").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to update metadata"))
			return
		}
	}

	// Fetch and return all metadata
	rows, err := database.DB.Query(c.Request.Context(),
		"SELECT meta_key, meta_value FROM instance_metadata WHERE instance_id = $1",
		serverID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "fetch_metadata_after_update").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to fetch metadata"))
		return
	}
	defer rows.Close()

	metadata := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			log.Error().Err(err).Str("operation", "scan_metadata_after_update").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to read metadata"))
			return
		}
		metadata[key] = value
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to update server metadata"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"metadata": metadata})
}

// ResetServerMetadata replaces all server metadata (PUT /v2.1/servers/:id/metadata)
func (svc *Service) ResetServerMetadata(c *gin.Context) {
	serverID := c.Param("id")

	var req struct {
		Metadata map[string]string `json:"metadata" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Check if server exists
	var exists bool
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM instances WHERE id = $1)",
		serverID,
	).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// Delete all existing metadata then insert new metadata atomically
	err = database.WithTx(c.Request.Context(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(c.Request.Context(),
			"DELETE FROM instance_metadata WHERE instance_id = $1",
			serverID,
		); err != nil {
			return fmt.Errorf("clear_metadata: %w", err)
		}
		for key, value := range req.Metadata {
			if _, err := tx.Exec(c.Request.Context(),
				`INSERT INTO instance_metadata (instance_id, meta_key, meta_value)
				 VALUES ($1, $2, $3)`,
				serverID, key, value,
			); err != nil {
				return fmt.Errorf("insert_metadata: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		log.Error().Err(err).Str("operation", "reset_metadata").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to reset metadata"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"metadata": req.Metadata})
}

// RebuildInstanceAction handles the rebuild action
func (svc *Service) RebuildInstanceAction(c *gin.Context, rebuildData any) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	rebuildMap, ok := rebuildData.(map[string]any)
	if !ok {
		common.SendError(c, common.NewBadRequestError("invalid rebuild data"))
		return
	}

	imageRef, _ := rebuildMap["imageRef"].(string)
	name, _ := rebuildMap["name"].(string)

	if imageRef == "" {
		common.SendError(c, common.NewBadRequestError("imageRef is required"))
		return
	}

	// Update instance in database
	now := time.Now()
	_, err := database.DB.Exec(c.Request.Context(),
		"UPDATE instances SET image_id = $1, name = COALESCE(NULLIF($2, ''), name), status = $3, updated_at = $4 WHERE id = $5 AND project_id = $6",
		imageRef, name, "REBUILD", now, instanceID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "rebuild_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to rebuild instance"))
		return
	}

	// In stub mode, simulate rebuild completion
	if svc.libvirtMode == "stub" {
		svc.wg.Add(1)
		go func() {
			defer svc.wg.Done()
			select {
			case <-time.After(2 * time.Second):
			case <-svc.ctx.Done():
				return
			}
			ctx, cancel := context.WithTimeout(svc.ctx, 5*time.Second)
			defer cancel()
			if _, err := database.DB.Exec(ctx,
				"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3 AND project_id = $4",
				"ACTIVE", time.Now(), instanceID, projectID); err != nil {
				log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark instance ACTIVE after async transition")
			}
		}()
	}

	// Return updated server details
	var server gin.H
	var flavorID, userID, imageID, serverName, status string
	var createdAt, updatedAt time.Time
	err = database.DB.QueryRow(c.Request.Context(),
		"SELECT id, name, flavor_id, image_id, user_id, status, created_at, updated_at FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&instanceID, &serverName, &flavorID, &imageID, &userID, &status, &createdAt, &updatedAt)

	if err != nil {
		log.Error().Err(err).Str("operation", "get_instance_after_rebuild").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get instance after rebuild"))
		return
	}

	server = gin.H{
		"id":        instanceID,
		"name":      serverName,
		"status":    status,
		"tenant_id": projectID,
		"user_id":   userID,
		"created":   createdAt.Format(time.RFC3339),
		"updated":   updatedAt.Format(time.RFC3339),
		"image": gin.H{
			"id": imageID,
		},
		"flavor": gin.H{
			"id": flavorID,
		},
	}

	c.JSON(http.StatusOK, gin.H{"server": server})
}

// RescueInstanceAction handles the rescue action
func (svc *Service) RescueInstanceAction(c *gin.Context, rescueData any) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	// Update instance status to RESCUE
	now := time.Now()
	_, err := database.DB.Exec(c.Request.Context(),
		"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3 AND project_id = $4",
		"RESCUE", now, instanceID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "rescue_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to rescue instance"))
		return
	}

	// Return admin password (in real OpenStack this would be a generated rescue password)
	c.JSON(http.StatusOK, gin.H{
		"adminPass": common.GeneratePassword(16),
	})
}

// CreateImageAction handles the createImage action
func (svc *Service) CreateImageAction(c *gin.Context, createImageData any) {
	projectID := c.GetString("project_id")

	createImageMap, ok := createImageData.(map[string]any)
	if !ok {
		common.SendError(c, common.NewBadRequestError("invalid createImage data"))
		return
	}

	imageName, _ := createImageMap["name"].(string)
	if imageName == "" {
		common.SendError(c, common.NewBadRequestError("name is required"))
		return
	}

	// Get metadata if provided
	metadata := make(map[string]string)
	if metadataRaw, ok := createImageMap["metadata"].(map[string]any); ok {
		for k, v := range metadataRaw {
			if vStr, ok := v.(string); ok {
				metadata[k] = vStr
			}
		}
	}

	// Create image record in database
	imageID := uuid.New().String()
	now := time.Now()
	_, err := database.DB.Exec(c.Request.Context(), `
		INSERT INTO images (id, name, project_id, status, container_format, disk_format, size_bytes, visibility, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, imageID, imageName, projectID, "active", "bare", "qcow2", 0, "private", now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_image").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create image"))
		return
	}

	// Store metadata if provided. Failures are logged but don't abort:
	// the image already exists and is usable; missing metadata can be
	// re-set by the caller without recreating the image.
	for key, value := range metadata {
		if _, err := database.DB.Exec(c.Request.Context(), `
			INSERT INTO image_properties (image_id, name, value)
			VALUES ($1, $2, $3)
		`, imageID, key, value); err != nil {
			log.Warn().Err(err).Str("image_id", imageID).Str("key", key).Msg("failed to persist image property")
		}
	}

	// Return Location header with image URL
	imageLocation := fmt.Sprintf("%s/v2/images/%s", common.BaseURL(c, 9292), imageID)
	c.Header("Location", imageLocation)
	c.Status(http.StatusAccepted)
}

// PauseInstanceAction handles the pause action
func (svc *Service) PauseInstanceAction(c *gin.Context) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	// Update instance status to PAUSED
	_, err := database.DB.Exec(c.Request.Context(),
		"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3 AND project_id = $4",
		"PAUSED", time.Now(), instanceID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "pause_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to pause instance"))
		return
	}

	c.Status(http.StatusAccepted)
}

// UnpauseInstanceAction handles the unpause action
func (svc *Service) UnpauseInstanceAction(c *gin.Context) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	// Update instance status to ACTIVE
	_, err := database.DB.Exec(c.Request.Context(),
		"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3 AND project_id = $4",
		"ACTIVE", time.Now(), instanceID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "unpause_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to unpause instance"))
		return
	}

	c.Status(http.StatusAccepted)
}

// LockInstanceAction handles the lock action
func (svc *Service) LockInstanceAction(c *gin.Context) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	// Update instance locked status
	_, err := database.DB.Exec(c.Request.Context(),
		"UPDATE instances SET locked = true, updated_at = $1 WHERE id = $2 AND project_id = $3",
		time.Now(), instanceID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "lock_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to lock instance"))
		return
	}

	c.Status(http.StatusAccepted)
}

// UnlockInstanceAction handles the unlock action
func (svc *Service) UnlockInstanceAction(c *gin.Context) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	// Update instance locked status
	_, err := database.DB.Exec(c.Request.Context(),
		"UPDATE instances SET locked = false, updated_at = $1 WHERE id = $2 AND project_id = $3",
		time.Now(), instanceID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "unlock_instance").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to unlock instance"))
		return
	}

	c.Status(http.StatusAccepted)
}

// ForceDeleteInstanceAction handles the forceDelete action
func (svc *Service) ForceDeleteInstanceAction(c *gin.Context) {
	instanceID := c.Param("id")
	projectID := c.GetString("project_id")

	// In stub mode, just delete from database
	if svc.libvirtMode == "stub" || svc.vmManager == nil {
		_, err := database.DB.Exec(c.Request.Context(),
			"DELETE FROM instances WHERE id = $1 AND project_id = $2",
			instanceID, projectID,
		)
		if err != nil {
			log.Error().Err(err).Str("operation", "force_delete_instance").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to delete instance"))
			return
		}
		c.Status(http.StatusNoContent)
		return
	}

	// In real mode, destroy VM then delete from database
	var libvirtDomainID any
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT libvirt_domain_id FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&libvirtDomainID)

	if err == nil && libvirtDomainID != nil {
		domainStr := fmt.Sprintf("%v", libvirtDomainID)
		if domainStr != "" {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
			defer cancel()
			// Best-effort: a leftover libvirt domain is hygiene, not
			// correctness — the instance row is about to be deleted.
			if err := svc.vmManager.DeleteVM(ctx, domainStr); err != nil {
				log.Warn().Err(err).Str("domain", domainStr).Str("instance_id", instanceID).Msg("failed to delete libvirt domain")
			}
		}
	}

	_, err = database.DB.Exec(c.Request.Context(),
		"DELETE FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "force_delete_instance_db").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete instance"))
		return
	}

	c.Status(http.StatusNoContent)
}
