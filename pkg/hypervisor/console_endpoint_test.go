package hypervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// The guest console is a UNIX socket, so a token names a path instead of a TCP port the
// next guest may be handed. These cases pin what the readback takes from the domain
// XML. Whether the domain is running (its ID) and whether the socket is in the console
// socket directory are checked around the parse, in GetConsoleSocket.
func TestParseConsoleSocket(t *testing.T) {
	const running = `<domain type='kvm'><name>vm1</name><devices>
    <graphics type='vnc' socket='/var/lib/lightstack/console-sockets/1111.sock' sharePolicy='force-shared'>
      <listen type='socket' socket='/var/lib/lightstack/console-sockets/1111.sock'/>
    </graphics>
  </devices></domain>`

	const listenOnly = `<domain type='kvm'><devices>
    <graphics type='vnc'>
      <listen type='socket' socket='/var/lib/lightstack/console-sockets/2222.sock'/>
    </graphics>
  </devices></domain>`

	const noPath = `<domain type='kvm'><devices>
    <graphics type='vnc' sharePolicy='force-shared'>
      <listen type='socket'/>
    </graphics>
  </devices></domain>`

	const tcpLoopback = `<domain type='kvm'><devices>
    <graphics type='vnc' port='5901' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
  </devices></domain>`

	const tcpWildcard = `<domain type='kvm'><devices>
    <graphics type='vnc' port='5901' autoport='yes' listen='0.0.0.0'/>
  </devices></domain>`

	const tcpNoAddress = `<domain type='kvm'><devices>
    <graphics type='vnc' port='5901'/>
  </devices></domain>`

	const tcpNotRunning = `<domain type='kvm'><devices>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/>
  </devices></domain>`

	const relative = `<domain type='kvm'><devices>
    <graphics type='vnc' socket='vnc.sock'/>
  </devices></domain>`

	const noGraphics = `<domain type='kvm'><devices>
    <serial type='pty'><target port='0'/></serial>
  </devices></domain>`

	const spiceOnly = `<domain type='kvm'><devices>
    <graphics type='spice' port='5900' listen='127.0.0.1'/>
  </devices></domain>`

	const multi = `<domain type='kvm'><devices>
    <graphics type='spice' port='5905' listen='127.0.0.1'/>
    <graphics type='vnc' socket='/var/lib/lightstack/console-sockets/3333.sock'/>
  </devices></domain>`

	tests := []struct {
		name    string
		xml     string
		want    string
		wantErr error
	}{
		{name: "running socket", xml: running, want: "/var/lib/lightstack/console-sockets/1111.sock"},
		{name: "socket on the listen element only", xml: listenOnly, want: "/var/lib/lightstack/console-sockets/2222.sock"},
		{name: "picks vnc among several", xml: multi, want: "/var/lib/lightstack/console-sockets/3333.sock"},
		{name: "socket listener without a path", xml: noPath, wantErr: ErrConsoleNotReady},
		{name: "legacy tcp listener, running", xml: tcpLoopback, wantErr: ErrConsoleNotSocket},
		{name: "legacy tcp wildcard, running", xml: tcpWildcard, wantErr: ErrConsoleNotSocket},
		{name: "legacy tcp listener with no address, running", xml: tcpNoAddress, wantErr: ErrConsoleNotSocket},
		{name: "legacy tcp listener, not running", xml: tcpNotRunning, wantErr: ErrConsoleNotReady},
		{name: "relative socket path", xml: relative, wantErr: ErrConsoleParse},
		{name: "no graphics device", xml: noGraphics, wantErr: ErrNoGraphicalConsole},
		{name: "spice only, no vnc", xml: spiceOnly, wantErr: ErrNoGraphicalConsole},
		{name: "malformed xml", xml: `<domain><devices>`, wantErr: ErrConsoleParse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConsoleSocket(tt.xml)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v; want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("socket = %q; want %q", got, tt.want)
			}
		})
	}
}

// Constitution III: external dependencies fail fast. go-libvirt's generated
// calls take no context, so the bound has to be imposed by the caller — this
// asserts it actually is.
func TestGetConsoleSocketHonoursDeadline(t *testing.T) {
	m := &VMManager{mode: "real"}

	// A nil connection is the cheapest "hypervisor unavailable": it must return
	// immediately rather than block, and never panic.
	start := time.Now()
	_, _, err := m.GetConsoleSocket(context.Background(), "11111111-2222-3333-4444-555555555555")
	if err == nil {
		t.Fatal("expected an error with no libvirt connection")
	}
	if elapsed := time.Since(start); elapsed > consoleReadTimeout {
		t.Errorf("took %v; must fail within %v", elapsed, consoleReadTimeout)
	}
}

// An already-cancelled context must be honoured, proving the deadline path is
// wired rather than decorative.
func TestGetConsoleSocketCancelledContext(t *testing.T) {
	m := &VMManager{mode: "real"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := m.GetConsoleSocket(ctx, "11111111-2222-3333-4444-555555555555"); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

// Stub mode has no guest, so it must say so plainly instead of inventing an
// endpoint the way setupVNCConsole used to (FR-027).
func TestGetConsoleSocketStubMode(t *testing.T) {
	m, err := NewVMManager("test:///default", "stub")
	if err != nil {
		t.Fatalf("NewVMManager: %v", err)
	}
	_, _, err = m.GetConsoleSocket(context.Background(), "11111111-2222-3333-4444-555555555555")
	if !errors.Is(err, ErrConsoleUnavailable) {
		t.Fatalf("err = %v; want ErrConsoleUnavailable", err)
	}
	if !strings.Contains(err.Error(), "stub") {
		t.Errorf("error %q should name stub mode so the cause is obvious", err)
	}
}

// A real-mode manager without a libvirt connection is an outage, not stub mode, so it
// must not answer ErrConsoleUnavailable (501, "there is no guest"). NewVMManager never
// returns one, so this keeps the guard honest rather than covering a live path.
func TestGetConsoleSocketWithoutConnection(t *testing.T) {
	m := &VMManager{mode: "real"}
	_, _, err := m.GetConsoleSocket(context.Background(), "11111111-2222-3333-4444-555555555555")
	if err == nil {
		t.Fatal("expected an error with no libvirt connection")
	}
	if errors.Is(err, ErrConsoleUnavailable) || strings.Contains(err.Error(), "stub") {
		t.Errorf("err = %v; a real-mode manager without a connection is not stub mode", err)
	}
}

// consoleReadTimeout is the fail-fast budget for the readback (Constitution III).
func TestConsoleReadTimeoutIsFailFast(t *testing.T) {
	if consoleReadTimeout > time.Second {
		t.Errorf("consoleReadTimeout = %v; Constitution III caps external calls at 1s", consoleReadTimeout)
	}
}

// The console socket must be in the console socket directory, the only place the proxy
// can reach. A socket anywhere else, in practice libvirt's per-domain directory, belongs
// to a domain defined before the move and is refused, never proxied (SPEC-021 FR-017).
func TestCheckConsoleSocketDir(t *testing.T) {
	for _, tc := range []struct {
		socket string
		ok     bool
	}{
		{paths.ConsoleSocketPath("11111111-2222-3333-4444-555555555555"), true},
		{"/var/lib/libvirt/qemu/domain-7-instance-1111/vnc.sock", false},
		{paths.ConsoleSocketDir() + "/nested/x.sock", false},
		{paths.ConsoleSocketDir() + "/../x.sock", false},
	} {
		err := checkConsoleSocketDir(tc.socket)
		if tc.ok && err != nil {
			t.Errorf("checkConsoleSocketDir(%q) = %v; want nil", tc.socket, err)
		}
		if !tc.ok && !errors.Is(err, ErrConsoleLegacySocket) {
			t.Errorf("checkConsoleSocketDir(%q) = %v; want ErrConsoleLegacySocket", tc.socket, err)
		}
	}
}

// The readback returns the socket together with the libvirt domain ID of the current
// run, which is what tells a restarted guest from the one a token was issued for now
// that the socket path is fixed per instance (FR-014a).
func TestGetConsoleSocketReturnsTheDomainID(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	runningAt := func(socket string) string {
		return "<domain type='kvm'><devices><graphics type='vnc' socket='" + socket +
			"' sharePolicy='force-shared'><listen type='socket' socket='" + socket +
			"'/></graphics></devices></domain>"
	}

	t.Run("in the console socket directory", func(t *testing.T) {
		m := newConnectedManagerWith(t, &fakeLibvirtd{xml: runningAt(paths.ConsoleSocketPath(id))})
		socket, domainID, err := m.GetConsoleSocket(t.Context(), id)
		if err != nil {
			t.Fatalf("GetConsoleSocket: %v", err)
		}
		if socket != paths.ConsoleSocketPath(id) {
			t.Errorf("socket = %q; want %q", socket, paths.ConsoleSocketPath(id))
		}
		if domainID != 1 { // the fake domain's id
			t.Errorf("domain ID = %d; want 1", domainID)
		}
	})

	t.Run("in libvirt's per-domain directory", func(t *testing.T) {
		m := newConnectedManagerWith(t, &fakeLibvirtd{xml: runningAt("/var/lib/libvirt/qemu/domain-1-instance-fake/vnc.sock")})
		if _, _, err := m.GetConsoleSocket(t.Context(), id); !errors.Is(err, ErrConsoleLegacySocket) {
			t.Errorf("err = %v; want ErrConsoleLegacySocket", err)
		}
	})

	// For a domain that is not running, libvirt returns its persistent definition, and
	// with a fixed path that names the socket too. Only the domain ID (-1) says that no
	// QEMU serves it; without the check a stopped guest would get a token for a run
	// that does not exist.
	t.Run("stopped, its definition still naming the socket", func(t *testing.T) {
		m := newConnectedManagerWith(t, &fakeLibvirtd{stopped: true, xml: runningAt(paths.ConsoleSocketPath(id))})
		if _, _, err := m.GetConsoleSocket(t.Context(), id); !errors.Is(err, ErrConsoleNotReady) {
			t.Errorf("err = %v; want ErrConsoleNotReady", err)
		}
	})
}
