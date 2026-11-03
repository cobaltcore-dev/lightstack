package glance

import (
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// ListMetadefNamespaces handles GET /v2/metadefs/namespaces
func (svc *Service) ListMetadefNamespaces(c *gin.Context) {
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT namespace, display_name, description, visibility, protected, owner, created_at, updated_at
		FROM metadef_namespaces
		ORDER BY namespace ASC
	`)

	if err != nil {
		log.Error().Err(err).Str("operation", "list_metadef_namespaces").Msg("failed to query metadef namespaces")
		common.SendError(c, common.NewInternalServerError("operation failed"))
		return
	}
	defer rows.Close()

	namespaces := []map[string]any{}
	for rows.Next() {
		var namespace, visibility string
		var displayName, description, owner *string
		var protected bool
		var createdAt, updatedAt time.Time

		err := rows.Scan(&namespace, &displayName, &description, &visibility, &protected, &owner, &createdAt, &updatedAt)
		if err != nil {
			continue
		}

		ns := map[string]any{
			"namespace":  namespace,
			"visibility": visibility,
			"protected":  protected,
			"created_at": createdAt.Format(time.RFC3339),
			"updated_at": updatedAt.Format(time.RFC3339),
		}

		if displayName != nil {
			ns["display_name"] = *displayName
		}
		if description != nil {
			ns["description"] = *description
		}
		if owner != nil {
			ns["owner"] = *owner
		}

		namespaces = append(namespaces, ns)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"namespaces": namespaces})
}

// CreateMetadefNamespace handles POST /v2/metadefs/namespaces
func (svc *Service) CreateMetadefNamespace(c *gin.Context) {
	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	namespace, ok := req["namespace"].(string)
	if !ok || namespace == "" {
		common.SendError(c, common.NewBadRequestError("namespace is required"))
		return
	}

	displayName, _ := req["display_name"].(string)
	description, _ := req["description"].(string)
	visibility, _ := req["visibility"].(string)
	if visibility == "" {
		visibility = "public"
	}
	owner, _ := req["owner"].(string)
	protected, _ := req["protected"].(bool)

	_, err := svc.pool.Exec(c.Request.Context(), `
		INSERT INTO metadef_namespaces (namespace, display_name, description, visibility, protected, owner, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, namespace, displayName, description, visibility, protected, owner, time.Now(), time.Now())

	if err != nil {
		common.SendError(c, common.NewConflictError("Namespace already exists"))
		return
	}

	// Handle resource type associations if provided
	if rtAssocs, ok := req["resource_type_associations"].([]any); ok {
		for _, assoc := range rtAssocs {
			if assocMap, ok := assoc.(map[string]any); ok {
				rtName, _ := assocMap["name"].(string)
				rtPrefix, _ := assocMap["prefix"].(string)

				if _, err := svc.pool.Exec(c.Request.Context(), `
					INSERT INTO metadef_resource_types (namespace, name, prefix, created_at)
					VALUES ($1, $2, $3, $4)
				`, namespace, rtName, rtPrefix, time.Now()); err != nil {
					log.Warn().Err(err).Str("namespace", namespace).Str("resource_type", rtName).Msg("failed to insert metadef resource-type association")
				}
			}
		}
	}

	result := map[string]any{
		"namespace":  namespace,
		"visibility": visibility,
		"protected":  protected,
		"created_at": time.Now().Format(time.RFC3339),
		"updated_at": time.Now().Format(time.RFC3339),
	}

	if displayName != "" {
		result["display_name"] = displayName
	}
	if description != "" {
		result["description"] = description
	}
	if owner != "" {
		result["owner"] = owner
	}

	c.JSON(http.StatusCreated, result)
}

// GetMetadefNamespace handles GET /v2/metadefs/namespaces/:namespace
func (svc *Service) GetMetadefNamespace(c *gin.Context) {
	namespace := c.Param("namespace")

	var ns, visibility string
	var displayName, description, owner *string
	var protected bool
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT namespace, display_name, description, visibility, protected, owner, created_at, updated_at
		FROM metadef_namespaces
		WHERE namespace = $1
	`, namespace).Scan(&ns, &displayName, &description, &visibility, &protected, &owner, &createdAt, &updatedAt)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("Namespace"))
		return
	}

	result := map[string]any{
		"namespace":  ns,
		"visibility": visibility,
		"protected":  protected,
		"created_at": createdAt.Format(time.RFC3339),
		"updated_at": updatedAt.Format(time.RFC3339),
	}

	if displayName != nil {
		result["display_name"] = *displayName
	}
	if description != nil {
		result["description"] = *description
	}
	if owner != nil {
		result["owner"] = *owner
	}

	// Get resource type associations
	rows, err := svc.pool.Query(c.Request.Context(), `
		SELECT name, prefix FROM metadef_resource_types WHERE namespace = $1
	`, namespace)

	if err == nil {
		defer rows.Close()
		var rtAssocs []map[string]any
		for rows.Next() {
			var name string
			var prefix *string
			if err := rows.Scan(&name, &prefix); err != nil {
				log.Warn().Err(err).Msg("failed to scan metadef resource-type row; skipping")
				continue
			}

			rt := map[string]any{
				"name": name,
			}
			if prefix != nil {
				rt["prefix"] = *prefix
			}

			rtAssocs = append(rtAssocs, rt)
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Msg("rows iteration error")
			common.SendError(c, common.NewInternalServerError("failed to read results"))
			return
		}

		if len(rtAssocs) > 0 {
			result["resource_type_associations"] = rtAssocs
		}
	}

	c.JSON(http.StatusOK, result)
}

// UpdateMetadefNamespace handles PUT /v2/metadefs/namespaces/:namespace
func (svc *Service) UpdateMetadefNamespace(c *gin.Context) {
	namespace := c.Param("namespace")

	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify namespace exists
	var exists bool
	err := svc.pool.QueryRow(c.Request.Context(), `
		SELECT EXISTS(SELECT 1 FROM metadef_namespaces WHERE namespace = $1)
	`, namespace).Scan(&exists)

	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("Namespace"))
		return
	}

	displayName, _ := req["display_name"].(string)
	description, _ := req["description"].(string)
	visibility, _ := req["visibility"].(string)
	protected, _ := req["protected"].(bool)

	_, err = svc.pool.Exec(c.Request.Context(), `
		UPDATE metadef_namespaces
		SET display_name = $1, description = $2, visibility = $3, protected = $4, updated_at = $5
		WHERE namespace = $6
	`, displayName, description, visibility, protected, time.Now(), namespace)

	if err != nil {
		log.Error().Err(err).Str("operation", "update_metadef_namespace").Msg("failed to update metadef namespace")
		common.SendError(c, common.NewInternalServerError("operation failed"))
		return
	}

	result := map[string]any{
		"namespace":  namespace,
		"visibility": visibility,
		"protected":  protected,
		"updated_at": time.Now().Format(time.RFC3339),
	}

	if displayName != "" {
		result["display_name"] = displayName
	}
	if description != "" {
		result["description"] = description
	}

	c.JSON(http.StatusOK, result)
}

// DeleteMetadefNamespace handles DELETE /v2/metadefs/namespaces/:namespace
func (svc *Service) DeleteMetadefNamespace(c *gin.Context) {
	namespace := c.Param("namespace")

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM metadef_namespaces WHERE namespace = $1",
		namespace,
	)

	if err != nil {
		log.Error().Err(err).Str("operation", "delete_metadef_namespace").Msg("failed to delete metadef namespace")
		common.SendError(c, common.NewInternalServerError("operation failed"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("Namespace"))
		return
	}

	c.Status(http.StatusNoContent)
}

// ListMetadefResourceTypes handles GET /v2/metadefs/resource_types
func (svc *Service) ListMetadefResourceTypes(c *gin.Context) {
	// Return predefined resource types
	resourceTypes := []map[string]any{
		{
			"name":       "OS::Glance::Image",
			"created_at": time.Now().Format(time.RFC3339),
			"updated_at": time.Now().Format(time.RFC3339),
		},
		{
			"name":       "OS::Cinder::Volume",
			"created_at": time.Now().Format(time.RFC3339),
			"updated_at": time.Now().Format(time.RFC3339),
		},
		{
			"name":       "OS::Nova::Flavor",
			"created_at": time.Now().Format(time.RFC3339),
			"updated_at": time.Now().Format(time.RFC3339),
		},
		{
			"name":       "OS::Nova::Instance",
			"created_at": time.Now().Format(time.RFC3339),
			"updated_at": time.Now().Format(time.RFC3339),
		},
	}

	c.JSON(http.StatusOK, gin.H{"resource_types": resourceTypes})
}

// --- Metadefs schema documents (GET /v2/schemas/metadefs/*) ---
//
// Upstream Glance serves these static JSON schemas; Horizon's Angular "Create Image"
// modal loads /v2/schemas/metadefs/namespace before it renders. Without the route it
// 404s, the modal's response handler reads `.data` off undefined (TypeError), and the
// create fails with "Unable to retrieve namespace / project / create image". These are
// fixed documents, so they need no database and no auth-specific state. Each plural
// path answers the collection schema wrapping its singular document, as upstream's
// glance/api/v2/schemas.py does.

// GetMetadefNamespaceSchema handles GET /v2/schemas/metadefs/namespace.
func (svc *Service) GetMetadefNamespaceSchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefNamespaceSchema())
}

// GetMetadefNamespacesSchema handles GET /v2/schemas/metadefs/namespaces.
func (svc *Service) GetMetadefNamespacesSchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefCollectionSchema("namespaces", metadefNamespaceSchema()))
}

// GetMetadefResourceTypeSchema handles GET /v2/schemas/metadefs/resource_type.
func (svc *Service) GetMetadefResourceTypeSchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefResourceTypeSchema())
}

// GetMetadefResourceTypesSchema handles GET /v2/schemas/metadefs/resource_types.
func (svc *Service) GetMetadefResourceTypesSchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefCollectionSchema("resource_type_associations", metadefResourceTypeSchema()))
}

// GetMetadefObjectSchema handles GET /v2/schemas/metadefs/object.
func (svc *Service) GetMetadefObjectSchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefObjectSchema())
}

// GetMetadefObjectsSchema handles GET /v2/schemas/metadefs/objects.
func (svc *Service) GetMetadefObjectsSchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefCollectionSchema("objects", metadefObjectSchema()))
}

// GetMetadefPropertySchema handles GET /v2/schemas/metadefs/property.
func (svc *Service) GetMetadefPropertySchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefPropertySchema())
}

// GetMetadefPropertiesSchema handles GET /v2/schemas/metadefs/properties. Upstream
// keys a namespace's properties by name (DictCollectionSchema), so the collection is
// an object of property schemas rather than an array.
func (svc *Service) GetMetadefPropertiesSchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefCollectionEnvelope("properties",
		gin.H{"type": "object", "additionalProperties": metadefPropertySchema()}))
}

// GetMetadefTagSchema handles GET /v2/schemas/metadefs/tag.
func (svc *Service) GetMetadefTagSchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefTagSchema())
}

// GetMetadefTagsSchema handles GET /v2/schemas/metadefs/tags.
func (svc *Service) GetMetadefTagsSchema(c *gin.Context) {
	c.JSON(http.StatusOK, metadefCollectionSchema("tags", metadefTagSchema()))
}

// metadefCollectionSchema wraps an item schema as upstream's CollectionSchema.raw()
// (glance/schema.py) does: the items as an array under the collection's name.
func metadefCollectionSchema(name string, item gin.H) gin.H {
	return metadefCollectionEnvelope(name, gin.H{"type": "array", "items": item})
}

// metadefCollectionEnvelope is the document both upstream collection kinds share: the
// members under the collection's name, plus the paging fields and their links.
func metadefCollectionEnvelope(name string, members gin.H) gin.H {
	return gin.H{
		"name": name,
		"properties": gin.H{
			name:     members,
			"first":  gin.H{"type": "string"},
			"next":   gin.H{"type": "string"},
			"schema": gin.H{"type": "string"},
		},
		"links": []gin.H{
			{"rel": "first", "href": "{first}"},
			{"rel": "next", "href": "{next}"},
			{"rel": "describedby", "href": "{schema}"},
		},
	}
}

func metadefNamespaceSchema() gin.H {
	return gin.H{
		"name":                 "namespace",
		"additionalProperties": false,
		"properties": gin.H{
			"namespace":                  gin.H{"type": "string", "description": "The unique namespace text."},
			"display_name":               gin.H{"type": "string", "description": "The user friendly name for the namespace."},
			"description":                gin.H{"type": "string", "description": "Provides a user friendly description of the namespace."},
			"visibility":                 gin.H{"type": "string", "enum": []string{"public", "private"}, "description": "Scope of namespace accessibility."},
			"protected":                  gin.H{"type": "boolean", "description": "If true, namespace will not be deletable."},
			"owner":                      gin.H{"type": "string", "description": "Owner of the namespace."},
			"created_at":                 gin.H{"type": "string", "format": "date-time"},
			"updated_at":                 gin.H{"type": "string", "format": "date-time"},
			"self":                       gin.H{"type": "string"},
			"schema":                     gin.H{"type": "string"},
			"resource_type_associations": gin.H{"type": "array"},
			"properties":                 gin.H{"type": "object"},
			"objects":                    gin.H{"type": "array"},
			"tags":                       gin.H{"type": "array"},
		},
	}
}

func metadefResourceTypeSchema() gin.H {
	return gin.H{
		"name": "resource_type_association",
		"properties": gin.H{
			"name":              gin.H{"type": "string"},
			"prefix":            gin.H{"type": "string"},
			"properties_target": gin.H{"type": "string"},
			"created_at":        gin.H{"type": "string", "format": "date-time"},
			"updated_at":        gin.H{"type": "string", "format": "date-time"},
		},
	}
}

func metadefObjectSchema() gin.H {
	return gin.H{
		"name": "object",
		"properties": gin.H{
			"name":        gin.H{"type": "string"},
			"description": gin.H{"type": "string"},
			"required":    gin.H{"type": "array"},
			"properties":  gin.H{"type": "object"},
			"schema":      gin.H{"type": "string"},
			"self":        gin.H{"type": "string"},
			"created_at":  gin.H{"type": "string", "format": "date-time"},
			"updated_at":  gin.H{"type": "string", "format": "date-time"},
		},
	}
}

func metadefPropertySchema() gin.H {
	return gin.H{
		"name": "property",
		"properties": gin.H{
			"name":        gin.H{"type": "string"},
			"title":       gin.H{"type": "string"},
			"type":        gin.H{"type": "string"},
			"description": gin.H{"type": "string"},
			"default":     gin.H{"type": "string"},
			"enum":        gin.H{"type": "array"},
		},
	}
}

func metadefTagSchema() gin.H {
	return gin.H{
		"name": "tag",
		"properties": gin.H{
			"name":       gin.H{"type": "string"},
			"created_at": gin.H{"type": "string", "format": "date-time"},
			"updated_at": gin.H{"type": "string", "format": "date-time"},
		},
	}
}
