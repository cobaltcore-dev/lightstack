package networking

import (
	"slices"
	"strings"
	"testing"
)

// SPEC-033 round 2, T125 (FR-041, FR-042, FR-047, FR-051; research R32; contracts/port-firewall.md):
// the firewall a node holds for the ports bound to it, rendered from their records as `iptables -S`
// prints each chain, so an unchanged chain compares equal and is left alone.

const (
	fwPortA = "aaaaaaaa-1111-4222-8333-444455556666" // stateful, both families, rules of every kind
	fwPortB = "bbbbbbbb-2222-4222-8333-444455556666" // stateless
	fwPortC = "cccccccc-3333-4222-8333-444455556666" // no group: no rules
	fwPortD = "dddddddd-4444-4222-8333-444455556666" // no address
)

func intp(v int) *int { return &v }

func fwPorts() []FirewallPort {
	return []FirewallPort{
		{ID: fwPortC, MAC: "fa:16:3e:cc:00:03", Addresses: []string{"10.0.1.7"}, Bridge: "br-net2", Zone: 4098},
		{ID: fwPortA, MAC: "fa:16:3e:aa:00:01", Addresses: []string{"fd00::5", "10.0.0.5"}, Bridge: "br-net1", Zone: 4097,
			Rules: []FirewallRule{
				{Direction: "egress", Ethertype: "IPv4"},
				{Direction: "egress", Ethertype: "IPv6"},
				{Direction: "ingress", Ethertype: "IPv4", Protocol: "tcp", PortMin: intp(22), PortMax: intp(22), Remote: "0.0.0.0/0"},
				{Direction: "ingress", Ethertype: "IPv4", Protocol: "6", PortMin: intp(22), PortMax: intp(22)}, // the same, by number
				{Direction: "ingress", Ethertype: "IPv4", Protocol: "icmp", PortMin: intp(8), Remote: "192.0.2.0/24"},
				{Direction: "ingress", Ethertype: "IPv4", Protocol: "icmp", PortMin: intp(0), PortMax: intp(0)}, // echo reply: type 0 is a type
				{Direction: "ingress", Ethertype: "IPv4", Remote: "10.0.0.6"},                                   // a remote group's member
				{Direction: "ingress", Ethertype: "IPv6", Protocol: "icmp"},                                     // icmp in IPv6 is ipv6-icmp
				{Direction: "egress", Ethertype: "IPv4", Protocol: "udp", PortMin: intp(53), PortMax: intp(54), Remote: "198.51.100.1/32"},
			}},
		{ID: fwPortB, MAC: "fa:16:3e:bb:00:02", Addresses: []string{"10.0.0.6"}, Bridge: "br-net1", Zone: 4097, Stateless: true,
			Rules: []FirewallRule{
				{Direction: "ingress", Ethertype: "IPv4", Protocol: "udp", PortMin: intp(53), PortMax: intp(54)},
				{Direction: "egress", Ethertype: "IPv4"},
			}},
		{ID: fwPortD, MAC: "fa:16:3e:dd:00:04", Bridge: "br-net2", Zone: 4098},
	}
}

func wantChain(t *testing.T, tables map[string]map[string][]string, table, chain string, want ...string) {
	t.Helper()
	got, ok := tables[table][chain]
	if !ok {
		t.Errorf("%s %s: no such chain", table, chain)
		return
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s %s:\n%s\nwant:\n%s", table, chain, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPortFirewallDevicesAndChains(t *testing.T) {
	if got := PortDevice(fwPortA); got != "tapaaaaaaaa-11" {
		t.Errorf("device %q; want tap and the port id's first eleven characters, as upstream's", got)
	}
	if got := portChain("i", fwPortA); got != "ls-iaaaaaaaa-1" {
		t.Errorf("chain %q; want ls-i and the port id's first ten characters", got)
	}
}

func TestPortFirewallIPv4(t *testing.T) {
	tables := portFirewallRules(fwPorts(), nil, "IPv4")
	const a, b, c, d = "tapaaaaaaaa-11", "tapbbbbbbbb-22", "tapcccccccc-33", "tapdddddddd-44"

	wantChain(t, tables, "filter", "ls-filter-top", "-A ls-filter-top -j ls-local")
	dispatch := func(dev, port string) []string {
		return []string{
			"-A ls-local -m physdev --physdev-out " + dev + " --physdev-is-bridged -j ls-sg-chain",
			"-A ls-local -m physdev --physdev-in " + dev + " --physdev-is-bridged -j ls-sg-chain",
			"-A ls-local -m physdev --physdev-in " + dev + " -j ls-o" + port[:10],
		}
	}
	// The guests' TAPs on the networks' bridges are guarded before any port's dispatch (T248).
	local := []string{
		"-A ls-local -i br-net1 -m physdev --physdev-in tap+ -j ls-bound-in",
		"-A ls-local -o br-net1 -m physdev --physdev-out tap+ --physdev-is-bridged -j ls-bound-out",
		"-A ls-local -i br-net2 -m physdev --physdev-in tap+ -j ls-bound-in",
		"-A ls-local -o br-net2 -m physdev --physdev-out tap+ --physdev-is-bridged -j ls-bound-out",
	}
	var boundIn, boundOut []string
	for _, p := range [][2]string{{a, fwPortA}, {b, fwPortB}, {c, fwPortC}, {d, fwPortD}} {
		local = append(local, dispatch(p[0], p[1])...)
		boundIn = append(boundIn, "-A ls-bound-in -m physdev --physdev-in "+p[0]+" -j RETURN")
		boundOut = append(boundOut, "-A ls-bound-out -m physdev --physdev-out "+p[0]+" -j RETURN")
	}
	wantChain(t, tables, "filter", "ls-bound-in", append(boundIn, "-A ls-bound-in -j DROP")...)
	wantChain(t, tables, "filter", "ls-bound-out", append(boundOut, "-A ls-bound-out -j DROP")...)
	local = append(local,
		"-A ls-local -m physdev --physdev-out lr+ --physdev-is-bridged -j ACCEPT",
		"-A ls-local -m physdev --physdev-in lr+ --physdev-is-bridged -j ACCEPT",
		"-A ls-local -m physdev --physdev-out ld+ --physdev-is-bridged -j ACCEPT",
		"-A ls-local -m physdev --physdev-in ld+ --physdev-is-bridged -j ACCEPT")
	wantChain(t, tables, "filter", "ls-local", local...)

	var sg []string
	for _, p := range [][2]string{{a, fwPortA}, {b, fwPortB}, {c, fwPortC}, {d, fwPortD}} {
		sg = append(sg,
			"-A ls-sg-chain -m physdev --physdev-out "+p[0]+" --physdev-is-bridged -j ls-i"+p[1][:10],
			"-A ls-sg-chain -m physdev --physdev-in "+p[0]+" --physdev-is-bridged -j ls-o"+p[1][:10])
	}
	wantChain(t, tables, "filter", "ls-sg-chain", append(sg, "-A ls-sg-chain -j ACCEPT")...)
	wantChain(t, tables, "filter", "ls-sg-fallback", "-A ls-sg-fallback -j DROP")

	wantChain(t, tables, "filter", "ls-iaaaaaaaa-1",
		"-A ls-iaaaaaaaa-1 -d 10.0.0.5/32 -p udp -m udp --sport 67 --dport 68 -j RETURN",
		"-A ls-iaaaaaaaa-1 -d 255.255.255.255/32 -p udp -m udp --sport 67 --dport 68 -j RETURN",
		"-A ls-iaaaaaaaa-1 -m state --state RELATED,ESTABLISHED -j RETURN",
		"-A ls-iaaaaaaaa-1 -p icmp -m icmp --icmp-type 0/0 -j RETURN",
		"-A ls-iaaaaaaaa-1 -p tcp -m tcp --dport 22 -j RETURN",
		"-A ls-iaaaaaaaa-1 -s 10.0.0.6/32 -j RETURN",
		"-A ls-iaaaaaaaa-1 -s 192.0.2.0/24 -p icmp -m icmp --icmp-type 8 -j RETURN",
		"-A ls-iaaaaaaaa-1 -m state --state INVALID -j DROP",
		"-A ls-iaaaaaaaa-1 -j ls-sg-fallback")
	wantChain(t, tables, "filter", "ls-oaaaaaaaa-1",
		"-A ls-oaaaaaaaa-1 -s 0.0.0.0/32 -d 255.255.255.255/32 -p udp -m udp --sport 68 --dport 67 -j RETURN",
		"-A ls-oaaaaaaaa-1 -j ls-saaaaaaaa-1",
		"-A ls-oaaaaaaaa-1 -p udp -m udp --sport 68 --dport 67 -j RETURN",
		"-A ls-oaaaaaaaa-1 -p udp -m udp --sport 67 --dport 68 -j DROP",
		"-A ls-oaaaaaaaa-1 -m state --state RELATED,ESTABLISHED -j RETURN",
		"-A ls-oaaaaaaaa-1 -d 198.51.100.1/32 -p udp -m udp --dport 53:54 -j RETURN",
		"-A ls-oaaaaaaaa-1 -j RETURN",
		"-A ls-oaaaaaaaa-1 -m state --state INVALID -j DROP",
		"-A ls-oaaaaaaaa-1 -j ls-sg-fallback")
	wantChain(t, tables, "filter", "ls-saaaaaaaa-1",
		"-A ls-saaaaaaaa-1 -s 10.0.0.5/32 -m mac --mac-source fa:16:3e:aa:00:01 -j RETURN",
		"-A ls-saaaaaaaa-1 -j DROP")

	// Stateless: no RELATED,ESTABLISHED and no INVALID rule (FR-051).
	wantChain(t, tables, "filter", "ls-ibbbbbbbb-2",
		"-A ls-ibbbbbbbb-2 -d 10.0.0.6/32 -p udp -m udp --sport 67 --dport 68 -j RETURN",
		"-A ls-ibbbbbbbb-2 -d 255.255.255.255/32 -p udp -m udp --sport 67 --dport 68 -j RETURN",
		"-A ls-ibbbbbbbb-2 -p udp -m udp --dport 53:54 -j RETURN",
		"-A ls-ibbbbbbbb-2 -j ls-sg-fallback")

	// No group: replies, its own DHCP, and nothing else.
	wantChain(t, tables, "filter", "ls-icccccccc-3",
		"-A ls-icccccccc-3 -d 10.0.1.7/32 -p udp -m udp --sport 67 --dport 68 -j RETURN",
		"-A ls-icccccccc-3 -d 255.255.255.255/32 -p udp -m udp --sport 67 --dport 68 -j RETURN",
		"-A ls-icccccccc-3 -m state --state RELATED,ESTABLISHED -j RETURN",
		"-A ls-icccccccc-3 -m state --state INVALID -j DROP",
		"-A ls-icccccccc-3 -j ls-sg-fallback")

	// No address: its MAC alone, as upstream's.
	wantChain(t, tables, "filter", "ls-sdddddddd-4",
		"-A ls-sdddddddd-4 -m mac --mac-source fa:16:3e:dd:00:04 -j RETURN",
		"-A ls-sdddddddd-4 -j DROP")

	wantChain(t, tables, "raw", "ls-ct",
		"-A ls-ct -m physdev --physdev-in "+a+" -j CT --zone 4097",
		"-A ls-ct -m physdev --physdev-in "+b+" -j CT --notrack",
		"-A ls-ct -m physdev --physdev-in "+c+" -j CT --zone 4098",
		"-A ls-ct -m physdev --physdev-in "+d+" -j CT --zone 4098",
		"-A ls-ct -i br-net1 -j CT --zone 4097",
		"-A ls-ct -i br-net2 -j CT --zone 4098")

	var chains []string
	for name := range tables["filter"] {
		chains = append(chains, name)
	}
	slices.Sort(chains)
	if want := 6 + 3*4; len(chains) != want {
		t.Errorf("filter chains %q; want the six fixed ones and three per port", chains)
	}
}

func TestPortFirewallIPv6(t *testing.T) {
	tables := portFirewallRules(fwPorts(), nil, "IPv6")
	wantChain(t, tables, "filter", "ls-iaaaaaaaa-1",
		"-A ls-iaaaaaaaa-1 -p ipv6-icmp -m icmp6 --icmpv6-type 130 -j RETURN",
		"-A ls-iaaaaaaaa-1 -p ipv6-icmp -m icmp6 --icmpv6-type 131 -j RETURN",
		"-A ls-iaaaaaaaa-1 -p ipv6-icmp -m icmp6 --icmpv6-type 132 -j RETURN",
		"-A ls-iaaaaaaaa-1 -p ipv6-icmp -m icmp6 --icmpv6-type 135 -j RETURN",
		"-A ls-iaaaaaaaa-1 -p ipv6-icmp -m icmp6 --icmpv6-type 136 -j RETURN",
		"-A ls-iaaaaaaaa-1 -p udp -m udp --sport 547 --dport 546 -j RETURN",
		"-A ls-iaaaaaaaa-1 -m state --state RELATED,ESTABLISHED -j RETURN",
		"-A ls-iaaaaaaaa-1 -p ipv6-icmp -j RETURN",
		"-A ls-iaaaaaaaa-1 -m state --state INVALID -j DROP",
		"-A ls-iaaaaaaaa-1 -j ls-sg-fallback")
	wantChain(t, tables, "filter", "ls-oaaaaaaaa-1",
		"-A ls-oaaaaaaaa-1 -s ::/128 -d ff02::/16 -p ipv6-icmp -m icmp6 --icmpv6-type 131 -j RETURN",
		"-A ls-oaaaaaaaa-1 -s ::/128 -d ff02::/16 -p ipv6-icmp -m icmp6 --icmpv6-type 135 -j RETURN",
		"-A ls-oaaaaaaaa-1 -s ::/128 -d ff02::/16 -p ipv6-icmp -m icmp6 --icmpv6-type 143 -j RETURN",
		"-A ls-oaaaaaaaa-1 -j ls-saaaaaaaa-1",
		"-A ls-oaaaaaaaa-1 -p ipv6-icmp -m icmp6 --icmpv6-type 134 -j DROP",
		"-A ls-oaaaaaaaa-1 -p ipv6-icmp -j RETURN",
		"-A ls-oaaaaaaaa-1 -p udp -m udp --sport 546 --dport 547 -j RETURN",
		"-A ls-oaaaaaaaa-1 -p udp -m udp --sport 547 --dport 546 -j DROP",
		"-A ls-oaaaaaaaa-1 -m state --state RELATED,ESTABLISHED -j RETURN",
		"-A ls-oaaaaaaaa-1 -j RETURN",
		"-A ls-oaaaaaaaa-1 -m state --state INVALID -j DROP",
		"-A ls-oaaaaaaaa-1 -j ls-sg-fallback")
	// Its address and the link-local address its MAC gives it, as upstream's.
	wantChain(t, tables, "filter", "ls-saaaaaaaa-1",
		"-A ls-saaaaaaaa-1 -s fd00::5/128 -m mac --mac-source fa:16:3e:aa:00:01 -j RETURN",
		"-A ls-saaaaaaaa-1 -s fe80::f816:3eff:feaa:1/128 -m mac --mac-source fa:16:3e:aa:00:01 -j RETURN",
		"-A ls-saaaaaaaa-1 -j DROP")
	// SPEC-033 T214: a port without an IPv6 address, but with addresses, has no IPv6 address
	// check, as upstream's iptables driver gives a family one only for the addresses the port has: its
	// IPv6 egress, link-local included, goes on to its rules. A port with no address at all is checked
	// by its MAC in both families.
	if chain, ok := tables["filter"]["ls-sbbbbbbbb-2"]; ok {
		t.Errorf("an IPv4-only port's IPv6 address check: %v; want none", chain)
	}
	if out := tables["filter"]["ls-obbbbbbbb-2"]; slices.Contains(out, "-A ls-obbbbbbbb-2 -j ls-sbbbbbbbb-2") {
		t.Errorf("an IPv4-only port's IPv6 egress jumps to an address check: %v", out)
	}
	wantChain(t, tables, "filter", "ls-sdddddddd-4",
		"-A ls-sdddddddd-4 -m mac --mac-source fa:16:3e:dd:00:04 -j RETURN",
		"-A ls-sdddddddd-4 -j DROP")
	if out := tables["filter"]["ls-odddddddd-4"]; !slices.Contains(out, "-A ls-odddddddd-4 -j ls-sdddddddd-4") {
		t.Errorf("a port without addresses: IPv6 egress %v; want its MAC check", out)
	}
}

func TestPortFirewallPayload(t *testing.T) {
	ports := fwPorts()
	desired := portFirewallRules(ports, nil, "IPv4")

	// The node holds what it should, and one more port, deleted since: its chains and dispatch go, in
	// one commit, and nothing else is written.
	held := portFirewallRules(append(slices.Clone(ports), FirewallPort{ID: "eeeeeeee-5555-4222-8333-444455556666",
		MAC: "fa:16:3e:ee:00:05", Addresses: []string{"10.0.0.9"}, Bridge: "br-net1", Zone: 4097}), nil, "IPv4")
	current := map[string]savedTable{}
	for _, table := range []string{"filter", "raw"} {
		current[table] = savedFromRules(held[table])
	}
	current["filter"].rules["FORWARD"] = []string{"-A FORWARD -j ls-filter-top"}
	current["filter"].rules["INPUT"] = []string{"-A INPUT -j ls-filter-top"}
	current["raw"].rules["PREROUTING"] = []string{"-A PREROUTING -j ls-ct"}

	payload := firewallPayload(current, desired)
	want := strings.Join([]string{
		"*filter",
		":ls-bound-in - [0:0]",
		":ls-bound-out - [0:0]",
		":ls-local - [0:0]",
		":ls-sg-chain - [0:0]",
		"-F ls-bound-in",
	}, "\n")
	if !strings.HasPrefix(payload, want) {
		t.Errorf("payload:\n%s\nwant it to begin:\n%s", payload, want)
	}
	for _, line := range []string{
		"-F ls-ieeeeeeee-5", "-X ls-ieeeeeeee-5", "-X ls-oeeeeeeee-5", "-X ls-seeeeeeee-5", "*raw", "-F ls-ct",
	} {
		if !slices.Contains(strings.Split(payload, "\n"), line) {
			t.Errorf("payload lacks %q:\n%s", line, payload)
		}
	}
	for _, unchanged := range []string{"ls-iaaaaaaaa-1", "ls-filter-top", "ls-sg-fallback", "FORWARD"} {
		if strings.Contains(payload, " "+unchanged+" ") || strings.Contains(payload, ":"+unchanged+" ") {
			t.Errorf("payload writes %s, which the node holds already:\n%s", unchanged, payload)
		}
	}
	// Chains are emptied before any is deleted: a deleted chain's referrers go first.
	if strings.Index(payload, "-X ls-ieeeeeeee-5") < strings.Index(payload, "-F ls-sg-chain") {
		t.Errorf("a chain is deleted before its referrers are emptied:\n%s", payload)
	}

	// A node without Lightstack's jumps gets them first in its chains; one holding exactly what it
	// should gets no payload at all.
	bare := map[string]savedTable{"filter": {rules: map[string][]string{}}, "raw": {rules: map[string][]string{}}}
	fresh := firewallPayload(bare, desired)
	for _, line := range []string{"-I FORWARD 1 -j ls-filter-top", "-I INPUT 1 -j ls-filter-top", "-I PREROUTING 1 -j ls-ct"} {
		if !slices.Contains(strings.Split(fresh, "\n"), line) {
			t.Errorf("a fresh node's payload lacks %q", line)
		}
	}
	same := map[string]savedTable{}
	for _, table := range []string{"filter", "raw"} {
		same[table] = savedFromRules(desired[table])
	}
	same["filter"].rules["FORWARD"] = []string{"-A FORWARD -j ls-filter-top"}
	same["filter"].rules["INPUT"] = []string{"-A INPUT -j ls-filter-top"}
	same["raw"].rules["PREROUTING"] = []string{"-A PREROUTING -j ls-ct"}
	if p := firewallPayload(same, desired); p != "" {
		t.Errorf("a node holding its firewall got a payload:\n%s", p)
	}
}

// SPEC-033 T205: a node with no bound port — a fresh node before its first guest, a dedicated
// network node, a node after teardown — wants the raw table's ls-ct with no rule. A bare node lacks the
// chain, and the jump from PREROUTING needs it: it is declared, so the commit holds, and a node that
// has it, empty, gets nothing.
func TestPortFirewallPayloadWithNoPort(t *testing.T) {
	desired := portFirewallRules(nil, nil, "IPv4")
	bare := map[string]savedTable{"filter": {rules: map[string][]string{}}, "raw": {rules: map[string][]string{}}}
	lines := strings.Split(firewallPayload(bare, desired), "\n")
	raw := slices.Index(lines, "*raw")
	if raw < 0 || !slices.Contains(lines[raw:], ":ls-ct - [0:0]") || !slices.Contains(lines[raw:], "-I PREROUTING 1 -j ls-ct") {
		t.Fatalf("a bare node's payload with no port:\n%s\nwant ls-ct declared before PREROUTING jumps to it", strings.Join(lines, "\n"))
	}
	if slices.Index(lines[raw:], ":ls-ct - [0:0]") > slices.Index(lines[raw:], "-I PREROUTING 1 -j ls-ct") {
		t.Errorf("ls-ct is declared after the jump to it:\n%s", strings.Join(lines, "\n"))
	}
	held := map[string]savedTable{}
	for _, table := range []string{"filter", "raw"} {
		held[table] = savedFromRules(desired[table])
	}
	held["filter"].rules["FORWARD"] = []string{"-A FORWARD -j ls-filter-top"}
	held["filter"].rules["INPUT"] = []string{"-A INPUT -j ls-filter-top"}
	held["raw"].rules["PREROUTING"] = []string{"-A PREROUTING -j ls-ct"}
	if p := firewallPayload(held, desired); p != "" {
		t.Errorf("a node holding the empty firewall got a payload:\n%s", p)
	}
}

// savedFromRules is a table as iptables-save would print the given chains.
func savedFromRules(chains map[string][]string) savedTable {
	t := savedTable{rules: map[string][]string{}}
	for name, rules := range chains {
		t.chains = append(t.chains, name)
		t.rules[name] = slices.Clone(rules)
	}
	slices.Sort(t.chains)
	return t
}
