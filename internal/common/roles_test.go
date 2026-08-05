package common

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestHasRole: the shared role check reads the "roles" context slice and matches
// exactly, distinguishing present from absent. No DB required.
func TestHasRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name  string
		roles []string
		role  string
		want  bool
	}{
		{"admin present among many", []string{"member", "admin"}, "admin", true},
		{"admin absent", []string{"member"}, "admin", false},
		{"no roles set", nil, "admin", false},
		{"exact non-admin match", []string{"reader"}, "reader", true},
		{"substring is not a match", []string{"administrator"}, "admin", false},
	}
	for _, tc := range cases {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if tc.roles != nil {
			c.Set("roles", tc.roles)
		}
		if got := HasRole(c, tc.role); got != tc.want {
			t.Errorf("%s: HasRole(%v, %q) = %v, want %v", tc.name, tc.roles, tc.role, got, tc.want)
		}
	}
}
