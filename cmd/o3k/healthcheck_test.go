package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRunHealthcheck exercises the `o3k healthcheck` subcommand against stub
// endpoints: 200 → exit 0, 503 → exit 1, unreachable → exit 1 (SPEC-013 FR-011).
func TestRunHealthcheck(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer ready.Close()
	if code := runHealthcheck([]string{"--addr", ready.URL, "--ready"}); code != 0 {
		t.Fatalf("ready endpoint → exit %d, want 0", code)
	}
	if code := runHealthcheck([]string{"--addr", ready.URL, "--live"}); code != 0 {
		t.Fatalf("live endpoint → exit %d, want 0", code)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("database unavailable"))
	}))
	defer down.Close()
	if code := runHealthcheck([]string{"--addr", down.URL}); code != 1 {
		t.Fatalf("503 endpoint → exit %d, want 1", code)
	}

	// Unreachable address (server closed) → exit 1.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	if code := runHealthcheck([]string{"--addr", closedURL}); code != 1 {
		t.Fatalf("unreachable → exit %d, want 1", code)
	}
}

// TestRunHealthcheck_UsageError: an unknown flag is a usage error → exit 2.
func TestRunHealthcheck_UsageError(t *testing.T) {
	if code := runHealthcheck([]string{"--bogus"}); code != 2 {
		t.Fatalf("bad flag → exit %d, want 2", code)
	}
}

// TestDrainDelay: O3K_DRAIN_SECONDS parsing — empty/invalid/non-positive → 0.
func TestDrainDelay(t *testing.T) {
	cases := map[string]time.Duration{
		"":    0,
		"abc": 0,
		"-3":  0,
		"0":   0,
		"5":   5 * time.Second,
	}
	for val, want := range cases {
		t.Run(val, func(t *testing.T) {
			t.Setenv("O3K_DRAIN_SECONDS", val)
			if got := drainDelay(); got != want {
				t.Fatalf("O3K_DRAIN_SECONDS=%q → %v, want %v", val, got, want)
			}
		})
	}
}
