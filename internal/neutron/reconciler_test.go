package neutron

import (
	"testing"
	"time"
)

// TestPollIntervalReportsTheEffectiveValue — cloud-storage#546, the reconcile-interval log line.
//
// cmd/o3k/main.go used to log cfg.Neutron.ReconcileInterval, the CONFIGURED value. Left unset that is
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
