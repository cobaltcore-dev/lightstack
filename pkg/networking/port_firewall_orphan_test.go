package networking

import (
	"slices"
	"strings"
	"testing"
)

// SPEC-033 T248, T314 (FR-041; contracts/port-firewall.md, "A TAP without a bound port"): a guest's TAP
// whose port is gone has no chains, so its frames are dropped — toward a bound port as well, where the
// bound port's dispatch would otherwise run that port's chain alone and accept the rest, and to this host,
// where nothing else would stop them — and so are the frames sent to it.

// tapFrame is a frame from one of a bridge's ports. The bridge forwards it to another of its ports — in
// FORWARD, -i and -o are the bridge, --physdev-in and --physdev-out its ports, and it is bridged — or, local,
// delivers it to this host: in INPUT, -i is the bridge and --physdev-in its port, there is no out side, and
// --physdev-is-bridged does not match (iptables-extensions(8)).
type tapFrame struct {
	bridge, in, out string
	local           bool
}

// verdict follows the frame through the filter table from ls-filter-top as iptables would, for the
// matches the dispatch uses. A port's chain is entered and returned from, as when the port's rules allow
// the frame: what it checks is the frame's content, which is not the question here. It answers ACCEPT,
// DROP, or "" when the frame leaves ls-filter-top undecided, and the port chains it entered.
func (f tapFrame) verdict(t *testing.T, filter map[string][]string) (string, []string) {
	t.Helper()
	var entered []string
	var walk func(chain string) string
	walk = func(chain string) string {
		if isPortChain(chain) {
			entered = append(entered, chain)
			return ""
		}
		for _, rule := range filter[chain] {
			target, ok := f.matches(t, rule)
			switch {
			case !ok:
			case target == "ACCEPT" || target == "DROP":
				return target
			case target == "RETURN":
				return ""
			default:
				if v := walk(target); v != "" {
					return v
				}
			}
		}
		return ""
	}
	return walk(chainTop), entered
}

// matches reports whether the frame matches rule, and the rule's target.
func (f tapFrame) matches(t *testing.T, rule string) (string, bool) {
	t.Helper()
	fields := strings.Fields(rule)[2:] // after -A <chain>
	target, ok := "", true
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "-m":
			i++
		case "--physdev-is-bridged":
			ok = ok && !f.local
		case "-i":
			i++
			ok = ok && interfaceMatches(fields[i], f.bridge)
		case "-o":
			i++
			ok = ok && !f.local && interfaceMatches(fields[i], f.bridge)
		case "--physdev-in":
			i++
			ok = ok && interfaceMatches(fields[i], f.in)
		case "--physdev-out":
			i++
			ok = ok && !f.local && interfaceMatches(fields[i], f.out)
		case "-j":
			i++
			target = fields[i]
		default:
			t.Fatalf("rule %q: %s is a match this test does not follow", rule, fields[i])
		}
	}
	return target, ok
}

// interfaceMatches is iptables' interface match: a name, or a prefix ending in +.
func interfaceMatches(pattern, name string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "+"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return pattern == name
}

func TestPortFirewall_ATAPWithoutABoundPortIsDroppedBothWays(t *testing.T) {
	const (
		bridge  = "br-net1"
		orphan  = "tapffffffff-99" // a running guest's TAP, its port deleted
		router  = "lr-0123456789a"
		foreign = "br-1234567890ab" // Docker's: not a network's bridge
	)
	a, b := PortDevice(fwPortA), PortDevice(fwPortB)
	ports := []FirewallPort{
		{ID: fwPortA, MAC: "fa:16:3e:aa:00:01", Addresses: []string{"10.0.0.5"}, Bridge: bridge, Zone: 4097},
		{ID: fwPortB, MAC: "fa:16:3e:bb:00:02", Addresses: []string{"10.0.0.6"}, Bridge: bridge, Zone: 4097},
	}
	filter := portFirewallRules(ports, []string{bridge}, "IPv4")["filter"]
	inA, outA, outB := portChain("i", fwPortA), portChain("o", fwPortA), portChain("o", fwPortB)
	for _, tc := range []struct {
		name    string
		frame   tapFrame
		verdict string
		entered []string
	}{
		{"an orphan's frame toward a bound port", tapFrame{bridge: bridge, in: orphan, out: a}, "DROP", nil},
		{"a bound port's frame toward an orphan", tapFrame{bridge: bridge, in: a, out: orphan}, "DROP", nil},
		{"an orphan's frame toward a router's leg", tapFrame{bridge: bridge, in: orphan, out: router}, "DROP", nil},
		{"a router's frame toward an orphan", tapFrame{bridge: bridge, in: router, out: orphan}, "DROP", nil},
		{"a bound port's frame toward another", tapFrame{bridge: bridge, in: b, out: a}, "ACCEPT", []string{inA, outB}},
		{"a router's frame toward a bound port", tapFrame{bridge: bridge, in: router, out: a}, "ACCEPT", []string{inA}},
		{"a bound port's frame toward a router", tapFrame{bridge: bridge, in: a, out: router}, "ACCEPT", []string{outA}},
		{"an orphan's frame to this host", tapFrame{bridge: bridge, in: orphan, local: true}, "DROP", nil},
		{"a bound port's frame to this host", tapFrame{bridge: bridge, in: a, local: true}, "", []string{outA}},
		{"TAPs of another program on a bridge of its own", tapFrame{bridge: foreign, in: "tap0", out: "tap1"}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, entered := tc.frame.verdict(t, filter)
			if verdict != tc.verdict || !slices.Equal(entered, tc.entered) {
				t.Errorf("verdict %q after entering %q; want %q after %q", verdict, entered, tc.verdict, tc.entered)
			}
		})
	}
}
