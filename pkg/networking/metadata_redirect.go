package networking

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// MetadataIP is the well-known link-local address cloud-init and the EC2
// metadata datasource query for instance metadata.
const MetadataIP = "169.254.169.254"

// MetadataServicePort is the host port the O3K metadata service listens on. The
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
// CALLER CONTRACT: desired must be complete. Pruning against a partially built set deletes live
// rules, so a caller whose enumeration failed must not call this at all rather than pass what it got.
func (mr *MetadataRedirect) Prune(desired map[string]string) (int, error) {
	if mr.mode == "stub" {
		return 0, nil
	}
	rules, err := mr.list()
	if err != nil {
		return 0, err
	}
	return deleteMetadataRules(staleMetadataRules(rules, desired))
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
// writes must be present and identical: the link-local destination, an o3k bridge name (the same
// strict "br-" + 8 lowercase hex shape the stale-bridge prune uses), tcp/80, and a DNAT to the
// metadata service port.
func parseMetadataRule(line string) (metadataRule, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "-A" || fields[1] != "PREROUTING" {
		return metadataRule{}, false
	}
	args := fields[2:]
	// A negated match ("! -d") inverts the rule's meaning; the scan below reads only the value after
	// a flag and would not see it. Nothing we write is negated, so refuse the whole line.
	if slices.Contains(args, "!") {
		return metadataRule{}, false
	}
	dst, _ := metadataArgValue(args, "-d")
	if dst != MetadataIP && dst != MetadataIP+"/32" {
		return metadataRule{}, false
	}
	bridge, _ := metadataArgValue(args, "-i")
	if !isManagedBridge(bridge) {
		return metadataRule{}, false
	}
	if proto, _ := metadataArgValue(args, "-p"); proto != "tcp" {
		return metadataRule{}, false
	}
	if dport, _ := metadataArgValue(args, "--dport"); dport != "80" {
		return metadataRule{}, false
	}
	if target, _ := metadataArgValue(args, "-j"); target != "DNAT" {
		return metadataRule{}, false
	}
	dest, ok := metadataArgValue(args, "--to-destination")
	if !ok || !strings.HasSuffix(dest, fmt.Sprintf(":%d", MetadataServicePort)) {
		return metadataRule{}, false
	}
	return metadataRule{bridge: bridge, dest: dest, args: args}, true
}

// metadataArgValue returns the value following the first occurrence of flag.
func metadataArgValue(args []string, flag string) (string, bool) {
	for i, f := range args {
		if f == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
