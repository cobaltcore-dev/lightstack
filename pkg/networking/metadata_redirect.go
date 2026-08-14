package networking

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// MetadataIP is the well-known link-local address cloud-init and the EC2
// metadata datasource query for instance metadata.
const MetadataIP = "169.254.169.254"

// MetadataServicePort is the host port the Lightstack metadata service listens on. The
// guest hits MetadataIP:80; we DNAT that to the host on this port.
const MetadataServicePort = 8775

// MetadataRedirect programs the per-bridge DNAT that makes the link-local
// metadata address reachable from guests on a tenant bridge (SPEC-009 WS-4).
//
// Single-node model: bridges live in the host/default namespace and double as
// the tenant L3 gateway, so the redirect is a default-namespace nat rule and the
// guest reaches it via a DHCP classless-static route (169.254.169.254/32 via the
// gateway, emitted by dnsmasq). The DNAT preserves the source IP so the metadata
// service can identify the instance by its fixed IP. Mirrors RouterManager:
// mode-gated, exec-based, idempotent (check with -C before -A).
type MetadataRedirect struct {
	mode string // "stub", "iptables", or "ebpf"
}

// NewMetadataRedirect creates a metadata-redirect programmer.
func NewMetadataRedirect(mode string) *MetadataRedirect {
	return &MetadataRedirect{mode: mode}
}

// Ensure installs the DNAT rule for one bridge if it is not already present.
// gatewayIP is the bridge's own address (where the host metadata service is
// reachable, since it binds all interfaces). No-op in stub mode and idempotent
// so the reconciler can call it every pass (SPEC-009 WS-4 / T029).
func (mr *MetadataRedirect) Ensure(bridgeName, gatewayIP string) error {
	if mr.mode == "stub" {
		return nil
	}
	if bridgeName == "" || gatewayIP == "" {
		return fmt.Errorf("metadata redirect: bridge name and gateway IP are required")
	}
	dest := fmt.Sprintf("%s:%d", gatewayIP, MetadataServicePort)
	match := []string{
		"-i", bridgeName,
		"-d", MetadataIP,
		"-p", "tcp", "--dport", "80",
		"-j", "DNAT", "--to-destination", dest,
	}
	// Idempotent: -C exits 0 only if the exact rule already exists.
	check := append([]string{"-t", "nat", "-C", "PREROUTING"}, match...)
	if exec.Command("iptables", check...).Run() == nil {
		return nil
	}
	add := append([]string{"-t", "nat", "-A", "PREROUTING"}, match...)
	if output, err := exec.Command("iptables", add...).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add metadata DNAT for %s: %w, output: %s", bridgeName, err, output)
	}
	return nil
}

// RemoveForBridge deletes every metadata DNAT rule for one bridge, whatever it points at
// (best-effort, for network/subnet teardown).
//
// Deliberately keyed on the bridge alone rather than on an exact (bridge, gateway) pair: by the time
// a network is deleted its subnets are already gone, so there is no row left to reconstruct the
// gateway address from, and an exact-pair delete would leave the rule behind. It also reclaims a
// rule whose gateway changed under it.
func (mr *MetadataRedirect) RemoveForBridge(bridgeName string) error {
	if mr.mode == "stub" || bridgeName == "" {
		return nil
	}
	rules, err := mr.list()
	if err != nil {
		return err
	}
	var forBridge []metadataRule
	for _, r := range rules {
		if r.bridge == bridgeName {
			forBridge = append(forBridge, r)
		}
	}
	_, err = deleteMetadataRules(forBridge)
	return err
}

// Prune reconciles the host's metadata DNAT rules against the set that should exist, deleting any
// that should not, and returns how many it reclaimed. desired maps bridge name to the address the
// redirect must point at (the bridge's own address, as Ensure was called with).
//
// iptables is host kernel state that outlives the pod, the release and the database, so every
// delete path that is not the pre-delete purge used to leak a rule — 101 of them accumulated on one
// host in an afternoon, 98 naming bridges that no longer existed (SPEC-024 host pass, finding 1).
//
// It reconciles the whole SET rather than only dropping rules whose interface has disappeared,
// because the one case that is not merely untidy needs the stronger check: a new network whose UUID
// shares its first 8 hex chars with a deleted one reuses the bridge name, and the stale rule — being
// earlier in the ordered chain — wins, sending that network's guests to a gateway that no longer
// exists. Metadata breaks and nothing logs an error. Matching on liveness alone would keep it.
//
// desired is called after the rules are listed, never before. A rule another path adds while the
// caller builds its picture is then either listed — and its network, committed before the rule was
// added, is in a picture read after the listing — or not listed and left alone. A picture built first
// let a subnet created in between lose its fresh redirect.
//
// CALLER CONTRACT: desired must be complete. Pruning against a partially built set deletes live
// rules, so a caller that cannot build it must return an error, which skips the prune. A nil map
// means no rule should exist (teardown).
func (mr *MetadataRedirect) Prune(desired func() (map[string]string, error)) (int, error) {
	if mr.mode == "stub" {
		return 0, nil
	}
	rules, err := mr.list()
	if err != nil {
		return 0, err
	}
	want, err := desired()
	if err != nil {
		return 0, err
	}
	return deleteMetadataRules(staleMetadataRules(rules, want))
}

// metadataRule is one parsed metadata DNAT rule from nat/PREROUTING.
type metadataRule struct {
	bridge string   // e.g. "br-f28a8a32"
	dest   string   // e.g. "10.244.10.1:8775"
	args   []string // the rule's own tokens after "-A PREROUTING", replayed verbatim on delete
}

// list returns the metadata DNAT rules currently in nat/PREROUTING, in chain order.
func (mr *MetadataRedirect) list() ([]metadataRule, error) {
	out, err := exec.Command("iptables", "-t", "nat", "-S", "PREROUTING").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list nat/PREROUTING: %w, output: %s", err, out)
	}
	var rules []metadataRule
	for line := range strings.SplitSeq(string(out), "\n") {
		if r, ok := parseMetadataRule(line); ok {
			rules = append(rules, r)
		}
	}
	return rules, nil
}

// staleMetadataRules returns the rules that do not match desired: an unknown bridge, or a known one
// pointing somewhere it should not. Nil desired means none should exist (teardown).
func staleMetadataRules(rules []metadataRule, desired map[string]string) []metadataRule {
	stale := []metadataRule{}
	for _, r := range rules {
		gatewayIP, wanted := desired[r.bridge]
		if wanted && r.dest == fmt.Sprintf("%s:%d", gatewayIP, MetadataServicePort) {
			continue
		}
		stale = append(stale, r)
	}
	return stale
}

// deleteMetadataRules removes parsed rules, returning how many went away. Best-effort per rule so
// one failure does not strand the rest; the first error is reported.
func deleteMetadataRules(rules []metadataRule) (int, error) {
	var removed int
	var firstErr error
	for _, r := range rules {
		del := append([]string{"-t", "nat", "-D", "PREROUTING"}, r.args...)
		if out, err := exec.Command("iptables", del...).CombinedOutput(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("delete metadata DNAT for %s: %w, output: %s", r.bridge, err, out)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// parseMetadataRule reports whether one line of `iptables -t nat -S PREROUTING` is a metadata
// redirect this service wrote, and parses it if so.
//
// The match is deliberately exact — the same chain carries k3s/kube-proxy, docker and our own
// floating-IP NAT, and a false positive here deletes someone else's rule. Every element Ensure
// writes must be present and identical — the link-local destination, a Lightstack bridge name (the same
// strict "br-" + 8 lowercase hex shape the stale-bridge prune uses), tcp/80, and a DNAT to one address
// on the metadata service port — and nothing else may be (see metadataRuleMatch).
func parseMetadataRule(line string) (metadataRule, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "-A" || fields[1] != "PREROUTING" {
		return metadataRule{}, false
	}
	args := fields[2:]
	match, ok := metadataRuleMatch(args)
	if !ok {
		return metadataRule{}, false
	}
	if dst := match["-d"]; dst != MetadataIP && dst != MetadataIP+"/32" {
		return metadataRule{}, false
	}
	bridge := match["-i"]
	if !isManagedBridge(bridge) {
		return metadataRule{}, false
	}
	if match["-p"] != "tcp" || match["--dport"] != "80" || match["-j"] != "DNAT" {
		return metadataRule{}, false
	}
	if m, present := match["-m"]; present && m != "tcp" {
		return metadataRule{}, false
	}
	// One address and the metadata port, nothing else: iptables also prints address ranges here.
	dest := match["--to-destination"]
	host, port, err := net.SplitHostPort(dest)
	if err != nil || port != strconv.Itoa(MetadataServicePort) {
		return metadataRule{}, false
	}
	if ip := net.ParseIP(host); ip == nil || ip.To4() == nil {
		return metadataRule{}, false
	}
	return metadataRule{bridge: bridge, dest: dest, args: args}, true
}

// metadataRuleFlags are the only flags a metadata redirect carries: the ones Ensure writes, and the
// `-m tcp` iptables adds when it prints `--dport`.
var metadataRuleFlags = map[string]bool{
	"-d": true, "-i": true, "-p": true, "-m": true, "--dport": true, "-j": true, "--to-destination": true,
}

// metadataRuleMatch reads a rule's arguments as flag/value pairs, in any order, refusing any flag a
// metadata redirect does not carry and any flag given twice. An extra match narrows or changes what
// the rule does, so a rule carrying one was not written by Ensure. A negation ("! -d") is refused the
// same way: "!" is not one of the flags.
func metadataRuleMatch(args []string) (map[string]string, bool) {
	if len(args)%2 != 0 {
		return nil, false
	}
	match := make(map[string]string, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		flag := args[i]
		if _, twice := match[flag]; twice || !metadataRuleFlags[flag] {
			return nil, false
		}
		match[flag] = args[i+1]
	}
	return match, true
}
