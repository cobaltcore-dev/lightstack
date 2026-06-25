package networking

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/vishvananda/netlink"
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
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
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

// EnsureFloatingIP makes an external floating IP reach a VM's fixed IP on a
// single node (SPEC-009 WS-3 / T033): it hosts the FIP as a /32 on the uplink so
// the node receives traffic for it (the operator routes / L2-advertises the pool
// to the node) and DNATs it to the fixed IP (inbound + host-local). Idempotent.
//
// v1 scope: FIP delivers *inbound* reachability; the VM's egress keeps using the
// node's shared egress SNAT (EnsureEgressSNAT), not a 1:1 egress-as-FIP SNAT —
// that would rewrite the source to the FIP and break egress whenever the FIP
// pool is not actually routable (the common single-node/dev case). Inbound is
// permitted here; security-group gating is layered on by T034.
func (rm *RouterManager) EnsureFloatingIP(floatingIP, fixedIP, uplink string) error {
	if rm.mode == "stub" {
		return nil
	}
	if floatingIP == "" || fixedIP == "" || uplink == "" {
		return fmt.Errorf("floating IP: floating IP, fixed IP, and uplink are required")
	}
	if err := enableIPv4Forwarding(); err != nil {
		return err
	}
	// Host the FIP on the uplink so traffic destined to it arrives locally.
	if err := ensureAddr(floatingIP+"/32", uplink); err != nil {
		return err
	}
	// Inbound DNAT: floating IP -> fixed IP (PREROUTING for forwarded traffic,
	// OUTPUT for host-local traffic so the node itself can reach the FIP too).
	if err := ensureRule("nat", "PREROUTING", "-d", floatingIP, "-j", "DNAT", "--to-destination", fixedIP); err != nil {
		return err
	}
	if err := ensureRule("nat", "OUTPUT", "-d", floatingIP, "-j", "DNAT", "--to-destination", fixedIP); err != nil {
		return err
	}
	// Inbound forwarding to the fixed IP is gated by the per-VM ingress filter
	// (EnsureIngressFilter / T034), not blanket-opened here.
	return nil
}

// WithdrawFloatingIP removes a floating IP's NAT rules and its address from the
// uplink (best-effort, for disassociate / release).
func (rm *RouterManager) WithdrawFloatingIP(floatingIP, fixedIP, uplink string) error {
	if rm.mode == "stub" || floatingIP == "" || fixedIP == "" || uplink == "" {
		return nil
	}
	deleteRule("nat", "PREROUTING", "-d", floatingIP, "-j", "DNAT", "--to-destination", fixedIP)
	deleteRule("nat", "OUTPUT", "-d", floatingIP, "-j", "DNAT", "--to-destination", fixedIP)
	_, _ = exec.Command("ip", "addr", "del", floatingIP+"/32", "dev", uplink).CombinedOutput()
	return nil
}

// IngressRule is a security-group ingress rule reduced to what the north-south
// filter needs (SPEC-009 WS-3 / T034).
type IngressRule struct {
	Protocol string // "tcp", "udp", "icmp", or "" (all protocols)
	PortMin  int
	PortMax  int
	Remote   string // source CIDR, or "" (any source)
}

// nsIngressChain is the per-fixed-IP filter chain name (e.g. O3K-NSI-10-0-0-162),
// well under iptables' 28-char limit.
func nsIngressChain(fixedIP string) string {
	return "O3K-NSI-" + strings.ReplaceAll(fixedIP, ".", "-")
}

// EnsureIngressFilter programs default-deny inbound to a VM's fixed IP, allowing
// only its security-group ingress rules (SPEC-009 WS-3 / T034). It (re)builds a
// per-fixed-IP filter chain — established/related, then one ACCEPT per ingress
// rule, then DROP — and jumps inbound FORWARD traffic (from the uplink, to the
// fixed IP) to it. Rebuilt from scratch each call so adding/removing an SG rule
// takes effect immediately. Idempotent.
func (rm *RouterManager) EnsureIngressFilter(fixedIP, uplink string, rules []IngressRule) error {
	if rm.mode == "stub" {
		return nil
	}
	if fixedIP == "" || uplink == "" {
		return fmt.Errorf("ingress filter: fixed IP and uplink are required")
	}
	chain := nsIngressChain(fixedIP)
	// Create the chain (ignore "already exists"), then flush + rebuild it.
	if out, err := exec.Command("iptables", "-t", "filter", "-N", chain).CombinedOutput(); err != nil &&
		!strings.Contains(string(out), "already exists") {
		return fmt.Errorf("create chain %s: %w, output: %s", chain, err, out)
	}
	if out, err := exec.Command("iptables", "-t", "filter", "-F", chain).CombinedOutput(); err != nil {
		return fmt.Errorf("flush chain %s: %w, output: %s", chain, err, out)
	}
	// Return traffic for the VM's own connections.
	if _, err := exec.Command("iptables", "-t", "filter", "-A", chain,
		"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT").CombinedOutput(); err != nil {
		return fmt.Errorf("ingress filter conntrack rule: %w", err)
	}
	// One ACCEPT per allowed ingress rule.
	for _, r := range rules {
		args := append([]string{"-t", "filter", "-A", chain}, ingressRuleSpec(r)...)
		args = append(args, "-j", "ACCEPT")
		if out, err := exec.Command("iptables", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("ingress filter rule %v: %w, output: %s", r, err, out)
		}
	}
	// Default-deny.
	if _, err := exec.Command("iptables", "-t", "filter", "-A", chain, "-j", "DROP").CombinedOutput(); err != nil {
		return fmt.Errorf("ingress filter default DROP: %w", err)
	}
	// Send inbound traffic for this fixed IP through the chain (idempotent).
	return ensureRule("filter", "FORWARD", "-i", uplink, "-d", fixedIP, "-j", chain)
}

// RemoveIngressFilter removes the per-fixed-IP ingress filter (best-effort, for
// disassociate / release).
func (rm *RouterManager) RemoveIngressFilter(fixedIP, uplink string) error {
	if rm.mode == "stub" || fixedIP == "" {
		return nil
	}
	chain := nsIngressChain(fixedIP)
	if uplink != "" {
		deleteRule("filter", "FORWARD", "-i", uplink, "-d", fixedIP, "-j", chain)
	}
	_, _ = exec.Command("iptables", "-t", "filter", "-F", chain).CombinedOutput()
	_, _ = exec.Command("iptables", "-t", "filter", "-X", chain).CombinedOutput()
	return nil
}

// ingressRuleSpec renders the match part of an ingress rule (no target).
func ingressRuleSpec(r IngressRule) []string {
	var s []string
	if r.Protocol != "" {
		s = append(s, "-p", r.Protocol)
	}
	if (r.Protocol == "tcp" || r.Protocol == "udp") && r.PortMin > 0 {
		if r.PortMax == 0 || r.PortMin == r.PortMax {
			s = append(s, "--dport", fmt.Sprintf("%d", r.PortMin))
		} else {
			s = append(s, "--dport", fmt.Sprintf("%d:%d", r.PortMin, r.PortMax))
		}
	}
	if r.Protocol == "icmp" && r.PortMin > 0 {
		s = append(s, "--icmp-type", fmt.Sprintf("%d", r.PortMin))
	}
	if r.Remote != "" && r.Remote != "0.0.0.0/0" {
		s = append(s, "-s", r.Remote)
	}
	return s
}

// ensureAddr assigns an address to a link if it is not already present. After a
// failed add it confirms by querying the device rather than string-matching the
// error, since iproute2 phrases "already there" differently across versions
// ("File exists" vs "Error: ipv4: Address already assigned").
func ensureAddr(cidr, dev string) error {
	out, err := exec.Command("ip", "addr", "add", cidr, "dev", dev).CombinedOutput()
	if err == nil {
		return nil
	}
	ip := strings.SplitN(cidr, "/", 2)[0]
	if check, _ := exec.Command("ip", "-o", "addr", "show", "dev", dev).CombinedOutput(); strings.Contains(string(check), ip+"/") {
		return nil // already assigned — idempotent success
	}
	return fmt.Errorf("assign %s to %s: %w, output: %s", cidr, dev, err, out)
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
