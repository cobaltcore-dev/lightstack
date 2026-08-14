package networking

import (
	"reflect"
	"testing"
)

// The metadata DNAT prune deletes rules out of the host's nat/PREROUTING chain — the same chain
// k3s/kube-proxy, docker and our own floating-IP NAT write to. Everything it is allowed to touch is
// decided by parseMetadataRule, so that match is pinned here rather than exercised against a live
// kernel: a false positive would delete another component's rule.
//
// Found during SPEC-024's real-mode host pass (cloud-storage#546 follow-up finding 1): 101 metadata
// DNAT rules on one host, 98 of them naming bridges that no longer existed.

func TestParseMetadataRule_MatchesOnlyOurOwnRules(t *testing.T) {
	ours := []struct {
		name       string
		line       string
		wantBridge string
		wantDest   string
	}{
		{
			name:       "as iptables -S prints it",
			line:       "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1:8775",
			wantBridge: "br-f28a8a32",
			wantDest:   "10.244.10.1:8775",
		},
		{
			name:       "bare address without the /32",
			line:       "-A PREROUTING -d 169.254.169.254 -i br-00000000 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.0.0.1:8775",
			wantBridge: "br-00000000",
			wantDest:   "10.0.0.1:8775",
		},
		{
			name:       "flags in another order",
			line:       "-A PREROUTING -i br-f28a8a32 -d 169.254.169.254/32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1:8775",
			wantBridge: "br-f28a8a32",
			wantDest:   "10.244.10.1:8775",
		},
		{
			name:       "exactly as Ensure writes it, without the implicit -m tcp",
			line:       "-A PREROUTING -i br-f28a8a32 -d 169.254.169.254 -p tcp --dport 80 -j DNAT --to-destination 10.244.10.1:8775",
			wantBridge: "br-f28a8a32",
			wantDest:   "10.244.10.1:8775",
		},
	}
	for _, tc := range ours {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseMetadataRule(tc.line)
			if !ok {
				t.Fatalf("parseMetadataRule(%q) = not ours; want ours (the prune would never reclaim it)", tc.line)
			}
			if got.bridge != tc.wantBridge || got.dest != tc.wantDest {
				t.Errorf("parsed bridge=%q dest=%q; want bridge=%q dest=%q", got.bridge, got.dest, tc.wantBridge, tc.wantDest)
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
			if r, ok := parseMetadataRule(tc.line); ok {
				t.Errorf("parseMetadataRule(%q) = ours (bridge=%q dest=%q); want not ours — the prune must never delete it",
					tc.line, r.bridge, r.dest)
			}
		})
	}
}

// The delete replays the rule's own tokens rather than rebuilding them, so a rule iptables printed
// is a rule iptables accepts back.
func TestParseMetadataRule_KeepsArgsForReplay(t *testing.T) {
	const line = "-A PREROUTING -d 169.254.169.254/32 -i br-f28a8a32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.244.10.1:8775"
	r, ok := parseMetadataRule(line)
	if !ok {
		t.Fatalf("parseMetadataRule(%q) = not ours", line)
	}
	want := []string{
		"-d", "169.254.169.254/32", "-i", "br-f28a8a32",
		"-p", "tcp", "-m", "tcp", "--dport", "80",
		"-j", "DNAT", "--to-destination", "10.244.10.1:8775",
	}
	if !reflect.DeepEqual(r.args, want) {
		t.Errorf("args = %v; want %v (everything after \"-A PREROUTING\", verbatim)", r.args, want)
	}
}

// staleMetadataRules is the whole prune decision. It reconciles the rule SET against what the
// database says should exist, rather than only asking whether each rule's bridge is still present —
// the difference is the collision case below, which a liveness-only sweep keeps forever.
func TestStaleMetadataRules(t *testing.T) {
	rule := func(bridge, dest string) metadataRule {
		return metadataRule{bridge: bridge, dest: dest}
	}
	names := func(rs []metadataRule) []string {
		out := []string{}
		for _, r := range rs {
			out = append(out, r.bridge+"->"+r.dest)
		}
		return out
	}

	for _, tc := range []struct {
		name    string
		rules   []metadataRule
		desired map[string]string
		want    []string
	}{
		{
			name:    "a live rule is kept",
			rules:   []metadataRule{rule("br-f28a8a32", "10.244.10.1:8775")},
			desired: map[string]string{"br-f28a8a32": "10.244.10.1"},
			want:    []string{},
		},
		{
			name:    "a deleted network's rule is reclaimed",
			rules:   []metadataRule{rule("br-deadbeef", "10.254.0.1:8775")},
			desired: map[string]string{"br-f28a8a32": "10.244.10.1"},
			want:    []string{"br-deadbeef->10.254.0.1:8775"},
		},
		{
			// The 1.4a collision: a new network draws a UUID sharing the first 8 hex chars with a
			// long-deleted one, so the bridge IS live but an earlier rule sends its guests to a
			// gateway that no longer exists. PREROUTING is ordered, the stale rule is first, and it
			// wins — metadata breaks silently. Matching on liveness alone would keep it.
			name: "a live bridge with the wrong destination is reclaimed",
			rules: []metadataRule{
				rule("br-f28a8a32", "192.168.100.1:8775"),
				rule("br-f28a8a32", "10.244.10.1:8775"),
			},
			desired: map[string]string{"br-f28a8a32": "10.244.10.1"},
			want:    []string{"br-f28a8a32->192.168.100.1:8775"},
		},
		{
			name:    "an empty desired set reclaims everything, which is teardown",
			rules:   []metadataRule{rule("br-f28a8a32", "10.244.10.1:8775"), rule("br-deadbeef", "10.254.0.1:8775")},
			desired: nil,
			want:    []string{"br-f28a8a32->10.244.10.1:8775", "br-deadbeef->10.254.0.1:8775"},
		},
		{
			name:    "no rules, nothing to do",
			rules:   nil,
			desired: map[string]string{"br-f28a8a32": "10.244.10.1"},
			want:    []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := names(staleMetadataRules(tc.rules, tc.desired))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("staleMetadataRules() = %v; want %v", got, tc.want)
			}
		})
	}
}

// Every host-touching entry point is mode-gated, so a stub-mode process never shells out to
// iptables — the property the whole stub mode rests on.
func TestMetadataRedirect_StubModeTouchesNothing(t *testing.T) {
	mr := NewMetadataRedirect("stub")
	if err := mr.Ensure("br-f28a8a32", "10.244.10.1"); err != nil {
		t.Errorf("Ensure in stub mode: %v; want no-op", err)
	}
	if err := mr.RemoveForBridge("br-f28a8a32"); err != nil {
		t.Errorf("RemoveForBridge in stub mode: %v; want no-op", err)
	}
	n, err := mr.Prune(func() (map[string]string, error) { return nil, nil })
	if err != nil {
		t.Errorf("Prune in stub mode: %v; want no-op", err)
	}
	if n != 0 {
		t.Errorf("Prune in stub mode reclaimed %d rule(s); want 0", n)
	}
}
