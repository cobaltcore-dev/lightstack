package networking

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
)

// The firewall of the ports bound to a node (SPEC-033 FR-041, FR-042, FR-047, FR-051; research R32;
// contracts/port-firewall.md): upstream's iptables firewall for Linux bridges, under Lightstack's
// names. Bridged traffic of a port's TAP is dispatched to the port's ingress and egress chains; the
// egress chain checks the port's addresses and MAC first; both end in the port's rules, with replies
// allowed and anything else sent to a DROP. A guest's TAP without a bound port is dropped both ways.
// Router and DHCP legs are trusted by device name. Each network's traffic is tracked in its own
// connection-tracking zone, so networks with one range do not share entries. Every chain is rendered as
// `iptables -S` prints it, so a chain the node holds already compares equal and is left alone.

// FirewallPort is a port bound to the node, as its firewall is to hold it.
type FirewallPort struct {
	ID        string
	MAC       string
	Addresses []string // its fixed IPs, of either family
	Bridge    string   // its network's bridge
	Zone      int      // its network's connection-tracking zone
	Stateless bool     // its groups are stateless (FR-051): untracked, no reply or INVALID rule
	Rules     []FirewallRule
}

// FirewallRule is one rule of a port's groups. A rule naming a remote group is one rule per member
// address, the port's own excluded, as upstream's without ipset.
type FirewallRule struct {
	Direction string // "ingress" or "egress"
	Ethertype string // "IPv4" or "IPv6"; "" reads as IPv4, upstream's default
	Protocol  string // upstream's name, or a number from 0 to 255 (ProtocolNumber); "" or "any" for any
	PortMin   *int   // the first port, or the ICMP type; nil for any
	PortMax   *int   // the last port, or the ICMP code; nil for any
	Remote    string // a prefix or an address; "" for any
}

// The chains every node keeps, and the port chains' prefixes.
const (
	chainTop      = "ls-filter-top"  // first in FORWARD and INPUT
	chainLocal    = "ls-local"       // guards the guests' TAPs, dispatches each port's traffic, accepts the legs
	chainBoundIn  = "ls-bound-in"    // a frame from a guest's TAP: a bound port's returns, any other is dropped
	chainBoundOut = "ls-bound-out"   // a frame to a guest's TAP: the same
	chainSG       = "ls-sg-chain"    // sends a port's traffic to its chains, and accepts what returns
	chainFallback = "ls-sg-fallback" // DROP: what no rule allowed
	chainZone     = "ls-ct"          // raw: each port's and network's connection-tracking zone
)

// trustedLegs are the host ends of the router and DHCP legs on a bridge, accepted unfiltered as
// upstream accepts its network:* ports: lr- for a router's interface, ld- for a network's DHCP.
var trustedLegs = []string{"lr+", "ld+"}

// PortDevice is a port's TAP, as Nova names it in the domain: tap and the port id's first eleven
// characters, upstream's name.
func PortDevice(portID string) string { return legName("tap", portID) }

// portChain is one of a port's chains: kind i (ingress), o (egress) or s (its addresses), then the
// port id's first ten characters.
func portChain(kind, portID string) string {
	if len(portID) > 10 {
		portID = portID[:10]
	}
	return "ls-" + kind + portID
}

// isPortChain reports whether a chain is a port's: ls-i, ls-o or ls-s and ten characters.
func isPortChain(name string) bool {
	if len(name) != len("ls-i")+10 || name == chainFallback {
		return false
	}
	return strings.HasPrefix(name, "ls-i") || strings.HasPrefix(name, "ls-o") || strings.HasPrefix(name, "ls-s")
}

// portFirewallRules is the firewall of ports in one family, "IPv4" or "IPv6": every chain Lightstack
// keeps, by table and chain, each as `iptables -S` prints it. The fixed chains are always there; a
// node without ports holds them empty of dispatch. nodeBridges are the networks' bridges the node holds,
// whose guest TAPs without a bound port are dropped.
func portFirewallRules(ports []FirewallPort, nodeBridges []string, family string) map[string]map[string][]string {
	sorted := slices.Clone(ports)
	slices.SortFunc(sorted, func(a, b FirewallPort) int { return strings.Compare(a.ID, b.ID) })
	filter := map[string][]string{
		chainTop:      {"-A " + chainTop + " -j " + chainLocal},
		chainLocal:    localChain(sorted, guardedBridges(sorted, nodeBridges)),
		chainSG:       sgChain(sorted),
		chainFallback: {"-A " + chainFallback + " -j DROP"},
	}
	filter[chainBoundIn], filter[chainBoundOut] = boundChains(sorted)
	for _, p := range sorted {
		filter[portChain("i", p.ID)] = ingressChain(p, family)
		filter[portChain("o", p.ID)] = egressChain(p, family)
		if checksAddresses(p, family) {
			filter[portChain("s", p.ID)] = spoofChain(p, family)
		}
	}
	return map[string]map[string][]string{"filter": filter, "raw": {chainZone: zoneChain(sorted)}}
}

// guardedBridges are the bridges whose guest TAPs are guarded, sorted: the networks' bridges the node
// holds and those its ports join — a network's only, so a TAP of another program on a bridge of its
// own keeps its traffic (SPEC-033 T213).
func guardedBridges(ports []FirewallPort, nodeBridges []string) []string {
	bridges := slices.Clone(nodeBridges)
	for _, p := range ports {
		if p.Bridge != "" {
			bridges = append(bridges, p.Bridge)
		}
	}
	slices.Sort(bridges)
	return slices.Compact(bridges)
}

// localChain is ls-local. A guest's TAP whose port is bound to no node, its port deleted while the
// guest runs, has no chains, so on the guarded bridges every frame from or to a TAP passes the bound
// ports' guards first: they drop such a TAP's frames before a port's dispatch can accept them, which
// would run the bound end's chain alone (SPEC-033 T248). The guard on frames from a TAP does not ask
// for --physdev-is-bridged, so it also takes those the bridge delivers to this host, which INPUT brings
// here and nothing else would stop (T314). Then each port's dispatch, then the trusted legs.
func localChain(ports []FirewallPort, bridges []string) []string {
	var local []string
	for _, b := range bridges {
		local = append(local,
			"-A "+chainLocal+" -i "+b+" -m physdev --physdev-in tap+ -j "+chainBoundIn,
			"-A "+chainLocal+" -o "+b+" -m physdev --physdev-out tap+ --physdev-is-bridged -j "+chainBoundOut)
	}
	for _, p := range ports {
		dev := PortDevice(p.ID)
		local = append(local,
			"-A "+chainLocal+" -m physdev --physdev-out "+dev+" --physdev-is-bridged -j "+chainSG,
			"-A "+chainLocal+" -m physdev --physdev-in "+dev+" --physdev-is-bridged -j "+chainSG,
			// Traffic from the port to this host, which INPUT brings here: its egress chain alone.
			"-A "+chainLocal+" -m physdev --physdev-in "+dev+" -j "+portChain("o", p.ID))
	}
	for _, leg := range trustedLegs {
		local = append(local,
			"-A "+chainLocal+" -m physdev --physdev-out "+leg+" --physdev-is-bridged -j ACCEPT",
			"-A "+chainLocal+" -m physdev --physdev-in "+leg+" --physdev-is-bridged -j ACCEPT")
	}
	return local
}

// boundChains are ls-bound-in and ls-bound-out: a bound port's TAP returns, any other is dropped.
func boundChains(ports []FirewallPort) (in, out []string) {
	for _, p := range ports {
		dev := PortDevice(p.ID)
		in = append(in, "-A "+chainBoundIn+" -m physdev --physdev-in "+dev+" -j RETURN")
		out = append(out, "-A "+chainBoundOut+" -m physdev --physdev-out "+dev+" -j RETURN")
	}
	return append(in, "-A "+chainBoundIn+" -j DROP"), append(out, "-A "+chainBoundOut+" -j DROP")
}

// sgChain is ls-sg-chain: a frame to a port goes through its ingress chain, one from a port through
// its egress chain, and what returns from both is accepted.
func sgChain(ports []FirewallPort) []string {
	var sg []string
	for _, p := range ports {
		dev := PortDevice(p.ID)
		sg = append(sg,
			"-A "+chainSG+" -m physdev --physdev-out "+dev+" --physdev-is-bridged -j "+portChain("i", p.ID),
			"-A "+chainSG+" -m physdev --physdev-in "+dev+" --physdev-is-bridged -j "+portChain("o", p.ID))
	}
	return append(sg, "-A "+chainSG+" -j ACCEPT")
}

// zoneChain is the raw table's ls-ct: each port's traffic in its network's zone, a stateless port's
// untracked, then each bridge of the ports' in its network's zone.
func zoneChain(ports []FirewallPort) []string {
	var zone []string
	bridges := map[string]int{}
	for _, p := range ports {
		target := "-j CT --zone " + strconv.Itoa(p.Zone)
		if p.Stateless {
			target = "-j CT --notrack"
		}
		zone = append(zone, "-A "+chainZone+" -m physdev --physdev-in "+PortDevice(p.ID)+" "+target)
		if p.Bridge != "" {
			bridges[p.Bridge] = p.Zone
		}
	}
	for _, b := range slices.Sorted(maps.Keys(bridges)) {
		zone = append(zone, "-A "+chainZone+" -i "+b+" -j CT --zone "+strconv.Itoa(bridges[b]))
	}
	return zone
}

// ingressChain is a port's ingress chain: DHCP answers to its addresses and to broadcast in IPv4,
// upstream's ICMPv6 and DHCPv6 answers in IPv6, then the port's ingress rules.
func ingressChain(p FirewallPort, family string) []string {
	c := portChain("i", p.ID)
	var lines []string
	if family == "IPv6" {
		for _, icmp := range []int{130, 131, 132, 135, 136} {
			lines = append(lines, fmt.Sprintf("-A %s -p ipv6-icmp -m icmp6 --icmpv6-type %d -j RETURN", c, icmp))
		}
		lines = append(lines, "-A "+c+" -p udp -m udp --sport 547 --dport 546 -j RETURN")
	} else {
		for _, a := range familyAddresses(p.Addresses, family) {
			lines = append(lines, "-A "+c+" -d "+a+" -p udp -m udp --sport 67 --dport 68 -j RETURN")
		}
		lines = append(lines, "-A "+c+" -d 255.255.255.255/32 -p udp -m udp --sport 67 --dport 68 -j RETURN")
	}
	return append(lines, ruleBlock(c, p, family, "ingress")...)
}

// egressChain is a port's egress chain: its DHCP requests, and in IPv6 the neighbour discovery of
// duplicate address detection, before its addresses are checked; after the check its DHCP requests
// again, a DHCP or router answer from it dropped, its ICMPv6 allowed; then its egress rules.
func egressChain(p FirewallPort, family string) []string {
	c, spoof := portChain("o", p.ID), portChain("s", p.ID)
	check := func() []string {
		if checksAddresses(p, family) {
			return []string{"-A " + c + " -j " + spoof}
		}
		return nil
	}
	var lines []string
	if family == "IPv6" {
		for _, icmp := range []int{131, 135, 143} {
			lines = append(lines, fmt.Sprintf("-A %s -s ::/128 -d ff02::/16 -p ipv6-icmp -m icmp6 --icmpv6-type %d -j RETURN", c, icmp))
		}
		lines = append(lines, check()...)
		lines = append(lines,
			"-A "+c+" -p ipv6-icmp -m icmp6 --icmpv6-type 134 -j DROP",
			"-A "+c+" -p ipv6-icmp -j RETURN",
			"-A "+c+" -p udp -m udp --sport 546 --dport 547 -j RETURN",
			"-A "+c+" -p udp -m udp --sport 547 --dport 546 -j DROP")
	} else {
		lines = append(lines, "-A "+c+" -s 0.0.0.0/32 -d 255.255.255.255/32 -p udp -m udp --sport 68 --dport 67 -j RETURN")
		lines = append(lines, check()...)
		lines = append(lines,
			"-A "+c+" -p udp -m udp --sport 68 --dport 67 -j RETURN",
			"-A "+c+" -p udp -m udp --sport 67 --dport 68 -j DROP")
	}
	return append(lines, ruleBlock(c, p, family, "egress")...)
}

// checksAddresses reports whether a port's egress in family goes through an address check, as
// upstream's iptables driver gives one: for the family's addresses the port has, or for its MAC when it
// has no address at all. A port with addresses, none of the family, has no check in it — its IPv6
// link-local traffic on an IPv4-only network goes on to its rules (SPEC-033 T214).
func checksAddresses(p FirewallPort, family string) bool {
	return len(p.Addresses) == 0 || len(familyAddresses(p.Addresses, family)) > 0
}

// spoofChain is a port's address check: its addresses of the family with its MAC, in IPv6 with the
// link-local address its MAC gives it, as upstream's; its MAC alone when it has no address at all. The
// MAC is written in lower case, as iptables-save prints it back, so a node holding the chain compares
// equal and is not written again (SPEC-033 T228).
func spoofChain(p FirewallPort, family string) []string {
	c := portChain("s", p.ID)
	mac := strings.ToLower(p.MAC)
	var lines []string
	if len(p.Addresses) == 0 {
		lines = append(lines, "-A "+c+" -m mac --mac-source "+mac+" -j RETURN")
	}
	addresses := familyAddresses(p.Addresses, family)
	if family == "IPv6" && len(addresses) > 0 {
		if lla, ok := linkLocalOf(p.MAC); ok && !slices.Contains(addresses, lla) {
			addresses = append(addresses, lla)
			slices.SortFunc(addresses, comparePrefixes)
		}
	}
	for _, a := range addresses {
		lines = append(lines, "-A "+c+" -s "+a+" -m mac --mac-source "+mac+" -j RETURN")
	}
	return append(lines, "-A "+c+" -j DROP")
}

// ruleBlock is a chain's end: replies, one RETURN per rule of the direction and family, sorted and
// each once, INVALID dropped, and the rest to the fallback. A stateless port's has neither the reply
// nor the INVALID rule (FR-051). A rule the renderer cannot express exactly is left out and logged
// (FR-059): a rule only ever allows, so leaving one out allows less, where writing it would put text
// the rule's owner chose into iptables-restore's input, or fail the node's whole commit.
func ruleBlock(chain string, p FirewallPort, family, direction string) []string {
	var lines []string
	if !p.Stateless {
		lines = append(lines, "-A "+chain+" -m state --state RELATED,ESTABLISHED -j RETURN")
	}
	var allow []string
	for _, r := range p.Rules {
		ethertype := r.Ethertype
		if ethertype == "" {
			ethertype = "IPv4"
		}
		if r.Direction != direction || ethertype != family {
			continue
		}
		match, ok := ruleMatch(r, direction, family)
		if !ok {
			log.Error().Str("port_id", p.ID).Str("direction", r.Direction).Str("ethertype", ethertype).
				Str("protocol", r.Protocol).Str("remote", r.Remote).
				Msg("a rule of the port's groups cannot be expressed in its firewall; it is left out")
			continue
		}
		allow = append(allow, "-A "+chain+" "+match+"-j RETURN")
	}
	slices.Sort(allow)
	lines = append(lines, slices.Compact(allow)...)
	if !p.Stateless {
		lines = append(lines, "-A "+chain+" -m state --state INVALID -j DROP")
	}
	return append(lines, "-A "+chain+" -j "+chainFallback)
}

// ruleMatch is a rule's match as `iptables -S` prints it, followed by a space, or "" for a rule
// matching everything: the remote, as the source of ingress and the destination of egress; the
// protocol; its ports, or its ICMP type and code. ok is false for a rule it cannot express exactly —
// each of which upstream refuses to store (FR-059): a remote that is no address or prefix of the
// family, a protocol upstream does not name, ports the protocol has none of or out of range.
func ruleMatch(r FirewallRule, direction, family string) (string, bool) {
	remote, ok := canonicalSource(r.Remote, family)
	if !ok {
		return "", false
	}
	proto, ok := ruleProtocol(r.Protocol, family)
	if !ok {
		return "", false
	}
	ports, ok := portMatch(proto, r.PortMin, r.PortMax)
	if !ok {
		return "", false
	}
	var b strings.Builder
	if remote != "" {
		flag := "-s "
		if direction == "egress" {
			flag = "-d "
		}
		b.WriteString(flag + remote + " ")
	}
	if proto != "" {
		b.WriteString("-p " + proto + " ")
	}
	return b.String() + ports, true
}

// portMatch is the match of a rule's ports, or of its ICMP type and code, followed by a space, or ""
// for none. A rule naming a first port alone matches that port. ok is false for ports on a protocol
// without them, a last port or a code without a first port or a type, and a value out of range.
func portMatch(proto string, first, last *int) (string, bool) {
	if first == nil {
		return "", last == nil
	}
	lo, hi := *first, *first
	if last != nil {
		hi = *last
	}
	switch proto {
	case "tcp", "udp", "sctp", "dccp", "udplite":
		if lo < 1 || hi < lo || hi > 65535 {
			return "", false
		}
		if proto == "udplite" { // it has no port match of its own; upstream's driver uses multiport too
			return "-m multiport --dports " + portSpec(lo, hi) + " ", true
		}
		return "-m " + proto + " --dport " + portSpec(lo, hi) + " ", true
	case "icmp", "ipv6-icmp":
		if lo < 0 || lo > 255 || (last != nil && (*last < 0 || *last > 255)) {
			return "", false
		}
		if proto == "icmp" {
			if lo == 255 && last == nil { // iptables reads type 255 alone as any type, and prints it so (T277)
				return "-m icmp --icmp-type any ", true
			}
			return "-m icmp --icmp-type " + icmpSpec(lo, last) + " ", true
		}
		return "-m icmp6 --icmpv6-type " + icmpSpec(lo, last) + " ", true
	}
	return "", false
}

// iptablesProtocolNames are the protocols `iptables -S` and `ip6tables -S` print by name, the table
// built into iptables (xtables_chain_protos); they print any other by number. The restore reads these
// names without /etc/protocols, which the image lacks, and refuses any other (SPEC-033 T249).
var iptablesProtocolNames = map[int]string{
	1: "icmp", 6: "tcp", 17: "udp", 33: "dccp", 50: "esp", 51: "ah", 58: "ipv6-icmp", 108: "ipcomp",
	132: "sctp", 135: "mobility-header", 136: "udplite",
}

// ruleProtocol is a rule's protocol as `iptables -S` prints it: "" for any, protocol 0 included; ICMP
// of an IPv6 rule is ipv6-icmp, as upstream converts it; others by name where iptables prints one,
// else by number. A name stands for upstream's number for it, which is not always what iptables
// would read it as: iptables reads "ipip" as 94, upstream's ipip is 4. ok is false for anything
// ProtocolNumber does not accept.
func ruleProtocol(p, family string) (string, bool) {
	if p == "" || strings.EqualFold(p, "any") {
		return "", true
	}
	n, ok := ProtocolNumber(p)
	switch {
	case !ok:
		return "", false
	case n == 0:
		return "", true
	case family == "IPv6" && (n == 1 || n == 58):
		return "ipv6-icmp", true
	}
	if name, ok := iptablesProtocolNames[n]; ok {
		return name, true
	}
	return strconv.Itoa(n), true
}

// icmpSpec is an ICMP type, with its code when the rule names one.
func icmpSpec(icmpType int, code *int) string {
	if code == nil {
		return strconv.Itoa(icmpType)
	}
	return strconv.Itoa(icmpType) + "/" + strconv.Itoa(*code)
}

// familyAddresses are a port's addresses of one family as hosts, /32 or /128, sorted.
func familyAddresses(addresses []string, family string) []string {
	var out []string
	for _, s := range addresses {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil || a.Unmap().Is4() != (family == "IPv4") {
			continue
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()).String())
	}
	slices.SortFunc(out, comparePrefixes)
	return out
}

// comparePrefixes orders host prefixes by address.
func comparePrefixes(a, b string) int {
	x, errA := netip.ParsePrefix(a)
	y, errB := netip.ParsePrefix(b)
	if errA != nil || errB != nil {
		return strings.Compare(a, b)
	}
	return x.Addr().Compare(y.Addr())
}

// linkLocalOf is the IPv6 link-local address a MAC gives a port, by EUI-64, as a /128.
func linkLocalOf(mac string) (string, bool) {
	var b [6]byte
	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		return "", false
	}
	for i, part := range parts {
		v, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return "", false
		}
		b[i] = byte(v)
	}
	a := netip.AddrFrom16([16]byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, b[0] ^ 0x02, b[1], b[2], 0xff, 0xfe, b[3], b[4], b[5]})
	return netip.PrefixFrom(a, 128).String(), true
}

// firewallJumps are the jumps from the built-in chains to Lightstack's, by table: each first in its
// chain.
var firewallJumps = map[string][][2]string{
	"filter": {{"FORWARD", chainTop}, {"INPUT", chainTop}},
	"raw":    {{"PREROUTING", chainZone}},
}

// firewallPayload is the restore input that brings the node's tables from current to desired, one
// commit per table that differs (tablePayload). "" when the node holds desired already.
func firewallPayload(current map[string]savedTable, desired map[string]map[string][]string) string {
	var b strings.Builder
	for _, table := range []string{"filter", "raw"} {
		b.WriteString(tablePayload(table, current[table], desired[table]))
	}
	return b.String()
}

// tablePayload is one table's commit: the chains that differ declared, emptied and filled; the port
// chains no longer wanted emptied and then deleted, after every chain that jumps to them was rewritten;
// and the jumps from the built-in chains put first where they are not. "" when cur holds want already.
func tablePayload(table string, cur savedTable, want map[string][]string) string {
	write, drop := changedChains(cur, want)
	var fixes []string
	for _, j := range firewallJumps[table] {
		fixes = append(fixes, jumpFixes(cur, j[0], j[1])...)
	}
	if len(write)+len(drop)+len(fixes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("*" + table + "\n")
	for _, name := range write {
		b.WriteString(":" + name + " - [0:0]\n")
	}
	for _, name := range write {
		b.WriteString("-F " + name + "\n")
		for _, r := range want[name] {
			b.WriteString(r + "\n")
		}
	}
	for _, name := range drop {
		b.WriteString("-F " + name + "\n")
	}
	for _, name := range drop {
		b.WriteString("-X " + name + "\n")
	}
	for _, f := range fixes {
		b.WriteString(f + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// changedChains are the chains of want that cur lacks or holds otherwise, and cur's port chains want no
// longer has, each sorted.
func changedChains(cur savedTable, want map[string][]string) (write, drop []string) {
	for name, rules := range want {
		// A chain the node lacks is declared even when it is to hold no rule: the raw table's ls-ct on a
		// node with no bound port, which PREROUTING's jump needs (SPEC-033 T205).
		if !slices.Contains(cur.chains, name) || !slices.Equal(cur.rules[name], rules) {
			write = append(write, name)
		}
	}
	for _, name := range cur.chains {
		if isPortChain(name) && want[name] == nil {
			drop = append(drop, name)
		}
	}
	slices.Sort(write)
	slices.Sort(drop)
	return write, drop
}

// jumpFixes puts a built-in chain's jump to target first, once: nothing when it is, else each copy
// deleted and one inserted at the top.
func jumpFixes(cur savedTable, builtin, target string) []string {
	jump := "-A " + builtin + " -j " + target
	rules := cur.rules[builtin]
	copies := 0
	for _, r := range rules {
		if r == jump {
			copies++
		}
	}
	if copies == 1 && rules[0] == jump {
		return nil
	}
	var out []string
	for range copies {
		out = append(out, "-D "+builtin+" -j "+target)
	}
	return append(out, "-I "+builtin+" 1 -j "+target)
}

// FirewallManager converges the firewall of the ports bound to this node (R32), in iptables and ebpf
// mode alike (FR-047). Stub mode programs nothing.
type FirewallManager struct {
	mode string
	// run runs the node's commands; tests replace it.
	run commandRunner
}

// NewFirewallManager creates the node's port firewall.
func NewFirewallManager(mode string) *FirewallManager {
	return &FirewallManager{mode: mode, run: execRunner}
}

// ConvergePorts makes the node's firewall hold ports, exactly: every port bound to it, so the chains
// of a port not among them are removed. Per family it reads the filter and raw tables once and writes
// what differs in one restore, every command within ctx's deadline and a second.
func (m *FirewallManager) ConvergePorts(ctx context.Context, ports []FirewallPort) error {
	if m.mode == "stub" {
		return nil
	}
	// The networks' bridges the node holds, whose TAPs without a bound port are dropped.
	out, err := runCommand(ctx, m.run, "", "ip", "-j", "link", "show", "type", "bridge")
	if err != nil {
		return fmt.Errorf("list the node's bridges: %w", err)
	}
	var nodeBridges []string
	for name := range parseLinks(out) {
		if isManagedBridge(name) {
			nodeBridges = append(nodeBridges, name)
		}
	}
	for _, f := range []struct{ family, save, restore string }{
		{"IPv4", "iptables-save", "iptables-restore"},
		{"IPv6", "ip6tables-save", "ip6tables-restore"},
	} {
		current := map[string]savedTable{}
		for _, table := range []string{"filter", "raw"} {
			out, err := runCommand(ctx, m.run, "", f.save, "-t", table)
			if err != nil {
				return fmt.Errorf("read the %s %s table: %w", f.family, table, err)
			}
			current[table] = parseSavedTable(string(out))
		}
		payload := firewallPayload(current, portFirewallRules(ports, nodeBridges, f.family))
		if payload == "" {
			continue
		}
		if _, err := runCommand(ctx, m.run, payload, f.restore, "--noflush", "--wait", "1"); err != nil {
			return fmt.Errorf("write the %s port firewall: %w", f.family, err)
		}
	}
	return nil
}

// RemoveAll removes the node's whole port firewall (research R48): every port's chains, the fixed
// chains, the routers' forward chain and the chains of earlier layouts, with every jump to them, in the
// filter and raw tables of both families, one restore per family. A chain it does not recognise, and
// every rule that is not a jump to one it removes, stay. Teardown's; reconciliation keeps the fixed
// chains.
func (m *FirewallManager) RemoveAll(ctx context.Context) error {
	if m.mode == "stub" {
		return nil
	}
	for _, f := range []struct{ family, save, restore string }{
		{"IPv4", "iptables-save", "iptables-restore"},
		{"IPv6", "ip6tables-save", "ip6tables-restore"},
	} {
		var payload strings.Builder
		for _, table := range []string{"filter", "raw"} {
			out, err := runCommand(ctx, m.run, "", f.save, "-t", table)
			if err != nil {
				return fmt.Errorf("read the %s %s table: %w", f.family, table, err)
			}
			payload.WriteString(chainRemovalPayload(table, parseSavedTable(string(out)), isLightstackChain, nil))
		}
		if payload.Len() == 0 {
			continue
		}
		if _, err := runCommand(ctx, m.run, payload.String(), f.restore, "--noflush", "--wait", "1"); err != nil {
			return fmt.Errorf("remove the %s port firewall: %w", f.family, err)
		}
	}
	return nil
}

// isLightstackChain reports whether a host chain is Lightstack's: a fixed chain of the port firewall,
// the routers' forward chain, a port's chain, or one of an earlier layout.
func isLightstackChain(name string) bool {
	switch name {
	case chainTop, chainLocal, chainBoundIn, chainBoundOut, chainSG, chainFallback, chainZone, forwardChain:
		return true
	}
	return isPortChain(name) || isRetiredChain(name)
}

// canonicalSource is a rule's remote as iptables prints it: a network with its prefix length, a host
// as /32 or /128, and "" for any. ok is false for a remote of the other family and for anything else
// — a name, an address with a zone, a netmask, more text — since the API stores a remote in exactly
// this form and a remote group's members are addresses (FR-059).
func canonicalSource(remote, family string) (string, bool) {
	if remote == "" {
		return "", true
	}
	prefix, err := netip.ParsePrefix(remote)
	if err != nil {
		addr, aerr := netip.ParseAddr(remote)
		if aerr != nil || addr.Zone() != "" {
			return "", false
		}
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}
	if prefix.Addr().Is4() != (family == "IPv4") {
		return "", false
	}
	if prefix.Bits() == 0 {
		return "", true
	}
	return prefix.Masked().String(), true
}

// portSpec is a port range as iptables prints it: one port, or first:last.
func portSpec(lo, hi int) string {
	if hi == 0 || hi == lo {
		return strconv.Itoa(lo)
	}
	return strconv.Itoa(lo) + ":" + strconv.Itoa(hi)
}
