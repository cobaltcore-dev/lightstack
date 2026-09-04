package hypervisor

import "testing"

// The per-instance directory and its console.log path are referenced verbatim in
// the domain XML (<log file='.../console.log'>), so the layout is a contract — a
// drift here is exactly what broke boot-from-volume once (SPEC-009 WS-0/WS-5).
func TestInstancePaths(t *testing.T) {
	if got, want := InstanceDir("iid"), "/var/lib/lightstack/instances/iid"; got != want {
		t.Errorf("InstanceDir() = %q, want %q", got, want)
	}
	if got, want := ConsoleLogPath("iid"), "/var/lib/lightstack/instances/iid/console.log"; got != want {
		t.Errorf("ConsoleLogPath() = %q, want %q", got, want)
	}
}
