package neutron_test

import (
	"net/http"
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/pagination"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-033 round 2, T103 (FR-057; research R50): through gophercloud, the seeded administrator lists
// and shows a second project's networks, ports, routers, floating IPs and security groups, and
// project_id/tenant_id narrow the listings to it; that project's member sees only their own (lock).

// projectResources are one resource of each kind, all of one project.
type projectResources struct {
	network, port, router, floatingIP, group string
}

// createProjectResources creates them through client, the floating IP on external.
func createProjectResources(t *testing.T, client *gophercloud.ServiceClient, external, cidr string) projectResources {
	t.Helper()
	tag := uuid.NewString()[:8]
	var r projectResources
	n, err := networks.Create(client, networks.CreateOpts{Name: "contract-admin-" + tag}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = networks.Delete(client, n.ID).ExtractErr() })
	s, err := subnets.Create(client, subnets.CreateOpts{NetworkID: n.ID, CIDR: cidr, IPVersion: gophercloud.IPv4}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = subnets.Delete(client, s.ID).ExtractErr() })
	p, err := ports.Create(client, ports.CreateOpts{NetworkID: n.ID}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ports.Delete(client, p.ID).ExtractErr() })
	rt, err := routers.Create(client, routers.CreateOpts{Name: "contract-admin-" + tag}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = routers.Delete(client, rt.ID).ExtractErr() })
	fip, err := floatingips.Create(client, floatingips.CreateOpts{FloatingNetworkID: external}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = floatingips.Delete(client, fip.ID).ExtractErr() })
	g, err := groups.Create(client, groups.CreateOpts{Name: "contract-admin-" + tag}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = groups.Delete(client, g.ID).ExtractErr() })
	r.network, r.port, r.router, r.floatingIP, r.group = n.ID, p.ID, rt.ID, fip.ID, g.ID
	return r
}

// adminCollection is one collection: its listing narrowed to a project, and its show, through
// gophercloud.
type adminCollection struct {
	name string
	id   func(projectResources) string
	list func(c *gophercloud.ServiceClient, byProject, byTenant string) ([]string, error)
	get  func(c *gophercloud.ServiceClient, id string) error
}

// ids collects the ids of a listing's pages.
func ids[T any](pager pagination.Pager, extract func(pagination.Page) ([]T, error), id func(T) string) ([]string, error) {
	pages, err := pager.AllPages()
	if err != nil {
		return nil, err
	}
	items, err := extract(pages)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, id(item))
	}
	return out, nil
}

func adminCollections() []adminCollection {
	return []adminCollection{
		{name: "networks", id: func(r projectResources) string { return r.network },
			list: func(c *gophercloud.ServiceClient, p, tn string) ([]string, error) {
				return ids(networks.List(c, networks.ListOpts{ProjectID: p, TenantID: tn}), networks.ExtractNetworks,
					func(n networks.Network) string { return n.ID })
			},
			get: func(c *gophercloud.ServiceClient, id string) error { return networks.Get(c, id).Err }},
		{name: "ports", id: func(r projectResources) string { return r.port },
			list: func(c *gophercloud.ServiceClient, p, tn string) ([]string, error) {
				return ids(ports.List(c, ports.ListOpts{ProjectID: p, TenantID: tn}), ports.ExtractPorts,
					func(p ports.Port) string { return p.ID })
			},
			get: func(c *gophercloud.ServiceClient, id string) error { return ports.Get(c, id).Err }},
		{name: "routers", id: func(r projectResources) string { return r.router },
			list: func(c *gophercloud.ServiceClient, p, tn string) ([]string, error) {
				return ids(routers.List(c, routers.ListOpts{ProjectID: p, TenantID: tn}), routers.ExtractRouters,
					func(r routers.Router) string { return r.ID })
			},
			get: func(c *gophercloud.ServiceClient, id string) error { return routers.Get(c, id).Err }},
		{name: "floating IPs", id: func(r projectResources) string { return r.floatingIP },
			list: func(c *gophercloud.ServiceClient, p, tn string) ([]string, error) {
				return ids(floatingips.List(c, floatingips.ListOpts{ProjectID: p, TenantID: tn}), floatingips.ExtractFloatingIPs,
					func(f floatingips.FloatingIP) string { return f.ID })
			},
			get: func(c *gophercloud.ServiceClient, id string) error { return floatingips.Get(c, id).Err }},
		{name: "security groups", id: func(r projectResources) string { return r.group },
			list: func(c *gophercloud.ServiceClient, p, tn string) ([]string, error) {
				return ids(groups.List(c, groups.ListOpts{ProjectID: p, TenantID: tn}), groups.ExtractGroups,
					func(g groups.SecGroup) string { return g.ID })
			},
			get: func(c *gophercloud.ServiceClient, id string) error { return groups.Get(c, id).Err }},
	}
}

func TestNeutronAdministratorListsEveryProject_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	admin := contract.SetupNeutronClient(t)
	member, memberProject := memberNetworkClient(t)
	external, _, _ := externalNetwork(t, admin)
	own := createProjectResources(t, admin, external, "10.245.20.0/24")
	theirs := createProjectResources(t, member, external, "10.245.21.0/24")

	for _, c := range adminCollections() {
		t.Run(c.name, func(t *testing.T) {
			listed, err := c.list(admin, "", "")
			require.NoError(t, err)
			assert.Contains(t, listed, c.id(theirs), "the administrator's unfiltered listing shows the other project's")

			for _, filter := range []struct{ name, project, tenant string }{
				{"project_id", memberProject, ""}, {"tenant_id", "", memberProject},
			} {
				listed, err := c.list(admin, filter.project, filter.tenant)
				require.NoError(t, err)
				assert.Contains(t, listed, c.id(theirs), "?%s names the other project", filter.name)
				assert.NotContains(t, listed, c.id(own), "?%s leaves the administrator's own out", filter.name)
			}
			assert.NoError(t, c.get(admin, c.id(theirs)), "the administrator shows the other project's by id")

			listed, err = c.list(member, "", "")
			require.NoError(t, err)
			assert.Contains(t, listed, c.id(theirs), "the member lists their own")
			assert.NotContains(t, listed, c.id(own), "the member does not list the administrator's (lock)")
			err = c.get(member, c.id(own))
			var notFound gophercloud.ErrDefault404
			assert.ErrorAs(t, err, &notFound, "the member showing the administrator's answers 404 (lock)")
		})
	}

	t.Run("a group's name resolves in the administrator's own project", func(t *testing.T) {
		g, err := groups.Get(member, theirs.group).Extract()
		require.NoError(t, err)
		code, _ := neutronRaw(t, admin, http.MethodGet, "security-groups/"+g.Name, "")
		assert.Equal(t, http.StatusNotFound, code, "the other project's group by name")
	})
}
