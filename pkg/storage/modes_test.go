package storage

import "testing"

// TestContainsMode covers the comma-separated storage-mode membership check used
// to pick backends (e.g. "local,rbd"); it must match whole tokens only, never a
// prefix/substring.
func TestContainsMode(t *testing.T) {
	cases := []struct {
		modes, target string
		want          bool
	}{
		{"local", "local", true},
		{"local,rbd", "rbd", true},
		{"local,rbd", "local", true},
		{"rbd,s3", "s3", true},
		{"local,rbd", "s3", false},
		{"", "local", false},
		{"local", "loc", false},    // not a prefix match
		{"local", "locals", false}, // not a superstring match
	}
	for _, c := range cases {
		if got := containsMode(c.modes, c.target); got != c.want {
			t.Errorf("containsMode(%q, %q) = %v; want %v", c.modes, c.target, got, c.want)
		}
	}
}
