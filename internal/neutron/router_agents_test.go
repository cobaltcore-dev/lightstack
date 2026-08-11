package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// GET /v2.0/routers/{id}/l3-agents answered from an unscoped existence check:
//
//	SELECT EXISTS(SELECT 1 FROM routers WHERE id = $1)
//
// so another project's router returned 200 with an empty agent list while an id belonging to no
// router returned 404. The two answers distinguish existence, which makes the endpoint a probe for
// routers the caller may not see. Its sibling ListDHCPAgentsOnNetwork, added by the same follow-up,
// scopes properly; this one was missed.
//
// The response body is empty either way — Lightstack schedules no agents — so nothing is disclosed except
// the fact of existence. That is still one bit more than a caller is entitled to, and it is the same
// non-disclosure rule the rest of the service follows: an invisible resource is indistinguishable
// from an absent one.

func seedRouter(t *testing.T, name, projectID string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO routers (id, name, project_id, admin_state_up, status, distributed, ha, created_at, updated_at)
		VALUES ($1, $2, $3, true, 'ACTIVE', false, false, now(), now())
	`, id, name, projectID); err != nil {
		t.Fatalf("seed router %s: %v", name, err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM routers WHERE id = $1", id) //nolint:errcheck // best-effort
	})
	return id
}

func TestListL3AgentsOnRouterDoesNotDiscloseOtherProjectsRouters(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()

	foreign := seedRouter(t, "l3-foreign-"+uuid.NewString()[:8], otherProjectID)

	for _, tc := range []struct{ name, id string }{
		{"another project's router", foreign},
		{"a router that does not exist", uuid.NewString()},
		{"a malformed id", "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, http.MethodGet, "/v2.0/routers/"+tc.id+"/l3-agents",
				neutronAuthHdr(seededProjectID, false))
			if code != http.StatusNotFound {
				t.Errorf("GET l3-agents for %s: code=%d body=%s; want 404 — an invisible router must "+
					"be indistinguishable from an absent one", tc.name, code, body)
			}
		})
	}
}

// The caller's own router still answers, with the empty list Lightstack genuinely has.
func TestListL3AgentsOnRouterAnswersForOwnRouter(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	own := seedRouter(t, "l3-own-"+uuid.NewString()[:8], seededProjectID)

	code, body := doReq(r, http.MethodGet, "/v2.0/routers/"+own+"/l3-agents",
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("GET l3-agents for the caller's own router: code=%d body=%s; want 200", code, body)
	}
	if agentsLen(t, body) != 0 {
		t.Errorf("agents = %s; want an empty list — Lightstack schedules no L3 agents", body)
	}
}

func agentsLen(t *testing.T, body string) int {
	t.Helper()
	var env struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse agents: %v; body=%s", err, body)
	}
	return len(env.Agents)
}

// The write path on the same subtree. Unscoped it would have let a caller schedule an agent onto
// another project's router; nothing is exploitable today only because neutron_agents is empty, which
// is cover by accident rather than by design.
func TestAddL3AgentToRouterDoesNotDiscloseOtherProjectsRouters(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()

	foreign := seedRouter(t, "l3-add-foreign-"+uuid.NewString()[:8], otherProjectID)

	code, body := doReqBody(r, http.MethodPost, "/v2.0/routers/"+foreign+"/l3-agents",
		`{"agent_id":"`+uuid.NewString()+`"}`, neutronAuthHdr(seededProjectID, false))
	if code != http.StatusNotFound {
		t.Errorf("POST l3-agents on another project's router: code=%d body=%s; want 404", code, body)
	}
}
