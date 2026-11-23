package networking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// A router on the network node (SPEC-033 FR-033, FR-035, FR-037, FR-038, FR-049; research R28, R37;
// contracts/router-namespace-dataplane.md): upstream's legacy L3 layout under Lightstack's names. The
// router's namespace, qrouter-<id>, holds a leg to the host for the gateway and one to each
// interface's bridge, the router's addresses and default route, and nat and mangle tables that are
// Lightstack's alone. The host routes the router's external addresses to the gateway's leg and
// answers ARP for them on the uplink, so the uplink's own configuration is never touched.
// ConvergeRouter makes the node hold what the router's records say, or nothing of the router.

// RouterState is a router as its records say, and so as the network node must hold it.
type RouterState struct {
	ID              string
	Gateway         *RouterGateway // nil without a gateway
	Interfaces      []RouterInterface
	FloatingIPs     []RouterFloatingIP     // floating IPs on a port, through this router
	PortForwardings []RouterPortForwarding // on floating IPs through this router
}

// RouterGateway is a router's gateway port on its external network.
type RouterGateway struct {
	PortID    string   // names the gateway's leg
	Addresses []string // the port's addresses, each with its subnet's prefix
	NextHop   string   // the external IPv4 subnet's gateway, the namespace's default route; "" for none
	SNAT      bool     // enable_snat
}

// RouterInterface is one of a router's interfaces: its port, holding its subnet's gateway.
type RouterInterface struct {
	PortID    string   // names the interface's leg
	Addresses []string // the port's fixed IPs, each with its subnet's prefix
	Bridge    string   // the network's bridge
}

// RouterFloatingIP is a floating IP through the router, at a fixed IP.
type RouterFloatingIP struct{ Address, FixedIP string }

// RouterPortForwarding is a port forwarding on a floating IP through the router.
type RouterPortForwarding struct {
	ID           string
	FloatingIP   string
	Protocol     string // tcp or udp
	ExternalPort int
	InternalIP   string
	InternalPort int
}

// Leg is a veth between a router's namespace and the host: Inside in the namespace, Outside on the
// host, enslaved to Bridge for an interface. The gateway's leg has no bridge.
type Leg struct{ Inside, Outside, Bridge string }

// RouterPlan is what a router asks of the node apart from its tables.
type RouterPlan struct {
	Namespace     string
	Legs          []Leg               // the gateway's first, then the interfaces' in port id order
	Addresses     map[string][]string // per leg in the namespace: the gateway's or the interface's addresses
	ExternalLeg   string              // the gateway's leg in the namespace; "" without a gateway
	Floating      []string            // the floating addresses, F/32 on ExternalLeg
	DefaultRoute  string              // the next hop on ExternalLeg; "" for none
	HostLeg       string              // the gateway's leg on the host; "" without a gateway
	HostAddresses []string            // the router's external IPv4 addresses: routed to HostLeg, proxied on the uplink
}

// RouterNamespace is a router's namespace on the network node.
func RouterNamespace(routerID string) string { return "qrouter-" + routerID }

// legName is a leg's device name: its prefix and a port id's first eleven characters, as upstream's,
// so it fits a device name's fifteen.
func legName(prefix, portID string) string {
	if len(portID) > 11 {
		portID = portID[:11]
	}
	return prefix + portID
}

// hostEnd is the host's end of a leg in a namespace: lg- for qg- and lr- for qr- in a router's, ld-
// for a DHCP namespace's tap; "" for any other device, which is not Lightstack's.
func hostEnd(inside string) string {
	switch {
	case strings.HasPrefix(inside, "qg-"):
		return "lg-" + strings.TrimPrefix(inside, "qg-")
	case strings.HasPrefix(inside, "qr-"):
		return "lr-" + strings.TrimPrefix(inside, "qr-")
	case strings.HasPrefix(inside, "tap"):
		return "ld-" + strings.TrimPrefix(inside, "tap")
	}
	return ""
}

// PlanRouter is the router's devices, addresses and routes. Floating addresses exist only with a
// gateway: a floating IP needs one (FR-039).
func PlanRouter(s RouterState) RouterPlan {
	p := RouterPlan{Namespace: RouterNamespace(s.ID), Addresses: map[string][]string{}}
	if g := s.Gateway; g != nil {
		p.ExternalLeg, p.HostLeg = legName("qg-", g.PortID), legName("lg-", g.PortID)
		p.Legs = append(p.Legs, Leg{Inside: p.ExternalLeg, Outside: p.HostLeg})
		p.Addresses[p.ExternalLeg] = slices.Clone(g.Addresses)
		p.DefaultRoute = g.NextHop
		host := map[string]bool{}
		if gw, ok := gatewaySource(g); ok {
			host[gw] = true
		}
		floating := map[string]bool{}
		for _, f := range s.FloatingIPs {
			floating[f.Address] = true
		}
		for _, pf := range s.PortForwardings {
			floating[pf.FloatingIP] = true
		}
		for _, f := range sortedAddresses(floating) {
			p.Floating = append(p.Floating, f+"/32")
			host[f] = true
		}
		p.HostAddresses = sortedAddresses(host)
	}
	interfaces := slices.Clone(s.Interfaces)
	slices.SortFunc(interfaces, func(a, b RouterInterface) int { return strings.Compare(a.PortID, b.PortID) })
	for _, i := range interfaces {
		inside := legName("qr-", i.PortID)
		p.Legs = append(p.Legs, Leg{Inside: inside, Outside: legName("lr-", i.PortID), Bridge: i.Bridge})
		p.Addresses[inside] = slices.Clone(i.Addresses)
	}
	return p
}

// gatewaySource is the gateway's first IPv4 address, the source of its SNAT, as upstream's.
func gatewaySource(g *RouterGateway) (string, bool) {
	for _, a := range g.Addresses {
		if prefix, err := netip.ParsePrefix(a); err == nil && prefix.Addr().Is4() {
			return prefix.Addr().String(), true
		}
	}
	return "", false
}

// sortedAddresses orders addresses as addresses, not as strings.
func sortedAddresses(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	slices.SortFunc(out, compareAddresses)
	return out
}

func compareAddresses(a, b string) int {
	x, errA := netip.ParseAddr(a)
	y, errB := netip.ParseAddr(b)
	if errA != nil || errB != nil {
		return strings.Compare(a, b)
	}
	return x.Compare(y)
}

const (
	lightstackComment = "-m comment --comment lightstack"
	// routerMark marks traffic in from the gateway's leg, so a reply to it is not SNATed to the
	// gateway on its way back out (upstream's external_ingress_mark and ROUTER_MARK_MASK).
	routerMark = "0x2/0xffff"
)

// portForwardingChain is a port forwarding's chain, upstream's pf-<id> under Lightstack's prefix.
func portForwardingChain(id string) string {
	if len(id) > 8 {
		id = id[:8]
	}
	return "ls-pf-" + id
}

// natRule writes one rule of a chain, its parts joined by spaces.
type natRule func(chain string, parts ...string)

// natChains are the chains the namespace's nat table always declares.
var natChains = []string{"ls-l3-PREROUTING", "ls-l3-OUTPUT", "ls-l3-POSTROUTING", "ls-l3-snat", "ls-l3-float-snat", "ls-l3-fip-pf"}

// renderRouterNAT is the namespace's nat table, whole, for iptables-restore: the namespace is
// Lightstack's alone, so the table is replaced in one commit (R28). Upstream's rules: per floating IP
// a DNAT both ways and its SNAT; with a gateway, traffic between the router's own networks is left
// alone, a reply to traffic that came in through a floating IP leaves through the gateway (the
// hairpin), and with SNAT on, the rest leaves as the gateway; port forwardings as upstream's.
func renderRouterNAT(s RouterState) string {
	var b strings.Builder
	rule := func(chain string, parts ...string) {
		b.WriteString("-A " + chain + " " + strings.Join(parts, " ") + "\n")
	}
	floating, forwardings := natTargets(s)
	b.WriteString("*nat\n:PREROUTING ACCEPT [0:0]\n:INPUT ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\n:POSTROUTING ACCEPT [0:0]\n")
	for _, chain := range natChains {
		b.WriteString(":" + chain + " - [0:0]\n")
	}
	for _, pf := range forwardings {
		b.WriteString(":" + portForwardingChain(pf.ID) + " - [0:0]\n")
	}
	rule("PREROUTING", lightstackComment, "-j ls-l3-PREROUTING")
	rule("OUTPUT", lightstackComment, "-j ls-l3-OUTPUT")
	rule("POSTROUTING", lightstackComment, "-j ls-l3-POSTROUTING")
	rule("POSTROUTING", lightstackComment, "-j ls-l3-snat")
	rule("ls-l3-PREROUTING", lightstackComment, "-j ls-l3-fip-pf")
	for _, f := range floating {
		rule("ls-l3-PREROUTING", "-d "+f.Address+"/32", lightstackComment, "-j DNAT --to-destination "+f.FixedIP)
	}
	for _, f := range floating {
		rule("ls-l3-OUTPUT", "-d "+f.Address+"/32", lightstackComment, "-j DNAT --to-destination "+f.FixedIP)
	}
	natSNATRules(rule, s.Gateway, floating)
	natForwardingRules(rule, forwardings)
	b.WriteString("COMMIT\n")
	return b.String()
}

// natTargets are the router's floating IPs, by address, and port forwardings, by id: none without a
// gateway, which both need (FR-039).
func natTargets(s RouterState) ([]RouterFloatingIP, []RouterPortForwarding) {
	if s.Gateway == nil {
		return nil, nil
	}
	floating := slices.Clone(s.FloatingIPs)
	slices.SortFunc(floating, func(a, b RouterFloatingIP) int { return compareAddresses(a.Address, b.Address) })
	forwardings := slices.Clone(s.PortForwardings)
	slices.SortFunc(forwardings, func(a, b RouterPortForwarding) int { return strings.Compare(a.ID, b.ID) })
	return floating, forwardings
}

// natSNATRules are the rules on the way out: with a gateway, traffic between the router's own networks
// left alone, its SNAT when on, and the hairpin; each floating IP's SNAT.
func natSNATRules(rule natRule, g *RouterGateway, floating []RouterFloatingIP) {
	qg := ""
	if g != nil {
		qg = legName("qg-", g.PortID)
		rule("ls-l3-POSTROUTING", "! -o "+qg+" -m conntrack ! --ctstate DNAT", lightstackComment, "-j ACCEPT")
	}
	rule("ls-l3-snat", lightstackComment, "-j ls-l3-float-snat")
	if g != nil {
		if source, ok := gatewaySource(g); ok {
			if g.SNAT {
				rule("ls-l3-snat", "-o "+qg, lightstackComment, "-j SNAT --to-source "+source)
			}
			rule("ls-l3-snat", "-m mark ! --mark "+routerMark+" -m conntrack --ctstate DNAT", lightstackComment, "-j SNAT --to-source "+source)
		}
	}
	for _, f := range floating {
		rule("ls-l3-float-snat", "-s "+f.FixedIP+"/32", lightstackComment, "-j SNAT --to-source "+f.Address)
	}
}

// natForwardingRules are the port forwardings: a jump to each one's chain, and its DNAT there.
func natForwardingRules(rule natRule, forwardings []RouterPortForwarding) {
	for _, pf := range forwardings {
		rule("ls-l3-fip-pf", lightstackComment, "-j "+portForwardingChain(pf.ID))
	}
	for _, pf := range forwardings {
		proto := strings.ToLower(pf.Protocol)
		rule(portForwardingChain(pf.ID),
			fmt.Sprintf("-d %s/32 -p %s -m %s --dport %d", pf.FloatingIP, proto, proto, pf.ExternalPort),
			lightstackComment, fmt.Sprintf("-j DNAT --to-destination %s:%d", pf.InternalIP, pf.InternalPort))
	}
}

// renderRouterMangle is the namespace's mangle table, whole: traffic in from the gateway's leg is
// marked, as upstream's external_gateway_mangle_rules.
func renderRouterMangle(s RouterState) string {
	var b strings.Builder
	b.WriteString("*mangle\n:PREROUTING ACCEPT [0:0]\n:INPUT ACCEPT [0:0]\n:FORWARD ACCEPT [0:0]\n:OUTPUT ACCEPT [0:0]\n:POSTROUTING ACCEPT [0:0]\n")
	b.WriteString(":ls-l3-mark - [0:0]\n")
	b.WriteString("-A PREROUTING " + lightstackComment + " -j ls-l3-mark\n")
	if s.Gateway != nil {
		b.WriteString("-A ls-l3-mark -i " + legName("qg-", s.Gateway.PortID) + " " + lightstackComment + " -j MARK --set-xmark " + routerMark + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// savedTable is a table as iptables-save prints it: its chains, and each chain's rules in order.
// Counters, policies and comments are not compared.
type savedTable struct {
	chains []string
	rules  map[string][]string
}

func parseSavedTable(out string) savedTable {
	t := savedTable{rules: map[string][]string{}}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, ":"):
			name, _, _ := strings.Cut(line[1:], " ")
			t.chains = append(t.chains, name)
		case strings.HasPrefix(line, "-A "):
			chain, _, _ := strings.Cut(line[3:], " ")
			t.rules[chain] = append(t.rules[chain], line)
		}
	}
	slices.Sort(t.chains)
	return t
}

func (t savedTable) equal(o savedTable) bool {
	if !slices.Equal(t.chains, o.chains) || len(t.rules) != len(o.rules) {
		return false
	}
	for chain, rules := range t.rules {
		if !slices.Equal(rules, o.rules[chain]) {
			return false
		}
	}
	return true
}

// heldRouter is what the node holds of a router, as read before converging it.
type heldRouter struct {
	heldNamespace
	forwarding  bool   // the namespace forwards and answers ARP as a router's does
	liberal     bool   // connection tracking takes TCP windows liberally
	defaultVia  string // the default route's next hop
	defaultDev  string // and its device
	nat, mangle savedTable
	hostLeg     string          // the host's end of the gateway's leg found in the namespace
	hostRoutes  []string        // addresses the host routes to hostLeg
	proxies     map[string]bool // addresses proxied on the uplink
	proxyARP    bool            // hostLeg answers ARP for the namespace
}

// errNoUplink: a router with a gateway needs the host's uplink, which carries its proxy entries.
var errNoUplink = errors.New("no uplink for the router's gateway")

// ConvergeRouter makes the node hold the router as state says, or nothing of it when state is nil
// (R28, R34), within ctx's deadline, every command within a second besides. It reads the node first
// and writes only what differs: a router as its records say is only read. Adding goes namespace,
// legs, addresses, one restore per table, the floating addresses, then the host's routes and proxy
// entries; removing goes the other way. The first command to fail fails the convergence. uplink is
// the host's uplink, which carries the proxy entries. Stub mode runs nothing.
func (rm *RouterManager) ConvergeRouter(ctx context.Context, routerID string, state *RouterState, uplink string) error {
	if rm.mode == "stub" {
		return nil
	}
	if state != nil && state.Gateway != nil && uplink == "" {
		return fmt.Errorf("router %s: %w", routerID, errNoUplink)
	}
	ns := RouterNamespace(routerID)
	held, err := rm.readRouter(ctx, ns, uplink)
	if err != nil {
		return fmt.Errorf("router %s: %w", routerID, err)
	}
	if state == nil {
		err = rm.removeRouter(ctx, ns, held, uplink)
	} else {
		err = rm.applyRouter(ctx, *state, held, uplink)
	}
	if err != nil {
		return fmt.Errorf("router %s: %w", routerID, err)
	}
	return nil
}

// HeldRouters lists the routers whose namespaces the node holds, by the ids that name them: a qrouter-
// name with a lower-case UUID (heldNamespaces). Stub mode holds none.
func (rm *RouterManager) HeldRouters(ctx context.Context) ([]string, error) {
	if rm.mode == "stub" {
		return nil, nil
	}
	return heldNamespaces(ctx, rm.run, "qrouter-")
}

// heldNamespaces lists the node's namespaces named prefix and an id as Lightstack writes one, a
// lower-case UUID, by those ids: the one recognition of Lightstack's namespaces — routers', networks'
// DHCP and an older deployment's projects' — for the reconciler and teardown, which remove such a
// namespace when no record names it. Upstream's agents name theirs the same way, qrouter- or qdhcp- and
// the id, so nothing here tells theirs apart: a node runs Lightstack's network layout, not theirs as well.
func heldNamespaces(ctx context.Context, run commandRunner, prefix string) ([]string, error) {
	out, err := runCommand(ctx, run, "", "ip", "netns", "list")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if id, ok := strings.CutPrefix(name, prefix); ok && isID(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// isID reports whether s is an id as Lightstack writes one: a lower-case UUID.
func isID(s string) bool {
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return false
	}
	for i, n := range []int{8, 4, 4, 4, 12} {
		if !isLowerHex(parts[i], n) {
			return false
		}
	}
	return true
}

// RemoveStrayProxies removes the proxy entries on the uplink that no router's leg routes, among the
// addresses ours recognises as Lightstack's, and returns them (contracts/router-namespace-dataplane.md,
// Reconciliation). A router adds an address's route before its proxy entry and removes the entry before
// the route, so with the entries listed before the routes, an entry without a route is never one a
// router is adding: it was left by a router whose namespace went, out of band or on another node. A
// router whose address it was gets the entry back when it is next converged. IPv6 entries are never a
// router's. uplink "" and stub mode do nothing.
func (rm *RouterManager) RemoveStrayProxies(ctx context.Context, uplink string, ours func(string) bool) ([]string, error) {
	if rm.mode == "stub" || uplink == "" {
		return nil, nil
	}
	out, err := rm.runIn(ctx, "", "ip", "-j", "neigh", "show", "proxy", "dev", uplink)
	if err != nil {
		return nil, err
	}
	proxied := jsonField(out, "dst")
	if out, err = rm.runIn(ctx, "", "ip", "-4", "-j", "route", "show"); err != nil {
		return nil, err
	}
	var routes []struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal(out, &routes); err != nil {
		return nil, fmt.Errorf("read the routes: %w", err)
	}
	routed := map[string]bool{}
	for _, r := range routes {
		if strings.HasPrefix(r.Dev, "lg-") {
			routed[strings.TrimSuffix(r.Dst, "/32")] = true
		}
	}
	var removed []string
	for _, a := range proxied {
		if ip, err := netip.ParseAddr(a); err != nil || !ip.Is4() || routed[a] || !ours(a) {
			continue
		}
		if _, err := rm.runIn(ctx, "", "ip", "neigh", "del", "proxy", a, "dev", uplink); err != nil {
			return removed, err
		}
		removed = append(removed, a)
	}
	return removed, nil
}

// readRouter reads what the node holds of the router in namespace ns.
func (rm *RouterManager) readRouter(ctx context.Context, ns, uplink string) (heldRouter, error) {
	held := heldRouter{proxies: map[string]bool{}}
	var err error
	if held.heldNamespace, err = readNamespace(ctx, rm.run, ns); err != nil {
		return held, err
	}
	if uplink != "" {
		out, err := rm.runIn(ctx, "", "ip", "-j", "neigh", "show", "proxy", "dev", uplink)
		if err != nil {
			return held, err
		}
		for _, a := range jsonField(out, "dst") {
			held.proxies[a] = true
		}
	}
	if !held.exists {
		return held, nil
	}
	for dev := range held.addresses {
		if strings.HasPrefix(dev, "qg-") {
			held.hostLeg = hostEnd(dev)
		}
	}
	if err := rm.readRouterNamespace(ctx, ns, &held); err != nil {
		return held, err
	}
	if held.hostLeg == "" {
		return held, nil
	}
	return held, rm.readHostLeg(ctx, &held)
}

// readRouterNamespace reads into held what the namespace ns holds besides its devices: its sysctls, its
// default route, its nat and mangle tables.
func (rm *RouterManager) readRouterNamespace(ctx context.Context, ns string, held *heldRouter) error {
	// The namespace's sysctls are what a pass cut short leaves undone besides its devices (SPEC-033
	// T191): read on every pass, written where they differ.
	out, err := rm.runIn(ctx, "", "ip", "netns", "exec", ns, "sysctl", "-n",
		"net.ipv4.ip_forward", "net.ipv4.conf.all.arp_ignore", "net.ipv4.conf.all.arp_announce")
	if err != nil {
		return err
	}
	held.forwarding = slices.Equal(strings.Fields(string(out)), []string{"1", "1", "2"})
	// The key exists once the nat table has loaded connection tracking: missing reads as not set.
	out, err = rm.runIn(ctx, "", "ip", "netns", "exec", ns, "sysctl", "-n", "net.netfilter.nf_conntrack_tcp_be_liberal")
	held.liberal = err == nil && strings.TrimSpace(string(out)) == "1"
	if out, err = rm.runIn(ctx, "", "ip", "-n", ns, "-j", "route", "show", "default"); err != nil {
		return err
	}
	if via := jsonField(out, "gateway"); len(via) > 0 {
		held.defaultVia = via[0]
	}
	if dev := jsonField(out, "dev"); len(dev) > 0 {
		held.defaultDev = dev[0]
	}
	for _, table := range []struct {
		name string
		into *savedTable
	}{{"nat", &held.nat}, {"mangle", &held.mangle}} {
		if out, err = rm.runIn(ctx, "", "ip", "netns", "exec", ns, "iptables-save", "-t", table.name); err != nil {
			return err
		}
		*table.into = parseSavedTable(string(out))
	}
	return nil
}

// readHostLeg reads into held what the host holds for the gateway's leg: the addresses it routes to
// the leg, and whether the leg answers ARP for the namespace.
func (rm *RouterManager) readHostLeg(ctx context.Context, held *heldRouter) error {
	out, err := rm.runIn(ctx, "", "ip", "-j", "route", "show", "dev", held.hostLeg)
	if err != nil {
		return err
	}
	for _, dst := range jsonField(out, "dst") {
		held.hostRoutes = append(held.hostRoutes, strings.TrimSuffix(dst, "/32"))
	}
	if out, err = rm.runIn(ctx, "", "sysctl", "-n", "net.ipv4.conf."+held.hostLeg+".proxy_arp"); err != nil {
		return err
	}
	held.proxyARP = strings.TrimSpace(string(out)) == "1"
	return nil
}

// applyRouter converges the node from held to s, in phases whose order keeps what the router no longer
// holds out of its tables before it goes: the namespace and its legs, what is no longer wanted going
// first; the tables; the legs no longer wanted and the floating addresses; then the host's side.
func (rm *RouterManager) applyRouter(ctx context.Context, s RouterState, held heldRouter, uplink string) error {
	p := PlanRouter(s)
	if err := rm.runSteps(ctx, routerLegSteps(p, held, uplink)); err != nil {
		return err
	}
	// The tables, one commit each.
	for _, table := range []struct {
		payload string
		held    savedTable
	}{{renderRouterNAT(s), held.nat}, {renderRouterMangle(s), held.mangle}} {
		if parseSavedTable(table.payload).equal(table.held) {
			continue
		}
		if _, err := rm.runIn(ctx, table.payload, "ip", "netns", "exec", p.Namespace, "iptables-restore"); err != nil {
			return err
		}
	}
	var after [][]string
	if !held.liberal {
		// Its key exists once the nat table has loaded connection tracking.
		after = append(after, []string{"ip", "netns", "exec", p.Namespace, "sysctl", "-w", "net.netfilter.nf_conntrack_tcp_be_liberal=1"})
	}
	if err := rm.runSteps(ctx, append(after, routerFloatingSteps(p, held)...)); err != nil {
		return err
	}
	if p.HostLeg == "" {
		return nil
	}
	return rm.applyRouterHostSide(ctx, p, held, uplink)
}

// runSteps runs commands, each its program and arguments, in order; the first failure ends them.
func (rm *RouterManager) runSteps(ctx context.Context, steps [][]string) error {
	for _, step := range steps {
		if _, err := rm.runIn(ctx, "", step[0], step[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// ipSteps are ip's arguments as commands.
func ipSteps(args [][]string) [][]string {
	steps := make([][]string, 0, len(args))
	for _, a := range args {
		steps = append(steps, append([]string{"ip"}, a...))
	}
	return steps
}

// wantsLeg reports whether the plan has a leg named dev in the namespace.
func (p RouterPlan) wantsLeg(dev string) bool {
	return slices.ContainsFunc(p.Legs, func(leg Leg) bool { return leg.Inside == dev })
}

// routerLegSteps are the commands before the tables: the namespace and what a pass cut short left
// undone in it (SPEC-033 T191); the host's routes and proxy entries no longer wanted, then the
// addresses; the legs — a leg a pass cut short is finished: its bridge, both ends up — their addresses
// and the default route.
func routerLegSteps(p RouterPlan, held heldRouter, uplink string) [][]string {
	ns := p.Namespace
	steps := ipSteps(held.namespaceSteps(ns))
	if !held.forwarding {
		steps = append(steps, []string{"ip", "netns", "exec", ns, "sysctl", "-w", "net.ipv4.ip_forward=1", "net.ipv4.conf.all.arp_ignore=1", "net.ipv4.conf.all.arp_announce=2"})
	}
	steps = append(steps, staleHostRouteSteps(p, held, uplink)...)
	steps = append(steps, staleAddressSteps(p, held)...)
	for _, leg := range p.Legs {
		steps = append(steps, ipSteps(held.legSteps(ns, leg.Outside, leg.Inside, leg.Bridge))...)
	}
	for _, leg := range p.Legs {
		for _, a := range p.Addresses[leg.Inside] {
			if !slices.Contains(held.addresses[leg.Inside], a) {
				steps = append(steps, []string{"ip", "-n", ns, "addr", "replace", a, "dev", leg.Inside})
			}
		}
	}
	// The route is compared by its leg too: a gateway on a new port may have the same next hop, and the old
	// leg, which goes after the tables, takes the routes through it along (SPEC-033 T316).
	switch {
	case p.DefaultRoute != "" && (held.defaultVia != p.DefaultRoute || held.defaultDev != p.ExternalLeg):
		steps = append(steps, []string{"ip", "-n", ns, "route", "replace", "default", "via", p.DefaultRoute, "dev", p.ExternalLeg})
	case p.DefaultRoute == "" && held.defaultVia != "":
		steps = append(steps, []string{"ip", "-n", ns, "route", "del", "default"})
	}
	return steps
}

// staleHostRouteSteps remove the host's routes to the gateway's leg that the plan no longer wants, each
// address's proxy entry on the uplink before its route.
func staleHostRouteSteps(p RouterPlan, held heldRouter, uplink string) [][]string {
	var steps [][]string
	sameLeg := held.hostLeg == p.HostLeg
	for _, a := range held.hostRoutes {
		if sameLeg && slices.Contains(p.HostAddresses, a) {
			continue
		}
		if held.proxies[a] {
			steps = append(steps, []string{"ip", "neigh", "del", "proxy", a, "dev", uplink})
		}
		steps = append(steps, []string{"ip", "route", "del", a + "/32", "dev", held.hostLeg})
	}
	return steps
}

// staleAddressSteps remove the addresses the plan no longer wants from the legs it keeps; a leg it
// does not keep goes with its addresses, after the tables.
func staleAddressSteps(p RouterPlan, held heldRouter) [][]string {
	var steps [][]string
	for _, dev := range sortedKeys(held.addresses) {
		if !p.wantsLeg(dev) {
			continue
		}
		for _, a := range held.addresses[dev] {
			if !slices.Contains(p.Addresses[dev], a) && (dev != p.ExternalLeg || !slices.Contains(p.Floating, a)) {
				steps = append(steps, []string{"ip", "-n", p.Namespace, "addr", "del", a, "dev", dev})
			}
		}
	}
	return steps
}

// routerFloatingSteps are the commands after the tables, which no longer name what they remove: the
// legs no longer wanted, then the floating addresses.
func routerFloatingSteps(p RouterPlan, held heldRouter) [][]string {
	var steps [][]string
	for _, dev := range sortedKeys(held.addresses) {
		if outside := hostEnd(dev); outside != "" && !p.wantsLeg(dev) {
			steps = append(steps, []string{"ip", "link", "del", outside})
		}
	}
	for _, f := range p.Floating {
		if !slices.Contains(held.addresses[p.ExternalLeg], f) {
			steps = append(steps, []string{"ip", "-n", p.Namespace, "addr", "replace", f, "dev", p.ExternalLeg})
		}
	}
	return steps
}

// applyRouterHostSide converges the host's side of a router with a gateway: forwarding, proxy ARP on
// its host leg, the host's forward chain, then its routes and the uplink's proxy entries.
func (rm *RouterManager) applyRouterHostSide(ctx context.Context, p RouterPlan, held heldRouter, uplink string) error {
	if err := rm.enableForwarding(); err != nil {
		return err
	}
	sameLeg := held.hostLeg == p.HostLeg
	if !held.proxyARP || !sameLeg {
		if err := rm.runSteps(ctx, [][]string{{"sysctl", "-w", "net.ipv4.conf." + p.HostLeg + ".proxy_arp=1"}}); err != nil {
			return err
		}
	}
	if err := rm.ensureForwardChain(ctx, uplink); err != nil {
		return err
	}
	var steps [][]string
	for _, a := range p.HostAddresses {
		if !sameLeg || !slices.Contains(held.hostRoutes, a) {
			steps = append(steps, []string{"ip", "route", "replace", a + "/32", "dev", p.HostLeg})
		}
	}
	for _, a := range p.HostAddresses {
		if !held.proxies[a] {
			steps = append(steps, []string{"ip", "neigh", "replace", "proxy", a, "dev", uplink})
		}
	}
	return rm.runSteps(ctx, steps)
}

// removeRouter removes everything of the router from the node: the host's proxy entries and routes
// first, then the legs, then the namespace with what is left in it.
func (rm *RouterManager) removeRouter(ctx context.Context, ns string, held heldRouter, uplink string) error {
	if !held.exists {
		return nil
	}
	run := func(name string, args ...string) error {
		_, err := rm.runIn(ctx, "", name, args...)
		return err
	}
	for _, a := range held.hostRoutes {
		if held.proxies[a] {
			if err := run("ip", "neigh", "del", "proxy", a, "dev", uplink); err != nil {
				return err
			}
		}
		if err := run("ip", "route", "del", a+"/32", "dev", held.hostLeg); err != nil {
			return err
		}
	}
	for _, dev := range sortedKeys(held.addresses) {
		if outside := hostEnd(dev); outside != "" {
			if err := run("ip", "link", "del", outside); err != nil {
				return err
			}
		}
	}
	return run("ip", "netns", "delete", ns)
}

// forwardChain holds the host's forward rules for the routers' gateway legs: both directions between
// the uplink and every lg- leg, jumped to from the top of FORWARD.
const forwardChain = "ls-l3-fwd"

// ensureForwardChain makes the host forward between the uplink and the routers' gateway legs. It reads
// the filter table and, where it differs, writes the chain, its rules and the jump from FORWARD in one
// restore under the xtables lock, as the port firewall's payload does: two routers converging on a fresh
// node at once cannot both find the chain missing and one fail making it (SPEC-033 T212).
func (rm *RouterManager) ensureForwardChain(ctx context.Context, uplink string) error {
	out, err := rm.runIn(ctx, "", "iptables-save", "-t", "filter")
	if err != nil {
		return fmt.Errorf("read the filter table: %w", err)
	}
	cur := parseSavedTable(string(out))
	rules := []string{
		"-A " + forwardChain + " -i " + uplink + " -o lg+ -m comment --comment lightstack -j ACCEPT",
		"-A " + forwardChain + " -i lg+ -o " + uplink + " -m comment --comment lightstack -j ACCEPT",
	}
	jump := "-A FORWARD -m comment --comment lightstack -j " + forwardChain
	jumped := slices.Contains(cur.rules["FORWARD"], jump)
	if slices.Contains(cur.chains, forwardChain) && slices.Equal(cur.rules[forwardChain], rules) && jumped {
		return nil
	}
	payload := "*filter\n:" + forwardChain + " - [0:0]\n-F " + forwardChain + "\n" + strings.Join(rules, "\n") + "\n"
	if !jumped {
		payload += "-I FORWARD 1 -m comment --comment lightstack -j " + forwardChain + "\n"
	}
	if _, err := rm.runIn(ctx, payload+"COMMIT\n", "iptables-restore", "--noflush", "--wait", "1"); err != nil {
		return fmt.Errorf("make the routers' forward chain: %w", err)
	}
	return nil
}

// runIn runs one command through the runner within ctx's deadline and at most a second.
func (rm *RouterManager) runIn(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	return runCommand(ctx, rm.run, stdin, name, args...)
}

// runCommand runs one command through run within ctx's deadline and at most a second. A failure
// carries the command and its output; an overrun wraps the context's error.
func runCommand(ctx context.Context, run commandRunner, stdin, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	out, err := run(cctx, name, args, stdin)
	if err == nil {
		return out, nil
	}
	if cctx.Err() != nil {
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), cctx.Err())
	}
	return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}

// parseDeviceAddresses reads `ip -j addr show` in a namespace: each Lightstack device's addresses
// with their prefixes. The loopback, other devices and IPv6 link-local addresses, which the kernel adds
// by itself, are left out; an IPv4 link-local address, such as the metadata address, is one Lightstack
// set.
func parseDeviceAddresses(out []byte) (map[string][]string, error) {
	var devices []struct {
		Name  string `json:"ifname"`
		Addrs []struct {
			Local  string `json:"local"`
			Prefix int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &devices); err != nil {
		return nil, err
	}
	held := map[string][]string{}
	for _, d := range devices {
		if hostEnd(d.Name) == "" {
			continue
		}
		held[d.Name] = []string{}
		for _, a := range d.Addrs {
			if ip, err := netip.ParseAddr(a.Local); err == nil && ip.Is6() && ip.IsLinkLocalUnicast() {
				continue
			}
			held[d.Name] = append(held[d.Name], a.Local+"/"+strconv.Itoa(a.Prefix))
		}
	}
	return held, nil
}

// jsonField reads one string field of every object in a JSON array, as `ip -j` prints them.
func jsonField(out []byte, field string) []string {
	var objects []map[string]any
	if json.Unmarshal(out, &objects) != nil {
		return nil
	}
	var values []string
	for _, o := range objects {
		if v, ok := o[field].(string); ok {
			values = append(values, v)
		}
	}
	return values
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
