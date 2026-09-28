package networking

import (
	"reflect"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/networking/ebpf"
)

// TestEBPFIngressRules pins how Neutron rules become XDP filter rules. The filter matches an
// IPv4 source CIDR and denies anything unmatched, so a translation that widens a rule opens the
// port. Two did:
//   - a rule whose only source is remote_group_id became 0.0.0.0/0 — "SSH from the bastion
//     group" admitted SSH from everywhere. It must admit nothing instead.
//   - an IPv6 rule became an IPv4 rule — "SSH over IPv6 from anywhere" opened IPv4 SSH to the
//     world, and one with an IPv6 prefix failed the port's whole rule set. The filter parses
//     IPv4 only, so an IPv6 rule has nothing to match there and must be left out.
func TestEBPFIngressRules(t *testing.T) {
	got := ebpfIngressRules([]SecurityGroupRule{
		{ID: "egress", Direction: "egress", Protocol: "tcp", PortRangeMin: 25, PortRangeMax: 25},
		{ID: "any-source", Direction: "ingress", Protocol: "tcp", PortRangeMin: 80, PortRangeMax: 80},
		{ID: "cidr", Direction: "ingress", Protocol: "tcp", PortRangeMin: 443, PortRangeMax: 443, RemoteIPPrefix: "10.0.0.0/8"},
		{ID: "remote-group", Direction: "ingress", Protocol: "tcp", PortRangeMin: 22, PortRangeMax: 22,
			RemoteGroupID: "6f0d6f0e-8a3b-4c4e-9a53-2f1f6c3b9d11"},
		{ID: "ipv6-any", Direction: "ingress", EtherType: "IPv6", Protocol: "tcp", PortRangeMin: 22, PortRangeMax: 22},
		{ID: "ipv6-prefix", Direction: "ingress", EtherType: "IPv6", Protocol: "tcp", PortRangeMin: 22, PortRangeMax: 22,
			RemoteIPPrefix: "::/0"},
		{ID: "ipv6-lowercase", Direction: "ingress", EtherType: "ipv6", Protocol: "udp", PortRangeMin: 53, PortRangeMax: 53},
	})
	want := []ebpf.SecurityGroupRule{
		{Protocol: 6, PortMin: 80, PortMax: 80, RemoteIPCIDR: "0.0.0.0/0"},
		{Protocol: 6, PortMin: 443, PortMax: 443, RemoteIPCIDR: "10.0.0.0/8"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ebpfIngressRules() =\n  %+v\nwant\n  %+v", got, want)
	}
}
