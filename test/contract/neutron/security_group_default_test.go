package neutron_test

import (
	"net/http"
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-033 round 2, T080 (FR-024–FR-026): `default`'s refusals, with upstream's Neutron types, through
// gophercloud, as a member and as an administrator.

// defaultGroupOf returns the id of the caller's project's default, after a port create made it
// exist and the port is gone again, so no port carries it.
func defaultGroupOf(t *testing.T, client *gophercloud.ServiceClient) string {
	t.Helper()
	n, err := networks.Create(client, networks.CreateOpts{Name: "contract-default-" + uuid.NewString()[:8]}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = networks.Delete(client, n.ID).ExtractErr() })
	_, err = subnets.Create(client, subnets.CreateOpts{NetworkID: n.ID, CIDR: "10.246.0.0/24", IPVersion: gophercloud.IPv4}).Extract()
	require.NoError(t, err)
	p, err := ports.Create(client, ports.CreateOpts{NetworkID: n.ID}).Extract()
	require.NoError(t, err)
	require.Len(t, p.SecurityGroups, 1, "a port created without groups gets default")
	require.NoError(t, ports.Delete(client, p.ID).ExtractErr())
	return p.SecurityGroups[0]
}

func TestNeutronDefaultSecurityGroup_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	t.Run("as a member", func(t *testing.T) {
		member, _ := memberNetworkClient(t)
		def := defaultGroupOf(t, member)

		code, typ := neutronRaw(t, member, http.MethodDelete, "security-groups/"+def, "")
		assert.Equal(t, http.StatusConflict, code, "a member deleting an unused default")
		assert.Equal(t, "SecurityGroupCannotRemoveDefault", typ)
		_, err := groups.Get(member, def).Extract()
		assert.NoError(t, err, "default is still there")

		code, typ = neutronRaw(t, member, http.MethodPut, "security-groups/"+def, `{"security_group":{"name":"web"}}`)
		assert.Equal(t, http.StatusConflict, code, "renaming default")
		assert.Equal(t, "SecurityGroupCannotUpdateDefault", typ)

		code, typ = neutronRaw(t, member, http.MethodPost, "security-groups", `{"security_group":{"name":"default"}}`)
		assert.Equal(t, http.StatusConflict, code, "a second default")
		assert.Equal(t, "SecurityGroupDefaultAlreadyExists", typ)

		// Other names may repeat, as upstream's do.
		name := "contract-twin-" + uuid.NewString()[:8]
		for range 2 {
			g, err := groups.Create(member, groups.CreateOpts{Name: name}).Extract()
			require.NoError(t, err, "a repeated name is accepted")
			t.Cleanup(func() { _ = groups.Delete(member, g.ID).ExtractErr() })
		}
	})

	t.Run("as an administrator", func(t *testing.T) {
		admin := contract.SetupNetworkV2Client(t)
		code, typ := neutronRaw(t, admin, http.MethodPost, "security-groups", `{"security_group":{"name":"DEFAULT"}}`)
		assert.Equal(t, http.StatusConflict, code, "a second default, in another case")
		assert.Equal(t, "SecurityGroupDefaultAlreadyExists", typ)

		pages, err := groups.List(admin, groups.ListOpts{Name: "default", ProjectID: adminProjectID(t, admin)}).AllPages()
		require.NoError(t, err)
		found, err := groups.ExtractGroups(pages)
		require.NoError(t, err)
		require.Len(t, found, 1, "the administrator's project has one default")
		code, typ = neutronRaw(t, admin, http.MethodPut, "security-groups/"+found[0].ID, `{"security_group":{"name":"web"}}`)
		assert.Equal(t, http.StatusConflict, code, "renaming default as an administrator")
		assert.Equal(t, "SecurityGroupCannotUpdateDefault", typ)
	})
}

// adminProjectID is the project the seeded administrator's token is scoped to: the project of a
// group it creates.
func adminProjectID(t *testing.T, admin *gophercloud.ServiceClient) string {
	t.Helper()
	g, err := groups.Create(admin, groups.CreateOpts{Name: "contract-probe-" + uuid.NewString()[:8]}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = groups.Delete(admin, g.ID).ExtractErr() })
	require.NotEmpty(t, g.TenantID)
	return g.TenantID
}
