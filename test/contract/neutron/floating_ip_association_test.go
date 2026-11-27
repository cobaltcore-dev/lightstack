package neutron_test

import (
	"net/http"
	"sync"
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

// SPEC-033 round 2, T099 (FR-027–FR-029): floating IPs through gophercloud — ten created at once, the
// 404 for a port no router joins to the floating network, and the 409 for a second floating IP of one
// network on one fixed IP.
func TestNeutronFloatingIPAssociation_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := contract.SetupNetworkV2Client(t)
	external, _, _ := externalNetwork(t, client)

	t.Run("ten at once", func(t *testing.T) {
		var mu sync.Mutex
		seen := map[string]bool{}
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				fip, err := floatingips.Create(client, floatingips.CreateOpts{FloatingNetworkID: external}).Extract()
				mu.Lock()
				defer mu.Unlock()
				if !assert.NoError(t, err) {
					return
				}
				t.Cleanup(func() { _ = floatingips.Delete(client, fip.ID).ExtractErr() })
				assert.False(t, seen[fip.FloatingIP], "address %s handed out twice", fip.FloatingIP)
				seen[fip.FloatingIP] = true
			}()
		}
		wg.Wait()
		assert.Len(t, seen, 10)
	})

	// An internal network with a port; no router joins it to the external network yet.
	n, err := networks.Create(client, networks.CreateOpts{Name: "contract-fip-int-" + uuid.NewString()[:8]}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = networks.Delete(client, n.ID).ExtractErr() })
	s, err := subnets.Create(client, subnets.CreateOpts{NetworkID: n.ID, CIDR: "10.249.0.0/24", IPVersion: gophercloud.IPv4}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = subnets.Delete(client, s.ID).ExtractErr() })
	p, err := ports.Create(client, ports.CreateOpts{NetworkID: n.ID}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ports.Delete(client, p.ID).ExtractErr() })

	t.Run("no joining router", func(t *testing.T) {
		code, typ := neutronRaw(t, client, http.MethodPost, "floatingips",
			`{"floatingip":{"floating_network_id":"`+external+`","port_id":"`+p.ID+`"}}`)
		assert.Equal(t, http.StatusNotFound, code)
		assert.Equal(t, "ExternalGatewayForFloatingIPNotFound", typ)

		fip, err := floatingips.Create(client, floatingips.CreateOpts{FloatingNetworkID: external}).Extract()
		require.NoError(t, err)
		t.Cleanup(func() { _ = floatingips.Delete(client, fip.ID).ExtractErr() })
		code, typ = neutronRaw(t, client, http.MethodPut, "floatingips/"+fip.ID, `{"floatingip":{"port_id":"`+p.ID+`"}}`)
		assert.Equal(t, http.StatusNotFound, code)
		assert.Equal(t, "ExternalGatewayForFloatingIPNotFound", typ)
	})

	t.Run("one per fixed IP per network", func(t *testing.T) {
		r, err := routers.Create(client, routers.CreateOpts{
			Name: "contract-fip-" + uuid.NewString()[:8], GatewayInfo: &routers.GatewayInfo{NetworkID: external},
		}).Extract()
		require.NoError(t, err)
		t.Cleanup(func() { _ = routers.Delete(client, r.ID).ExtractErr() })
		_, err = routers.AddInterface(client, r.ID, routers.AddInterfaceOpts{SubnetID: s.ID}).Extract()
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = routers.RemoveInterface(client, r.ID, routers.RemoveInterfaceOpts{SubnetID: s.ID}).Extract()
		})

		first, err := floatingips.Create(client, floatingips.CreateOpts{FloatingNetworkID: external, PortID: p.ID}).Extract()
		require.NoError(t, err)
		t.Cleanup(func() { _ = floatingips.Delete(client, first.ID).ExtractErr() })
		assert.Equal(t, r.ID, first.RouterID)

		code, typ := neutronRaw(t, client, http.MethodPost, "floatingips",
			`{"floatingip":{"floating_network_id":"`+external+`","port_id":"`+p.ID+`"}}`)
		assert.Equal(t, http.StatusConflict, code)
		assert.Equal(t, "FloatingIPPortAlreadyAssociated", typ)
	})
}
