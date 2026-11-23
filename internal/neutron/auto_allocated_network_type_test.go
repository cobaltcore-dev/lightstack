package neutron

import (
	"context"
	"net/http"
	"testing"
)

// SPEC-033 T273 (R40): the auto-allocated network is made as a network create makes one: VXLAN with an
// MTU of 1450 where the node coordinates VXLAN, flat with 1500 otherwise. It was always flat, which the VXLAN
// coordinator does not serve.
func TestAutoAllocatedNetworkTypeIsANetworkCreates(t *testing.T) {
	requireDB(t)
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	r, svc := newNeutronTestRouterWithService()
	svc.dhcp, svc.brManager = newFakeDHCP(), newFakeBridges()
	svc.vxlanCoordinator = &VXLANCoordinator{}
	cleanupAutoAllocated(t)
	network := postAutoAllocated(t, r, hdr, http.StatusCreated)
	var networkType string
	var mtu int
	if err := testPool.QueryRow(context.Background(), "SELECT network_type, mtu FROM networks WHERE id = $1", network).Scan(&networkType, &mtu); err != nil {
		t.Fatal(err)
	}
	if networkType != "vxlan" || mtu != 1450 {
		t.Errorf("auto-allocated network %s/%d; want vxlan/1450, as a network create makes one", networkType, mtu)
	}
}
