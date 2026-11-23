package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T193 (SC-016; FR-019a): upstream's get_security_groups, get_security_group_rules
// and create_security_group ensure the project's default group, so a project with no port yet shows it
// and a client finds it by name (Terraform's openstack_networking_secgroup_v2 data source, `openstack
// security group rule create … default`). An administrator's project_id names the project, a project
// that does not exist has none, and a listing once it exists writes nothing.

func TestListingsEnsureTheProjectsDefault(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	fresh := func(t *testing.T, name string) string {
		project := createTestProject(t, name)
		t.Cleanup(func() { mustExec(t, "DELETE FROM security_groups WHERE project_id = $1", project) })
		return project
	}
	defaults := func(t *testing.T, project string) []string {
		t.Helper()
		ids, err := queryStrings(context.Background(), testPool,
			"SELECT id::text FROM security_groups WHERE project_id = $1 AND name = 'default'", project)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	listed := func(t *testing.T, body, key string) []string {
		t.Helper()
		var out map[string][]struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			GroupID string `json:"security_group_id"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		var ids []string
		for _, item := range out[key] {
			if key == "security_group_rules" {
				ids = append(ids, item.GroupID)
			} else if item.Name == "default" {
				ids = append(ids, item.ID)
			}
		}
		return ids
	}

	t.Run("a member's group listing", func(t *testing.T) {
		project := fresh(t, "spec033-src21-groups")
		for pass := 1; pass <= 2; pass++ {
			code, body := doReq(r, http.MethodGet, "/v2.0/security-groups?name=default", projectHdr(project))
			got, stored := listed(t, body, "security_groups"), defaults(t, project)
			if code != http.StatusOK || len(got) != 1 || len(stored) != 1 || got[0] != stored[0] {
				t.Errorf("listing %d: code=%d listed %v, stored %v; want the one default, listed", pass, code, got, stored)
			}
		}
	})

	t.Run("a member's rule listing", func(t *testing.T) {
		project := fresh(t, "spec033-src21-rules")
		code, body := doReq(r, http.MethodGet, "/v2.0/security-group-rules", projectHdr(project))
		stored := defaults(t, project)
		if code != http.StatusOK || len(stored) != 1 {
			t.Fatalf("code=%d, defaults %v; want 200 and one default", code, stored)
		}
		n := 0
		for _, group := range listed(t, body, "security_group_rules") {
			if group == stored[0] {
				n++
			}
		}
		if n != 4 {
			t.Errorf("%d of the default's rules listed; want its 4", n)
		}
	})

	t.Run("a create", func(t *testing.T) {
		project := fresh(t, "spec033-src21-create")
		code, body := doJSONReq(r, http.MethodPost, "/v2.0/security-groups", `{"security_group":{"name":"web"}}`, projectHdr(project))
		if code != http.StatusCreated || len(defaults(t, project)) != 1 {
			t.Errorf("create: code=%d body=%s, defaults %v; want 201 and the project's default", code, body, defaults(t, project))
		}
	})

	t.Run("an administrator's project_id", func(t *testing.T) {
		project := fresh(t, "spec033-src21-admin")
		admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
		code, body := doReq(r, http.MethodGet, "/v2.0/security-groups?project_id="+project, admin)
		got, stored := listed(t, body, "security_groups"), defaults(t, project)
		if code != http.StatusOK || len(got) != 1 || len(stored) != 1 || got[0] != stored[0] {
			t.Errorf("code=%d listed %v, stored %v; want the named project's default", code, got, stored)
		}
		missing := uuid.NewString()
		if code, body := doReq(r, http.MethodGet, "/v2.0/security-groups?project_id="+missing, admin); code != http.StatusOK || len(listed(t, body, "security_groups")) != 0 {
			t.Errorf("a project that does not exist: code=%d body=%s; want 200 and nothing", code, body)
		}
	})
}
