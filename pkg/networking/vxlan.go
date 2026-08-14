package networking

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
)

// VXLANManager manages VXLAN interfaces and forwarding database entries
type VXLANManager struct {
	mode       string // "stub" or real mode (iptables/ebpf)
	vxlanPort  int
	mu         sync.Mutex
	stubVXLANs map[string]bool              // For stub mode: networkID -> exists
	stubFDBs   map[string]map[string]string // For stub mode: networkID -> MAC -> remoteIP
}

// NewVXLANManager creates a new VXLAN manager
func NewVXLANManager(mode string, vxlanPort int) *VXLANManager {
	if vxlanPort == 0 {
		vxlanPort = 4789 // Default VXLAN port
	}

	return &VXLANManager{
		mode:       mode,
		vxlanPort:  vxlanPort,
		stubVXLANs: make(map[string]bool),
		stubFDBs:   make(map[string]map[string]string),
	}
}

// CreateVXLAN creates a VXLAN interface for a network
// networkID: Network UUID
// vni: VXLAN Network Identifier (1000-16777215)
// localIP: Local tunnel endpoint IP
func (m *VXLANManager) CreateVXLAN(networkID string, vni int, localIP string) error {
	if m.mode == "stub" {
		return m.createVXLANStub(networkID, vni)
	}
	return m.createVXLANReal(networkID, vni, localIP)
}

// createVXLANStub simulates VXLAN creation
func (m *VXLANManager) createVXLANStub(networkID string, vni int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stubVXLANs[networkID] = true
	m.stubFDBs[networkID] = make(map[string]string)
	return nil
}

// createVXLANReal creates an actual VXLAN interface
func (m *VXLANManager) createVXLANReal(networkID string, vni int, localIP string) error {
	vxlanName := m.GetVXLANName(networkID)

	// Check if VXLAN interface already exists
	if _, err := netlink.LinkByName(vxlanName); err == nil {
		return nil // Already exists
	}

	// Parse local IP
	localAddr := net.ParseIP(localIP)
	if localAddr == nil {
		return fmt.Errorf("invalid local IP: %s", localIP)
	}

	// Create VXLAN link using netlink
	vxlan := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name: vxlanName,
			MTU:  1450, // Standard MTU minus VXLAN overhead (50 bytes)
		},
		VxlanId:  vni,
		Port:     m.vxlanPort,
		Learning: true, // Enable MAC learning
		SrcAddr:  localAddr,
		L2miss:   true,  // Notify on L2 miss
		L3miss:   true,  // Notify on L3 miss
		NoAge:    false, // Allow FDB entry aging
		GBP:      false, // No Group-Based Policy
		Age:      300,   // FDB entry age timeout (seconds)
	}

	if err := netlink.LinkAdd(vxlan); err != nil {
		if !strings.Contains(err.Error(), "exists") {
			return fmt.Errorf("failed to create VXLAN interface %s: %w", vxlanName, err)
		}
	}

	// Bring the VXLAN interface up
	link, err := netlink.LinkByName(vxlanName)
	if err != nil {
		return fmt.Errorf("failed to find VXLAN interface %s: %w", vxlanName, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to bring up VXLAN interface %s: %w", vxlanName, err)
	}

	return nil
}

// DeleteVXLAN deletes a VXLAN interface
func (m *VXLANManager) DeleteVXLAN(networkID string) error {
	if m.mode == "stub" {
		return m.deleteVXLANStub(networkID)
	}
	return m.deleteVXLANReal(networkID)
}

// deleteVXLANStub simulates VXLAN deletion
func (m *VXLANManager) deleteVXLANStub(networkID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.stubVXLANs, networkID)
	delete(m.stubFDBs, networkID)
	return nil
}

// deleteVXLANReal deletes an actual VXLAN interface
func (m *VXLANManager) deleteVXLANReal(networkID string) error {
	vxlanName := m.GetVXLANName(networkID)

	link, err := netlink.LinkByName(vxlanName)
	if err != nil {
		return nil // Already deleted
	}

	return netlink.LinkDel(link)
}

// AddFDBEntry adds a forwarding database entry (MAC -> remote VTEP IP mapping)
// This tells the kernel where to send packets for a specific MAC address
func (m *VXLANManager) AddFDBEntry(networkID, macAddress, remoteIP string) error {
	if m.mode == "stub" {
		return m.addFDBEntryStub(networkID, macAddress, remoteIP)
	}
	return m.addFDBEntryReal(networkID, macAddress, remoteIP)
}

// addFDBEntryStub simulates FDB entry addition
func (m *VXLANManager) addFDBEntryStub(networkID, macAddress, remoteIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stubFDBs[networkID] == nil {
		m.stubFDBs[networkID] = make(map[string]string)
	}
	m.stubFDBs[networkID][macAddress] = remoteIP
	return nil
}

// addFDBEntryReal adds an actual FDB entry using netlink
func (m *VXLANManager) addFDBEntryReal(networkID, macAddress, remoteIP string) error {
	vxlanName := m.GetVXLANName(networkID)

	link, err := netlink.LinkByName(vxlanName)
	if err != nil {
		return fmt.Errorf("failed to find VXLAN interface %s: %w", vxlanName, err)
	}

	// Parse MAC address
	mac, err := net.ParseMAC(macAddress)
	if err != nil {
		return fmt.Errorf("invalid MAC address %s: %w", macAddress, err)
	}

	// Parse remote IP
	remoteAddr := net.ParseIP(remoteIP)
	if remoteAddr == nil {
		return fmt.Errorf("invalid remote IP: %s", remoteIP)
	}

	// Create FDB entry
	neigh := &netlink.Neigh{
		LinkIndex:    link.Attrs().Index,
		Family:       AF_BRIDGE,
		State:        NUD_PERMANENT,
		Flags:        NTF_SELF,
		IP:           remoteAddr,
		HardwareAddr: mac,
	}

	// Add or replace FDB entry
	if err := netlink.NeighSet(neigh); err != nil {
		return fmt.Errorf("failed to add FDB entry for %s: %w", macAddress, err)
	}

	return nil
}

// RemoveFDBEntry removes a forwarding database entry
func (m *VXLANManager) RemoveFDBEntry(networkID, macAddress string) error {
	if m.mode == "stub" {
		return m.removeFDBEntryStub(networkID, macAddress)
	}
	return m.removeFDBEntryReal(networkID, macAddress)
}

// removeFDBEntryStub simulates FDB entry removal
func (m *VXLANManager) removeFDBEntryStub(networkID, macAddress string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stubFDBs[networkID] != nil {
		delete(m.stubFDBs[networkID], macAddress)
	}
	return nil
}

// removeFDBEntryReal removes an actual FDB entry
func (m *VXLANManager) removeFDBEntryReal(networkID, macAddress string) error {
	vxlanName := m.GetVXLANName(networkID)

	link, err := netlink.LinkByName(vxlanName)
	if err != nil {
		return nil // Interface doesn't exist, entry already gone
	}

	mac, err := net.ParseMAC(macAddress)
	if err != nil {
		return fmt.Errorf("invalid MAC address %s: %w", macAddress, err)
	}

	neigh := &netlink.Neigh{
		LinkIndex:    link.Attrs().Index,
		Family:       AF_BRIDGE,
		HardwareAddr: mac,
		Flags:        NTF_SELF,
	}

	// Best effort deletion - ignore errors
	_ = netlink.NeighDel(neigh)
	return nil
}

// AttachToBridge attaches the VXLAN interface to a bridge, so VMs on that bridge can communicate over
// the tunnel.
//
// There is deliberately no namespace parameter. Every other method on this type — CreateVXLAN,
// DeleteVXLAN, AddFDBEntry, RemoveFDBEntry — resolves the interface with netlink.LinkByName in the
// namespace o3k runs in, so moving it elsewhere put it beyond its own manager's reach: FDB entries
// could not be installed at all, DeleteVXLAN reported success while leaking the device, and
// CreateVXLAN's existence check missed it and rebuilt it on the next pass (SPEC-024 /
// cloud-storage#546 F10).
func (m *VXLANManager) AttachToBridge(networkID, bridgeName string) error {
	if m.mode == "stub" {
		return nil // No-op in stub mode
	}
	return m.attachToBridgeReal(networkID, bridgeName)
}

// attachToBridgeReal attaches VXLAN interface to bridge
func (m *VXLANManager) attachToBridgeReal(networkID, bridgeName string) error {
	vxlanName := m.GetVXLANName(networkID)

	vxlanLink, err := netlink.LinkByName(vxlanName)
	if err != nil {
		return fmt.Errorf("failed to find VXLAN interface: %w", err)
	}

	bridgeLink, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to find bridge: %w", err)
	}

	// Checked assertion: an unchecked one panics when the name resolves to something that is not a
	// bridge, which is reachable now that both links are looked up in the shared root namespace.
	// BridgeManager.AttachToBridge already guards this the same way.
	br, ok := bridgeLink.(*netlink.Bridge)
	if !ok {
		return fmt.Errorf("link %s is not a bridge (got %T)", bridgeName, bridgeLink)
	}

	if err := netlink.LinkSetMaster(vxlanLink, br); err != nil {
		return fmt.Errorf("failed to attach VXLAN to bridge: %w", err)
	}

	return nil
}

// GetVXLANName returns the VXLAN interface name for a network
// Format: vxlan-{first 8 chars of network UUID}
func (m *VXLANManager) GetVXLANName(networkID string) string {
	if len(networkID) > 8 {
		return "vxlan-" + networkID[:8]
	}
	return "vxlan-" + networkID
}

// ListFDBEntries lists all FDB entries for a network (for debugging)
func (m *VXLANManager) ListFDBEntries(networkID string) (map[string]string, error) {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()

		entries := make(map[string]string)
		if m.stubFDBs[networkID] != nil {
			for mac, ip := range m.stubFDBs[networkID] {
				entries[mac] = ip
			}
		}
		return entries, nil
	}

	// In real mode, query kernel FDB table
	vxlanName := m.GetVXLANName(networkID)
	link, err := netlink.LinkByName(vxlanName)
	if err != nil {
		return nil, fmt.Errorf("failed to find VXLAN interface: %w", err)
	}

	neighbors, err := netlink.NeighList(link.Attrs().Index, AF_BRIDGE)
	if err != nil {
		return nil, fmt.Errorf("failed to list FDB entries: %w", err)
	}

	entries := make(map[string]string)
	for _, neigh := range neighbors {
		if neigh.HardwareAddr != nil && neigh.IP != nil {
			entries[neigh.HardwareAddr.String()] = neigh.IP.String()
		}
	}

	return entries, nil
}
