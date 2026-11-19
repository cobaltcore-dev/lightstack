package database

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The release step freezes the open release migration into numbered files (SPEC-032 FR-021,
// FR-023, FR-024, contracts/migrations-folder.md § Freeze).

// fragmentFiles lists the files left in dir's next/ directory.
func fragmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, fragmentDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// withoutFirstLine is data after its first line: what the open and the frozen form of a migration
// share.
func withoutFirstLine(data []byte) string {
	_, rest, _ := strings.Cut(string(data), "\n")
	return rest
}

func TestFreeze(t *testing.T) {
	plain := map[string]string{
		"next/.gitkeep": "",
		"next/20990101000000-1-freeze-demo.up.sql":   "CREATE TABLE freeze_demo (id TEXT);\n",
		"next/20990101000000-1-freeze-demo.down.sql": "DROP TABLE freeze_demo;\n",
	}

	t.Run("plain fragments become one plain pair", func(t *testing.T) {
		dir, _ := copyMigrationFiles(t)
		writeFiles(t, dir, plain)
		served := map[string][]byte{}
		for _, b := range []Backend{Postgres, SQLite} {
			view, err := newMigrationView(os.DirFS(dir), b)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range []string{"up", "down"} {
				data, err := fs.ReadFile(view, "002_open."+part+".sql")
				if err != nil {
					t.Fatal(err)
				}
				served[string(b)+"."+part] = data
			}
		}
		res, err := Freeze(dir, "v0.7.0")
		if err != nil {
			t.Fatalf("Freeze: %v", err)
		}
		if res.Version != 2 || strings.Join(res.Written, " ") != "002_v0.7.0.down.sql 002_v0.7.0.up.sql" {
			t.Fatalf("Freeze = %+v; want 002_v0.7.0's up and down files", res)
		}
		for _, part := range []string{"up", "down"} {
			frozen, err := os.ReadFile(filepath.Join(dir, "002_v0.7.0."+part+".sql"))
			if err != nil {
				t.Fatal(err)
			}
			want := "-- release migration 002 (v0.7.0): frozen from migrations/next/ by lightstack-migrate freeze"
			if first, _, _ := strings.Cut(string(frozen), "\n"); first != want {
				t.Errorf("%s header %q; want %q", part, first, want)
			}
			for _, b := range []string{"postgres", "sqlite"} {
				if withoutFirstLine(frozen) != withoutFirstLine(served[b+"."+part]) {
					t.Errorf("frozen %s differs from the %s open migration beyond its header", part, b)
				}
			}
		}
		if left := fragmentFiles(t, dir); strings.Join(left, " ") != ".gitkeep" {
			t.Errorf("next/ holds %v after the freeze; want only .gitkeep", left)
		}
		if err := ValidateMigrations(dir); err != nil {
			t.Errorf("ValidateMigrations after the freeze: %v", err)
		}
	})

	t.Run("a backend-specific fragment gives each backend its pair", func(t *testing.T) {
		dir, _ := copyMigrationFiles(t)
		writeFiles(t, dir, map[string]string{
			"next/20990101000000-1-freeze-pg.postgres.up.sql":   "CREATE INDEX freeze_idx ON volumes USING btree (name);\n",
			"next/20990101000000-1-freeze-pg.postgres.down.sql": "DROP INDEX freeze_idx;\n",
		})
		res, err := Freeze(dir, "v0.7.0")
		if err != nil {
			t.Fatalf("Freeze: %v", err)
		}
		want := "002_v0.7.0.postgres.down.sql 002_v0.7.0.postgres.up.sql 002_v0.7.0.sqlite.down.sql 002_v0.7.0.sqlite.up.sql"
		if strings.Join(res.Written, " ") != want {
			t.Fatalf("Freeze wrote %v; want %s", res.Written, want)
		}
		if err := ValidateMigrations(dir); err != nil {
			t.Errorf("ValidateMigrations after the freeze: %v", err)
		}
	})

	t.Run("no fragments, no migration", func(t *testing.T) {
		dir, _ := copyMigrationFiles(t)
		writeFiles(t, dir, map[string]string{"next/.gitkeep": ""})
		res, err := Freeze(dir, "v0.7.0")
		if err != nil || res.Version != 0 || len(res.Written) != 0 {
			t.Fatalf("Freeze = %+v, %v; want nothing written", res, err)
		}
		if !strings.Contains(res.String(), "no fragments in migrations/next/: release v0.7.0 adds no migration") {
			t.Errorf("result says %q", res.String())
		}
	})

	t.Run("a failed freeze changes nothing", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir, _ := copyMigrationFiles(t)
		writeFiles(t, dir, plain)
		before := fragmentFiles(t, dir)
		// A read-only next/ lets the release files be written, then fails the first fragment removal.
		next := filepath.Join(dir, fragmentDir)
		if err := os.Chmod(next, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(next, 0o755) })
		if _, err := Freeze(dir, "v0.7.0"); err == nil || !strings.Contains(err.Error(), "rolled back") {
			t.Fatalf("Freeze with a read-only next/ = %v; want a failure that was rolled back", err)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "002_*")); len(left) != 0 {
			t.Errorf("the failed freeze left %v", left)
		}
		if got := fragmentFiles(t, dir); !slices.Equal(got, before) {
			t.Errorf("fragments after the failed freeze = %v; want %v", got, before)
		}
		if err := ValidateMigrations(dir); err != nil {
			t.Errorf("the folder after the failed freeze: %v", err)
		}
		if err := os.Chmod(next, 0o755); err != nil {
			t.Fatal(err)
		}
		if res, err := Freeze(dir, "v0.7.0"); err != nil || res.Version != 2 {
			t.Fatalf("Freeze once next/ is writable = %+v, %v; want release migration 002", res, err)
		}
	})

	t.Run("a release name that is not v<semver>", func(t *testing.T) {
		dir, _ := copyMigrationFiles(t)
		writeFiles(t, dir, plain)
		for _, release := range []string{"v0.7", "0.7.0", "v0.7.0_rc1"} {
			if _, err := Freeze(dir, release); err == nil || !strings.Contains(err.Error(), "release name must be v<semver>") {
				t.Errorf("Freeze(%q) = %v; want the release-name refusal", release, err)
			}
		}
		if left := fragmentFiles(t, dir); len(left) != 3 {
			t.Errorf("a refused freeze changed next/: %v", left)
		}
	})

	t.Run("a release that already has a migration", func(t *testing.T) {
		dir, _ := copyMigrationFiles(t)
		writeFiles(t, dir, plain)
		if _, err := Freeze(dir, "v0.7.0"); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, dir, plain)
		if _, err := Freeze(dir, "v0.7.0"); err == nil || !strings.Contains(err.Error(), "release v0.7.0 already has migration 002_v0.7.0.") {
			t.Fatalf("a second freeze of v0.7.0 = %v; want it refused, naming the file", err)
		}
	})

	t.Run("databases across the freeze", func(t *testing.T) {
		dir, _ := copyMigrationFiles(t)
		writeFiles(t, dir, plain)
		// earlier applies the open release migration with one fragment; a second one follows.
		earlier, _ := freshDatabase(t, "lightstack_freeze_earlier")
		if err := MigrateUp(earlier, dir); err != nil {
			t.Fatal(err)
		}
		writeFiles(t, dir, map[string]string{
			"next/20990101000001-2-freeze-more.up.sql":   "CREATE TABLE freeze_more (id TEXT);\n",
			"next/20990101000001-2-freeze-more.down.sql": "DROP TABLE freeze_more;\n",
		})
		// current applies the open state the release freezes.
		current, _ := freshDatabase(t, "lightstack_freeze_current")
		if err := MigrateUp(current, dir); err != nil {
			t.Fatal(err)
		}
		if _, err := Freeze(dir, "v0.7.0"); err != nil {
			t.Fatal(err)
		}
		if err := MigrateUp(current, dir); err != nil {
			t.Errorf("a database migrated with the open state, after the freeze (FR-021): %v", err)
		}
		if err := MigrateUp(earlier, dir); !IsStale(err) || !strings.Contains(err.Error(), "has since been frozen as release v0.7.0 with different content") {
			t.Errorf("a database migrated with an earlier state, after the freeze = %v; want the stale refusal", err)
		}
	})
}
