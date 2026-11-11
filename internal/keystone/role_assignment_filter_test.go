package keystone

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ListRoleAssignments filters by id through eqID, so a dash-free scope.project.id — the
// form rgw_compat_tenant_ids advertises, which a client sends back — matches the
// assignment on SQLite (ids are text), where the previous raw filter returned an empty
// list (SRC-9).
func TestRoleAssignmentFilterCanonicalisesProjectID(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	ctx := context.Background()
	project, user, role := uuid.NewString(), uuid.NewString(), uuid.NewString()
	seed := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO projects (id, name) VALUES ($1, $2)`, []any{project, "spec028-ra-" + project[:8]}},
		{`INSERT INTO users (id, name, password_hash) VALUES ($1, $2, 'x')`, []any{user, "spec028-ra-" + user[:8]}},
		{`INSERT INTO roles (id, name) VALUES ($1, $2)`, []any{role, "spec028-ra-" + role[:8]}},
		{`INSERT INTO role_assignments (user_id, project_id, role_id) VALUES ($1, $2, $3)`, []any{user, project, role}},
	}
	for _, s := range seed {
		if _, err := testPool.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed %q: %v", s.sql, err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM role_assignments WHERE project_id = $1`, project)
		testPool.Exec(context.Background(), `DELETE FROM roles WHERE id = $1`, role)
		testPool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user)
		testPool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, project)
	})

	r := newIdentityTestRouter()
	dashFree := strings.ReplaceAll(project, "-", "")
	code, body := doReq(r, http.MethodGet, "/v3/role_assignments?scope.project.id="+dashFree, adminHdr)
	if code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s; want 200", code, body)
	}
	if !strings.Contains(body, role) {
		t.Errorf("role assignment for the dash-free project id not found in %s", body)
	}
}
