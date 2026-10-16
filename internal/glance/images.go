package glance

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// Service handles Glance API endpoints
type Service struct {
	pool        *pgxpool.Pool
	mode        string
	storageMode string
	cephPool    string
	cephConf    string
	s3Bucket    string
	s3Region    string
	s3Endpoint  string
	imageStore  *storage.ImageStore
	cache       *cache.Cache
}

// NewService creates a new Glance service.
//
// Returns an error if the configured storage backend fails to initialize
// in a way that cannot be served by a hybrid fallback (e.g. mode == "s3"
// with no AWS credentials). See storage.NewImageStore for the per-mode
// failure policy.
func NewService(pool *pgxpool.Pool, mode, cephPool, cephConf string, s3opts storage.S3Options, cacheInstance *cache.Cache) (*Service, error) {
	imageStore, err := storage.NewImageStore(mode, cephPool, cephConf, s3opts)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Glance image store: %w", err)
	}
	return &Service{
		pool:        pool,
		mode:        mode,
		storageMode: mode, // storageMode same as mode for backward compatibility
		cephPool:    cephPool,
		cephConf:    cephConf,
		s3Bucket:    s3opts.Bucket,
		s3Region:    s3opts.Region,
		s3Endpoint:  s3opts.Endpoint,
		imageStore:  imageStore,
		cache:       cacheInstance,
	}, nil
}

// ImageStore returns the underlying image store so other services (e.g. Nova's
// lazy fetch-on-boot) can share the same backend client (SPEC-014).
func (svc *Service) ImageStore() *storage.ImageStore {
	return svc.imageStore
}

// RegisterRoutes registers Glance routes (excluding version discovery which is handled separately)
func (svc *Service) RegisterRoutes(r *gin.RouterGroup) {
	// Note: Version discovery (GET / and GET /v2) are registered separately
	// in main.go without auth middleware to comply with OpenStack spec
	//
	// gophercloud's NewImageServiceV2() ALWAYS appends /v2 to the catalog endpoint,
	// so we register routes WITHOUT /v2 prefix to avoid /v2/v2 doubling

	// Images
	r.GET("/images", svc.ListImages)
	r.POST("/images", svc.CreateImage)
	r.GET("/images/:id", svc.GetImage)
	r.DELETE("/images/:id", svc.DeleteImage)
	r.PATCH("/images/:id", svc.UpdateImage)

	// Image data
	r.PUT("/images/:id/file", svc.UploadImageData)
	r.GET("/images/:id/file", svc.DownloadImageData)

	// Image import
	r.POST("/images/:id/stage", svc.StageImageData)
	r.POST("/images/:id/import", svc.ImportImage)
	r.GET("/images/:id/import", svc.GetImageImportInfo)

	// Image members (sharing)
	r.POST("/images/:id/members", svc.CreateImageMember)
	r.GET("/images/:id/members", svc.ListImageMembers)
	r.GET("/images/:id/members/:member_id", svc.GetImageMember)
	r.PUT("/images/:id/members/:member_id", svc.UpdateImageMember)
	r.DELETE("/images/:id/members/:member_id", svc.DeleteImageMember)

	// Image tags
	r.PUT("/images/:id/tags/:tag", svc.AddImageTag)
	r.DELETE("/images/:id/tags/:tag", svc.DeleteImageTag)

	// Image actions
	r.POST("/images/:id/actions/deactivate", svc.DeactivateImage)
	r.POST("/images/:id/actions/reactivate", svc.ReactivateImage)

	// Schemas
	r.GET("/schemas/image", svc.GetImageSchema)
	r.GET("/schemas/images", svc.GetImagesSchema)
	r.GET("/schemas/member", svc.GetMemberSchema)
	r.GET("/schemas/members", svc.GetMembersSchema)

	// Tasks and import
	r.POST("/tasks", svc.CreateTask)
	r.GET("/tasks", svc.ListTasks)
	r.GET("/tasks/:id", svc.GetTask)

	// Stores
	r.GET("/stores", svc.ListStores)
	r.GET("/stores/info", svc.GetStoresInfo)

	// Metadefs
	r.GET("/metadefs/namespaces", svc.ListMetadefNamespaces)
	r.POST("/metadefs/namespaces", svc.CreateMetadefNamespace)
	r.GET("/metadefs/namespaces/:namespace", svc.GetMetadefNamespace)
	r.PUT("/metadefs/namespaces/:namespace", svc.UpdateMetadefNamespace)
	r.DELETE("/metadefs/namespaces/:namespace", svc.DeleteMetadefNamespace)
	r.GET("/metadefs/resource_types", svc.ListMetadefResourceTypes)

	// Cache management
	r.GET("/cache/images", svc.ListCachedImages)
	r.DELETE("/cache/images", svc.ClearCache)
	r.PUT("/cache/images/:id", svc.PrefetchImage)
	r.DELETE("/cache/images/:id", svc.DeleteCachedImage)
}

// CreateImageRequest represents an image creation request
type CreateImageRequest struct {
	Name            string   `json:"name"`
	DiskFormat      string   `json:"disk_format"`
	ContainerFormat string   `json:"container_format"`
	Visibility      string   `json:"visibility"`
	MinDisk         int      `json:"min_disk"`
	MinRAM          int      `json:"min_ram"`
	Protected       bool     `json:"protected"`
	Tags            []string `json:"tags"`
}

// GetVersions returns all available API versions
func (svc *Service) GetVersions(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"versions": []gin.H{
			{
				"id":     "v2.9",
				"status": "CURRENT",
				"links": []gin.H{
					{
						"rel": "self",
						// Must include the /v2/ path: clients do version discovery
						// against the unversioned catalog endpoint and use this href
						// as the base, so omitting it sends requests to /images (404).
						"href": fmt.Sprintf("%s/v2/", common.BaseURL(c, 9292)),
					},
				},
			},
		},
	})
}

// GetVersionV2 returns v2 API version details
func (svc *Service) GetVersionV2(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version": gin.H{
			"id":     "v2.9",
			"status": "CURRENT",
			"links": []gin.H{
				{
					"rel":  "self",
					"href": fmt.Sprintf("%s/v2/", common.BaseURL(c, 9292)),
				},
			},
		},
	})
}

// CreateImage creates a new image
func (svc *Service) CreateImage(c *gin.Context) {
	var req CreateImageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	projectID := c.GetString("project_id")
	imageID := uuid.New().String()

	// Set defaults
	visibility := "private"
	if req.Visibility != "" {
		// Same vocabulary as the patch path, through the same predicate. This path had no
		// domain check at all, so `{"visibility":"nonsense"}` was answered 201 and stored a
		// value nothing honours.
		if !validImageVisibility(req.Visibility) {
			common.SendError(c, common.NewInvalidValueError("visibility", req.Visibility,
				strings.Join(imageVisibilities, " or ")))
			return
		}
		visibility = req.Visibility
	}

	diskFormat := "qcow2"
	if req.DiskFormat != "" {
		diskFormat = req.DiskFormat
	}

	containerFormat := "bare"
	if req.ContainerFormat != "" {
		containerFormat = req.ContainerFormat
	}

	// Tag length is validated BEFORE the image row is written — upstream's
	// schema caps a tag at 255 characters and answers 400, while the column's
	// VARCHAR(255) would turn an over-long tag into a 500 after the image was
	// already committed.
	for _, tag := range req.Tags {
		if len(tag) > 255 {
			common.SendError(c, common.NewBadRequestError("image tag too long (max 255 characters)"))
			return
		}
	}

	// Insert into database. Record the owning project for images of EVERY visibility,
	// not just private ones — visibility governs who can see an image, ownership governs
	// who can delete it (F6, FR-014). Storing public images ownerless (project_id NULL)
	// was the root cause of the delete no-op.
	now := time.Now()
	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO images (id, name, project_id, status, visibility, disk_format, container_format, min_disk_gb, min_ram_mb, rbd_pool, rbd_image, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, imageID, req.Name, sql.NullString{String: projectID, Valid: projectID != ""}, "queued", visibility, diskFormat, containerFormat, req.MinDisk, req.MinRAM, svc.cephPool, "image-"+imageID, now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_image").Msg("failed to insert image into database")
		common.SendError(c, common.NewInternalServerError("failed to create image"))
		return
	}

	// Persist the requested tags. The 201 response below echoes req.Tags, but
	// the rows were never written before, so a follow-up show returned `tags: []`
	// — `openstack image create --tag` silently dropped them (SPEC-028). The
	// dedicated PUT uses the same ON CONFLICT DO NOTHING, so a request repeating
	// a tag stays legal. Length was validated above, before the image row.
	for _, tag := range req.Tags {
		if _, err := svc.pool.Exec(c.Request.Context(), `
			INSERT INTO image_tags (image_id, tag)
			VALUES ($1, $2)
			ON CONFLICT (image_id, tag) DO NOTHING
		`, imageID, tag); err != nil {
			log.Error().Err(err).Str("operation", "create_image").Str("image_id", imageID).Msg("failed to insert image tag")
			common.SendError(c, common.NewInternalServerError("failed to create image"))
			return
		}
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":               imageID,
		"name":             req.Name,
		"status":           "queued",
		"visibility":       visibility,
		"disk_format":      diskFormat,
		"container_format": containerFormat,
		"min_disk":         req.MinDisk,
		"min_ram":          req.MinRAM,
		"protected":        req.Protected,
		"tags":             req.Tags,
		"created_at":       now.Format(time.RFC3339),
		"updated_at":       now.Format(time.RFC3339),
		"self":             fmt.Sprintf("/v2/images/%s", imageID),
		"file":             fmt.Sprintf("/v2/images/%s/file", imageID),
		"schema":           "/v2/schemas/image",
	})
}

// ListImages lists all images
func (svc *Service) ListImages(c *gin.Context) {
	projectID := c.GetString("project_id")

	// The filters upstream's own visibility rules take (glance/api/v2/images.py).
	visibility := c.Query("visibility")
	switch visibility {
	case "", "public", "private", "shared", "community", "all":
	default:
		common.SendError(c, common.NewBadRequestError("Invalid visibility value: "+visibility))
		return
	}
	memberStatus := c.DefaultQuery("member_status", "accepted")
	switch memberStatus {
	case "pending", "accepted", "rejected", "all":
	default:
		common.SendError(c, common.NewBadRequestError("Invalid status: "+memberStatus))
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

	// Which images the list holds follows upstream (glance/db/sqlalchemy/api.py
	// _select_images_query and _image_get_all, SPEC-020 FR-026): an administrator's list
	// starts from every image; anyone else's from public and community images, their own,
	// and shared images whose membership has member_status (accepted unless asked).
	// ?visibility= keeps one visibility, or "all" keeps everything; without it, community
	// images are left out unless they are the caller's own. Narrower than reading one image
	// (common.ImageVisibleSQL): a pending share can be shown but is not listed by default.
	//
	// The caller and the membership status are each read by several placeholders, so each is
	// appended once and its placeholder reused.
	f := &common.SQLFilter{}
	caller := f.Arg(common.CallerProjectID(projectID))
	admin := f.Arg(common.HasRole(c, common.RoleAdmin))
	membership := f.Arg(memberStatus)
	f.Cond("(" + admin + `
		OR i.visibility IN ('public', 'community')
		OR i.project_id::text = ` + caller + `
		OR (i.visibility = 'shared' AND ` + caller + ` <> '' AND EXISTS (
			SELECT 1 FROM image_members m
			WHERE m.image_id = i.id
			  AND replace(lower(m.member_id), '-', '') = replace(` + caller + `, '-', '')
			  AND (` + membership + ` = 'all' OR m.status = ` + membership + `))))`)
	switch visibility {
	case "":
		f.Cond("(i.visibility <> 'community' OR i.project_id::text = " + caller + ")")
	case "all":
	default:
		f.Eq("i.visibility", visibility)
	}
	if status := c.Query("status"); status != "" {
		f.Eq("i.status", status)
	}

	// The remaining standard Glance v2 list parameters (glance's ALLOWED_PARAMS):
	// name, disk_format, container_format, size_min, size_max, owner, tag. All are
	// exact matches upstream. They were silently ignored before, which broke the
	// terraform-provider-openstack image data source (it sends `name` server-side
	// and does no client-side matching) and `openstack image list --tag` (SPEC-028).
	if name := c.Query("name"); name != "" {
		f.Eq("i.name", name)
	}
	if diskFormat := c.Query("disk_format"); diskFormat != "" {
		f.Eq("i.disk_format", diskFormat)
	}
	if containerFormat := c.Query("container_format"); containerFormat != "" {
		f.Eq("i.container_format", containerFormat)
	}
	// size_min/size_max are byte bounds; a NULL size (a queued image) matches
	// neither, which is upstream's behaviour too.
	if sizeMin := c.Query("size_min"); sizeMin != "" {
		if n, err := strconv.ParseInt(sizeMin, 10, 64); err == nil && n >= 0 {
			f.Cond("i.size_bytes >= " + f.Arg(n))
		} else {
			common.SendError(c, common.NewBadRequestError("Invalid size_min value: "+sizeMin))
			return
		}
	}
	if sizeMax := c.Query("size_max"); sizeMax != "" {
		if n, err := strconv.ParseInt(sizeMax, 10, 64); err == nil && n >= 0 {
			f.Cond("i.size_bytes <= " + f.Arg(n))
		} else {
			common.SendError(c, common.NewBadRequestError("Invalid size_max value: "+sizeMax))
			return
		}
	}
	if owner := c.Query("owner"); owner != "" {
		// Dash-insensitive, like the membership comparison above — callers hold
		// the id in either spelling.
		f.Cond("replace(i.project_id::text, '-', '') = replace(" + f.Arg(owner) + ", '-', '')")
	}
	if tag := c.Query("tag"); tag != "" {
		f.Cond("EXISTS (SELECT 1 FROM image_tags t WHERE t.image_id = i.id AND t.tag = " + f.Arg(tag) + ")")
	}

	// Marker-based pagination over a TOTAL order.
	//
	// This used to order by created_at alone and resolve the marker to `created_at < $n`. created_at
	// is not unique — images uploaded by one script or seed share it — so the order was undefined
	// among ties AND the predicate excluded every row sharing the marker's timestamp rather than only
	// those before it, silently dropping the marker's siblings from the walk. id is the primary key,
	// so (created_at, id) is total and the composite comparison is exclusive of exactly one row.
	// Same defect and same fix as the compute listings in #549.
	//
	// The marker is resolved as a show resolves an image (common.ImageVisibleSQL), as upstream resolves
	// it with _image_get, and one that does not resolve is refused with upstream's 400. It used to be
	// read with no visibility at all and ignored when absent, so another project's private image
	// narrowed the listing where an unknown id did not — telling the caller that image exists.
	if marker := c.Query("marker"); marker != "" {
		var markerCreatedAt time.Time
		var markerID string
		err := svc.pool.QueryRow(c.Request.Context(),
			"SELECT i.created_at, i.id FROM images i WHERE i.id::text = $1 AND "+common.ImageVisibleSQL("i", 2, 3),
			marker, common.CallerProjectID(projectID), common.HasRole(c, common.RoleAdmin),
		).Scan(&markerCreatedAt, &markerID)
		if errors.Is(err, pgx.ErrNoRows) {
			common.SendError(c, common.NewBadRequestError(fmt.Sprintf("No image found with ID %s", marker)))
			return
		}
		if err != nil {
			log.Error().Err(err).Str("marker", marker).Msg("failed to resolve pagination marker")
			common.SendError(c, common.NewInternalServerError("failed to list images"))
			return
		}
		f.Cond(fmt.Sprintf("(i.created_at, i.id) < (%s, %s)", f.Arg(markerCreatedAt), f.Arg(markerID)))
	}

	where := f.Where()
	limitOffset := fmt.Sprintf(" LIMIT %s OFFSET %s", f.Arg(limit), f.Arg(offset))
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT i.id, i.name, i.status, i.visibility, i.size_bytes, i.disk_format, i.container_format, i.min_disk_gb, i.min_ram_mb, i.created_at, i.updated_at
		FROM images i`+where+`
		ORDER BY i.created_at DESC, i.id DESC`+limitOffset, f.Args()...)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_images").Msg("failed to query images")
		common.SendError(c, common.NewInternalServerError("failed to list images"))
		return
	}
	defer rows.Close()

	var images []gin.H
	for rows.Next() {
		var id, name, status, visibility string
		// Nullable, and null in upstream's schema until set. A plain string would make pgx
		// end the whole result set on one image without formats, failing every caller's list.
		var diskFormat, containerFormat *string
		var sizeBytes sql.NullInt64
		var minDisk, minRAM int
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&id, &name, &status, &visibility, &sizeBytes, &diskFormat, &containerFormat, &minDisk, &minRAM, &createdAt, &updatedAt); err != nil {
			log.Warn().Err(err).Str("operation", "ListImages").Msg("failed to scan row")
			continue
		}

		image := gin.H{
			"id":               id,
			"name":             name,
			"status":           status,
			"visibility":       visibility,
			"disk_format":      diskFormat,
			"container_format": containerFormat,
			"min_disk":         minDisk,
			"min_ram":          minRAM,
			"created_at":       createdAt.Format(time.RFC3339),
			"updated_at":       updatedAt.Format(time.RFC3339),
			"self":             fmt.Sprintf("/v2/images/%s", id),
			"file":             fmt.Sprintf("/v2/images/%s/file", id),
			"schema":           "/v2/schemas/image",
		}

		if sizeBytes.Valid {
			image["size"] = sizeBytes.Int64
		}

		images = append(images, image)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if images == nil {
		images = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{
		"images": images,
		"schema": "/v2/schemas/images",
		"first":  "/v2/images",
	})
}

// imageCacheKey names a cached image entry, scoped to the caller: its project, and whether
// it reads as an administrator.
//
// The caller is part of the key because GetImage reads the cache BEFORE applying its
// visibility predicate (common.ImageVisibleSQL). Keyed on the image alone, the first fetch
// by an owner populated an entry that any authenticated caller knowing the id could then
// read for the whole TTL, where the uncached answer is 404. An administrator reads every
// image, so its entries must not reach a member of the same project. Same shape as
// networkCacheKey in internal/neutron, raised on PR #79.
func imageCacheKey(projectID string, admin bool, imageID string) string {
	scope := "member"
	if admin {
		scope = "admin"
	}
	return "image:" + projectID + ":" + scope + ":" + imageID
}

// GetImage returns a single image
func (svc *Service) GetImage(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")
	admin := common.HasRole(c, common.RoleAdmin)
	ctx := c.Request.Context()

	// Try cache first. The key is scoped to the caller (see imageCacheKey): this lookup
	// happens before the visibility predicate below, so an unscoped key would serve one
	// tenant's private image to another.
	if svc.cache != nil {
		var cached gin.H
		if err := svc.cache.Get(ctx, imageCacheKey(projectID, admin, imageID), &cached); err == nil {
			c.JSON(http.StatusOK, cached)
			return
		}
	}

	// Cache miss - query database
	var id, name, status, visibility string
	var diskFormat, containerFormat *string // nullable, see ListImages
	var checksum sql.NullString
	var sizeBytes sql.NullInt64
	var minDisk, minRAM int
	var createdAt, updatedAt time.Time

	// By id or by name (a Lightstack extension), and only an image the caller may read, by
	// upstream's rule for reading one image (SPEC-020 FR-026).
	err := svc.pool.QueryRow(ctx, `
		SELECT i.id, i.name, i.status, i.visibility, i.size_bytes, i.disk_format, i.container_format, i.min_disk_gb, i.min_ram_mb, i.checksum, i.created_at, i.updated_at
		FROM images i
		WHERE (i.id::text = $1 OR i.name = $1) AND `+common.ImageVisibleSQL("i", 2, 3)+`
		LIMIT 1
	`, imageID, common.CallerProjectID(projectID), admin).Scan(&id, &name, &status, &visibility, &sizeBytes, &diskFormat, &containerFormat, &minDisk, &minRAM, &checksum, &createdAt, &updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_image").Str("image_id", imageID).Msg("failed to query image")
		common.SendError(c, common.NewInternalServerError("failed to get image"))
		return
	}

	image := gin.H{
		"id":               id,
		"name":             name,
		"status":           status,
		"visibility":       visibility,
		"disk_format":      diskFormat,
		"container_format": containerFormat,
		"min_disk":         minDisk,
		"min_ram":          minRAM,
		"created_at":       createdAt.Format(time.RFC3339),
		"updated_at":       updatedAt.Format(time.RFC3339),
		"self":             fmt.Sprintf("/v2/images/%s", id),
		"file":             fmt.Sprintf("/v2/images/%s/file", id),
		"schema":           "/v2/schemas/image",
	}

	if sizeBytes.Valid {
		image["size"] = sizeBytes.Int64
	}

	if checksum.Valid && checksum.String != "" {
		image["checksum"] = checksum.String
	}

	// Load tags
	rows, err := svc.pool.Query(ctx,
		"SELECT tag FROM image_tags WHERE image_id = $1 ORDER BY tag",
		id,
	)
	if err == nil {
		defer rows.Close()
		var tags []string
		for rows.Next() {
			var tag string
			if err := rows.Scan(&tag); err != nil {
				log.Warn().Err(err).Str("operation", "GetImage").Msg("failed to scan row")
			} else {
				tags = append(tags, tag)
			}
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Msg("rows iteration error")
			common.SendError(c, common.NewInternalServerError("failed to read results"))
			return
		}
		if tags == nil {
			tags = []string{}
		}
		image["tags"] = tags
	}

	// Store in cache (1h TTL per config)
	if svc.cache != nil {
		svc.cache.TrySet(ctx, imageCacheKey(projectID, admin, id), image, 1*time.Hour)
	}

	c.JSON(http.StatusOK, image)
}

// imageDeleteAuthz applies upstream Glance's delete authorization (F6): the image's
// owner, or an admin, may delete it. Returns 0 to proceed; http.StatusForbidden when
// the caller can see the image (it is public) but does not own it; http.StatusNotFound
// to hide an image the caller cannot see (private and not owned). Visibility controls
// who can see the image, ownership controls who can delete it.
func imageDeleteAuthz(isAdmin bool, owner sql.NullString, caller, visibility string) int {
	if isAdmin || (owner.Valid && owner.String == caller) {
		return 0
	}
	if visibility == "public" {
		return http.StatusForbidden
	}
	return http.StatusNotFound
}

// imageInUse reports whether any instance boots from the image (instances.image_id)
// or any volume was cloned from it (volumes.source_image_id) — either pins the image
// against deletion (OpenStack-standard 409). Independent local copies do not set
// source_image_id, so they do not pin.
func (svc *Service) imageInUse(ctx context.Context, imageID string) (bool, error) {
	var inUse bool
	err := svc.pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM instances WHERE image_id = $1
			UNION ALL
			SELECT 1 FROM volumes WHERE source_image_id = $1)`,
		imageID,
	).Scan(&inUse)
	return inUse, err
}

// DeleteImage removes an image, authorized owner-or-admin per upstream Glance (F6):
// 404 when it doesn't exist or the caller cannot see it, 403 when visible but not
// owned, 409 when in use, 204 on success.
func (svc *Service) DeleteImage(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")
	ctx := c.Request.Context()

	// Look up the image's owner and visibility; a missing row is 404. Authorization
	// then follows upstream Glance — owner-or-admin may delete (F6, FR-014a).
	var owner sql.NullString
	var visibility string
	err := svc.pool.QueryRow(ctx,
		"SELECT project_id, visibility FROM images WHERE id = $1",
		imageID,
	).Scan(&owner, &visibility)
	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_image_lookup").Str("image_id", imageID).Msg("failed to look up image")
		common.SendError(c, common.NewInternalServerError("failed to delete image"))
		return
	}

	switch imageDeleteAuthz(common.HasRole(c, common.RoleAdmin), owner, projectID, visibility) {
	case http.StatusNotFound:
		common.SendError(c, common.NewNotFoundError("image"))
		return
	case http.StatusForbidden:
		common.SendError(c, common.NewForbiddenError("not authorized to delete this image"))
		return
	}

	inUse, err := svc.imageInUse(ctx, imageID)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_image_inuse_check").Str("image_id", imageID).Msg("failed to check image usage")
		common.SendError(c, common.NewInternalServerError("failed to delete image"))
		return
	}
	if inUse {
		common.SendError(c, common.NewConflictError("image is in use by one or more instances or volumes"))
		return
	}

	// Delete from the database by id (authorization verified above) and confirm a row
	// was removed, so a no-op delete surfaces as 404 rather than a false success
	// (F6, FR-015). The former `AND project_id = $2` never matched a public image —
	// whose project_id is NULL — which was the root cause of the silent no-op.
	tag, err := svc.pool.Exec(ctx, "DELETE FROM images WHERE id = $1", imageID)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_image_db").Str("image_id", imageID).Msg("failed to delete image from database")
		common.SendError(c, common.NewInternalServerError("failed to delete image"))
		return
	}
	if tag.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}

	// Delete from storage (best effort — the metadata row is already gone, so the API
	// result stays 204). Log a failure: nothing will retry it, so the backend object is
	// orphaned and this line is the only record of which image it belonged to.
	if err := svc.imageStore.DeleteImage(ctx, imageID); err != nil {
		log.Warn().Err(err).Str("image_id", imageID).Msg("image row deleted but backend object delete failed; object may be orphaned")
	}

	// Invalidate cache. Entries are keyed per caller (imageCacheKey), so evicting this image
	// means evicting every project's view of it, not just the deleter's.
	if svc.cache != nil {
		svc.cache.TryDeletePattern(ctx, "image:*:"+imageID)
		svc.cache.TryDeletePattern(ctx, "images:*")
	}

	c.Status(http.StatusNoContent)
}

// imageFieldKind is the column's type, so a value of the wrong shape is refused as a
// client error rather than handed to PostgreSQL to reject as a fault (FR-012).
type imageFieldKind int

const (
	imageFieldString imageFieldKind = iota
	imageFieldInt
)

type imageUpdateField struct {
	column string
	kind   imageFieldKind
	// allowed, when non-empty, is the closed set of values this property may take.
	// Type-checking alone let any string through; see imageVisibilities.
	allowed []string
}

// imageVisibilities is the visibility vocabulary this product HONOURS: upstream's whole
// set. Every value is honoured on read (common.ImageVisibleSQL and ListImages, SPEC-020
// FR-026) — a `shared` image is read by the projects it is shared with, through
// image_members, and a `community` image by anyone.
//
// The value used to be type-checked and not domain-checked, so any string was stored and
// answered 200. An unrecognised visibility is none of these four, so the image then read
// as owner-only, which fails closed but is still a success the product did not carry out —
// the silent success this feature exists to remove, and the same reason the server-list
// filters refuse `host` rather than ignoring it. A closed set refuses it instead and names
// the accepted values.
var imageVisibilities = []string{"public", "private", "shared", "community"}

// validImageVisibility is shared by the create and patch paths so the two cannot accept
// different vocabularies — they did, and only the patch path was reviewed.
func validImageVisibility(v string) bool {
	return slices.Contains(imageVisibilities, v)
}

// imageUpdateFields is the set of paths a patch may write. Unchanged by
// SPEC-025-followup: this feature makes the handler honest about what it will NOT write,
// it does not widen what can be written.
//
// All four are Glance BASE properties carrying a schema-defined default, which is why
// `remove` is refused for every one of them (FR-011) — there is no absent state to restore
// them to, and resetting one to its default would make `remove` a synonym for a write the
// client never asked for.
var imageUpdateFields = map[string]imageUpdateField{
	"/name":       {"name", imageFieldString, nil},
	"/visibility": {"visibility", imageFieldString, imageVisibilities},
	"/min_disk":   {"min_disk_gb", imageFieldInt, nil},
	"/min_ram":    {"min_ram_mb", imageFieldInt, nil},
}

func allowedImageUpdateField(path string) (string, bool) {
	f, ok := imageUpdateFields[path]
	return f.column, ok
}

// UpdateImage applies a JSON-Patch document to an image (SPEC-025-followup US2).
//
// The media type is `application/openstack-images-v2.1-json-patch`, which upstream defines
// as conforming to RFC 6902 — so `add`, `remove` and `replace` are all part of the
// contract. This handler previously applied only `replace`, `continue`d past every other
// operation and every non-allowlisted path, then fell through to GetImage and answered
// **200 with the unchanged image**. `openstack image set --min-disk N` sends `add`, so the
// documented way to set a property reported success and did nothing, silently in both
// directions: the only log line fired when a `replace` UPDATE errored, so a discarded
// operation left no trace at all.
//
// Three properties the shape below exists to guarantee:
//
//   - ATOMIC (FR-014). The whole document becomes ONE update statement, so it cannot
//     half-apply. The previous form ran an Exec per operation and logged-and-continued on
//     failure, which is how a partial write was reachable.
//   - NO SILENT SUCCESS (FR-012, FR-013). Every operation and path the handler cannot
//     honour is refused, naming what was refused. Nothing is discarded.
//   - NO ZERO-ROW SUCCESS (FR-015). RowsAffected is checked. The trailing GetImage already
//     answered 404 for a missing or other-project image, so this was the right answer for
//     the wrong reason — the UPDATE matched nothing and nobody looked.
func (svc *Service) UpdateImage(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	var updates []map[string]any
	if err := c.ShouldBindJSON(&updates); err != nil {
		common.SendError(c, common.NewBadRequestError("request body must be a JSON-Patch document (an array of operations)"))
		return
	}

	order, assignments, perr := planImagePatch(updates)
	if perr != nil {
		common.SendError(c, perr)
		return
	}

	if len(order) == 0 {
		// An empty document changes nothing and is not an error; the client still gets the
		// image, which is what upstream returns. Nothing was written, so nothing is evicted.
		svc.GetImage(c)
		return
	}

	matched, err := applyImagePatch(c.Request.Context(), svc.pool, imageID, projectID, order, assignments)
	if err != nil {
		log.Error().Err(err).Str("operation", "update_image").Str("image_id", imageID).Msg("failed to apply image patch")
		common.SendError(c, common.NewInternalServerError("failed to update image"))
		return
	}
	if !matched {
		// The image does not exist, or belongs to another project. Answered here rather
		// than left to the read below, so the reason is the write that matched nothing.
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}

	// Evict before re-reading. GetImage below tries the cache first, so without this the
	// response is the pre-patch body and every caller's view stays stale for the whole TTL.
	// Entries are keyed per caller (imageCacheKey), so evicting this image means evicting
	// every project's view of it, not just the patcher's — same as DeleteImage.
	if svc.cache != nil {
		svc.cache.TryDeletePattern(c.Request.Context(), "image:*:"+imageID)
		svc.cache.TryDeletePattern(c.Request.Context(), "images:*")
	}

	// The response reports the applied state (FR-016).
	svc.GetImage(c)
}

// planPatchOperation validates ONE operation and reports the field it writes and the
// coerced value.
//
// It RETURNS the refusal rather than sending it. Keeping *gin.Context out is the point:
// all five refusals below are then reachable from a unit test, where previously the only
// way to exercise any of them was an HTTP request — which is how a value-coercion defect
// survived nine passing tests that bound their JSON with a decoder production does not use.
func planPatchOperation(update map[string]any) (imageUpdateField, any, *common.OpenStackError) {
	var none imageUpdateField

	op, ok := update["op"].(string)
	if !ok {
		return none, nil, common.NewBadRequestError("each patch operation must carry a string \"op\"")
	}
	path, ok := update["path"].(string)
	if !ok {
		return none, nil, common.NewBadRequestError("each patch operation must carry a string \"path\"")
	}

	// `remove` is refused for every writable path (FR-011): all four are base properties
	// with schema defaults, so none has an absent state. Upstream answers 403 for a base
	// property, which is also what it answers for the unwritable paths below — the two
	// refusals are the same kind of thing.
	if op == "remove" {
		return none, nil, common.NewForbiddenError(fmt.Sprintf(
			"Cannot remove %s: it is a base image property with a schema default and cannot be absent. Set a value with replace instead.", path))
	}
	if op != "add" && op != "replace" {
		return none, nil, common.NewBadRequestError(fmt.Sprintf(
			"Unsupported patch operation %q; this service implements add and replace", op))
	}

	// `add` and `replace` are the SAME write for these four scalar properties: each is one
	// column on one row, so setting a property that has no value and replacing one that
	// does are indistinguishable at the storage layer (FR-010).
	field, ok := imageUpdateFields[path]
	if !ok {
		return none, nil, common.NewForbiddenError(fmt.Sprintf(
			"Cannot modify %s: it is not a client-writable image property", path))
	}

	value, err := coerceImagePatchValue(field, path, update["value"])
	if err != nil {
		return none, nil, err
	}
	return field, value, nil
}

// planImagePatch validates the ENTIRE document and reports the columns to assign, in
// first-seen order, with their values.
//
// Validating everything before returning is what makes the operation atomic: a refusal
// happens before any statement is built, so there is no partial state to roll back. The
// previous form ran an Exec per operation and logged-and-continued on failure, which is
// how a half-applied patch was reachable.
func planImagePatch(updates []map[string]any) ([]string, map[string]any, *common.OpenStackError) {
	assignments := make(map[string]any, len(updates))
	order := make([]string, 0, len(updates))

	for _, update := range updates {
		field, value, err := planPatchOperation(update)
		if err != nil {
			return nil, nil, err
		}
		if _, seen := assignments[field.column]; !seen {
			order = append(order, field.column)
		}
		// Last write wins for a repeated path. PostgreSQL rejects two assignments to one
		// column in a single SET, so the document is collapsed rather than passed through.
		assignments[field.column] = value
	}
	return order, assignments, nil
}

// applyImagePatch writes the plan as ONE statement and reports whether a row matched.
//
// The bool is not a convenience: a zero-row UPDATE means the image does not exist or
// belongs to another project, and the caller has to answer 404 for it (FR-015). The
// previous form checked nothing and let the trailing read produce the 404 — the right
// answer for the wrong reason, since the write had already matched nothing and nobody
// looked.
func applyImagePatch(ctx context.Context, pool *pgxpool.Pool, imageID, projectID string, order []string, assignments map[string]any) (bool, error) {
	var upd common.SQLUpdate
	for _, column := range order {
		upd.Set(column, assignments[column])
	}
	upd.Set("updated_at", time.Now())
	query := fmt.Sprintf("UPDATE images SET %s WHERE id::text = %s AND project_id = %s",
		upd.Assignments(), upd.Arg(imageID), upd.Arg(projectID))

	tag, err := pool.Exec(ctx, query, upd.Args()...)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// coerceImagePatchValue checks a patch value against its column's type and returns the
// value to write, or the client error to answer with.
//
// A NUMBER ARRIVES AS json.Number, NOT float64. `internal/middleware` sets
// `binding.EnableDecoderUseNumber = true` in a package `init()`, so every JSON number
// bound into an `any` reaches a handler as json.Number — a string type. Nova's handlers
// document this in three separate places; cinder's os-extend action branches on it
// explicitly.
//
// This function accepted only float64 on its first pass, so EVERY integer patch was
// refused with 400 "Invalid field value" — `openstack image set --min-disk 10` traded one
// silent failure for a loud wrong one. Found on the 2026-09-02 real-mode host run, and NOT
// by the unit tests: the flag is set by an `init()` in a package the glance test harness
// never imported, so the tests bound numbers as float64 and asserted a shape production
// never produces. The harness now imports it (see image_patch_test.go).
//
// float64 is still accepted: a caller that binds without the flag is not wrong, and a
// coercion that works either way cannot be broken by a change in link order.
func coerceImagePatchValue(field imageUpdateField, path string, raw any) (any, *common.OpenStackError) {
	switch field.kind {
	case imageFieldInt:
		n, ok := patchInt(raw)
		if !ok {
			return nil, common.NewInvalidValueError(path, fmt.Sprintf("%v", raw), "an integer")
		}
		return n, nil
	case imageFieldString:
		s, ok := raw.(string)
		if !ok {
			return nil, common.NewInvalidValueError(path, fmt.Sprintf("%v", raw), "a string")
		}
		// A closed set is checked HERE rather than left to PostgreSQL, which has no
		// constraint on these columns (migration 001 gives visibility a comment listing
		// the values and no CHECK), so an unhonoured value would simply be stored.
		if len(field.allowed) > 0 && !slices.Contains(field.allowed, s) {
			return nil, common.NewInvalidValueError(path, s, strings.Join(field.allowed, " or "))
		}
		return s, nil
	}
	return nil, common.NewBadRequestError(fmt.Sprintf("unhandled property %s", path))
}

// patchInt narrows a bound JSON value to an integer, accepting every shape the decoder can
// produce for one. A bool is deliberately NOT accepted: Go would not convert it anyway, and
// `{"value": true}` for min_disk is a client error rather than a 1.
func patchInt(raw any) (int64, bool) {
	switch v := raw.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case float64:
		if v != float64(int64(v)) {
			return 0, false
		}
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

// UploadImageData uploads image data
func (svc *Service) UploadImageData(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	// Check if image exists and is in queued status
	var status string
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT status FROM images WHERE id = $1 AND (visibility = 'public' OR project_id = $2)",
		imageID, projectID,
	).Scan(&status)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}

	if status != "queued" {
		common.SendError(c, common.NewConflictError("image data already exists"))
		return
	}

	// Update status to saving
	if _, err := svc.pool.Exec(c.Request.Context(),
		"UPDATE images SET status = $1, updated_at = $2 WHERE id = $3",
		"saving", time.Now(), imageID); err != nil {
		log.Warn().Err(err).Str("image_id", imageID).Msg("failed to mark image as saving")
	}

	// Upload to storage, hashing the uploaded bytes so the image's checksum
	// (MD5, Glance convention) is recorded — it lets a lazily fetched base be
	// integrity-verified on boot (SPEC-014 FR-002/FR-017).
	hasher := md5.New() // #nosec G401 -- Glance checksum is MD5 (compatibility), not a security control
	size, normalizedToRaw, err := svc.imageStore.UploadImage(c.Request.Context(), imageID, io.TeeReader(c.Request.Body, hasher))
	if err != nil {
		// Revert status to queued (best-effort; the upload error is the
		// user-facing one)
		if _, revertErr := svc.pool.Exec(c.Request.Context(),
			"UPDATE images SET status = $1 WHERE id = $2",
			"queued", imageID); revertErr != nil {
			log.Warn().Err(revertErr).Str("image_id", imageID).Msg("rollback: failed to revert image status to queued after upload failure")
		}
		log.Error().Err(err).Str("operation", "upload_image").Str("image_id", imageID).Msg("failed to upload image to storage")
		common.SendError(c, common.NewServiceUnavailableError("failed to upload image"))
		return
	}

	// Mark active and set size + checksum. Normalizing backends (local/RBD real
	// mode) store a raw base, so disk_format becomes "raw" (SPEC-009 WS-0/FR-003);
	// the S3 backend stores the bytes as-is, so its create-time disk_format is left
	// untouched rather than mislabeled "raw" (SPEC-014).
	checksum := hex.EncodeToString(hasher.Sum(nil))
	if _, err := svc.pool.Exec(c.Request.Context(),
		`UPDATE images SET status = $1, size_bytes = $2,
			disk_format = CASE WHEN $3 THEN 'raw' ELSE disk_format END,
			checksum = $4, updated_at = $5 WHERE id = $6`,
		"active", size, normalizedToRaw, checksum, time.Now(), imageID); err != nil {
		log.Error().Err(err).Str("image_id", imageID).Msg("failed to mark image as active after successful upload")
	}

	c.Status(http.StatusNoContent)
}

// DownloadImageData downloads image data
func (svc *Service) DownloadImageData(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	// Check if image exists and is active
	var status string
	var sizeBytes sql.NullInt64
	// Downloadable by whoever may read the image (SPEC-020 FR-026).
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT i.status, i.size_bytes FROM images i WHERE i.id::text = $1 AND "+common.ImageVisibleSQL("i", 2, 3),
		imageID, common.CallerProjectID(projectID), common.HasRole(c, common.RoleAdmin),
	).Scan(&status, &sizeBytes)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "download_image").Str("image_id", imageID).Msg("failed to query image")
		common.SendError(c, common.NewInternalServerError("failed to download image"))
		return
	}

	if status != "active" {
		common.SendError(c, common.NewConflictError("image is not active"))
		return
	}

	// Set headers
	c.Header("Content-Type", "application/octet-stream")
	if sizeBytes.Valid {
		c.Header("Content-Length", fmt.Sprintf("%d", sizeBytes.Int64))
	}

	// Download from storage
	writer := c.Writer
	if err := svc.imageStore.DownloadImage(c.Request.Context(), imageID, writer); err != nil {
		// Cannot send JSON error here as we've already started sending data
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
}

// GetImageSchema returns the image schema
func (svc *Service) GetImageSchema(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name": "image",
		"properties": gin.H{
			"id":               gin.H{"type": "string"},
			"name":             gin.H{"type": "string"},
			"status":           gin.H{"type": "string"},
			"visibility":       gin.H{"type": "string"},
			"size":             gin.H{"type": "integer"},
			"disk_format":      gin.H{"type": "string"},
			"container_format": gin.H{"type": "string"},
			"min_disk":         gin.H{"type": "integer"},
			"min_ram":          gin.H{"type": "integer"},
			"created_at":       gin.H{"type": "string"},
			"updated_at":       gin.H{"type": "string"},
		},
	})
}

// GetImagesSchema returns the images list schema
func (svc *Service) GetImagesSchema(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name": "images",
		"properties": gin.H{
			"images": gin.H{
				"type": "array",
				"items": gin.H{
					"type": "object",
					"name": "image",
				},
			},
		},
	})
}

// CreateImageMember creates a new image member (share image with another project)
func (svc *Service) CreateImageMember(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	var req struct {
		Member string `json:"member"` // member_id is the project ID to share with
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Check if image exists and is owned by requester
	var ownerID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id FROM images WHERE id = $1",
		imageID,
	).Scan(&ownerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "create_image_member_check").Str("image_id", imageID).Msg("failed to query image")
		common.SendError(c, common.NewInternalServerError("failed to create image member"))
		return
	}

	if !ownerID.Valid || ownerID.String != projectID {
		common.SendError(c, common.NewForbiddenError("not authorized to share this image"))
		return
	}

	// Insert image member
	memberID := uuid.New().String()
	now := time.Now()
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO image_members (id, image_id, member_id, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, memberID, imageID, req.Member, "pending", now, now)

	if err != nil {
		log.Error().Err(err).Str("operation", "create_image_member").Str("image_id", imageID).Msg("failed to insert image member")
		common.SendError(c, common.NewInternalServerError("failed to create image member"))
		return
	}
	svc.evictImage(c.Request.Context(), imageID)

	c.JSON(http.StatusOK, gin.H{
		"member_id":  req.Member,
		"image_id":   imageID,
		"status":     "pending",
		"created_at": now.Format(time.RFC3339),
		"updated_at": now.Format(time.RFC3339),
		"schema":     "/v2/schemas/member",
	})
}

// ListImageMembers lists all members for an image
func (svc *Service) ListImageMembers(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	// Check if image exists and requester has access
	var ownerID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id FROM images WHERE id = $1",
		imageID,
	).Scan(&ownerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "list_image_members_check").Str("image_id", imageID).Msg("failed to query image")
		common.SendError(c, common.NewInternalServerError("failed to list image members"))
		return
	}

	// Only owner can list members
	if !ownerID.Valid || ownerID.String != projectID {
		common.SendError(c, common.NewForbiddenError("not authorized to view image members"))
		return
	}

	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT member_id, status, created_at, updated_at
		FROM image_members
		WHERE image_id = $1
		ORDER BY created_at DESC
	`, imageID)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_image_members").Str("image_id", imageID).Msg("failed to query image members")
		common.SendError(c, common.NewInternalServerError("failed to list image members"))
		return
	}
	defer rows.Close()

	var members []gin.H
	for rows.Next() {
		var memberID, status string
		var createdAt, updatedAt time.Time

		if err := rows.Scan(&memberID, &status, &createdAt, &updatedAt); err != nil {
			log.Warn().Err(err).Msg("failed to scan image member row")
			continue
		}

		members = append(members, gin.H{
			"member_id":  memberID,
			"image_id":   imageID,
			"status":     status,
			"created_at": createdAt.Format(time.RFC3339),
			"updated_at": updatedAt.Format(time.RFC3339),
			"schema":     "/v2/schemas/member",
		})
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	if members == nil {
		members = []gin.H{}
	}

	c.JSON(http.StatusOK, gin.H{
		"members": members,
		"schema":  "/v2/schemas/members",
	})
}

// GetImageMember gets a specific image member
func (svc *Service) GetImageMember(c *gin.Context) {
	imageID := c.Param("id")
	memberID := c.Param("member_id")
	projectID := c.GetString("project_id")

	// Check if image exists and requester has access
	var ownerID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id FROM images WHERE id = $1",
		imageID,
	).Scan(&ownerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_image_member_check").Str("image_id", imageID).Msg("failed to query image")
		common.SendError(c, common.NewInternalServerError("failed to get image member"))
		return
	}

	// Only owner or member can view membership
	if (!ownerID.Valid || ownerID.String != projectID) && memberID != projectID {
		common.SendError(c, common.NewForbiddenError("not authorized"))
		return
	}

	var status string
	var createdAt, updatedAt time.Time
	err = svc.pool.QueryRow(c.Request.Context(), `
		SELECT status, created_at, updated_at
		FROM image_members
		WHERE image_id = $1 AND member_id = $2
	`, imageID, memberID).Scan(&status, &createdAt, &updatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("member"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "get_image_member").Str("image_id", imageID).Msg("failed to query image member")
		common.SendError(c, common.NewInternalServerError("failed to get image member"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"member_id":  memberID,
		"image_id":   imageID,
		"status":     status,
		"created_at": createdAt.Format(time.RFC3339),
		"updated_at": updatedAt.Format(time.RFC3339),
		"schema":     "/v2/schemas/member",
	})
}

// UpdateImageMember updates image member status
func (svc *Service) UpdateImageMember(c *gin.Context) {
	imageID := c.Param("id")
	memberID := c.Param("member_id")
	projectID := c.GetString("project_id")

	var req struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Validate status
	if req.Status != "accepted" && req.Status != "rejected" && req.Status != "pending" {
		common.SendError(c, common.NewBadRequestError("invalid status"))
		return
	}

	// Check if image exists
	var ownerID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id FROM images WHERE id = $1",
		imageID,
	).Scan(&ownerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "update_image_member_check").Str("image_id", imageID).Msg("failed to query image")
		common.SendError(c, common.NewInternalServerError("failed to update image member"))
		return
	}

	// Both owner and member can update status:
	// - Owner can set to "pending"
	// - Member can set to "accepted" or "rejected"
	isOwner := ownerID.Valid && ownerID.String == projectID
	isMember := memberID == projectID

	if !isOwner && !isMember {
		common.SendError(c, common.NewForbiddenError("not authorized"))
		return
	}

	// Owner can only set status to "pending"
	if isOwner && !isMember && req.Status != "pending" {
		common.SendError(c, common.NewForbiddenError("owner can only set status to pending"))
		return
	}

	// Update member status
	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE image_members
		SET status = $1, updated_at = $2
		WHERE image_id = $3 AND member_id = $4
	`, req.Status, time.Now(), imageID, memberID)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_image_member").Str("image_id", imageID).Msg("failed to update image member")
		common.SendError(c, common.NewInternalServerError("failed to update image member"))
		return
	}

	// Return updated member
	var status string
	var createdAt, updatedAt time.Time
	err = svc.pool.QueryRow(c.Request.Context(), `
		SELECT status, created_at, updated_at
		FROM image_members
		WHERE image_id = $1 AND member_id = $2
	`, imageID, memberID).Scan(&status, &createdAt, &updatedAt)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_image_member_fetch").Str("image_id", imageID).Msg("failed to fetch updated image member")
		common.SendError(c, common.NewInternalServerError("failed to fetch updated image member"))
		return
	}
	svc.evictImage(c.Request.Context(), imageID)

	c.JSON(http.StatusOK, gin.H{
		"member_id":  memberID,
		"image_id":   imageID,
		"status":     status,
		"created_at": createdAt.Format(time.RFC3339),
		"updated_at": updatedAt.Format(time.RFC3339),
		"schema":     "/v2/schemas/member",
	})
}

// DeleteImageMember deletes an image member (unshare)
func (svc *Service) DeleteImageMember(c *gin.Context) {
	imageID := c.Param("id")
	memberID := c.Param("member_id")
	projectID := c.GetString("project_id")

	// Check if image exists and requester is owner
	var ownerID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id FROM images WHERE id = $1",
		imageID,
	).Scan(&ownerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_image_member_check").Str("image_id", imageID).Msg("failed to query image owner")
		common.SendError(c, common.NewInternalServerError("failed to delete image member"))
		return
	}

	// Only owner can delete members
	if !ownerID.Valid || ownerID.String != projectID {
		common.SendError(c, common.NewForbiddenError("not authorized"))
		return
	}

	// Delete member
	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM image_members WHERE image_id = $1 AND member_id = $2",
		imageID, memberID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_image_member").Str("image_id", imageID).Msg("failed to delete image member")
		common.SendError(c, common.NewInternalServerError("failed to delete image member"))
		return
	}
	svc.evictImage(c.Request.Context(), imageID)

	c.Status(http.StatusNoContent)
}

// evictImage drops every caller's cached view of an image. A membership change decides who
// may read a shared image, so a cached read must not outlive it: a project the image is no
// longer shared with would keep reading it until the TTL expired.
func (svc *Service) evictImage(ctx context.Context, imageID string) {
	if svc.cache != nil {
		svc.cache.TryDeletePattern(ctx, "image:*:"+imageID)
	}
}

// AddImageTag adds a tag to an image
func (svc *Service) AddImageTag(c *gin.Context) {
	imageID := c.Param("id")
	tag := c.Param("tag")
	projectID := c.GetString("project_id")

	// Check image exists and user has permission
	var ownerID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id FROM images WHERE id = $1",
		imageID,
	).Scan(&ownerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "add_image_tag_check").Str("image_id", imageID).Msg("failed to query image owner")
		common.SendError(c, common.NewInternalServerError("failed to add image tag"))
		return
	}

	// Only owner can add tags
	if !ownerID.Valid || ownerID.String != projectID {
		common.SendError(c, common.NewForbiddenError("not authorized"))
		return
	}

	// Upstream's schema caps a tag at 255 characters and answers 400; the
	// column's VARCHAR(255) would instead surface as a 500.
	if len(tag) > 255 {
		common.SendError(c, common.NewBadRequestError("image tag too long (max 255 characters)"))
		return
	}

	// Add tag (ignore if already exists due to UNIQUE constraint)
	_, err = svc.pool.Exec(c.Request.Context(), `
		INSERT INTO image_tags (image_id, tag)
		VALUES ($1, $2)
		ON CONFLICT (image_id, tag) DO NOTHING
	`, imageID, tag)

	if err != nil {
		log.Error().Err(err).Str("operation", "add_image_tag").Str("image_id", imageID).Msg("failed to add image tag")
		common.SendError(c, common.NewInternalServerError("failed to add image tag"))
		return
	}

	c.Status(http.StatusNoContent)
}

// DeleteImageTag removes a tag from an image
func (svc *Service) DeleteImageTag(c *gin.Context) {
	imageID := c.Param("id")
	tag := c.Param("tag")
	projectID := c.GetString("project_id")

	// Check image exists and user has permission
	var ownerID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id FROM images WHERE id = $1",
		imageID,
	).Scan(&ownerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_image_tag_check").Str("image_id", imageID).Msg("failed to query image owner")
		common.SendError(c, common.NewInternalServerError("failed to delete image tag"))
		return
	}

	// Only owner can delete tags
	if !ownerID.Valid || ownerID.String != projectID {
		common.SendError(c, common.NewForbiddenError("not authorized"))
		return
	}

	// Delete tag
	_, err = svc.pool.Exec(c.Request.Context(),
		"DELETE FROM image_tags WHERE image_id = $1 AND tag = $2",
		imageID, tag,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_image_tag").Str("image_id", imageID).Msg("failed to delete image tag")
		common.SendError(c, common.NewInternalServerError("failed to delete image tag"))
		return
	}

	c.Status(http.StatusNoContent)
}

// DeactivateImage deactivates an image
func (svc *Service) DeactivateImage(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	// Check image exists and user has permission
	var ownerID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id FROM images WHERE id = $1",
		imageID,
	).Scan(&ownerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "deactivate_image_check").Str("image_id", imageID).Msg("failed to query image owner")
		common.SendError(c, common.NewInternalServerError("failed to deactivate image"))
		return
	}

	// Only owner can deactivate
	if !ownerID.Valid || ownerID.String != projectID {
		common.SendError(c, common.NewForbiddenError("not authorized"))
		return
	}

	// Update status to deactivated
	_, err = svc.pool.Exec(c.Request.Context(),
		"UPDATE images SET status = $1, updated_at = $2 WHERE id = $3",
		"deactivated", time.Now(), imageID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "deactivate_image").Str("image_id", imageID).Msg("failed to deactivate image")
		common.SendError(c, common.NewInternalServerError("failed to deactivate image"))
		return
	}

	c.Status(http.StatusNoContent)
}

// ReactivateImage reactivates a deactivated image
func (svc *Service) ReactivateImage(c *gin.Context) {
	imageID := c.Param("id")
	projectID := c.GetString("project_id")

	// Check image exists and user has permission
	var ownerID sql.NullString
	err := svc.pool.QueryRow(c.Request.Context(),
		"SELECT project_id FROM images WHERE id = $1",
		imageID,
	).Scan(&ownerID)

	if errors.Is(err, pgx.ErrNoRows) {
		common.SendError(c, common.NewNotFoundError("image"))
		return
	}
	if err != nil {
		log.Error().Err(err).Str("operation", "reactivate_image_check").Str("image_id", imageID).Msg("failed to query image owner")
		common.SendError(c, common.NewInternalServerError("failed to reactivate image"))
		return
	}

	// Only owner can reactivate
	if !ownerID.Valid || ownerID.String != projectID {
		common.SendError(c, common.NewForbiddenError("not authorized"))
		return
	}

	// Update status to active
	_, err = svc.pool.Exec(c.Request.Context(),
		"UPDATE images SET status = $1, updated_at = $2 WHERE id = $3",
		"active", time.Now(), imageID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "reactivate_image").Str("image_id", imageID).Msg("failed to reactivate image")
		common.SendError(c, common.NewInternalServerError("failed to reactivate image"))
		return
	}

	c.Status(http.StatusNoContent)
}

// GetMemberSchema returns the member schema
func (svc *Service) GetMemberSchema(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name": "member",
		"properties": gin.H{
			"created_at": gin.H{
				"type":        "string",
				"description": "Date and time of member creation",
				"readOnly":    true,
			},
			"image_id": gin.H{
				"type":        "string",
				"description": "An identifier for the image",
				"pattern":     "^([0-9a-fA-F]){8}-([0-9a-fA-F]){4}-([0-9a-fA-F]){4}-([0-9a-fA-F]){4}-([0-9a-fA-F]){12}$",
			},
			"member_id": gin.H{
				"type":        "string",
				"description": "An identifier for the image member (tenant ID)",
			},
			"status": gin.H{
				"type":        "string",
				"description": "The status of this image member",
				"enum":        []string{"pending", "accepted", "rejected"},
			},
			"updated_at": gin.H{
				"type":        "string",
				"description": "Date and time of last member update",
				"readOnly":    true,
			},
			"schema": gin.H{
				"type":        "string",
				"description": "The URL for the schema describing this member",
				"readOnly":    true,
			},
		},
	})
}

// GetMembersSchema returns the members list schema
func (svc *Service) GetMembersSchema(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name": "members",
		"properties": gin.H{
			"members": gin.H{
				"type": "array",
				"items": gin.H{
					"type": "object",
					"name": "member",
					"properties": gin.H{
						"created_at": gin.H{
							"type":        "string",
							"description": "Date and time of member creation",
						},
						"image_id": gin.H{
							"type":        "string",
							"description": "An identifier for the image",
						},
						"member_id": gin.H{
							"type":        "string",
							"description": "An identifier for the image member",
						},
						"status": gin.H{
							"type":        "string",
							"description": "The status of this image member",
						},
						"updated_at": gin.H{
							"type":        "string",
							"description": "Date and time of last member update",
						},
						"schema": gin.H{
							"type": "string",
						},
					},
				},
			},
			"schema": gin.H{
				"type": "string",
			},
		},
	})
}
