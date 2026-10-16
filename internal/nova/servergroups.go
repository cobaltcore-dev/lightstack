package nova

import (
	"net/http"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// ListServerGroups handles GET /v2.1/os-server-groups
func (svc *Service) ListServerGroups(c *gin.Context) {
	projectID := c.GetString("project_id")
	// An unscoped token names no project, so it owns no server group. Bound as ""
	// the empty project would fail the uuid cast and answer 500.
	if projectID == "" {
		c.JSON(http.StatusOK, gin.H{"server_groups": []gin.H{}})
		return
	}

	rows, err := svc.pool.Query(c.Request.Context(),
		`SELECT id, name, policies, members, project_id, created_at, updated_at
		 FROM server_groups WHERE project_id = $1`,
		projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_server_groups").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list server groups"))
		return
	}
	defer rows.Close()

	serverGroups := []gin.H{}
	for rows.Next() {
		var id, name, projectID string
		var policies []string
		var members []string
		var createdAt, updatedAt time.Time

		err := rows.Scan(&id, &name, &policies, &members, &projectID, &createdAt, &updatedAt)
		if err != nil {
			continue
		}

		serverGroups = append(serverGroups,
			serverGroupResponse(id, name, policies, members, projectID))
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to list server groups"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"server_groups": serverGroups})
}

// validServerGroupPolicies — the vocabulary upstream accepts for either the
// legacy `policies` array or the 2.64+ `policy` string.
var validServerGroupPolicies = map[string]bool{
	"affinity":           true,
	"anti-affinity":      true,
	"soft-affinity":      true,
	"soft-anti-affinity": true,
}

// serverGroupPolicyFrom resolves the policy from either request shape:
// pre-2.63 clients send `policies: ["anti-affinity"]`, 2.64+ clients (OSC 7
// among them, whenever the negotiated microversion is ≥2.64 — and the server
// advertises 2.90) send `policy: "anti-affinity"`. Returns ok=false when
// both or neither is present, which upstream answers with 400.
func serverGroupPolicyFrom(policies []string, policy string) (string, bool) {
	if policy != "" && len(policies) > 0 {
		return "", false
	}
	if policy != "" {
		if !validServerGroupPolicies[policy] {
			return "", false
		}
		return policy, true
	}
	if len(policies) == 1 {
		if !validServerGroupPolicies[policies[0]] {
			return "", false
		}
		return policies[0], true
	}
	// An empty policies array or more than one entry: upstream's array form
	// takes exactly one policy.
	return "", false
}

// serverGroupResponse renders one server group for the API. It carries BOTH
// the legacy `policies` array and the 2.64+ `policy` string, so clients of
// any microversion read the field they negotiated for.
func serverGroupResponse(id, name string, policies []string, members []string, projectID string) gin.H {
	policy := ""
	if len(policies) > 0 {
		policy = policies[0]
	}
	return gin.H{
		"id":         id,
		"name":       name,
		"policies":   policies,
		"policy":     policy,
		"members":    members,
		"metadata":   gin.H{},
		"project_id": projectID,
	}
}

// CreateServerGroup handles POST /v2.1/os-server-groups
func (svc *Service) CreateServerGroup(c *gin.Context) {
	projectID := c.GetString("project_id")

	var req struct {
		ServerGroup struct {
			Name     string   `json:"name"`
			Policies []string `json:"policies"`
			Policy   string   `json:"policy"`
			// 2.64+ also accepts rules (max_server_per_host). Accepted and
			// validated for shape, not persisted: placement is not enforced
			// in stub mode, and a rules column would need a migration for a
			// knob nothing reads yet.
			Rules *struct {
				MaxServerPerHost int `json:"max_server_per_host"`
			} `json:"rules"`
		} `json:"server_group"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}
	if req.ServerGroup.Name == "" {
		common.SendError(c, common.NewBadRequestError("name is required"))
		return
	}

	policy, ok := serverGroupPolicyFrom(req.ServerGroup.Policies, req.ServerGroup.Policy)
	if !ok {
		common.SendError(c, common.NewBadRequestError(
			"server group needs exactly one policy (policy string or policies array)"))
		return
	}

	// An unscoped token has no project to create the group in.
	if projectID == "" {
		common.SendError(c, common.NewForbiddenError(
			"a server group belongs to a project; use a project-scoped token"))
		return
	}

	// Upstream nova refuses a duplicate (project, name) with 409 — and the
	// server group is then addressable by name unambiguously. The handler
	// used to insert unconditionally: a second group with the same name was
	// silently accepted with 200, and `server group show <name>` became a
	// coin toss (SPEC-028).
	var nameTaken bool
	err := svc.pool.QueryRow(c.Request.Context(),
		`SELECT EXISTS(SELECT 1 FROM server_groups WHERE project_id = $1 AND name = $2)`,
		projectID, req.ServerGroup.Name).Scan(&nameTaken)
	if err != nil {
		log.Error().Err(err).Str("operation", "create_server_group_dupcheck").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create server group"))
		return
	}
	if nameTaken {
		common.SendError(c, common.NewResourceInUseError("server group", "create",
			"Server group "+req.ServerGroup.Name+" already exists."))
		return
	}

	groupID := uuid.New().String()
	now := time.Now()

	_, err = svc.pool.Exec(c.Request.Context(),
		`INSERT INTO server_groups (id, name, policies, members, project_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		groupID, req.ServerGroup.Name, []string{policy}, []string{}, projectID, now, now,
	)
	if err != nil {
		// The migration 076 constraint makes the duplicate check race-safe:
		// two concurrent creates both pass it, and exactly one insert wins.
		if common.IsUniqueViolation(err) {
			common.SendError(c, common.NewResourceInUseError("server group", "create",
				"Server group "+req.ServerGroup.Name+" already exists."))
			return
		}
		log.Error().Err(err).Str("operation", "create_server_group").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to create server group"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"server_group": serverGroupResponse(groupID, req.ServerGroup.Name, []string{policy}, []string{}, projectID),
	})
}

// GetServerGroup handles GET /v2.1/os-server-groups/:id
func (svc *Service) GetServerGroup(c *gin.Context) {
	groupID := c.Param("id")
	projectID := c.GetString("project_id")
	// An unscoped token owns no server group (see ListServerGroups).
	if projectID == "" {
		common.SendError(c, common.NewNotFoundError("server group"))
		return
	}

	var name, projectIDFromDB string
	var policies []string
	var members []string
	var createdAt, updatedAt time.Time

	err := svc.pool.QueryRow(c.Request.Context(),
		`SELECT name, policies, members, project_id, created_at, updated_at
		 FROM server_groups WHERE id = $1 AND project_id = $2`,
		groupID, projectID,
	).Scan(&name, &policies, &members, &projectIDFromDB, &createdAt, &updatedAt)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("server group"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"server_group": serverGroupResponse(groupID, name, policies, members, projectIDFromDB),
	})
}

// DeleteServerGroup handles DELETE /v2.1/os-server-groups/:id
func (svc *Service) DeleteServerGroup(c *gin.Context) {
	groupID := c.Param("id")
	projectID := c.GetString("project_id")
	// An unscoped token owns no server group (see ListServerGroups).
	if projectID == "" {
		common.SendError(c, common.NewNotFoundError("server group"))
		return
	}

	result, err := svc.pool.Exec(c.Request.Context(),
		"DELETE FROM server_groups WHERE id = $1 AND project_id = $2",
		groupID, projectID,
	)
	if err != nil {
		log.Error().Err(err).Str("operation", "delete_server_group").Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to delete server group"))
		return
	}

	if result.RowsAffected() == 0 {
		common.SendError(c, common.NewNotFoundError("server group"))
		return
	}

	c.Status(http.StatusNoContent)
}
