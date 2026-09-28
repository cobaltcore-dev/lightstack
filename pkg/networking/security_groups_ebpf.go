package networking

import (
	"fmt"
	"net"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/cobaltcore-dev/lightstack/pkg/networking/ebpf"
)

// ApplySecurityGroupToPort applies security group rules to a specific port (eBPF-specific)
func (m *SecurityGroupManager) ApplySecurityGroupToPort(portID string, portMAC net.HardwareAddr, rules []SecurityGroupRule) error {
	if m.mode != "ebpf" {
		return fmt.Errorf("ApplySecurityGroupToPort only supported in eBPF mode")
	}

	// Convert port MAC address to port ID hash
	portIDHash := ebpf.MACToPortID(portMAC)

	// Update eBPF map
	return m.ebpfMgr.UpdateSecurityGroup(portIDHash, ebpfIngressRules(rules))
}

// ebpfIngressRules converts Neutron security group rules to the XDP filter's format.
//
// A rule whose only source is remote_group_id is dropped, not widened. The filter matches a
// source CIDR, and "members of group X" would need those members' addresses — which this
// per-port snapshot, programmed once when the port is created, cannot keep current as members
// come and go or their addresses are reused. The empty-prefix default below used to turn such a
// rule into 0.0.0.0/0, admitting every source. Dropping it fails closed: the filter denies
// anything unmatched, so the rule admits nothing — as the north-south filter already treats it
// (portIngressRules skips remote-group rules).
//
// IPv6 rules are dropped too. The filter parses IPv4 only (IPv6 frames pass it untouched), so an
// IPv6 rule has nothing to match there — but translated anyway, one without a prefix became IPv4
// 0.0.0.0/0, and one with an IPv6 prefix made UpdateSecurityGroup reject the port's whole rule
// set. Ethertype is compared case-insensitively: the API stores whatever spelling it is sent.
func ebpfIngressRules(rules []SecurityGroupRule) []ebpf.SecurityGroupRule {
	ebpfRules := make([]ebpf.SecurityGroupRule, 0, len(rules))
	for _, rule := range rules {
		// Skip egress rules (eBPF XDP only handles ingress)
		if rule.Direction == "egress" {
			continue
		}
		if strings.EqualFold(rule.EtherType, "IPv6") {
			continue
		}
		if rule.RemoteIPPrefix == "" && rule.RemoteGroupID != "" {
			log.Warn().Str("rule_id", rule.ID).Str("remote_group_id", rule.RemoteGroupID).
				Msg("eBPF filter cannot express a remote-group source; the rule admits nothing")
			continue
		}

		// Convert protocol string to int
		protocol := ebpf.ProtocolStringToInt(rule.Protocol)

		// Use default CIDR if not specified
		remoteIP := rule.RemoteIPPrefix
		if remoteIP == "" {
			remoteIP = "0.0.0.0/0" // Allow from any
		}

		// Convert port range (Neutron uses -1 for "any")
		portMin := uint16(rule.PortRangeMin)
		portMax := uint16(rule.PortRangeMax)
		if rule.PortRangeMin == -1 {
			portMin = 0
			portMax = 65535
		}

		ebpfRules = append(ebpfRules, ebpf.SecurityGroupRule{
			Protocol:     protocol,
			Direction:    0, // ingress
			PortMin:      portMin,
			PortMax:      portMax,
			RemoteIPCIDR: remoteIP,
		})
	}
	return ebpfRules
}

// RemoveSecurityGroupFromPort removes security group rules from a specific port (eBPF-specific)
func (m *SecurityGroupManager) RemoveSecurityGroupFromPort(portID string, portMAC net.HardwareAddr) error {
	if m.mode != "ebpf" {
		return fmt.Errorf("RemoveSecurityGroupFromPort only supported in eBPF mode")
	}

	portIDHash := ebpf.MACToPortID(portMAC)
	return m.ebpfMgr.RemoveSecurityGroup(portIDHash)
}

// GetStatistics returns eBPF packet filter statistics
func (m *SecurityGroupManager) GetStatistics() (*ebpf.Statistics, error) {
	if m.mode != "ebpf" {
		return nil, fmt.Errorf("GetStatistics only supported in eBPF mode")
	}

	return m.ebpfMgr.GetStatistics()
}

// AttachToInterface attaches the XDP program to a network interface (eBPF-specific)
func (m *SecurityGroupManager) AttachToInterface(ifaceName string) error {
	if m.mode != "ebpf" {
		return fmt.Errorf("AttachToInterface only supported in eBPF mode")
	}

	if m.ebpfMgr == nil {
		return fmt.Errorf("eBPF manager not initialized")
	}

	return m.ebpfMgr.AttachToInterface(ifaceName)
}

// DetachFromInterface detaches the XDP program from a network interface (eBPF-specific)
func (m *SecurityGroupManager) DetachFromInterface(ifaceName string) error {
	if m.mode != "ebpf" {
		return fmt.Errorf("DetachFromInterface only supported in eBPF mode")
	}

	if m.ebpfMgr == nil {
		return fmt.Errorf("eBPF manager not initialized")
	}

	return m.ebpfMgr.DetachFromInterface(ifaceName)
}

// Close releases all resources (eBPF-specific)
func (m *SecurityGroupManager) Close() error {
	if m.mode == "ebpf" && m.ebpfMgr != nil {
		return m.ebpfMgr.Close()
	}
	return nil
}
