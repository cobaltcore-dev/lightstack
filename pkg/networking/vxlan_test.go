package networking

import (
	"fmt"
	"testing"
)

func TestNewVXLANManager(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		vxlanPort int
		wantPort  int
	}{
		{
			name:      "stub mode with default port",
			mode:      "stub",
			vxlanPort: 0,
			wantPort:  4789,
		},
		{
			name:      "stub mode with custom port",
			mode:      "stub",
			vxlanPort: 8472,
			wantPort:  8472,
		},
		{
			name:      "real mode with default port",
			mode:      "iptables",
			vxlanPort: 0,
			wantPort:  4789,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewVXLANManager(tt.mode, tt.vxlanPort)
			if mgr == nil {
				t.Fatal("NewVXLANManager returned nil")
			}
			if mgr.mode != tt.mode {
				t.Errorf("mode = %q, want %q", mgr.mode, tt.mode)
			}
			if mgr.vxlanPort != tt.wantPort {
				t.Errorf("vxlanPort = %d, want %d", mgr.vxlanPort, tt.wantPort)
			}
		})
	}
}

func TestVXLANManagerStubMode(t *testing.T) {
	mgr := NewVXLANManager("stub", 4789)

	networkID := "test-network-123"
	vni := 1000
	localIP := "192.168.1.10"

	// Test CreateVXLAN in stub mode
	err := mgr.CreateVXLAN(networkID, vni, localIP)
	if err != nil {
		t.Fatalf("CreateVXLAN failed: %v", err)
	}

	// Verify VXLAN was created
	mgr.mu.Lock()
	if !mgr.stubVXLANs[networkID] {
		t.Error("VXLAN was not created in stub mode")
	}
	if mgr.stubFDBs[networkID] == nil {
		t.Error("FDB map was not initialized")
	}
	mgr.mu.Unlock()

	// Test AddFDBEntry in stub mode
	macAddress := "52:54:00:12:34:56"
	remoteIP := "192.168.1.20"
	err = mgr.AddFDBEntry(networkID, macAddress, remoteIP)
	if err != nil {
		t.Fatalf("AddFDBEntry failed: %v", err)
	}

	// Verify FDB entry
	mgr.mu.Lock()
	if mgr.stubFDBs[networkID][macAddress] != remoteIP {
		t.Errorf("FDB entry = %q, want %q", mgr.stubFDBs[networkID][macAddress], remoteIP)
	}
	mgr.mu.Unlock()

	// Test ListFDBEntries
	entries, err := mgr.ListFDBEntries(networkID)
	if err != nil {
		t.Fatalf("ListFDBEntries failed: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("ListFDBEntries returned %d entries, want 1", len(entries))
	}
	if entries[macAddress] != remoteIP {
		t.Errorf("FDB entry = %q, want %q", entries[macAddress], remoteIP)
	}

	// Test RemoveFDBEntry
	err = mgr.RemoveFDBEntry(networkID, macAddress)
	if err != nil {
		t.Fatalf("RemoveFDBEntry failed: %v", err)
	}

	// Verify entry was removed
	entries, err = mgr.ListFDBEntries(networkID)
	if err != nil {
		t.Fatalf("ListFDBEntries failed: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ListFDBEntries returned %d entries after removal, want 0", len(entries))
	}

	// Test DeleteVXLANDevice
	err = mgr.DeleteVXLANDevice(mgr.GetVXLANName(networkID))
	if err != nil {
		t.Fatalf("DeleteVXLANDevice failed: %v", err)
	}

	// Verify VXLAN was deleted
	mgr.mu.Lock()
	if mgr.stubVXLANs[networkID] {
		t.Error("VXLAN still exists after deletion")
	}
	if mgr.stubFDBs[networkID] != nil {
		t.Error("FDB map still exists after deletion")
	}
	mgr.mu.Unlock()
}

func TestGetVXLANName(t *testing.T) {
	mgr := NewVXLANManager("stub", 4789)

	tests := []struct {
		name      string
		networkID string
		want      string
	}{
		{
			name:      "full UUID",
			networkID: "12345678-1234-1234-1234-123456789abc",
			want:      "vxlan-12345678",
		},
		{
			name:      "short ID",
			networkID: "abc",
			want:      "vxlan-abc",
		},
		{
			name:      "exactly 8 chars",
			networkID: "12345678",
			want:      "vxlan-12345678",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mgr.GetVXLANName(tt.networkID)
			if got != tt.want {
				t.Errorf("GetVXLANName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The namespaced variant this used to also exercise is gone: it moved the interface out of reach of
// every other VXLANManager method, so there is no longer a second placement to cover (SPEC-024 /
// cloud-storage#546 F10). Idempotence is asserted instead — the coordinator re-attaches every poll.
func TestAttachToBridgeStubMode(t *testing.T) {
	mgr := NewVXLANManager("stub", 4789)

	networkID := "test-network-123"
	bridgeName := "br-test"

	// Should be no-op in stub mode
	if err := mgr.AttachToBridge(networkID, bridgeName); err != nil {
		t.Errorf("AttachToBridge failed in stub mode: %v", err)
	}

	if err := mgr.AttachToBridge(networkID, bridgeName); err != nil {
		t.Errorf("AttachToBridge is not idempotent in stub mode: %v", err)
	}
}

func TestMultipleFDBEntries(t *testing.T) {
	mgr := NewVXLANManager("stub", 4789)

	networkID := "test-network-123"
	err := mgr.CreateVXLAN(networkID, 1000, "192.168.1.10")
	if err != nil {
		t.Fatalf("CreateVXLAN failed: %v", err)
	}

	// Add multiple FDB entries
	entries := map[string]string{
		"52:54:00:12:34:56": "192.168.1.20",
		"52:54:00:12:34:57": "192.168.1.21",
		"52:54:00:12:34:58": "192.168.1.22",
	}

	for mac, ip := range entries {
		err := mgr.AddFDBEntry(networkID, mac, ip)
		if err != nil {
			t.Fatalf("AddFDBEntry failed for %s: %v", mac, err)
		}
	}

	// List all entries
	listed, err := mgr.ListFDBEntries(networkID)
	if err != nil {
		t.Fatalf("ListFDBEntries failed: %v", err)
	}

	if len(listed) != len(entries) {
		t.Errorf("ListFDBEntries returned %d entries, want %d", len(listed), len(entries))
	}

	for mac, wantIP := range entries {
		gotIP, ok := listed[mac]
		if !ok {
			t.Errorf("MAC %s not found in listed entries", mac)
			continue
		}
		if gotIP != wantIP {
			t.Errorf("MAC %s: IP = %q, want %q", mac, gotIP, wantIP)
		}
	}

	// Remove one entry
	err = mgr.RemoveFDBEntry(networkID, "52:54:00:12:34:56")
	if err != nil {
		t.Fatalf("RemoveFDBEntry failed: %v", err)
	}

	// Verify only 2 entries remain
	listed, err = mgr.ListFDBEntries(networkID)
	if err != nil {
		t.Fatalf("ListFDBEntries failed: %v", err)
	}

	if len(listed) != 2 {
		t.Errorf("ListFDBEntries returned %d entries after removal, want 2", len(listed))
	}
}

func TestListFDBEntriesEmptyNetwork(t *testing.T) {
	mgr := NewVXLANManager("stub", 4789)

	// List entries for non-existent network
	entries, err := mgr.ListFDBEntries("nonexistent-network")
	if err != nil {
		t.Fatalf("ListFDBEntries failed: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("ListFDBEntries returned %d entries for nonexistent network, want 0", len(entries))
	}
}

func TestConcurrentFDBOperations(t *testing.T) {
	mgr := NewVXLANManager("stub", 4789)

	networkID := "test-network-123"
	err := mgr.CreateVXLAN(networkID, 1000, "192.168.1.10")
	if err != nil {
		t.Fatalf("CreateVXLAN failed: %v", err)
	}

	// Add entries concurrently
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(n int) {
			mac := fmt.Sprintf("52:54:00:12:34:%02x", n)
			ip := fmt.Sprintf("192.168.1.%d", 20+n)
			err := mgr.AddFDBEntry(networkID, mac, ip)
			if err != nil {
				t.Errorf("Concurrent AddFDBEntry failed: %v", err)
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify all entries were added
	entries, err := mgr.ListFDBEntries(networkID)
	if err != nil {
		t.Fatalf("ListFDBEntries failed: %v", err)
	}

	if len(entries) != 10 {
		t.Errorf("ListFDBEntries returned %d entries after concurrent adds, want 10", len(entries))
	}
}

// SPEC-033 round 2, T144: a deleted network's VXLAN device is found by its name, the only trace of the
// network left on the node — its VNI and forwarding rows go with its record — and removed by it.
func TestVXLANDevices(t *testing.T) {
	mgr := NewVXLANManager("stub", 0)
	a, b := "0a1b2c3d-0000-4000-8000-00000000000a", "1b2c3d4e-0000-4000-8000-00000000000b"
	for i, id := range []string{a, b} {
		if err := mgr.CreateVXLAN(id, 1001+i, "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgr.AddFDBEntry(a, "fa:16:3e:00:00:01", "192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.VXLANDevices()
	if err != nil || fmt.Sprint(got) != "[vxlan-0a1b2c3d vxlan-1b2c3d4e]" {
		t.Fatalf("devices = %q, %v", got, err)
	}
	if err := mgr.DeleteVXLANDevice("vxlan-0a1b2c3d"); err != nil {
		t.Fatal(err)
	}
	if got, _ := mgr.VXLANDevices(); fmt.Sprint(got) != "[vxlan-1b2c3d4e]" {
		t.Errorf("devices after the delete = %q", got)
	}
	if entries, _ := mgr.ListFDBEntries(a); len(entries) != 0 {
		t.Errorf("the deleted device's entries are still listed: %v", entries)
	}
	if err := mgr.DeleteVXLANDevice(mgr.GetVXLANName(b)); err != nil {
		t.Fatal(err)
	}
	if got, _ := mgr.VXLANDevices(); len(got) != 0 {
		t.Errorf("devices after deleting the network's = %q", got)
	}
}

func TestIsVXLANDevice(t *testing.T) {
	for name, want := range map[string]bool{
		"vxlan-0a1b2c3d": true,
		"vxlan-0A1B2C3D": false,
		"vxlan-0a1b2c3":  false,
		"vxlan-foo":      false,
		"vxlan0":         false,
		"flannel.1":      false,
	} {
		if got := isVXLANDevice(name); got != want {
			t.Errorf("isVXLANDevice(%q) = %v, want %v", name, got, want)
		}
	}
}
