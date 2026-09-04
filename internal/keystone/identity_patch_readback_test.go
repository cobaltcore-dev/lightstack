package keystone

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// patchJSON issues an authenticated PATCH and returns the status and decoded body.
func patchJSON(t *testing.T, r http.Handler, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("PATCH", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range adminHdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var decoded map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("PATCH %s: body is not JSON (%v): %s", path, err, w.Body.String())
	}
	return w.Code, decoded
}

// TestUpdateProjectPartialPatchEchoesUntouchedFields pins the PATCH 200 path that
// the not-found tests cannot reach: a partial update must still return the whole
// project, including the columns the request did not mention. Upstream Keystone's
// project-update-response.json returns the full resource, so answering with only
// the changed attributes would break Horizon/Terraform read-back. This is the
// regression guard for the `UPDATE ... RETURNING` rewrite (PR #74 review, H1).
func TestUpdateProjectPartialPatchEchoesUntouchedFields(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newIdentityTestRouter()

	const id = "00000000-0000-0000-0000-0000000b5150"
	t.Cleanup(func() {
		if _, err := database.DB.Exec(ctx, "DELETE FROM projects WHERE id = $1", id); err != nil {
			t.Errorf("cleanup project %s: %v", id, err)
		}
	})
	if _, err := database.DB.Exec(ctx,
		`INSERT INTO projects (id, name, description, domain_id, enabled)
		 VALUES ($1, 'b515-readback', 'seeded description', $2, false)
		 ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description,
		     domain_id = EXCLUDED.domain_id, enabled = EXCLUDED.enabled`,
		id, defaultDomainID); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// Only `name` is sent; description / domain_id / enabled must survive untouched.
	code, body := patchJSON(t, r, "/v3/projects/"+id, `{"project":{"name":"b515-renamed"}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH /v3/projects/%s: code=%d body=%v; want 200", id, code, body)
	}

	project, ok := body["project"].(map[string]any)
	if !ok {
		t.Fatalf("response has no project object: %v", body)
	}
	for field, want := range map[string]any{
		"id":          id,
		"name":        "b515-renamed",       // changed by the PATCH
		"description": "seeded description", // untouched — must still be echoed
		"domain_id":   defaultDomainID,      // untouched
		"enabled":     false,                // untouched, and false ⇒ not just a zero-value pass
	} {
		if got := project[field]; got != want {
			t.Errorf("project[%q] = %#v, want %#v", field, got, want)
		}
	}

	// The rename must be durable, not just reflected in the response.
	var persisted string
	if err := database.DB.QueryRow(ctx, "SELECT name FROM projects WHERE id = $1", id).Scan(&persisted); err != nil {
		t.Fatalf("read back project name: %v", err)
	}
	if persisted != "b515-renamed" {
		t.Errorf("persisted name = %q, want %q", persisted, "b515-renamed")
	}
}

// TestUpdateRolePatchReturnsUpdatedName is the role counterpart: the 200 path must
// report the new name and persist it (PR #74 review, H1).
func TestUpdateRolePatchReturnsUpdatedName(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newIdentityTestRouter()

	const id = "00000000-0000-0000-0000-0000000b5151"
	t.Cleanup(func() {
		if _, err := database.DB.Exec(ctx, "DELETE FROM roles WHERE id = $1", id); err != nil {
			t.Errorf("cleanup role %s: %v", id, err)
		}
	})
	if _, err := database.DB.Exec(ctx,
		`INSERT INTO roles (id, name) VALUES ($1, 'b515-role')
		 ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name`, id); err != nil {
		t.Fatalf("seed role: %v", err)
	}

	code, body := patchJSON(t, r, "/v3/roles/"+id, `{"role":{"name":"b515-role-renamed"}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH /v3/roles/%s: code=%d body=%v; want 200", id, code, body)
	}

	role, ok := body["role"].(map[string]any)
	if !ok {
		t.Fatalf("response has no role object: %v", body)
	}
	if role["id"] != id || role["name"] != "b515-role-renamed" {
		t.Errorf("role = %#v; want id=%q name=%q", role, id, "b515-role-renamed")
	}

	var persisted string
	if err := database.DB.QueryRow(ctx, "SELECT name FROM roles WHERE id = $1", id).Scan(&persisted); err != nil {
		t.Fatalf("read back role name: %v", err)
	}
	if persisted != "b515-role-renamed" {
		t.Errorf("persisted name = %q, want %q", persisted, "b515-role-renamed")
	}
}
