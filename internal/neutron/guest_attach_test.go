package neutron

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// tenantMessager is the behaviour compute asserts before it shows an allocation failure to the tenant.
type tenantMessager interface{ TenantMessage() string }

// TestAllocationFailuresMarkWhatATenantMaySee: a failed boot's reason is shown in the server's own
// fault, which the tenant reads. The failures they can act on are marked as fit to show, and say which
// network; a failure only an operator can act on — here the database, via a cancelled request — is
// not, so compute reports it without its text. A wrapped database error used to reach `server show`.
func TestAllocationFailuresMarkWhatATenantMaySee(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	ctx := context.Background()
	svc := NewService(testPool, "stub", nil)

	noSubnet := seedNetwork(t, "spec025-visible-nosubnet")
	foreignExternal := seedAttachNetwork(t, "spec025-visible-external", otherProjectID, false, true)
	for _, tc := range []struct{ name, networkID string }{
		{"absent network", uuid.New().String()},
		{"no subnet", noSubnet},
		{"unshared external network", foreignExternal},
	} {
		_, err := svc.AllocatePortForInstance(ctx, tc.networkID, seededProjectID, uuid.New().String(), nil, false)
		var visible tenantMessager
		if !errors.As(err, &visible) {
			t.Errorf("%s: %v is not marked fit for the tenant; it is a condition they can act on", tc.name, err)
			continue
		}
		if !strings.Contains(visible.TenantMessage(), tc.networkID) {
			t.Errorf("%s: tenant message %q does not name the network", tc.name, visible.TenantMessage())
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, call := range []struct {
		name string
		err  error
	}{
		{"pre-flight", svc.NetworkUsableBy(cancelled, noSubnet, seededProjectID, false)},
		{"allocation", func() error {
			_, err := svc.AllocatePortForInstance(cancelled, noSubnet, seededProjectID, uuid.New().String(), nil, false)
			return err
		}()},
	} {
		var visible tenantMessager
		if call.err == nil || errors.As(call.err, &visible) {
			t.Errorf("%s with the database unreachable: %v; want an error NOT marked for the tenant", call.name, call.err)
		}
	}
}
