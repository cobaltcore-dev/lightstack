package common

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHasRole(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name  string
		roles any // nil => never set on the context
		query string
		want  bool
	}{
		{"exact match", []string{"admin"}, "admin", true},
		{"match among several", []string{"member", "reader", "admin"}, "admin", true},
		{"non-admin role matches itself", []string{"member"}, "member", true},
		{"wrong role", []string{"member"}, "admin", false},
		{"empty role list", []string{}, "admin", false},
		{"roles never set", nil, "admin", false},
		// The contract that separates HasRole from middleware.RequireRole: "admin"
		// must NOT satisfy some other required role here.
		{"admin is not a wildcard", []string{"admin"}, "member", false},
		{"case sensitive", []string{"Admin"}, "admin", false},
		// gin's GetStringSlice returns the zero value on a type mismatch rather than
		// panicking, so a malformed context is a plain false, not a crash.
		{"roles of the wrong type", 42, "admin", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if tc.roles != nil {
				c.Set("roles", tc.roles)
			}
			if got := HasRole(c, tc.query); got != tc.want {
				t.Errorf("HasRole(ctx{roles:%v}, %q) = %v, want %v", tc.roles, tc.query, got, tc.want)
			}
		})
	}
}
