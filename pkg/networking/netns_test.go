package networking

import (
	"testing"
)

// Test namespace manager in stub mode
func TestNamespaceManagerStubMode(t *testing.T) {
	mgr := NewNetworkNamespaceManager("stub")

	projectID := "test-project-123"

	// Create namespace
	if err := mgr.CreateNamespace(projectID); err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}

	// Verify namespace exists
	nsName := mgr.GetNamespaceName(projectID)
	if !mgr.NamespaceExists(nsName) {
		t.Error("Namespace should exist after creation")
	}

	// Delete namespace
	if err := mgr.DeleteNamespace(projectID); err != nil {
		t.Fatalf("Failed to delete namespace: %v", err)
	}

	// Verify namespace no longer exists
	if mgr.NamespaceExists(nsName) {
		t.Error("Namespace should not exist after deletion")
	}
}

// Test bridge manager in stub mode
func TestBridgeManagerStubMode(t *testing.T) {
	mgr := NewBridgeManager("stub")

	bridgeName := "br-test123"

	// Create bridge
	if err := mgr.CreateBridge(bridgeName); err != nil {
		t.Fatalf("Failed to create bridge: %v", err)
	}

	// Verify bridge exists
	mgr.mu.Lock()
	exists := mgr.stubBridges[bridgeName]
	mgr.mu.Unlock()

	if !exists {
		t.Error("Bridge should exist after creation")
	}

	// Delete bridge
	if err := mgr.DeleteBridge(bridgeName); err != nil {
		t.Fatalf("Failed to delete bridge: %v", err)
	}

	// Verify bridge no longer exists
	mgr.mu.Lock()
	exists = mgr.stubBridges[bridgeName]
	mgr.mu.Unlock()

	if exists {
		t.Error("Bridge should not exist after deletion")
	}
}

// Test TAP device manager in stub mode
func TestTAPManagerStubMode(t *testing.T) {
	mgr := NewTAPDeviceManager("stub")

	tapName := "tap-test123"

	// Create TAP device
	if err := mgr.CreateTAPDevice(tapName); err != nil {
		t.Fatalf("Failed to create TAP device: %v", err)
	}

	// Verify TAP device exists
	mgr.mu.Lock()
	exists := mgr.stubTAPs[tapName]
	mgr.mu.Unlock()

	if !exists {
		t.Error("TAP device should exist after creation")
	}

	// Delete TAP device
	if err := mgr.DeleteTAPDevice(tapName); err != nil {
		t.Fatalf("Failed to delete TAP device: %v", err)
	}

	// Verify TAP device no longer exists
	mgr.mu.Lock()
	exists = mgr.stubTAPs[tapName]
	mgr.mu.Unlock()

	if exists {
		t.Error("TAP device should not exist after deletion")
	}
}

// Test network lifecycle in stub mode
func TestNetworkLifecycleStub(t *testing.T) {
	nsMgr := NewNetworkNamespaceManager("stub")
	brMgr := NewBridgeManager("stub")
	tapMgr := NewTAPDeviceManager("stub")

	projectID := "project-123"
	networkID := "network-456"
	portID := "port-789"

	// 1. Create namespace for project
	if err := nsMgr.CreateNamespace(projectID); err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}

	// 2. Create bridge for network
	bridgeName := "br-" + networkID[:8]
	if err := brMgr.CreateBridge(bridgeName); err != nil {
		t.Fatalf("Failed to create bridge: %v", err)
	}

	// 3. Create TAP device for port
	tapName := "tap-" + portID[:8]
	if err := tapMgr.CreateTAPDevice(tapName); err != nil {
		t.Fatalf("Failed to create TAP device: %v", err)
	}

	// 4. Attach TAP to bridge
	if err := brMgr.AttachToBridge(tapName, bridgeName); err != nil {
		t.Fatalf("Failed to attach TAP to bridge: %v", err)
	}

	// 5. Cleanup - delete in reverse order
	if err := tapMgr.DeleteTAPDevice(tapName); err != nil {
		t.Fatalf("Failed to delete TAP device: %v", err)
	}

	if err := brMgr.DeleteBridge(bridgeName); err != nil {
		t.Fatalf("Failed to delete bridge: %v", err)
	}

	if err := nsMgr.DeleteNamespace(projectID); err != nil {
		t.Fatalf("Failed to delete namespace: %v", err)
	}

	// Verify all cleaned up
	nsName := nsMgr.GetNamespaceName(projectID)
	if nsMgr.NamespaceExists(nsName) {
		t.Error("Namespace should be deleted")
	}

	brMgr.mu.Lock()
	bridgeExists := brMgr.stubBridges[bridgeName]
	brMgr.mu.Unlock()
	if bridgeExists {
		t.Error("Bridge should be deleted")
	}

	tapMgr.mu.Lock()
	tapExists := tapMgr.stubTAPs[tapName]
	tapMgr.mu.Unlock()
	if tapExists {
		t.Error("TAP device should be deleted")
	}
}

// TestBridgeNameForNetworkFollowsTheConvention pins the naming rule itself.
//
// "br-" plus the FIRST 8 characters of the network id. Neutron creates the bridge under
// this name and compute names the same bridge when it re-creates a guest from stored
// state; if the two disagree by one character the guest attaches to a bridge nothing plugs
// into, and nothing reports an error — it simply has no network. The rule was hand-inlined
// at twelve production sites before it became this function.
func TestBridgeNameForNetworkFollowsTheConvention(t *testing.T) {
	for _, tc := range []struct{ networkID, want string }{
		{"abcdef01-2345-6789-abcd-ef0123456789", "br-abcdef01"},
		{"0123456789abcdef-full-uuid-shape", "br-01234567"},
		{"12345678", "br-12345678"}, // exactly 8: no truncation, no padding
		{"1234567", "br-1234567"},   // shorter than 8: used whole rather than panicking
		{"", "br-"},
	} {
		if got := BridgeNameForNetwork(tc.networkID); got != tc.want {
			t.Errorf("BridgeNameForNetwork(%q) = %q; want %q", tc.networkID, got, tc.want)
		}
	}
}

// TestBridgeNameIsRecognisedByThePruner pins the pair: every name the producer emits for a
// real network id must be one isManagedBridge accepts.
//
// The two halves of this convention are not equivalent at every input, and the prune gate
// is the dangerous side — it decides which bridges the reconciler may delete. A producer
// that emitted a name the pruner rejects leaves a stale bridge forever; one that emitted a
// name matching some FOREIGN bridge would have it deleted. Real ids are UUIDs, which is
// what makes the pairing hold; the short-id branch is documented as defensive only and is
// asserted here as NOT recognised, so the mismatch is a stated fact rather than a surprise.
func TestBridgeNameIsRecognisedByThePruner(t *testing.T) {
	for _, networkID := range []string{
		"abcdef01-2345-6789-abcd-ef0123456789",
		"00000000-0000-0000-0000-000000000001",
		"12345678",
	} {
		name := BridgeNameForNetwork(networkID)
		if !isManagedBridge(name) {
			t.Errorf("BridgeNameForNetwork(%q) = %q, which isManagedBridge rejects; such a bridge would never be pruned",
				networkID, name)
		}
	}

	// The documented edge: a network id shorter than 8 characters yields a name the pruner
	// does not recognise. Asserted so the divergence cannot change silently.
	if short := BridgeNameForNetwork("1234567"); isManagedBridge(short) {
		t.Errorf("isManagedBridge(%q) = true; the length rule is what stops the pruner touching foreign bridges, "+
			"so a 10-character name matching it would be a widening of what may be deleted", short)
	}
}
