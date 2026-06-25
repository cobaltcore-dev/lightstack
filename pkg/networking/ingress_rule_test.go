package networking

import (
	"strings"
	"testing"
)

// TestIngressRuleSpec covers the security-group-rule → iptables-args rendering
// (SPEC-009 WS-3 default-deny ingress): protocol, single port vs range, icmp
// type, and the remote-CIDR (with the 0.0.0.0/0 "any" shortcut dropping -s).
func TestIngressRuleSpec(t *testing.T) {
	cases := []struct {
		name string
		rule IngressRule
		want string // space-joined iptables args
	}{
		{"tcp single port", IngressRule{Protocol: "tcp", PortMin: 22}, "-p tcp --dport 22"},
		{"tcp port range", IngressRule{Protocol: "tcp", PortMin: 80, PortMax: 90}, "-p tcp --dport 80:90"},
		{"tcp equal min==max", IngressRule{Protocol: "tcp", PortMin: 443, PortMax: 443}, "-p tcp --dport 443"},
		{"udp", IngressRule{Protocol: "udp", PortMin: 53}, "-p udp --dport 53"},
		{"icmp type", IngressRule{Protocol: "icmp", PortMin: 8}, "-p icmp --icmp-type 8"},
		{"remote cidr", IngressRule{Protocol: "tcp", PortMin: 22, Remote: "10.0.0.0/8"}, "-p tcp --dport 22 -s 10.0.0.0/8"},
		{"any-remote drops -s", IngressRule{Protocol: "tcp", PortMin: 22, Remote: "0.0.0.0/0"}, "-p tcp --dport 22"},
		{"protocol only", IngressRule{Protocol: "tcp"}, "-p tcp"},
		{"empty", IngressRule{}, ""},
	}
	for _, c := range cases {
		if got := strings.Join(ingressRuleSpec(c.rule), " "); got != c.want {
			t.Errorf("%s: ingressRuleSpec(%+v) = %q; want %q", c.name, c.rule, got, c.want)
		}
	}
}
