package networking

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/rs/zerolog/log"
)

// A network's DHCP on the network node (SPEC-033 FR-037; research R29;
// contracts/router-namespace-dataplane.md): upstream's DHCP agent layout with forced metadata. The
// namespace qdhcp-<network> holds one device, the network's DHCP port, on the network's bridge through
// a veth; it carries the port's address on each DHCP subnet and 169.254.169.254, and dnsmasq answers
// in it with static ranges, so only a port with a reservation gets a lease. Lightstack's metadata
// listener in the namespace (internal/metadata) answers on 169.254.169.254.

// MetadataIP is the well-known link-local address cloud-init and the EC2 metadata datasource query
// for instance metadata. A network's DHCP namespace holds it, and Lightstack's metadata listener
// answers on it there.
const MetadataIP = "169.254.169.254"

// DHCPState is a network's DHCP as its records say, and so as the network node must hold it.
type DHCPState struct {
	NetworkID string
	PortID    string // the network's DHCP port: names its device
	Bridge    string // the network's bridge
	Subnets   []DHCPSubnet
	Hosts     []DHCPHost // a reservation per port with an address on a DHCP subnet
}

// DHCPSubnet is one of a network's DHCP subnets, which are IPv4.
type DHCPSubnet struct {
	ID      string
	CIDR    string   // its range
	Address string   // the DHCP port's address on it
	Gateway string   // "" for none
	DNS     []string // its DNS servers
}

// DHCPHost is a port's reservation: its MAC and its address.
type DHCPHost struct{ MAC, IP string }

// DHCPNamespace is a network's DHCP namespace on the network node.
func DHCPNamespace(networkID string) string { return "qdhcp-" + networkID }

// dhcpPlan is what a network's DHCP asks of the node apart from dnsmasq.
type dhcpPlan struct {
	Namespace string
	Device    string   // the DHCP port's device in the namespace, tap<port[:11]>
	HostEnd   string   // its host end, ld-<port[:11]>, on Bridge
	Bridge    string   // the network's bridge
	Addresses []string // the DHCP port's addresses with their subnets' prefixes, then 169.254.169.254/32
}

// planDHCP is the network's DHCP namespace, device and addresses.
func planDHCP(s DHCPState) dhcpPlan {
	p := dhcpPlan{Namespace: DHCPNamespace(s.NetworkID), Device: legName("tap", s.PortID), HostEnd: legName("ld-", s.PortID), Bridge: s.Bridge}
	for _, sub := range servedSubnets(s.Subnets) {
		p.Addresses = append(p.Addresses, fmt.Sprintf("%s/%d", sub.address, sub.prefix.Bits()))
	}
	p.Addresses = append(p.Addresses, MetadataIP+"/32")
	return p
}

// servedSubnet is a DHCP subnet dnsmasq serves: its range and the DHCP port's address on it, parsed.
type servedSubnet struct {
	DHCPSubnet
	prefix  netip.Prefix
	address netip.Addr
}

// servedSubnets are the subnets the network's DHCP serves, sorted by id: those whose range is an IPv4
// prefix holding the DHCP port's address. The API stores no other (SPEC-033 T247); one stored before
// is left out rather than written as an IPv4 range, which has no mask for an IPv6 prefix length.
func servedSubnets(subnets []DHCPSubnet) []servedSubnet {
	var out []servedSubnet
	for _, sub := range subnets {
		prefix, perr := netip.ParsePrefix(sub.CIDR)
		address, aerr := netip.ParseAddr(sub.Address)
		if perr != nil || aerr != nil || !prefix.Addr().Is4() || !prefix.Contains(address) {
			continue
		}
		out = append(out, servedSubnet{DHCPSubnet: sub, prefix: prefix, address: address})
	}
	slices.SortFunc(out, func(a, b servedSubnet) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// dhcpLease is a lease's life, upstream's default.
const dhcpLease = "86400s"

// renderDNSMasq is dnsmasq's configuration for the network, upstream's options: a static range per
// subnet, so only a reserved address is leased; the gateway, when the subnet has one; option 121 with
// the metadata address through the DHCP port and the default route through the gateway; the subnet's
// DNS servers; and a reservation per port. dnsmasq answers no DNS (port=0), as before. A subnet that is
// not IPv4 (servedSubnets) with its reservations (servedHosts), and a gateway or DNS server that is not
// an IPv4 address (ipv4Address), are left out.
func renderDNSMasq(s DHCPState, leaseFile string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("# dnsmasq of network %s, Lightstack's", s.NetworkID)
	line("interface=%s", legName("tap", s.PortID))
	line("bind-interfaces")
	line("except-interface=lo")
	line("port=0")
	line("dhcp-leasefile=%s", leaseFile)
	served := servedSubnets(s.Subnets)
	if len(served) < len(s.Subnets) {
		log.Error().Str("network_id", s.NetworkID).Int("left_out", len(s.Subnets)-len(served)).
			Msg("subnets of the network that are not IPv4, or whose DHCP address is not in them, are left out of its DHCP")
	}
	for _, sub := range served {
		dnsmasqSubnet(line, sub)
	}
	for _, h := range servedHosts(s.Hosts, served) {
		line("dhcp-host=%s,%s", h.MAC, h.IP)
	}
	return b.String()
}

// servedHosts are the reservations on the served subnets, sorted by MAC and address: one whose address
// is on no served subnet has no range to be leased from (T247).
func servedHosts(hosts []DHCPHost, served []servedSubnet) []DHCPHost {
	var out []DHCPHost
	for _, h := range hosts {
		ip, err := netip.ParseAddr(h.IP)
		if err == nil && slices.ContainsFunc(served, func(sub servedSubnet) bool { return sub.prefix.Contains(ip) }) {
			out = append(out, DHCPHost{MAC: h.MAC, IP: ip.String()})
		}
	}
	slices.SortFunc(out, func(a, b DHCPHost) int {
		if c := strings.Compare(a.MAC, b.MAC); c != 0 {
			return c
		}
		return strings.Compare(a.IP, b.IP)
	})
	return out
}

// dnsmasqSubnet writes a subnet's range and options through line.
func dnsmasqSubnet(line func(format string, args ...any), sub servedSubnet) {
	tag := "tag:subnet-" + sub.ID
	line("dhcp-range=set:subnet-%s,%s,static,%s,%s", sub.ID, sub.prefix.Masked().Addr(), netmask(sub.prefix.Bits()), dhcpLease)
	routes := MetadataIP + "/32," + sub.address.String()
	if gateway, ok := ipv4Address(sub.Gateway, sub.ID, "gateway"); ok {
		line("dhcp-option=%s,option:router,%s", tag, gateway)
		routes += ",0.0.0.0/0," + gateway
	}
	line("dhcp-option=%s,option:classless-static-route,%s", tag, routes)
	var dns []string
	for _, server := range sub.DNS {
		if address, ok := ipv4Address(server, sub.ID, "DNS server"); ok {
			dns = append(dns, address)
		}
	}
	if len(dns) > 0 {
		line("dhcp-option=%s,option:dns-server,%s", tag, strings.Join(dns, ","))
	}
}

// ipv4Address is a subnet's gateway or DNS server as dnsmasq is to read it, and false for none. The API
// stores either only as an address (FR-060); anything else is left out of the configuration and logged,
// never written into it: dnsmasq starts as root, and a newline would start a directive.
func ipv4Address(value, subnetID, what string) (string, bool) {
	if value == "" {
		return "", false
	}
	if address, err := netip.ParseAddr(value); err == nil && address.Is4() {
		return address.String(), true
	}
	log.Error().Str("subnet_id", subnetID).Str("value", value).
		Msg("the subnet's " + what + " is not an IPv4 address; it is left out of the DHCP configuration")
	return "", false
}

// netmask is an IPv4 prefix length as a dotted mask; a length past 32 is held to 32, so no prefix can
// make it panic.
func netmask(bits int) string {
	mask := uint32(0xffffffff) << (32 - min(bits, 32)) // a shift of 32 or more leaves 0
	return fmt.Sprintf("%d.%d.%d.%d", mask>>24, mask>>16&0xff, mask>>8&0xff, mask&0xff)
}

// HeldNetworks lists the networks whose DHCP namespaces the node holds, by the ids that name them: a
// qdhcp- name with a lower-case UUID (heldNamespaces). Stub mode holds none.
func (m *DHCPManager) HeldNetworks(ctx context.Context) ([]string, error) {
	if m.mode == "stub" {
		return nil, nil
	}
	return heldNamespaces(ctx, m.run, "qdhcp-")
}

// ConvergeDHCP makes the node hold a network's DHCP as state says, or nothing of it when state is nil
// (R29, R34), within ctx's deadline and a second per command. It reads the node first and changes only
// what differs; dnsmasq is restarted only when its configuration changed or it is not running, and its
// lease file is kept. Stub mode runs nothing.
func (m *DHCPManager) ConvergeDHCP(ctx context.Context, networkID string, state *DHCPState) error {
	if m.mode == "stub" {
		return nil
	}
	ns := DHCPNamespace(networkID)
	held, err := readNamespace(ctx, m.run, ns)
	if err != nil {
		return fmt.Errorf("network %s's DHCP: %w", networkID, err)
	}
	files := dhcpFiles{conf: filepath.Join(m.configPath, networkID+".conf"), lease: filepath.Join(m.leasePath, networkID+".leases")}
	if state == nil {
		err = m.removeDHCP(ctx, networkID, ns, held, files)
	} else {
		err = m.applyDHCP(ctx, *state, held, files)
	}
	if err != nil {
		return fmt.Errorf("network %s's DHCP: %w", networkID, err)
	}
	return nil
}

type dhcpFiles struct{ conf, lease string }

func (m *DHCPManager) applyDHCP(ctx context.Context, s DHCPState, ns heldNamespace, files dhcpFiles) error {
	p := planDHCP(s)
	run := func(name string, args ...string) error {
		_, err := runCommand(ctx, m.run, "", name, args...)
		return err
	}
	for _, cmd := range ns.namespaceSteps(p.Namespace) {
		if err := run("ip", cmd...); err != nil {
			return err
		}
	}
	// A device of another DHCP port, the network's earlier one, goes; this port's is made, or finished
	// where a pass cut it short (SPEC-033 T191).
	for _, dev := range sortedKeys(ns.addresses) {
		if dev != p.Device {
			if err := run("ip", "link", "del", hostEnd(dev)); err != nil {
				return err
			}
		}
	}
	for _, cmd := range ns.legSteps(p.Namespace, p.HostEnd, p.Device, p.Bridge) {
		if err := run("ip", cmd...); err != nil {
			return err
		}
	}
	held := ns.addresses[p.Device]
	for _, a := range held {
		if !slices.Contains(p.Addresses, a) {
			if err := run("ip", "-n", p.Namespace, "addr", "del", a, "dev", p.Device); err != nil {
				return err
			}
		}
	}
	for _, a := range p.Addresses {
		if !slices.Contains(held, a) {
			if err := run("ip", "-n", p.Namespace, "addr", "replace", a, "dev", p.Device); err != nil {
				return err
			}
		}
	}
	return m.ensureDNSMasq(ctx, s.NetworkID, p.Namespace, renderDNSMasq(s, files.lease), files)
}

// ensureDNSMasq runs dnsmasq in the namespace with configuration rendered: left alone when it already
// runs with it; otherwise the configuration is written, the running one stopped, any left by an
// earlier process on this configuration stopped too, and a new one started.
func (m *DHCPManager) ensureDNSMasq(ctx context.Context, networkID, ns, rendered string, files dhcpFiles) error {
	current, err := os.ReadFile(files.conf)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read the dnsmasq configuration: %w", err)
	}
	m.mu.Lock()
	pid, tracked := m.runningPIDs[networkID]
	m.mu.Unlock()
	running := tracked && m.alive(pid)
	if string(current) == rendered && running {
		return nil
	}
	if string(current) != rendered {
		if err := os.WriteFile(files.conf, []byte(rendered), 0o644); err != nil {
			return fmt.Errorf("write the dnsmasq configuration: %w", err)
		}
	}
	if err := m.stopDNSMasq(ctx, networkID, files.conf); err != nil {
		return err
	}
	started, err := m.spawn([]string{"ip", "netns", "exec", ns, "dnsmasq", "--conf-file=" + files.conf, "--no-daemon", "--log-facility=-"})
	if err != nil {
		return fmt.Errorf("start dnsmasq: %w", err)
	}
	m.mu.Lock()
	m.runningPIDs[networkID] = started
	m.mu.Unlock()
	return nil
}

// stopDNSMasq stops the network's dnsmasq this process started, and any an earlier process left
// running on the same configuration.
func (m *DHCPManager) stopDNSMasq(ctx context.Context, networkID, conf string) error {
	m.mu.Lock()
	pid, tracked := m.runningPIDs[networkID]
	delete(m.runningPIDs, networkID)
	m.mu.Unlock()
	if tracked && m.alive(pid) {
		// One that exited meanwhile is stopped too.
		if _, err := runCommand(ctx, m.run, "", "kill", strconv.Itoa(pid)); err != nil && m.alive(pid) {
			return err
		}
	}
	// pkill answers 1 when nothing matched, which is the usual case.
	if _, err := runCommand(ctx, m.run, "", "pkill", "-f", conf); errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// removeDHCP removes the network's DHCP from the node: dnsmasq, the device, the namespace, then its
// files.
func (m *DHCPManager) removeDHCP(ctx context.Context, networkID, ns string, held heldNamespace, files dhcpFiles) error {
	if err := m.stopDNSMasq(ctx, networkID, files.conf); err != nil {
		return err
	}
	if held.exists {
		for _, dev := range sortedKeys(held.addresses) {
			if _, err := runCommand(ctx, m.run, "", "ip", "link", "del", hostEnd(dev)); err != nil {
				return err
			}
		}
		if _, err := runCommand(ctx, m.run, "", "ip", "netns", "delete", ns); err != nil {
			return err
		}
	}
	for _, f := range []string{files.conf, files.lease} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", f, err)
		}
	}
	return nil
}

// spawnProcess starts a long-running command and reaps it when it exits; its output goes to the
// process's own.
func spawnProcess(args []string) (int, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

// processAlive reports whether a process still runs.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}
