package neutron

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SPEC-033 T179: dhcpPortOf took the oldest network:dhcp port whose device
// is the network, whichever project made it, and syncDHCPPort adopted it. The network's DHCP port is the
// one Lightstack writes, in the network's project (SPEC-033 FR-062).

func seedDHCPClaim(t *testing.T, project, network string, created time.Time) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips, created_at)
		VALUES ($1, 'src7-dhcp', $2, $3, $2, 'network:dhcp', $4, 'ACTIVE', '[]'::jsonb, $5)`,
		id, network, project, fmt.Sprintf("fa:16:3e:57:%02x:%02x", rand.IntN(256), rand.IntN(256)), created)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id = $1", id) })
	return id
}

func TestDHCPPortIsTheNetworksProjects(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	owner := createTestProject(t, "src7-dhcp-owner")
	network := seedExternalNetwork(t, "src7-dhcp-"+uuid.NewString()[:6], owner, true, false)
	planted := seedDHCPClaim(t, seededProjectID, network, time.Now().Add(-time.Hour))

	if p, found, err := dhcpPortOf(ctx, testPool, network); err != nil || found {
		t.Errorf("with only another project's claim: port %q found %v (%v); want none", p.id, found, err)
	}
	own := seedDHCPClaim(t, owner, network, time.Now())
	if p, found, err := dhcpPortOf(ctx, testPool, network); err != nil || !found || p.id != own {
		t.Errorf("the network's DHCP port: %q found %v (%v); want %s, not the older %s", p.id, found, err, own, planted)
	}
}
