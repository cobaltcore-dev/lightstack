package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// writeMigrations writes a migrations folder of the given files into a temp dir.
func writeMigrations(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestValidateMigrations pins the post-squash folder grammar (SPEC-032 contracts/migrations-folder.md):
// version 001 is the baseline pair of each backend, every later numbered file is a release migration
// numbered consecutively from 002 with one release per version, and each backend's view pairs every
// up with a down. Any other numbered file is refused: schema changes go into fragments (FR-026).
func TestValidateMigrations(t *testing.T) {
	valid := map[string]string{
		"001_baseline.postgres.up.sql":   "CREATE TABLE a (id UUID);",
		"001_baseline.postgres.down.sql": "DROP TABLE a;",
		"001_baseline.sqlite.up.sql":     "CREATE TABLE a (id TEXT);",
		"001_baseline.sqlite.down.sql":   "DROP TABLE a;",
		"002_v0.7.0.up.sql":              "ALTER TABLE a ADD COLUMN x TEXT;",
		"002_v0.7.0.down.sql":            "ALTER TABLE a DROP COLUMN x;",
		"003_v0.8.0.postgres.up.sql":     "ALTER TABLE a ADD COLUMN y BOOLEAN;",
		"003_v0.8.0.postgres.down.sql":   "ALTER TABLE a DROP COLUMN y;",
		"003_v0.8.0.sqlite.up.sql":       "ALTER TABLE a ADD COLUMN y INTEGER;",
		"003_v0.8.0.sqlite.down.sql":     "ALTER TABLE a DROP COLUMN y;",
		"004_v0.9.0-rc.1.up.sql":         "SELECT 1;",
		"004_v0.9.0-rc.1.down.sql":       "SELECT 1;",
		"README.md":                      "not a migration",
	}
	with := func(changes map[string]string, drop ...string) map[string]string {
		out := map[string]string{}
		for k, v := range valid {
			out[k] = v
		}
		for _, k := range drop {
			delete(out, k)
		}
		for k, v := range changes {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string // "" for valid
	}{
		{"valid", valid, ""},
		{"a PostgreSQL down file missing", with(nil, "001_baseline.postgres.down.sql"), "postgres view: 001_baseline.postgres.up.sql has no down migration"},
		{"a SQLite down file missing", with(nil, "001_baseline.sqlite.down.sql"), "sqlite view: 001_baseline.sqlite.up.sql has no down migration"},
		{"a down file shared by both views missing", with(nil, "002_v0.7.0.down.sql"), "has no down migration"},
		{"no SQLite baseline", with(nil, "001_baseline.sqlite.up.sql", "001_baseline.sqlite.down.sql"),
			"version 001 must be the baseline pair of each backend"},
		{"a plain file at 001", with(map[string]string{"001_extra.up.sql": "", "001_extra.down.sql": ""}),
			"version 001 must be the baseline pair of each backend"},
		{"a hand-numbered migration", with(map[string]string{"005_add_x.up.sql": "", "005_add_x.down.sql": ""}),
			"005_add_x.down.sql: a numbered migration other than the baseline or a release migration; schema changes go into fragments (migrations/next/)"},
		{"a release name that is not semver", with(map[string]string{"005_v0.9.up.sql": "", "005_v0.9.down.sql": ""}),
			"schema changes go into fragments (migrations/next/)"},
		{"a gap in the release numbers", with(map[string]string{"006_v1.0.0.up.sql": "", "006_v1.0.0.down.sql": ""}),
			"release migrations must be numbered consecutively from 002: 006_v1.0.0.down.sql"},
		{"one release on two versions", with(map[string]string{"005_v0.8.0.up.sql": "", "005_v0.8.0.down.sql": ""}),
			"release v0.8.0 has two versions: 003 and 005"},
		{"a version naming two releases", with(map[string]string{"003_v0.8.1.up.sql": "", "003_v0.8.1.down.sql": ""}),
			"version 003 names two releases: v0.8.0 and v0.8.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMigrations(writeMigrations(t, tc.files))
			if tc.want == "" && err != nil {
				t.Fatalf("ValidateMigrations: %v; want nil", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("ValidateMigrations = %v; want an error containing %q", err, tc.want)
			}
		})
	}
}

// TestMigrationsFolder pins the real migrations/ folder, not a synthetic one (FR-006, FR-026): a
// pairing mistake there stops the default container, which runs lightstack-migrate before the
// server, and a hand-numbered migration would bring back the per-feature numbering the squash
// ended. CI runs this in the unit-test job, so both fail the build first.
func TestMigrationsFolder(t *testing.T) {
	const dir = "../../migrations"
	if err := ValidateMigrations(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	baseline := map[string]bool{}
	for _, e := range entries {
		f, ok := parseMigrationFile(e.Name())
		if !ok {
			continue
		}
		switch {
		case f.version == 1:
			baseline[e.Name()] = true
		case !releaseFileNameSpec.MatchString(e.Name()):
			t.Errorf("%s is neither the baseline nor a release migration", e.Name())
		}
	}
	for _, want := range []string{"001_baseline.postgres.up.sql", "001_baseline.postgres.down.sql",
		"001_baseline.sqlite.up.sql", "001_baseline.sqlite.down.sql"} {
		if !baseline[want] {
			t.Errorf("the folder lacks %s", want)
		}
	}
	if len(baseline) != 4 {
		t.Errorf("version 001 holds %d files; want exactly the four baseline files", len(baseline))
	}
}

// releaseFileNameSpec is a release migration's file name, NNN_v<semver>[.backend].(up|down).sql,
// written out here independently of the implementation's own pattern.
var releaseFileNameSpec = regexp.MustCompile(`^\d{3}_v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\.(postgres|sqlite))?\.(up|down)\.sql$`)

// sqliteTables lists the tables of a SQLite database other than the migrator's and
// SQLite's own.
func sqliteTables(t *testing.T, path string) []string {
	t.Helper()
	db, err := openSQLiteHandle(context.Background(), path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE type = 'table'
		AND name NOT IN ('schema_migrations', 'sqlite_sequence') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestMigrateFuncsRefuseABadURL: every migrate entry point validates the URL first and
// returns the scheme error, touching nothing on disk.
func TestMigrateFuncsRefuseABadURL(t *testing.T) {
	const bad = "mysql://db/x"
	const migrations = "../../migrations"
	checks := map[string]func() error{
		"MigrateUp":             func() error { return MigrateUp(bad, migrations) },
		"MigrateDown":           func() error { return MigrateDown(bad, migrations) },
		"MigrateReset":          func() error { return MigrateReset(bad, migrations) },
		"MigrateToVersion":      func() error { return MigrateToVersion(bad, migrations, 50) },
		"ForceMigrationVersion": func() error { return ForceMigrationVersion(bad, migrations, 50) },
		"GetMigrationStatus":    func() error { _, err := GetMigrationStatus(bad, migrations); return err },
	}
	for name, run := range checks {
		if err := run(); err == nil || !strings.Contains(err.Error(), "accepted schemes") {
			t.Errorf("%s(%q) = %v; want the scheme refusal", name, bad, err)
		}
	}
}

// TestInspectCommandsDoNotCreateADatabase: status, down and force open must-exist, so a
// mistyped path is an error and no empty database (nor its directory) is conjured — unlike
// up, which creates on a first start.
func TestInspectCommandsDoNotCreateADatabase(t *testing.T) {
	const migrations = "../../migrations"
	dir := filepath.Join(t.TempDir(), "absent")
	path := filepath.Join(dir, "lightstack.db")
	url := "sqlite://" + path

	checks := map[string]func() error{
		"status": func() error { _, err := GetMigrationStatus(url, migrations); return err },
		"down":   func() error { return MigrateDown(url, migrations) },
		"force":  func() error { return ForceMigrationVersion(url, migrations, 1) },
	}
	for name, run := range checks {
		if err := run(); err == nil {
			t.Errorf("%s on a missing file succeeded; want an error", name)
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("%s created %s (stat: %v); must-exist commands create nothing", name, dir, statErr)
		}
	}
	// status says what is missing, as a type a caller can tell from a failure: the new release
	// creates the database at its first start (SPEC-032 FR-022).
	info, err := GetMigrationStatus(url, migrations)
	if !IsMissingDatabase(err) || !strings.Contains(err.Error(), path) {
		t.Errorf("status on a missing file: %v; want a missing-database error naming %s", err, path)
	}
	if info == nil || info.Version != 0 {
		t.Errorf("status on a missing file: info %+v; want version 0", info)
	}
}

// TestSQLiteMigrationsGoDownThroughTheBaseline: both backends start at the baseline, 001 (FR-006).
// Down and reset go through the baseline's down file to an empty database, and up rebuilds it.
func TestSQLiteMigrationsGoDownThroughTheBaseline(t *testing.T) {
	const migrations = "../../migrations"
	path := filepath.Join(t.TempDir(), "db", "lightstack.db")
	url := "sqlite://" + path

	if err := MigrateUp(url, migrations); err != nil {
		t.Fatalf("up: %v", err)
	}
	newest, err := GetMigrationStatus(url, migrations)
	if err != nil || newest.Version < 1 {
		t.Fatalf("version after up = %+v, %v; want 001 or later", newest, err)
	}
	for v := newest.Version; v >= 1; v-- {
		if err := MigrateDown(url, migrations); err != nil {
			t.Fatalf("down from %d: %v", v, err)
		}
	}
	if tables := sqliteTables(t, path); len(tables) != 0 {
		t.Fatalf("tables after going down through the baseline: %v; want none", tables)
	}
	if err := MigrateUp(url, migrations); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := MigrateReset(url, migrations); err != nil {
		t.Fatalf("reset: %v", err)
	}
	after, err := GetMigrationStatus(url, migrations)
	if err != nil || after.Version != newest.Version || after.Dirty {
		t.Fatalf("version after reset = %+v, %v; want %d, clean", after, err, newest.Version)
	}
	if tables := sqliteTables(t, path); len(tables) < 50 {
		t.Fatalf("%d tables after reset; want the whole schema", len(tables))
	}
}

// TestMigrateResetRecoversADirtyDatabase: reset is the way out of a dirty SQLite database, so it
// must empty and re-migrate one that golang-migrate's own down refuses to touch.
func TestMigrateResetRecoversADirtyDatabase(t *testing.T) {
	const migrations = "../../migrations"
	path := filepath.Join(t.TempDir(), "db", "lightstack.db")
	url := "sqlite://" + path

	if err := MigrateUp(url, migrations); err != nil {
		t.Fatalf("up: %v", err)
	}
	// Mark the database dirty, as an interrupted migration leaves it; m.Down() would refuse.
	func() {
		db, err := openSQLiteHandle(context.Background(), path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("UPDATE schema_migrations SET dirty = 1"); err != nil {
			t.Fatal(err)
		}
	}()
	if status, err := GetMigrationStatus(url, migrations); err != nil || !status.Dirty {
		t.Fatalf("status before reset = %+v, %v; want dirty", status, err)
	}

	if err := MigrateReset(url, migrations); err != nil {
		t.Fatalf("reset of a dirty database: %v", err)
	}
	after, err := GetMigrationStatus(url, migrations)
	if err != nil || after.Dirty {
		t.Fatalf("status after reset = %+v, %v; want clean", after, err)
	}
	if tables := sqliteTables(t, path); len(tables) < 50 {
		t.Fatalf("%d tables after reset; want the whole schema", len(tables))
	}
}

// TestMigrateResetCreatesAMissingDatabase: reset is create-if-missing, so emptying a file that is
// not there is not an error — the file is created and migrated up.
func TestMigrateResetCreatesAMissingDatabase(t *testing.T) {
	const migrations = "../../migrations"
	path := filepath.Join(t.TempDir(), "db", "lightstack.db")
	url := "sqlite://" + path

	if err := MigrateReset(url, migrations); err != nil {
		t.Fatalf("reset of a missing database: %v", err)
	}
	if tables := sqliteTables(t, path); len(tables) < 50 {
		t.Fatalf("%d tables after reset; want the whole schema", len(tables))
	}
}

// TestMigrateUpRefusesANewerDatabase pins SPEC-030 FR-008 for a restored backup: a database
// written by a newer Lightstack carries a version this binary has no migration for. It must be
// refused — golang-migrate already stops on it — and the refusal must say why, naming both
// versions, instead of "no migration found for version N: read down …". check, the gate a restore
// runs before the start, refuses it the same way: it cannot know the schema a newer one wrote.
func TestMigrateUpRefusesANewerDatabase(t *testing.T) {
	const migrations = "../../migrations"
	url := "sqlite://" + filepath.Join(t.TempDir(), "db", "lightstack.db")
	if err := MigrateUp(url, migrations); err != nil {
		t.Fatalf("up: %v", err)
	}
	st, err := GetMigrationStatus(url, migrations)
	if err != nil {
		t.Fatal(err)
	}
	latest := st.Version
	if err := MigrateUp(url, migrations); err != nil {
		t.Fatalf("up on a current database: %v; want no change", err)
	}

	// IntegrityCheck's "is this a Lightstack database" gate (schemaVersion) reads the real
	// schema_migrations table golang-migrate wrote, not a hand-built mimic: a freshly migrated
	// database passes and reports its version.
	if v, err := IntegrityCheck(context.Background(), url, migrations); err != nil || v != uint64(latest) {
		t.Fatalf("IntegrityCheck on a freshly migrated database = %d, %v; want version %d", v, err, latest)
	}

	if err := ForceMigrationVersion(url, migrations, int(latest)+1); err != nil {
		t.Fatalf("force %d: %v", latest+1, err)
	}
	for name, err := range map[string]error{
		"up":    MigrateUp(url, migrations),
		"check": checkErr(IntegrityCheck(context.Background(), url, migrations)),
	} {
		if err == nil {
			t.Fatalf("%s on a database newer than this binary succeeded", name)
		}
		for _, want := range []string{fmt.Sprint(latest + 1), fmt.Sprint(latest), "newer Lightstack"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s refusal %q does not mention %q", name, err, want)
			}
		}
	}
}
