package neutron

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-024 host-pass follow-up, finding 2 — Horizon's admin Network list rendered "-" in the
// Project column for every network, while the API returned the correct owner in both spellings.
//
// Upstream Horizon assigns that column inside a block guarded by DHCP-agent-scheduler support
// (admin/networks/views.py: `if networks and show_agents_column and
// is_dhcp_agent_scheduler_supported(...)`), so a missing alias blanks a column that has nothing to
// do with DHCP agents. The same block also fetches each network's agents, so the alias and the
// endpoint have to land together — advertising one without the other trades a blank column for an
// error pop-up per row.

// TestListDHCPAgentsOnNetwork_EmptyForASingleNode: O3K runs dnsmasq directly rather than through a
// scheduled agent, so it hosts no DHCP agents and says so. Empty is the honest answer and matches
// GET /v2.0/agents, which reports the same because neutron_agents is never seeded.
func TestListDHCPAgentsOnNetwork_EmptyForASingleNode(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	netID := seedExternalNetwork(t, "agents-own-"+uuid.NewString()[:8], seededProjectID, false, false)

	code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+netID+"/dhcp-agents", neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("GET dhcp-agents: code=%d body=%s; want 200 — a 404 makes Horizon raise an error pop-up per network row", code, body)
	}
	var env struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse dhcp-agents: %v; body=%s", err, body)
	}
	if len(env.Agents) != 0 {
		t.Errorf("dhcp-agents = %v; want an empty list — O3K schedules no DHCP agents", env.Agents)
	}
	if env.Agents == nil {
		t.Errorf("dhcp-agents body is %s; want an \"agents\" key holding [], not a missing or null one", body)
	}
}

// A network the caller cannot see must not be distinguishable from one that does not exist, so the
// sub-resource takes the same owner-or-shared-or-external predicate as GetNetwork rather than a
// bare existence check.
func TestListDHCPAgentsOnNetwork_NotFound(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	privateElsewhere := seedExternalNetwork(t, "agents-other-"+uuid.NewString()[:8], otherProjectID, false, false)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"unknown network", "/v2.0/networks/" + uuid.NewString() + "/dhcp-agents"},
		{"another project's private network", "/v2.0/networks/" + privateElsewhere + "/dhcp-agents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, http.MethodGet, tc.path, neutronAuthHdr(seededProjectID, false))
			if code != http.StatusNotFound {
				t.Errorf("GET %s: code=%d body=%s; want 404", tc.path, code, body)
			}
		})
	}
}

// A shared or external network is visible to every project (FR-033a), so its sub-resource is too —
// read-only visibility, exactly as #545 established.
func TestListDHCPAgentsOnNetwork_VisibleNetworksOfOthers(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()

	for _, tc := range []struct {
		name             string
		shared, external bool
	}{
		{"shared", true, false},
		{"external", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := seedExternalNetwork(t, "agents-"+tc.name+"-"+uuid.NewString()[:8], otherProjectID, tc.shared, tc.external)
			code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+id+"/dhcp-agents", neutronAuthHdr(seededProjectID, false))
			if code != http.StatusOK {
				t.Errorf("GET dhcp-agents on a %s network: code=%d body=%s; want 200", tc.name, code, body)
			}
		})
	}
}
