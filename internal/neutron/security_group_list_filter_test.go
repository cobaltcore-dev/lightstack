package neutron

import (
	"context"
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
	// A listing ensures the project's default group, as upstream's (SPEC-033 T193).
	def := ensuredDefault(t, project)

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
		{"", []string{a, b, c, def}}, // lock: no query lists the caller's groups, its default among them
	} {
		if got, want := list(t, tc.query), sorted(tc.want...); !equalIDs(got, want) {
			t.Errorf("GET /v2.0/security-groups%s: %v; want %v", tc.query, got, want)
		}
	}
}

// SPEC-033 round 2, T071 (FR-019a, FR-019b; research R38): every attribute filter upstream's group
// listing honours, each repeatable, within what the caller may see. shared and the tag filters answer
// from what is true of every Lightstack group: not shared, no tags. A boolean that is not one is 400.
func TestListSecurityGroupsAttributeFilters(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	project := createTestProject(t, "spec033-attr")
	other := createTestProject(t, "spec033-attr-other")
	a := sgTestGroup(t, project, "a")
	b := sgTestGroup(t, project, "b")
	c := sgTestGroup(t, project, "c")
	x := sgTestGroup(t, other, "x")
	mustExec(t, "UPDATE security_groups SET description = 'alpha' WHERE id = $1 OR id = $2", a, c)
	mustExec(t, "UPDATE security_groups SET description = 'beta' WHERE id = $1", b)
	mustExec(t, "UPDATE security_groups SET stateful = $1, revision_number = 2 WHERE id = $2", false, c)
	member := map[string]string{"X-Test-Project": project, "X-Test-Roles": "member"}
	admin := map[string]string{"X-Test-Project": project, "X-Test-Roles": "admin"}
	// A listing ensures the default group of the project it concerns, as upstream's (branch review
	// T193): every query that matches it lists it.
	defP, defO := ensuredDefault(t, project), ensuredDefault(t, other)

	list := func(t *testing.T, hdr map[string]string, query string) (int, []string) {
		t.Helper()
		code, body := doReq(r, http.MethodGet, "/v2.0/security-groups"+query, hdr)
		if code != http.StatusOK {
			return code, nil
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
		return code, ids
	}

	all := []string{a, b, c}
	allP := []string{a, b, c, defP}
	for _, tc := range []struct {
		name  string
		hdr   map[string]string
		query string
		want  []string
	}{
		{"description", member, "?description=alpha", []string{a, c}},
		{"description, repeated", member, "?description=alpha&description=beta", all},
		{"a repeated name", member, "?name=a&name=b", []string{a, b}},
		{"own project_id", member, "?project_id=" + project, allP},
		{"own tenant_id", member, "?tenant_id=" + project, allP},
		{"another project as a member", member, "?project_id=" + other, nil},
		{"another project's tenant_id as a member", member, "?tenant_id=" + other, nil},
		{"another project as an administrator", admin, "?project_id=" + other, []string{x, defO}},
		{"two projects as an administrator", admin, "?project_id=" + project + "&tenant_id=" + other, append([]string{x, defO}, allP...)},
		{"shared=false", member, "?shared=false", allP},
		{"shared=False", member, "?shared=False", allP},
		{"shared=no", member, "?shared=no", allP},
		{"stateful=yes", member, "?stateful=yes", []string{a, b, defP}},
		{"shared=true", member, "?shared=true", nil},
		{"stateful=true", member, "?stateful=true", []string{a, b, defP}},
		{"stateful=false", member, "?stateful=false", []string{c}},
		{"revision_number", member, "?revision_number=2", []string{c}},
		{"revision_number, repeated", member, "?revision_number=0&revision_number=2", allP},
		{"tags", member, "?tags=blue", nil},
		{"tags-any", member, "?tags-any=blue", nil},
		{"not-tags", member, "?not-tags=blue", allP},
		{"not-tags-any", member, "?not-tags-any=blue", allP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, got := list(t, tc.hdr, tc.query)
			if code != http.StatusOK {
				t.Fatalf("GET /v2.0/security-groups%s: code=%d; want 200", tc.query, code)
			}
			if want := sorted(tc.want...); !equalIDs(got, want) {
				t.Errorf("GET /v2.0/security-groups%s: %v; want %v", tc.query, got, want)
			}
		})
	}

	for _, query := range []string{"?shared=maybe", "?stateful=maybe", "?revision_number=two"} {
		t.Run("refused "+query, func(t *testing.T) {
			if code, _ := list(t, member, query); code != http.StatusBadRequest {
				t.Errorf("GET /v2.0/security-groups%s: code=%d; want 400", query, code)
			}
		})
	}
}

// T071 (FR-019b): a group in a listing, a show, a create and an update reports its stateful and
// revision_number, and shared: false and tags: [], as upstream's does.
func TestSecurityGroupBodiesCarryUpstreamFields(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	project := createTestProject(t, "spec033-fields")
	hdr := map[string]string{"X-Test-Project": project, "X-Test-Roles": "member"}

	check := func(t *testing.T, where string, g map[string]any, stateful bool, revision float64) {
		t.Helper()
		if g["stateful"] != stateful {
			t.Errorf("%s: stateful = %v; want %v", where, g["stateful"], stateful)
		}
		if g["revision_number"] != revision {
			t.Errorf("%s: revision_number = %v; want %v", where, g["revision_number"], revision)
		}
		if g["shared"] != false {
			t.Errorf("%s: shared = %v; want false", where, g["shared"])
		}
		if tags, ok := g["tags"].([]any); !ok || len(tags) != 0 {
			t.Errorf("%s: tags = %#v; want []", where, g["tags"])
		}
	}
	decode := func(t *testing.T, body, key string) map[string]any {
		t.Helper()
		var out map[string]map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return out[key]
	}

	code, body := doJSONReq(r, http.MethodPost, "/v2.0/security-groups", `{"security_group":{"name":"spec033-fields"}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	created := decode(t, body, "security_group")
	id, _ := created["id"].(string)
	t.Cleanup(func() { mustExec(t, "DELETE FROM security_groups WHERE id = $1", id) })
	check(t, "create", created, true, 0)

	code, body = doReq(r, http.MethodGet, "/v2.0/security-groups/"+id, hdr)
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	check(t, "show", decode(t, body, "security_group"), true, 0)

	code, body = doReq(r, http.MethodGet, "/v2.0/security-groups?id="+id, hdr)
	if code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", code, body)
	}
	groups := securityGroupsFromBody(t, body)
	if len(groups) != 1 {
		t.Fatalf("list: %d groups; want 1", len(groups))
	}
	check(t, "list", groups[0], true, 0)

	code, body = doJSONReq(r, http.MethodPut, "/v2.0/security-groups/"+id, `{"security_group":{"description":"changed"}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("update: code=%d body=%s", code, body)
	}
	check(t, "update", decode(t, body, "security_group"), true, 1)
}

// ensuredDefault ensures project's default group, as a listing does, and removes the project's groups
// after the test.
func ensuredDefault(t *testing.T, project string) string {
	t.Helper()
	id, err := EnsureDefaultSecurityGroup(context.Background(), testPool, project)
	if err != nil {
		t.Fatalf("ensure the default group of %s: %v", project, err)
	}
	t.Cleanup(func() { mustExec(t, "DELETE FROM security_groups WHERE project_id = $1", project) })
	return id
}
