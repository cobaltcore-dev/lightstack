package networking

import (
	"slices"
	"strings"
	"testing"
)

// SPEC-033 T169 (FR-059): a rule's remote and protocol went into
// iptables-restore's input as stored, so a newline in either wrote commands that ran as root, and a
// value iptables does not parse failed the node's whole commit. The renderer now writes only what it
// parses, and leaves out a rule it cannot express exactly: a rule only ever allows, so leaving one
// out allows less.

const fwRulePort = "eeeeeeee-5555-4222-8333-444455556666"

// renderRule renders one port that carries rule alone, for rule's family.
func renderRule(rule FirewallRule) map[string]map[string][]string {
	family := rule.Ethertype
	if family == "" {
		family = "IPv4"
	}
	return portFirewallRules([]FirewallPort{{ID: fwRulePort, MAC: "fa:16:3e:ee:00:05",
		Addresses: []string{"10.0.0.9", "fd00::9"}, Bridge: "br-net1", Zone: 4097,
		Rules: []FirewallRule{rule}}}, nil, family)
}

func TestPortFirewallLeavesOutRulesItCannotExpress(t *testing.T) {
	cases := map[string]FirewallRule{
		"commands after a newline in the remote": {Direction: "ingress", Ethertype: "IPv4",
			Remote: "1.2.3.4\n-I ls-local 1 -j ACCEPT\n-A ls-local"},
		"a host name":                            {Direction: "ingress", Ethertype: "IPv4", Remote: "example.com"},
		"a prefix of the other family":           {Direction: "ingress", Ethertype: "IPv4", Remote: "2001:db8::/32"},
		"an address with a zone":                 {Direction: "ingress", Ethertype: "IPv6", Remote: "fe80::1%eth0"},
		"a netmask, which is never stored":       {Direction: "ingress", Ethertype: "IPv4", Remote: "10.0.0.0/255.0.0.0"},
		"a line after a newline in the protocol": {Direction: "ingress", Ethertype: "IPv4", Protocol: "1\n:ls-ct -"},
		"a protocol upstream does not name":      {Direction: "ingress", Ethertype: "IPv4", Protocol: "foo"},
		"a protocol number past 255":             {Direction: "ingress", Ethertype: "IPv4", Protocol: "256"},
		"ports without a protocol": {Direction: "ingress", Ethertype: "IPv4",
			PortMin: intp(22), PortMax: intp(22)},
		"ports on a protocol without ports": {Direction: "ingress", Ethertype: "IPv4", Protocol: "gre",
			PortMin: intp(1), PortMax: intp(2)},
		"a last port without a first": {Direction: "ingress", Ethertype: "IPv4", Protocol: "tcp", PortMax: intp(22)},
		"port 0":                      {Direction: "ingress", Ethertype: "IPv4", Protocol: "tcp", PortMin: intp(0), PortMax: intp(0)},
		"a port past 65535":           {Direction: "ingress", Ethertype: "IPv4", Protocol: "tcp", PortMin: intp(70000)},
		"an ICMP type past 255":       {Direction: "ingress", Ethertype: "IPv4", Protocol: "icmp", PortMin: intp(300)},
		"an ICMP code without a type": {Direction: "ingress", Ethertype: "IPv4", Protocol: "icmp", PortMax: intp(0)},
	}
	for name, rule := range cases {
		t.Run(name, func(t *testing.T) {
			got := renderRule(rule)
			want := renderRule(FirewallRule{Ethertype: rule.Ethertype}) // a rule of no direction: none rendered
			for table, chains := range got {
				for chain, lines := range chains {
					if !slices.Equal(lines, want[table][chain]) {
						t.Errorf("%s %s:\n%s\nwant:\n%s", table, chain, strings.Join(lines, "\n"),
							strings.Join(want[table][chain], "\n"))
					}
					for _, line := range lines {
						if strings.ContainsAny(line, "\r\n") {
							t.Errorf("%s %s: a line spans lines: %q", table, chain, line)
						}
					}
				}
			}
		})
	}
}

// A protocol renders by its number, whatever the spelling the rule stored: upstream's names map to
// upstream's numbers, which is not always what iptables would make of the name (iptables reads
// "ipip" as 94, upstream's ipip is 4).
func TestPortFirewallRendersProtocolsByNumber(t *testing.T) {
	// ingress is what rule adds to the port's ingress chain.
	ingress := func(rule FirewallRule) []string {
		base := renderRule(FirewallRule{Ethertype: rule.Ethertype})["filter"][portChain("i", fwRulePort)]
		rule.Direction = "ingress"
		var added []string
		for _, line := range renderRule(rule)["filter"][portChain("i", fwRulePort)] {
			if !slices.Contains(base, line) {
				added = append(added, line)
			}
		}
		return added
	}
	chain := "-A " + portChain("i", fwRulePort) + " "
	cases := []struct {
		name string
		rule FirewallRule
		want string // the rule's line, or "" for a rule matching every packet of its family
	}{
		{"a number with a leading zero is decimal", FirewallRule{Protocol: "006", PortMin: intp(22), PortMax: intp(22)},
			chain + "-p tcp -m tcp --dport 22 -j RETURN"},
		{"a name in capitals", FirewallRule{Protocol: "TCP", PortMin: intp(22), PortMax: intp(22)},
			chain + "-p tcp -m tcp --dport 22 -j RETURN"},
		{"upstream's ipip is protocol 4", FirewallRule{Protocol: "ipip"}, chain + "-p 4 -j RETURN"},
		{"upstream's ipv6-encap is protocol 41", FirewallRule{Ethertype: "IPv6", Protocol: "ipv6-encap"},
			chain + "-p 41 -j RETURN"},
		{"protocol 0 is every protocol", FirewallRule{Protocol: "0"}, chain + "-j RETURN"},
		{"ip is protocol 0", FirewallRule{Protocol: "ip"}, chain + "-j RETURN"},
		{"any", FirewallRule{Protocol: "any"}, chain + "-j RETURN"},
		{"UDP-Lite ports, through multiport", FirewallRule{Protocol: "udplite", PortMin: intp(22), PortMax: intp(30)},
			chain + "-p udplite -m multiport --dports 22:30 -j RETURN"},
		{"UDP-Lite by number", FirewallRule{Protocol: "136", PortMin: intp(22), PortMax: intp(22)},
			chain + "-p udplite -m multiport --dports 22 -j RETURN"},
		{"an IPv6 rule's ICMP by number", FirewallRule{Ethertype: "IPv6", Protocol: "58", PortMin: intp(128)},
			chain + "-p ipv6-icmp -m icmp6 --icmpv6-type 128 -j RETURN"},
		// SPEC-033 T249: by name exactly where iptables prints one; its restore without /etc/protocols, the
		// image's, refuses any other name, and a number iptables prints by name never compares equal.
		{"upstream's vrrp is protocol 112, which iptables has no name for", FirewallRule{Protocol: "vrrp"},
			chain + "-p 112 -j RETURN"},
		{"112 by number", FirewallRule{Protocol: "112"}, chain + "-p 112 -j RETURN"},
		{"upstream's gre is protocol 47, which iptables has no name for", FirewallRule{Protocol: "gre"},
			chain + "-p 47 -j RETURN"},
		{"47 by number", FirewallRule{Protocol: "47"}, chain + "-p 47 -j RETURN"},
		{"108 is ipcomp", FirewallRule{Protocol: "108"}, chain + "-p ipcomp -j RETURN"},
		{"135 is mobility-header", FirewallRule{Ethertype: "IPv6", Protocol: "135"},
			chain + "-p mobility-header -j RETURN"},
		{"58 in an IPv4 rule is ipv6-icmp", FirewallRule{Protocol: "58"}, chain + "-p ipv6-icmp -j RETURN"},
		// SPEC-033 T277: ICMP type 255 alone is what iptables prints as any type.
		{"ICMP type 255 without a code is any type", FirewallRule{Protocol: "icmp", PortMin: intp(255)},
			chain + "-p icmp -m icmp --icmp-type any -j RETURN"},
		{"ICMP type 255 with a code stays a type", FirewallRule{Protocol: "icmp", PortMin: intp(255), PortMax: intp(0)},
			chain + "-p icmp -m icmp --icmp-type 255/0 -j RETURN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ingress(tc.rule)
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("allowed:\n%s\nwant:\n%s", strings.Join(got, "\n"), tc.want)
			}
		})
	}
}
