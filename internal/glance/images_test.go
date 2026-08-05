package glance

import (
	"database/sql"
	"testing"
)

func TestAllowedImageUpdateField(t *testing.T) {
	tests := []struct {
		path    string
		allowed bool
		field   string
	}{
		{"/name", true, "name"},
		{"/visibility", true, "visibility"},
		{"/min_disk", true, "min_disk_gb"},
		{"/min_ram", true, "min_ram_mb"},
		{"/malicious; DROP TABLE images;--", false, ""},
		{"/nonexistent", false, ""},
	}
	for _, tt := range tests {
		field, ok := allowedImageUpdateField(tt.path)
		if ok != tt.allowed {
			t.Errorf("allowedImageUpdateField(%q) ok = %v, want %v", tt.path, ok, tt.allowed)
		}
		if ok && field != tt.field {
			t.Errorf("allowedImageUpdateField(%q) field = %q, want %q", tt.path, field, tt.field)
		}
	}
}

// TestImageDeleteAuthz: delete authorization follows upstream Glance ownership
// semantics (F6) — owner-or-admin may delete; a non-owner non-admin gets 403 for a
// visible (public) image and 404 for one they cannot see (so existence isn't leaked).
// 0 means "authorized, proceed to delete". Pure — no DB.
func TestImageDeleteAuthz(t *testing.T) {
	owner := func(p string) sql.NullString { return sql.NullString{String: p, Valid: true} }
	none := sql.NullString{} // ownerless (legacy public image, or owner project deleted)

	cases := []struct {
		name    string
		isAdmin bool
		owner   sql.NullString
		caller  string
		vis     string
		want    int
	}{
		{"owner deletes own private", false, owner("p1"), "p1", "private", 0},
		{"owner deletes own public", false, owner("p1"), "p1", "public", 0},
		{"admin deletes another project's public", true, owner("p1"), "p2", "public", 0},
		{"admin deletes ownerless public (legacy)", true, none, "p2", "public", 0},
		{"non-owner non-admin deletes public", false, owner("p1"), "p2", "public", 403},
		{"non-owner non-admin ownerless public", false, none, "p2", "public", 403},
		{"non-owner non-admin private is hidden", false, owner("p1"), "p2", "private", 404},
	}
	for _, tc := range cases {
		if got := imageDeleteAuthz(tc.isAdmin, tc.owner, tc.caller, tc.vis); got != tc.want {
			t.Errorf("%s: imageDeleteAuthz = %d, want %d", tc.name, got, tc.want)
		}
	}
}
