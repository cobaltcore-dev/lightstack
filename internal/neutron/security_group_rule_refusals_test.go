package neutron

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T196: a rule's create validates its group ids before anything is read, as
// upstream's type:uuid validation does (400), and the transaction that stores it answers whether the
// groups are the caller's (404); a delete reads its rule once, locked, in the transaction that deletes it.
// The refusals are unchanged but for the malformed security_group_id, which answered 404.

func TestSecurityGroupRuleRefusals(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	requireSecondProject(t)
	own, foreign := sgTestGroup(t, seededProjectID, "spec033-src24"), sgTestGroup(t, otherProjectID, "spec033-src24-other")
	rule := func(group, remote string) string {
		body := `{"security_group_rule":{"security_group_id":"` + group + `","direction":"ingress","ethertype":"IPv4"`
		if remote != "" {
			body += `,"remote_group_id":"` + remote + `"`
		}
		return body + `}}`
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"a malformed security_group_id", rule("not-a-uuid", ""), http.StatusBadRequest},
		{"a malformed remote_group_id", rule(own, "not-a-uuid"), http.StatusBadRequest},
		{"another project's group", rule(foreign, ""), http.StatusNotFound},
		{"a group that does not exist", rule(uuid.NewString(), ""), http.StatusNotFound},
		{"another project's remote group", rule(own, foreign), http.StatusNotFound},
	} {
		if code, body := doJSONReq(r, http.MethodPost, "/v2.0/security-group-rules", tc.body, hdr); code != tc.status {
			t.Errorf("create, %s: code=%d body=%s; want %d", tc.name, code, body, tc.status)
		}
	}

	code, body := doJSONReq(r, http.MethodPost, "/v2.0/security-group-rules", rule(foreign, ""),
		map[string]string{"X-Test-Project": otherProjectID, "X-Test-Roles": "member"})
	if code != http.StatusCreated {
		t.Fatalf("the other project's own rule: code=%d body=%s; want 201", code, body)
	}
	theirs := createdID(t, body, "security_group_rule")
	for _, id := range []string{theirs, uuid.NewString(), "not-a-uuid"} {
		if code, body := doReq(r, http.MethodDelete, "/v2.0/security-group-rules/"+id, hdr); code != http.StatusNotFound {
			t.Errorf("delete %s: code=%d body=%s; want 404", id, code, body)
		}
	}
}
