package keystone

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// postEC2Cred drives CreateEC2Credential with a fully-populated auth context and
// returns the recorded response. roles feeds hasRole; user_id/project_id feed the
// self-or-admin guard and the self-service project fallback. The handler reads no
// Service fields, so a zero-value Service suffices.
func postEC2Cred(t *testing.T, targetUserID, callerUserID, callerProjectID string, roles []string, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost,
		"/v3/users/"+targetUserID+"/credentials/OS-EC2", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: targetUserID}}
	c.Set("user_id", callerUserID)
	c.Set("project_id", callerProjectID)
	c.Set("roles", roles)
	(&Service{pool: testPool}).CreateEC2Credential(c)
	return w
}

// ec2ErrMessage extracts the message from Lightstack's Nova-style error envelope
// ({"<code>":{"message":...}}), so the test can assert the exact text.
func ec2ErrMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env map[string]struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	for _, v := range env {
		return v.Message
	}
	return ""
}

// PR #73 review (r3558650928): an admin-supplied tenant_id that does not name an
// existing project must yield Keystone's 404 ("Could not find project: <id>."),
// not the 500 the credentials.project_id foreign key would otherwise produce.
// Verified against real Keystone (DevStack): POST .../credentials/OS-EC2 with a
// bogus tenant_id returns 404 ProjectNotFound with exactly that message.
func TestCreateEC2Credential_ProjectExistence(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	var adminID, projID string
	if err := testPool.QueryRow(ctx, "SELECT id FROM users WHERE name = 'admin'").Scan(&adminID); err != nil {
		t.Fatalf("seeded admin user: %v", err)
	}
	if err := testPool.QueryRow(ctx, "SELECT id FROM projects LIMIT 1").Scan(&projID); err != nil {
		t.Fatalf("seeded project: %v", err)
	}
	admin := []string{"admin"}

	// 1. non-existent but well-formed UUID -> 404 with Keystone's exact message.
	const ghost = "00000000-0000-0000-0000-0000000000ff"
	w := postEC2Cred(t, adminID, adminID, projID, admin, `{"tenant_id":"`+ghost+`"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("ghost tenant_id: status = %d, want 404\nbody: %s", w.Code, w.Body.String())
	}
	if got, want := ec2ErrMessage(t, w), "Could not find project: "+ghost+"."; got != want {
		t.Fatalf("ghost tenant_id message = %q, want Keystone's %q", got, want)
	}

	// 2. malformed id -> also 404 (Keystone treats any non-match as not found),
	//    never the raw 22P02 -> 500 that a bare `id = $1` cast would give.
	w = postEC2Cred(t, adminID, adminID, projID, admin, `{"tenant_id":"not-a-uuid"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("malformed tenant_id: status = %d, want 404\nbody: %s", w.Code, w.Body.String())
	}
	if got, want := ec2ErrMessage(t, w), "Could not find project: not-a-uuid."; got != want {
		t.Fatalf("malformed tenant_id message = %q, want %q", got, want)
	}

	// 3. a real project -> 201 (and clean up the row we create).
	w = postEC2Cred(t, adminID, adminID, projID, admin, `{"tenant_id":"`+projID+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("valid tenant_id: status = %d, want 201\nbody: %s", w.Code, w.Body.String())
	}
	var created struct {
		Credential struct {
			Access string `json:"access"`
		} `json:"credential"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err == nil && created.Credential.Access != "" {
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(),
				"DELETE FROM credentials WHERE access_key = $1", created.Credential.Access)
		})
	}
}
