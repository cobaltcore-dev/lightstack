package keystone

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIdentityManagementAuthz_Gates pins the review-2026-10-13 fixes (C-1, C-2,
// M-5): the identity management surface — user updates, role assignment,
// project/role/group/domain/catalog management — must not honor a plain
// member. Before the fix every one of these answered 2xx to any authenticated
// caller, making PATCH /users/:id a passwordless account takeover and
// PUT .../roles/:role_id a one-request privilege escalation.
func TestIdentityManagementAuthz_Gates(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()

	const adminUID = "00000000-0000-0000-0000-000000000001" // seeded admin
	const projectID = "00000000-0000-0000-0000-000000000002"
	const memberUID = "00000000-0000-0000-0000-0000000000aa"
	member := map[string]string{"X-Test-Roles": "member", "X-Test-User": memberUID}

	// A valid target id, so these failures are the authz check and not the 404.
	takeover := `{"user":{"password":"attacker-controlled"}}`

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"member cannot reset another user's password (C-1)", "PATCH", "/v3/users/" + adminUID, takeover},
		{"member cannot disable another user (C-1)", "PATCH", "/v3/users/" + adminUID, `{"user":{"enabled":false}}`},
		{"member cannot self-grant a role (C-2)", "PUT", "/v3/projects/" + projectID + "/users/" + memberUID + "/roles/00000000-0000-0000-0000-000000000003", ""},
		{"member cannot revoke a role (C-2)", "DELETE", "/v3/projects/" + projectID + "/users/" + adminUID + "/roles/00000000-0000-0000-0000-000000000003", ""},
		{"member cannot create users", "POST", "/v3/users", `{"user":{"name":"x"}}`},
		{"member cannot enumerate users (M-5)", "GET", "/v3/users", ""},
		{"member cannot read another user's record (M-5)", "GET", "/v3/users/" + adminUID, ""},
		{"member cannot create projects", "POST", "/v3/projects", `{"project":{"name":"x"}}`},
		{"member cannot update projects", "PATCH", "/v3/projects/" + projectID, `{"project":{"name":"x"}}`},
		{"member cannot delete projects", "DELETE", "/v3/projects/" + projectID, ""},
		{"member cannot create roles", "POST", "/v3/roles", `{"role":{"name":"x"}}`},
		{"member cannot list role assignments (M-5)", "GET", "/v3/role_assignments", ""},
		{"member cannot create groups", "POST", "/v3/groups", `{"group":{"name":"x"}}`},
		{"member cannot list domains", "GET", "/v3/domains", ""},
		{"member cannot create services", "POST", "/v3/services", `{"service":{"name":"x","type":"compute"}}`},
		{"member cannot list endpoints", "GET", "/v3/endpoints", ""},
		{"member cannot mint credentials for another user (C-3)", "POST", "/v3/credentials", `{"credential":{"user_id":"` + adminUID + `","type":"ec2","blob":"{}"}}`},
		{"member cannot mint an application credential for another user (H-4)", "POST", "/v3/users/" + adminUID + "/application_credentials", `{"application_credential":{"name":"x"}}`},
		{"member cannot list another user's application credentials (H-4)", "GET", "/v3/users/" + adminUID + "/application_credentials", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			for k, v := range member {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s: code=%d body=%s; want 403", tc.method, tc.path, w.Code, w.Body.String())
			}
		})
	}
}

// TestIdentitySelfServiceStillWorks is the complement: the gates must not lock
// a member out of their own record — PATCH of one's own profile fields (name,
// email) stays allowed, while the admin-only fields on the same route
// (password, enabled) stay denied even for self.
func TestIdentitySelfServiceStillWorks(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()

	// A real member row — the authz pass must not lock a user out of their
	// own record.
	req := httptest.NewRequest("POST", "/v3/users", strings.NewReader(
		`{"user":{"name":"authz-self","password":"Scratch-1","domain_id":"default"}}`))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range adminHdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create scratch user: code=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.User.ID == "" {
		t.Fatalf("create scratch user: no id in %s", w.Body.String())
	}
	t.Cleanup(func() {
		req := httptest.NewRequest("DELETE", "/v3/users/"+created.User.ID, nil)
		for k, v := range adminHdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	})
	member := map[string]string{"X-Test-Roles": "member", "X-Test-User": created.User.ID}

	doPatch := func(body string) int {
		req := httptest.NewRequest("PATCH", "/v3/users/"+created.User.ID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range member {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	if code := doPatch(`{"user":{"email":"member@example.com"}}`); code != http.StatusOK {
		t.Errorf("self PATCH of profile fields: code=%d; want 200", code)
	}
	if code := doPatch(`{"user":{"password":"new-password"}}`); code != http.StatusForbidden {
		t.Errorf("self PATCH of password: code=%d; want 403 (use ChangePassword, which requires the original)", code)
	}
	if code := doPatch(`{"user":{"enabled":false}}`); code != http.StatusForbidden {
		t.Errorf("self PATCH of enabled: code=%d; want 403", code)
	}
}

// TestUpdateUserAdminPathUnchanged pins the admin side of the C-1 fix: an
// admin can still set password/enabled through PATCH /users/:id.
func TestUpdateUserAdminPathUnchanged(t *testing.T) {
	requireDB(t)
	r := newIdentityTestRouter()

	do := func(method, path, body string, hdr map[string]string) (*httptest.ResponseRecorder, string) {
		var reader *strings.Reader
		if body != "" {
			reader = strings.NewReader(body)
		} else {
			reader = strings.NewReader("")
		}
		req := httptest.NewRequest(method, path, reader)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w, w.Body.String()
	}

	// Create a scratch user as admin.
	w, body := do("POST", "/v3/users", `{"user":{"name":"authz-scratch","password":"Scratch-1","domain_id":"default"}}`, adminHdr)
	if w.Code != http.StatusCreated {
		t.Fatalf("create scratch user: code=%d body=%s", w.Code, body)
	}
	var created struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.User.ID == "" {
		t.Fatalf("create scratch user: no id in %s", body)
	}
	t.Cleanup(func() { do("DELETE", "/v3/users/"+created.User.ID, "", adminHdr) })

	// Admin PATCH with password + enabled must still succeed.
	w, body = do("PATCH", "/v3/users/"+created.User.ID,
		`{"user":{"password":"Scratch-2","enabled":true}}`, adminHdr)
	if w.Code != http.StatusOK {
		t.Errorf("admin PATCH password/enabled: code=%d body=%s; want 200", w.Code, body)
	}
}
