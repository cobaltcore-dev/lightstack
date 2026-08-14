package neutron

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPollIntervalReportsTheEffectiveValue — cloud-storage#546, the reconcile-interval log line.
//
// cmd/lightstack/main.go used to log cfg.Neutron.ReconcileInterval, the CONFIGURED value. Left unset that is
// zero, so a default real-mode deployment printed "Real-mode network reconciler started
// (interval: 0s)" and read as though periodic reconciliation were switched off — when in fact
// NewNetworkReconciler clamps anything <= 0 to 30s and it reconciles every 30 seconds.
//
// The accessor exists so the log site cannot restate the clamp and drift from it.
func TestPollIntervalReportsTheEffectiveValue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{"unset defaults to 30s", 0, 30 * time.Second},
		{"negative defaults to 30s", -5 * time.Second, 30 * time.Second},
		{"explicit value is kept", 45 * time.Second, 45 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewNetworkReconciler(nil, tc.configured).PollInterval(); got != tc.want {
				t.Errorf("PollInterval() = %s, want %s — the log would say %s", got, tc.want, got)
			}
		})
	}
}

// TestApplyDHCPForNetworkTellsUnreadFromAbsent — a network whose subnet could not be read is not a
// network without one. applyDHCPForNetwork used to answer both with two empty strings, so the
// reconciler left an unread network out of the set it prunes metadata redirects against and deleted
// a live network's redirect on a transient read error. Both cases return before any host call, which
// is why a non-stub Service with nothing but a pool is enough here.
func TestApplyDHCPForNetworkTellsUnreadFromAbsent(t *testing.T) {
	requireDB(t)
	svc := &Service{pool: testPool, mode: "iptables"}

	t.Run("no DHCP-enabled subnet is nothing to do", func(t *testing.T) {
		bridge, addr, err := svc.applyDHCPForNetwork(context.Background(), absentID, false)
		if bridge != "" || addr != "" || err != nil {
			t.Fatalf("= (%q, %q, %v), want (\"\", \"\", nil)", bridge, addr, err)
		}
	})

	t.Run("a failed read is an error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		bridge, addr, err := svc.applyDHCPForNetwork(ctx, absentID, false)
		if err == nil {
			t.Fatal("a read that failed returned no error — the reconciler would prune this network's redirect")
		}
		if bridge != "" || addr != "" {
			t.Errorf("= (%q, %q), want nothing asserted", bridge, addr)
		}
	})
}

// TestPruneDeletedNetworksKeepsWhatAReadCanSee — the prune removes the bridge and the DHCP server of a
// network the database no longer has, and nothing of one it has. It lists the host state first and
// reads the networks after, so a network created while a pass runs is in the read; the pass used to
// read once up front, and a network created between that read and the prune lost both right after
// its 201. That interleaving cannot be forced here; the contract around it can.
func TestPruneDeletedNetworksKeepsWhatAReadCanSee(t *testing.T) {
	requireDB(t)
	svc := NewService(testPool, "stub", nil)
	r := NewNetworkReconciler(svc, 0)

	live := seedNetwork(t, "spec024-prune-live-"+uuid.NewString()[:8])
	gone := uuid.NewString() // a network with no row: deleted, as far as the host can tell
	for _, id := range []string{live, gone} {
		plantStubBridge(t, svc, id)
		markDHCPServing(t, svc, id)
	}

	r.pruneDeletedNetworks(context.Background())

	if !hasStubBridge(t, svc, "br-"+live[:8]) || !svc.dhcpManager.IsRunning(live) {
		t.Errorf("an existing network lost its host state: bridge=%v dhcp=%v",
			hasStubBridge(t, svc, "br-"+live[:8]), svc.dhcpManager.IsRunning(live))
	}
	if hasStubBridge(t, svc, "br-"+gone[:8]) || svc.dhcpManager.IsRunning(gone) {
		t.Errorf("a deleted network kept its host state: bridge=%v dhcp=%v",
			hasStubBridge(t, svc, "br-"+gone[:8]), svc.dhcpManager.IsRunning(gone))
	}
}

// TestDesiredRedirectsNowAddsNetworksTheEnumerationMissed — the metadata DNAT prune compares the
// listed rules with what this returns, called after the listing. A network whose DHCP subnet was
// created after the pass enumerated them, and whose handler has added its redirect since, is not in
// the pass's own map; without this read its fresh rule was pruned. The pass's entries are kept as
// they are, and the added address is the one applyDHCPForNetwork derives.
func TestDesiredRedirectsNowAddsNetworksTheEnumerationMissed(t *testing.T) {
	requireDB(t)
	svc := &Service{pool: testPool, mode: "iptables"}

	inPass := seedNetwork(t, "spec024-redirect-pass-"+uuid.NewString()[:8])
	seedDHCPSubnet(t, inPass, "spec024-redirect-pass", "10.70.0.0/24")
	missed := seedNetwork(t, "spec024-redirect-missed-"+uuid.NewString()[:8])
	seedDHCPSubnet(t, missed, "spec024-redirect-missed", "10.71.0.0/24")
	noDHCP := seedNetwork(t, "spec024-redirect-nodhcp-"+uuid.NewString()[:8])
	seedSubnet(t, noDHCP, "spec024-redirect-nodhcp", "10.72.0.0/24", nil, 4, false)

	pass := map[string]string{"br-" + inPass[:8]: "10.70.0.99"}
	want, err := svc.desiredRedirectsNow(context.Background(), pass)
	if err != nil {
		t.Fatalf("desiredRedirectsNow: %v", err)
	}
	if got := want["br-"+inPass[:8]]; got != "10.70.0.99" {
		t.Errorf("the pass's own entry = %q, want it kept as 10.70.0.99", got)
	}
	if got := want["br-"+missed[:8]]; got != "10.71.0.1" {
		t.Errorf("the missed network's redirect = %q, want 10.71.0.1 (its gatewayless subnet's first host)", got)
	}
	if _, ok := want["br-"+noDHCP[:8]]; ok {
		t.Errorf("a network without a DHCP-enabled subnet got a redirect")
	}
	if len(pass) != 1 {
		t.Errorf("the pass's map was modified: %v", pass)
	}
}
