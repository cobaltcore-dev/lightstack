package neutron_test

import (
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-033 T040 (FR-008c): a group a port carries cannot be deleted (409), as upstream; once the port
// is gone it can (204), and then it is gone (404).
func TestNeutronSecurityGroupDeleteInUse_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := contract.SetupNetworkV2Client(t)
	group := sgFixture(t, client)
	netID := networkFixture(t, client, "10.245.12.0/24")
	port, err := ports.Create(client, ports.CreateOpts{NetworkID: netID, SecurityGroups: &[]string{group.ID}}).Extract()
	require.NoError(t, err)

	err = groups.Delete(client, group.ID).ExtractErr()
	var conflict gophercloud.ErrDefault409
	assert.ErrorAs(t, err, &conflict, "deleting a group a port carries")

	require.NoError(t, ports.Delete(client, port.ID).ExtractErr())
	assert.NoError(t, groups.Delete(client, group.ID).ExtractErr(), "deleting the group once no port carries it")

	err = groups.Delete(client, group.ID).ExtractErr()
	var notFound gophercloud.ErrDefault404
	assert.ErrorAs(t, err, &notFound, "deleting it again")
}
