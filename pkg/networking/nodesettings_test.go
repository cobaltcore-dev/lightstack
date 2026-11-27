package networking

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// SPEC-033 round 2, T149 (FR-046; research R35): at start a real-mode node loads br_netfilter and has
// bridged traffic pass iptables and ip6tables, and reads both settings back; what does not hold is
// named, so readiness can fail naming it. The check reads the settings live, so readiness follows them.
// Stub mode runs nothing.

// bridgeRoot is a proc file system under a test root: the bridge settings with their values, or no
// bridge directory at all when values is nil — br_netfilter not loaded.
func bridgeRoot(t *testing.T, values map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if values == nil {
		return root
	}
	dir := filepath.Join(root, "proc", "sys", "net", "bridge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range values {
		path := filepath.Join(dir, name)
		if value == "<directory>" { // a setting the kernel refuses: no write can succeed
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func realModeSettings(f *fakeRunner, root string) *NodeSettings {
	n := NewNodeSettings("iptables")
	n.run, n.root = f.run, root
	return n
}

func setting(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "proc", "sys", "net", "bridge", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestNodeSettings_Ensure(t *testing.T) {
	root := bridgeRoot(t, map[string]string{"bridge-nf-call-iptables": "0\n", "bridge-nf-call-ip6tables": "0\n"})
	f := &fakeRunner{}
	n := realModeSettings(f, root)
	if err := n.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := f.commands(); !slices.Equal(got, []string{"modprobe br_netfilter"}) {
		t.Errorf("commands %q; want br_netfilter loaded", got)
	}
	for _, c := range f.calls {
		if !c.hasDeadline || c.budget > time.Second {
			t.Errorf("%s: deadline %v, budget %s; want at most a second", c.name, c.hasDeadline, c.budget)
		}
	}
	for _, name := range []string{"bridge-nf-call-iptables", "bridge-nf-call-ip6tables"} {
		if got := setting(t, root, name); got != "1" {
			t.Errorf("%s = %q; want 1", name, got)
		}
	}
	if err := n.Check(); err != nil {
		t.Errorf("check after ensure: %v", err)
	}
}

// A module built into the kernel, or loaded by the host, needs no modprobe: a failing one fails nothing
// while the settings hold.
func TestNodeSettings_LoadedAlready(t *testing.T) {
	root := bridgeRoot(t, map[string]string{"bridge-nf-call-iptables": "1\n", "bridge-nf-call-ip6tables": "1\n"})
	f := &fakeRunner{respond: func(context.Context, fakeCall) ([]byte, error) {
		return []byte("modprobe: can't change directory to '/lib/modules': No such file or directory"), errors.New("exit status 1")
	}}
	if err := realModeSettings(f, root).Ensure(context.Background()); err != nil {
		t.Errorf("ensure: %v; want the settings that hold to count", err)
	}
}

func TestNodeSettings_WhatDoesNotHoldIsNamed(t *testing.T) {
	t.Run("no module: both settings missing, and the load's failure", func(t *testing.T) {
		root := bridgeRoot(t, nil)
		f := &fakeRunner{respond: func(context.Context, fakeCall) ([]byte, error) {
			return []byte("modprobe: module br_netfilter not found"), errors.New("exit status 1")
		}}
		n := realModeSettings(f, root)
		err := n.Ensure(context.Background())
		if err == nil {
			t.Fatal("no error")
		}
		for _, want := range []string{"br_netfilter", "bridge-nf-call-iptables", "bridge-nf-call-ip6tables"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
		if err := n.Check(); err == nil || !strings.Contains(err.Error(), "bridge-nf-call-iptables") {
			t.Errorf("check: %v; want it to name the missing settings", err)
		}
	})

	t.Run("one setting refused: it alone is named", func(t *testing.T) {
		root := bridgeRoot(t, map[string]string{"bridge-nf-call-iptables": "0\n", "bridge-nf-call-ip6tables": "<directory>"})
		n := realModeSettings(&fakeRunner{}, root)
		err := n.Ensure(context.Background())
		if err == nil || !strings.Contains(err.Error(), "bridge-nf-call-ip6tables") || strings.Contains(err.Error(), "bridge-nf-call-iptables") {
			t.Errorf("ensure: %v; want bridge-nf-call-ip6tables alone named", err)
		}
		if got := setting(t, root, "bridge-nf-call-iptables"); got != "1" {
			t.Errorf("bridge-nf-call-iptables = %q; want it set all the same", got)
		}
		if err := n.Check(); err == nil || !strings.Contains(err.Error(), "bridge-nf-call-ip6tables") {
			t.Errorf("check: %v", err)
		}
	})

	t.Run("a setting turned off later fails the check", func(t *testing.T) {
		root := bridgeRoot(t, map[string]string{"bridge-nf-call-iptables": "1\n", "bridge-nf-call-ip6tables": "1\n"})
		n := realModeSettings(&fakeRunner{}, root)
		if err := n.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "proc", "sys", "net", "bridge", "bridge-nf-call-iptables"), []byte("0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := n.Check(); err == nil || !strings.Contains(err.Error(), "bridge-nf-call-iptables") {
			t.Errorf("check: %v; want the setting turned off named", err)
		}
	})
}

func TestNodeSettings_StubRunsNothing(t *testing.T) {
	root := bridgeRoot(t, nil)
	f := &fakeRunner{}
	n := NewNodeSettings("stub")
	n.run, n.root = f.run, root
	if err := n.Ensure(context.Background()); err != nil || n.Check() != nil {
		t.Errorf("stub: ensure %v, check %v; want both nil", err, n.Check())
	}
	if len(f.calls) != 0 {
		t.Errorf("stub mode ran %q", f.commands())
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("stub mode wrote %v", entries)
	}
}
