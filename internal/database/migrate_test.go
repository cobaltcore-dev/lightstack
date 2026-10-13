package database

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLatestSourceMigrationVersion pins the `lightstack migrate --dry-run`
// backing: the highest .up.sql version wins, and files that do not follow the
// <version>_<name>.up.sql naming scheme are ignored like the file source
// driver ignores them.
func TestLatestSourceMigrationVersion(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"001_initial.up.sql":         "",
		"002_add_servers.up.sql":     "",
		"002_add_servers.down.sql":   "", // down migrations do not count
		"10_add_ports.up.sql":        "", // no zero padding required
		"README.md":                  "", // not a migration
		"not-a-number_rename.up.sql": "", // malformed version prefix
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	latest, err := LatestSourceMigrationVersion(dir)
	if err != nil {
		t.Fatalf("LatestSourceMigrationVersion: %v", err)
	}
	if latest != 10 {
		t.Errorf("LatestSourceMigrationVersion = %d; want 10", latest)
	}

	if _, err := LatestSourceMigrationVersion(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Error("LatestSourceMigrationVersion accepted a missing directory; want error")
	}
}
