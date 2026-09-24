package hypervisor

import (
	"testing"
	"time"
)

// A libvirtd restart closes Lightstack's connection, and nothing used to open it again:
// every libvirt call failed until the pod restarted, and /livez — which checks only
// the database — never noticed.
func TestVMManagerReconnectsWhenLibvirtdGoesAway(t *testing.T) {
	d := &pipeDialer{}
	m := newConnectedManager(t, d)
	defer m.Close()

	d.drop()

	waitFor(t, "a new dial after libvirtd went away", func() bool { return d.dialCount() >= 2 })
	waitFor(t, "libvirt to answer again", func() bool {
		_, err := m.conn.Load().ConnectGetLibVersion()
		return err == nil
	})
}

// Close is a deliberate disconnect. The reconnect loop has to stop before it, or it
// would treat the shutdown as a lost connection and dial again.
func TestVMManagerCloseDoesNotReconnect(t *testing.T) {
	d := &pipeDialer{}
	m := newConnectedManager(t, d)

	m.Close()
	m.Close() // idempotent: teardown and shutdown paths may both close

	time.Sleep(50 * time.Millisecond) // ten backoff ceilings: time enough for a stray dial
	if got := d.dialCount(); got != 1 {
		t.Fatalf("dialed %d times; want no dial after Close", got)
	}
}
