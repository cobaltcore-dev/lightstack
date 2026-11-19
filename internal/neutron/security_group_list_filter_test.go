package neutron

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
)

// SPEC-033 T031 (FR-019a; contracts/network-api.md; research R13): the group listing honours `id`,
// repeated, and `name`. Horizon's Edit Security Groups sends a repeated ?id=, and Terraform's data
// source sends ?name= and refuses more than one result.
func TestListSecurityGroupsFilters(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	project := createTestProject(t, "spec033-list")
	other := createTestProject(t, "spec033-list-other")
	a := sgTestGroup(t, project, "a")
	b := sgTestGroup(t, project, "b")
	c := sgTestGroup(t, project, "c")
	x := sgTestGroup(t, other, "x")
	hdr := projectHdr(project)

	list := func(t *testing.T, query string) []string {
		t.Helper()
		code, body := doReq(r, http.MethodGet, "/v2.0/security-groups"+query, hdr)
		if code != http.StatusOK {
			t.Fatalf("GET %s: code=%d body=%s; want 200", query, code, body)
		}
		var out struct {
			SecurityGroups []struct {
				ID string `json:"id"`
			} `json:"security_groups"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		ids := []string{}
		for _, g := range out.SecurityGroups {
			ids = append(ids, g.ID)
		}
		sort.Strings(ids)
		return ids
	}

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"?id=" + a, []string{a}},
		{"?id=" + a + "&id=" + b, []string{a, b}},
		{"?name=b", []string{b}},
		{"?id=" + a + "&name=b", nil},
		{"?id=" + x, nil},
		{"?id=not-a-uuid", nil},
		{"", []string{a, b, c}}, // lock: no query lists the caller's groups
	} {
		if got, want := list(t, tc.query), sorted(tc.want...); !equalIDs(got, want) {
			t.Errorf("GET /v2.0/security-groups%s: %v; want %v", tc.query, got, want)
		}
	}
}
