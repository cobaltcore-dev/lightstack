package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// TestResolveBackupDest covers the three branches of the backup target resolution: too many
// arguments is refused, a named file is made absolute, and no file falls back to the default
// under <state_path>/backups with a timestamped name.
func TestResolveBackupDest(t *testing.T) {
	// resolveBackupDest mutates the package-global base via paths.SetBase; restore it.
	t.Cleanup(func() { paths.SetBase(paths.DefaultBase) })

	t.Run("too many arguments", func(t *testing.T) {
		if _, err := resolveBackupDest([]string{"a.db", "b.db"}, "/var/lib/lightstack"); err == nil ||
			!strings.Contains(err.Error(), "usage") {
			t.Fatalf("resolveBackupDest(two files) = %v; want a usage error", err)
		}
	})

	t.Run("named relative file is made absolute", func(t *testing.T) {
		got, err := resolveBackupDest([]string{"my.db"}, "/var/lib/lightstack")
		if err != nil {
			t.Fatalf("resolveBackupDest: %v", err)
		}
		if !filepath.IsAbs(got) || filepath.Base(got) != "my.db" {
			t.Errorf("resolveBackupDest(my.db) = %q; want an absolute path ending in my.db", got)
		}
	})

	t.Run("named absolute file is kept", func(t *testing.T) {
		got, err := resolveBackupDest([]string{"/tmp/backup.db"}, "/var/lib/lightstack")
		if err != nil {
			t.Fatalf("resolveBackupDest: %v", err)
		}
		if got != "/tmp/backup.db" {
			t.Errorf("resolveBackupDest(/tmp/backup.db) = %q; want it unchanged", got)
		}
	})

	t.Run("no file defaults under state_path", func(t *testing.T) {
		const statePath = "/srv/lightstack"
		got, err := resolveBackupDest(nil, statePath)
		if err != nil {
			t.Fatalf("resolveBackupDest: %v", err)
		}
		if dir := filepath.Dir(got); dir != filepath.Join(statePath, "backups") {
			t.Errorf("default backup directory = %q; want %q", dir, filepath.Join(statePath, "backups"))
		}
		if base := filepath.Base(got); !strings.HasPrefix(base, "lightstack-") || !strings.HasSuffix(base, ".db") {
			t.Errorf("default backup name = %q; want lightstack-<time>.db", base)
		}
	})
}
