package networking

import "testing"

// The retired-state removal deletes the earlier layout's metadata DNAT out of the host's
// nat/PREROUTING chain — the same chain k3s/kube-proxy, docker and other NAT write to. Which of its
// rules it may touch is decided by isMetadataRule, so that match is pinned here rather than
// exercised against a live kernel: a false positive would delete another component's rule.
//
// Found during SPEC-024's real-mode host pass (cloud-storage#546 follow-up finding 1): 101 metadata
// DNAT rules on one host, 98 of them naming bridges that no longer existed.

func TestIsMetadataRule_MatchesOnlyOurOwnRules(t *testing.T) {
	ours := []struct{ name, line string }{
		{"as iptables -S prints it", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1:8775"},
		{"bare address without the /32", "-A PREROUTING -d 169.254.169.254 -i br-00000000 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.0.0.1:8775"},
		{"flags in another order", "-A PREROUTING -i br-f28a8a32 -d 169.254.169.254/32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1:8775"},
		{"exactly as Ensure wrote it, without the implicit -m tcp", "-A PREROUTING -i br-f28a8a32 -d 169.254.169.254 -p tcp --dport 80 -j DNAT --to-destination 10.244.10.1:8775"},
	}
	for _, tc := range ours {
		t.Run(tc.name, func(t *testing.T) {
			if !isMetadataRule(tc.line) {
				t.Errorf("isMetadataRule(%q) = not ours; want ours (the prune would never reclaim it)", tc.line)
			}
		})
	}

	foreign := []struct {
		name string
		line string
	}{
		{"kube-proxy jump", `-A PREROUTING -m comment --comment "kubernetes service portals" -j KUBE-SERVICES`},
		{"docker jump", "-A PREROUTING -m addrtype --dst-type LOCAL -j DOCKER"},
		{"our own floating-IP DNAT", "-A PREROUTING -d 203.0.113.5/32 -j DNAT --to-destination 10.244.10.7"},
		{"metadata redirected elsewhere, not by us", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1:80"},
		{"docker's 12-hex bridge", "-A PREROUTING -d 169.254.169.254/32 -i br-0123456789ab -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.0.0.1:8775"},
		{"not a bridge at all", "-A PREROUTING -d 169.254.169.254/32 -i eth0 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.0.0.1:8775"},
		{"different destination port", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 8080 -j DNAT --to-destination 10.0.0.1:8775"},
		{"udp not tcp", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p udp -m udp --dport 80 -j DNAT --to-destination 10.0.0.1:8775"},
		{"REDIRECT not DNAT", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j REDIRECT --to-ports 8775"},
		{"negated destination", "-A PREROUTING ! -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.0.0.1:8775"},
		{"same shape in another chain", "-A OUTPUT -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.0.0.1:8775"},
		{"an extra source match", "-A PREROUTING -s 10.0.0.0/8 -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1:8775"},
		{"an extra comment match", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -m comment --comment other -j DNAT --to-destination 10.244.10.1:8775"},
		{"a flag given twice", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -i br-00000000 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1:8775"},
		{"an address-range destination", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1-10.244.10.5:8775"},
		{"a destination with two ports", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1:123:8775"},
		{"a destination without an address", "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination :8775"},
		{"chain policy line", "-P PREROUTING ACCEPT"},
		{"blank", ""},
	}
	for _, tc := range foreign {
		t.Run(tc.name, func(t *testing.T) {
			if isMetadataRule(tc.line) {
				t.Errorf("isMetadataRule(%q) = ours; want not ours — the prune must never delete it", tc.line)
			}
		})
	}
}
