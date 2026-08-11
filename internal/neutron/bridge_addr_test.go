package neutron

import "testing"

// TestBridgeAddrFor pins FR-023a's address decision (SPEC-024 / cloud-storage#546).
//
// bridgeAddrFor answers one question: which address does a network's bridge carry? With a gateway it
// is the gateway; without one it is the first host address of the CIDR — held as the bridge's own
// on-link address so dnsmasq can bind and the metadata DNAT can hang off it, but never advertised as
// a default route.
//
// It had no test because neither caller reaches it in stub mode — applyDHCPForNetwork returns early —
// so no amount of handler testing could touch it, and the gap was invisible in the suite. The
// gatewayless path is host-validated (real-mode R4), but a host pass is not a regression guard.
func TestBridgeAddrFor(t *testing.T) {
	str := func(s string) *string { return &s }

	for _, tc := range []struct {
		name    string
		gateway *string
		cidr    string
		want    string
	}{
		{"a declared gateway is used verbatim", str("10.244.10.1"), "10.244.10.0/24", "10.244.10.1"},
		{
			// Upstream lets a gateway sit anywhere in the subnet, not only at .1.
			name:    "a gateway away from the first address is still used verbatim",
			gateway: str("10.244.10.254"), cidr: "10.244.10.0/24", want: "10.244.10.254",
		},
		{"no gateway falls back to the first host address", nil, "10.244.20.0/24", "10.244.20.1"},
		{
			// `--gateway none` arrives as an explicit empty string on some paths and as nil on
			// others; both mean the same thing and must not diverge.
			name: "an empty gateway is the same as none", gateway: str(""), cidr: "10.244.20.0/24", want: "10.244.20.1",
		},
		{
			// The prefix is not always /24. A fallback that assumed the last octet would land
			// outside a smaller subnet — the same class of bug dhcpRange was fixed for.
			name: "a /26 takes the first host address of ITS range", gateway: nil, cidr: "10.0.0.64/26", want: "10.0.0.65",
		},
		{"a /30 point-to-point range", nil, "192.0.2.4/30", "192.0.2.5"},
		{
			// The caller treats "" as "skip this network" rather than programming a bogus address.
			name: "an unparseable CIDR yields the skip signal", gateway: nil, cidr: "not-a-cidr", want: "",
		},
		{
			// A gateway wins even when the CIDR is unusable: the address was given, not derived.
			name: "a gateway survives an unparseable CIDR", gateway: str("10.0.0.1"), cidr: "garbage", want: "10.0.0.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bridgeAddrFor(tc.gateway, tc.cidr); got != tc.want {
				t.Errorf("bridgeAddrFor(%v, %q) = %q; want %q", tc.gateway, tc.cidr, got, tc.want)
			}
		})
	}
}
