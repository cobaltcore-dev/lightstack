package keystone

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- exercising the SigV2 path, which mandates HMAC-SHA1
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The s3tokens/ec2tokens endpoints must fail closed for disabled identities,
// like every other identity path in this package (password issue, token
// re-scope, GET /v3/auth/tokens validation). The EC2 credential and its role
// assignments SURVIVE a disable, so a correctly-signed request from a disabled
// identity must not keep minting fresh envelopes — S3 access would outlive the
// disable indefinitely, with no JWT expiry to bound it.

const (
	s3DisabledUserID    = "deadbeef-0000-4000-8000-000000543105"
	s3DisabledProjectID = "deadbeef-0000-4000-8000-0000006b7d19"
)

// seedS3DisabledIdentity installs an enabled user with a member role and an
// EC2 credential (valid secret, fixed access key) scoped to a dedicated
// project, so the tests below can flip `enabled` without touching state any
// other test relies on. Everything is removed again on cleanup.
func seedS3DisabledIdentity(t *testing.T) (access, secret string) {
	t.Helper()
	requireDB(t)
	seedRGWCompatFixtures(t)
	ctx := context.Background()

	access = "AKS3DISABLEDTESTKEY01"
	secret = "s3-disabled-test-secret"
	blob, err := json.Marshal(ec2Blob{
		Access:   access,
		Secret:   secret,
		UserID:   s3DisabledUserID,
		TenantID: s3DisabledProjectID,
	})
	if err != nil {
		t.Fatalf("marshal ec2 blob: %v", err)
	}

	if _, err := testPool.Exec(ctx, `
		INSERT INTO users (id, name, password_hash, enabled, domain_id)
		VALUES ($1, 'spec007-s3disabled', 'not-a-hash', true, $2)
		ON CONFLICT (id) DO NOTHING`, s3DisabledUserID, defaultDomainID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO projects (id, name, description, enabled, domain_id)
		VALUES ($1, 'spec007-s3disabled-project', 's3tokens disabled-identity test fixture', true, $2)
		ON CONFLICT (id) DO NOTHING`, s3DisabledProjectID, defaultDomainID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO role_assignments (user_id, project_id, role_id)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, s3DisabledUserID, s3DisabledProjectID, rgwCompatMemberRoleID); err != nil {
		t.Fatalf("seed role assignment: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO credentials (user_id, project_id, type, blob, access_key)
		VALUES ($1, $2, 'ec2', $3, $4)`, s3DisabledUserID, s3DisabledProjectID, string(blob), access); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, "DELETE FROM credentials WHERE user_id = $1", s3DisabledUserID); err != nil {
			t.Errorf("cleanup credential: %v", err)
		}
		if _, err := testPool.Exec(ctx, "DELETE FROM role_assignments WHERE user_id = $1", s3DisabledUserID); err != nil {
			t.Errorf("cleanup role assignments: %v", err)
		}
		if _, err := testPool.Exec(ctx, "DELETE FROM users WHERE id = $1", s3DisabledUserID); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
		if _, err := testPool.Exec(ctx, "DELETE FROM projects WHERE id = $1", s3DisabledProjectID); err != nil {
			t.Errorf("cleanup project: %v", err)
		}
	})
	return access, secret
}

// signedS3TokensBody builds a POST /v3/s3tokens body carrying a correctly
// SigV2-signed request: RGW sends the string-to-sign base64-encoded in
// `token`, and the signature is base64(HMAC-SHA1(secret, stringToSign)).
func signedS3TokensBody(access, secret string) string {
	const stringToSign = "GET\n\n\nFri, 25 Sep 2026 00:00:00 GMT\n/bucket/object"
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	return fmt.Sprintf(`{"credentials":{"access":%q,"token":%q,"signature":%q}}`,
		access,
		base64.StdEncoding.EncodeToString([]byte(stringToSign)),
		base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

func postS3Tokens(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := newRGWCompatRouter(false)
	req := httptest.NewRequest(http.MethodPost, "/v3/s3tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertMaskedS3Denial checks the s3tokens denial discipline: 401 carrying
// Keystone's generic auth-failure message, with no reason leak.
func assertMaskedS3Denial(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "The request you have made requires authentication.") {
		t.Errorf("response must carry Keystone's generic auth-failure message: %s", w.Body.String())
	}
	for _, leak := range []string{"disabled", "missing", "not found", "signature"} {
		if strings.Contains(strings.ToLower(w.Body.String()), leak) {
			t.Errorf("response leaks the rejection reason %q: %s", leak, w.Body.String())
		}
	}
}

// TestS3TokensDisabledUserDenied: a correctly-signed EC2 request from a user
// disabled after the credential was minted must stop resolving (401, masked).
// The credential and the role assignment survive the disable — the liveness
// check in renderS3Token is the only thing between a disabled identity and a
// fresh, fully valid envelope on every request.
func TestS3TokensDisabledUserDenied(t *testing.T) {
	requireDB(t)
	access, secret := seedS3DisabledIdentity(t)

	// Control: while enabled, the same signed request resolves.
	if w := postS3Tokens(t, signedS3TokensBody(access, secret)); w.Code != http.StatusOK {
		t.Fatalf("enabled control: status = %d body=%s, want 200", w.Code, w.Body.String())
	}

	if _, err := testPool.Exec(context.Background(),
		"UPDATE users SET enabled = false WHERE id = $1", s3DisabledUserID); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	assertMaskedS3Denial(t, postS3Tokens(t, signedS3TokensBody(access, secret)))
}

// TestS3TokensDisabledProjectDenied: the project-scoped sibling — a project
// disabled after the credential was minted must fail closed too, mirroring
// the disabled-project 401 on the token-issue paths.
func TestS3TokensDisabledProjectDenied(t *testing.T) {
	requireDB(t)
	access, secret := seedS3DisabledIdentity(t)

	if w := postS3Tokens(t, signedS3TokensBody(access, secret)); w.Code != http.StatusOK {
		t.Fatalf("enabled control: status = %d body=%s, want 200", w.Code, w.Body.String())
	}

	if _, err := testPool.Exec(context.Background(),
		"UPDATE projects SET enabled = false WHERE id = $1", s3DisabledProjectID); err != nil {
		t.Fatalf("disable project: %v", err)
	}
	assertMaskedS3Denial(t, postS3Tokens(t, signedS3TokensBody(access, secret)))
}

// TestRenderS3TokenMissingIdentityDenied: a stale EC2 blob tenant (project
// row gone while the credential survives) or a user row that vanished under
// the credential must deny — not render an envelope with empty names.
func TestRenderS3TokenMissingIdentityDenied(t *testing.T) {
	requireDB(t)
	seedRGWCompatFixtures(t)

	for _, tc := range []struct{ name, user, project string }{
		{"missing user", "deadbeef-0000-4000-8000-00000000ba51", rgwCompatProjectID},
		{"missing project", rgwCompatSeededAdminID, "deadbeef-0000-4000-8000-00000000ba52"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v3/s3tokens", nil)
			NewService(testPool, nil, nil).renderS3Token(c, tc.user, tc.project)
			assertMaskedS3Denial(t, w)
		})
	}
}
