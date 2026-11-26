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

// TestMigrationView pins the view's two rules (SPEC-028 FR-024, SPEC-032 FR-006): a backend file
// belongs to its backend only, and it replaces the plain file of its version — separately for up
// and down. Down files follow the same rules; non-migration files are ignored. Nothing is skipped:
// both backends start at the baseline, 001.
func TestMigrationView(t *testing.T) {
	src := migrationsFS(
		"001_baseline.postgres.up.sql", "001_baseline.postgres.down.sql",
		"001_baseline.sqlite.up.sql", "001_baseline.sqlite.down.sql",
		"002_v0.7.0.up.sql", "002_v0.7.0.down.sql",
		"003_v0.8.0.postgres.up.sql", "003_v0.8.0.postgres.down.sql",
		"003_v0.8.0.sqlite.up.sql", "003_v0.8.0.sqlite.down.sql",
		"004_v0.9.0.up.sql", "004_v0.9.0.down.sql",
		"004_v0.9.0.sqlite.down.sql",
		"README.md",
	)
	cases := map[Backend][]string{
		Postgres: {
			"001_baseline.postgres.down.sql", "001_baseline.postgres.up.sql",
			"002_v0.7.0.down.sql", "002_v0.7.0.up.sql",
			"003_v0.8.0.postgres.down.sql", "003_v0.8.0.postgres.up.sql",
			"004_v0.9.0.down.sql", "004_v0.9.0.up.sql",
		},
		SQLite: {
			"001_baseline.sqlite.down.sql", "001_baseline.sqlite.up.sql",
			"002_v0.7.0.down.sql", "002_v0.7.0.up.sql",
			"003_v0.8.0.sqlite.down.sql", "003_v0.8.0.sqlite.up.sql",
			"004_v0.9.0.sqlite.down.sql", "004_v0.9.0.up.sql",
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
			hidden := "001_baseline.sqlite.up.sql"
			if backend == SQLite {
				hidden = "001_baseline.postgres.up.sql"
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

// TestMigrationViewSkipsNothing: the retired rule 3 let SQLite skip every version below its own
// baseline. A view now neither requires a SQLite baseline nor hides a low version from SQLite —
// whether the folder holds the baseline pair is ValidateMigrations' rule, not the view's.
func TestMigrationViewSkipsNothing(t *testing.T) {
	v, err := newMigrationView(migrationsFS("001_a.up.sql", "001_a.down.sql", "002_b.up.sql", "002_b.down.sql"), SQLite)
	if err != nil {
		t.Fatalf("a SQLite view without a SQLite baseline: %v", err)
	}
	if got := strings.Join(viewNames(t, v), " "); got != "001_a.down.sql 001_a.up.sql 002_b.down.sql 002_b.up.sql" {
		t.Fatalf("view = %s; want every file", got)
	}
}
