package neutron_test

import (
	"testing"

	contract "github.com/cobaltcore-dev/o3k/test/contract"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/quotas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNeutronGetQuota_Contract(t *testing.T) {
	skipIfO3KNotRunning(t)

	client := contract.SetupNeutronClient(t)

	// Get quota for the current project.
	//
	// The second argument is a PROJECT ID, not a token. This passed
	// client.ProviderClient.TokenID until 2026-08-03, which sent the whole JWT as the
	// path segment — GET /v2.0/quotas/eyJhbGci… — so O3K correctly answered 404
	// "project could not be found" and the test had never once exercised the endpoint.
	projectID := contract.GetProjectIDFromToken(t, client)
	quota, err := quotas.Get(client, projectID).Extract()
	require.NoError(t, err)
	require.NotNil(t, quota)

	// Verify quota structure
	assert.GreaterOrEqual(t, quota.Network, 0, "Network quota should be >= 0")
	assert.GreaterOrEqual(t, quota.Subnet, 0, "Subnet quota should be >= 0")
	assert.GreaterOrEqual(t, quota.Port, 0, "Port quota should be >= 0")
	assert.GreaterOrEqual(t, quota.Router, 0, "Router quota should be >= 0")
	assert.GreaterOrEqual(t, quota.FloatingIP, 0, "FloatingIP quota should be >= 0")
	assert.GreaterOrEqual(t, quota.SecurityGroup, 0, "SecurityGroup quota should be >= 0")
	assert.GreaterOrEqual(t, quota.SecurityGroupRule, 0, "SecurityGroupRule quota should be >= 0")
}
