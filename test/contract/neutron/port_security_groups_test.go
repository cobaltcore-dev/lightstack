package neutron_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-033 T032 (FR-017–FR-019a; contracts/network-api.md): a port's groups through the Networking
// API, the group listing's id and name filters, and Horizon's Edit Security Groups sequence.

// sgFixture creates a security group, deleted when the test ends. Create it before anything that
// will carry it: cleanups run last in, first out, and a group a port carries cannot be deleted.
func sgFixture(t *testing.T, client *gophercloud.ServiceClient) groups.SecGroup {
	t.Helper()
	g, err := groups.Create(client, groups.CreateOpts{Name: "contract-sg-" + uuid.NewString()[:8]}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = groups.Delete(client, g.ID).ExtractErr() })
	return *g
}

// networkFixture creates a network with a subnet, deleted when the test ends.
func networkFixture(t *testing.T, client *gophercloud.ServiceClient, cidr string) string {
	t.Helper()
	n, err := networks.Create(client, networks.CreateOpts{Name: "spec033-net-" + uuid.NewString()[:8]}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = networks.Delete(client, n.ID).ExtractErr() })
	_, err = subnets.Create(client, subnets.CreateOpts{NetworkID: n.ID, CIDR: cidr, IPVersion: gophercloud.IPv4}).Extract()
	require.NoError(t, err)
	return n.ID
}

func portFixture(t *testing.T, client *gophercloud.ServiceClient, networkID string, sgs *[]string) ports.Port {
	t.Helper()
	p, err := ports.Create(client, ports.CreateOpts{NetworkID: networkID, SecurityGroups: sgs}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ports.Delete(client, p.ID).ExtractErr() })
	return *p
}

// listGroupIDs sends GET /v2.0/security-groups with the raw query and returns the ids, sorted.
func listGroupIDs(t *testing.T, client *gophercloud.ServiceClient, query string) []string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, client.ServiceURL("security-groups")+query, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, query)
	var out struct {
		SecurityGroups []struct {
			ID string `json:"id"`
		} `json:"security_groups"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	ids := []string{}
	for _, g := range out.SecurityGroups {
		ids = append(ids, g.ID)
	}
	sort.Strings(ids)
	return ids
}

func sortedIDs(ids ...string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

func TestNeutronPortSecurityGroups_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := contract.SetupNetworkV2Client(t)
	web := sgFixture(t, client)
	netID := networkFixture(t, client, "10.245.10.0/24")

	t.Run("update replaces the list, the empty list included", func(t *testing.T) {
		port := portFixture(t, client, netID, nil)
		updated, err := ports.Update(client, port.ID, ports.UpdateOpts{SecurityGroups: &[]string{web.ID}}).Extract()
		require.NoError(t, err)
		assert.Equal(t, []string{web.ID}, updated.SecurityGroups)
		got, err := ports.Get(client, port.ID).Extract()
		require.NoError(t, err)
		assert.Equal(t, []string{web.ID}, got.SecurityGroups)

		updated, err = ports.Update(client, port.ID, ports.UpdateOpts{SecurityGroups: &[]string{}}).Extract()
		require.NoError(t, err)
		assert.Empty(t, updated.SecurityGroups)
	})

	t.Run("create with an empty list gives no group", func(t *testing.T) {
		port := portFixture(t, client, netID, &[]string{})
		got, err := ports.Get(client, port.ID).Extract()
		require.NoError(t, err)
		assert.Empty(t, got.SecurityGroups)
	})
}

func TestNeutronSecurityGroupListFilters_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := contract.SetupNetworkV2Client(t)
	a, b := sgFixture(t, client), sgFixture(t, client)

	pages, err := groups.List(client, groups.ListOpts{Name: a.Name}).AllPages()
	require.NoError(t, err)
	byName, err := groups.ExtractGroups(pages)
	require.NoError(t, err)
	require.Len(t, byName, 1, "a name names one group")
	assert.Equal(t, a.ID, byName[0].ID)

	assert.Equal(t, sortedIDs(a.ID, b.ID), listGroupIDs(t, client, "?id="+a.ID+"&id="+b.ID))
}

// Horizon's Edit Security Groups: the instance's ports by device id, their groups by a repeated ?id=,
// then each port updated with the chosen list. The server then reports that list.
func TestNeutronHorizonEditSecurityGroups_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	network := contract.SetupNetworkV2Client(t)
	compute := contract.SetupComputeV2Client(t)
	chosen := sgFixture(t, network)
	netID := networkFixture(t, network, "10.245.11.0/24")
	image := contract.CreateTestImage(t)

	server, err := servers.Create(compute, servers.CreateOpts{
		Name: "spec033-horizon-edit", FlavorRef: "m1.small", ImageRef: image,
		Networks: []servers.Network{{UUID: netID}},
	}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = servers.Delete(compute, server.ID).ExtractErr() })

	var serverPorts []ports.Port
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		pages, err := ports.List(network, ports.ListOpts{DeviceID: server.ID}).AllPages()
		require.NoError(t, err)
		if serverPorts, err = ports.ExtractPorts(pages); err == nil && len(serverPorts) > 0 {
			break
		}
	}
	require.NotEmpty(t, serverPorts, "the server never got a port")

	var carried []string
	for _, p := range serverPorts {
		carried = append(carried, p.SecurityGroups...)
	}
	query := "?id=" + strings.Join(carried, "&id=")
	assert.Equal(t, sortedIDs(carried...), listGroupIDs(t, network, query), "the listing by the ports' ids")

	for _, p := range serverPorts {
		_, err := ports.Update(network, p.ID, ports.UpdateOpts{SecurityGroups: &[]string{chosen.ID}}).Extract()
		require.NoError(t, err)
	}
	s, err := servers.Get(compute, server.ID).Extract()
	require.NoError(t, err)
	var names []string
	for _, g := range s.SecurityGroups {
		if n, ok := g["name"].(string); ok {
			names = append(names, n)
		}
	}
	assert.Equal(t, []string{chosen.Name}, names, "the server reports the chosen list")
}
