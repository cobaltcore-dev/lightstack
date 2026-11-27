package neutron

import (
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
)

// A security group rule is checked on create as upstream checks it (neutron master), in upstream's
// order: the API layer's converters and validators (neutron/extensions/securitygroup.py), then
// _validate_security_group_rule (neutron/db/securitygroups_db.py). A rule's remote and protocol go
// into the input iptables-restore reads, as root, on every node binding a port with the group, so
// nothing is stored that these do not accept (SPEC-033 FR-059).

// securityGroupRule is a rule as a create request gives it and, once checked, as it is stored.
type securityGroupRule struct {
	SecurityGroupID string  `json:"security_group_id" binding:"required"`
	Direction       string  `json:"direction" binding:"required"`
	EtherType       *string `json:"ethertype"`
	Protocol        *string `json:"protocol"`
	PortRangeMin    *int    `json:"port_range_min"`
	PortRangeMax    *int    `json:"port_range_max"`
	RemoteIPPrefix  *string `json:"remote_ip_prefix"`
	RemoteGroupID   *string `json:"remote_group_id"`
}

// The protocols upstream allows ports on (SG_PORT_PROTO_NUMS: dccp, sctp, tcp, udp, udplite), and
// those only an IPv6 rule may name, as _validate_ethertype_and_protocol compares them.
var (
	portProtocols     = []int{33, 132, 6, 17, 136}
	ipv6OnlyProtocols = []string{"ipv6-encap", "ipv6-frag", "ipv6-icmp", "icmpv6", "ipv6-nonxt", "ipv6-opts",
		"ipv6-route", "41", "44", "58", "59", "60", "43"}
)

// checked is the rule as upstream stores it, or upstream's refusal of it.
func (r securityGroupRule) checked() (securityGroupRule, *common.OpenStackError) {
	// The API layer, attribute by attribute: its converter, then its validator. An empty remote group is
	// none, as upstream reads a falsy one (SPEC-033 T266).
	if r.RemoteGroupID != nil && *r.RemoteGroupID == "" {
		r.RemoteGroupID = nil
	}
	if r.Direction != "ingress" && r.Direction != "egress" {
		return r, invalidAttribute("direction", r.Direction+" is not in valid_values")
	}
	var refusal *common.OpenStackError
	if r.Protocol, refusal = convertProtocol(r.Protocol); refusal != nil {
		return r, refusal
	}
	for _, port := range []*int{r.PortRangeMin, r.PortRangeMax} {
		if port != nil && (*port < 0 || *port > 65535) {
			return r, ruleRefusal("SecurityGroupInvalidPortValue", fmt.Sprintf("Invalid value for port %d", *port))
		}
	}
	if r.EtherType, refusal = convertEtherType(r.EtherType); refusal != nil {
		return r, refusal
	}
	var prefix netip.Prefix
	if r.RemoteIPPrefix, prefix, refusal = convertRemoteIPPrefix(r.RemoteIPPrefix); refusal != nil {
		return r, refusal
	}

	// _validate_security_group_rule, in its order.
	if *r.EtherType == "IPv6" && r.Protocol != nil { // _make_canonical_ipv6_icmp_protocol
		switch *r.Protocol {
		case "icmp", "icmpv6":
			r.Protocol = new("ipv6-icmp")
		case "1":
			r.Protocol = new("58")
		}
	}
	if r.PortRangeMin != nil && r.PortRangeMax != nil && *r.PortRangeMin == 1 && *r.PortRangeMax == 65535 {
		r.PortRangeMin, r.PortRangeMax = nil, nil // _make_canonical_port_range: every port is no port
	}
	if refusal = checkPortRange(r); refusal != nil {
		return r, refusal
	}
	if r.RemoteIPPrefix != nil && prefix.Addr().Is4() != (*r.EtherType == "IPv4") {
		return r, ruleRefusal("SecurityGroupRuleParameterConflict",
			fmt.Sprintf("Conflicting value ethertype %s for CIDR %s", *r.EtherType, *r.RemoteIPPrefix))
	}
	if *r.EtherType == "IPv4" && r.Protocol != nil && slices.Contains(ipv6OnlyProtocols, *r.Protocol) {
		return r, ruleRefusal("SecurityGroupEthertypeConflictWithProtocol",
			fmt.Sprintf("Invalid ethertype %s for protocol %s.", *r.EtherType, *r.Protocol))
	}
	if r.RemoteIPPrefix != nil && r.RemoteGroupID != nil {
		return r, ruleRefusal("SecurityGroupMultipleRemoteEntites",
			"Only one of remote_ip_prefix or remote_group_id or remote_address_group_id may be provided.")
	}
	return r, nil
}

// convertProtocol is upstream's convert_protocol: none for none and "any"; a number from 0 to 255 as
// given; one of upstream's names, lowercased. "ANY" is not "any" there, and is stored as "any",
// which means any too.
func convertProtocol(p *string) (*string, *common.OpenStackError) {
	if p == nil || *p == "any" {
		return nil, nil
	}
	if n, err := strconv.Atoi(*p); err == nil {
		if n < 0 || n > 255 {
			return nil, invalidProtocol(*p)
		}
		return p, nil
	}
	name := strings.ToLower(*p)
	if _, known := networking.ProtocolNumber(name); !known && name != "any" {
		return nil, invalidProtocol(*p)
	}
	return &name, nil
}

// invalidProtocol is upstream's SecurityGroupRuleInvalidProtocol, listing what it accepts as Python
// prints the tuple.
func invalidProtocol(p string) *common.OpenStackError {
	values := "(None, 'any'"
	for _, name := range networking.ProtocolNames() {
		values += ", '" + name + "'"
	}
	return ruleRefusal("SecurityGroupRuleInvalidProtocol", fmt.Sprintf("Security group rule protocol %s not supported. "+
		"Only protocol values %s) and integer representations [0 to 255] are supported.", p, values))
}

// convertEtherType is upstream's convert_ethertype_to_case_insensitive, after the attribute's
// default, IPv4.
func convertEtherType(e *string) (*string, *common.OpenStackError) {
	if e == nil {
		return new("IPv4"), nil
	}
	for _, known := range []string{"IPv4", "IPv6"} {
		if strings.EqualFold(*e, known) {
			return &known, nil
		}
	}
	return nil, ruleRefusal("SecurityGroupRuleInvalidEtherType", fmt.Sprintf(
		"Security group rule for ethertype '%s' not supported. Allowed values are ['IPv4', 'IPv6'].", *e))
}

// convertRemoteIPPrefix is upstream's convert_ip_prefix_to_cidr: none for none and "", else the
// prefix as netaddr prints it.
func convertRemoteIPPrefix(p *string) (*string, netip.Prefix, *common.OpenStackError) {
	if p == nil || *p == "" {
		return nil, netip.Prefix{}, nil
	}
	prefix, ok := parseIPNetwork(*p)
	if !ok {
		return nil, netip.Prefix{}, ruleRefusal("InvalidCIDR", fmt.Sprintf("Invalid CIDR %s given as IP prefix.", *p))
	}
	return new(prefix.String()), prefix, nil
}

// parseIPNetwork reads an address or a network as netaddr.IPNetwork does: an address alone, which is
// its host prefix, or followed by "/" and a prefix length, a netmask or a hostmask. The address keeps
// its host bits, as netaddr prints them: 10.0.0.5/24 stays 10.0.0.5/24.
func parseIPNetwork(s string) (netip.Prefix, bool) {
	address, mask, hasMask := strings.Cut(s, "/")
	addr, err := netip.ParseAddr(address)
	if err != nil || addr.Zone() != "" {
		return netip.Prefix{}, false
	}
	bits := addr.BitLen()
	if hasMask {
		var ok bool
		if bits, ok = maskLength(addr, mask); !ok {
			return netip.Prefix{}, false
		}
	}
	return netip.PrefixFrom(addr, bits), true
}

// maskLength is the prefix length a mask stands for: a number up to the address's length, or a
// netmask (ones, then zeros) or a hostmask (zeros, then ones) of the address's family, read as a
// netmask first, as netaddr does.
func maskLength(addr netip.Addr, mask string) (int, bool) {
	if n, err := strconv.Atoi(mask); err == nil {
		return n, n >= 0 && n <= addr.BitLen()
	}
	m, err := netip.ParseAddr(mask)
	if err != nil || m.Zone() != "" || m.Is4() != addr.Is4() {
		return 0, false
	}
	raw := m.AsSlice()
	width := len(raw) * 8
	bit := func(i int) bool { return raw[i/8]&(0x80>>(i%8)) != 0 }
	for _, lead := range []bool{true, false} {
		n := 0
		for n < width && bit(n) == lead {
			n++
		}
		rest := n
		for rest < width && bit(rest) != lead {
			rest++
		}
		if rest == width {
			return n, true
		}
	}
	return 0, false
}

// checkPortRange is upstream's _validate_port_range.
func checkPortRange(r securityGroupRule) *common.OpenStackError {
	first, last := r.PortRangeMin, r.PortRangeMax
	if first == nil && last == nil {
		return nil
	}
	if r.Protocol == nil {
		return ruleRefusal("SecurityGroupProtocolRequiredWithPorts", "Must also specify protocol if port range is given.")
	}
	number, ok := networking.ProtocolNumber(*r.Protocol)
	switch {
	case ok && slices.Contains(portProtocols, number):
		if (first != nil && *first == 0) || (last != nil && *last == 0) {
			return ruleRefusal("SecurityGroupInvalidPortValue", "Invalid value for port 0")
		}
		if first == nil || last == nil || *first > *last {
			return ruleRefusal("SecurityGroupInvalidPortRange",
				"For TCP/UDP protocols, port_range_min must be <= port_range_max")
		}
	case ok && (number == 1 || number == 58):
		for _, v := range []struct {
			field, attr string
			value       *int
		}{{"type", "port_range_min", first}, {"code", "port_range_max", last}} {
			if v.value != nil && *v.value > 255 {
				return ruleRefusal("SecurityGroupInvalidIcmpValue",
					fmt.Sprintf("Invalid value for ICMP %s (%s) %d. It must be 0 to 255.", v.field, v.attr, *v.value))
			}
		}
		if first == nil {
			return ruleRefusal("SecurityGroupMissingIcmpType", fmt.Sprintf(
				"ICMP code (port-range-max) %d is provided but ICMP type (port-range-min) is missing.", *last))
		}
	default:
		protocol := "None" // "any": upstream's number for it is None
		if ok {
			protocol = strconv.Itoa(number)
		}
		return ruleRefusal("SecurityGroupInvalidProtocolForPort", fmt.Sprintf(
			"Ports cannot be specified for protocol %s. Ports are only supported for DCCP, SCTP, TCP, UDP, UDPLITE.",
			protocol))
	}
	return nil
}

// ruleRefusal is one of upstream's rule checks' refusals: an InvalidInput, answered 400 under its own
// name.
func ruleRefusal(neutronType, message string) *common.OpenStackError {
	return common.NewNeutronError(http.StatusBadRequest, neutronType, message)
}
