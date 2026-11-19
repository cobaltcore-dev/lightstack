package database

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// Fragments in migrations/next/ assemble into one open release migration per backend (SPEC-032
// FR-014–FR-017, data-model §7, contracts/migrations-folder.md). These tests are pure: the folder is
// an fstest.MapFS.

// releasedFolder is a folder at the squash with one release migration, 002_v0.7.0.
func releasedFolder() fstest.MapFS {
	return migrationsFS(
		"001_baseline.postgres.up.sql", "001_baseline.postgres.down.sql",
		"001_baseline.sqlite.up.sql", "001_baseline.sqlite.down.sql",
		"002_v0.7.0.up.sql", "002_v0.7.0.down.sql",
		"README.md",
	)
}

// withFragments adds next/ files to src, each holding its SQL.
func withFragments(src fstest.MapFS, files map[string]string) fstest.MapFS {
	for name, sql := range files {
		src["next/"+name] = &fstest.MapFile{Data: []byte(sql)}
	}
	return src
}

func TestParseFragment(t *testing.T) {
	for _, tc := range []struct {
		file string
		ok   bool
		want fragment
	}{
		{"20261118063000-596-add-volume-col.up.sql", true, fragment{file: "20261118063000-596-add-volume-col.up.sql", name: "20261118063000-596-add-volume-col", direction: "up"}},
		{"20261118063000-596-add-volume-col.postgres.down.sql", true, fragment{file: "20261118063000-596-add-volume-col.postgres.down.sql", name: "20261118063000-596-add-volume-col", backend: Postgres, direction: "down"}},
		{"20261118063000-1-x.sqlite.up.sql", true, fragment{file: "20261118063000-1-x.sqlite.up.sql", name: "20261118063000-1-x", backend: SQLite, direction: "up"}},
		{"2026111806300-596-short-time.up.sql", false, fragment{}},
		{"202611180630000-596-long-time.up.sql", false, fragment{}},
		{"20261118063000-ab-ticket.up.sql", false, fragment{}},
		{"20261118063000-596-Upper.up.sql", false, fragment{}},
		{"20261118063000-596-under_score.up.sql", false, fragment{}},
		{"20261118063000-596-trailing-.up.sql", false, fragment{}},
		{"20261118063000-596--double.up.sql", false, fragment{}},
		{"20261118063000-596.up.sql", false, fragment{}},
		{"20261118063000-596-x.mysql.up.sql", false, fragment{}},
		{"20261118063000-596-x.up.SQL", false, fragment{}},
		{"20261118063000-596-x.sql", false, fragment{}},
		{"005_add_x.up.sql", false, fragment{}},
	} {
		got, ok := parseFragment(tc.file)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseFragment(%q) = %+v, %v; want %+v, %v", tc.file, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReadFragmentsValidates(t *testing.T) {
	const grammar = ": a fragment name must be <UTC YYYYMMDDhhmmss>-<ticket>-<purpose>[.postgres|.sqlite].(up|down).sql"
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string // "" for valid
	}{
		{"valid, with a dot-file", map[string]string{
			".gitkeep":                    "",
			"20261118063000-596-a.up.sql": "", "20261118063000-596-a.down.sql": "",
			"20261118063001-597-b.postgres.up.sql": "", "20261118063001-597-b.postgres.down.sql": "",
			"20261118063001-597-b.sqlite.up.sql": "", "20261118063001-597-b.sqlite.down.sql": "",
		}, ""},
		{"a bad name", map[string]string{"add-column.up.sql": "", "add-column.down.sql": ""},
			"next/add-column.down.sql" + grammar},
		{"a stray file", map[string]string{"README.md": ""}, "next/README.md" + grammar},
		{"up without down", map[string]string{"20261118063000-596-a.up.sql": ""},
			"fragment next/20261118063000-596-a.up.sql has no down part"},
		{"down without up", map[string]string{"20261118063000-596-a.sqlite.down.sql": ""},
			"fragment next/20261118063000-596-a.sqlite.down.sql has no up part"},
		{"one name both plain and backend-specific", map[string]string{
			"20261118063000-596-a.up.sql": "", "20261118063000-596-a.down.sql": "",
			"20261118063000-596-a.postgres.up.sql": "", "20261118063000-596-a.postgres.down.sql": "",
		}, "fragment 20261118063000-596-a exists both plain and as .postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readFragments(withFragments(fstest.MapFS{}, tc.files))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("readFragments: %v", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Fatalf("readFragments = %v; want %q", err, tc.want)
			}
		})
	}

	t.Run("no next directory", func(t *testing.T) {
		if frags, err := readFragments(releasedFolder()); err != nil || len(frags) != 0 {
			t.Fatalf("readFragments without next/ = %v, %v; want none", frags, err)
		}
	})
}

func TestAssembleOpen(t *testing.T) {
	src := withFragments(releasedFolder(), map[string]string{
		".gitkeep":                                        "",
		"20261118063000-596-create-demo.up.sql":           "CREATE TABLE demo (id TEXT);\n",
		"20261118063000-596-create-demo.down.sql":         "DROP TABLE demo;\n",
		"20261118063001-597-demo-index.postgres.up.sql":   "CREATE INDEX demo_id ON demo USING btree (id);",
		"20261118063001-597-demo-index.postgres.down.sql": "DROP INDEX demo_id;",
		"20261118063001-597-demo-index.sqlite.up.sql":     "CREATE INDEX demo_id ON demo (id);\n",
		"20261118063001-597-demo-index.sqlite.down.sql":   "DROP INDEX demo_id;\n",
		"20261118063002-598-demo-note.up.sql":             "ALTER TABLE demo ADD COLUMN note TEXT;\n",
		"20261118063002-598-demo-note.down.sql":           "ALTER TABLE demo DROP COLUMN note;\n",
	})
	const header = "-- open release migration 003 (unreleased): assembled from migrations/next/\n"
	for backend, idx := range map[Backend][2]string{
		Postgres: {"CREATE INDEX demo_id ON demo USING btree (id);\n", "DROP INDEX demo_id;\n"},
		SQLite:   {"CREATE INDEX demo_id ON demo (id);\n", "DROP INDEX demo_id;\n"},
	} {
		t.Run(string(backend), func(t *testing.T) {
			open, err := assembleOpen(src, backend)
			if err != nil || open == nil {
				t.Fatalf("assembleOpen = %v, %v", open, err)
			}
			if open.version != 3 {
				t.Errorf("version = %d; want 3, one above 002_v0.7.0", open.version)
			}
			body := "-- fragment: 20261118063000-596-create-demo.up.sql\nCREATE TABLE demo (id TEXT);\n" +
				"-- fragment: 20261118063001-597-demo-index." + string(backend) + ".up.sql\n" + idx[0] +
				"-- fragment: 20261118063002-598-demo-note.up.sql\nALTER TABLE demo ADD COLUMN note TEXT;\n"
			sum := checksumBody([]byte(body))
			wantUp := header + "-- checksum: " + sum + "\n" + body +
				"INSERT INTO applied_migrations (version, checksum) VALUES (3, '" + sum + "');\n"
			if string(open.up) != wantUp {
				t.Errorf("up =\n%s\nwant\n%s", open.up, wantUp)
			}
			// The down part removes the record first, then undoes the fragments newest first.
			wantDown := header + "DELETE FROM applied_migrations WHERE version = 3;\n" +
				"-- fragment: 20261118063002-598-demo-note.down.sql\nALTER TABLE demo DROP COLUMN note;\n" +
				"-- fragment: 20261118063001-597-demo-index." + string(backend) + ".down.sql\n" + idx[1] +
				"-- fragment: 20261118063000-596-create-demo.down.sql\nDROP TABLE demo;\n"
			if string(open.down) != wantDown {
				t.Errorf("down =\n%s\nwant\n%s", open.down, wantDown)
			}
		})
	}

	t.Run("same creation time sorts by ticket, then purpose, on both backends", func(t *testing.T) {
		src := withFragments(releasedFolder(), map[string]string{
			"20261118063000-612-a.up.sql": "-- 612-a\n", "20261118063000-612-a.down.sql": "",
			"20261118063000-596-b.postgres.up.sql": "-- 596-b\n", "20261118063000-596-b.postgres.down.sql": "",
			"20261118063000-596-b.sqlite.up.sql": "-- 596-b\n", "20261118063000-596-b.sqlite.down.sql": "",
			"20261118063000-596-a.up.sql": "-- 596-a\n", "20261118063000-596-a.down.sql": "",
		})
		for _, backend := range []Backend{Postgres, SQLite} {
			open, err := assembleOpen(src, backend)
			if err != nil {
				t.Fatal(err)
			}
			up := string(open.up)
			a, b, c := strings.Index(up, "-- 596-a"), strings.Index(up, "-- 596-b"), strings.Index(up, "-- 612-a")
			if a < 0 || a >= b || b >= c {
				t.Errorf("%s order: 596-a at %d, 596-b at %d, 612-a at %d; want ascending", backend, a, b, c)
			}
		}
	})

	t.Run("one backend's fragment gives both backends the version", func(t *testing.T) {
		src := withFragments(releasedFolder(), map[string]string{
			"20261118063000-596-pg-only.postgres.up.sql":   "SELECT 1;\n",
			"20261118063000-596-pg-only.postgres.down.sql": "SELECT 1;\n",
		})
		open, err := assembleOpen(src, SQLite)
		if err != nil || open == nil {
			t.Fatalf("assembleOpen(SQLite) = %v, %v; want version 003 with an empty body", open, err)
		}
		if sum, err := parseChecksumHeader(open.up); open.version != 3 || err != nil || sum != checksumBody(nil) {
			t.Errorf("SQLite open migration: version %d, checksum %s (%v); want 3 and the empty body's", open.version, sum, err)
		}
	})

	t.Run("no fragments, no open migration", func(t *testing.T) {
		src := withFragments(releasedFolder(), map[string]string{".gitkeep": ""})
		if open, err := assembleOpen(src, Postgres); err != nil || open != nil {
			t.Fatalf("assembleOpen = %+v, %v; want none", open, err)
		}
	})
}

// TestMigrationViewServesTheOpenMigration pins FR-017: every reader of the folder reads it through
// the view, which serves the open release migration under a numbered name and never lists next/.
func TestMigrationViewServesTheOpenMigration(t *testing.T) {
	src := withFragments(releasedFolder(), map[string]string{
		"20261118063000-596-create-demo.up.sql":   "CREATE TABLE demo (id TEXT);\n",
		"20261118063000-596-create-demo.down.sql": "DROP TABLE demo;\n",
	})
	for _, backend := range []Backend{Postgres, SQLite} {
		t.Run(string(backend), func(t *testing.T) {
			v, err := newMigrationView(src, backend)
			if err != nil {
				t.Fatal(err)
			}
			names := viewNames(t, v)
			if !contains(names, "003_open.up.sql") || !contains(names, "003_open.down.sql") {
				t.Fatalf("view lists %v; want 003_open.up.sql and 003_open.down.sql", names)
			}
			for _, n := range names {
				if strings.HasPrefix(n, "next") {
					t.Errorf("view lists %s", n)
				}
			}
			if v.latestVersion() != 3 {
				t.Errorf("latestVersion = %d; want 3", v.latestVersion())
			}
			open, err := assembleOpen(src, backend)
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string][]byte{"003_open.up.sql": open.up, "003_open.down.sql": open.down} {
				got, err := fs.ReadFile(v, name)
				if err != nil || string(got) != string(want) {
					t.Errorf("ReadFile(%s) = %q, %v; want the assembled bytes", name, got, err)
				}
				info, err := fs.Stat(v, name)
				if err != nil || info.Size() != int64(len(want)) || info.IsDir() || info.Name() != name {
					t.Errorf("Stat(%s) = %v, %v", name, info, err)
				}
			}
			if sum, err := v.expectedChecksum(3); err != nil || sum != checksumBody(open.upBody) {
				t.Errorf("expectedChecksum(3) = %s, %v; want checksumBody(BODY) %s", sum, err, checksumBody(open.upBody))
			}
		})
	}

	t.Run("an invalid fragment fails the view", func(t *testing.T) {
		bad := withFragments(releasedFolder(), map[string]string{"20261118063000-596-x.up.sql": ""})
		if _, err := newMigrationView(bad, Postgres); err == nil || !strings.Contains(err.Error(), "has no down part") {
			t.Fatalf("newMigrationView with a fragment missing its down part = %v", err)
		}
	})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
