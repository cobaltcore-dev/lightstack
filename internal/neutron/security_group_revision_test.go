package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// SPEC-033 round 2, T073 (FR-050; research R46): a security group carries upstream's revision number.
// It is 0 when created, one more on every update of the group and every rule added to or deleted from
// it, and an undone change puts it back, compare-and-set, with the rest of the change. PUT and DELETE
// honour If-Match: revision_number=<n>, answering 412 RevisionNumberConstraintFailed otherwise.

func groupRevision(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		"SELECT revision_number FROM security_groups WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatalf("read revision_number of %s: %v", id, err)
	}
	return n
}

// doIfMatch sends a JSON request with an If-Match header.
func doIfMatch(r http.Handler, method, path, body, ifMatch string, hdr map[string]string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", ifMatch)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func neutronErrorType(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		NeutronError struct {
			Type string `json:"type"`
		} `json:"NeutronError"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return out.NeutronError.Type
}

// createGroupViaAPI creates a group through POST /v2.0/security-groups and removes it in cleanup.
func createGroupViaAPI(t *testing.T, r *gin.Engine, hdr map[string]string, body string) map[string]any {
	t.Helper()
	code, resp := doJSONReq(r, http.MethodPost, "/v2.0/security-groups", body, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create group %s: code=%d body=%s", body, code, resp)
	}
	var out struct {
		Group map[string]any `json:"security_group"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("decode %s: %v", resp, err)
	}
	id, _ := out.Group["id"].(string)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM security_groups WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
	})
	return out.Group
}

func TestSecurityGroupRevisionNumber(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	project := createTestProject(t, "spec033-rev")
	hdr := map[string]string{"X-Test-Project": project, "X-Test-Roles": "member"}

	t.Run("0 at creation, one more per update and per rule added or deleted", func(t *testing.T) {
		g := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-rev"}}`)
		id := g["id"].(string)
		if got := groupRevision(t, id); got != 0 {
			t.Fatalf("revision after create = %d; want 0 (the starting rules do not count)", got)
		}
		if code, body := doJSONReq(r, http.MethodPut, "/v2.0/security-groups/"+id, `{"security_group":{"description":"one"}}`, hdr); code != http.StatusOK {
			t.Fatalf("update: code=%d body=%s", code, body)
		}
		if got := groupRevision(t, id); got != 1 {
			t.Fatalf("revision after one update = %d; want 1", got)
		}
		code, body := doJSONReq(r, http.MethodPost, "/v2.0/security-group-rules",
			`{"security_group_rule":{"security_group_id":"`+id+`","direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":22,"port_range_max":22}}`, hdr)
		if code != http.StatusCreated {
			t.Fatalf("add rule: code=%d body=%s", code, body)
		}
		if got := groupRevision(t, id); got != 2 {
			t.Fatalf("revision after a rule add = %d; want 2", got)
		}
		var rule struct {
			Rule struct {
				ID string `json:"id"`
			} `json:"security_group_rule"`
		}
		_ = json.Unmarshal([]byte(body), &rule)
		if code, body := doJSONReq(r, http.MethodDelete, "/v2.0/security-group-rules/"+rule.Rule.ID, "", hdr); code != http.StatusNoContent {
			t.Fatalf("delete rule: code=%d body=%s", code, body)
		}
		if got := groupRevision(t, id); got != 3 {
			t.Fatalf("revision after a rule delete = %d; want 3", got)
		}

		code, body = doReq(r, http.MethodGet, "/v2.0/security-groups?revision_number=3&id="+id, hdr)
		if code != http.StatusOK || !strings.Contains(body, id) {
			t.Errorf("?revision_number=3: code=%d body=%s; want the group listed by its stored revision", code, body)
		}
		code, body = doReq(r, http.MethodGet, "/v2.0/security-groups?revision_number=2&id="+id, hdr)
		if code != http.StatusOK || strings.Contains(body, id) {
			t.Errorf("?revision_number=2: code=%d body=%s; want the group left out", code, body)
		}
	})

	t.Run("If-Match on update", func(t *testing.T) {
		g := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-ifmatch"}}`)
		id := g["id"].(string)
		code, body := doIfMatch(r, http.MethodPut, "/v2.0/security-groups/"+id, `{"security_group":{"description":"stale"}}`, "revision_number=7", hdr)
		if code != http.StatusPreconditionFailed {
			t.Fatalf("update with a stale revision: code=%d body=%s; want 412", code, body)
		}
		if typ := neutronErrorType(t, body); typ != "RevisionNumberConstraintFailed" {
			t.Errorf("type = %q; want RevisionNumberConstraintFailed", typ)
		}
		var desc string
		if err := testPool.QueryRow(context.Background(), "SELECT COALESCE(description, '') FROM security_groups WHERE id = $1", id).Scan(&desc); err != nil {
			t.Fatal(err)
		}
		if desc == "stale" || groupRevision(t, id) != 0 {
			t.Errorf("a refused update changed the group: description %q, revision %d", desc, groupRevision(t, id))
		}
		if code, body := doIfMatch(r, http.MethodPut, "/v2.0/security-groups/"+id, `{"security_group":{"description":"fresh"}}`, "revision_number=0", hdr); code != http.StatusOK {
			t.Errorf("update with the current revision: code=%d body=%s; want 200", code, body)
		}
	})

	t.Run("If-Match on delete", func(t *testing.T) {
		g := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-ifmatch-del"}}`)
		id := g["id"].(string)
		code, body := doIfMatch(r, http.MethodDelete, "/v2.0/security-groups/"+id, "", "revision_number=4", hdr)
		if code != http.StatusPreconditionFailed {
			t.Fatalf("delete with a stale revision: code=%d body=%s; want 412", code, body)
		}
		if !groupExists(t, id) {
			t.Fatal("a refused delete removed the group")
		}
		if code, body := doIfMatch(r, http.MethodDelete, "/v2.0/security-groups/"+id, "", "revision_number=0", hdr); code != http.StatusNoContent {
			t.Errorf("delete with the current revision: code=%d body=%s; want 204", code, body)
		}
	})

	t.Run("the extension list advertises stateful-security-group", func(t *testing.T) {
		code, body := doReq(r, http.MethodGet, "/v2.0/extensions", hdr)
		if code != http.StatusOK {
			t.Fatalf("extensions: code=%d body=%s", code, body)
		}
		if !strings.Contains(body, `"alias":"stateful-security-group"`) {
			t.Errorf("extensions do not list stateful-security-group: %s", body)
		}
	})
}

// T073: a rule change the node cannot apply is undone with its revision, compare-and-set.
func TestSecurityGroupRevisionUndone(t *testing.T) {
	requireDB(t)

	t.Run("a failed add leaves the revision where it was", func(t *testing.T) {
		rf := newRuleFixture(t)
		before := groupRevision(t, rf.web)
		rf.failFirstFilter()
		if code, body, _ := rf.addRule(t); code != http.StatusInternalServerError {
			t.Fatalf("add: code=%d body=%s; want 500", code, body)
		}
		if got := groupRevision(t, rf.web); got != before {
			t.Errorf("revision after an undone add = %d; want %d", got, before)
		}
	})

	t.Run("a failed delete leaves the revision where it was", func(t *testing.T) {
		rf := newRuleFixture(t)
		code, body, id := rf.addRule(t)
		if code != http.StatusCreated {
			t.Fatalf("add: code=%d body=%s", code, body)
		}
		before := groupRevision(t, rf.web)
		rf.failFirstFilter()
		if code, body := doJSONReq(rf.fx.r, http.MethodDelete, "/v2.0/security-group-rules/"+id, "", rf.fx.hdr); code != http.StatusInternalServerError {
			t.Fatalf("delete: code=%d body=%s; want 500", code, body)
		}
		if got := groupRevision(t, rf.web); got != before {
			t.Errorf("revision after an undone delete = %d; want %d", got, before)
		}
	})

	t.Run("the undo keeps a revision another request moved on", func(t *testing.T) {
		rf := newRuleFixture(t)
		before := groupRevision(t, rf.web)
		// The filter rebuild fails; before it does, another request updates the group.
		rf.fx.fake.setHook(func(c dataplaneCall) error {
			if c.Method == "EnsureIngressFilter" {
				mustExec(t, "UPDATE security_groups SET revision_number = revision_number + 1, description = 'moved on' WHERE id = $1", rf.web)
				rf.fx.fake.setHook(nil)
				return errNode
			}
			return nil
		})
		if code, body, _ := rf.addRule(t); code != http.StatusInternalServerError {
			t.Fatalf("add: code=%d body=%s; want 500", code, body)
		}
		if got := groupRevision(t, rf.web); got != before+2 {
			t.Errorf("revision = %d; want %d: the other request's bump kept, the undone add's not taken back over it", got, before+2)
		}
	})
}
