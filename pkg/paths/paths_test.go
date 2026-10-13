package paths

import "testing"

// TestLayout pins the node-local hostPath layout under the default root. These exact
// paths are what the chart mounts by default and what the host-validated real mode
// reads/writes, so a change here is a deliberate layout change, not an accident
// (SPEC-009).
func TestLayout(t *testing.T) {
	cases := []struct{ got, want string }{
		{Base(), "/var/lib/lightstack"},
		{ImagesDir(), "/var/lib/lightstack/images"},
		{VolumesDir(), "/var/lib/lightstack/volumes"},
		{InstancesDir(), "/var/lib/lightstack/instances"},
		{CloudInitDir(), "/var/lib/lightstack/cloud-init"},
		{DHCPDir(), "/var/lib/lightstack/dhcp"},
		{ConsoleTokenDir(), "/var/lib/lightstack/console-tokens"},
		{ConsoleSocketDir(), "/var/lib/lightstack/console-sockets"},
		{DBDir(), "/var/lib/lightstack/db"},
		{ConsoleSocketPath("1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"),
			"/var/lib/lightstack/console-sockets/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d.sock"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("path = %q; want %q", c.got, c.want)
		}
	}
}

// Every directory follows the configured root (state_path). A helper that kept the
// default would split the node's state across two roots — and libvirtd, which opens
// the paths written into the domain XML on the host, would find only half of it.
func TestSetBaseMovesEveryDir(t *testing.T) {
	SetBase("/data/lightstack")
	t.Cleanup(func() { SetBase(DefaultBase) })

	cases := []struct{ got, want string }{
		{Base(), "/data/lightstack"},
		{ImagesDir(), "/data/lightstack/images"},
		{VolumesDir(), "/data/lightstack/volumes"},
		{InstancesDir(), "/data/lightstack/instances"},
		{CloudInitDir(), "/data/lightstack/cloud-init"},
		{DHCPDir(), "/data/lightstack/dhcp"},
		{ConsoleTokenDir(), "/data/lightstack/console-tokens"},
		{ConsoleSocketDir(), "/data/lightstack/console-sockets"},
		{DBDir(), "/data/lightstack/db"},
		{ConsoleSocketPath("x"), "/data/lightstack/console-sockets/x.sock"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("path = %q; want %q", c.got, c.want)
		}
	}
}
