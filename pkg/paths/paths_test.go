package paths

import "testing"

// TestLayout pins the node-local hostPath layout. These exact paths are what the
// chart mounts and what the host-validated real mode reads/writes, so a change
// here is a deliberate layout change, not an accident (SPEC-009).
func TestLayout(t *testing.T) {
	cases := []struct{ got, want string }{
		{Base, "/var/lib/o3k"},
		{ImagesDir(), "/var/lib/o3k/images"},
		{VolumesDir(), "/var/lib/o3k/volumes"},
		{InstancesDir(), "/var/lib/o3k/instances"},
		{CloudInitDir(), "/var/lib/o3k/cloud-init"},
		{DHCPDir(), "/var/lib/o3k/dhcp"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("path = %q; want %q", c.got, c.want)
		}
	}
}
