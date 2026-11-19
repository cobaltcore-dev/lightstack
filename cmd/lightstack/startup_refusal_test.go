package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// binDir holds the lightstack and lightstack-migrate binaries TestMain builds, for the
// tests that run an entry point the way an operator does.
var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lightstack-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, pkg := range map[string]string{"lightstack": ".", "lightstack-migrate": "../lightstack-migrate"} {
		build := exec.Command("go", "build", "-o", filepath.Join(dir, name), pkg)
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "building %s: %v\n", name, err)
			_ = os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	binDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// entryPoint is one way an operator starts something that opens the database.
type entryPoint struct {
	name string
	bin  string
	args func(config string) []string
	exit int // on a refused configuration
}

var entryPoints = []entryPoint{
	{"server", "lightstack", func(c string) []string { return []string{"--config", c, "--migrations", "../../migrations"} }, 1},
	{"cleanup", "lightstack", func(c string) []string { return []string{"cleanup", "--config", c, "--dry-run"} }, 2},
	{"console-tokens", "lightstack", func(c string) []string { return []string{"console-tokens", "--config", c, "list"} }, 2},
	{"lightstack-migrate", "lightstack-migrate", func(c string) []string {
		return []string{"up", "-config", c, "-migrations", "../../migrations"}
	}, 1},
}

// outcome is what run observed: the exit code, stderr, both streams combined (the structured log
// goes to stdout) and how long the process took.
type outcome struct {
	code           int
	stderr, output string
	elapsed        time.Duration
}

// run starts ep with a configuration of database.url and extra YAML, and env on top of an
// environment without any LIGHTSTACK_ variable, so nothing overrides the file.
func run(t *testing.T, ep entryPoint, url, extra string, env map[string]string) outcome {
	t.Helper()
	config := writeConfig(t, url, extra)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, ep.bin), ep.args(config)...)
	cmd.Env = scrubbedEnv(env)
	var errBuf, outBuf bytes.Buffer
	cmd.Stderr = &errBuf
	cmd.Stdout = &outBuf
	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		t.Fatalf("%s %v did not exit within 10s; stderr: %s", ep.bin, ep.args(config), errBuf.String())
	}
	return outcome{exitCode(t, err), errBuf.String(), outBuf.String() + errBuf.String(), elapsed}
}

// writeConfig writes a lightstack.yaml with database.url and extra YAML into a temp dir.
func writeConfig(t *testing.T, url, extra string) string {
	t.Helper()
	config := filepath.Join(t.TempDir(), "lightstack.yaml")
	yaml := fmt.Sprintf("database:\n  url: %q\nlogging:\n  level: info\n  format: json\n%s", url, extra)
	if err := os.WriteFile(config, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

// scrubbedEnv is the current environment with every LIGHTSTACK_ variable removed and env added on
// top, so the configuration file alone decides the run.
func scrubbedEnv(env map[string]string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LIGHTSTACK_") {
			out = append(out, kv)
		}
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// exitCode is the process's exit code, failing the test for a non-exit error (the binary could not
// be run at all).
func exitCode(t *testing.T, err error) int {
	t.Helper()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		t.Fatalf("running the binary: %v", err)
		return 0
	}
}

// TestStartupRefusals pins FR-001, FR-002, FR-004, FR-005 and SC-003: every entry point
// that opens the database refuses a bad configuration within a second, with its own
// exit code and the contract's message, before it creates any file or directory.
func TestStartupRefusals(t *testing.T) {
	const accepted = "accepted schemes: postgres, postgresql, sqlite"
	for _, tc := range []struct {
		name       string
		url        func(watch string) string
		extra      string
		env        map[string]string
		msg        string
		serverOnly bool
	}{
		{name: "empty URL", url: func(string) string { return "" },
			msg: "database.url is empty; " + accepted},
		{name: "keyword/value string", url: func(string) string { return "host=db user=lightstack" },
			msg: "database.url has no scheme; " + accepted},
		{name: "unsupported scheme", url: func(string) string { return "mysql://db/lightstack" },
			msg: `database.url scheme "mysql" is not supported; ` + accepted},
		{name: "relative SQLite path", url: func(string) string { return "sqlite://watch/x.db" },
			msg: "is not a valid SQLite URL; expected sqlite:///absolute/path/lightstack.db"},
		{name: "SQLite URL with a query", url: func(w string) string { return "sqlite://" + w + "/db/x.db?mode=ro" },
			msg: "is not a valid SQLite URL; expected sqlite:///absolute/path/lightstack.db"},
		{name: "declared backend differs", url: func(w string) string { return "sqlite://" + w + "/db/x.db" },
			env: map[string]string{"LIGHTSTACK_DB_BACKEND": "postgres"},
			msg: "LIGHTSTACK_DB_BACKEND=postgres but database.url selects sqlite"},
		{name: "declared backend invalid", url: func(string) string { return "postgres://db/lightstack" },
			env: map[string]string{"LIGHTSTACK_DB_BACKEND": "mysql"},
			msg: `LIGHTSTACK_DB_BACKEND="mysql" is not valid; accepted values: postgres, sqlite`},
		{name: "SQLite with VXLAN", url: func(w string) string { return "sqlite://" + w + "/db/x.db" },
			extra: "neutron:\n  vxlan_enabled: true\n", serverOnly: true,
			msg: "database.url selects sqlite, but neutron.vxlan_enabled is true; SQLite serves a single node only"},
		{name: "SQLite inside a storage tree", url: func(w string) string { return "sqlite://" + filepath.Join(w, "state", "images", "control", "x.db") },
			msg: "lies inside the node-local storage directory"},
	} {
		for _, ep := range entryPoints {
			if tc.serverOnly && ep.name != "server" {
				continue
			}
			t.Run(tc.name+"/"+ep.name, func(t *testing.T) {
				watch := filepath.Join(t.TempDir(), "watch")
				extra := tc.extra + fmt.Sprintf("state_path: %q\n", filepath.Join(watch, "state"))
				out := run(t, ep, tc.url(watch), extra, tc.env)
				if out.code != ep.exit {
					t.Errorf("exit code = %d; want %d. stderr: %s", out.code, ep.exit, out.stderr)
				}
				if !strings.Contains(out.stderr, tc.msg) {
					t.Errorf("stderr does not name the problem %q:\n%s", tc.msg, out.stderr)
				}
				if out.elapsed > time.Second {
					t.Errorf("refused after %v; want under 1s", out.elapsed)
				}
				if _, err := os.Stat(watch); !os.IsNotExist(err) {
					t.Errorf("the refusal created %s (stat: %v); a refused configuration must leave nothing behind", watch, err)
				}
			})
		}
	}
}

// TestMissingSQLiteFile pins FR-012 for the entry points that must not create the
// database: cleanup treats a missing file like an unreachable PostgreSQL — it warns
// naming the path and exits 0 — and console-tokens exits 1 naming it. Neither creates it.
func TestMissingSQLiteFile(t *testing.T) {
	for _, tc := range []struct {
		ep   entryPoint
		exit int
		want string
	}{
		{entryPoints[1], 0, "database unreachable"},
		{entryPoints[2], 1, "failed to connect to the database"},
	} {
		t.Run(tc.ep.name, func(t *testing.T) {
			watch := filepath.Join(t.TempDir(), "watch")
			file := filepath.Join(watch, "db", "lightstack.db")
			extra := fmt.Sprintf("state_path: %q\n", filepath.Join(t.TempDir(), "state"))
			out := run(t, tc.ep, "sqlite://"+file, extra, nil)
			if out.code != tc.exit {
				t.Errorf("exit code = %d; want %d. output: %s", out.code, tc.exit, out.output)
			}
			if !strings.Contains(out.output, tc.want) || !strings.Contains(out.output, file) {
				t.Errorf("output does not say %q naming %s:\n%s", tc.want, file, out.output)
			}
			if _, err := os.Stat(watch); !os.IsNotExist(err) {
				t.Errorf("%s created %s (stat: %v); it must never create the database", tc.ep.name, watch, err)
			}
		})
	}
}

// TestUnsupportedLibvirtURIRefused pins SPEC-031 FR-009: in real mode, a libvirt_uri Lightstack
// cannot honour stops every entry point that loads the configuration, naming the value and the
// forms it accepts, before the database is opened. lightstack-migrate never reads libvirt.
func TestUnsupportedLibvirtURIRefused(t *testing.T) {
	const uri = "qemu+tcp://h/system"
	for _, ep := range entryPoints {
		if ep.name == "lightstack-migrate" {
			continue
		}
		t.Run(ep.name, func(t *testing.T) {
			watch := filepath.Join(t.TempDir(), "watch")
			// Real mode allows a state_path of 50 characters at most; t.TempDir's are longer.
			state, err := os.MkdirTemp("", "ls")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(state) })
			extra := fmt.Sprintf("state_path: %q\nnova:\n  libvirt_mode: real\n  libvirt_uri: %q\n", state, uri)
			out := run(t, ep, "sqlite://"+filepath.Join(watch, "db", "x.db"), extra, nil)
			if out.code != ep.exit {
				t.Errorf("exit code = %d; want %d. stderr: %s", out.code, ep.exit, out.stderr)
			}
			if !strings.Contains(out.stderr, uri) || !strings.Contains(out.stderr, "accepted") {
				t.Errorf("stderr does not name %s and the accepted forms:\n%s", uri, out.stderr)
			}
			if _, err := os.Stat(watch); !os.IsNotExist(err) {
				t.Errorf("the refusal created %s (stat: %v); it must come at configuration load, before the database is opened", watch, err)
			}
		})
	}
}

// databaseEntryPoints are the entry points that migrate the database before anything else.
func databaseEntryPoints(t *testing.T) []entryPoint {
	t.Helper()
	var out []entryPoint
	for _, ep := range entryPoints {
		if ep.name == "server" || ep.name == "lightstack-migrate" {
			out = append(out, ep)
		}
	}
	if len(out) != 2 {
		t.Fatalf("found %d of the 2 migrating entry points", len(out))
	}
	return out
}

// sqliteWithVersion writes a SQLite database whose only content is golang-migrate's record of
// version, as a database migrated before the squash records it.
func sqliteWithVersion(t *testing.T, version uint64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db", "lightstack.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := database.Open(ctx, "sqlite://"+path, database.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE TABLE schema_migrations (version uint64, dirty bool)",
		fmt.Sprintf("INSERT INTO schema_migrations (version, dirty) VALUES (%d, 0)", version),
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return "sqlite://" + path
}

// TestStartupRefusesAPreSquashDatabase pins SPEC-032 FR-011 at both migrating entry points: a
// database migrated before the migration squash is refused with the reason and the way out, never
// as "written by a newer Lightstack", and nothing migrates it.
func TestStartupRefusesAPreSquashDatabase(t *testing.T) {
	for _, ep := range databaseEntryPoints(t) {
		t.Run(ep.name, func(t *testing.T) {
			url := sqliteWithVersion(t, 82)
			extra := fmt.Sprintf("state_path: %q\n", filepath.Join(t.TempDir(), "state"))
			out := run(t, ep, url, extra, nil)
			if out.code != 1 {
				t.Errorf("exit code = %d; want 1. stderr: %s", out.code, out.stderr)
			}
			if !strings.Contains(out.stderr, "from before the migration squash") {
				t.Errorf("stderr does not say the database predates the squash:\n%s", out.stderr)
			}
			if strings.Contains(out.stderr, "newer Lightstack") {
				t.Errorf("a pre-squash database is reported as written by a newer Lightstack:\n%s", out.stderr)
			}
		})
	}
}

// migrateCommand is lightstack-migrate running args, its options before the positional ones.
func migrateCommand(args ...string) entryPoint {
	return entryPoint{"lightstack-migrate " + strings.Join(args, " "), "lightstack-migrate", func(c string) []string {
		return append([]string{args[0], "-config", c, "-migrations", "../../migrations"}, args[1:]...)
	}, 1}
}

// TestMigrateCommandsOnAPreSquashDatabase pins SPEC-032 FR-011 and FR-022 for the commands that
// report or prompt: status prints the state and the message; down, goto and force refuse before
// they prompt, so nobody is asked to confirm what cannot run.
func TestMigrateCommandsOnAPreSquashDatabase(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string // in the combined output
	}{
		{[]string{"status"}, []string{"Current Version: 82", "Status: PRE-SQUASH", "from before the migration squash"}},
		{[]string{"down"}, []string{"from before the migration squash"}},
		{[]string{"goto", "1"}, []string{"from before the migration squash"}},
		{[]string{"force", "1"}, []string{"from before the migration squash"}},
	} {
		ep := migrateCommand(tc.args...)
		t.Run(ep.name, func(t *testing.T) {
			url := sqliteWithVersion(t, 82)
			extra := fmt.Sprintf("state_path: %q\n", filepath.Join(t.TempDir(), "state"))
			out := run(t, ep, url, extra, nil)
			if out.code != 1 {
				t.Errorf("exit code = %d; want 1. output:\n%s", out.code, out.output)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.output, w) {
					t.Errorf("output does not contain %q:\n%s", w, out.output)
				}
			}
			if strings.Contains(out.output, "(yes/no)") {
				t.Errorf("prompted before refusing:\n%s", out.output)
			}
		})
	}
}

// TestStatusNamesTheMigration pins SPEC-032 FR-022 at the command: status names what the recorded
// version is.
func TestStatusNamesTheMigration(t *testing.T) {
	url := "sqlite://" + filepath.Join(t.TempDir(), "db", "lightstack.db")
	extra := fmt.Sprintf("state_path: %q\n", filepath.Join(t.TempDir(), "state"))
	if out := run(t, migrateCommand("up"), url, extra, nil); out.code != 0 {
		t.Fatalf("up: exit %d:\n%s", out.code, out.output)
	}
	out := run(t, migrateCommand("status"), url, extra, nil)
	want := "Migration: baseline"
	if out.code != 0 || !strings.Contains(out.output, want) {
		t.Fatalf("status: exit %d; want %q in:\n%s", out.code, want, out.output)
	}
}

// TestStartupRefusesAStaleDatabase pins SPEC-032 FR-020 at both migrating entry points: a database
// that applied an earlier state of the open release migration is refused, untouched.
func TestStartupRefusesAStaleDatabase(t *testing.T) {
	migrations := filepath.Join(t.TempDir(), "migrations")
	if err := os.CopyFS(migrations, os.DirFS("../../migrations")); err != nil {
		t.Fatal(err)
	}
	fragment := filepath.Join(migrations, "next", "20990101000000-1-stale-demo")
	for part, sql := range map[string]string{"up": "CREATE TABLE stale_demo (id TEXT);\n", "down": "DROP TABLE stale_demo;\n"} {
		if err := os.WriteFile(fragment+"."+part+".sql", []byte(sql), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	withMigrations := func(name, bin string, args func(c string) []string) entryPoint {
		return entryPoint{name, bin, args, 1}
	}
	server := withMigrations("server", "lightstack", func(c string) []string { return []string{"--config", c, "--migrations", migrations} })
	migrate := withMigrations("lightstack-migrate", "lightstack-migrate", func(c string) []string {
		return []string{"up", "-config", c, "-migrations", migrations}
	})
	url := "sqlite://" + filepath.Join(t.TempDir(), "db", "lightstack.db")
	extra := fmt.Sprintf("state_path: %q\n", filepath.Join(t.TempDir(), "state"))
	if out := run(t, migrate, url, extra, nil); out.code != 0 {
		t.Fatalf("migrate to the open release migration: exit %d:\n%s", out.code, out.output)
	}
	if err := os.WriteFile(fragment+".up.sql", []byte("CREATE TABLE stale_demo (id TEXT);\nCREATE TABLE stale_more (id TEXT);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []entryPoint{server, migrate} {
		t.Run(ep.name, func(t *testing.T) {
			out := run(t, ep, url, extra, nil)
			if out.code != 1 || !strings.Contains(out.stderr, "applied an earlier state of the open release migration") {
				t.Errorf("exit %d; want 1 and the stale refusal on stderr:\n%s", out.code, out.output)
			}
		})
	}

	// status reports it; down and goto refuse before they prompt; force, the repair tool for a
	// dirty version, still prompts (contracts/cli-and-startup.md).
	command := func(args ...string) entryPoint {
		return withMigrations("lightstack-migrate "+strings.Join(args, " "), "lightstack-migrate", func(c string) []string {
			return append([]string{args[0], "-config", c, "-migrations", migrations}, args[1:]...)
		})
	}
	for _, tc := range []struct {
		args   []string
		code   int
		want   string
		prompt bool
	}{
		{[]string{"status"}, 1, "Status: STALE", false},
		{[]string{"down"}, 1, "applied an earlier state of the open release migration", false},
		{[]string{"goto", "1"}, 1, "applied an earlier state of the open release migration", false},
		{[]string{"force", "2"}, 0, "Force cancelled", true},
	} {
		ep := command(tc.args...)
		t.Run(ep.name, func(t *testing.T) {
			out := run(t, ep, url, extra, nil)
			if out.code != tc.code || !strings.Contains(out.output, tc.want) || strings.Contains(out.output, "(yes/no)") != tc.prompt {
				t.Errorf("exit %d; want %d, %q, prompt %v:\n%s", out.code, tc.code, tc.want, tc.prompt, out.output)
			}
		})
	}
}

// TestStatusOnAMissingSQLiteFile pins status on a database file that does not exist yet: still an
// error and still nothing created (SPEC-028: a mistyped path is an error), but reported as the
// status NO DATABASE rather than as a failure (SPEC-032 FR-022).
func TestStatusOnAMissingSQLiteFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	path := filepath.Join(dir, "lightstack.db")
	extra := fmt.Sprintf("state_path: %q\n", filepath.Join(t.TempDir(), "state"))
	out := run(t, migrateCommand("status"), "sqlite://"+path, extra, nil)
	if out.code != 1 || !strings.Contains(out.output, "Status: NO DATABASE") || !strings.Contains(out.output, path) {
		t.Errorf("exit %d; want 1, Status: NO DATABASE and the path:\n%s", out.code, out.output)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("status created %s (stat: %v)", dir, err)
	}
}
