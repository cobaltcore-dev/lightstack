package nova

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/console"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/middleware"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// Service handles Nova API endpoints
type Service struct {
	pool        *pgxpool.Pool
	libvirtURI  string
	libvirtMode string
	vmManager   *hypervisor.VMManager
	cache       *cache.Cache
	imageStore  *storage.ImageStore // lazy fetch-on-boot of S3/RGW-backed image bases (SPEC-014)
	neutronSvc  NeutronService      // For port allocation
	cinderSvc   CinderService       // For boot-from-new-volume (SPEC-020 FR-025)
	// Console token store and the browser-reachable proxy origin (SPEC-021).
	// consoleStore is nil only in tests that never touch the console.
	consoleStore        *console.Store
	consoleProxyBaseURL string
	consoleEnabled      bool
	// shutdownTimeout is how long a guest gets to shut down cleanly before a stop
	// forces it off. Zero means the package default; see SetShutdownTimeout.
	shutdownTimeout time.Duration
	wg              sync.WaitGroup
	ctx             context.Context
	cancel          context.CancelFunc
}

const (
	novaMinVersion     = "2.1"
	novaCurrentVersion = "2.90"
)

// NeutronService defines the interface for Neutron operations Nova needs.
type NeutronService interface {
	AllocatePortForInstance(ctx context.Context, networkID, projectID, instanceID string, securityGroupIDs []string, admin bool) (any, error)
	// ReleaseInstancePorts is AllocatePortForInstance's rollback: it deletes the
	// ports Neutron allocated for a boot that failed after allocating them
	// (review 2026-10-13 N-6) — the port rows, their host plumbing and their
	// DHCP reservations. Scoped to nova-owned ports; a tenant-created port the
	// server was told to use is the tenant's, not the boot's.
	ReleaseInstancePorts(ctx context.Context, instanceID string) error
	// NetworkUsableBy reports whether a project may attach a guest to a network,
	// returning a non-nil error naming the reason if not. admin is whether the caller
	// holds the administrator role, which decides an unshared external network; both
	// methods take it, so the pre-flight and the allocation judge the same caller.
	//
	// Compute asks rather than deciding: the attach rule belongs to the network
	// service, and duplicating the predicate here is how the two drift. Used to refuse a
	// bad network BEFORE anything is created (FR-017), which is the half of a network
	// failure that IS knowable up front.
	NetworkUsableBy(ctx context.Context, networkID, projectID string, admin bool) error
}

// NewService creates a new Nova service
func NewService(pool *pgxpool.Pool, libvirtURI, libvirtMode string, cacheInstance *cache.Cache) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		pool:        pool,
		libvirtURI:  libvirtURI,
		libvirtMode: libvirtMode,
		cache:       cacheInstance,
		neutronSvc:  nil, // Set via SetNeutronService after initialization
		// Consoles are on unless the deployment says no proxy runs (SetConsoleEnabled).
		consoleEnabled: true,
		ctx:            ctx,
		cancel:         cancel,
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

// SetConsoleStore wires the console-token store (SPEC-021). Without it the
// console endpoints refuse rather than improvise, which is the point: a token
// we cannot persist is a token we cannot revoke.
func (svc *Service) SetConsoleStore(s *console.Store) {
	svc.consoleStore = s
}

// SetConsoleProxyBaseURL sets the browser-reachable ORIGIN of the console proxy.
// Empty means "derive it from the request", never "use localhost" (FR-021).
func (svc *Service) SetConsoleProxyBaseURL(u string) {
	svc.consoleProxyBaseURL = u
}

// SetConsoleEnabled records whether a console proxy is deployed. Without one,
// console requests are refused rather than answered with a URL that leads nowhere.
func (svc *Service) SetConsoleEnabled(enabled bool) {
	svc.consoleEnabled = enabled
}

// SetShutdownTimeout sets how long a guest OS is given to shut down cleanly before a
// stop powers it off the hard way (SPEC-025 FR-022, common.DefaultShutdownTimeout).
//
// A setter rather than a NewService parameter, matching the image-store and Cinder
// wiring: every test constructing a Service would otherwise have to name a value it
// does not care about.
func (svc *Service) SetShutdownTimeout(d time.Duration) {
	svc.shutdownTimeout = d
}

// guestShutdownGrace is the EFFECTIVE grace period: defaulted when unset, and clamped to
// what a synchronous stop can still answer within.
//
// Clamped here as well as in LoadConfig because this is the value the handler actually
// uses, and not every Service is built by LoadConfig — every test sets it directly. A
// grace period longer than common.MaxShutdownTimeout does not merely run long: net/http
// closes the connection at the write deadline with no response written, so the stop
// succeeds and reports nothing (SPEC-025 FR-022b).
func (svc *Service) guestShutdownGrace() time.Duration {
	if svc.shutdownTimeout <= 0 {
		return common.DefaultShutdownTimeout
	}
	return min(svc.shutdownTimeout, common.MaxShutdownTimeout)
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
	if err := svc.pool.QueryRow(svc.ctx, `
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

// CinderService is the narrow block-storage surface compute needs.
//
// One method, mirroring NeutronService: compute names an image and a size and gets a
// volume id back, with no knowledge of backing stores or volume types. The alternative —
// compute writing its own volume rows — would duplicate the decision about what makes a
// volume bootable, which is how the create-from-image and boot-from-volume paths would
// drift apart.
type CinderService interface {
	CreateBootVolumeFromImage(ctx context.Context, name, projectID, userID, imageID string, sizeGB int) (string, error)
}

// SetCinderService wires the block-storage service in. Nil in stub-only tests and in any
// deployment without block storage; the boot-from-new-volume path checks before using it.
func (svc *Service) SetCinderService(cinder CinderService) {
	svc.cinderSvc = cinder
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

// PrepareConsoleSocketDir makes the console socket directory QEMU's (SPEC-021
// FR-017a). Until it has, the hypervisor refuses every guest start, because each domain
// names its console socket in that directory and libvirt does not prepare it.
func (svc *Service) PrepareConsoleSocketDir(ctx context.Context, dir string) error {
	if svc.vmManager == nil {
		return errNoHypervisor
	}
	return svc.vmManager.PrepareConsoleSocketDir(ctx, dir)
}

// startRefusal is the 500 for a guest start the hypervisor refused. A console socket
// directory that is not ready is named, since the operator has to fix it on the host
// (FR-017a); any other failure keeps the generic message.
func startRefusal(err error, generic string) *common.OpenStackError {
	if errors.Is(err, hypervisor.ErrConsoleSocketDirNotReady) {
		return common.NewInternalServerError(err.Error())
	}
	return common.NewInternalServerError(generic)
}

// RegisterRoutes registers Nova routes
func (svc *Service) RegisterRoutes(r *gin.RouterGroup) {
	// Version discovery
	r.GET("/", svc.ListVersions)
	r.GET("/v2.1", svc.GetVersion)

	// Microversion negotiation applies to the whole v2.1 surface (FR-038, FR-041): the
	// applied version is reported on every response, and an out-of-range one is refused
	// before any handler runs. Attached to the group rather than to the version documents,
	// which are what previously carried the header — and always as the maximum.
	v21 := r.Group("/v2.1", MicroversionMiddleware())
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
		// Console-auth-token compatibility view (microversion 2.31+, SPEC-021
		// FR-015). Nothing in Lightstack's console path depends on it — the proxy
		// resolves tokens from its projection directory — but `openstack` and
		// third-party tooling expect the endpoint to exist.
		v21.GET("/os-console-auth-tokens/:token", svc.ShowConsoleAuthToken)

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

// GetVersion returns version details for GET /v2.1.
//
// The links array is not decoration: python-openstackclient reads this document
// during discovery and indexes into version.links unconditionally, so omitting it
// aborted the client with "list index out of range" — `openstack flavor list` failed
// for anyone who had not pinned OS_COMPUTE_API_VERSION (cloud-storage#547 B4,
// SPEC-025 FR-001/FR-008). The version LIST at "/" always had links; this document,
// which is the one discovery actually fetches, did not.
func (svc *Service) GetVersion(c *gin.Context) {
	c.Header("OpenStack-API-Version", "compute "+novaCurrentVersion)
	c.Header("OpenStack-API-Minimum-Version", "compute "+novaMinVersion)
	c.JSON(200, gin.H{
		"version": gin.H{
			"id":          "v2.1",
			"status":      "CURRENT",
			"version":     novaCurrentVersion,
			"min_version": novaMinVersion,
			"links": []gin.H{
				{"rel": "self", "href": fmt.Sprintf("%s/v2.1", common.BaseURL(c, 8774))},
			},
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
		UserData             string               `json:"user_data"`
		BlockDeviceMappingV2 []BlockDeviceMapping `json:"block_device_mapping_v2"`
	} `json:"server"`
}

// BlockDeviceMapping is one block_device_mapping_v2 entry.
//
// A named type rather than an anonymous struct because the boot-from-new-volume path
// appends to the list after creating the volume, and an anonymous struct would have to be
// repeated field for field at that call site.
type BlockDeviceMapping struct {
	BootIndex       any    `json:"boot_index"`
	UUID            string `json:"uuid"`
	SourceType      string `json:"source_type"`
	DestinationType string `json:"destination_type"`
	// VolumeSize is typed `any` because upstream accepts a number or a string of digits;
	// bdmVolumeSize reads it.
	VolumeSize          any  `json:"volume_size"`
	DeleteOnTermination bool `json:"delete_on_termination"`
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
// An explicit imageRef and a real boot volume both take precedence — see the caller in
// CreateServer. When the entry also asks for destination_type=volume, the image is
// materialized into a volume first (rootImageToVolumeBDM); this function only reports
// which image the root disk comes from.
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

// rootImageToVolumeBDM reports the root block-device mapping that asks for a NEW volume
// created from an image, if the request has one.
//
// This is the dashboard wizard's default: "Create New Volume = Yes" sends the image as a
// source_type=image / destination_type=volume mapping rather than as imageRef. Upstream
// creates a volume from the image and boots from it, honouring volume_size and
// delete_on_termination (SPEC-020 FR-025).
func rootImageToVolumeBDM(req *CreateServerRequest) (imageID string, rawSize any, deleteOnTermination bool, found bool) {
	for _, bdm := range req.Server.BlockDeviceMappingV2 {
		if bdm.SourceType != "image" || bdm.UUID == "" || bdm.DestinationType != "volume" {
			continue
		}
		if !isRootBootIndex(bdm.BootIndex) {
			continue
		}
		return bdm.UUID, bdm.VolumeSize, bdm.DeleteOnTermination, true
	}
	return "", nil, false, false
}

// bdmVolumeSize reads a volume_size as upstream's request schema accepts it: an integer
// from 1 to 2^31-1, as a number or as a string of digits. ok is false for anything else.
//
// gin sets EnableDecoderUseNumber, so a JSON number arrives as json.Number rather than
// float64. An absent size is 0 and ok, which is what makes the default in
// resolveBootVolumeSize apply. Upstream's schema lets "0" through and then fails the
// build, since no volume can have size 0; it is refused here at once.
func bdmVolumeSize(raw any) (size int, ok bool) {
	var n int64
	switch v := raw.(type) {
	case nil:
		return 0, true
	case int:
		n = int64(v)
	case float64:
		if v != math.Trunc(v) || v < 1 || v > math.MaxInt32 {
			return 0, false
		}
		n = int64(v)
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0, false
		}
		n = parsed
	case string:
		if v == "" || strings.Trim(v, "0123456789") != "" {
			return 0, false
		}
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, false
		}
		n = parsed
	default:
		return 0, false
	}
	if n < 1 || n > math.MaxInt32 {
		return 0, false
	}
	return int(n), true
}

// errBootImageNotFound: the image a new boot volume is to be made from does not exist.
var errBootImageNotFound = errors.New("boot image not found")

// bootVolumeSizeError: no boot volume of a workable size can be made for the request.
type bootVolumeSizeError struct{ msg string }

func (e *bootVolumeSizeError) Error() string { return e.msg }

// resolveBootVolumeSize decides how large a new boot volume must be, or reports why the
// request cannot be satisfied.
//
// The floor is max(image.min_disk_gb, ceil(image.size_bytes)) — both, not either:
//
//   - min_disk_gb alone is not enough. It defaults to 0, so an image that states no
//     minimum would accept a 1 GiB volume however large its contents are, and the result
//     would be an unbootable volume rather than a refusal.
//   - The actual size alone is not enough either: an image may need more room than its
//     compressed contents occupy, which is what min_disk exists to express.
//
// When no size is requested the flavor's disk governs, floored the same way: the volume IS
// the server's root disk, so the flavor is the size the user already chose by picking it.
//
// An image that does not exist is errBootImageNotFound and a size that cannot work is a
// *bootVolumeSizeError; both are the caller's to fix. Any other error is the database's.
func resolveBootVolumeSize(ctx context.Context, pool *pgxpool.Pool, imageID string, requested, flavorDiskGB int) (int, error) {
	parsed, err := uuid.Parse(imageID)
	if err != nil {
		return 0, errBootImageNotFound
	}
	var minDiskGB int
	var sizeBytes sql.NullInt64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(min_disk_gb, 0), size_bytes FROM images WHERE id = $1`, parsed.String(),
	).Scan(&minDiskGB, &sizeBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errBootImageNotFound
		}
		return 0, fmt.Errorf("reading image %s: %w", imageID, err)
	}

	const bytesPerGiB = 1 << 30
	contentGB := 0
	if sizeBytes.Valid && sizeBytes.Int64 > 0 {
		// Round up: a 1.2 GiB image does not fit in 1 GiB.
		contentGB = int((sizeBytes.Int64 + bytesPerGiB - 1) / bytesPerGiB)
	}

	floor := minDiskGB
	if contentGB > floor {
		floor = contentGB
	}

	size := requested
	if size <= 0 {
		size = flavorDiskGB
		if size < floor {
			size = floor
		}
		if size <= 0 {
			// Neither the flavor nor the image gives a usable size. Refuse rather than
			// create a zero-size volume.
			return 0, &bootVolumeSizeError{fmt.Sprintf("cannot determine a boot volume size for image %s", imageID)}
		}
		return size, nil
	}

	if size < floor {
		return 0, &bootVolumeSizeError{fmt.Sprintf("volume_size %d GiB is too small for image %s, which needs at least %d GiB",
			size, imageID, floor)}
	}
	return size, nil
}

// removeBootVolume deletes a boot volume made for a server create that then failed, the
// way DeleteServer removes a delete-on-termination volume: the row, then the local backing.
// Best-effort; svc.ctx so it runs even if the request context was cancelled.
func (svc *Service) removeBootVolume(volumeID string) {
	if _, err := svc.pool.Exec(svc.ctx, "DELETE FROM volumes WHERE id = $1", volumeID); err != nil {
		log.Error().Err(err).Str("volume_id", volumeID).Msg("failed create: could not remove its boot volume")
		return
	}
	if err := storage.RemoveLocalVolume(volumeID); err != nil {
		log.Warn().Err(err).Str("volume_id", volumeID).Msg("failed create: could not remove its boot volume's backing")
	}
}

// markInstanceError flags an instance ERROR (best-effort), used from the
// VM-creation goroutine's failure paths. Uses svc.ctx so it still runs even if
// the request context was cancelled.
func (svc *Service) markInstanceError(instanceID string, reason string) {
	now := time.Now()
	if _, err := svc.pool.Exec(svc.ctx,
		`UPDATE instances
		    SET status = 'ERROR', updated_at = $1,
		        fault_code = $2, fault_message = $3, fault_created = $1
		  WHERE id = $4`,
		now, http.StatusInternalServerError, reason, instanceID); err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark instance ERROR")
	}
}

// tenantMessager is an error the network service has marked as fit for the tenant to read: a
// condition of their network they can act on. Nothing else it returns is shown to them.
type tenantMessager interface{ TenantMessage() string }

// allocationFault is the reason a failed port allocation records in the server's fault, which the
// tenant reads. Only a failure the network service marked for them is shown as worded; any other — a
// database or host error — is in the log line the caller writes and is reported without its text, as
// upstream records only a non-Nova exception's class name (nova/compute/utils.py exception_to_dict).
func allocationFault(networkID string, err error) string {
	var visible tenantMessager
	if errors.As(err, &visible) {
		return fmt.Sprintf("could not attach the server to network %s: %s", networkID, visible.TenantMessage())
	}
	return fmt.Sprintf("could not attach the server to network %s", networkID)
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

	// Does the root disk ask for a NEW volume created from an image? That is the dashboard
	// wizard's default. Detected here, acted on once the flavor is known: the flavor's disk
	// is what governs a size the caller left unspecified.
	bootVolImage, bootVolRawSize, bootVolDOT, wantsNewBootVolume := rootImageToVolumeBDM(&req)
	bootVolSize, sizeOK := bdmVolumeSize(bootVolRawSize)
	if wantsNewBootVolume && !sizeOK {
		common.SendError(c, common.NewBadRequestError(
			fmt.Sprintf("Invalid input for field/attribute volume_size. Value: %v.", bootVolRawSize)))
		return
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
	err := svc.pool.QueryRow(c.Request.Context(),
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

	// Every image the server is made from must be one the caller can read, and active
	// (SPEC-020 FR-026), checked before anything is reserved or created. Upstream asks Glance
	// with the caller's token, so another project's private image and an id that names no
	// image get the same answer. ImageRef holds the root image whichever way it arrived, an
	// adopted mapping included; a new boot volume's image is checked too.
	isAdmin := common.HasRole(c, common.RoleAdmin)
	for _, img := range []string{req.Server.ImageRef, bootVolImage} {
		if img == "" {
			continue
		}
		status, found, verr := common.VisibleImage(c.Request.Context(), svc.pool, img, projectID, isAdmin)
		if verr != nil {
			log.Error().Err(verr).Str("operation", "check_image").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to check the image"))
			return
		}
		if !found {
			logger.Warn().Str("image_ref", img).Msg("refusing server create: image not found or not visible")
			middleware.LogOperationEnd(c, "create", "server", req.Server.Name, time.Since(start), nil)
			common.SendError(c, common.NewBadRequestError("Can not find requested image"))
			return
		}
		if status != "active" {
			middleware.LogOperationEnd(c, "create", "server", req.Server.Name, time.Since(start), nil)
			common.SendError(c, common.NewBadRequestError(fmt.Sprintf("Image %s is not active.", img)))
			return
		}
	}

	// Check quotas before creating instance
	if err := CheckQuota(c, svc.pool, "instances", 1); err != nil {
		if _, ok := err.(*QuotaExceededError); ok {
			common.SendError(c, common.NewQuotaExceededError("instances"))
			return
		}
		log.Error().Err(err).Str("operation", "check_quota_instances").Msg("quota check error")
		common.SendError(c, common.NewInternalServerError("failed to check quota"))
		return
	}

	// Check cores quota
	if err := CheckQuota(c, svc.pool, "cores", flavor.VCPUs); err != nil {
		if _, ok := err.(*QuotaExceededError); ok {
			common.SendError(c, common.NewQuotaExceededError("cores"))
			return
		}
		log.Error().Err(err).Str("operation", "check_quota_cores").Msg("quota check error")
		common.SendError(c, common.NewInternalServerError("failed to check quota"))
		return
	}

	// Check RAM quota
	if err := CheckQuota(c, svc.pool, "ram", flavor.RAMMB); err != nil {
		if _, ok := err.(*QuotaExceededError); ok {
			common.SendError(c, common.NewQuotaExceededError("ram"))
			return
		}
		log.Error().Err(err).Str("operation", "check_quota_ram").Msg("quota check error")
		common.SendError(c, common.NewInternalServerError("failed to check quota"))
		return
	}

	// Refuse a network the caller cannot use BEFORE the instance row exists (FR-017,
	// FR-016a). This is the half of a network failure that is knowable up front — a
	// wrong or unreachable network id — and answering it synchronously is what stops
	// the ordinary typo from becoming a background failure the user has to poll for.
	//
	// The other half, an allocation that fails once under way, cannot be answered here
	// because the create is acknowledged before allocation begins; it lands the server
	// in ERROR with a readable reason instead (FR-015, FR-016).
	if svc.neutronSvc != nil {
		for _, network := range req.Server.Networks {
			if network.UUID == "" {
				continue
			}
			if err := svc.neutronSvc.NetworkUsableBy(c.Request.Context(), network.UUID, projectID, isAdmin); err != nil {
				middleware.LogOperationEnd(c, "create", "server", req.Server.Name, time.Since(start), err)
				// Upstream answers an unshared external network with its own 403
				// (ExternalNetworkAttachForbidden); every other refusal is a 400. Only an
				// error the network service marked for the tenant is a refusal: anything
				// else is a check that failed, which is a 500 and keeps its text in the log.
				var forbidden *neutron.ExternalNetworkAttachForbiddenError
				var visible tenantMessager
				switch {
				case errors.As(err, &forbidden):
					logger.Warn().Err(err).Str("network_id", network.UUID).
						Msg("refusing server create: requested network is not usable by this project")
					common.SendError(c, common.NewForbiddenError(forbidden.Error()))
				case errors.As(err, &visible):
					logger.Warn().Err(err).Str("network_id", network.UUID).
						Msg("refusing server create: requested network is not usable by this project")
					common.SendError(c, common.NewBadRequestError(
						fmt.Sprintf("network %s is not available to this project: %s", network.UUID, visible.TenantMessage())))
				default:
					logger.Error().Err(err).Str("network_id", network.UUID).
						Msg("server create: checking a requested network failed")
					common.SendError(c, common.NewInternalServerError("failed to check the requested networks"))
				}
				return
			}
		}
	}

	// FR-002a: validate the keypair exists for this user before creating anything,
	// so an unknown key_name fails fast with a Nova-compatible 400 (matching real
	// Nova v2.1 strict input validation) instead of silently booting a keyless VM.
	if req.Server.KeyName != "" {
		var kpExists bool
		if err := svc.pool.QueryRow(c.Request.Context(),
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
		sgErr := svc.pool.QueryRow(c.Request.Context(),
			`SELECT id FROM security_groups
			 WHERE (id::text = $1 OR name = $1) AND project_id = $2
			 ORDER BY (id::text = $1) DESC
			 LIMIT 1`,
			name, projectID,
		).Scan(&sgID)
		if errors.Is(sgErr, pgx.ErrNoRows) {
			if name == "default" {
				if sgID, sgErr = neutron.EnsureDefaultSecurityGroup(c.Request.Context(), svc.pool, projectID); sgErr != nil {
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
		sgID, sgErr := neutron.EnsureDefaultSecurityGroup(c.Request.Context(), svc.pool, projectID)
		if sgErr != nil {
			log.Error().Err(sgErr).Str("operation", "ensure_default_sg").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to ensure default security group"))
			return
		}
		sgIDs = append(sgIDs, sgID)
	}

	// Create the boot volume BEFORE the instance row exists (SPEC-020 FR-025).
	//
	// Done here, synchronously, after every check that can refuse the request, rather than
	// in the boot goroutine: a request asking for a volume it cannot have must be refused
	// while it still can be, and a size the image does not fit in is knowable now. Once
	// created, the volume is handed to the existing boot-from-volume path as if the caller
	// had named it, so there is one code path that boots from a root volume, not two.
	var newBootVolumeID string
	if wantsNewBootVolume {
		if svc.cinderSvc == nil {
			common.SendError(c, common.NewBadRequestError(
				"this deployment cannot create a boot volume: no block-storage service is configured"))
			return
		}

		size, sizeErr := resolveBootVolumeSize(c.Request.Context(), svc.pool, bootVolImage, bootVolSize, flavor.DiskGB)
		var badSize *bootVolumeSizeError
		switch {
		case sizeErr == nil:
		case errors.Is(sizeErr, errBootImageNotFound):
			// Upstream's answer for a server create naming an unknown image.
			common.SendError(c, common.NewBadRequestError("Can not find requested image"))
			return
		case errors.As(sizeErr, &badSize):
			logger.Warn().Err(sizeErr).Str("image_id", bootVolImage).Int("requested_gb", bootVolSize).
				Msg("refusing server create: boot volume size")
			common.SendError(c, common.NewBadRequestError(badSize.msg))
			return
		default:
			log.Error().Err(sizeErr).Str("operation", "resolve_boot_volume_size").Msg("database error")
			common.SendError(c, common.NewInternalServerError("failed to read the boot image"))
			return
		}

		// Unnamed, as upstream creates it.
		volumeID, volErr := svc.cinderSvc.CreateBootVolumeFromImage(c.Request.Context(),
			"", projectID, userID, bootVolImage, size)
		if volErr != nil {
			// Never fall back to booting the image directly: the caller asked for a volume
			// and would be told it worked.
			//
			// A quota refusal is answered as one (SPEC-025-followup FR-020, FR-022): the
			// boot is refused BEFORE any instance row exists, so nothing is left implying a
			// volume, and the caller gets 413 naming the resource rather than a 503 that
			// reads as "the storage service is down". Recognised through a behavioural
			// interface because compute does not import block storage — see
			// QuotaExceededError.QuotaResource.
			var limited interface{ QuotaResource() string }
			if errors.As(volErr, &limited) {
				logger.Warn().Err(volErr).Str("resource", limited.QuotaResource()).
					Msg("refusing server create: block-storage quota exhausted")
				common.SendError(c, common.NewQuotaExceededError(limited.QuotaResource()))
				return
			}
			logger.Error().Err(volErr).Str("image_id", bootVolImage).Int("size_gb", size).
				Msg("boot volume could not be created; refusing the boot")
			common.SendError(c, common.NewServiceUnavailableError("could not create the boot volume"))
			return
		}

		logger.Info().Str("volume_id", volumeID).Str("image_id", bootVolImage).
			Int("size_gb", size).Bool("delete_on_termination", bootVolDOT).
			Msg("created boot volume from image")
		newBootVolumeID = volumeID

		// Hand it to the existing root-volume path. The image reference is cleared so nothing
		// downstream also treats this as a direct image boot.
		req.Server.BlockDeviceMappingV2 = append(req.Server.BlockDeviceMappingV2, BlockDeviceMapping{
			BootIndex: 0, UUID: volumeID, SourceType: "volume",
			DestinationType: "volume", DeleteOnTermination: bootVolDOT,
		})
		req.Server.ImageRef = ""
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
	err = database.WithTx(c.Request.Context(), svc.pool, func(tx pgx.Tx) error {
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
		if newBootVolumeID != "" {
			svc.removeBootVolume(newBootVolumeID)
		}
		common.SendError(c, common.NewInternalServerError("failed to create instance"))
		return
	}

	logger.Info().Str("instance_id", instanceID).Str("flavor", flavor.Name).Msg("Instance record created")

	// Persist cloud-init user_data so the metadata service serves it raw to
	// cloud-init (SPEC-009 WS-4 / T027).
	if decodedUserData != "" {
		if _, err := svc.pool.Exec(c.Request.Context(),
			`INSERT INTO instance_userdata (instance_id, userdata) VALUES ($1, $2)
			 ON CONFLICT (instance_id) DO UPDATE SET userdata = EXCLUDED.userdata`,
			instanceID, decodedUserData); err != nil {
			logger.Warn().Err(err).Str("instance_id", instanceID).Msg("failed to persist user_data")
		}
	}

	// Log instance action

	if _, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO instance_actions (instance_id, action, request_id, user_id, project_id, start_time, message)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, instanceID, "create", requestID(c), userID, projectID, now, "Instance created"); err != nil {
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
					if _, err := svc.pool.Exec(context.Background(),
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
			portsAllocated := false
			bootSucceeded := false
			// Any failure from the first successful allocation onward (a later
			// network, the boot volume, libvirt itself) leaves the earlier ports
			// allocated for a server that will never run. Release them on the way
			// out unless the boot completed (review 2026-10-13 N-6: they used to
			// leak — still listed by the API, leases still reserved).
			defer func() {
				if portsAllocated && !bootSucceeded && svc.neutronSvc != nil {
					rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer rcancel()
					if err := svc.neutronSvc.ReleaseInstancePorts(rctx, instanceID); err != nil {
						log.Warn().Err(err).Str("instance_id", instanceID).
							Msg("failed to release allocated ports after failed boot")
					}
				}
			}()
			if svc.neutronSvc != nil && len(req.Server.Networks) > 0 {
				for _, network := range req.Server.Networks {
					log.Debug().Str("network_id", network.UUID).Msg("Allocating port for network")
					// sgIDs: the groups resolved and validated above, so the port is
					// filtered by the groups the server reports (cloud-storage#579).
					portInfoRaw, err := svc.neutronSvc.AllocatePortForInstance(ctx, network.UUID, projectID, instanceID, sgIDs, isAdmin)
					if err != nil {
						// FAIL the boot. This used to log and `continue`, so the
						// server reached ACTIVE with an empty address map and no
						// interface at all — the user was told it was running while
						// it had no connectivity and could not reach the metadata
						// service (cloud-storage#547 B13, FR-015/FR-020).
						//
						// A partial success is still a failure: one attached network
						// out of two is not what was asked for, and a server that
						// silently has fewer interfaces than requested is the same
						// class of lie in a smaller size.
						log.Error().Err(err).
							Str("instance_id", instanceID).
							Str("network_id", network.UUID).
							Msg("Failed to allocate port from Neutron; failing the boot")
						svc.markInstanceError(instanceID, allocationFault(network.UUID, err))
						return
					}

					// Type assert to neutron.PortInfo
					portInfo, ok := portInfoRaw.(*neutron.PortInfo)
					if !ok {
						// The allocation reported success but returned something
						// unusable, so the guest would boot without this interface.
						// Same outcome as an outright failure, for the same reason.
						log.Error().
							Str("instance_id", instanceID).
							Str("network_id", network.UUID).
							Msg("port allocation returned an unusable result; failing the boot")
						svc.markInstanceError(instanceID,
							fmt.Sprintf("network %s returned an unusable port", network.UUID))
						return
					}
					networks = append(networks, hypervisor.NetworkConfig{
						PortID:     portInfo.ID,
						MACAddress: portInfo.MAC,
						BridgeName: networking.BridgeNameForNetwork(portInfo.NetworkID),
					})
					portsAllocated = true
				}
			}

			// Build a config-drive (cloud-init ISO) when a key and/or user_data is
			// supplied, so the VM is provisioned even without metadata-service
			// reachability (SPEC-009 WS-4 / T028).
			var cloudInit *hypervisor.CloudInitConfig
			var sshKey string
			if req.Server.KeyName != "" {
				if err := svc.pool.QueryRow(ctx,
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
				if err := svc.pool.QueryRow(ctx,
					"SELECT status, COALESCE(bootable, false), project_id, COALESCE(attached_to_instance_id::text, ''), COALESCE(rbd_pool, ''), COALESCE(rbd_image, '') FROM volumes WHERE id = $1",
					rootVol).Scan(&volStatus, &volBootable, &volProject, &volAttached, &rbdPool, &rbdImage); err != nil {
					log.Error().Err(err).Str("instance_id", instanceID).Str("volume_id", rootVol).Msg("boot-from-volume: root volume not found")
					svc.markInstanceError(instanceID, "boot volume "+rootVol+" was not found")
					return
				}
				if volProject != projectID || volStatus != "available" || !volBootable || volAttached != "" {
					log.Error().Str("instance_id", instanceID).Str("volume_id", rootVol).
						Str("status", volStatus).Bool("bootable", volBootable).Bool("owned", volProject == projectID).
						Msg("boot-from-volume: root volume not eligible (need available + bootable + owned + unattached)")
					svc.markInstanceError(instanceID, "boot volume "+rootVol+" is not available, bootable, owned by this project and unattached")
					return
				}
				// Root disk = the bootable volume's backing, selected by the one rule the
				// attach path and self-heal use too (volumeDisk): if the node-local qcow2
				// exists it's a local volume; otherwise it's RBD (rbd:pool/image, which
				// GenerateVMXML renders as a network disk). Do NOT key off rbd_pool/rbd_image —
				// Lightstack sets those defaults on every volume, local ones included, so they
				// can't distinguish the backend (SPEC-009 WS-5). Local is the validated
				// v1 mode; the RBD branch shares the (Ceph-untested) attach RBD path.
				imagePath = volumeRootDiskPath(rootVol, rbdPool, rbdImage)
				// boot-from-volume skips StageOverlay, so create the per-instance
				// directory the serial console.log lives in — otherwise libvirt
				// fails to start the domain (SPEC-009 WS-5).
				if err := hypervisor.EnsureInstanceDir(instanceID); err != nil {
					log.Error().Err(err).Str("instance_id", instanceID).Msg("boot-from-volume: failed to create instance dir")
					if _, dbErr := svc.pool.Exec(svc.ctx,
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
				if _, e := svc.pool.Exec(ctx,
					"INSERT INTO volume_attachments (id, volume_id, instance_id, device, delete_on_termination, attached_at) VALUES ($1, $2, $3, $4, $5, $6)",
					uuid.New().String(), rootVol, instanceID, rootDevice, rootDOT, bvNow); e != nil {
					log.Error().Err(e).Str("instance_id", instanceID).Str("volume_id", rootVol).Msg("boot-from-volume: failed to record root attachment")
					svc.markInstanceError(instanceID, "the boot volume could not be attached as the root device")
					return
				}
				if _, e := svc.pool.Exec(ctx,
					"UPDATE volumes SET attached_to_instance_id = $1, status = 'in-use', updated_at = $2 WHERE id = $3",
					instanceID, bvNow, rootVol); e != nil {
					log.Error().Err(e).Str("instance_id", instanceID).Str("volume_id", rootVol).Msg("boot-from-volume: failed to mark volume in-use")
					svc.markInstanceError(instanceID, "the boot volume could not be marked in use")
					return
				}
			} else {
				parsedImageUUID, err := uuid.Parse(req.Server.ImageRef)
				if err != nil {
					log.Error().Err(err).Str("image_ref", req.Server.ImageRef).Str("instance_id", instanceID).Msg("Invalid imageRef — not a valid UUID")
					if _, dbErr := svc.pool.Exec(svc.ctx,
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
						_ = svc.pool.QueryRow(svc.ctx,
							"SELECT COALESCE(checksum, '') FROM images WHERE id = $1", imageUUID).Scan(&checksum)
						if _, ferr := svc.imageStore.EnsureLocalBase(svc.ctx, imageUUID, checksum); ferr != nil {
							log.Error().Err(ferr).Str("instance_id", instanceID).Str("image_id", imageUUID).Msg("Failed to materialize image base for boot")
							if _, dbErr := svc.pool.Exec(svc.ctx,
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
						if _, dbErr := svc.pool.Exec(svc.ctx,
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
				if _, dbErr := svc.pool.Exec(dbCtx,
					"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3",
					"ERROR", time.Now(), instanceID); dbErr != nil {
					log.Error().Err(dbErr).Str("instance_id", instanceID).Msg("failed to mark instance ERROR after libvirt create failure")
				}
				return
			}

			log.Debug().Str("libvirt_uuid", libvirtUUID).Msg("VM created successfully")
			// The boot completed: the allocated ports belong to the running
			// server now, and the release defer must leave them alone.
			bootSucceeded = true

			logger.Info().
				Str("instance_id", instanceID).
				Str("libvirt_uuid", libvirtUUID).
				Dur("duration", time.Since(libvirtStart)).
				Msg("VM created successfully via libvirt")

			// Update instance with libvirt UUID
			dbCtx, dbCancel := context.WithTimeout(svc.ctx, 5*time.Second)
			defer dbCancel()
			if _, err := svc.pool.Exec(dbCtx, `
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
		if _, err := svc.pool.Exec(c.Request.Context(), `
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
	filter := newServerFilter(c, "")
	// Filters are applied to BOTH listings from the same accumulator, so a client cannot
	// get different rows for one query depending on which representation it asked for
	// (FR-002). A refused filter is answered here rather than silently dropped (FR-004).
	if ferr := filter.applyQueryFilters(c, ""); ferr != nil {
		common.SendError(c, ferr)
		return
	}

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
		// Marker-based pagination: get the marker's offset in the SAME ordering
		// and the SAME scope as the page query below. Under a cross-tenant view
		// the marker may name another project's server, so the project predicate
		// has to drop with it. A marker that does not resolve in this scope —
		// another project's server, an absent one, a malformed reference — is
		// refused with upstream's 400, never treated as no marker: that silently
		// restarted the listing at page 1 (SPEC-023 contract L6).
		// ROW_NUMBER is 1-based, so the marker's row number IS the count of rows
		// up to and including it — exactly the offset that starts the next page.
		// The previous `- 1` made the offset point AT the marker, so the marker
		// row was returned again on every page boundary: walking the pages
		// repeated a server rather than advancing past it. A marker is exclusive
		// upstream (L5).
		var markerOffset int
		// The marker predicate sits on the ranked subquery, not inside it, so it
		// is appended after the scope's arguments rather than joined with them.
		rank := filter.CloneArgs()
		markerPlaceholder := rank.Arg(markerParam)
		err := svc.pool.QueryRow(c.Request.Context(),
			`SELECT rn FROM (
			   SELECT id, ROW_NUMBER() OVER (ORDER BY created_at DESC, id DESC) AS rn
			   FROM instances`+filter.Where()+`
			 ) ranked WHERE id::text = `+markerPlaceholder,
			rank.Args()...,
		).Scan(&markerOffset)
		if errors.Is(err, pgx.ErrNoRows) {
			common.SendError(c, common.NewBadRequestError(fmt.Sprintf("marker [%s] not found", markerParam)))
			return
		}
		if err != nil {
			log.Error().Err(err).Str("marker", markerParam).Msg("failed to resolve pagination marker")
			common.SendError(c, common.NewInternalServerError("failed to list servers"))
			return
		}
		offset = markerOffset
	}

	// ORDER BY created_at alone is not a total order — created_at is not unique,
	// and instances created by a script or a seed share it. Ties make page
	// boundaries non-deterministic, which a cross-tenant listing makes far more
	// likely. id is the primary key, so (created_at, id) is total (FR-005, L5).
	where := filter.Where()
	rows, err := svc.pool.Query(c.Request.Context(),
		"SELECT id, name, project_id FROM instances"+where+
			" ORDER BY created_at DESC, id DESC"+filter.limitOffset(limit, offset),
		filter.Args()...,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_servers").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list servers"))
		return
	}
	defer rows.Close()

	var servers []gin.H
	for rows.Next() {
		var id, name, ownerProject string
		if err := rows.Scan(&id, &name, &ownerProject); err != nil {
			log.Warn().Err(err).Msg("failed to scan server row")
			continue
		}
		// tenant_id on every entry: without it a cross-tenant listing is a flat
		// list of servers with no way to tell whose they are (FR-006, L4).
		servers = append(servers, gin.H{
			"id":        id,
			"name":      name,
			"tenant_id": ownerProject,
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
	filter := newServerFilter(c, "i.")
	// Same accumulator, same refusals as the brief listing (FR-002, FR-004). The alias
	// differs because this query joins flavors, which is exactly why serverFilter takes the
	// alias rather than having the caller patch the rendered SQL afterwards.
	if ferr := filter.applyQueryFilters(c, "i."); ferr != nil {
		common.SendError(c, ferr)
		return
	}

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

	// Marker-based pagination. The marker is resolved in the SAME scope as the
	// page query: under a cross-tenant view it may name another project's server.
	// One that does not resolve in this scope is refused with upstream's 400,
	// never treated as no marker — that silently restarted the listing at page 1
	// (L6).
	if marker := c.Query("marker"); marker != "" {
		lookup := newServerFilter(c, "")
		// The marker resolves under the SAME query filters as the page, not scope alone
		// (FR-005). Without this the two listings diverge: the brief listing computes a
		// ROW_NUMBER offset inside the filtered set, so its marker is inherently
		// filter-relative, while this keyset lookup would resolve a marker the filter
		// excludes. A client only ever pages with a marker taken from the same filtered
		// listing, so the two agree in every case a client produces — but leaving them
		// asymmetric means the pathological case behaves differently depending on which
		// representation was asked for, which is what FR-002 forbids.
		if ferr := lookup.applyQueryFilters(c, ""); ferr != nil {
			common.SendError(c, ferr)
			return
		}
		lookup.Eq("id::text", marker)
		var markerCreatedAt time.Time
		var markerID string
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT created_at, id::text FROM instances"+lookup.Where(),
			lookup.Args()...,
		).Scan(&markerCreatedAt, &markerID)
		if errors.Is(err, pgx.ErrNoRows) {
			common.SendError(c, common.NewBadRequestError(fmt.Sprintf("marker [%s] not found", marker)))
			return
		}
		if err != nil {
			log.Error().Err(err).Str("marker", marker).Msg("failed to resolve pagination marker")
			common.SendError(c, common.NewInternalServerError("failed to list servers"))
			return
		}
		filter.beforeKeyset("i.", markerCreatedAt, markerID)
	}

	rows, err := svc.pool.Query(c.Request.Context(), fmt.Sprintf(`
		SELECT i.id, i.name, i.status, i.power_state, i.project_id, i.user_id,
		       i.flavor_id, i.image_id, i.created_at, i.updated_at, i.launched_at,
		       i.key_name,
		       f.vcpus, f.ram_mb, f.disk_gb, f.name as flavor_name
		FROM instances i
		LEFT JOIN flavors f ON i.flavor_id = f.id
		%s
		ORDER BY i.created_at DESC, i.id DESC%s
	`, filter.Where(), filter.limitOffset(limit, offset)), filter.Args()...)

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

		entry := gin.H{
			"id":                     id,
			"name":                   name,
			"status":                 status,
			"tenant_id":              rowProjectID,
			"user_id":                userID,
			"created":                createdAt.Format(time.RFC3339),
			"updated":                updatedAt.Format(time.RFC3339),
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

	// Attach addresses to every entry in ONE batched query (SPEC-025-followup FR-045).
	// This listing already batched security_groups the same way — the addresses were the
	// remaining per-row query, and the two now cost two round trips for any page size.
	addrMap, err := svc.instanceAddressMap(c.Request.Context(), ids)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_servers_detail_addresses").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list servers"))
		return
	}
	for _, s := range servers {
		sid, _ := s["id"].(string)
		if addrs, ok := addrMap[sid]; ok {
			s["addresses"] = addrs
		} else {
			// A server with no ports reports an EMPTY map, not null and not a missing key
			// (FR-047) — the per-instance reader returns gin.H{} for the same case, and a
			// client distinguishing "no addresses" from "field absent" would otherwise see
			// the two listings disagree.
			s["addresses"] = gin.H{}
		}
	}

	// Attach security_groups to every entry in one batched query (no N+1, FR-007).
	sgMap, err := securityGroupNamesForServers(c.Request.Context(), svc.pool, ids)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_servers_detail_sgs").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list security groups"))
		return
	}
	for _, s := range servers {
		sid, _ := s["id"].(string)
		s["security_groups"] = formatSecurityGroups(sgMap[sid])
	}

	// And their attached volumes, in one query (FR-027).
	volMap, err := volumesAttachedForServers(c.Request.Context(), svc.pool, ids)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_servers_detail_volumes").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list servers"))
		return
	}
	for _, s := range servers {
		sid, _ := s["id"].(string)
		s["os-extended-volumes:volumes_attached"] = attachedVolumes(volMap[sid])
	}

	if servers == nil {
		servers = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{"servers": servers})
}

// instanceAddressMap builds the address map for MANY instances in one query
// (SPEC-025-followup US7, FR-045 – FR-048).
//
// The detailed listing called getInstanceAddresses once per row, so a page of N servers
// cost N address queries. This is the same SQL — the same floating_ips LEFT JOIN with
// ARRAY_AGG ... FILTER, the same GROUP BY p.id giving one row per port — taking the page's
// instance ids at once and returning device_id so rows can be bucketed by server.
//
// THE PROJECT PREDICATE IS THE SUBTLE PART. The per-instance form filtered
// `p.project_id = $2`, where the argument was the INSTANCE's own project rather than the
// caller's. Batched, there is no single project to pass, so the pairing is stated per row
// instead: joining instances on `i.id = p.device_id AND i.project_id = p.project_id` keeps
// a cross-tenant administrative listing correctly attributed. Dropping the predicate
// entirely would let a port whose project disagrees with its instance's contribute an
// address to that instance — which is why it is a join condition and not an omission.
func (svc *Service) instanceAddressMap(ctx context.Context, instanceIDs []string) (map[string]gin.H, error) {
	byInstance := make(map[string]gin.H, len(instanceIDs))
	if len(instanceIDs) == 0 {
		return byInstance, nil
	}

	rows, err := svc.pool.Query(ctx, `
		SELECT p.device_id::text,
		       p.network_id,
		       p.fixed_ips,
		       n.name,
		       COALESCE(
		           ARRAY_AGG(f.floating_ip_address) FILTER (WHERE f.floating_ip_address IS NOT NULL),
		           '{}'
		       ) AS floating_ips
		FROM ports p
		JOIN networks n ON p.network_id = n.id
		JOIN instances i ON i.id = p.device_id AND i.project_id = p.project_id
		LEFT JOIN floating_ips f ON f.port_id = p.id
		WHERE p.device_id::text = ANY($1)
		GROUP BY p.device_id, p.id, p.network_id, p.fixed_ips, n.name
	`, instanceIDs)
	if err != nil {
		return nil, fmt.Errorf("query ports for %d instances: %w", len(instanceIDs), err)
	}
	defer rows.Close()

	for rows.Next() {
		var deviceID, networkID, networkName string
		var fixedIPsJSON []byte
		var floatingIPs []string
		if err := rows.Scan(&deviceID, &networkID, &fixedIPsJSON, &networkName, &floatingIPs); err != nil {
			log.Warn().Err(err).Msg("failed to scan batched address row")
			continue
		}
		if byInstance[deviceID] == nil {
			byInstance[deviceID] = gin.H{}
		}
		appendAddressEntries(byInstance[deviceID], networkName, fixedIPsJSON, floatingIPs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ports for %d instances: %w", len(instanceIDs), err)
	}
	return byInstance, nil
}

// appendAddressEntries merges one port's fixed and floating addresses into a network's
// entry list, shared by the per-instance and batched readers so the two cannot describe
// the same port differently.
//
// Upstream chains fixed and floating addresses into the SAME per-network list and tags
// each entry with its own kind; the type was once the literal "fixed", so only one kind
// could ever appear however many the deployment had.
func appendAddressEntries(addresses gin.H, networkName string, fixedIPsJSON []byte, floatingIPs []string) {
	var fixedIPs []map[string]any
	if err := json.Unmarshal(fixedIPsJSON, &fixedIPs); err != nil {
		return
	}

	var entries []gin.H
	if existing, ok := addresses[networkName].([]gin.H); ok {
		entries = existing
	}
	for _, ipInfo := range fixedIPs {
		if ipAddr, ok := ipInfo["ip_address"].(string); ok {
			entries = append(entries, addressEntry(ipAddr, "fixed"))
		}
	}
	for _, floating := range floatingIPs {
		if floating != "" {
			entries = append(entries, addressEntry(floating, "floating"))
		}
	}
	if len(entries) > 0 {
		addresses[networkName] = entries
	}
}

// getInstanceAddresses retrieves network addresses for an instance from ports.
//
// projectID MUST be the instance's OWNING project, not the caller's. Both call
// sites read it from the instance row for exactly this reason: once an
// administrator can view another project's server (SPEC-023 FR-004), passing the
// caller's project here would find no ports and return an empty addresses dict —
// a server that appears to have no network at all. Every sub-lookup feeding a
// response has to widen with its parent.
func (svc *Service) getInstanceAddresses(ctx context.Context, instanceID, projectID string) (gin.H, error) {
	addresses := gin.H{}

	// One row per port, with every floating address associated to it aggregated
	// alongside its fixed ones.
	//
	// The floating join is the fix for cloud-storage#547 B17: this query read
	// ports.fixed_ips alone, so a floating address that was successfully associated —
	// bound to the port and ACTIVE — never appeared in the one field `server show`,
	// `server list`'s Networks column, the dashboard's instance list and the
	// infrastructure-as-code provider all read. The user was told the association had
	// not taken effect.
	//
	// Aggregated in SQL rather than by a second query per port: a listing already runs
	// this once per instance, and one round trip per port on top of that would make the
	// cost quadratic in a project's size.
	rows, err := svc.pool.Query(ctx, `
		SELECT p.network_id,
		       p.fixed_ips,
		       n.name,
		       COALESCE(
		           ARRAY_AGG(f.floating_ip_address) FILTER (WHERE f.floating_ip_address IS NOT NULL),
		           '{}'
		       ) AS floating_ips
		FROM ports p
		JOIN networks n ON p.network_id = n.id
		LEFT JOIN floating_ips f ON f.port_id = p.id
		WHERE p.device_id = $1 AND p.project_id = $2
		GROUP BY p.id, p.network_id, p.fixed_ips, n.name
	`, instanceID, projectID)

	if err != nil {
		return nil, fmt.Errorf("query ports for instance %s: %w", instanceID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var networkID, networkName string
		var fixedIPsJSON []byte
		var floatingIPs []string

		if err := rows.Scan(&networkID, &fixedIPsJSON, &networkName, &floatingIPs); err != nil {
			log.Warn().Err(err).Msg("failed to scan network row")
			continue
		}

		// Shared with the batched reader (FR-046): both must describe a port the same
		// way, and a second copy of this merge is how they would come to disagree.
		appendAddressEntries(addresses, networkName, fixedIPsJSON, floatingIPs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ports for instance %s: %w", instanceID, err)
	}

	return addresses, nil
}

// addressEntry is one entry of a server's per-network address list, in upstream's
// shape. The kind is a parameter precisely because it used to be a constant.
func addressEntry(addr, kind string) gin.H {
	version := 4
	if strings.Contains(addr, ":") {
		version = 6
	}
	return gin.H{
		"addr":            addr,
		"version":         version,
		"OS-EXT-IPS:type": kind,
	}
}

// GetServer returns details for a single server
func (svc *Service) GetServer(c *gin.Context) {
	// Owner-or-administrator: an administrator may show any project's server
	// (SPEC-023 FR-004). The lookup below is then keyed on the resolved id and
	// its owning project rather than on the caller's own.
	instanceID, projectID, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}

	var id, name, status string
	var userID, flavorID, imageID, keyName, faultMessage sql.NullString
	var faultCode sql.NullInt32
	var faultCreated sql.NullTime
	var powerState int
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT id, name, status, power_state, project_id,
		       user_id::text, flavor_id::text, image_id::text,
		       created_at, updated_at, key_name,
		       fault_code, fault_message, fault_created
		FROM instances
		WHERE id = $1 AND project_id = $2
	`, instanceID, projectID).Scan(&id, &name, &status, &powerState, &projectID, &userID, &flavorID, &imageID,
		&createdAt, &updatedAt, &keyName, &faultCode, &faultMessage, &faultCreated)

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
	sgNames, err := securityGroupNamesForServer(c.Request.Context(), svc.pool, id)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_server_sgs").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get security groups"))
		return
	}
	response["security_groups"] = formatSecurityGroups(sgNames)

	volMap, err := volumesAttachedForServers(c.Request.Context(), svc.pool, []string{id})
	if err != nil {
		log.Error().Err(err).Str("operation", "get_server_volumes").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to get server"))
		return
	}
	response["os-extended-volumes:volumes_attached"] = attachedVolumes(volMap[id])

	// fault: why a failed server failed, readable from the server itself (FR-016).
	//
	// Present only while the server is in ERROR and a reason was recorded, which is
	// upstream's behaviour — a server that never failed carries no fault key at all,
	// so this is invisible to every response that was already correct. Before this a
	// user whose boot failed had nowhere to look: `server show` reported a status and
	// nothing else, and the reason existed only in the service log.
	//
	// `details` is deliberately omitted: upstream fills it with a traceback for
	// administrators, and there is none to offer here.
	if status == "ERROR" && faultMessage.Valid && faultMessage.String != "" {
		fault := gin.H{
			"code":    http.StatusInternalServerError,
			"message": faultMessage.String,
		}
		if faultCode.Valid {
			fault["code"] = int(faultCode.Int32)
		}
		if faultCreated.Valid {
			fault["created"] = faultCreated.Time.Format(time.RFC3339)
		}
		response["fault"] = fault
	}

	c.JSON(http.StatusOK, gin.H{"server": response})
}

// DeleteServer deletes a server
func (svc *Service) DeleteServer(c *gin.Context) {
	logger := middleware.GetLogger(c)
	start := time.Now()

	// Owner-or-administrator: an administrator may delete any project's server
	// (SPEC-023 FR-004). Note a *name* still never spans projects — targetInstance
	// keeps name matching inside the caller's own project, so an administrator's
	// `server delete web-1` cannot reach a different tenant's `web-1` (FR-005c).
	instanceID, projectID, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}

	middleware.LogOperationStart(c, "delete", "server", instanceID)
	recordInstanceAction(c, svc.pool, instanceID, projectID, "delete")

	var instanceUUID string
	var libvirtDomainID sql.NullString
	var instanceImageID sql.NullString
	queryStart := time.Now()
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT id, libvirt_domain_id, image_id FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&instanceUUID, &libvirtDomainID, &instanceImageID)
	middleware.LogDatabaseQuery(c, "SELECT libvirt_domain_id", time.Since(queryStart), err)

	if errors.Is(err, pgx.ErrNoRows) {
		logger.Warn().Str("instance_id", instanceID).Msg("Instance not found")
		middleware.LogOperationEnd(c, "delete", "server", instanceID, time.Since(start), err)
		common.SendError(c, common.NewNotFoundError("instance"))
		return
	}

	// Revoke console tokens before the domain goes, not merely before the row, so
	// no link outlives the console it was issued for. The FK cascade removes the
	// rows, but not their files (SPEC-021 FR-014).
	if !svc.revokeConsoleTokens(c, instanceUUID, "instance deleted") {
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
	tx, txErr := svc.pool.Begin(ctx)
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

	// The instance's ports, split by who owns them (review 2026-10-13 N-8):
	//
	// - Ports Neutron allocated for the boot (device_owner 'compute:nova') are
	//   Nova's to delete. Without this they linger after the server is gone and
	//   deleting the network then fails with "network has active ports" — which
	//   breaks `terraform destroy` (SPEC-009 / SC-007) and leaks ports.
	//
	// - A port the tenant created and attached (os-interface with port_id) is
	//   the tenant's resource, not the server's: upstream detaches it — clears
	//   the device, parks it DOWN — rather than deleting it. The blanket
	//   `DELETE ... WHERE device_id = $1` used to destroy it: the tenant lost a
	//   port they made, with its fixed IP, by deleting a server.
	if _, e := tx.Exec(ctx, `
		UPDATE ports
		SET device_id = NULL, device_owner = NULL, status = 'DOWN', updated_at = NOW()
		WHERE device_id = $1 AND COALESCE(device_owner, '') <> 'compute:nova'
	`, instanceUUID); e != nil {
		logger.Error().Err(e).Str("instance_id", instanceUUID).Msg("delete: detach tenant ports failed")
		common.SendError(c, common.NewInternalServerError("failed to delete server"))
		return
	}
	if _, e := tx.Exec(ctx,
		"DELETE FROM ports WHERE device_id = $1 AND device_owner = 'compute:nova'", instanceUUID); e != nil {
		logger.Error().Err(e).Str("instance_id", instanceUUID).Msg("delete: remove instance ports failed")
		common.SendError(c, common.NewInternalServerError("failed to delete server"))
		return
	}

	// Delete the instance row — by its resolved id alone: matching the name too
	// would also delete a server named after this one's id, and leave its ports
	// and attachments behind.
	queryStart = time.Now()
	_, err = tx.Exec(ctx,
		"DELETE FROM instances WHERE id = $1 AND project_id = $2",
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

// consoleActions are the actions dispatched BEFORE the shared resolve-and-record
// step in ServerAction.
//
// They resolve their own target through resolveServerOr404 — the same
// owner-or-administrator rule, not a weaker one — and stay out of the instance's
// action history: reading a console or its boot log is not an action taken on
// the server.
var consoleActions = map[string]func(*Service, *gin.Context, any){
	"os-getVNCConsole":    (*Service).GetVNCConsoleAction,
	"os-getConsoleOutput": (*Service).GetConsoleOutputAction,
	"os-getSerialConsole": (*Service).GetSerialConsoleAction,
	"os-getSPICEConsole":  (*Service).GetSPICEConsoleAction,
	"os-getRDPConsole":    (*Service).GetRDPConsoleAction,
}

// serverActions are the actions dispatched AFTER the target has been resolved
// and authorized, keyed by the upstream action name.
//
// A table rather than an if-chain: with exactly one key per body (enforced in
// ServerAction) there is no precedence to encode, so the ladder's ordering was
// carrying no meaning — only the risk that a later branch became unreachable.
// Membership is now the whole dispatch rule, and the recorded action name and
// the handler invoked are read from the same key by construction.
//
// Every entry takes the RESOLVED instance id and OWNING project rather than
// re-reading them: the target is resolved once, in ServerAction, and passing it
// down is what stops the rule from being re-implemented ~25 times. Handlers that
// do not need a value take it as _ so the signature stays uniform.
var serverActions = map[string]func(*Service, *gin.Context, any, string, string){
	"suspend":       func(s *Service, c *gin.Context, _ any, id, proj string) { s.SuspendInstance(c, id, proj) },
	"resume":        func(s *Service, c *gin.Context, _ any, id, proj string) { s.ResumeInstance(c, id, proj) },
	"shelve":        func(s *Service, c *gin.Context, _ any, id, proj string) { s.ShelveInstance(c, id, proj) },
	"shelveOffload": func(s *Service, c *gin.Context, _ any, id, proj string) { s.ShelveInstance(c, id, proj) },
	"unshelve":      func(s *Service, c *gin.Context, _ any, id, proj string) { s.UnshelveInstance(c, id, proj) },
	"confirmResize": func(s *Service, c *gin.Context, _ any, id, proj string) { s.ConfirmResizeInstance(c, id, proj) },
	"revertResize":  func(s *Service, c *gin.Context, _ any, id, proj string) { s.RevertResizeInstance(c, id, proj) },
	"evacuate":      func(s *Service, c *gin.Context, _ any, id, proj string) { s.EvacuateInstance(c, id, proj) },
	"migrate":       func(s *Service, c *gin.Context, _ any, id, proj string) { s.MigrateInstance(c, id, proj) },
	"restore":       func(s *Service, c *gin.Context, _ any, id, _ string) { s.RestoreInstance(c, id) },

	"resize": func(s *Service, c *gin.Context, d any, id, proj string) { s.ResizeInstanceAction(c, d, id, proj) },
	// The only entry that still discards both: createImage names a NEW image and never
	// looks the server up, so it has no reference to resolve.
	"createImage": func(s *Service, c *gin.Context, d any, _, _ string) { s.CreateImageAction(c, d) },

	"rebuild": func(s *Service, c *gin.Context, d any, id, proj string) { s.RebuildInstanceAction(c, d, id, proj) },
	"rescue":  func(s *Service, c *gin.Context, d any, id, proj string) { s.RescueInstanceAction(c, d, id, proj) },

	"pause":       func(s *Service, c *gin.Context, _ any, id, proj string) { s.PauseInstanceAction(c, id, proj) },
	"unpause":     func(s *Service, c *gin.Context, _ any, id, proj string) { s.UnpauseInstanceAction(c, id, proj) },
	"lock":        func(s *Service, c *gin.Context, _ any, id, proj string) { s.LockInstanceAction(c, id, proj) },
	"unlock":      func(s *Service, c *gin.Context, _ any, id, proj string) { s.UnlockInstanceAction(c, id, proj) },
	"forceDelete": func(s *Service, c *gin.Context, _ any, id, proj string) { s.ForceDeleteInstanceAction(c, id, proj) },

	// These read their body from the context rather than a parameter; left as-is
	// so this refactor changes dispatch only.
	"os-migrateLive": func(s *Service, c *gin.Context, d any, id, proj string) {
		setActionData(c, d)
		s.LiveMigrateInstance(c, id, proj)
	},
	"addSecurityGroup": func(s *Service, c *gin.Context, d any, id, proj string) {
		setActionData(c, d)
		s.AddSecurityGroup(c, id, proj)
	},
	"removeSecurityGroup": func(s *Service, c *gin.Context, d any, id, proj string) {
		setActionData(c, d)
		s.RemoveSecurityGroup(c, id, proj)
	},
	"changePassword": func(s *Service, c *gin.Context, d any, id, proj string) {
		setActionData(c, d)
		s.ChangePassword(c, id, proj)
	},
	"createBackup": func(s *Service, c *gin.Context, d any, id, proj string) {
		setActionData(c, d)
		s.CreateBackupAction(c, id, proj)
	},
	"os-resetState": func(s *Service, c *gin.Context, d any, id, proj string) {
		setActionData(c, d)
		s.ResetStateAction(c, id, proj)
	},

	"os-resetNetwork": func(s *Service, c *gin.Context, _ any, id, proj string) { s.ResetNetworkAction(c, id, proj) },
}

// setActionData passes an action's body to handlers that read it from the
// context rather than taking it as a parameter.
func setActionData(c *gin.Context, data any) { c.Set("action_data", data) }

// lifecycleActions are the actions lifecycleAction performs on the guest's libvirt
// domain. With consoleActions and serverActions they are every action ServerAction
// accepts; anything else is refused before the target is resolved.
var lifecycleActions = map[string]bool{"reboot": true, "os-stop": true, "os-start": true}

// instanceActionName is the name a dispatched action is recorded under in the
// server's action history: upstream's vocabulary (nova/compute/instance_actions.py),
// so os-instance-actions reads as it does against Nova. Most actions are their
// request key without the "os-" prefix; these two are not.
func instanceActionName(action string) string {
	switch action {
	case "os-migrateLive":
		return "live-migration" // LIVE_MIGRATION
	case "forceDelete":
		return "delete" // upstream's force_delete records DELETE
	}
	return strings.TrimPrefix(action, "os-")
}

// ServerAction performs an action on a server
func (svc *Service) ServerAction(c *gin.Context) {
	instanceID := c.Param("id")

	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Warn().Err(err).Str("operation", "server_action_bind").Msg("Failed to bind JSON")
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Exactly one action per request, as upstream's API documents — every action
	// form in the Nova api-ref carries a single, required key.
	//
	// Without this, a body naming two actions was answered `202` after performing
	// whichever the dispatch ladder below reached first and silently discarding
	// the rest: a contradictory request accepted, partially executed, and reported
	// as success. That is the same shape SPEC-023 removes elsewhere — the
	// domain-scoped request answered `201` with no scope (FR-009).
	//
	// It also makes the dispatch deterministic by construction: with one key
	// there is no map-iteration order to depend on, so the recorded action can
	// never name something other than the action performed (FR-007).
	if len(req) != 1 {
		common.SendError(c, common.NewBadRequestError("exactly one action per request"))
		return
	}

	// Exactly one key, so this IS the action — the name that gets recorded and
	// the handler that runs are now read from the same place.
	var action string
	var actionData any
	for k, v := range req {
		action, actionData = k, v
	}

	log.Debug().Str("instance_id", instanceID).
		Str("caller_project", c.GetString("project_id")).
		Interface("action", req).Msg("ServerAction")

	// Console actions run first — they resolve their own target and are not
	// recorded as actions on the server.
	if handler, ok := consoleActions[action]; ok {
		handler(svc, c, actionData)
		return
	}

	// Refuse an action this API does not know now, before the instance is resolved,
	// recorded or has its console tokens revoked — the order upstream's dispatcher uses
	// (nova/api/openstack/wsgi.py) — so a request naming no action this API knows
	// cannot invalidate live console links, or become a 500 when the revoke fails
	// closed.
	handler, known := serverActions[action]
	if !known && !lifecycleActions[action] {
		common.SendError(c, common.NewBadRequestError("unknown action"))
		return
	}
	// A reboot's body is checked here too, for the same reason: a malformed one must not
	// revoke the instance's console links on its way to a 400.
	var hardReboot bool
	if action == "reboot" {
		hard, err := parseRebootType(actionData)
		if err != nil {
			common.SendError(c, common.NewBadRequestError(
				"Invalid input for field/attribute reboot: "+err.Error()))
			return
		}
		hardReboot = hard
	}

	// Authorize ONCE, here, for every remaining action this dispatches to
	// (SPEC-023 FR-001, T017).
	//
	// The ~25 sub-handlers below each read c.GetString("project_id") and scope
	// their own queries by it. Resolving the target in each of them would be 25
	// chances for the rule to diverge. Instead the target is resolved once here and
	// the context's project is rebound to the OWNING project for the rest of the
	// request, so every sub-handler operates on the effective project without
	// needing to know the rule exists.
	//
	// Rebinding is the correct semantics, not a shortcut: an administrator acting
	// on a tenant's instance should have the resulting state, quota accounting and
	// action record attributed to that tenant (FR-007), not to the administrator's
	// own project. A non-administrator never reaches this line for an instance
	// they do not own — targetInstance refuses first — so the rebind cannot widen
	// anyone's authority.
	//
	// Placed AFTER the console actions above deliberately: those resolve their
	// own target through the same helper and are reads, so they must not land in
	// the action history this step writes.
	//
	// Actions gated to administrators only (evacuate, os-resetState) also check
	// the role and resolve their own target; they do not rely on this.
	ownedID, ownerProject, authorized := resolveServerOr404(c, svc.pool, instanceID)
	if !authorized {
		return
	}
	c.Set("project_id", ownerProject)
	projectID := ownerProject
	instanceID = ownedID

	// Record against the OWNER, so the affected tenant can see what was done to
	// their server and by whom — one call site for every action this dispatches
	// to (SPEC-023 FR-007, FR-007a).
	recordInstanceAction(c, svc.pool, instanceID, ownerProject, instanceActionName(action))

	if known {
		handler(svc, c, actionData, instanceID, projectID)
		return
	}

	// Everything left is a libvirt lifecycle action (reboot, os-stop, os-start).
	svc.lifecycleAction(c, action, instanceID, projectID, hardReboot)
}

// lifecycleAction performs the actions that reach the hypervisor directly —
// reboot, os-stop, os-start. ServerAction has already refused every other action,
// and a malformed reboot, before resolving the target; hardReboot is the reboot
// type it parsed.
//
// Split out of ServerAction because it is the only branch that needs the guest's
// libvirt domain.
//
// instanceID is already resolved and projectID is already the OWNING project.
func (svc *Service) lifecycleAction(c *gin.Context, action, instanceID, projectID string, hardReboot bool) {
	// The target is already resolved; this reads the guest's libvirt domain,
	// which resolveServerOr404 does not carry. That resolved UUID is also what
	// console-token revocation keys on below, since the caller may have
	// addressed the instance by name (SPEC-021 FR-014).
	//
	// updated_at is read with it as the row's revision: a correction this action makes
	// from what it observes is written only if nothing has written the row since.
	var libvirtDomainID any
	var readAt *time.Time
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT libvirt_domain_id, updated_at FROM instances WHERE id = $1",
		instanceID,
	).Scan(&libvirtDomainID, &readAt)

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

	// Every action left at this point (reboot, os-stop, os-start) can move or end
	// the guest's console, so outstanding links go first. Revoke BEFORE acting, not
	// after: afterwards leaves a window where the old link is live against a
	// restarting domain (SPEC-021 FR-014). A stop that completes later, or one the
	// guest does itself, is caught by the reconciler's socket check (FR-014a).
	if !svc.revokeConsoleTokens(c, instanceID, "lifecycle action") {
		return
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
		switch action {
		case "reboot":
			// Just mark as rebooting then active, under upstream's status for the type.
			status := "REBOOT"
			if hardReboot {
				status = "HARD_REBOOT"
			}
			if _, err := svc.pool.Exec(c.Request.Context(),
				"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3 AND project_id = $4",
				status, time.Now(), instanceID, projectID); err != nil {
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
				if _, err := svc.pool.Exec(ctx,
					"UPDATE instances SET status = $1, updated_at = $2 WHERE id = $3 AND project_id = $4",
					"ACTIVE", time.Now(), instanceID, projectID); err != nil {
					log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark stub instance ACTIVE after reboot")
				}
			}()
		case "os-stop":
			if _, err := svc.pool.Exec(c.Request.Context(),
				"UPDATE instances SET status = $1, power_state = $2, updated_at = $3 WHERE id = $4 AND project_id = $5",
				"SHUTOFF", 4, time.Now(), instanceID, projectID); err != nil {
				log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark stub instance SHUTOFF")
			}
		case "os-start":
			if _, err := svc.pool.Exec(c.Request.Context(),
				"UPDATE instances SET status = $1, power_state = $2, updated_at = $3 WHERE id = $4 AND project_id = $5",
				"ACTIVE", 1, time.Now(), instanceID, projectID); err != nil {
				log.Error().Err(err).Str("instance_id", instanceID).Msg("failed to mark stub instance ACTIVE on start")
			}
		}
		c.Status(http.StatusAccepted)
		return
	}

	// A stop may wait out the guest's entire grace period before forcing it off, so the
	// bound has to exceed that period. At the old flat 10s the wait would be cancelled
	// on every guest slower than ten seconds, and every one of them force-killed.
	ctx, cancel := context.WithTimeout(c.Request.Context(), svc.guestShutdownGrace()+common.LifecycleResponseSlack)
	defer cancel()

	// Each action names the state the server is in once it succeeds, so the record can
	// be written from one place rather than three (SPEC-025 FR-022, FR-023, FR-024a).
	var (
		wantStatus   string
		wantPower    int
		healedDomain string
	)
	switch action {
	case "reboot":
		err = svc.vmManager.RebootVM(ctx, libvirtDomainStr, hardReboot)
		wantStatus, wantPower = "ACTIVE", 1
	case "os-stop":
		// The guest may already be off: shut down from inside, or stopped by an
		// earlier request whose record was written optimistically. libvirt errors on a
		// shutdown request to an inactive domain, and that error had nowhere to go but
		// a 500 (cloud-storage#547 B12). Upstream answers 409 here — its precondition
		// is "the server status must be ACTIVE or ERROR".
		//
		// Correcting the record before refusing is what stops the refusal latching: a
		// 409 over a record that still disagrees with the hypervisor locks the server
		// out of the only action that would have cleared the disagreement.
		if svc.observedRunState(ctx, libvirtDomainStr) == hypervisor.RunStateDown {
			svc.persistObservedRunState(c.Request.Context(), instanceID, projectID, "SHUTOFF", 4, readAt)
			common.SendError(c, common.NewOperationConflictError(action, "the server is already stopped"))
			return
		}
		err = svc.vmManager.StopVM(ctx, libvirtDomainStr, svc.guestShutdownGrace())
		wantStatus, wantPower = "SHUTOFF", 4
	case "os-start":
		// The mirror of the above, and the half the host probe hit eight times running:
		// the record said SHUTOFF while the guest had never stopped, so every start
		// reached libvirt with an already-active domain and answered 500. Upstream's
		// precondition is "the server status must be SHUTOFF".
		if svc.observedRunState(ctx, libvirtDomainStr) == hypervisor.RunStateUp {
			svc.persistObservedRunState(c.Request.Context(), instanceID, projectID, "ACTIVE", 1, readAt)
			common.SendError(c, common.NewOperationConflictError(action, "the server is already running"))
			return
		}
		err = svc.vmManager.StartVM(ctx, libvirtDomainStr)
		if errors.Is(err, hypervisor.ErrDomainNotFound) {
			// Self-heal (FR-025): the server exists and its disk is still there;
			// only libvirt's record of the guest has gone. Re-create it from stored
			// state and start it, the way upstream's hard reboot does, rather than
			// returning an unexplained error over a record that claims the server
			// runs. The new domain identifier replaces the stale one.
			healedDomain, err = svc.reviveGuest(ctx, instanceID)
			if err != nil {
				// FR-025c / FR-026b: a self-heal that cannot complete is an action
				// that cannot be performed, not an internal fault — the server
				// exists and the request was well formed, so this is a state an
				// operator has to resolve. Leave the record honest before
				// answering: it must not go on claiming the server runs.
				svc.persistRunState(c, instanceID, projectID, "ERROR", 0, "")
				log.Error().Err(err).Str("instance_id", instanceID).
					Msg("self-heal failed; the server has no guest and could not be given one")
				// Why it could not — a path on the node, a hypervisor error — is the
				// log line's above; the tenant reads this answer.
				common.SendError(c, common.NewOperationConflictError(action,
					"the server's guest is absent and could not be re-created"))
				return
			}
			svc.recordSelfHeal(c, instanceID, projectID, healedDomain)
		}
		wantStatus, wantPower = "ACTIVE", 1
	}

	if err != nil {
		sendLifecycleError(c, action, err)
		return
	}

	// Persist what just happened. The real-mode branch previously wrote NOTHING —
	// only the stub branch recorded the transition — so a SUCCESSFUL stop left
	// "ACTIVE"/power_state=1 in the row and the API went on serving a running server
	// that was shut off (cloud-storage#547 B12). Nothing reconciled the two, and
	// reading the server reported ACTIVE either way, so the divergence stayed
	// invisible until the next action failed.
	svc.persistRunState(c, instanceID, projectID, wantStatus, wantPower, healedDomain)

	c.Status(http.StatusAccepted)
}

// recordSelfHeal notes on the triggering action's own record that the guest had to be
// re-created, so a repeatedly vanishing guest is discoverable rather than masked by a
// start that keeps succeeding (FR-025b).
//
// Deliberately NOT a new action name or a new endpoint (FR-025d): the os-start row is
// already written for this request and already carries a message column, so the repair
// is visible to the server's owner through os-instance-actions without inventing an
// event upstream does not have.
func (svc *Service) recordSelfHeal(c *gin.Context, instanceID, projectID, domainID string) {
	if _, err := svc.pool.Exec(c.Request.Context(),
		`UPDATE instance_actions
		    SET message = $1, updated_at = $2
		  WHERE instance_id = $3 AND project_id = $4 AND request_id = $5`,
		"guest was absent and was re-created from stored state (domain "+domainID+")",
		time.Now(), instanceID, projectID, requestID(c),
	); err != nil {
		// Best-effort: the guest is already back and the start is going to succeed.
		// Losing the note is worth reporting, not worth failing the request for.
		log.Warn().Err(err).Str("instance_id", instanceID).
			Msg("self-heal succeeded but could not be recorded on the action")
	}
}

// persistRunState records the state an action just produced. Best-effort by design:
// the hypervisor has already acted, so failing the request here would report a
// failure for work that was done. The divergence it leaves is what the state
// reconciler exists to correct (FR-024b).
//
// healedDomain is non-empty only when a self-heal re-created the guest, in which case
// the new domain identifier replaces the stale one.
func (svc *Service) persistRunState(c *gin.Context, instanceID, projectID, status string, power int, healedDomain string) {
	query := "UPDATE instances SET status = $1, power_state = $2, updated_at = $3 WHERE id = $4 AND project_id = $5"
	args := []any{status, power, time.Now(), instanceID, projectID}
	if healedDomain != "" {
		query = "UPDATE instances SET status = $1, power_state = $2, updated_at = $3, libvirt_domain_id = $6 WHERE id = $4 AND project_id = $5"
		args = append(args, healedDomain)
	}
	if _, err := svc.pool.Exec(c.Request.Context(), query, args...); err != nil {
		log.Error().Err(err).
			Str("instance_id", instanceID).Str("status", status).
			Msg("lifecycle action succeeded but its state could not be persisted")
	}
}

// persistObservedRunState records a run state this action OBSERVED rather than caused — the
// correction a refused stop or start makes before answering 409 — and reports whether it did.
//
// Only if the row is as this action read it (readAt, its updated_at then): the observation is
// a sample taken a moment earlier, and a concurrent start or stop that finished since recorded
// a newer truth, which a stale "running" or "stopped" must not overwrite. The state reconciler
// guards its own corrections the same way. NULL-safe, so a row that never had an updated_at is
// still correctable. Upstream serialises lifecycle actions per instance instead; that would
// mean holding a lock through a stop's whole grace period.
//
// An action's own outcome is not written through here: what it just did is the newest truth.
func (svc *Service) persistObservedRunState(ctx context.Context, instanceID, projectID, status string, power int, readAt *time.Time) bool {
	tag, err := svc.pool.Exec(ctx,
		`UPDATE instances SET status = $1, power_state = $2, updated_at = $3
		  WHERE id = $4 AND project_id = $5 AND updated_at IS NOT DISTINCT FROM $6`,
		status, power, time.Now(), instanceID, projectID, readAt)
	if err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Str("status", status).
			Msg("could not correct the record from the observed guest state")
		return false
	}
	return tag.RowsAffected() > 0
}

// observedRunState reports what the hypervisor says the guest is doing, or
// RunStateUnknown when it will not say.
//
// Unknown covers three cases that must all behave alike: no hypervisor wired in, an
// unreachable one, and an absent guest. None is grounds for refusing an action — a
// blip must not become a precondition failure, and an absent guest belongs to the
// self-heal path, which is downstream of here (FR-025).
func (svc *Service) observedRunState(ctx context.Context, libvirtDomainID string) hypervisor.RunState {
	if svc.vmManager == nil {
		return hypervisor.RunStateUnknown
	}
	state, err := svc.vmManager.GuestRunState(ctx, libvirtDomainID)
	if err != nil {
		// Logged at debug, not warn: an absent guest reaches this on every self-heal,
		// and the action about to run reports the outcome that matters.
		log.Debug().Err(err).Str("libvirt_domain", libvirtDomainID).
			Msg("could not read guest run state; proceeding with the action")
		return hypervisor.RunStateUnknown
	}
	return state
}

// sendLifecycleError turns a hypervisor failure into the answer a client can act on.
//
// Three outcomes have to stay distinguishable (FR-026, FR-026a): the target is absent
// (answered earlier, by the dispatcher), the action cannot be performed in the
// server's current state, or something failed internally. Every libvirt failure used
// to be a flat 500, which is what made B12 unreadable from outside — a vanished guest
// and an unreachable libvirt produced the identical answer, so a client could not tell
// whether retrying made any sense.
//
// A guest the hypervisor does not have is a STATE problem, so it is a conflict.
// Deliberately not 503 for an unreachable hypervisor, apt as that would be: upstream
// compute does not emit it for a server action, clients branch on status against
// upstream, and Article I outranks descriptive precision. The extra detail is in the log.
// Any other failure goes through startRefusal, which names a console socket directory
// that is not ready (SPEC-021 FR-017a) and otherwise keeps the generic message.
func sendLifecycleError(c *gin.Context, action string, err error) {
	log.Error().Err(err).Str("operation", action).Msg("libvirt error")
	if errors.Is(err, hypervisor.ErrDomainNotFound) {
		common.SendError(c, common.NewOperationConflictError(action,
			"the server has no guest on the hypervisor"))
		return
	}
	common.SendError(c, startRefusal(err, "failed to perform "+action))
}

// ListFlavors lists all flavors (brief)
func (svc *Service) ListFlavors(c *gin.Context) {
	rows, err := svc.pool.Query(c.Request.Context(),
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
		lookupErr := svc.pool.QueryRow(ctx,
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

	rows, err := svc.pool.Query(ctx, query, args...)
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
	err := svc.pool.QueryRow(ctx,
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
	if !requireAdmin(c, "os-hypervisors:index") {
		return
	}
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
	if !requireAdmin(c, "os-hypervisors:detail") {
		return
	}
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
	if !requireAdmin(c, "os-hypervisors:statistics") {
		return
	}
	// Count running instances. Scan failures degrade to zero (the stats
	// response still ships) and are logged for diagnosis.
	var runningVMs int
	if err := svc.pool.QueryRow(c.Request.Context(),
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
	rows, err := svc.pool.Query(c.Request.Context(),
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
	rows, err := svc.pool.Query(c.Request.Context(),
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
	quotas, _, err := resolveQuotaLimits(ctx, svc.pool, projectID)
	if err != nil {
		log.Error().Err(err).Str("operation", "get_limits").Msg("failed to resolve quota limits")
		common.SendError(c, common.NewInternalServerError("failed to get limits"))
		return
	}

	// Query current usage from database.
	//
	// This counted `AND i.status != 'DELETED'` until SPEC-025. Nothing in the product
	// has ever written that status — every delete path is a hard DELETE FROM instances
	// — so the clause excluded nothing and was the only usage query carrying it. Left
	// in place it was a latent under-count: the moment anything began writing that
	// status, this view would silently stop counting those servers while the quota
	// views kept counting them (FR-040, FR-041).
	var instancesUsed, coresUsed, ramUsed int
	if err := svc.pool.QueryRow(ctx,
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
		if err := svc.pool.QueryRow(ctx, q.sql, projectID).Scan(q.dst); err != nil {
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
	if !requireAdmin(c, "os-services:index") {
		return
	}
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
	// Owner-or-administrator, like every other server operation (SPEC-023
	// FR-004). The follow-up queries use the resolved id.
	serverID, _, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}

	// Fetch metadata
	rows, err := svc.pool.Query(c.Request.Context(),
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
	var req struct {
		Metadata map[string]string `json:"metadata" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Owner-or-administrator (SPEC-023 FR-004); the writes below use the
	// resolved id.
	serverID, _, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}

	// Upsert each metadata key-value pair
	for key, value := range req.Metadata {
		_, err := svc.pool.Exec(c.Request.Context(),
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
	rows, err := svc.pool.Query(c.Request.Context(),
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
	var req struct {
		Metadata map[string]string `json:"metadata" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Owner-or-administrator (SPEC-023 FR-004); the replace below uses the
	// resolved id.
	serverID, _, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}

	// Delete all existing metadata then insert new metadata atomically
	err := database.WithTx(c.Request.Context(), svc.pool, func(tx pgx.Tx) error {
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
func (svc *Service) RebuildInstanceAction(c *gin.Context, rebuildData any, instanceID, projectID string) {
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

	// The new image must be one the caller can read, and active, as for a create
	// (SPEC-020 FR-026).
	imageStatus, found, err := common.VisibleImage(c.Request.Context(), svc.pool, imageRef, projectID, common.HasRole(c, common.RoleAdmin))
	if err != nil {
		log.Error().Err(err).Str("operation", "rebuild_check_image").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to rebuild instance"))
		return
	}
	if !found {
		common.SendError(c, common.NewBadRequestError("Cannot find image for rebuild"))
		return
	}
	if imageStatus != "active" {
		common.SendError(c, common.NewBadRequestError(fmt.Sprintf("Image %s is not active.", imageRef)))
		return
	}

	// Update instance in database
	now := time.Now()
	_, err = svc.pool.Exec(c.Request.Context(),
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
			if _, err := svc.pool.Exec(ctx,
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
	err = svc.pool.QueryRow(c.Request.Context(),
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
func (svc *Service) RescueInstanceAction(c *gin.Context, rescueData any, instanceID, projectID string) {
	// Update instance status to RESCUE
	now := time.Now()
	_, err := svc.pool.Exec(c.Request.Context(),
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
	_, err := svc.pool.Exec(c.Request.Context(), `
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
		if _, err := svc.pool.Exec(c.Request.Context(), `
			INSERT INTO image_properties (image_id, name, value)
			VALUES ($1, $2, $3)
		`, imageID, key, value); err != nil {
			log.Warn().Err(err).Str("image_id", imageID).Str("key", key).Msg("failed to persist image property")
		}
	}

	// The image is identified by the Location header at every microversion, and
	// additionally in the BODY from 2.45 (SPEC-025-followup FR-039).
	//
	// Upstream introduced the response body for BOTH the createImage and createBackup
	// actions at 2.45. This handler returned no body at any version — upstream's pre-2.45
	// shape, served unconditionally — which is the mirror image of the defect
	// CreateBackupAction had: that one returned the body unconditionally, under a comment
	// claiming it was gated. Found by the real-mode host pass, whose R8b tested this action
	// while the gate had only been applied to the other one.
	imageLocation := fmt.Sprintf("%s/v2/images/%s", common.BaseURL(c, 9292), imageID)
	c.Header("Location", imageLocation)
	if !wantsCreateImageIDInBody(c) {
		c.Status(http.StatusAccepted)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"image_id": imageID})
}

// PauseInstanceAction handles the pause action
func (svc *Service) PauseInstanceAction(c *gin.Context, instanceID, projectID string) {
	// Update instance status to PAUSED
	_, err := svc.pool.Exec(c.Request.Context(),
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
func (svc *Service) UnpauseInstanceAction(c *gin.Context, instanceID, projectID string) {
	// Update instance status to ACTIVE
	_, err := svc.pool.Exec(c.Request.Context(),
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
func (svc *Service) LockInstanceAction(c *gin.Context, instanceID, projectID string) {
	// Update instance locked status
	_, err := svc.pool.Exec(c.Request.Context(),
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
func (svc *Service) UnlockInstanceAction(c *gin.Context, instanceID, projectID string) {
	// Update instance locked status
	_, err := svc.pool.Exec(c.Request.Context(),
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
func (svc *Service) ForceDeleteInstanceAction(c *gin.Context, instanceID, projectID string) {
	// Read in both modes: a found row is what proves this project owns the
	// instance, and the console revoke below is not project-scoped.
	var libvirtDomainID any
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT libvirt_domain_id FROM instances WHERE id = $1 AND project_id = $2",
		instanceID, projectID,
	).Scan(&libvirtDomainID)

	// Revoke console tokens before the row goes. The FK cascade removes their rows
	// but not their projection files, and a file the proxy can still read keeps
	// resolving, with no row left to revoke it by, until the next orphan sweep
	// (SPEC-021 FR-014).
	if err == nil && !svc.revokeConsoleTokens(c, instanceID, "instance force-deleted") {
		return
	}

	// In stub mode, just delete from database
	if svc.libvirtMode == "stub" || svc.vmManager == nil {
		_, err := svc.pool.Exec(c.Request.Context(),
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

	_, err = svc.pool.Exec(c.Request.Context(),
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
