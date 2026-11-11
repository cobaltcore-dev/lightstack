package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// TestValidateMigrations pins contracts/migrations.md: each backend's view must pair
// every up with a down, hold exactly one SQLite baseline and no version twice.
func TestValidateMigrations(t *testing.T) {
	valid := map[string]string{
		"001_init.up.sql":              "CREATE TABLE a (id TEXT);",
		"001_init.down.sql":            "DROP TABLE a;",
		"002_more.up.sql":              "CREATE TABLE b (id TEXT);",
		"002_more.down.sql":            "DROP TABLE b;",
		"002_baseline.sqlite.up.sql":   "CREATE TABLE a (id TEXT); CREATE TABLE b (id TEXT);",
		"002_baseline.sqlite.down.sql": "DROP TABLE b; DROP TABLE a;",
		"003_x.up.sql":                 "ALTER TABLE a ADD COLUMN x TEXT;",
		"003_x.down.sql":               "ALTER TABLE a DROP COLUMN x;",
		"README.md":                    "not a migration",
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
		{"a PostgreSQL down file missing", with(nil, "001_init.down.sql"), "postgres view: 001_init.up.sql has no down migration"},
		{"a SQLite down file missing", with(nil, "002_baseline.sqlite.down.sql"), "sqlite view: 002_baseline.sqlite.up.sql has no down migration"},
		{"a down file shared by both views missing", with(nil, "003_x.down.sql"), "has no down migration"},
		{"no SQLite baseline", with(nil, "002_baseline.sqlite.up.sql", "002_baseline.sqlite.down.sql"), "no SQLite baseline"},
		{"two SQLite baselines", with(map[string]string{
			"003_baseline.sqlite.up.sql": "", "003_baseline.sqlite.down.sql": "",
		}), "two SQLite baselines"},
		{"a version twice after the rules", with(map[string]string{
			"003_y.sqlite.up.sql": "", "003_y.sqlite.down.sql": "", "003_z.sqlite.up.sql": "", "003_z.sqlite.down.sql": "",
		}), "migrations for version 3 in the sqlite view"},
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

// TestShippedMigrationsValidate pins the real migrations/ folder, not a synthetic one: a
// pairing mistake there stops the default container, which runs lightstack-migrate before the
// server, and this fails the build first.
func TestShippedMigrationsValidate(t *testing.T) {
	if err := ValidateMigrations("../../migrations"); err != nil {
		t.Fatal(err)
	}
}

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

// SQLite starts at its baseline: a version below it cannot be reached or forced, and
// down and reset go through the baseline's down file to an empty database.
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
		"force":  func() error { return ForceMigrationVersion(url, migrations, 74) },
	}
	for name, run := range checks {
		if err := run(); err == nil {
			t.Errorf("%s on a missing file succeeded; want an error", name)
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("%s created %s (stat: %v); must-exist commands create nothing", name, dir, statErr)
		}
	}
}

func TestSQLiteMigrationsStartAtTheBaseline(t *testing.T) {
	const migrations = "../../migrations"
	path := filepath.Join(t.TempDir(), "db", "lightstack.db")
	url := "sqlite://" + path

	for name, run := range map[string]func() error{
		"goto":  func() error { return MigrateToVersion(url, migrations, 50) },
		"force": func() error { return ForceMigrationVersion(url, migrations, 50) },
	} {
		err := run()
		if err == nil || err.Error() != "SQLite starts at its baseline 073; version 50 is below it" {
			t.Errorf("%s 50 = %v; want the below-baseline refusal", name, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a refused goto created the database (stat: %v)", err)
	}

	if err := MigrateUp(url, migrations); err != nil {
		t.Fatalf("up: %v", err)
	}
	newest, err := GetMigrationStatus(url, migrations)
	if err != nil || newest.Version < 74 {
		t.Fatalf("version after up = %+v, %v; want 074 or later", newest, err)
	}
	for v := newest.Version; v >= 73; v-- {
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
