package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
)

// migrateFolderNoChecks migrates the database at dbURL with the migrations in src, through the
// backend's view, exactly as withMigrator does — but without ValidateMigrations and without
// classifying the database first. The pre-squash history in testdata/pre-squash fails both by
// design (SPEC-032), and the baseline generators, the equivalence check and the pre-squash
// refusal tests replay it. target 0 migrates to the newest version.
func migrateFolderNoChecks(t *testing.T, dbURL string, src fs.FS, target uint64) {
	t.Helper()
	backend, err := ParseURL(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	view, err := newMigrationView(src, backend)
	if err != nil {
		t.Fatalf("migration view: %v", err)
	}
	drv, err := iofs.New(view, ".")
	if err != nil {
		t.Fatalf("migration source: %v", err)
	}
	m, cleanup, err := newMigrator(backend, dbURL, drv, true)
	if err != nil {
		_ = drv.Close()
		t.Fatalf("migrator: %v", err)
	}
	defer cleanup()
	if target == 0 {
		err = m.Up()
	} else {
		err = m.Migrate(uint(target))
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to %d: %v", target, err)
	}
}

// dropBelow hides every numbered migration file of src below version. It reproduces the retired
// view rule 3 — SQLite started at its own baseline — for replaying the pre-squash SQLite history,
// which began at 073_baseline.sqlite (research R4).
func dropBelow(src fs.FS, version uint64) fs.FS {
	return belowFilter{src: src, min: version}
}

type belowFilter struct {
	src fs.FS
	min uint64
}

func (f belowFilter) hidden(name string) bool {
	m, ok := parseMigrationFile(name)
	return ok && m.version < f.min
}

func (f belowFilter) Open(name string) (fs.File, error) {
	if f.hidden(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return f.src.Open(name)
}

func (f belowFilter) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.src, name)
	if err != nil {
		return nil, err
	}
	kept := entries[:0:0]
	for _, e := range entries {
		if !f.hidden(e.Name()) {
			kept = append(kept, e)
		}
	}
	return kept, nil
}

func TestDropBelow(t *testing.T) {
	src := fstest.MapFS{
		"072_x.up.sql":                {},
		"073_baseline.sqlite.up.sql":  {},
		"073_volume_transfer.up.sql":  {},
		"074_reservation_node.up.sql": {},
		"README.md":                   {},
	}
	entries, err := fs.ReadDir(dropBelow(src, 73), ".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"073_baseline.sqlite.up.sql", "073_volume_transfer.up.sql", "074_reservation_node.up.sql", "README.md"}
	if len(names) != len(want) {
		t.Fatalf("dropBelow kept %v; want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("dropBelow kept %v; want %v", names, want)
		}
	}
	if _, err := dropBelow(src, 73).Open("072_x.up.sql"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a hidden file still opens: %v", err)
	}
}

// copyMigrationFiles copies the real migrations folder's files — not next/, so none of the
// repository's fragments — into a temp dir, and returns it with the newest version it holds.
func copyMigrationFiles(t *testing.T) (string, uint64) {
	t.Helper()
	const real = "../../migrations"
	dir := t.TempDir()
	entries, err := os.ReadDir(real)
	if err != nil {
		t.Fatal(err)
	}
	var newest uint64
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(real, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if f, ok := parseMigrationFile(e.Name()); ok && f.version > newest {
			newest = f.version
		}
	}
	return dir, newest
}

// writeFiles writes each file of files, relative to dir, creating its directory.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// databaseModel reads the schema model of the database at url, on either backend: what the
// round-trip, fragment and merge tests compare. source is the migrations' SQL, whose install-time
// values the model normalizes (migrationsSource).
func databaseModel(t *testing.T, ctx context.Context, url, source string) schemaModel {
	t.Helper()
	var m schemaModel
	switch backend, err := ParseURL(url); {
	case err != nil:
		t.Fatal(err)
	case backend == Postgres:
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		tables, err := introspectPostgres(ctx, conn, "public")
		if err != nil {
			t.Fatal(err)
		}
		if m, err = postgresModel(tables, source); err != nil {
			t.Fatal(err)
		}
	default:
		path, err := sqlitePathFromURL(url)
		if err != nil {
			t.Fatal(err)
		}
		db, err := openSQLiteHandle(ctx, path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if m, err = sqliteModel(ctx, db, source); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// execMigrationSQL runs sql — one statement or several — on the database at url, outside any migrator and
// without Lightstack's SQL translation: the way a migration file runs.
func execMigrationSQL(ctx context.Context, url, sql string) error {
	backend, err := ParseURL(url)
	if err != nil {
		return err
	}
	if backend == Postgres {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		_, err = conn.Exec(ctx, sql) // without arguments: one simple query, several statements
		return err
	}
	path, err := sqlitePathFromURL(url)
	if err != nil {
		return err
	}
	db, err := openSQLiteHandle(ctx, path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1})
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, sql)
	return err
}

// withoutFragments hides the next/ directory of src: the numbered migrations alone, with no open
// release migration assembled.
func withoutFragments(src fs.FS) fs.FS { return fragmentFilter{src} }

type fragmentFilter struct{ src fs.FS }

func (f fragmentFilter) hidden(name string) bool {
	return name == fragmentDir || strings.HasPrefix(name, fragmentDir+"/")
}

func (f fragmentFilter) Open(name string) (fs.File, error) {
	if f.hidden(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return f.src.Open(name)
}

func (f fragmentFilter) ReadDir(name string) ([]fs.DirEntry, error) {
	if f.hidden(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	entries, err := fs.ReadDir(f.src, name)
	if err != nil {
		return nil, err
	}
	kept := entries[:0:0]
	for _, e := range entries {
		if name != "." || e.Name() != fragmentDir {
			kept = append(kept, e)
		}
	}
	return kept, nil
}

// replayFragments applies dir's numbered migrations to the empty database at url, then each of
// dir's fragments on its own, in name order, and returns the first failure, naming the fragment. A
// failing open release migration fails as a whole; this tells which fragment broke it, as when one
// sorts before a fragment it depends on (SPEC-032 US3 scenario 5).
func replayFragments(t *testing.T, ctx context.Context, url, dir string) error {
	t.Helper()
	backend, err := ParseURL(url)
	if err != nil {
		return err
	}
	migrateFolderNoChecks(t, url, withoutFragments(os.DirFS(dir)), 0)
	frags, err := readFragments(os.DirFS(dir))
	if err != nil {
		return err
	}
	for _, f := range frags {
		if f.direction != "up" || (f.backend != "" && f.backend != backend) {
			continue
		}
		sql, err := os.ReadFile(filepath.Join(dir, "next", f.file))
		if err != nil {
			return err
		}
		if err := execMigrationSQL(ctx, url, string(sql)); err != nil {
			return fmt.Errorf("fragment %s: %w", f.file, err)
		}
	}
	return nil
}

// migrationsWithProbeRelease copies the real migrations folder's files (not next/) into a temp dir
// and adds one release migration past its newest version, which adds the column volumes.probe. It
// returns the folder and the probe's version. Tests that need a version past another one — an older
// database migrated forward — get one whatever the real folder holds.
func migrationsWithProbeRelease(t *testing.T) (string, uint64) {
	t.Helper()
	dir, newest := copyMigrationFiles(t)
	probe := newest + 1
	header := fmt.Sprintf("release migration %03d (v0.0.0-probe): a test fixture", probe)
	for name, data := range map[string][]byte{
		fmt.Sprintf("%03d_v0.0.0-probe.up.sql", probe):   renderUp(header, probe, []byte("ALTER TABLE volumes ADD COLUMN probe TEXT;\n")),
		fmt.Sprintf("%03d_v0.0.0-probe.down.sql", probe): renderDown(header, probe, []byte("ALTER TABLE volumes DROP COLUMN probe;\n")),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, probe
}
