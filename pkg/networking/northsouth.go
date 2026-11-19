package networking

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// DefaultUplinkInterface returns the host's primary uplink: the interface the
// kernel would use to reach the public internet. Single-node WS-3 masquerades
// tenant egress (and hosts floating IPs) out this interface, auto-mapping the
// external network to the node uplink — no operator-created br-ex/NIC (SPEC-009
// research R10).
func DefaultUplinkInterface() (string, error) {
	// Ask the kernel directly which link reaches a public address — robust
	// across default-route representations (Dst nil vs 0.0.0.0/0, multiple
	// default routes, metrics).
	if routes, err := netlink.RouteGet(net.IPv4(1, 1, 1, 1)); err == nil {
		for _, r := range routes {
			if r.LinkIndex > 0 {
				if link, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
					return link.Attrs().Name, nil
				}
			}
		}
	}
	// Fallback: scan the route table for the default route (Dst nil or 0.0.0.0/0).
	routes, err := netlink.RouteList(nil, unix.AF_INET)
	if err != nil {
		return "", fmt.Errorf("list routes: %w", err)
	}
	for _, r := range routes {
		if r.Dst == nil || r.Dst.IP.IsUnspecified() {
			if link, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
				return link.Attrs().Name, nil
			}
		}
	}
	return "", fmt.Errorf("no IPv4 default route / uplink found")
}

// EnsureEgressSNAT masquerades a tenant subnet's outbound traffic out the uplink,
// in the default namespace — single-node: the VMs and their gateway bridge live
// there, so SNAT must too (a per-router netns rule would never see their
// traffic). Also enables IPv4 forwarding and permits the forward path both ways,
// since the host FORWARD policy is often DROP (e.g. whenever Docker is
// installed). Idempotent (SPEC-009 WS-3 / T032).
func (rm *RouterManager) EnsureEgressSNAT(tenantCIDR, uplink string) error {
	if rm.mode == "stub" {
		return nil
	}
	if tenantCIDR == "" || uplink == "" {
		return fmt.Errorf("egress SNAT: tenant CIDR and uplink are required")
	}
	if err := enableIPv4Forwarding(); err != nil {
		return err
	}
	if err := ensureRule("nat", "POSTROUTING", "-s", tenantCIDR, "-o", uplink, "-j", "MASQUERADE"); err != nil {
		return err
	}
	if err := ensureRule("filter", "FORWARD", "-s", tenantCIDR, "-o", uplink, "-j", "ACCEPT"); err != nil {
		return err
	}
	return ensureRule("filter", "FORWARD", "-d", tenantCIDR, "-i", uplink,
		"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
}

// RemoveEgressSNAT deletes the egress rules for a tenant subnet (best-effort, for
// gateway-clear / interface-detach).
func (rm *RouterManager) RemoveEgressSNAT(tenantCIDR, uplink string) error {
	if rm.mode == "stub" || tenantCIDR == "" || uplink == "" {
		return nil
	}
	deleteRule("nat", "POSTROUTING", "-s", tenantCIDR, "-o", uplink, "-j", "MASQUERADE")
	deleteRule("filter", "FORWARD", "-s", tenantCIDR, "-o", uplink, "-j", "ACCEPT")
	deleteRule("filter", "FORWARD", "-d", tenantCIDR, "-i", uplink,
		"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
	return nil
}

// fipMark marks every NAT rule Lightstack writes for a floating IP (SPEC-033 research R23). It is
// what lets reconciliation remove a rule that has no record without touching an operator's.
const fipMark = "lightstack-fip"

// natHooks are the nat chains a floating IP is translated in: PREROUTING for forwarded traffic,
// OUTPUT for the node's own.
var natHooks = []string{"PREROUTING", "OUTPUT"}

// FloatingNAT is one rule of the shape: `-d <F>/32 [-m comment --comment lightstack-fip] -j DNAT
// --to-destination <X>` in PREROUTING or OUTPUT, with nothing else in it. A port forwarding's rule,
// with -p and --dport, is not of the shape (research R23).
type FloatingNAT struct {
	Chain      string
	FloatingIP string
	FixedIP    string
	Marked     bool
}

// NorthSouth is what a node holds for floating IPs: its NAT rules of the shape, marked or not; the
// fixed IPs that have an ingress filter; and the uplink's addresses (research R24).
type NorthSouth struct {
	NAT       []FloatingNAT
	Filters   []string
	Addresses []string
}

// EnsureFloatingIP makes floating IP F reach fixed IP X on this node (SPEC-009 WS-3 / T033, SPEC-033
// research R23). In one nat-table commit it deletes every rule of F's that points elsewhere or is
// unmarked, and appends the marked rule for X where it is missing, in both hooks; then it adds F to
// the uplink if it is missing, so traffic for F reaches the node only once the NAT is in place. The
// caller ensures X's filter first. It reports the fixed IPs the deleted rules pointed at, other than
// X. Every command runs under a one-second deadline.
//
// v1 scope: FIP delivers *inbound* reachability; the VM's egress keeps using the node's shared
// egress SNAT (EnsureEgressSNAT), not a 1:1 egress-as-FIP SNAT.
func (rm *RouterManager) EnsureFloatingIP(floatingIP, fixedIP, uplink string) ([]string, error) {
	if rm.mode == "stub" {
		return nil, nil
	}
	if floatingIP == "" || fixedIP == "" || uplink == "" {
		return nil, fmt.Errorf("floating IP: floating IP, fixed IP, and uplink are required")
	}
	if err := rm.enableForwarding(); err != nil {
		return nil, err
	}
	all, err := rm.readFloatingNAT()
	if err != nil {
		return nil, err
	}
	payload, replaced := natAssociatePayload(natRulesOf(all, floatingIP), floatingIP, fixedIP)
	if payload != "" {
		if out, err := rm.runBounded(payload, "iptables-restore", "--noflush", "--wait", "1"); err != nil {
			return nil, fmt.Errorf("point floating IP %s at %s: %w, output: %s", floatingIP, fixedIP, err, strings.TrimSpace(string(out)))
		}
	}
	cidrs, err := rm.uplinkAddresses(uplink)
	if err != nil {
		return nil, err
	}
	if _, ok := addressCIDR(cidrs, floatingIP); !ok {
		if out, err := rm.runBounded("", "ip", "addr", "add", floatingIP+"/32", "dev", uplink); err != nil {
			return nil, fmt.Errorf("assign %s/32 to %s: %w, output: %s", floatingIP, uplink, err, strings.TrimSpace(string(out)))
		}
	}
	return replaced, nil
}

// WithdrawFloatingIP stops this node forwarding floating IP F: it removes F from the uplink first, so
// no more traffic for F arrives, then deletes every rule of F's, marked or not, in one nat-table
// commit. It reports the fixed IPs those rules pointed at. Nothing there is not an error. Every
// command runs under a one-second deadline.
func (rm *RouterManager) WithdrawFloatingIP(floatingIP, uplink string) ([]string, error) {
	if rm.mode == "stub" {
		return nil, nil
	}
	if floatingIP == "" || uplink == "" {
		return nil, fmt.Errorf("floating IP: floating IP and uplink are required")
	}
	cidrs, err := rm.uplinkAddresses(uplink)
	if err != nil {
		return nil, err
	}
	if cidr, ok := addressCIDR(cidrs, floatingIP); ok {
		if out, err := rm.runBounded("", "ip", "addr", "del", cidr, "dev", uplink); err != nil {
			return nil, fmt.Errorf("remove %s from %s: %w, output: %s", cidr, uplink, err, strings.TrimSpace(string(out)))
		}
	}
	all, err := rm.readFloatingNAT()
	if err != nil {
		return nil, err
	}
	payload, replaced := natWithdrawPayload(natRulesOf(all, floatingIP))
	if payload != "" {
		if out, err := rm.runBounded(payload, "iptables-restore", "--noflush", "--wait", "1"); err != nil {
			return nil, fmt.Errorf("withdraw floating IP %s: %w, output: %s", floatingIP, err, strings.TrimSpace(string(out)))
		}
	}
	return replaced, nil
}

// NorthSouthState lists this node's floating-IP state for reconciliation (research R24): every NAT
// rule of the shape, marked or not; the fixed IP of every LS-NSI chain; and the uplink's addresses.
// Stub mode holds nothing.
func (rm *RouterManager) NorthSouthState(uplink string) (NorthSouth, error) {
	if rm.mode == "stub" {
		return NorthSouth{}, nil
	}
	if uplink == "" {
		return NorthSouth{}, fmt.Errorf("north-south state: uplink is required")
	}
	nat, err := rm.readFloatingNAT()
	if err != nil {
		return NorthSouth{}, err
	}
	out, err := rm.runBounded("", "iptables", "-t", "filter", "-S")
	if err != nil {
		return NorthSouth{}, fmt.Errorf("list filter chains: %w, output: %s", err, strings.TrimSpace(string(out)))
	}
	var filters []string
	for _, l := range strings.Split(string(out), "\n") {
		chain, ok := strings.CutPrefix(strings.TrimSpace(l), "-N ")
		if !ok {
			continue
		}
		if ip, ok := fixedIPOfChain(chain); ok {
			filters = append(filters, ip)
		}
	}
	slices.Sort(filters)
	cidrs, err := rm.uplinkAddresses(uplink)
	if err != nil {
		return NorthSouth{}, err
	}
	addrs := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		addrs = append(addrs, strings.SplitN(c, "/", 2)[0])
	}
	return NorthSouth{NAT: nat, Filters: filters, Addresses: addrs}, nil
}

// readFloatingNAT reads both nat hooks once and returns their rules of the shape.
func (rm *RouterManager) readFloatingNAT() ([]FloatingNAT, error) {
	var all []FloatingNAT
	for _, hook := range natHooks {
		out, err := rm.runBounded("", "iptables", "-t", "nat", "-S", hook)
		if err != nil {
			return nil, fmt.Errorf("read nat %s: %w, output: %s", hook, err, strings.TrimSpace(string(out)))
		}
		all = append(all, parseFloatingNAT(hook, strings.Split(string(out), "\n"))...)
	}
	return all, nil
}

// uplinkAddresses lists the uplink's IPv4 addresses, each with its prefix length, as `ip -o addr`
// prints them.
func (rm *RouterManager) uplinkAddresses(uplink string) ([]string, error) {
	out, err := rm.runBounded("", "ip", "-o", "addr", "show", "dev", uplink)
	if err != nil {
		return nil, fmt.Errorf("list the addresses of %s: %w, output: %s", uplink, err, strings.TrimSpace(string(out)))
	}
	var cidrs []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "inet" {
				cidrs = append(cidrs, f[i+1])
				break
			}
		}
	}
	return cidrs, nil
}

// addressCIDR finds ip among cidrs and returns it with its prefix length.
func addressCIDR(cidrs []string, ip string) (string, bool) {
	for _, c := range cidrs {
		if strings.SplitN(c, "/", 2)[0] == ip {
			return c, true
		}
	}
	return "", false
}

// parseFloatingNAT returns the rules of the shape among a nat chain's lines, as `iptables -t nat -S
// <chain>` prints them, marked and unmarked alike. Anything else, a port forwarding included, is not
// returned.
func parseFloatingNAT(chain string, lines []string) []FloatingNAT {
	var out []FloatingNAT
	for _, l := range lines {
		f := strings.Fields(l)
		var r FloatingNAT
		switch {
		case len(f) == 8 && f[0] == "-A" && f[1] == chain && f[2] == "-d" &&
			f[4] == "-j" && f[5] == "DNAT" && f[6] == "--to-destination":
			r = FloatingNAT{Chain: chain, FloatingIP: f[3], FixedIP: f[7]}
		case len(f) == 12 && f[0] == "-A" && f[1] == chain && f[2] == "-d" &&
			f[4] == "-m" && f[5] == "comment" && f[6] == "--comment" && f[7] == fipMark &&
			f[8] == "-j" && f[9] == "DNAT" && f[10] == "--to-destination":
			r = FloatingNAT{Chain: chain, FloatingIP: f[3], FixedIP: f[11], Marked: true}
		default:
			continue
		}
		ip, ok := strings.CutSuffix(r.FloatingIP, "/32")
		if !ok || net.ParseIP(ip) == nil || net.ParseIP(r.FixedIP) == nil {
			continue
		}
		r.FloatingIP = ip
		out = append(out, r)
	}
	return out
}

// natRulesOf returns the rules for one floating IP.
func natRulesOf(all []FloatingNAT, floatingIP string) []FloatingNAT {
	var out []FloatingNAT
	for _, r := range all {
		if r.FloatingIP == floatingIP {
			out = append(out, r)
		}
	}
	return out
}

// natRuleSpec renders a rule as iptables prints it, without its -A or -D.
func natRuleSpec(r FloatingNAT) string {
	s := r.Chain + " -d " + r.FloatingIP + "/32"
	if r.Marked {
		s += " -m comment --comment " + fipMark
	}
	return s + " -j DNAT --to-destination " + r.FixedIP
}

// natAssociatePayload returns the nat-table restore input that leaves F with exactly the marked rule
// to X in each hook, or "" when that already holds. current are F's rules. replaced are the fixed
// IPs, other than X, that the deleted rules pointed at.
func natAssociatePayload(current []FloatingNAT, floatingIP, fixedIP string) (string, []string) {
	var lines, replaced []string
	for _, hook := range natHooks {
		kept := false
		for _, r := range current {
			if r.Chain != hook || r.FloatingIP != floatingIP {
				continue
			}
			if r.Marked && r.FixedIP == fixedIP && !kept {
				kept = true
				continue
			}
			lines = append(lines, "-D "+natRuleSpec(r))
			if r.FixedIP != fixedIP && !slices.Contains(replaced, r.FixedIP) {
				replaced = append(replaced, r.FixedIP)
			}
		}
		if !kept {
			lines = append(lines, "-A "+natRuleSpec(FloatingNAT{Chain: hook, FloatingIP: floatingIP, FixedIP: fixedIP, Marked: true}))
		}
	}
	if len(lines) == 0 {
		return "", replaced
	}
	return "*nat\n" + strings.Join(lines, "\n") + "\nCOMMIT\n", replaced
}

// natWithdrawPayload returns the nat-table restore input that deletes every given rule, or "" when
// there is none, and the fixed IPs they pointed at.
func natWithdrawPayload(current []FloatingNAT) (string, []string) {
	if len(current) == 0 {
		return "", nil
	}
	var lines, replaced []string
	for _, r := range current {
		lines = append(lines, "-D "+natRuleSpec(r))
		if !slices.Contains(replaced, r.FixedIP) {
			replaced = append(replaced, r.FixedIP)
		}
	}
	return "*nat\n" + strings.Join(lines, "\n") + "\nCOMMIT\n", replaced
}

// filterRemovalPayload returns the filter-table restore input that removes a chain in one commit:
// every FORWARD jump to it, as `iptables -S FORWARD` prints them, then a flush, then the chain.
func filterRemovalPayload(chain string, jumps []string) string {
	var b strings.Builder
	b.WriteString("*filter\n")
	for _, j := range jumps {
		b.WriteString("-D " + strings.TrimPrefix(strings.TrimSpace(j), "-A ") + "\n")
	}
	b.WriteString("-F " + chain + "\n-X " + chain + "\nCOMMIT\n")
	return b.String()
}

// fixedIPOfChain reads the fixed IP back out of an LS-NSI chain's name.
func fixedIPOfChain(chain string) (string, bool) {
	rest, ok := strings.CutPrefix(chain, "LS-NSI-")
	if !ok {
		return "", false
	}
	ip := strings.ReplaceAll(rest, "-", ".")
	return ip, net.ParseIP(ip) != nil
}

// IngressRule is a security-group ingress rule reduced to what the north-south
// filter needs (SPEC-009 WS-3 / T034).
type IngressRule struct {
	Protocol string // "tcp", "udp", "icmp", or "" (all protocols)
	PortMin  int
	PortMax  int
	Remote   string // source CIDR, or "" (any source)
}

// nsIngressChain is the per-fixed-IP filter chain name (e.g. LS-NSI-10-0-0-162).
// The prefix is the short form deliberately: the full product name would put a
// three-digit-octet address at 30 chars, over iptables' 28-char limit, and this
// namer has no truncation guard. Worst case here is "LS-NSI-" + 15 = 22.
func nsIngressChain(fixedIP string) string {
	return "LS-NSI-" + strings.ReplaceAll(fixedIP, ".", "-")
}

// EnsureIngressFilter makes a VM's fixed IP default-deny for inbound traffic from the uplink,
// allowing replies and the port's security-group ingress rules (SPEC-009 WS-3 / T034). The filter is
// the chain LS-NSI-<fixed IP>, reached by a FORWARD jump for traffic from the uplink to the fixed IP.
//
// It reads the chain first and leaves it alone when it already holds the rendering (research R7).
// Otherwise it replaces the whole chain in one iptables-restore commit, so the chain holds either
// the complete old rules or the complete new ones, each ending in DROP, and a failed commit applies
// nothing (R6, FR-007a). The jump is ensured last. Every command runs under a one-second deadline;
// an overrun is a failure, and the old filter stays whole (FR-007b).
func (rm *RouterManager) EnsureIngressFilter(fixedIP, uplink string, rules []IngressRule) error {
	if rm.mode == "stub" {
		return nil
	}
	if fixedIP == "" || uplink == "" {
		return fmt.Errorf("ingress filter: fixed IP and uplink are required")
	}
	chain := nsIngressChain(fixedIP)
	desired := RenderIngressFilter(chain, rules)
	current, err := rm.runBounded("", "iptables", "-t", "filter", "-S", chain)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("read ingress filter %s: %w", chain, err)
	}
	// Any other failure of the read is a chain that does not exist yet: it reads as empty.
	if err != nil || !ChainMatches(strings.Split(string(current), "\n"), desired) {
		out, err := rm.runBounded(RestorePayload(chain, desired), "iptables-restore", "--noflush", "--wait", "1")
		if err != nil {
			return fmt.Errorf("replace ingress filter %s: %w, output: %s", chain, err, strings.TrimSpace(string(out)))
		}
	}
	return rm.ensureRuleBounded("filter", "FORWARD", "-i", uplink, "-d", fixedIP, "-j", chain)
}

// RenderIngressFilter returns the -A lines of a floating IP's ingress filter in iptables' canonical
// form, the form `iptables -S` prints back: replies first, one ACCEPT per allowance, DROP last. The
// allowances are sorted and a duplicate is kept once, so the rendering does not depend on the order
// the rules were read in, and an unchanged filter compares equal (research R7). All of them accept,
// so their order does not change what the filter lets through.
func RenderIngressFilter(chain string, rules []IngressRule) []string {
	allow := make([]string, 0, len(rules))
	for _, r := range rules {
		allow = append(allow, "-A "+chain+" "+ingressMatch(r)+"-j ACCEPT")
	}
	slices.Sort(allow)
	allow = slices.Compact(allow)
	lines := make([]string, 0, len(allow)+2)
	lines = append(lines, "-A "+chain+" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT")
	lines = append(lines, allow...)
	return append(lines, "-A "+chain+" -j DROP")
}

// RestorePayload is the iptables-restore input that replaces a chain with lines in one commit: the
// declaration creates the chain or keeps it, and the flush empties it within the same commit.
func RestorePayload(chain string, lines []string) string {
	var b strings.Builder
	b.WriteString("*filter\n:" + chain + " - [0:0]\n-F " + chain + "\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// ChainMatches reports whether a chain, as `iptables -S <chain>` printed it, holds exactly the
// desired lines in order. The chain's -N line is not a rule and is ignored. Empty output, a chain
// that does not exist, never matches.
func ChainMatches(current, desired []string) bool {
	rules := make([]string, 0, len(current))
	for _, l := range current {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "-N ") {
			continue
		}
		rules = append(rules, l)
	}
	return len(rules) > 0 && slices.Equal(rules, desired)
}

// ingressMatch renders a rule's match in canonical form, followed by a space, or "" for a rule that
// matches everything: the source first, then the protocol and its port or ICMP type.
func ingressMatch(r IngressRule) string {
	var b strings.Builder
	if src := canonicalSource(r.Remote); src != "" {
		b.WriteString("-s " + src + " ")
	}
	proto := canonicalProtocol(r.Protocol)
	if proto != "" {
		b.WriteString("-p " + proto + " ")
	}
	switch {
	case (proto == "tcp" || proto == "udp") && r.PortMin > 0:
		b.WriteString("-m " + proto + " --dport " + portSpec(r.PortMin, r.PortMax) + " ")
	case proto == "icmp" && r.PortMin > 0:
		b.WriteString("-m icmp --icmp-type " + strconv.Itoa(r.PortMin) + " ")
	}
	return b.String()
}

// canonicalSource renders a remote prefix as iptables prints it: a network with its prefix length, a
// host as /32, and nothing for any source. A prefix that does not parse is passed on unchanged;
// iptables-restore then rejects the commit and the old filter stays.
func canonicalSource(remote string) string {
	if remote == "" {
		return ""
	}
	if ip := net.ParseIP(remote); ip != nil {
		if ip.To4() != nil {
			remote += "/32"
		} else {
			remote += "/128"
		}
	}
	_, n, err := net.ParseCIDR(remote)
	if err != nil {
		return remote
	}
	if ones, _ := n.Mask.Size(); ones == 0 {
		return ""
	}
	return n.String()
}

// canonicalProtocol renders a rule's protocol as iptables prints it: in lower case, and by name for
// the protocols the filter matches ports or types of, which Neutron also accepts by number.
func canonicalProtocol(p string) string {
	switch p = strings.ToLower(p); p {
	case "1":
		return "icmp"
	case "6":
		return "tcp"
	case "17":
		return "udp"
	}
	return p
}

func portSpec(lo, hi int) string {
	if hi == 0 || hi == lo {
		return strconv.Itoa(lo)
	}
	return strconv.Itoa(lo) + ":" + strconv.Itoa(hi)
}

// RemoveIngressFilter removes a fixed IP's filter in one commit: every FORWARD jump to it, a flush,
// then the chain (research R23). A chain that is already gone counts as removed. Every command runs
// under a one-second deadline, and every error is returned.
func (rm *RouterManager) RemoveIngressFilter(fixedIP string) error {
	if rm.mode == "stub" {
		return nil
	}
	if fixedIP == "" {
		return fmt.Errorf("ingress filter: fixed IP is required")
	}
	chain := nsIngressChain(fixedIP)
	out, err := rm.runBounded("", "iptables", "-t", "filter", "-S", chain)
	if err != nil {
		if missingChain(out) {
			return nil
		}
		return fmt.Errorf("read ingress filter %s: %w, output: %s", chain, err, strings.TrimSpace(string(out)))
	}
	out, err = rm.runBounded("", "iptables", "-t", "filter", "-S", "FORWARD")
	if err != nil {
		return fmt.Errorf("read FORWARD: %w, output: %s", err, strings.TrimSpace(string(out)))
	}
	var jumps []string
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) >= 4 && f[0] == "-A" && f[len(f)-2] == "-j" && f[len(f)-1] == chain {
			jumps = append(jumps, strings.TrimSpace(l))
		}
	}
	if out, err := rm.runBounded(filterRemovalPayload(chain, jumps), "iptables-restore", "--noflush", "--wait", "1"); err != nil {
		return fmt.Errorf("remove ingress filter %s: %w, output: %s", chain, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// missingChain reports whether a failed `iptables -S <chain>` failed because the chain does not exist.
// Both backends print libxtables' ENOENT text for it; any other failure is an error.
func missingChain(out []byte) bool {
	return strings.Contains(string(out), "No chain/target/match by that name")
}

// ensureRule appends an iptables rule if it is not already present (idempotent).
func ensureRule(table, chain string, rule ...string) error {
	check := append([]string{"-t", table, "-C", chain}, rule...)
	if exec.Command("iptables", check...).Run() == nil {
		return nil
	}
	add := append([]string{"-t", table, "-A", chain}, rule...)
	if out, err := exec.Command("iptables", add...).CombinedOutput(); err != nil {
		return fmt.Errorf("iptables -t %s -A %s %v: %w, output: %s", table, chain, rule, err, out)
	}
	return nil
}

// commandTimeout bounds each command of the floating-IP dataplane (Article III, FR-007b). A command
// that overruns is killed and counts as failed.
const commandTimeout = time.Second

// commandRunner runs one command with stdin under ctx and returns its combined output.
type commandRunner func(ctx context.Context, name string, args []string, stdin string) ([]byte, error)

// execRunner is the real commandRunner. The context kills the command when it overruns, and
// WaitDelay bounds the wait for its output after that.
func execRunner(ctx context.Context, name string, args []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.CombinedOutput()
}

// runBounded runs one command through the runner under a fresh one-second deadline. An overrun
// fails with an error wrapping context.DeadlineExceeded.
func (rm *RouterManager) runBounded(stdin, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := rm.run(ctx, name, args, stdin)
	if err != nil && ctx.Err() != nil {
		return out, fmt.Errorf("%s %s: %w after %s", name, strings.Join(args, " "), ctx.Err(), commandTimeout)
	}
	return out, err
}

// ensureRuleBounded is ensureRule through the runner, each command under its own deadline: check the
// rule, and append it when the check fails. A check that overruns is an error, not a reason to append
// a second copy.
func (rm *RouterManager) ensureRuleBounded(table, chain string, rule ...string) error {
	check := append([]string{"-t", table, "-C", chain}, rule...)
	_, err := rm.runBounded("", "iptables", check...)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("check a rule in %s: %w", chain, err)
	}
	add := append([]string{"-t", table, "-A", chain}, rule...)
	if out, err := rm.runBounded("", "iptables", add...); err != nil {
		return fmt.Errorf("iptables -t %s -A %s %v: %w, output: %s", table, chain, rule, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// deleteRule removes an iptables rule, ignoring "not present" (best-effort).
func deleteRule(table, chain string, rule ...string) {
	del := append([]string{"-t", table, "-D", chain}, rule...)
	_, _ = exec.Command("iptables", del...).CombinedOutput()
}

// enableIPv4Forwarding turns on net.ipv4.ip_forward so the host routes tenant
// traffic to the uplink. Idempotent.
func enableIPv4Forwarding() error {
	const p = "/proc/sys/net/ipv4/ip_forward"
	if cur, err := os.ReadFile(p); err == nil && len(cur) > 0 && cur[0] == '1' {
		return nil
	}
	if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	return nil
}
