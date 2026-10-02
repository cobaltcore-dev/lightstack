package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Security-group rules are firewall state, and all three single-rule handlers addressed a
// rule (or its group) purely by the id in the request: Create wrote into any tenant's
// chain, Get returned any tenant's protocol/ports/remote CIDRs, and Delete removed any
// rule in the cluster — from iptables as well as the table. ListSecurityGroupRules was
// the only one that scoped, via a JOIN on the owning group's project.
//
// Reported on PR #79 as SEC-1/SEC-2/SEC-3. Pre-existing rather than introduced there: the
// base commit has the same zero project_id references in all three.
//
// A foreign group or rule must read as ABSENT (404), never Forbidden — a 403 confirms the
// id exists, which is the same cross-tenant disclosure being closed.

// foreignSecurityGroup creates a security group, and one rule in it, owned by a project
// that is not the caller's. Returns (groupID, ruleID).
func foreignSecurityGroup(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	other := createTestProject(t, "spec020-sgrule-authz-owner")

	sgID, ruleID := uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO security_groups (id, name, project_id, description) VALUES ($1, $2, $3, '')`,
		sgID, "foreign-sg-"+sgID[:8], other); err != nil {
		t.Fatalf("seed foreign security group: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO security_group_rules (id, security_group_id, direction, ethertype, protocol,
		                                   port_range_min, port_range_max, remote_ip_prefix, created_at)
		 VALUES ($1, $2, 'ingress', 'IPv4', 'tcp', 22, 22, '10.9.0.0/16', NOW())`,
		ruleID, sgID); err != nil {
		t.Fatalf("seed foreign rule: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM security_group_rules WHERE id = $1`, ruleID) //nolint:errcheck // best-effort cleanup
		testPool.Exec(ctx, `DELETE FROM security_groups WHERE id = $1`, sgID)        //nolint:errcheck // best-effort cleanup
	})
	return sgID, ruleID
}

// SEC-1: a rule may not be created in a group the caller does not own.
func TestCreateSecurityGroupRule_RejectsForeignGroup(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	foreignSG, _ := foreignSecurityGroup(t)

	code, body := doReqBody(r, http.MethodPost, "/v2.0/security-group-rules",
		`{"security_group_rule":{"security_group_id":"`+foreignSG+`","direction":"ingress",`+
			`"ethertype":"IPv4","protocol":"tcp","port_range_min":80,"port_range_max":80}}`,
		neutronAuthHdr(seededProjectID, true))
	if code != http.StatusNotFound {
		t.Errorf("create rule in a foreign group: code=%d body=%s; want 404", code, body)
	}

	// And nothing may have been written to the foreign group.
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM security_group_rules WHERE security_group_id = $1 AND port_range_min = 80`,
		foreignSG).Scan(&n); err != nil {
		t.Fatalf("count foreign rules: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rule(s) landed in the foreign group despite the 404", n)
	}
}

// SEC-1, other direction: the caller's OWN group must still work.
func TestCreateSecurityGroupRule_AllowsOwnGroup(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)
	ownSG := seededDefaultGroupID(t, r, hdr)

	code, body := doReqBody(r, http.MethodPost, "/v2.0/security-group-rules",
		`{"security_group_rule":{"security_group_id":"`+ownSG+`","direction":"ingress",`+
			`"ethertype":"IPv4","protocol":"tcp","port_range_min":8080,"port_range_max":8080}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create rule in own group: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		Rule struct{ ID string } `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("parse created rule: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), //nolint:errcheck // best-effort cleanup
			`DELETE FROM security_group_rules WHERE id = $1`, created.Rule.ID)
	})
}

// SEC-3: a foreign rule must not be readable.
func TestGetSecurityGroupRule_HidesForeignRule(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	_, foreignRule := foreignSecurityGroup(t)

	code, body := doReq(r, http.MethodGet, "/v2.0/security-group-rules/"+foreignRule,
		neutronAuthHdr(seededProjectID, true))
	if code != http.StatusNotFound {
		t.Errorf("get a foreign rule: code=%d body=%s; want 404 (its ports and remote CIDR are firewall config)", code, body)
	}
}

// SEC-2: a foreign rule must not be deletable, and must survive the attempt.
func TestDeleteSecurityGroupRule_RejectsForeignRule(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	_, foreignRule := foreignSecurityGroup(t)

	code, body := doReq(r, http.MethodDelete, "/v2.0/security-group-rules/"+foreignRule,
		neutronAuthHdr(seededProjectID, true))
	if code != http.StatusNotFound {
		t.Errorf("delete a foreign rule: code=%d body=%s; want 404", code, body)
	}

	var alive bool
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM security_group_rules WHERE id = $1)`, foreignRule).Scan(&alive); err != nil {
		t.Fatalf("check rule survival: %v", err)
	}
	if !alive {
		t.Error("the foreign rule was deleted despite the 404")
	}
}

// SEC-2, other direction: deleting one's own rule still works, and an unknown id is a 404
// rather than the 204 the unscoped version returned for a no-op delete.
func TestDeleteSecurityGroupRule_OwnRuleAndUnknownID(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)
	ownSG := seededDefaultGroupID(t, r, hdr)

	_, body := doReqBody(r, http.MethodPost, "/v2.0/security-group-rules",
		`{"security_group_rule":{"security_group_id":"`+ownSG+`","direction":"ingress",`+
			`"ethertype":"IPv4","protocol":"udp","port_range_min":9999,"port_range_max":9999}}`, hdr)
	var created struct {
		Rule struct{ ID string } `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("parse created rule: %v; body=%s", err, body)
	}

	if code, b := doReq(r, http.MethodDelete, "/v2.0/security-group-rules/"+created.Rule.ID, hdr); code != http.StatusNoContent {
		t.Errorf("delete own rule: code=%d body=%s; want 204", code, b)
	}
	if code, b := doReq(r, http.MethodDelete, "/v2.0/security-group-rules/"+uuid.NewString(), hdr); code != http.StatusNotFound {
		t.Errorf("delete unknown rule: code=%d body=%s; want 404", code, b)
	}
}

// TestCreateSecurityGroupRule_RemoteGroupMustBeCallers: remote_group_id names the group whose
// members a rule admits, so it passes the same gate as the target group. Upstream refuses a
// malformed id (400, `uuid_or_none`) and a group the rule's project cannot see (404,
// _validate_security_group_rule). Unchecked, a malformed or unknown id reached the uuid foreign
// key as a 500, and another project's group was stored as a cross-tenant reference — the 500/201
// split also told the caller which foreign group ids exist. Raised on PR #79.
func TestCreateSecurityGroupRule_RemoteGroupMustBeCallers(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)
	ownSG := seededDefaultGroupID(t, r, hdr)
	foreignSG, _ := foreignSecurityGroup(t)

	create := func(remote string) (int, string) {
		return doReqBody(r, http.MethodPost, "/v2.0/security-group-rules",
			`{"security_group_rule":{"security_group_id":"`+ownSG+`","direction":"ingress",`+
				`"ethertype":"IPv4","protocol":"tcp","port_range_min":2201,"port_range_max":2201,`+
				`"remote_group_id":"`+remote+`"}}`, hdr)
	}
	for _, tc := range []struct {
		name, remote string
		want         int
	}{
		{"malformed id", "not-a-uuid", http.StatusBadRequest},
		{"unknown group", uuid.NewString(), http.StatusNotFound},
		{"another project's group", foreignSG, http.StatusNotFound},
	} {
		if code, body := create(tc.remote); code != tc.want {
			t.Errorf("%s: code=%d body=%s; want %d", tc.name, code, body, tc.want)
		}
	}
	var stored int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM security_group_rules WHERE security_group_id = $1 AND port_range_min = 2201`,
		ownSG).Scan(&stored); err != nil {
		t.Fatalf("count rules: %v", err)
	}
	if stored != 0 {
		t.Errorf("%d refused rule(s) were stored anyway", stored)
	}

	// The caller's own group — including the group itself, upstream's default pattern — still works.
	code, body := create(ownSG)
	if code != http.StatusCreated {
		t.Fatalf("own remote group: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		Rule struct{ ID string } `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("parse created rule: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), //nolint:errcheck // best-effort cleanup
			`DELETE FROM security_group_rules WHERE id = $1`, created.Rule.ID)
	})
}
