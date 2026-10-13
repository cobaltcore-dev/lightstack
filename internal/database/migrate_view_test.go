package database

import (
	"errors"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func migrationsFS(names ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for _, n := range names {
		m[n] = &fstest.MapFile{Data: []byte("-- " + n + "\n")}
	}
	return m
}

func viewNames(t *testing.T, v fs.FS) []string {
	t.Helper()
	entries, err := fs.ReadDir(v, ".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestMigrationView pins FR-024's three rules: a backend file belongs to its backend
// only, it replaces the plain file of its version, and SQLite skips every version
// below its baseline. Down files follow the same rules; non-migration files are ignored.
func TestMigrationView(t *testing.T) {
	src := migrationsFS(
		"001_initial.up.sql", "001_initial.down.sql",
		"072_old.up.sql", "072_old.down.sql",
		"073_volume_transfer.up.sql", "073_volume_transfer.down.sql",
		"073_baseline.sqlite.up.sql", "073_baseline.sqlite.down.sql",
		"074_shared.up.sql", "074_shared.down.sql",
		"075_split.postgres.up.sql", "075_split.postgres.down.sql",
		"075_split.sqlite.up.sql", "075_split.sqlite.down.sql",
		"076_override.up.sql", "076_override.down.sql",
		"076_override.sqlite.up.sql", "076_override.sqlite.down.sql",
		"README.md",
	)
	cases := map[Backend][]string{
		Postgres: {
			"001_initial.down.sql", "001_initial.up.sql",
			"072_old.down.sql", "072_old.up.sql",
			"073_volume_transfer.down.sql", "073_volume_transfer.up.sql",
			"074_shared.down.sql", "074_shared.up.sql",
			"075_split.postgres.down.sql", "075_split.postgres.up.sql",
			"076_override.down.sql", "076_override.up.sql",
		},
		SQLite: {
			"073_baseline.sqlite.down.sql", "073_baseline.sqlite.up.sql",
			"074_shared.down.sql", "074_shared.up.sql",
			"075_split.sqlite.down.sql", "075_split.sqlite.up.sql",
			"076_override.sqlite.down.sql", "076_override.sqlite.up.sql",
		},
	}
	for backend, want := range cases {
		t.Run(string(backend), func(t *testing.T) {
			v, err := newMigrationView(src, backend)
			if err != nil {
				t.Fatalf("view: %v", err)
			}
			if got := viewNames(t, v); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("view:\n got  %v\n want %v", got, want)
			}
			hidden := "073_baseline.sqlite.up.sql"
			if backend == SQLite {
				hidden = "001_initial.up.sql"
			}
			if _, err := v.Open(hidden); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Open(%s) = %v; want fs.ErrNotExist", hidden, err)
			}
			// golang-migrate's own source reads the view without a duplicate version.
			drv, err := iofs.New(v, ".")
			if err != nil {
				t.Fatalf("iofs.New on the view: %v", err)
			}
			_ = drv.Close()
		})
	}
}

// TestMigrationViewBaseline: SQLite starts at its baseline; the baseline is required
// for a SQLite view and never shown to PostgreSQL.
func TestMigrationViewBaseline(t *testing.T) {
	if got, err := baselineVersion(migrationsFS("073_baseline.sqlite.up.sql", "001_a.up.sql")); err != nil || got != 73 {
		t.Fatalf("baselineVersion = %d, %v; want 73", got, err)
	}
	if _, err := newMigrationView(migrationsFS("001_a.up.sql", "001_a.down.sql"), SQLite); err == nil {
		t.Fatal("a SQLite view without a baseline was accepted")
	}
}
