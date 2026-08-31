package keystone

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
)

// SPEC-025 T025 (FR-007, FR-010..FR-013): the identity self-link rule, at the unit
// level.
//
// The contract-level counterpart walks live responses; this one pins the rule itself,
// so a regression names the helper rather than an endpoint. Both matter: the defect
// class here is a link that reads fine in code and is unusable on the wire.
func TestIdentityLink(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtx := func(host string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/v3/projects", nil)
		c.Request.Host = host
		return c
	}

	t.Run("is absolute and carries a scheme", func(t *testing.T) {
		got := identityLink(newCtx("lightstack.example:35357"), "projects", "abc")
		self, ok := got["self"].(string)
		if !ok {
			t.Fatalf("links has no self string: %#v", got)
		}
		u, err := url.Parse(self)
		if err != nil {
			t.Fatalf("self %q does not parse: %v", self, err)
		}
		if u.Scheme == "" {
			t.Errorf("self %q is scheme-less; the 6.x client renders it but no client can follow it", self)
		}
		if u.Host == "" {
			t.Errorf("self %q is relative", self)
		}
		if want := "http://lightstack.example:35357/v3/projects/abc"; self != want {
			t.Errorf("self = %q; want %q", self, want)
		}
	})

	t.Run("names the host the client addressed", func(t *testing.T) {
		t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "configured.example")
		got := identityLink(newCtx("client.chose.me:35357"), "users", "u1")
		if self := got["self"].(string); self != "http://client.chose.me:35357/v3/users/u1" {
			t.Errorf("self = %q; the request host must outrank LIGHTSTACK_ENDPOINT_HOST (FR-002)", self)
		}
	})

	t.Run("falls back to the configured host when the request has none", func(t *testing.T) {
		t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "configured.example")
		got := identityLink(newCtx(""), "roles", "r1")
		if self := got["self"].(string); self != "http://configured.example:35357/v3/roles/r1" {
			t.Errorf("self = %q; want the configured host on the identity port (FR-003)", self)
		}
	})

	t.Run("every collection it is used for renders the same shape", func(t *testing.T) {
		for _, collection := range []string{"projects", "users", "roles", "groups"} {
			got := identityLink(newCtx("h:35357"), collection, "x")
			want := "http://h:35357/v3/" + collection + "/x"
			if self := got["self"].(string); self != want {
				t.Errorf("%s: self = %q; want %q", collection, self, want)
			}
		}
	})
}
