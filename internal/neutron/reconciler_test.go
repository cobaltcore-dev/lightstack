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

// TestReadDHCPStateTellsUnreadFromAbsent — a network whose records could not be read is not a network
// without DHCP. Reading both as "nothing to serve" would converge a live network's DHCP away on a
// transient read error, taking DHCP and metadata from its guests.
func TestReadDHCPStateTellsUnreadFromAbsent(t *testing.T) {
	requireDB(t)

	t.Run("no DHCP port is nothing to serve", func(t *testing.T) {
		_, found, err := readDHCPState(context.Background(), testPool, absentID, nil)
		if found || err != nil {
			t.Fatalf("= (found %v, %v), want (false, nil)", found, err)
		}
	})

	t.Run("a failed read is an error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := readDHCPState(ctx, testPool, absentID, nil); err == nil {
			t.Fatal("a read that failed returned no error — the unit would converge this network's DHCP away")
		}
	})
}

// TestPruneDeletedNetworksKeepsWhatAReadCanSee — the prune removes the bridge of a network the database
// no longer has, and not one it has. It lists the bridges first and reads the networks after, so a
// network created while a pass runs is in the read; the pass used to read once up front, and a network
// created between that read and the prune lost its bridge right after its 201. That interleaving
// cannot be forced here; the contract around it can.
func TestPruneDeletedNetworksKeepsWhatAReadCanSee(t *testing.T) {
	requireDB(t)
	svc := NewService(testPool, "stub", nil)
	r := NewNetworkReconciler(svc, 0)

	live := seedNetwork(t, "spec024-prune-live-"+uuid.NewString()[:8])
	gone := uuid.NewString() // a network with no row: deleted, as far as the host can tell
	for _, id := range []string{live, gone} {
		plantStubBridge(t, svc, id)
	}

	r.pruneDeletedNetworks(context.Background())

	if !hasStubBridge(t, svc, "br-"+live[:8]) {
		t.Error("an existing network lost its bridge")
	}
	if hasStubBridge(t, svc, "br-"+gone[:8]) {
		t.Error("a deleted network kept its bridge")
	}
}
