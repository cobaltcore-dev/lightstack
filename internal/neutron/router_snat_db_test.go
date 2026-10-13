package neutron

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// TestEgressSNATFollowsEnableSNAT pins snatOnPredicate on both backends: a router with a
// gateway masquerades its subnets unless enable_snat is false — absent means on, as
// upstream — and a router without a gateway masquerades nothing. The predicate compares
// JSON values, which PostgreSQL and SQLite render differently, so each case is a stored
// document as the API writes it.
func TestEgressSNATFollowsEnableSNAT(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := NewService(testPool, "stub", nil)
	extNet := seedNetwork(t, "spec028-snat-ext-"+uuid.NewString()[:8])

	cases := []struct {
		name    string
		gateway *string // external_gateway_info as stored; nil for none
		cidr    string
		snat    bool
	}{
		{"enable_snat absent", ptrTo(fmt.Sprintf(`{"network_id":%q}`, extNet)), "10.96.1.0/24", true},
		{"enable_snat true", ptrTo(fmt.Sprintf(`{"network_id":%q,"enable_snat":true}`, extNet)), "10.96.2.0/24", true},
		{"enable_snat false", ptrTo(fmt.Sprintf(`{"network_id":%q,"enable_snat":false}`, extNet)), "10.96.3.0/24", false},
		{"no gateway", nil, "10.96.4.0/24", false},
		{"gateway without a network", ptrTo(`{"enable_snat":true}`), "10.96.5.0/24", false},
	}
	for _, tc := range cases {
		netID := seedNetwork(t, "spec028-snat-"+uuid.NewString()[:8])
		gw := tc.cidr[:len(tc.cidr)-4] + "1"
		subnetID := seedSubnet(t, netID, "spec028-snat", tc.cidr, &gw, 4, false)
		routerID := uuid.NewString()
		mustExec(t, `INSERT INTO routers (id, name, project_id, external_gateway_info) VALUES ($1, 'spec028-snat', $2, $3::jsonb)`,
			routerID, seededProjectID, tc.gateway)
		cleanupRows(t, "routers", routerID)
		portID := seedRouterPort(t, routerID, netID, subnetID, gw)
		t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM ports WHERE id = $1", portID) })
	}

	cidrs, err := svc.egressSNATCIDRs(ctx)
	if err != nil {
		t.Fatalf("egressSNATCIDRs: %v", err)
	}
	for _, tc := range cases {
		if got := slices.Contains(cidrs, tc.cidr); got != tc.snat {
			t.Errorf("%s: %s masqueraded = %v; want %v", tc.name, tc.cidr, got, tc.snat)
		}
	}
}

func ptrTo(s string) *string { return &s }
