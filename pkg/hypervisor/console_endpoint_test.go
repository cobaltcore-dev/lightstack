package hypervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The whole point of SPEC-021 is that the console port comes from the hypervisor
// instead of `5900 + hash(id) % 1000`. These cases pin the parse, including the
// one that would silently reintroduce the old bug: asking libvirt for the
// INACTIVE definition returns port="-1" (autoport unresolved), and treating that
// as a real port hands the browser a dead endpoint.
func TestParseConsoleEndpoint(t *testing.T) {
	const running = `<domain type='kvm'><name>vm1</name><devices>
    <graphics type='vnc' port='5901' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
  </devices></domain>`

	const unresolved = `<domain type='kvm'><devices>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/>
  </devices></domain>`

	const noGraphics = `<domain type='kvm'><devices>
    <serial type='pty'><target port='0'/></serial>
  </devices></domain>`

	const spiceOnly = `<domain type='kvm'><devices>
    <graphics type='spice' port='5900' listen='127.0.0.1'/>
  </devices></domain>`

	const multi = `<domain type='kvm'><devices>
    <graphics type='spice' port='5905' listen='127.0.0.1'/>
    <graphics type='vnc' port='5902' listen='127.0.0.1'/>
  </devices></domain>`

	tests := []struct {
		name     string
		xml      string
		wantHost string
		wantPort int
		wantErr  error
	}{
		{name: "resolved port", xml: running, wantHost: "127.0.0.1", wantPort: 5901},
		{name: "picks vnc among several", xml: multi, wantHost: "127.0.0.1", wantPort: 5902},
		{name: "autoport unresolved", xml: unresolved, wantErr: ErrConsoleNotReady},
		{name: "no graphics device", xml: noGraphics, wantErr: ErrNoGraphicalConsole},
		{name: "spice only, no vnc", xml: spiceOnly, wantErr: ErrNoGraphicalConsole},
		{name: "malformed xml", xml: `<domain><devices>`, wantErr: ErrConsoleParse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, err := parseConsoleEndpoint(tt.xml)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v; want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tt.wantHost || port != tt.wantPort {
				t.Errorf("got %s:%d; want %s:%d", host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// A guest that listens on 0.0.0.0 is reachable without a token, which voids the
// entire access-control model (FR-017). The XML template binds loopback, so a
// wildcard listen means something regressed — surface it rather than proxy to it.
func TestParseConsoleEndpointRejectsWildcardListen(t *testing.T) {
	const wildcard = `<domain type='kvm'><devices>
    <graphics type='vnc' port='5901' autoport='yes' listen='0.0.0.0'/>
  </devices></domain>`

	if _, _, err := parseConsoleEndpoint(wildcard); !errors.Is(err, ErrConsoleNotLoopback) {
		t.Fatalf("err = %v; want ErrConsoleNotLoopback", err)
	}
}

// Constitution III: external dependencies fail fast. go-libvirt's generated
// calls take no context, so the bound has to be imposed by the caller — this
// asserts it actually is.
func TestGetConsoleEndpointHonoursDeadline(t *testing.T) {
	m := &VMManager{mode: "real", conn: nil}

	// A nil connection is the cheapest "hypervisor unavailable": it must return
	// immediately rather than block, and never panic.
	start := time.Now()
	_, _, err := m.GetConsoleEndpoint(context.Background(), "11111111-2222-3333-4444-555555555555")
	if err == nil {
		t.Fatal("expected an error with no libvirt connection")
	}
	if elapsed := time.Since(start); elapsed > consoleReadTimeout {
		t.Errorf("took %v; must fail within %v", elapsed, consoleReadTimeout)
	}
}

// An already-cancelled context must be honoured, proving the deadline path is
// wired rather than decorative.
func TestGetConsoleEndpointCancelledContext(t *testing.T) {
	m := &VMManager{mode: "real", conn: nil}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := m.GetConsoleEndpoint(ctx, "11111111-2222-3333-4444-555555555555"); err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}

// Stub mode has no guest, so it must say so plainly instead of inventing an
// endpoint the way setupVNCConsole used to (FR-027).
func TestGetConsoleEndpointStubMode(t *testing.T) {
	m, err := NewVMManager("test:///default", "stub")
	if err != nil {
		t.Fatalf("NewVMManager: %v", err)
	}
	_, _, err = m.GetConsoleEndpoint(context.Background(), "11111111-2222-3333-4444-555555555555")
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
