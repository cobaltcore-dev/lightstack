package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T194 (FR-019a, FR-057): the rule listing honours upstream's attribute filters,
// each repeatable, within what the caller may see: `openstack security group rule list <group>` sends
// security_group_id and hides the group column, so a listing that ignored it showed every visible rule as
// that group's — for an administrator, every project's.

func TestListSecurityGroupRulesFilters(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	project := createTestProject(t, "spec033-src22")
	t.Cleanup(func() { mustExec(t, "DELETE FROM security_groups WHERE project_id = $1", project) })
	a, b := sgTestGroup(t, project, "a"), sgTestGroup(t, project, "b")
	rule := func(group, direction, protocol string, port int) string {
		id := uuid.NewString()
		mustExec(t, `INSERT INTO security_group_rules (id, security_group_id, direction, ethertype, protocol, port_range_min, port_range_max, remote_ip_prefix)
			VALUES ($1, $2, $3, 'IPv4', $4, $5, $5, '10.0.0.0/24')`, id, group, direction, protocol, port)
		return id
	}
	aIn, aOut, bIn := rule(a, "ingress", "tcp", 22), rule(a, "egress", "udp", 53), rule(b, "ingress", "tcp", 80)
	mine := map[string]bool{aIn: true, aOut: true, bIn: true}

	list := func(t *testing.T, hdr map[string]string, query string) []string {
		t.Helper()
		code, body := doReq(r, http.MethodGet, "/v2.0/security-group-rules"+query, hdr)
		if code != http.StatusOK {
			t.Fatalf("GET %s: code=%d body=%s; want 200", query, code, body)
		}
		var out struct {
			Rules []struct {
				ID string `json:"id"`
			} `json:"security_group_rules"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		ids := []string{}
		for _, rl := range out.Rules {
			if mine[rl.ID] {
				ids = append(ids, rl.ID)
			}
		}
		sort.Strings(ids)
		return ids
	}
	hdr := projectHdr(project)
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	for _, tc := range []struct {
		hdr   map[string]string
		query string
		want  []string
	}{
		{hdr, "?security_group_id=" + a, []string{aIn, aOut}},
		{hdr, "?security_group_id=" + a + "&security_group_id=" + b, []string{aIn, aOut, bIn}},
		{hdr, "?security_group_id=" + a + "&direction=egress", []string{aOut}},
		{hdr, "?protocol=tcp", []string{aIn, bIn}},
		{hdr, "?port_range_min=80", []string{bIn}},
		{hdr, "?ethertype=IPv6", nil},
		{hdr, "?remote_ip_prefix=10.0.0.0/24&id=" + aIn, []string{aIn}},
		{hdr, "?security_group_id=not-a-uuid", nil},
		{admin, "?security_group_id=" + b, []string{bIn}},
	} {
		if got, want := list(t, tc.hdr, tc.query), sorted(tc.want...); !equalIDs(got, want) {
			t.Errorf("GET /v2.0/security-group-rules%s as %v: %v; want %v", tc.query, tc.hdr["X-Test-Roles"], got, want)
		}
	}
	var stored int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM security_group_rules WHERE id::text = ANY($1)", []string{aIn, aOut, bIn}).Scan(&stored); err != nil || stored != 3 {
		t.Fatalf("%d rules stored (%v)", stored, err)
	}
}
