package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// loadConfig is where every entry point — the server, cleanup and console-tokens —
// turns state_path into the node-local root. A subcommand that skipped it would act on
// the default root while the server used the configured one: cleanup would purge the
// wrong directory, console-tokens would list and purge nothing.
func TestLoadConfigAppliesStatePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lightstack.yaml")
	if err := os.WriteFile(path, []byte("state_path: /data/lightstack\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { paths.SetBase(paths.DefaultBase) })

	if _, err := loadConfig(path); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got, want := paths.ConsoleTokenDir(), "/data/lightstack/console-tokens"; got != want {
		t.Errorf("ConsoleTokenDir = %q; want %q", got, want)
	}
}
