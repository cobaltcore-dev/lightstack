package neutron

import (
	"context"
	"testing"
	"time"
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
