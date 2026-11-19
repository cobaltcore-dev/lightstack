package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// SPEC-031 FR-009: in real mode loadConfig refuses a libvirt_uri Lightstack cannot honour,
// naming it. Stub mode never dials libvirt, so it does not check the value.
func TestLoadConfigLibvirtURI(t *testing.T) {
	t.Cleanup(func() { paths.SetBase(paths.DefaultBase) })
	load := func(mode, uri string) error {
		path := filepath.Join(t.TempDir(), "lightstack.yaml")
		config := fmt.Sprintf("nova:\n  libvirt_mode: %s\n  libvirt_uri: %q\n", mode, uri)
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfig(path)
		return err
	}

	if err := load("real", "qemu:///session"); err == nil || !strings.Contains(err.Error(), "qemu:///session") {
		t.Errorf("real mode, qemu:///session: %v; want an error naming the value", err)
	}
	if err := load("stub", "qemu:///session"); err != nil {
		t.Errorf("stub mode, qemu:///session: %v; stub mode does not check libvirt_uri", err)
	}
	if err := load("real", "qemu+unix:///system?socket=/run/x/s"); err != nil {
		t.Errorf("real mode, an explicit socket: %v", err)
	}
}
