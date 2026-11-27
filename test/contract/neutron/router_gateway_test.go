package neutron_test

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-033 round 2, T095 (FR-038, FR-039): a router's gateway address read back through gophercloud's
// routers.Get, and upstream's two 409s for an interface or a gateway a floating IP goes through.

// externalNetwork creates an external network with a /24 subnet as the administrator, both deleted
// when the test ends, and returns the network, the subnet and the subnet's first three octets.
func externalNetwork(t *testing.T, client *gophercloud.ServiceClient) (string, string, string) {
	t.Helper()
	prefix := fmt.Sprintf("198.18.%d", rand.IntN(250))
	var net struct {
		Network struct {
			ID string `json:"id"`
		} `json:"network"`
	}
	_, err := client.Post(client.ServiceURL("networks"), map[string]any{"network": map[string]any{
		"name": "contract-ext-" + uuid.NewString()[:8], "router:external": true,
	}}, &net, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = networks.Delete(client, net.Network.ID).ExtractErr() })
	s, err := subnets.Create(client, subnets.CreateOpts{NetworkID: net.Network.ID, CIDR: prefix + ".0/24", IPVersion: gophercloud.IPv4}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = subnets.Delete(client, s.ID).ExtractErr() })
	return net.Network.ID, s.ID, prefix
}

func TestNeutronRouterGateway_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := contract.SetupNetworkV2Client(t)
	external, externalSubnet, _ := externalNetwork(t, client)

	r, err := routers.Create(client, routers.CreateOpts{
		Name: "contract-gw-" + uuid.NewString()[:8], GatewayInfo: &routers.GatewayInfo{NetworkID: external},
	}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = routers.Delete(client, r.ID).ExtractErr() })

	got, err := routers.Get(client, r.ID).Extract()
	require.NoError(t, err)
	require.Len(t, got.GatewayInfo.ExternalFixedIPs, 1, "external_fixed_ips read back")
	assert.Equal(t, externalSubnet, got.GatewayInfo.ExternalFixedIPs[0].SubnetID)
	assert.NotEmpty(t, got.GatewayInfo.ExternalFixedIPs[0].IPAddress)

	// A floating IP through the router: an internal subnet joined to it, a port on that subnet.
	n, err := networks.Create(client, networks.CreateOpts{Name: "contract-gw-int-" + uuid.NewString()[:8]}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = networks.Delete(client, n.ID).ExtractErr() })
	s, err := subnets.Create(client, subnets.CreateOpts{NetworkID: n.ID, CIDR: "10.248.0.0/24", IPVersion: gophercloud.IPv4}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = subnets.Delete(client, s.ID).ExtractErr() })
	_, err = routers.AddInterface(client, r.ID, routers.AddInterfaceOpts{SubnetID: s.ID}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = routers.RemoveInterface(client, r.ID, routers.RemoveInterfaceOpts{SubnetID: s.ID}).Extract()
	})
	p, err := ports.Create(client, ports.CreateOpts{NetworkID: n.ID}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ports.Delete(client, p.ID).ExtractErr() })
	fip, err := floatingips.Create(client, floatingips.CreateOpts{FloatingNetworkID: external, PortID: p.ID}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = floatingips.Delete(client, fip.ID).ExtractErr() })

	code, typ := neutronRaw(t, client, http.MethodPut, "routers/"+r.ID+"/remove_router_interface", `{"subnet_id":"`+s.ID+`"}`)
	assert.Equal(t, http.StatusConflict, code, "removing the interface the floating IP goes through")
	assert.Equal(t, "RouterInterfaceInUseByFloatingIP", typ)

	code, typ = neutronRaw(t, client, http.MethodPut, "routers/"+r.ID, `{"router":{"external_gateway_info":{}}}`)
	assert.Equal(t, http.StatusConflict, code, "clearing the gateway the floating IP goes through")
	assert.Equal(t, "RouterExternalGatewayInUseByFloatingIp", typ)
}
