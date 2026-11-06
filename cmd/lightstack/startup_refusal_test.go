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
