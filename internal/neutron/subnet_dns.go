package neutron

import (
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/common"
)

// maxDNSNameservers is upstream's max_dns_nameservers, at its default.
const maxDNSNameservers = 5

// checkNameservers checks a new subnet's DNS servers as upstream does (neutron master): neutron-lib's
// validate_nameservers — each an IP address, an IPv4 one written as upstream writes it, none twice —
// then _validate_subnet's count and IP version. They go into the dnsmasq configuration of the
// network's DHCP, which starts as root on the network node, so nothing is stored that these refuse
// (SPEC-033 FR-060; SPEC-033 T172).
func checkNameservers(servers []string, ipVersion int) *common.OpenStackError {
	seen := map[string]bool{}
	addrs := make([]netip.Addr, 0, len(servers))
	for _, host := range servers {
		addr, reason := parseIPAddress(host)
		if reason != "" {
			return invalidAttribute("dns_nameservers", fmt.Sprintf("'%s' is not a valid nameserver. %s", host, reason))
		}
		if seen[host] {
			return invalidAttribute("dns_nameservers", fmt.Sprintf("Duplicate nameserver '%s'", host))
		}
		seen[host] = true
		addrs = append(addrs, addr)
	}
	if len(servers) > maxDNSNameservers {
		return common.NewNeutronError(http.StatusBadRequest, "DNSNameServersExhausted", fmt.Sprintf(
			"Unable to complete operation for new subnet. The number of DNS nameservers exceeds the limit %d.", maxDNSNameservers))
	}
	for i, addr := range addrs {
		version := 6
		if addr.Is4() {
			version = 4
		}
		if version != ipVersion {
			return invalidInput(fmt.Sprintf("dns_nameserver '%s' does not match the ip_version '%d'", servers[i], ipVersion))
		}
	}
	return nil
}

// parseIPAddress is upstream's validate_ip_address: the address, or why upstream does not accept it. An
// IPv4 address must be written as upstream writes it: it reads 010.0.0.1 as 10.0.0.1, and recommends
// that instead. Upstream's other check, three dots or a colon, guards against netaddr's shorter IPv4
// forms ("10", "10.1.2"), which netip refuses already.
func parseIPAddress(s string) (netip.Addr, string) {
	if addr, err := netip.ParseAddr(s); err == nil && addr.Zone() == "" {
		return addr, ""
	}
	if written, ok := zeroFilledIPv4(s); ok {
		return netip.Addr{}, fmt.Sprintf("'%s' is not an accepted IP address, '%s' is recommended", s, written)
	}
	return netip.Addr{}, fmt.Sprintf("'%s' is not a valid IP address", s)
}

// zeroFilledIPv4 is an IPv4 address with leading zeros in its octets, as netaddr's ZEROFILL reads it.
func zeroFilledIPv4(s string) (string, bool) {
	octets := strings.Split(s, ".")
	if len(octets) != 4 {
		return "", false
	}
	for i, octet := range octets {
		if octet == "" || strings.Trim(octet, "0123456789") != "" {
			return "", false
		}
		n, err := strconv.Atoi(octet)
		if err != nil || n > 255 {
			return "", false
		}
		octets[i] = strconv.Itoa(n)
	}
	return strings.Join(octets, "."), true
}
