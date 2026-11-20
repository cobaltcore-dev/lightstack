package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
)

// SPEC-033 round 2, T078 (FR-024–FR-026; research R39, R53): `default` stays the project's default, as
// upstream's does. A non-administrator cannot delete it; nobody renames it; no second group takes its
// name, in any case. Other names may repeat, and a name two groups share resolves to neither in the
// group's path, where a client then lists by name.

func ruleShapesOf(t *testing.T, groupID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT direction, ethertype, COALESCE(remote_group_id::text, ''), COALESCE(remote_ip_prefix, '')
		  FROM security_group_rules WHERE security_group_id = $1`, groupID)
	if err != nil {
		t.Fatalf("read rules of %s: %v", groupID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var dir, ether, remote, prefix string
		if err := rows.Scan(&dir, &ether, &remote, &prefix); err != nil {
			t.Fatal(err)
		}
		if remote == groupID {
			remote = "self"
		}
		out = append(out, dir+"/"+ether+"/"+remote+"/"+prefix)
	}
	sort.Strings(out)
	return out
}

func ensureDefault(t *testing.T, projectID string) string {
	t.Helper()
	id, err := EnsureDefaultSecurityGroup(context.Background(), testPool, projectID)
	if err != nil {
		t.Fatalf("ensure default: %v", err)
	}
	return id
}

func TestDefaultSecurityGroupIsKept(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	t.Run("a member cannot delete an unused default", func(t *testing.T) {
		project := createTestProject(t, "spec033-def-del")
		def := ensureDefault(t, project)
		rules := ruleShapesOf(t, def)
		code, body := doJSONReq(r, http.MethodDelete, "/v2.0/security-groups/"+def, "",
			map[string]string{"X-Test-Project": project, "X-Test-Roles": "member"})
		if code != http.StatusConflict {
			t.Fatalf("member delete of default: code=%d body=%s; want 409", code, body)
		}
		if typ := neutronErrorType(t, body); typ != "SecurityGroupCannotRemoveDefault" {
			t.Errorf("type = %q; want SecurityGroupCannotRemoveDefault", typ)
		}
		if !groupExists(t, def) {
			t.Fatal("the refused delete removed default")
		}
		if got := ruleShapesOf(t, def); !equalIDs(got, rules) {
			t.Errorf("default's rules after the refusal: %v; want %v", got, rules)
		}
	})

	t.Run("an administrator may delete an unused default; the next use makes a new one", func(t *testing.T) {
		project := createTestProject(t, "spec033-def-admin")
		def := ensureDefault(t, project)
		code, body := doJSONReq(r, http.MethodDelete, "/v2.0/security-groups/"+def, "",
			map[string]string{"X-Test-Project": project, "X-Test-Roles": "admin"})
		if code != http.StatusNoContent {
			t.Fatalf("administrator delete of default: code=%d body=%s; want 204", code, body)
		}
		again := ensureDefault(t, project)
		if again == def {
			t.Fatal("EnsureDefaultSecurityGroup returned the deleted group's id")
		}
		want := []string{"egress/IPv4//", "egress/IPv6//", "ingress/IPv4/self/", "ingress/IPv6/self/"}
		if got := ruleShapesOf(t, again); !equalIDs(got, want) {
			t.Errorf("the new default's rules: %v; want %v (FR-043)", got, want)
		}
	})

	t.Run("an in-use default is in use, for everyone", func(t *testing.T) {
		project := createTestProject(t, "spec033-def-inuse")
		def := ensureDefault(t, project)
		net := seedNetwork(t, "spec033-def-net")
		sub := seedSubnet(t, net, "spec033-def-subnet", "10.53.0.0/24", nil, 4, true)
		port, _ := seedPortWithIP(t, net, sub)
		mustExec(t, "DELETE FROM port_security_groups WHERE port_id = $1", port)
		sgTestAttach(t, port, def)
		for _, role := range []string{"member", "admin"} {
			code, body := doJSONReq(r, http.MethodDelete, "/v2.0/security-groups/"+def, "",
				map[string]string{"X-Test-Project": project, "X-Test-Roles": role})
			if code != http.StatusConflict {
				t.Fatalf("%s delete of an in-use default: code=%d body=%s; want 409", role, code, body)
			}
			if typ := neutronErrorType(t, body); typ != "SecurityGroupInUse" {
				t.Errorf("%s: type = %q; want SecurityGroupInUse — in use is checked first, as upstream", role, typ)
			}
		}
	})

	t.Run("default keeps its name", func(t *testing.T) {
		project := createTestProject(t, "spec033-def-name")
		def := ensureDefault(t, project)
		hdr := map[string]string{"X-Test-Project": project, "X-Test-Roles": "admin"}
		for _, tc := range []struct {
			body, wantType string
		}{
			{`{"security_group":{"name":"web"}}`, "SecurityGroupCannotUpdateDefault"},
			{`{"security_group":{"name":"Default"}}`, "SecurityGroupDefaultAlreadyExists"},
			{`{"security_group":{"name":"default"}}`, "SecurityGroupDefaultAlreadyExists"},
		} {
			code, body := doJSONReq(r, http.MethodPut, "/v2.0/security-groups/"+def, tc.body, hdr)
			if code != http.StatusConflict {
				t.Fatalf("PUT default %s: code=%d body=%s; want 409", tc.body, code, body)
			}
			if typ := neutronErrorType(t, body); typ != tc.wantType {
				t.Errorf("PUT default %s: type = %q; want %s", tc.body, typ, tc.wantType)
			}
		}
		var name string
		if err := testPool.QueryRow(context.Background(), "SELECT name FROM security_groups WHERE id = $1", def).Scan(&name); err != nil || name != "default" {
			t.Fatalf("default's name after the refusals: %q (%v)", name, err)
		}
		if code, body := doJSONReq(r, http.MethodPut, "/v2.0/security-groups/"+def, `{"security_group":{"description":"still default"}}`, hdr); code != http.StatusOK {
			t.Errorf("PUT default's description: code=%d body=%s; want 200", code, body)
		}
	})

	t.Run("no second group takes the name", func(t *testing.T) {
		project := createTestProject(t, "spec033-def-dup")
		ensureDefault(t, project)
		hdr := map[string]string{"X-Test-Project": project, "X-Test-Roles": "member"}
		for _, name := range []string{"default", "DEFAULT"} {
			code, body := doJSONReq(r, http.MethodPost, "/v2.0/security-groups", `{"security_group":{"name":"`+name+`"}}`, hdr)
			if code != http.StatusConflict {
				t.Fatalf("POST %q: code=%d body=%s; want 409", name, code, body)
			}
			if typ := neutronErrorType(t, body); typ != "SecurityGroupDefaultAlreadyExists" {
				t.Errorf("POST %q: type = %q; want SecurityGroupDefaultAlreadyExists", name, typ)
			}
		}
		other := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-other"}}`)["id"].(string)
		code, body := doJSONReq(r, http.MethodPut, "/v2.0/security-groups/"+other, `{"security_group":{"name":"default"}}`, hdr)
		if code != http.StatusConflict {
			t.Fatalf("rename another group to default: code=%d body=%s; want 409", code, body)
		}
		if typ := neutronErrorType(t, body); typ != "SecurityGroupDefaultAlreadyExists" {
			t.Errorf("rename to default: type = %q; want SecurityGroupDefaultAlreadyExists", typ)
		}
	})
}

func TestSecurityGroupNamesMayRepeat(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	project := createTestProject(t, "spec033-dupnames")
	hdr := map[string]string{"X-Test-Project": project, "X-Test-Roles": "member"}

	first := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"web"}}`)["id"].(string)
	second := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"web"}}`)["id"].(string)
	third := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"app"}}`)["id"].(string)
	if code, body := doJSONReq(r, http.MethodPut, "/v2.0/security-groups/"+third, `{"security_group":{"name":"web"}}`, hdr); code != http.StatusOK {
		t.Fatalf("rename to a name in use: code=%d body=%s; want 200 (FR-026)", code, body)
	}

	code, body := doReq(r, http.MethodGet, "/v2.0/security-groups?name=web", hdr)
	if code != http.StatusOK {
		t.Fatalf("?name=web: code=%d body=%s", code, body)
	}
	var out struct {
		Groups []struct {
			ID string `json:"id"`
		} `json:"security_groups"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range out.Groups {
		got = append(got, g.ID)
	}
	if want := sorted(first, second, third); !equalIDs(sorted(got...), want) {
		t.Errorf("?name=web lists %v; want all three %v", got, want)
	}

	code, body = doReq(r, http.MethodGet, "/v2.0/security-groups/web", hdr)
	if code != http.StatusNotFound {
		t.Errorf("GET /v2.0/security-groups/web with three matches: code=%d body=%s; want 404", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/security-groups/"+second, hdr); code != http.StatusOK {
		t.Errorf("GET by id: code=%d body=%s; want 200", code, body)
	}
}
