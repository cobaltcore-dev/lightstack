package networking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// RetiredRecords are the records that make retired state Lightstack's where nothing else marks it
// (SPEC-033 research R24's rule): every floating IP's address, associated or not, and every subnet's
// CIDR. Only the operator's teardown gives them (lightstack cleanup): a tenant chooses a subnet's CIDR,
// and an operator's own masquerade or DNAT has the earlier layout's shape, so reconciliation gives none
// and touches only what a mark or a name makes Lightstack's (SPEC-033 T190). A partial set only
// leaves more in place.
type RetiredRecords struct {
	FloatingIPs []string
	SubnetCIDRs []string
}

// RetiredState removes from the node what earlier layouts of Lightstack's real mode left on it and this
// one never writes (SPEC-033 FR-042, FR-044; research R29, R33, R40), as far as it can tell the state is
// Lightstack's: by a mark, by a name only Lightstack gives, or — given records, by the teardown only — by
// a record. It never touches state that is unmarked and that no record names.
//
//   - dnsmasq started on the host on a configuration of Lightstack's: every network's dnsmasq now runs
//     in its DHCP namespace;
//   - floating addresses on the uplink, /32 and of a floating IP with a record, and any address on a
//     network's bridge: no bridge holds an address now;
//   - in the host's nat table: floating-IP DNAT marked lightstack-fip, an unmarked one of the same shape
//     for an address with a record, the per-bridge metadata DNAT, and the egress MASQUERADE of a subnet
//     with a record out the uplink: all NAT lives in the routers' namespaces now;
//   - in the host's filter table, the two FORWARD accepts that masquerade came with, for the same subnet
//     and uplink: the routers' forward chain carries their traffic now;
//   - the LS-NSI-* floating-IP filters and the LS-SG-* group chains, with every jump to them: the ports'
//     chains enforce both now;
//   - Neutron's own TAPs, tap- and eight characters of a port id: the guest's TAP is libvirt's now.
type RetiredState struct {
	mode string
	// run runs the node's commands; tests replace it.
	run commandRunner
	// proc is the proc file system the host's dnsmasq processes are found in; tests replace it.
	proc string
	// dhcpConfigDir holds the dnsmasq configuration of every layout.
	dhcpConfigDir string
}

// NewRetiredState creates the remover of the earlier layouts' state. Stub mode removes nothing.
func NewRetiredState(mode string) *RetiredState {
	return &RetiredState{mode: mode, run: execRunner, proc: "/proc", dhcpConfigDir: filepath.Join(paths.DHCPDir(), "configs")}
}

// Remove removes the retired state the node holds, every command within ctx's deadline and a second. Each
// kind is independent: a failure is reported and the others still go. The host dnsmasq go first and the
// addresses before the NAT that used them, so nothing serves or forwards while half removed; uplink ""
// leaves alone what only the uplink makes recognisable.
func (r *RetiredState) Remove(ctx context.Context, uplink string, records RetiredRecords) error {
	if r.mode == "stub" {
		return nil
	}
	errs := []error{r.stopHostDNSMasq(ctx)}
	devices, err := readDevices(ctx, r.run)
	if err != nil {
		errs = append(errs, err)
	} else {
		errs = append(errs, r.removeAddresses(ctx, devices, uplink, records.FloatingIPs))
	}
	errs = append(errs, r.removeNAT(ctx, uplink, records), r.removeChains(ctx, uplink, records))
	if err == nil {
		errs = append(errs, r.removeTAPs(ctx, devices))
	}
	return errors.Join(errs...)
}

// stopHostDNSMasq stops every dnsmasq that runs in this process's network namespace on a configuration
// in Lightstack's DHCP directory. One in a network's DHCP namespace runs in that namespace, and is left.
func (r *RetiredState) stopHostDNSMasq(ctx context.Context) error {
	own, err := os.Readlink(filepath.Join(r.proc, "self", "ns", "net"))
	if err != nil {
		return fmt.Errorf("read this process's network namespace: %w", err)
	}
	entries, err := os.ReadDir(r.proc)
	if err != nil {
		return fmt.Errorf("list the processes: %w", err)
	}
	var pids []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	var errs []error
	for _, pid := range pids {
		dir := filepath.Join(r.proc, strconv.Itoa(pid))
		if !r.hostDNSMasq(dir, own) {
			continue
		}
		if _, err := runCommand(ctx, r.run, "", "kill", strconv.Itoa(pid)); err != nil {
			errs = append(errs, fmt.Errorf("stop the host dnsmasq %d: %w", pid, err))
		}
	}
	return errors.Join(errs...)
}

// hostDNSMasq reports whether the process in dir is dnsmasq on a configuration of Lightstack's, in the
// network namespace own. A process that exited meanwhile, or whose command line reads empty, is not.
func (r *RetiredState) hostDNSMasq(dir, own string) bool {
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil || len(cmdline) == 0 {
		return false
	}
	argv := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if filepath.Base(argv[0]) != "dnsmasq" {
		return false
	}
	conf := "--conf-file=" + r.dhcpConfigDir + string(filepath.Separator)
	if !slices.ContainsFunc(argv[1:], func(a string) bool { return strings.HasPrefix(a, conf) }) {
		return false
	}
	netns, err := os.Readlink(filepath.Join(dir, "ns", "net"))
	return err == nil && netns == own
}

// device is one of the node's devices as `ip -j addr show` prints it.
type device struct {
	Name  string `json:"ifname"`
	Addrs []struct {
		Family string `json:"family"`
		Local  string `json:"local"`
		Prefix int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// readDevices reads the node's devices with their addresses.
func readDevices(ctx context.Context, run commandRunner) ([]device, error) {
	out, err := runCommand(ctx, run, "", "ip", "-j", "addr", "show")
	if err != nil {
		return nil, fmt.Errorf("read the devices: %w", err)
	}
	var devices []device
	if err := json.Unmarshal(out, &devices); err != nil {
		return nil, fmt.Errorf("read the devices: %w", err)
	}
	return devices, nil
}

// removeAddresses removes the uplink's /32 IPv4 addresses that are floating IPs' and every address on a
// network's bridge but the IPv6 link-local one the kernel gives it.
func (r *RetiredState) removeAddresses(ctx context.Context, devices []device, uplink string, floating []string) error {
	var errs []error
	for _, d := range devices {
		for _, a := range d.Addrs {
			ip, err := netip.ParseAddr(a.Local)
			if err != nil {
				continue
			}
			retired := false
			switch {
			case uplink != "" && d.Name == uplink:
				retired = ip.Is4() && a.Prefix == 32 && slices.Contains(floating, ip.String())
			case isManagedBridge(d.Name):
				retired = !ip.Is6() || !ip.IsLinkLocalUnicast()
			}
			if !retired {
				continue
			}
			addr := ip.String() + "/" + strconv.Itoa(a.Prefix)
			if _, err := runCommand(ctx, r.run, "", "ip", "addr", "del", addr, "dev", d.Name); err != nil {
				errs = append(errs, fmt.Errorf("remove %s from %s: %w", addr, d.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// removeNAT removes the host's retired NAT rules in one commit.
func (r *RetiredState) removeNAT(ctx context.Context, uplink string, records RetiredRecords) error {
	out, err := runCommand(ctx, r.run, "", "iptables-save", "-t", "nat")
	if err != nil {
		return fmt.Errorf("read the nat table: %w", err)
	}
	subnets := recordedSubnets(records)
	var deletes []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-A ") {
			continue
		}
		if retiredFloatingNAT(line, records.FloatingIPs) || retiredEgressSNAT(line, uplink, subnets) {
			deletes = append(deletes, "-D "+strings.TrimPrefix(line, "-A "))
		} else if isMetadataRule(line) {
			deletes = append(deletes, "-D "+strings.TrimPrefix(line, "-A "))
		}
	}
	if len(deletes) == 0 {
		return nil
	}
	payload := "*nat\n" + strings.Join(deletes, "\n") + "\nCOMMIT\n"
	if _, err := runCommand(ctx, r.run, payload, "iptables-restore", "--noflush", "--wait", "1"); err != nil {
		return fmt.Errorf("remove the retired rules of the nat table: %w", err)
	}
	return nil
}

// floatingIPMark marked every floating-IP NAT rule the host carried in the earlier layout.
const floatingIPMark = "lightstack-fip"

// retiredFloatingNAT reports whether a nat rule is an earlier layout's floating-IP DNAT: `-d F/32
// [-m comment --comment lightstack-fip] -j DNAT --to-destination X` in PREROUTING or OUTPUT, nothing
// else in it — marked, or unmarked with a record of F. A port forwarding's rule is not of the shape.
func retiredFloatingNAT(line string, floating []string) bool {
	f := strings.Fields(line)
	if len(f) < 8 || (f[1] != "PREROUTING" && f[1] != "OUTPUT") || f[2] != "-d" {
		return false
	}
	address, ok := strings.CutSuffix(f[3], "/32")
	if !ok {
		return false
	}
	if ip, err := netip.ParseAddr(address); err != nil || !ip.Is4() {
		return false
	}
	switch {
	case len(f) == 8 && f[4] == "-j" && f[5] == "DNAT" && f[6] == "--to-destination":
		return slices.Contains(floating, address)
	case len(f) == 12 && f[4] == "-m" && f[5] == "comment" && f[6] == "--comment" && f[7] == floatingIPMark &&
		f[8] == "-j" && f[9] == "DNAT" && f[10] == "--to-destination":
		return true
	}
	return false
}

// recordedSubnets are the records' subnet CIDRs, masked, as a set.
func recordedSubnets(records RetiredRecords) map[string]bool {
	subnets := map[string]bool{}
	for _, cidr := range records.SubnetCIDRs {
		if p, err := netip.ParsePrefix(cidr); err == nil {
			subnets[p.Masked().String()] = true
		}
	}
	return subnets
}

// recordedSubnet reports whether a rule's prefix is a subnet with a record.
func recordedSubnet(prefix string, subnets map[string]bool) bool {
	p, err := netip.ParsePrefix(prefix)
	return err == nil && subnets[p.Masked().String()]
}

// retiredEgressSNAT reports whether a nat rule is an earlier layout's egress SNAT: `-A POSTROUTING -s
// <subnet> -o <uplink> -j MASQUERADE`, nothing else in it, for a subnet with a record.
func retiredEgressSNAT(line, uplink string, subnets map[string]bool) bool {
	f := strings.Fields(line)
	if uplink == "" || len(f) != 8 || f[1] != "POSTROUTING" || f[2] != "-s" || f[4] != "-o" || f[5] != uplink ||
		f[6] != "-j" || f[7] != "MASQUERADE" {
		return false
	}
	return recordedSubnet(f[3], subnets)
}

// retiredForwardAccept reports whether a filter rule is one of the two FORWARD accepts the earlier
// layout's egress SNAT wrote beside its masquerade, nothing else in it, for a subnet with a record:
// `-A FORWARD -s <subnet> -o <uplink> -j ACCEPT` and `-A FORWARD -d <subnet> -i <uplink> -m conntrack
// --ctstate RELATED,ESTABLISHED -j ACCEPT` (SPEC-033 T278).
func retiredForwardAccept(line, uplink string, subnets map[string]bool) bool {
	f := strings.Fields(line)
	if uplink == "" || len(f) < 8 || f[1] != "FORWARD" {
		return false
	}
	out := len(f) == 8 && f[2] == "-s" && f[4] == "-o" && f[5] == uplink && f[6] == "-j" && f[7] == "ACCEPT"
	back := len(f) == 12 && f[2] == "-d" && f[4] == "-i" && f[5] == uplink &&
		strings.Join(f[6:], " ") == "-m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT"
	return (out || back) && recordedSubnet(f[3], subnets)
}

// isRetiredChain reports whether a filter chain is an earlier layout's: a floating IP's filter or a
// group's chain.
func isRetiredChain(name string) bool {
	return strings.HasPrefix(name, "LS-NSI-") || strings.HasPrefix(name, "LS-SG-")
}

// removeChains removes the retired filter chains, with every jump to them, and the retired FORWARD
// accepts, in one commit.
func (r *RetiredState) removeChains(ctx context.Context, uplink string, records RetiredRecords) error {
	out, err := runCommand(ctx, r.run, "", "iptables-save", "-t", "filter")
	if err != nil {
		return fmt.Errorf("read the filter table: %w", err)
	}
	subnets := recordedSubnets(records)
	accept := func(rule string) bool { return retiredForwardAccept(rule, uplink, subnets) }
	payload := chainRemovalPayload("filter", parseSavedTable(string(out)), isRetiredChain, accept)
	if payload == "" {
		return nil
	}
	if _, err := runCommand(ctx, r.run, payload, "iptables-restore", "--noflush", "--wait", "1"); err != nil {
		return fmt.Errorf("remove the retired chains: %w", err)
	}
	return nil
}

// chainRemovalPayload is the restore input that removes from table every chain remove names, and every
// rule of another chain that also names (nil for none): each rule of another chain that jumps or goes to
// one deleted, or that also names, then every chain emptied, then every one deleted, so a chain that
// jumps to another is empty by the time that one goes. "" when the table has nothing to remove.
func chainRemovalPayload(table string, cur savedTable, remove, also func(string) bool) string {
	var chains, deletes []string
	for _, name := range cur.chains {
		if remove(name) {
			chains = append(chains, name)
			continue
		}
		for _, rule := range cur.rules[name] {
			f := strings.Fields(rule)
			jumps := len(f) >= 2 && (f[len(f)-2] == "-j" || f[len(f)-2] == "-g") && remove(f[len(f)-1])
			if jumps || (also != nil && also(rule)) {
				deletes = append(deletes, "-D "+strings.TrimPrefix(rule, "-A "))
			}
		}
	}
	if len(chains)+len(deletes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("*" + table + "\n")
	for _, d := range deletes {
		b.WriteString(d + "\n")
	}
	for _, name := range chains {
		b.WriteString("-F " + name + "\n")
	}
	for _, name := range chains {
		b.WriteString("-X " + name + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// removeTAPs removes the TAPs Neutron itself made for its ports: tap- and the port id's first eight
// characters. libvirt's, tap and the port id's first eleven, have no dash straight after tap and are
// left.
func (r *RetiredState) removeTAPs(ctx context.Context, devices []device) error {
	var errs []error
	for _, d := range devices {
		if !isNeutronTAP(d.Name) {
			continue
		}
		if _, err := runCommand(ctx, r.run, "", "ip", "link", "del", d.Name); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", d.Name, err))
		}
	}
	return errors.Join(errs...)
}

// isNeutronTAP reports whether a device is a TAP Neutron made in an earlier layout: tap- and eight
// lower-case hexadecimal characters.
func isNeutronTAP(name string) bool {
	suffix, ok := strings.CutPrefix(name, "tap-")
	return ok && isLowerHex(suffix, 8)
}

// isLowerHex reports whether s is n lower-case hexadecimal characters.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
