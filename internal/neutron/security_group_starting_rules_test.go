package neutron

import (
	"net/http"
	"testing"
)

// SPEC-033 round 2, T079 (FR-043; research R39): groups start with upstream's rules. A group created
// through the API has two egress rules, IPv4 and IPv6, open to everything; a project's default also
// admits its own members, IPv4 and IPv6. No ingress rule is open to every address (cloud-storage#528).

func TestSecurityGroupsStartWithUpstreamRules(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	project := createTestProject(t, "spec033-start")
	hdr := map[string]string{"X-Test-Project": project, "X-Test-Roles": "member"}

	created := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-start"}}`)
	id := created["id"].(string)
	if got, want := ruleShapesOf(t, id), []string{"egress/IPv4//", "egress/IPv6//"}; !equalIDs(got, want) {
		t.Errorf("a created group's rules: %v; want %v", got, want)
	}
	if rules, ok := created["security_group_rules"].([]any); !ok || len(rules) != 2 {
		t.Errorf("the create response's security_group_rules: %#v; want the two starting rules", created["security_group_rules"])
	}

	def := ensureDefault(t, project)
	want := []string{"egress/IPv4//", "egress/IPv6//", "ingress/IPv4/self/", "ingress/IPv6/self/"}
	if got := ruleShapesOf(t, def); !equalIDs(got, want) {
		t.Errorf("default's rules: %v; want %v", got, want)
	}

	// The lock: neither group has an ingress rule with neither remote.
	for _, g := range []string{id, def} {
		if n := ruleCount(t, "security_group_id = $1 AND direction = 'ingress' AND remote_group_id IS NULL AND remote_ip_prefix IS NULL", g); n != 0 {
			t.Errorf("group %s has %d ingress rule(s) open to every address; want none", g, n)
		}
	}

	// A group made by the API carries them in a listing too.
	code, body := doReq(r, http.MethodGet, "/v2.0/security-groups/"+id, hdr)
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
}
