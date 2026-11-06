package hypervisor

import (
	"fmt"
	"strings"
	"testing"
)

// The forms of nova.libvirt_uri the binary accepts, and where each one dials. Any other
// value used to be dialed verbatim as a socket path, so the session and remote URIs the
// configuration docs listed only ever failed as "no such file or directory".
func TestParseLibvirtURI(t *testing.T) {
	accepted := []struct {
		uri, socket string // socket "" = the automatic choice
	}{
		{"", ""},
		{"qemu:///system", ""},
		{"qemu+unix:///system", ""},
		{"qemu:///system?socket=/run/x/s", "/run/x/s"},
		{"qemu+unix:///system?socket=/run/x/s", "/run/x/s"},
		{"/run/libvirt/virtqemud-sock", "/run/libvirt/virtqemud-sock"},
	}
	for _, tc := range accepted {
		got, err := ParseLibvirtURI(tc.uri)
		if err != nil {
			t.Errorf("ParseLibvirtURI(%q): %v; want accepted", tc.uri, err)
			continue
		}
		if got.Socket != tc.socket {
			t.Errorf("ParseLibvirtURI(%q).Socket = %q; want %q", tc.uri, got.Socket, tc.socket)
		}
	}

	refused := []string{
		"qemu:///session",
		"qemu:///embed?root=/x",
		"lxc:///system",
		"qemu://host/system",
		"qemu+tcp://host/system",
		"qemu+ssh://u@h/system",
		"qemu+tls://h/system",
		"qemu+libssh2://h/system",
		"qemu+ext:///system",
		"relative/sock",
		"qemu:///system?socket=relative",
		"qemu:///system?socket=",
		"qemu:///system?socket=/a&socket=/b",
		"qemu:///system?mode=direct",
		"qemu:///system#frag",
		"qemu:///system/",
	}
	for _, uri := range refused {
		_, err := ParseLibvirtURI(uri)
		if err == nil {
			t.Errorf("ParseLibvirtURI(%q) accepted; want refused", uri)
			continue
		}
		// One message for every refusal: the value, then the forms that work.
		if msg := err.Error(); !strings.Contains(msg, fmt.Sprintf("%q", uri)) || !strings.Contains(msg, "accepted") {
			t.Errorf("ParseLibvirtURI(%q) error %q; want it to name the value and the accepted forms", uri, msg)
		}
	}
}
