package nova

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Shared helpers for the server tests: requests and response bodies, keypairs, projects,
// security groups and their rules. The read-back tests name their rows with the
// `readback-` prefix (or use the fresh test project), and cleanupReadback removes them.

// freshProjectID is a throwaway project (created per-test) that has NO seeded
// `default` security group — used to prove neutron.EnsureDefaultSecurityGroup (FR-006a).
const freshProjectID = "00000000-0000-0000-0000-0000000000f0"

func createTestKeypair(t *testing.T, name string) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO keypairs (name, user_id, public_key) VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, name) DO NOTHING`,
		name, seededUserID, "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABreadback"); err != nil {
		t.Fatalf("createTestKeypair(%s): %v", name, err)
	}
}

func createTestProject(t *testing.T, id, name string) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO projects (id, name, description, enabled) VALUES ($1, $2, '', true)
		 ON CONFLICT (id) DO NOTHING`, id, name); err != nil {
		t.Fatalf("createTestProject(%s): %v", name, err)
	}
}

// securityGroupExists reports whether a named security group exists in a project.
func securityGroupExists(t *testing.T, name, projectID string) bool {
	t.Helper()
	var ok bool
	if err := testPool.QueryRow(t.Context(),
		"SELECT EXISTS(SELECT 1 FROM security_groups WHERE name = $1 AND project_id = $2)",
		name, projectID).Scan(&ok); err != nil {
		t.Fatalf("securityGroupExists(%s): %v", name, err)
	}
	return ok
}

// cleanupReadback removes the rows the read-back tests created. instances cascade to
// server_security_groups; deleting the fresh project cascades its instances and
// any ensured `default` security group.
func cleanupReadback(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	for _, q := range []string{
		"DELETE FROM instances WHERE name LIKE 'readback-%'",
		"DELETE FROM keypairs WHERE name LIKE 'readback-%'",
		"DELETE FROM security_groups WHERE name LIKE 'readback-%'",
	} {
		if _, err := testPool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanupReadback (%s): %v", q, err)
		}
	}
	// Last: instances and security_groups reference projects by FK.
	if _, err := testPool.Exec(ctx, "DELETE FROM projects WHERE id = $1", freshProjectID); err != nil {
		t.Fatalf("cleanupReadback (delete project %s): %v", freshProjectID, err)
	}
}

// serverBody unmarshals a {"server": {...}} response body.
func serverBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var env struct {
		Server map[string]any `json:"server"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse server body: %v; body=%s", err, body)
	}
	return env.Server
}

// serversBody unmarshals a {"servers": [...]} response body.
func serversBody(t *testing.T, body string) []map[string]any {
	t.Helper()
	var env struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse servers body: %v; body=%s", err, body)
	}
	return env.Servers
}

// createServerAs posts a create request scoped to the given project/user.
func createServerAs(t *testing.T, r *gin.Engine, projectID, userID, bodyJSON string) (int, string) {
	t.Helper()
	return doReq(r, http.MethodPost, "/v2.1/servers", bodyJSON, testAuthHdr(projectID, userID))
}

// withImage gives a server-create body the boot source every create must name: an imageRef to
// an image seeded for projectID and removed when the test ends.
func withImage(t *testing.T, projectID, bodyJSON string) string {
	t.Helper()
	const head = `{"server":{`
	if !strings.HasPrefix(bodyJSON, head) {
		t.Fatalf("withImage: the body does not start with %s: %s", head, bodyJSON)
	}
	return fmt.Sprintf(`%s"imageRef":%q,%s`, head, seedImageFor(t, projectID), bodyJSON[len(head):])
}

// instanceCount returns how many instances exist with the given name in a project.
func instanceCount(t *testing.T, name, projectID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(),
		"SELECT count(*) FROM instances WHERE name = $1 AND project_id = $2", name, projectID).Scan(&n); err != nil {
		t.Fatalf("instanceCount: %v", err)
	}
	return n
}

// sgNamesFromResponse extracts the names from a server response's security_groups
// field (shape [{"name": ...}]).
func sgNamesFromResponse(t *testing.T, sg any) []string {
	t.Helper()
	list, ok := sg.([]any)
	if !ok {
		t.Fatalf("security_groups is not a list: %#v", sg)
	}
	var names []string
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("security_groups entry is not an object: %#v", e)
		}
		n, _ := m["name"].(string)
		names = append(names, n)
	}
	return names
}

// serverSecurityGroupCount returns how many security groups are persisted for a server.
func serverSecurityGroupCount(t *testing.T, serverID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(),
		"SELECT count(*) FROM server_security_groups WHERE server_id = $1", serverID,
	).Scan(&n); err != nil {
		t.Fatalf("count server_security_groups: %v", err)
	}
	return n
}

// ruleCountForSG returns how many rules a security group currently has.
func ruleCountForSG(t *testing.T, sgID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(),
		"SELECT count(*) FROM security_group_rules WHERE security_group_id = $1", sgID,
	).Scan(&n); err != nil {
		t.Fatalf("count rules: %v", err)
	}
	return n
}

// sgRuleShapes returns a security group's rules as sorted
// "direction/ethertype/protocol/portMin-portMax" strings, so a test can assert the
// exact rule set rather than just how many rules there are.
func sgRuleShapes(t *testing.T, sgID string) []string {
	t.Helper()
	rows, err := testPool.Query(t.Context(),
		`SELECT direction, ethertype, protocol, port_range_min, port_range_max
		 FROM security_group_rules WHERE security_group_id = $1`, sgID)
	if err != nil {
		t.Fatalf("query rules: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var direction, ethertype string
		var protocol sql.NullString
		var portMin, portMax sql.NullInt32
		if err := rows.Scan(&direction, &ethertype, &protocol, &portMin, &portMax); err != nil {
			t.Fatalf("scan rule: %v", err)
		}
		out = append(out, fmt.Sprintf("%s/%s/%s/%d-%d",
			direction, ethertype, protocol.String, portMin.Int32, portMax.Int32))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate rules: %v", err)
	}
	slices.Sort(out)
	return out
}
