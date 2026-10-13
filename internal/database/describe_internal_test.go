package database

import "testing"

// TestLocation covers the credential-free location rendering, including the edge cases the
// startup log line can meet: a bad URL, a PostgreSQL URL without an explicit port, and a
// SQLite path.
func TestLocation(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"postgres://u:p@db.example.com:5432/lightstack", "db.example.com:5432/lightstack"},
		{"postgres://u:p@db.example.com/lightstack", "db.example.com:5432/lightstack"}, // default port filled in
		{"sqlite:///var/lib/lightstack/db/lightstack.db", "/var/lib/lightstack/db/lightstack.db"},
		{"://not a url", ""}, // unparseable → empty, never credentials
	} {
		if got := location(tc.in); got != tc.want {
			t.Errorf("location(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
