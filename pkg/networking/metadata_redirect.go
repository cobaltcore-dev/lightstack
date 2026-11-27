package networking

import (
	"net"
	"strconv"
	"strings"
)

// metadataServicePort is the host port the Lightstack metadata service listens on, where an earlier
// layout's per-bridge DNAT sent the guests' MetadataIP:80.
const metadataServicePort = 8775

// The per-bridge metadata DNAT an earlier layout wrote to reach the metadata service from a tenant
// bridge (SPEC-009 WS-4): `-i br-… -d 169.254.169.254 -p tcp --dport 80 -j DNAT --to-destination
// <bridge address>:8775` in nat/PREROUTING. Guests reach metadata through their network's DHCP
// namespace now (SPEC-033 R29), and nothing writes the rule; a node that ran the earlier layout still
// has it, and RetiredState removes it (FR-044).

// isMetadataRule reports whether one line of the nat table, as iptables-save or `iptables -S` prints it,
// is a metadata redirect an earlier layout wrote.
//
// The match is deliberately exact — the same chain carries k3s/kube-proxy, docker and other NAT, and a
// false positive here deletes someone else's rule. Every element the earlier layout wrote must be
// present and identical — the link-local destination, a Lightstack bridge name (the strict "br-" + 8
// lowercase hex shape the stale-bridge prune uses), tcp/80, and a DNAT to one address on the metadata
// service port — and nothing else may be (see metadataRuleMatch).
func isMetadataRule(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "-A" || fields[1] != "PREROUTING" {
		return false
	}
	match, ok := metadataRuleMatch(fields[2:])
	if !ok {
		return false
	}
	if dst := match["-d"]; dst != MetadataIP && dst != MetadataIP+"/32" {
		return false
	}
	if !isManagedBridge(match["-i"]) {
		return false
	}
	if match["-p"] != "tcp" || match["--dport"] != "80" || match["-j"] != "DNAT" {
		return false
	}
	if m, present := match["-m"]; present && m != "tcp" {
		return false
	}
	// One address and the metadata port, nothing else: iptables also prints address ranges here.
	host, port, err := net.SplitHostPort(match["--to-destination"])
	if err != nil || port != strconv.Itoa(metadataServicePort) {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() != nil
}

// metadataRuleFlags are the only flags a metadata redirect carries: the ones the earlier layout wrote,
// and the `-m tcp` iptables adds when it prints `--dport`.
var metadataRuleFlags = map[string]bool{
	"-d": true, "-i": true, "-p": true, "-m": true, "--dport": true, "-j": true, "--to-destination": true,
}

// metadataRuleMatch reads a rule's arguments as flag/value pairs, in any order, refusing any flag a
// metadata redirect does not carry and any flag given twice. An extra match narrows or changes what
// the rule does, so a rule carrying one was not written by the earlier layout. A negation ("! -d") is
// refused the same way: "!" is not one of the flags.
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
