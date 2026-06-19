package neutron

import (
	"net"
	"testing"
)

// TestDHCPRange verifies the DHCP pool stays inside the subnet for prefixes other
// than /24 (the pre-fix code hardcoded .10–.250 and overflowed smaller subnets),
// while preserving the conventional .10–.250 window for a /24 (SPEC-009 WS-2).
func TestDHCPRange(t *testing.T) {
	cases := []struct {
		cidr, wantStart, wantEnd string
	}{
		{"10.0.0.0/24", "10.0.0.10", "10.0.0.250"}, // conventional window preserved
		{"192.168.5.0/24", "192.168.5.10", "192.168.5.250"},
		{"10.0.0.0/26", "10.0.0.10", "10.0.0.62"}, // end clamped to broadcast-1
		{"10.0.0.0/28", "10.0.0.10", "10.0.0.14"}, // end clamped
		{"10.0.0.0/29", "10.0.0.2", "10.0.0.6"},   // start falls back (.10 doesn't fit)
	}
	for _, c := range cases {
		_, ipNet, err := net.ParseCIDR(c.cidr)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", c.cidr, err)
		}
		gotStart, gotEnd := dhcpRange(ipNet)
		if gotStart != c.wantStart || gotEnd != c.wantEnd {
			t.Errorf("dhcpRange(%s) = %s..%s; want %s..%s", c.cidr, gotStart, gotEnd, c.wantStart, c.wantEnd)
		}
		// Every produced bound must be inside the subnet.
		if !ipNet.Contains(net.ParseIP(gotStart)) || !ipNet.Contains(net.ParseIP(gotEnd)) {
			t.Errorf("dhcpRange(%s) produced %s..%s outside the subnet", c.cidr, gotStart, gotEnd)
		}
	}
}
