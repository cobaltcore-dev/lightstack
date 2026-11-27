package networking

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
)

// NetworkNamespaceManager removes the per-project light-ns-<project> Linux network namespaces older
// deployments made.
//
// Nothing in Lightstack creates these any more. Guest bridges, TAPs, the VXLAN devices and the ports'
// firewall all live in the namespace Lightstack itself runs in, because libvirt has to reach them there
// and because per-network layer-2 isolation comes from one bridge per network rather than from a
// namespace per project (SPEC-024 / cloud-storage#546 F10). Teardown is the only caller: it lists the
// namespaces an older deployment left behind and removes them. Do not reintroduce a way to create one,
// or to run guest networking inside one. Router and DHCP namespaces (qrouter-*, qdhcp-*) are a separate
// and still-live concern.
type NetworkNamespaceManager struct {
	nsPrefix string
	mode     string // "stub", "iptables", or "ebpf"
	run      commandRunner
}

// NewNetworkNamespaceManager creates the remover of the project namespaces. Stub mode holds none.
func NewNetworkNamespaceManager(mode string) *NetworkNamespaceManager {
	return &NetworkNamespaceManager{nsPrefix: "light-ns-", mode: mode, run: execRunner}
}

// HeldProjects lists the projects whose namespaces an older deployment left on the node, by the ids
// that name them: a light-ns- name with a lower-case UUID, recognised as a router's and a network's
// DHCP namespace are (heldNamespaces). Stub mode holds none.
func (m *NetworkNamespaceManager) HeldProjects(ctx context.Context) ([]string, error) {
	if m.mode == "stub" {
		return nil, nil
	}
	return heldNamespaces(ctx, m.run, m.nsPrefix)
}

// DeleteNamespace removes a project's namespace, within ctx's deadline and a second. A namespace that is
// gone already is removed; any other failure is reported, so teardown never counts a namespace removed
// that stays (SPEC-033 T279). Stub mode runs nothing.
func (m *NetworkNamespaceManager) DeleteNamespace(ctx context.Context, projectID string) error {
	if m.mode == "stub" {
		return nil
	}
	out, err := runCommand(ctx, m.run, "", "ip", "netns", "delete", m.nsPrefix+projectID)
	if err != nil && !strings.Contains(string(out), "No such file or directory") {
		return err
	}
	return nil
}

// BridgeManager manages Linux bridges
type BridgeManager struct {
	mode        string // "stub", "iptables", or "ebpf"
	mu          sync.Mutex
	stubBridges map[string]bool // For stub mode
}

// NewBridgeManager creates a new bridge manager
func NewBridgeManager(mode string) *BridgeManager {
	return &BridgeManager{
		mode:        mode,
		stubBridges: make(map[string]bool),
	}
}

// CreateBridge creates a bridge in the namespace Lightstack itself runs in.
//
// There is deliberately no namespace parameter. ListManagedBridges is netlink-only and therefore
// root-namespace-only, so a bridge built anywhere else was invisible to the reconciler's prune.
// libvirt, the guest TAPs, dnsmasq and the egress SNAT rules are all here too (SPEC-024 /
// cloud-storage#546 F10).
func (m *BridgeManager) CreateBridge(bridgeName string) error {
	if m.mode == "stub" {
		return m.createBridgeStub(bridgeName)
	}
	// Both iptables and eBPF use real bridges
	return m.createBridgeReal(bridgeName)
}

// createBridgeStub simulates bridge creation
func (m *BridgeManager) createBridgeStub(bridgeName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stubBridges[bridgeName] = true
	return nil
}

// createBridgeReal creates an actual bridge. Idempotent: an existing bridge is brought up rather than
// reported as an error, which the retired in-namespace variant could not do — it shelled out to
// `ip link add` and failed outright on a second call.
func (m *BridgeManager) createBridgeReal(bridgeName string) error {
	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: bridgeName,
		},
	}

	if err := netlink.LinkAdd(bridge); err != nil {
		if !strings.Contains(err.Error(), "exists") {
			return fmt.Errorf("failed to create bridge %s: %w", bridgeName, err)
		}
	}

	// Bring bridge up
	link, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to find bridge %s: %w", bridgeName, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up bridge %s: %w", bridgeName, err)
	}

	return nil
}

// DeleteBridge deletes a bridge. Namespace-free, for the reason CreateBridge gives.
func (m *BridgeManager) DeleteBridge(bridgeName string) error {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.stubBridges, bridgeName)
		return nil
	}

	link, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return nil // Already deleted
	}

	return netlink.LinkDel(link)
}

// BridgeNameForNetwork is the per-network Linux bridge name: "br-" plus the first 8
// characters of the network id.
//
// THE SINGLE DEFINITION of a convention two services have to agree on exactly. Neutron
// creates the bridge and attaches the DHCP and TAP devices to it; compute names the same
// bridge when it re-creates a guest from stored state, and a guest pointed at a name
// nothing plugs into attaches to no network at all, silently. The rule was hand-inlined at
// nine sites in the network service and once more in compute before this became a
// function.
//
// isManagedBridge below parses what this produces, and the two are NOT equivalent at every
// input: a network id shorter than 8 characters yields a name isManagedBridge rejects on
// length, so such a bridge would never be pruned. Network ids are UUIDs, so the short
// branch is defensive only — it exists so a truncated id cannot panic the slice.
func BridgeNameForNetwork(networkID string) string {
	if len(networkID) < 8 {
		return "br-" + networkID
	}
	return "br-" + networkID[:8]
}

// isManagedBridge reports whether name is a Lightstack per-network bridge: "br-" plus
// the first 8 hex chars of a network UUID (BridgeNameForNetwork builds it).
// The strict shape ensures the reconciler never prunes a foreign bridge (docker
// uses br-<12hex>, libvirt uses virbrN).
func isManagedBridge(name string) bool {
	suffix, ok := strings.CutPrefix(name, "br-")
	return ok && isLowerHex(suffix, 8)
}

// ListManagedBridges returns the names of all lightstack-managed bridges (br-<id8>)
// present in the default namespace, so the reconciler can prune the ones whose
// network was deleted (SPEC-009 WS-2).
func (m *BridgeManager) ListManagedBridges() ([]string, error) {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		var names []string
		for b := range m.stubBridges {
			if isManagedBridge(b) {
				names = append(names, b)
			}
		}
		return names, nil
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range links {
		if l.Type() == "bridge" && isManagedBridge(l.Attrs().Name) {
			names = append(names, l.Attrs().Name)
		}
	}
	return names, nil
}
