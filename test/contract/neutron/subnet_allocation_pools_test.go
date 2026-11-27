package neutron_test

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-033 round 2, T088 (FR-030, FR-032): a subnet's allocation pools through gophercloud — created
// with pools, read back, a port's address taken from them, and upstream's 409 for a pool holding the
// gateway. (Named for allocation pools: subnet_pools_test.go covers the subnet-pool resource.)
func TestNeutronSubnetAllocationPools_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := contract.SetupNetworkV2Client(t)
	prefix := fmt.Sprintf("10.247.%d", rand.IntN(250))

	n, err := networks.Create(client, networks.CreateOpts{Name: "contract-pools-" + uuid.NewString()[:8]}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = networks.Delete(client, n.ID).ExtractErr() })

	gateway := prefix + ".1"
	_, err = subnets.Create(client, subnets.CreateOpts{
		NetworkID: n.ID, CIDR: prefix + ".0/24", IPVersion: gophercloud.IPv4, GatewayIP: &gateway,
		AllocationPools: []subnets.AllocationPool{{Start: prefix + ".1", End: prefix + ".9"}},
	}).Extract()
	require.Error(t, err, "a pool holding the gateway")
	code, typ := neutronRaw(t, client, http.MethodPost, "subnets",
		fmt.Sprintf(`{"subnet":{"network_id":%q,"cidr":"%s.0/24","ip_version":4,"gateway_ip":%q,"allocation_pools":[{"start":"%s.1","end":"%s.9"}]}}`,
			n.ID, prefix, gateway, prefix, prefix))
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "GatewayConflictWithAllocationPools", typ)

	s, err := subnets.Create(client, subnets.CreateOpts{
		NetworkID: n.ID, CIDR: prefix + ".0/24", IPVersion: gophercloud.IPv4, GatewayIP: &gateway,
		AllocationPools: []subnets.AllocationPool{{Start: prefix + ".20", End: prefix + ".21"}},
	}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = subnets.Delete(client, s.ID).ExtractErr() })
	want := []subnets.AllocationPool{{Start: prefix + ".20", End: prefix + ".21"}}
	assert.Equal(t, want, s.AllocationPools, "the create response")

	got, err := subnets.Get(client, s.ID).Extract()
	require.NoError(t, err)
	assert.Equal(t, want, got.AllocationPools, "read back")

	p, err := ports.Create(client, ports.CreateOpts{NetworkID: n.ID}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ports.Delete(client, p.ID).ExtractErr() })
	require.Len(t, p.FixedIPs, 1)
	assert.Contains(t, []string{prefix + ".20", prefix + ".21"}, p.FixedIPs[0].IPAddress, "a port's address comes from the pool")
}
