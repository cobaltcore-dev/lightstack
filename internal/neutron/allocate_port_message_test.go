package neutron

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// SPEC-025 FR-018: the reason a boot failed is TENANT-VISIBLE, so the error text is an
// API surface rather than a log line.
//
// AllocatePortForInstance's failure lands verbatim in `instances.fault_message` and is
// served in the server-detail `fault` object. Found on a host: booting onto a
// subnet-less network produced
//
//	could not attach the server to network <id>: no subnet found for network <id>: no rows in result set
//
// The trailing clause is `pgx.ErrNoRows` wrapped with %w — meaningless to the tenant
// reading it, and it names the storage layer. The nova-side test for this path injects a
// fake error, so nothing there could catch it; only a real query can.
func TestAllocatePortMessageIsReadableWhenNoSubnetExists(t *testing.T) {
	requireDB(t)

	svc := &Service{pool: testPool}
	netID := seedNetwork(t, "spec025-nosubnet")

	_, err := svc.AllocatePortForInstance(context.Background(), netID, seededProjectID, uuid.New().String(), nil, false)
	if err == nil {
		t.Fatal("allocation succeeded against a network with no subnet; there is no address to hand out")
	}

	msg := err.Error()
	if !strings.Contains(msg, netID) {
		t.Errorf("message does not name the network: %q", msg)
	}
	if !strings.Contains(msg, "no subnet") {
		t.Errorf("message does not name the condition: %q", msg)
	}
	if strings.Contains(msg, "no rows in result set") {
		t.Errorf("the tenant-visible message carries pgx's own error text: %q\n"+
			"it lands in fault_message and tells a tenant about the database rather than "+
			"about their network", msg)
	}
}

// NOTE on the sibling branch: a genuine query failure (as opposed to ErrNoRows) must NOT
// be reported as "no subnet found", or a tenant is told to add a subnet they may already
// have. That branch is deliberately NOT tested here. The obvious attempt — a cancelled
// context — passes whatever the code does, because the network lookup runs first and
// fails there, so the subnet query is never reached. Making it real needs a
// fault-injection seam this package does not have, and a test that cannot fail is worse
// than none: it reads as coverage.
