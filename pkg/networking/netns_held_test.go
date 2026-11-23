package networking

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// SPEC-033 T222: an older deployment's project namespaces are recognised as routers' and
// networks' DHCP namespaces are, by heldNamespaces — light-ns- and a lower-case UUID — so the
// database-less teardown recognises all three one way.
func TestHeldProjects(t *testing.T) {
	const project = "6f7a8b9c-0000-4000-8000-000000000006"
	f := &fakeRunner{respond: func(context.Context, fakeCall) ([]byte, error) {
		return []byte(testNetnsList + "light-ns-" + project + " (id: 3)\nlight-ns-p1\nlight-ns-6F7A8B9C-0000-4000-8000-000000000007\n"), nil
	}}
	m := NewNetworkNamespaceManager("iptables")
	m.run = f.run
	got, err := m.HeldProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{project}) {
		t.Errorf("held = %q; want [%s]: a light-ns- name with a lower-case UUID, nothing else", got, project)
	}

	t.Run("a failed listing is an error, not a node holding none", func(t *testing.T) {
		f := &fakeRunner{respond: func(context.Context, fakeCall) ([]byte, error) { return nil, errors.New("exit status 1") }}
		m := NewNetworkNamespaceManager("iptables")
		m.run = f.run
		if _, err := m.HeldProjects(context.Background()); err == nil {
			t.Fatal("no error")
		}
	})

	t.Run("stub mode holds none and runs nothing", func(t *testing.T) {
		f := &fakeRunner{}
		m := NewNetworkNamespaceManager("stub")
		m.run = f.run
		if got, err := m.HeldProjects(context.Background()); err != nil || len(got) != 0 || len(f.calls) != 0 {
			t.Errorf("= %q, %v after %q; want none, nothing run", got, err, f.commands())
		}
	})
}

// SPEC-033 T279: removing an older deployment's project namespace runs `ip netns delete` within a
// second; a namespace that is gone already is removed, and any other failure is reported, so teardown
// never counts a namespace removed that stays.
func TestDeleteNamespace(t *testing.T) {
	const project = "6f7a8b9c-0000-4000-8000-000000000006"
	for _, tc := range []struct {
		name    string
		out     string
		err     error
		wantErr bool
	}{
		{"removed", "", nil, false},
		{"gone already", `Cannot remove namespace file "/run/netns/light-ns-` + project + `": No such file or directory`,
			errors.New("exit status 1"), false},
		{"a failure", `Cannot remove namespace file "/run/netns/light-ns-` + project + `": Device or resource busy`,
			errors.New("exit status 1"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{respond: func(context.Context, fakeCall) ([]byte, error) { return []byte(tc.out), tc.err }}
			m := NewNetworkNamespaceManager("iptables")
			m.run = f.run
			err := m.DeleteNamespace(context.Background(), project)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v; want an error %v", err, tc.wantErr)
			}
			if got := f.commands(); !slices.Equal(got, []string{"ip netns delete light-ns-" + project}) {
				t.Errorf("commands %q; want the delete alone", got)
			}
			for _, c := range f.calls {
				if !c.hasDeadline || c.budget > time.Second {
					t.Errorf("%s: deadline %v, budget %s; want at most a second", c.name, c.hasDeadline, c.budget)
				}
			}
		})
	}

	t.Run("stub mode runs nothing", func(t *testing.T) {
		f := &fakeRunner{}
		m := NewNetworkNamespaceManager("stub")
		m.run = f.run
		if err := m.DeleteNamespace(context.Background(), project); err != nil || len(f.calls) != 0 {
			t.Errorf("%v after %q", err, f.commands())
		}
	})
}
