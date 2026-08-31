package neutron

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// seedAttachNetwork inserts a network with the given owner and flags, plus a subnet, so an
// allocation that is allowed actually succeeds and the test reads the rule rather than a missing
// address.
func seedAttachNetwork(t *testing.T, name, owner string, shared, external bool) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.New().String()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, external, network_type, mtu, created_at, updated_at)
		VALUES ($1, $2, $3, true, 'ACTIVE', $4, $5, 'flat', 1500, now(), now())
	`, id, name, owner, shared, external); err != nil {
		t.Fatalf("seed network %s: %v", name, err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
		VALUES ($1, $2, $3, $4, '10.247.0.0/24', '10.247.0.1', 4, false, now(), now())
	`, uuid.New().String(), name+"-subnet", id, owner); err != nil {
		t.Fatalf("seed subnet for %s: %v", name, err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM ports WHERE network_id = $1", id)   //nolint:errcheck // best-effort cleanup
		testPool.Exec(ctx, "DELETE FROM subnets WHERE network_id = $1", id) //nolint:errcheck // best-effort cleanup
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", id)        //nolint:errcheck // best-effort cleanup
	})
	return id
}

// TestGuestAttachRuleMatchesUpstreamAndBothPathsAgree pins which networks a guest may be attached to.
//
// The boot pre-flight (NetworkUsableBy) used the network READ rule — owner, shared or external —
// while the allocation it vouches for (AllocatePortForInstance) attached on owner-or-shared. A
// foreign external network therefore passed the pre-flight, the create answered 202, and the server
// then failed with "network not found" for a network the tenant can list.
//
// Upstream Nova refuses an interface on an unshared external network to anyone but an administrator,
// whoever owns the network, and allows a shared one to everybody: `network:attach_external_network`
// defaults to admin, and _check_external_network_attach (nova/network/neutron.py) raises
// ExternalNetworkAttachForbidden on `router:external and not shared`.
//
// Both functions run over the same matrix, because the defect was the two disagreeing (FR-016a).
func TestGuestAttachRuleMatchesUpstreamAndBothPathsAgree(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	ctx := context.Background()
	svc := NewService(testPool, "stub", nil)

	type outcome string
	const (
		attach    outcome = "attach"
		forbidden outcome = "forbidden" // upstream's ExternalNetworkAttachForbidden
		notFound  outcome = "not found" // indistinguishable from an absent network
	)
	cases := []struct {
		name             string
		owner            string // "" seeds nothing: an id that names no network
		shared, external bool
		member, admin    outcome
	}{
		{"own private", seededProjectID, false, false, attach, attach},
		{"foreign shared", otherProjectID, true, false, attach, attach},
		{"foreign shared external", otherProjectID, true, true, attach, attach},
		{"foreign external", otherProjectID, false, true, forbidden, attach},
		{"own external", seededProjectID, false, true, forbidden, attach},
		{"foreign private", otherProjectID, false, false, notFound, notFound},
		{"absent", "", false, false, notFound, notFound},
	}

	for _, tc := range cases {
		netID := uuid.New().String()
		if tc.owner != "" {
			netID = seedAttachNetwork(t, "spec025-attach-"+tc.name, tc.owner, tc.shared, tc.external)
		}
		for _, admin := range []bool{false, true} {
			want := tc.member
			if admin {
				want = tc.admin
			}
			preErr := svc.NetworkUsableBy(ctx, netID, seededProjectID, admin)
			_, allocErr := svc.AllocatePortForInstance(ctx, netID, seededProjectID, uuid.New().String(), nil, admin)

			for _, got := range []struct {
				path string
				err  error
				// the not-found wording each path already used; kept so a foreign private network
				// still reads exactly like one that does not exist
				notFoundMsg string
			}{
				{"pre-flight", preErr, "no such network"},
				{"allocation", allocErr, fmt.Sprintf("network %s not found", netID)},
			} {
				label := fmt.Sprintf("%s, admin=%v, %s", tc.name, admin, got.path)
				var fe *ExternalNetworkAttachForbiddenError
				switch want {
				case attach:
					if got.err != nil {
						t.Errorf("%s: refused (%v); want the attach allowed", label, got.err)
					}
				case forbidden:
					if !errors.As(got.err, &fe) {
						t.Errorf("%s: got %v; want ExternalNetworkAttachForbiddenError", label, got.err)
						continue
					}
					wantMsg := "It is not allowed to create an interface on external network " + netID
					if got.err.Error() != wantMsg {
						t.Errorf("%s: message %q; want upstream's %q", label, got.err.Error(), wantMsg)
					}
				case notFound:
					if got.err == nil || errors.As(got.err, &fe) {
						t.Errorf("%s: got %v; want the not-found refusal", label, got.err)
						continue
					}
					if got.err.Error() != got.notFoundMsg {
						t.Errorf("%s: message %q; want %q, the same as for an id that names nothing",
							label, got.err.Error(), got.notFoundMsg)
					}
				}
			}
		}
	}
}
