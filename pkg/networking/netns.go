package networking

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/vishvananda/netlink"
)

// NetworkNamespaceManager manages Linux network namespaces
type NetworkNamespaceManager struct {
	nsPrefix string
	mode     string // "stub", "iptables", or "ebpf"
	mu       sync.Mutex
	stubNS   map[string]bool // For stub mode
}

// NewNetworkNamespaceManager creates a new namespace manager
func NewNetworkNamespaceManager(mode string) *NetworkNamespaceManager {
	// Ensure /var/run/netns directory exists for real mode
	if mode != "stub" {
		if err := os.MkdirAll("/var/run/netns", 0755); err != nil {
			log.Warn().Err(err).Msg("failed to create /var/run/netns directory")
		}
	}

	return &NetworkNamespaceManager{
		nsPrefix: "light-ns-",
		mode:     mode,
		stubNS:   make(map[string]bool),
	}
}

// CreateNamespace creates a network namespace for a project
func (m *NetworkNamespaceManager) CreateNamespace(projectID string) error {
	if m.mode == "stub" {
		return m.createNamespaceStub(projectID)
	}
	// Both iptables and eBPF use real namespaces
	return m.createNamespaceReal(projectID)
}

// createNamespaceStub simulates namespace creation
func (m *NetworkNamespaceManager) createNamespaceStub(projectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	nsName := m.nsPrefix + projectID
	m.stubNS[nsName] = true
	return nil
}

// createNamespaceReal creates an actual namespace
func (m *NetworkNamespaceManager) createNamespaceReal(projectID string) error {
	nsName := m.nsPrefix + projectID

	// Check if namespace already exists
	if m.NamespaceExists(nsName) {
		return nil // Already exists
	}

	// Create namespace using ip netns add
	cmd := exec.Command("ip", "netns", "add", nsName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create namespace %s: %w", nsName, err)
	}

	return nil
}

// DeleteNamespace deletes a network namespace
func (m *NetworkNamespaceManager) DeleteNamespace(projectID string) error {
	if m.mode == "stub" {
		return m.deleteNamespaceStub(projectID)
	}
	// Both iptables and eBPF use real namespaces
	return m.deleteNamespaceReal(projectID)
}

// deleteNamespaceStub simulates namespace deletion
func (m *NetworkNamespaceManager) deleteNamespaceStub(projectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	nsName := m.nsPrefix + projectID
	delete(m.stubNS, nsName)
	return nil
}

// deleteNamespaceReal deletes an actual namespace
func (m *NetworkNamespaceManager) deleteNamespaceReal(projectID string) error {
	nsName := m.nsPrefix + projectID

	if !m.NamespaceExists(nsName) {
		return nil // Already deleted
	}

	cmd := exec.Command("ip", "netns", "delete", nsName)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to delete namespace %s: %w", nsName, err)
	}

	return nil
}

// NamespaceExists checks if a namespace exists
func (m *NetworkNamespaceManager) NamespaceExists(nsName string) bool {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.stubNS[nsName]
	}

	cmd := exec.Command("ip", "netns", "list")
	output, err := cmd.Output()
	if err != nil {
		log.Debug().Str("namespace", nsName).Err(err).Msg("NamespaceExists: failed to run 'ip netns list'")
		return false
	}

	log.Debug().Str("namespace", nsName).Str("output", string(output)).Msg("NamespaceExists: checking namespace list")
	namespaces := strings.Split(string(output), "\n")
	for _, ns := range namespaces {
		// `ip netns list` output may include metadata like "(id: 42)"; use only the name field
		fields := strings.Fields(ns)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == nsName {
			log.Debug().Str("namespace", nsName).Msg("NamespaceExists: found")
			return true
		}
	}

	log.Debug().Str("namespace", nsName).Msg("NamespaceExists: not found")
	return false
}

// GetNamespaceName returns the namespace name for a project
func (m *NetworkNamespaceManager) GetNamespaceName(projectID string) string {
	return m.nsPrefix + projectID
}

// EnsureNamespaceExists checks if namespace exists and creates it if not
func (m *NetworkNamespaceManager) EnsureNamespaceExists(projectID string) error {
	nsName := m.GetNamespaceName(projectID)

	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.stubNS[nsName] = true
		return nil
	}

	log.Debug().Str("project", projectID).Str("namespace", nsName).Msg("EnsureNamespaceExists called")

	// Check if namespace exists
	if m.NamespaceExists(nsName) {
		log.Debug().Str("namespace", nsName).Msg("namespace already exists")
		return nil
	}

	log.Debug().Str("namespace", nsName).Msg("namespace does not exist, creating")
	// Create namespace if it doesn't exist
	err := m.CreateNamespace(projectID)
	if err != nil {
		log.Error().Err(err).Str("namespace", nsName).Msg("failed to create namespace")
		return err
	}
	log.Debug().Str("namespace", nsName).Msg("namespace created successfully")
	return nil
}

// ExecInNamespace executes a command in a namespace
func (m *NetworkNamespaceManager) ExecInNamespace(projectID string, args ...string) error {
	if m.mode == "stub" {
		// In stub mode, just check if namespace exists
		nsName := m.GetNamespaceName(projectID)
		if !m.NamespaceExists(nsName) {
			return fmt.Errorf("namespace %s does not exist", nsName)
		}
		return nil
	}

	nsName := m.GetNamespaceName(projectID)
	fullArgs := append([]string{"netns", "exec", nsName}, args...)
	cmd := exec.Command("ip", fullArgs...)
	return cmd.Run()
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

// CreateBridge creates a bridge in the default or specified namespace
func (m *BridgeManager) CreateBridge(bridgeName string, inNamespace bool, nsName string) error {
	if m.mode == "stub" {
		return m.createBridgeStub(bridgeName)
	}
	// Both iptables and eBPF use real bridges
	return m.createBridgeReal(bridgeName, inNamespace, nsName)
}

// createBridgeStub simulates bridge creation
func (m *BridgeManager) createBridgeStub(bridgeName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stubBridges[bridgeName] = true
	return nil
}

// createBridgeReal creates an actual bridge
func (m *BridgeManager) createBridgeReal(bridgeName string, inNamespace bool, nsName string) error {
	if inNamespace {
		// Create bridge in namespace using ip command
		cmd := exec.Command("ip", "netns", "exec", nsName, "ip", "link", "add", bridgeName, "type", "bridge")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to create bridge %s in namespace %s: %w", bridgeName, nsName, err)
		}

		// Bring bridge up
		cmd = exec.Command("ip", "netns", "exec", nsName, "ip", "link", "set", bridgeName, "up")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to bring up bridge %s: %w", bridgeName, err)
		}
	} else {
		// Create bridge in default namespace using netlink
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
	}

	return nil
}

// EnsureBridgeIP assigns addrCIDR (e.g. "10.0.0.1/24") to the bridge in the
// default namespace; idempotent (an existing address is fine). dnsmasq with
// bind-interfaces needs the bridge to carry an address in the subnet it serves
// (SPEC-009 WS-2). The bridge also acts as the tenant subnet's L3 gateway.
func (m *BridgeManager) EnsureBridgeIP(bridgeName, addrCIDR string) error {
	if m.mode == "stub" {
		return nil
	}
	link, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("bridge %s not found: %w", bridgeName, err)
	}
	addr, err := netlink.ParseAddr(addrCIDR)
	if err != nil {
		return fmt.Errorf("parse bridge addr %s: %w", addrCIDR, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil && !strings.Contains(err.Error(), "exists") {
		return fmt.Errorf("assign %s to bridge %s: %w", addrCIDR, bridgeName, err)
	}
	return nil
}

// DeleteBridge deletes a bridge
func (m *BridgeManager) DeleteBridge(bridgeName string, inNamespace bool, nsName string) error {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.stubBridges, bridgeName)
		return nil
	}

	if inNamespace {
		cmd := exec.Command("ip", "netns", "exec", nsName, "ip", "link", "delete", bridgeName)
		return cmd.Run()
	}

	link, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return nil // Already deleted
	}

	return netlink.LinkDel(link)
}

// isManagedBridge reports whether name is an o3k per-network bridge: "br-" plus
// the first 8 hex chars of a network UUID (neutron builds "br-"+networkID[:8]).
// The strict shape ensures the reconciler never prunes a foreign bridge (docker
// uses br-<12hex>, libvirt uses virbrN).
func isManagedBridge(name string) bool {
	if !strings.HasPrefix(name, "br-") || len(name) != 11 {
		return false
	}
	for _, c := range name[3:] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ListManagedBridges returns the names of all o3k-managed bridges (br-<id8>)
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

// AttachToBridge attaches an interface to a bridge
func (m *BridgeManager) AttachToBridge(ifName, bridgeName string, inNamespace bool, nsName string) error {
	if m.mode == "stub" {
		// Deliberately does NOT gate on stubBridges. That map is process-local bookkeeping —
		// written by createBridgeStub, never persisted, never rebuilt — so gating on it made port
		// creation fail for every network created before the current process, with a 500 reading
		// "failed to attach TAP to bridge" (SPEC-024 / cloud-storage#546 F10). The recorded network
		// is the authority for what exists; in stub mode there is no kernel object to attach to and
		// nothing to verify (FR-002).
		return nil
	}

	if inNamespace {
		cmd := exec.Command("ip", "netns", "exec", nsName, "ip", "link", "set", ifName, "master", bridgeName)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to attach %s to bridge %s in namespace %s: %w, output: %s", ifName, bridgeName, nsName, err, string(output))
		}
		return nil
	}

	iface, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to find interface %s: %w", ifName, err)
	}

	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to find bridge %s: %w", bridgeName, err)
	}

	br, ok := bridge.(*netlink.Bridge)
	if !ok {
		return fmt.Errorf("link %s is not a bridge (got %T)", bridgeName, bridge)
	}

	return netlink.LinkSetMaster(iface, br)
}

// TAPDeviceManager manages TAP devices
type TAPDeviceManager struct {
	mode     string // "stub", "iptables", or "ebpf"
	mu       sync.Mutex
	stubTAPs map[string]bool // For stub mode
}

// NewTAPDeviceManager creates a new TAP device manager
func NewTAPDeviceManager(mode string) *TAPDeviceManager {
	return &TAPDeviceManager{
		mode:     mode,
		stubTAPs: make(map[string]bool),
	}
}

// CreateTAPDevice creates a TAP device in the namespace o3k itself runs in, which is where the
// bridge, dnsmasq and the egress SNAT rules also live.
//
// There is deliberately no namespace parameter. The in-namespace variant served only the retired
// light-ns-<project> placement, and its last caller went away with the F10 fix (SPEC-024 /
// cloud-storage#546): guest TAPs must sit beside their bridge, and per-network layer-2 isolation
// comes from one bridge per network rather than from a namespace per project. Taking the parameter
// while ignoring it invited the inverse defect — a caller naming a namespace and being silently
// placed elsewhere.
func (m *TAPDeviceManager) CreateTAPDevice(tapName string) error {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.stubTAPs[tapName] = true
		return nil
	}

	// Both iptables and eBPF use real TAP devices
	cmd := exec.Command("ip", "tuntap", "add", tapName, "mode", "tap")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create TAP device %s: %w", tapName, err)
	}

	// Bring TAP device up
	cmd = exec.Command("ip", "link", "set", tapName, "up")
	return cmd.Run()
}

// DeleteTAPDevice deletes a TAP device. Namespace-free, for the reason CreateTAPDevice gives.
func (m *TAPDeviceManager) DeleteTAPDevice(tapName string) error {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.stubTAPs, tapName)
		return nil
	}

	cmd := exec.Command("ip", "link", "delete", tapName)
	return cmd.Run()
}
