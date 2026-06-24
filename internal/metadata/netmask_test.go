package metadata

import "testing"

// TestCidrToNetmask covers the CIDR → dotted-netmask conversion served in
// network_data.json, across prefix lengths and the parse-failure fallback.
func TestCidrToNetmask(t *testing.T) {
	cases := []struct{ cidr, want string }{
		{"10.0.0.0/24", "255.255.255.0"},
		{"10.0.0.0/16", "255.255.0.0"},
		{"10.0.0.0/8", "255.0.0.0"},
		{"10.0.0.0/26", "255.255.255.192"},
		{"10.0.0.0/30", "255.255.255.252"},
		{"192.168.1.0/25", "255.255.255.128"},
		{"not-a-cidr", "not-a-cidr"}, // parse failure with no "/" → returns input
	}
	for _, c := range cases {
		if got := cidrToNetmask(c.cidr); got != c.want {
			t.Errorf("cidrToNetmask(%q) = %q; want %q", c.cidr, got, c.want)
		}
	}
}
