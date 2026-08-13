package neutron

import (
	"net/http"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// ListAgents returns all network agents
func (svc *Service) ListAgents(c *gin.Context) {
	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT id, agent_type, "binary", host, description, admin_state_up, alive, started_at, created_at, configurations
		FROM neutron_agents
		ORDER BY created_at DESC
	`)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_agents").Msg("failed to query agents")
		common.SendError(c, common.NewInternalServerError("failed to list agents"))
		return
	}
	defer rows.Close()

	agents := []map[string]any{}
	for rows.Next() {
		var id, agentType, binary, host string
		var description *string
		var adminStateUp, alive bool
		var startedAt, createdAt time.Time
		var configurations map[string]any

		if err := rows.Scan(&id, &agentType, &binary, &host, &description, &adminStateUp, &alive, &startedAt, &createdAt, &configurations); err != nil {
			log.Warn().Err(err).Msg("failed to scan agent row")
			continue
		}

		agent := map[string]any{
			"id":             id,
			"agent_type":     agentType,
			"binary":         binary,
			"host":           host,
			"admin_state_up": adminStateUp,
			"alive":          alive,
			"started_at":     startedAt.Format(time.RFC3339),
			"created_at":     createdAt.Format(time.RFC3339),
			"configurations": configurations,
		}

		if description != nil {
			agent["description"] = *description
		}

		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

// GetAgent returns a specific agent by ID
func (svc *Service) GetAgent(c *gin.Context) {
	agentID := c.Param("id")

	var id, agentType, binary, host string
	var description *string
	var adminStateUp, alive bool
	var startedAt, createdAt time.Time
	var configurations map[string]any

	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT id, agent_type, "binary", host, description, admin_state_up, alive, started_at, created_at, configurations
		FROM neutron_agents
		WHERE id = $1
	`, agentID).Scan(&id, &agentType, &binary, &host, &description, &adminStateUp, &alive, &startedAt, &createdAt, &configurations)

	if err != nil {
		common.SendError(c, common.NewNotFoundError("agent"))
		return
	}

	agent := map[string]any{
		"id":             id,
		"agent_type":     agentType,
		"binary":         binary,
		"host":           host,
		"admin_state_up": adminStateUp,
		"alive":          alive,
		"started_at":     startedAt.Format(time.RFC3339),
		"created_at":     createdAt.Format(time.RFC3339),
		"configurations": configurations,
	}

	if description != nil {
		agent["description"] = *description
	}

	c.JSON(http.StatusOK, gin.H{"agent": agent})
}

// ListDHCPAgentsOnNetwork returns the DHCP agents hosting a network — always none.
//
// O3K serves DHCP by running dnsmasq directly on the node, not by scheduling a network onto an
// agent, so it hosts no DHCP agents and says so. That matches GET /v2.0/agents, which reports the
// same because nothing ever populates neutron_agents.
//
// It exists because Horizon's admin Network list assigns its **Project** column inside a block
// guarded by `is_dhcp_agent_scheduler_supported`, and that same block fetches each network's agents.
// With the alias advertised but this endpoint missing, every row raised an error pop-up; with
// neither, the ownership column rendered "-" for every network even though the API returned the
// right owner (SPEC-024 host pass, finding 2).
//
// Scheduling (POST/DELETE) is deliberately absent: there is no agent to schedule onto, so the honest
// answer to a schedule request is that the route does not exist.
func (svc *Service) ListDHCPAgentsOnNetwork(c *gin.Context) {
	networkID := c.Param("id")
	projectID := c.GetString("project_id")

	// Owner or shared or external, the same predicate GetNetwork uses (FR-033a) — a network the
	// caller cannot see must not be distinguishable here from one that does not exist.
	var exists bool
	err := database.DB.QueryRow(c.Request.Context(), `
		SELECT EXISTS(
			SELECT 1 FROM networks
			WHERE (id::text = $1 OR name = $1)
			  AND (project_id = $2 OR shared = true OR external = true)
		)`, networkID, projectID).Scan(&exists)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_dhcp_agents_on_network").Str("network_id", networkID).Msg("database error")
		common.SendError(c, common.NewInternalServerError("failed to list agents"))
		return
	}
	if !exists {
		common.SendError(c, common.NewNotFoundError("network"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"agents": []map[string]any{}})
}

// ListL3AgentsOnRouter returns L3 agents hosting a specific router
func (svc *Service) ListL3AgentsOnRouter(c *gin.Context) {
	routerID := c.Param("id")

	// Verify router exists
	var exists bool
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM routers WHERE id = $1)",
		routerID).Scan(&exists)
	if err != nil || !exists {
		common.SendError(c, common.NewNotFoundError("router"))
		return
	}

	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT a.id, a.agent_type, a."binary", a.host, a.description, a.admin_state_up, a.alive, a.started_at, a.created_at, a.configurations
		FROM neutron_agents a
		JOIN router_l3_agents r ON a.id = r.agent_id
		WHERE r.router_id = $1
		ORDER BY a.created_at DESC
	`, routerID)
	if err != nil {
		log.Error().Err(err).Str("operation", "list_l3_agents_on_router").Msg("failed to query agents")
		common.SendError(c, common.NewInternalServerError("failed to list agents"))
		return
	}
	defer rows.Close()

	agents := []map[string]any{}
	for rows.Next() {
		var id, agentType, binary, host string
		var description *string
		var adminStateUp, alive bool
		var startedAt, createdAt time.Time
		var configurations map[string]any

		if err := rows.Scan(&id, &agentType, &binary, &host, &description, &adminStateUp, &alive, &startedAt, &createdAt, &configurations); err != nil {
			log.Warn().Err(err).Msg("failed to scan agent row")
			continue
		}

		agent := map[string]any{
			"id":             id,
			"agent_type":     agentType,
			"binary":         binary,
			"host":           host,
			"admin_state_up": adminStateUp,
			"alive":          alive,
			"started_at":     startedAt.Format(time.RFC3339),
			"created_at":     createdAt.Format(time.RFC3339),
			"configurations": configurations,
		}

		if description != nil {
			agent["description"] = *description
		}

		agents = append(agents, agent)
	}
	if err := rows.Err(); err != nil {
		log.Error().Err(err).Msg("rows iteration error")
		common.SendError(c, common.NewInternalServerError("failed to read results"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

// AddL3AgentToRouter schedules a router to an L3 agent
func (svc *Service) AddL3AgentToRouter(c *gin.Context) {
	routerID := c.Param("id")

	var req struct {
		AgentID string `json:"agent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Verify router exists
	var routerExists bool
	err := database.DB.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM routers WHERE id = $1)",
		routerID).Scan(&routerExists)
	if err != nil || !routerExists {
		common.SendError(c, common.NewNotFoundError("router"))
		return
	}

	// Verify agent exists
	var agentExists bool
	err = database.DB.QueryRow(c.Request.Context(),
		"SELECT EXISTS(SELECT 1 FROM neutron_agents WHERE id = $1)",
		req.AgentID).Scan(&agentExists)
	if err != nil || !agentExists {
		common.SendError(c, common.NewNotFoundError("agent"))
		return
	}

	// Add association
	_, err = database.DB.Exec(c.Request.Context(), `
		INSERT INTO router_l3_agents (router_id, agent_id, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (router_id, agent_id) DO NOTHING
	`, routerID, req.AgentID, time.Now())

	if err != nil {
		log.Error().Err(err).Str("operation", "add_l3_agent_to_router").Msg("failed to add agent to router")
		common.SendError(c, common.NewInternalServerError("failed to add agent to router"))
		return
	}

	c.Status(http.StatusCreated)
}
