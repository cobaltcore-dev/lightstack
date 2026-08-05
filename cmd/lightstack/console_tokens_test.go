package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Usage, help and an unknown subcommand are answered without a database: they are what
// an operator reaches for when something is wrong, and the database may be what is
// wrong. The config names a database nothing listens on, so a path that tried to
// connect would exit 1 with a connection error instead.
func TestRunConsoleTokensUsageNeedsNoDatabase(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "lightstack.yaml")
	db := "database:\n  url: postgres://x@127.0.0.1:1/x?sslmode=disable\n"
	if err := os.WriteFile(cfg, []byte(db), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, 2}, // no subcommand: usage error
		{[]string{"help"}, 0},
		{[]string{"wat"}, 2}, // unknown subcommand: usage error
	} {
		args := append([]string{"--config", cfg}, tc.args...)
		if got := runConsoleTokens(args); got != tc.want {
			t.Errorf("runConsoleTokens(%v) = %d; want %d", tc.args, got, tc.want)
		}
	}
}
