package main

import (
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
)

// A zero HealthCheckPeriod reaches pgxpool as time.NewTicker(0), which panics —
// taking the process down at startup rather than returning an error. The Helm
// chart renders no database pool timings at all, so an unset config is the normal
// case, not an edge case.
//
// Found on a real cluster: `lightstack console-tokens list` built its own PoolConfig and
// crashed with "panic: non-positive interval for NewTicker". Every path that opens
// a pool must default through newPoolConfig.
func TestNewPoolConfigDefaultsAreTickerSafe(t *testing.T) {
	// A config with no database section at all — what the chart produces.
	pc := newPoolConfig(&common.Config{})

	if pc.HealthCheckPeriod <= 0 {
		t.Errorf("HealthCheckPeriod = %v; must be positive or pgxpool panics", pc.HealthCheckPeriod)
	}
	if pc.MaxConnLifetime <= 0 {
		t.Errorf("MaxConnLifetime = %v; must be positive", pc.MaxConnLifetime)
	}
	if pc.MaxConnIdleTime <= 0 {
		t.Errorf("MaxConnIdleTime = %v; must be positive", pc.MaxConnIdleTime)
	}
	if pc.MinConns <= 0 {
		t.Errorf("MinConns = %d; must be positive", pc.MinConns)
	}
	// pgxpool hands MaxConns to puddle as its size, and puddle refuses anything
	// below 1 ("MaxSize must be >= 1"), so an omitted max_connections — what
	// deploy-single-node.sh used to write — fails the whole startup.
	if pc.MaxConns != 20 {
		t.Errorf("MaxConns = %d; want the documented default of 20", pc.MaxConns)
	}
}

// Negative values are not "set": a negative health-check period reaches
// time.NewTicker exactly like a zero one and panics, and a negative MaxConns fails
// the pool. Each takes the default, the same as an omitted value.
func TestNewPoolConfigReplacesNegativeValues(t *testing.T) {
	cfg := &common.Config{}
	cfg.Database.MaxConnections = -1
	cfg.Database.MinConnections = -1
	cfg.Database.MaxConnLifetime = -time.Minute
	cfg.Database.MaxConnIdleTime = -time.Minute
	cfg.Database.HealthCheckPeriod = -time.Minute

	pc := newPoolConfig(cfg)

	if pc.MaxConns != 20 || pc.MinConns != 2 {
		t.Errorf("conns = %d/%d; want the defaults 20/2", pc.MaxConns, pc.MinConns)
	}
	if pc.MaxConnLifetime != time.Hour || pc.MaxConnIdleTime != 15*time.Minute || pc.HealthCheckPeriod != time.Minute {
		t.Errorf("durations = %v/%v/%v; want the defaults 1h/15m/1m",
			pc.MaxConnLifetime, pc.MaxConnIdleTime, pc.HealthCheckPeriod)
	}
}

// Explicit config values must survive — the defaults only fill gaps.
func TestNewPoolConfigHonoursExplicitValues(t *testing.T) {
	cfg := &common.Config{}
	cfg.Database.MaxConnections = 42
	cfg.Database.MinConnections = 7
	cfg.Database.MaxConnLifetime = 3 * time.Hour
	cfg.Database.MaxConnIdleTime = 9 * time.Minute
	cfg.Database.HealthCheckPeriod = 30 * time.Second

	pc := newPoolConfig(cfg)

	if pc.MaxConns != 42 || pc.MinConns != 7 {
		t.Errorf("conns = %d/%d; want 42/7", pc.MaxConns, pc.MinConns)
	}
	if pc.MaxConnLifetime != 3*time.Hour {
		t.Errorf("MaxConnLifetime = %v; want 3h", pc.MaxConnLifetime)
	}
	if pc.MaxConnIdleTime != 9*time.Minute {
		t.Errorf("MaxConnIdleTime = %v; want 9m", pc.MaxConnIdleTime)
	}
	if pc.HealthCheckPeriod != 30*time.Second {
		t.Errorf("HealthCheckPeriod = %v; want 30s", pc.HealthCheckPeriod)
	}
}
