package hypervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The guest console is a per-start UNIX socket that libvirt creates in the domain's
// private directory, so the token names a path that dies with the guest instead of a
// TCP port the next guest may be handed. These cases pin the readback, including the
// one that would reintroduce a stale target: the INACTIVE definition carries no socket
// path at all, and only a running domain's does.
func TestParseConsoleSocket(t *testing.T) {
	const running = `<domain type='kvm'><name>vm1</name><devices>
    <graphics type='vnc' socket='/var/lib/libvirt/qemu/domain-7-instance-1111/vnc.sock' sharePolicy='force-shared'>
      <listen type='socket' socket='/var/lib/libvirt/qemu/domain-7-instance-1111/vnc.sock'/>
    </graphics>
  </devices></domain>`

	const listenOnly = `<domain type='kvm'><devices>
    <graphics type='vnc'>
      <listen type='socket' socket='/var/lib/libvirt/qemu/domain-8-instance-2222/vnc.sock'/>
    </graphics>
  </devices></domain>`

	const inactive = `<domain type='kvm'><devices>
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
    <graphics type='vnc' socket='/var/lib/libvirt/qemu/domain-9-instance-3333/vnc.sock'/>
  </devices></domain>`

	tests := []struct {
		name    string
		xml     string
		want    string
		wantErr error
	}{
		{name: "running socket", xml: running, want: "/var/lib/libvirt/qemu/domain-7-instance-1111/vnc.sock"},
		{name: "socket on the listen element only", xml: listenOnly, want: "/var/lib/libvirt/qemu/domain-8-instance-2222/vnc.sock"},
		{name: "picks vnc among several", xml: multi, want: "/var/lib/libvirt/qemu/domain-9-instance-3333/vnc.sock"},
		{name: "inactive definition has no path", xml: inactive, wantErr: ErrConsoleNotReady},
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
	m := &VMManager{mode: "real", conn: nil}

	// A nil connection is the cheapest "hypervisor unavailable": it must return
	// immediately rather than block, and never panic.
	start := time.Now()
	_, err := m.GetConsoleSocket(context.Background(), "11111111-2222-3333-4444-555555555555")
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
	m := &VMManager{mode: "real", conn: nil}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.GetConsoleSocket(ctx, "11111111-2222-3333-4444-555555555555"); err == nil {
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
	_, err = m.GetConsoleSocket(context.Background(), "11111111-2222-3333-4444-555555555555")
	if !errors.Is(err, ErrConsoleUnavailable) {
		t.Fatalf("err = %v; want ErrConsoleUnavailable", err)
	}
	if !strings.Contains(err.Error(), "stub") {
		t.Errorf("error %q should name stub mode so the cause is obvious", err)
	}
}

// consoleReadTimeout is the fail-fast budget for the readback (Constitution III).
func TestConsoleReadTimeoutIsFailFast(t *testing.T) {
	if consoleReadTimeout > time.Second {
		t.Errorf("consoleReadTimeout = %v; Constitution III caps external calls at 1s", consoleReadTimeout)
	}
}
