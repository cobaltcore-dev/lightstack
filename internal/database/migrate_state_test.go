package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The classification every entry point runs before golang-migrate (SPEC-032 research R9,
// data-model §3), and the refusal of a database migrated before the squash (FR-011, FR-012).

// preSquashDatabase migrates a fresh database on the selected backend with the pre-squash history
// to version (0: the squash point). SQLite's history begins at its 073 baseline.
func preSquashDatabase(t *testing.T, name string, version uint64) (string, Backend) {
	t.Helper()
	url, backend := freshDatabase(t, name)
	src := os.DirFS(preSquashDir)
	if backend == SQLite {
		src = dropBelow(src, 73)
	}
	migrateFolderNoChecks(t, url, src, version)
	return url, backend
}

// execSQL runs statements on the database at url, outside any migrator.
func execSQL(t *testing.T, url string, backend Backend, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	if backend == Postgres {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		for _, s := range stmts {
			if _, err := conn.Exec(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		return
	}
	db := openSQLiteFile(t, ctx, strings.TrimPrefix(url, "sqlite://"))
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	_ = db.Close()
}

// setMigrationVersion rewrites golang-migrate's record of the database's version.
func setMigrationVersion(t *testing.T, url string, backend Backend, version uint64, dirty bool) {
	t.Helper()
	d := map[bool]string{true: "true", false: "false"}[dirty]
	if backend == SQLite {
		d = map[bool]string{true: "1", false: "0"}[dirty]
	}
	execSQL(t, url, backend, fmt.Sprintf("UPDATE schema_migrations SET version = %d, dirty = %s", version, d))
}

func inspectState(t *testing.T, url string) (DatabaseState, uint64, bool) {
	t.Helper()
	in, err := inspectDatabase(url, "../../migrations")
	if err != nil {
		t.Fatalf("inspectDatabase: %v", err)
	}
	return in.state, in.version, in.dirty
}

func assertPreSquashRefusal(t *testing.T, name string, err error) {
	t.Helper()
	switch {
	case err == nil:
		t.Errorf("%s accepted a pre-squash database", name)
	case !IsPreSquash(err):
		t.Errorf("%s: %v; want a pre-squash refusal", name, err)
	case !strings.Contains(err.Error(), "from before the migration squash"):
		t.Errorf("%s: the refusal does not say why: %v", name, err)
	case strings.Contains(err.Error(), "newer Lightstack"), strings.Contains(err.Error(), "no migration found"):
		t.Errorf("%s: a pre-squash database misreported: %v", name, err)
	}
}

func TestDatabaseState(t *testing.T) {
	const migrations = "../../migrations"

	t.Run("fresh", func(t *testing.T) {
		url, _ := freshDatabase(t, "lightstack_state_fresh")
		if state, _, _ := inspectState(t, url); state != StateFresh {
			t.Fatalf("state = %v; want fresh", state)
		}
	})

	t.Run("fresh SQLite file not yet created", func(t *testing.T) {
		if _, backend := freshDatabase(t, "lightstack_state_probe"); backend != SQLite {
			t.Skip("SQLite only")
		}
		path := filepath.Join(t.TempDir(), "absent", "lightstack.db")
		url := "sqlite://" + path
		if state, _, _ := inspectState(t, url); state != StateFresh {
			t.Fatalf("state = %v; want fresh", state)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("inspecting a missing file created %s (stat: %v)", filepath.Dir(path), err)
		}
		if err := MigrateUp(url, migrations); err != nil {
			t.Fatalf("MigrateUp on a missing file: %v", err)
		}
		if state, version, _ := inspectState(t, url); state != StateCurrent || version != 1 {
			t.Fatalf("after MigrateUp: state %v at %d; want current at 1", state, version)
		}
	})

	t.Run("current", func(t *testing.T) {
		url, _ := freshDatabase(t, "lightstack_state_current")
		if err := MigrateUp(url, migrations); err != nil {
			t.Fatal(err)
		}
		if state, version, dirty := inspectState(t, url); state != StateCurrent || version != 1 || dirty {
			t.Fatalf("state %v at %d (dirty %v); want current at 1", state, version, dirty)
		}
		info, err := GetMigrationStatus(url, migrations)
		if err != nil || info.Version != 1 {
			t.Fatalf("GetMigrationStatus = %+v, %v; want current at 1", info, err)
		}
	})

	t.Run("newer", func(t *testing.T) {
		url, backend := freshDatabase(t, "lightstack_state_newer")
		if err := MigrateUp(url, migrations); err != nil {
			t.Fatal(err)
		}
		setMigrationVersion(t, url, backend, 7, false)
		if state, _, _ := inspectState(t, url); state != StateNewer {
			t.Fatalf("state = %v; want newer", state)
		}
		if err := MigrateUp(url, migrations); err == nil || !strings.Contains(err.Error(), "newer Lightstack") || IsPreSquash(err) {
			t.Fatalf("MigrateUp on a newer database = %v; want the newer-Lightstack refusal", err)
		}
	})

	t.Run("dirty", func(t *testing.T) {
		url, backend := freshDatabase(t, "lightstack_state_dirty")
		if err := MigrateUp(url, migrations); err != nil {
			t.Fatal(err)
		}
		setMigrationVersion(t, url, backend, 1, true)
		if state, _, dirty := inspectState(t, url); state != StateDirty || !dirty {
			t.Fatalf("state = %v; want dirty", state)
		}
		if err := MigrateUp(url, migrations); err == nil || IsPreSquash(err) {
			t.Fatalf("MigrateUp on a dirty post-squash database = %v; want golang-migrate's dirty refusal", err)
		}
	})

	t.Run("baseline in flight", func(t *testing.T) {
		url, backend := freshDatabase(t, "lightstack_state_inflight")
		ddl := "CREATE TABLE schema_migrations (version BIGINT NOT NULL PRIMARY KEY, dirty BOOLEAN NOT NULL)"
		ins := "INSERT INTO schema_migrations (version, dirty) VALUES (1, true)"
		if backend == SQLite {
			ddl = "CREATE TABLE schema_migrations (version uint64, dirty bool)"
			ins = "INSERT INTO schema_migrations (version, dirty) VALUES (1, 1)"
			// The node applying the baseline has created the file.
			if err := os.WriteFile(strings.TrimPrefix(url, "sqlite://"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		execSQL(t, url, backend, ddl, ins)
		if state, _, _ := inspectState(t, url); state != StateBaselineInFlight {
			t.Fatalf("state = %v; want baseline in flight", state)
		}
		if err := MigrateUp(url, migrations); err == nil || IsPreSquash(err) {
			t.Fatalf("MigrateUp while another node applies the baseline = %v; want golang-migrate's own answer", err)
		}
	})

	for _, tc := range []struct {
		name    string
		version uint64
		dirty   bool
	}{
		{"pre-squash at the squash point", squashPoint, false},
		{"pre-squash and dirty", squashPoint, true},
		{"pre-squash at a reused low number", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, backend := preSquashDatabase(t, "lightstack_state_presquash", 0)
			setMigrationVersion(t, url, backend, tc.version, tc.dirty)
			if state, version, _ := inspectState(t, url); state != StatePreSquash || version != tc.version {
				t.Fatalf("state %v at %d; want pre-squash at %d", state, version, tc.version)
			}
			info, statusErr := GetMigrationStatus(url, migrations)
			assertPreSquashRefusal(t, "status", statusErr)
			if info == nil || uint64(info.Version) != tc.version {
				t.Errorf("GetMigrationStatus info = %+v; want pre-squash at %d", info, tc.version)
			}
			for name, err := range map[string]error{
				"up":    MigrateUp(url, migrations),
				"down":  MigrateDown(url, migrations),
				"goto":  MigrateToVersion(url, migrations, 1),
				"force": ForceMigrationVersion(url, migrations, 1),
			} {
				assertPreSquashRefusal(t, name, err)
			}
			if backend == SQLite {
				_, err := IntegrityCheck(context.Background(), url, migrations)
				assertPreSquashRefusal(t, "check", err)
			}
			// Nothing above moved the version.
			if state, version, dirty := inspectState(t, url); state != StatePreSquash || version != tc.version || dirty != tc.dirty {
				t.Fatalf("after the refusals: state %v at %d (dirty %v); want it untouched", state, version, dirty)
			}
			// reset recreates the database, which is the way out.
			if err := MigrateReset(url, migrations); err != nil {
				t.Fatalf("MigrateReset on a pre-squash database: %v", err)
			}
			if state, version, _ := inspectState(t, url); state != StateCurrent || version != 1 {
				t.Fatalf("after reset: state %v at %d; want current at 1", state, version)
			}
		})
	}
}

// TestMigrationStatusKind pins FR-022: status says what the recorded version is — nothing yet, the
// baseline, a release migration, or the open release migration of a build between releases.
func TestMigrationStatusKind(t *testing.T) {
	url, backend := freshDatabase(t, "lightstack_status_kind")
	if backend == SQLite {
		// status opens an existing file only; an empty one is an empty database.
		if err := os.WriteFile(strings.TrimPrefix(url, "sqlite://"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	migrations, probe := migrationsWithProbeRelease(t)
	kind := func(step string) string {
		t.Helper()
		info, err := GetMigrationStatus(url, migrations)
		if err != nil {
			t.Fatalf("%s: GetMigrationStatus: %v", step, err)
		}
		return info.Kind
	}
	if got := kind("empty"); got != "none (empty database)" {
		t.Errorf("empty database: Kind %q", got)
	}
	if err := MigrateToVersion(url, migrations, 1); err != nil {
		t.Fatal(err)
	}
	if got := kind("baseline"); got != "baseline" {
		t.Errorf("at 001: Kind %q; want baseline", got)
	}
	if err := MigrateUp(url, migrations); err != nil {
		t.Fatal(err)
	}
	if got := kind("release"); got != "release v0.0.0-probe" {
		t.Errorf("at the probe release %03d: Kind %q; want release v0.0.0-probe", probe, got)
	}
	writeFiles(t, migrations, map[string]string{
		"next/20990101000000-1-kind-demo.up.sql":   "CREATE TABLE kind_demo (id TEXT);\n",
		"next/20990101000000-1-kind-demo.down.sql": "DROP TABLE kind_demo;\n",
	})
	if err := MigrateUp(url, migrations); err != nil {
		t.Fatal(err)
	}
	if got, want := kind("open"), fmt.Sprintf("open release migration %03d (unreleased)", probe+1); got != want {
		t.Errorf("at the open migration: Kind %q; want %q", got, want)
	}
}

// staleFragment is the fixture fragment of TestStaleOpenMigration, without its direction.
const staleFragment = "next/20990101000000-1-stale-demo"

// openMigrationFixture is a copy of the migrations folder with one fragment, and a fresh database
// on the selected backend migrated to its open release migration, 002.
func openMigrationFixture(t *testing.T, name string) (dir, url string, backend Backend) {
	t.Helper()
	dir, _ = copyMigrationFiles(t)
	writeFiles(t, dir, map[string]string{
		staleFragment + ".up.sql":   "CREATE TABLE stale_demo (id TEXT);\n",
		staleFragment + ".down.sql": "DROP TABLE stale_demo;\n",
	})
	url, backend = freshDatabase(t, name)
	if err := MigrateUp(url, dir); err != nil {
		t.Fatalf("migrate to the open release migration: %v", err)
	}
	return dir, url, backend
}

// freezeAs writes what the open release migration of src assembles to as the release migration
// 002_<release>, changing only the first line of each file — as the release step freezes it — and
// removes every fragment.
func freezeAs(t *testing.T, dir, release string) {
	t.Helper()
	open, err := assembleOpen(os.DirFS(dir), Postgres)
	if err != nil || open == nil {
		t.Fatalf("assemble: %v, %v", open, err)
	}
	header := fmt.Sprintf("-- release migration 002 (%s): frozen from migrations/next/ by lightstack-migrate freeze", release)
	for part, data := range map[string][]byte{"up": open.up, "down": open.down} {
		_, rest, _ := strings.Cut(string(data), "\n")
		writeFiles(t, dir, map[string]string{fmt.Sprintf("002_%s.%s.sql", release, part): header + "\n" + rest})
	}
	if err := os.RemoveAll(filepath.Join(dir, "next")); err != nil {
		t.Fatal(err)
	}
}

// assertStale checks every entry point that must refuse a stale database at version refuses it,
// naming the cause, and that none of them moved its version (FR-020).
func assertStale(t *testing.T, url, dir string, backend Backend, version uint64, want string) {
	t.Helper()
	info, statusErr := GetMigrationStatus(url, dir)
	if !IsStale(statusErr) || !strings.Contains(statusErr.Error(), want) {
		t.Errorf("status: %v; want a stale refusal containing %q", statusErr, want)
	}
	if info == nil || uint64(info.Version) != version {
		t.Errorf("status info = %+v; want version %d", info, version)
	}
	checks := map[string]error{
		"up":   MigrateUp(url, dir),
		"down": MigrateDown(url, dir),
		"goto": MigrateToVersion(url, dir, 1),
	}
	if backend == SQLite {
		_, checks["check"] = IntegrityCheck(context.Background(), url, dir)
	}
	for name, err := range checks {
		if !IsStale(err) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v; want a stale refusal containing %q", name, err, want)
		}
	}
	if state, got, _ := inspectState(t, url); got != version {
		t.Errorf("after the refusals: %v at %d; want the version left at %d", state, got, version)
	}
}

// TestStaleOpenMigration pins SPEC-032 FR-019–FR-021 on the selected backend: a database that applied
// an earlier state of the open release migration, or of a migration since frozen differently, is
// refused with the cause; the same content frozen as a release carries on.
func TestStaleOpenMigration(t *testing.T) {
	const openStale = "applied an earlier state of the open release migration 002"

	t.Run("unchanged fragments are no false positive", func(t *testing.T) {
		dir, url, _ := openMigrationFixture(t, "lightstack_stale_same")
		if err := MigrateUp(url, dir); err != nil {
			t.Fatalf("MigrateUp again: %v", err)
		}
		if info, err := GetMigrationStatus(url, dir); err != nil || info.Version != 2 {
			t.Fatalf("status = %+v, %v; want current at 2", info, err)
		}
	})

	t.Run("a fragment edited", func(t *testing.T) {
		dir, url, backend := openMigrationFixture(t, "lightstack_stale_edit")
		writeFiles(t, dir, map[string]string{staleFragment + ".up.sql": "CREATE TABLE stale_demo (id TEXT);\nCREATE TABLE stale_more (id TEXT);\n"})
		assertStale(t, url, dir, backend, 2, openStale)
	})

	t.Run("a fragment added", func(t *testing.T) {
		dir, url, backend := openMigrationFixture(t, "lightstack_stale_add")
		writeFiles(t, dir, map[string]string{
			"next/20990101000001-2-stale-more.up.sql":   "CREATE TABLE stale_more (id TEXT);\n",
			"next/20990101000001-2-stale-more.down.sql": "DROP TABLE stale_more;\n",
		})
		assertStale(t, url, dir, backend, 2, openStale)
	})

	t.Run("the only fragment removed", func(t *testing.T) {
		dir, url, _ := openMigrationFixture(t, "lightstack_stale_removed")
		if err := os.RemoveAll(filepath.Join(dir, "next")); err != nil {
			t.Fatal(err)
		}
		if err := MigrateUp(url, dir); err == nil || !strings.Contains(err.Error(), "newer Lightstack") || IsStale(err) {
			t.Fatalf("MigrateUp = %v; want the newer-Lightstack refusal", err)
		}
	})

	t.Run("frozen with the same content", func(t *testing.T) {
		dir, url, _ := openMigrationFixture(t, "lightstack_stale_frozen")
		freezeAs(t, dir, "v0.7.0")
		if err := MigrateUp(url, dir); err != nil {
			t.Fatalf("MigrateUp after the freeze (FR-021): %v", err)
		}
		if info, err := GetMigrationStatus(url, dir); err != nil || info.Kind != "release v0.7.0" {
			t.Fatalf("status = %+v, %v; want current at release v0.7.0", info, err)
		}
	})

	t.Run("frozen with different content", func(t *testing.T) {
		dir, url, backend := openMigrationFixture(t, "lightstack_stale_frozen_diff")
		writeFiles(t, dir, map[string]string{
			"next/20990101000001-2-stale-more.up.sql":   "CREATE TABLE stale_more (id TEXT);\n",
			"next/20990101000001-2-stale-more.down.sql": "DROP TABLE stale_more;\n",
		})
		freezeAs(t, dir, "v0.7.0")
		assertStale(t, url, dir, backend, 2, "applied an earlier state of migration 002, which has since been frozen as release v0.7.0 with different content")
	})

	t.Run("no record of what it applied", func(t *testing.T) {
		dir, url, backend := openMigrationFixture(t, "lightstack_stale_norecord")
		execSQL(t, url, backend, "DELETE FROM applied_migrations WHERE version = 2")
		assertStale(t, url, dir, backend, 2, "is at migration version 2 but holds no record of what it applied")
	})

	t.Run("the baseline changed since it was applied", func(t *testing.T) {
		dir, _ := copyMigrationFiles(t)
		url, backend := freshDatabase(t, "lightstack_stale_baseline")
		if err := MigrateUp(url, dir); err != nil {
			t.Fatalf("migrate to the baseline: %v", err)
		}
		// Regenerating the baseline rewrites its checksum header, which is what the view serves for
		// version 001; the database still records the checksum of the baseline it applied.
		name := filepath.Join(dir, fmt.Sprintf("001_baseline.%s.up.sql", backend))
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		header, rest, _ := strings.Cut(string(data), "\n")
		_, body, _ := strings.Cut(rest, "\n")
		regenerated := header + "\n" + checksumPrefix + checksumBody([]byte("a regenerated baseline")) + "\n" + body
		if err := os.WriteFile(name, []byte(regenerated), 0o644); err != nil {
			t.Fatal(err)
		}
		assertStale(t, url, dir, backend, 1, "applied an earlier state of the baseline, migration 001")
	})

	t.Run("a dirty version is not stale", func(t *testing.T) {
		dir, url, backend := openMigrationFixture(t, "lightstack_stale_dirty")
		writeFiles(t, dir, map[string]string{staleFragment + ".up.sql": "CREATE TABLE stale_demo (id TEXT);\nSELECT 1;\n"})
		setMigrationVersion(t, url, backend, 2, true)
		if state, _, _ := inspectState(t, url); state != StateDirty {
			t.Errorf("state = %v; want dirty", state)
		}
		if err := MigrateUp(url, dir); err == nil || IsStale(err) || !strings.Contains(strings.ToLower(err.Error()), "dirty") {
			t.Fatalf("MigrateUp on a dirty database = %v; want golang-migrate's dirty refusal", err)
		}
	})

	t.Run("force stays allowed", func(t *testing.T) {
		dir, url, _ := openMigrationFixture(t, "lightstack_stale_force")
		writeFiles(t, dir, map[string]string{staleFragment + ".up.sql": "CREATE TABLE stale_demo (id TEXT);\nSELECT 1;\n"})
		if err := ForceMigrationVersion(url, dir, 2); err != nil {
			t.Fatalf("ForceMigrationVersion on a stale database: %v; force repairs a dirty version and stays allowed", err)
		}
	})
}
