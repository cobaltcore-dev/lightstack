package networking

import (
	"strconv"
	"strings"
)

// ipProtocols is neutron-lib's IP_PROTOCOL_MAP, in its order: the protocol names a security group
// rule may give, with upstream's numbers for them. icmpv6 is upstream's older name for ipv6-icmp.
var ipProtocols = []struct {
	name   string
	number int
}{
	{"ah", 51}, {"dccp", 33}, {"egp", 8}, {"esp", 50}, {"gre", 47}, {"hopopt", 0}, {"icmp", 1},
	{"igmp", 2}, {"ip", 0}, {"ipip", 4}, {"ipv6-encap", 41}, {"ipv6-frag", 44}, {"ipv6-icmp", 58},
	{"icmpv6", 58}, {"ipv6-nonxt", 59}, {"ipv6-opts", 60}, {"ipv6-route", 43}, {"ospf", 89},
	{"pgm", 113}, {"rsvp", 46}, {"sctp", 132}, {"tcp", 6}, {"udp", 17}, {"udplite", 136}, {"vrrp", 112},
}

// ProtocolNames are the protocol names a security group rule may give, in upstream's order.
func ProtocolNames() []string {
	names := make([]string, len(ipProtocols))
	for i, p := range ipProtocols {
		names[i] = p.name
	}
	return names
}

// ProtocolNumber is the IP protocol a security group rule's protocol stands for, as upstream reads
// it: one of upstream's names, in any case, or a decimal number from 0 to 255. ok is false for
// anything else, "any" and "" included.
func ProtocolNumber(p string) (int, bool) {
	if n, err := strconv.Atoi(p); err == nil {
		return n, n >= 0 && n <= 255
	}
	for _, known := range ipProtocols {
		if strings.EqualFold(known.name, p) {
			return known.number, true
		}
	}
	return 0, false
}
