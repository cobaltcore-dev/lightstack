package main

import (
	"errors"
	"os"
	"os/exec"
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

// TestFreezeNeedsNoConfig pins SPEC-032 FR-023: freeze works on the migrations folder alone, so a
// maintainer runs it in a checkout with no configuration and no database.
func TestFreezeNeedsNoConfig(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "lightstack-migrate")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	copyFolder := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "migrations")
		if err := os.CopyFS(dir, os.DirFS("../../migrations")); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	freeze := func(t *testing.T, migrations string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, "freeze", "-migrations", migrations, "v0.7.0")
		cmd.Dir = t.TempDir() // no config/lightstack.yaml here
		cmd.Env = append(os.Environ(), "LIGHTSTACK_DB_URL=")
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}

	t.Run("no fragments", func(t *testing.T) {
		out, code := freeze(t, copyFolder(t))
		if code != 0 || !strings.Contains(out, "adds no migration") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("a fragment", func(t *testing.T) {
		migrations := copyFolder(t)
		for part, sql := range map[string]string{"up": "CREATE TABLE freeze_demo (id TEXT);\n", "down": "DROP TABLE freeze_demo;\n"} {
			if err := os.WriteFile(filepath.Join(migrations, "next", "20990101000000-1-freeze-demo."+part+".sql"), []byte(sql), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out, code := freeze(t, migrations)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if _, err := os.Stat(filepath.Join(migrations, "002_v0.7.0.up.sql")); err != nil {
			t.Fatalf("freeze wrote no 002_v0.7.0.up.sql: %v\n%s", err, out)
		}
	})
}
