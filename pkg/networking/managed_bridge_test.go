package networking

import "testing"

// TestIsManagedBridge locks down the strict "br-<8 lowercase hex>" match the
// reconciler's stale-bridge prune relies on — it must accept Lightstack bridges and
// reject everything else (docker br-<12hex>, libvirt virbrN, etc.) so the prune
// can never delete a foreign bridge (SPEC-009 WS-2).
func TestIsManagedBridge(t *testing.T) {
	managed := []string{"br-12345678", "br-deadbeef", "br-00000000", "br-e286c335"}
	foreign := []string{
		"br-1234567",      // 7 hex — too short
		"br-123456789",    // 9 hex — too long
		"br-1234567g",     // non-hex char
		"br-ABCDEF12",     // uppercase — UUIDs are lowercase
		"br-0123456789ab", // 12 hex — docker
		"virbr0",          // libvirt
		"docker0",
		"br-",
		"eth0",
		"",
	}
	for _, n := range managed {
		if !isManagedBridge(n) {
			t.Errorf("isManagedBridge(%q) = false; want true (Lightstack bridge)", n)
		}
	}
	for _, n := range foreign {
		if isManagedBridge(n) {
			t.Errorf("isManagedBridge(%q) = true; want false (must not be pruned)", n)
		}
	}
}
