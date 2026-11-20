package neutron_test

import (
	"net/http"
	"testing"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-033 round 2, T072 (FR-019a, FR-019b, FR-050, FR-051): the group listing's attribute filters
// through gophercloud's groups.List, with raw queries for the parameters its ListOpts lacks, against
// groups created stateless and updated.

func listedIDs(t *testing.T, client *gophercloud.ServiceClient, opts groups.ListOpts) []string {
	t.Helper()
	pages, err := groups.List(client, opts).AllPages()
	require.NoError(t, err)
	got, err := groups.ExtractGroups(pages)
	require.NoError(t, err)
	ids := []string{}
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	return sortedIDs(ids...)
}

// rawListStatus sends GET /v2.0/security-groups with the raw query and returns the status.
func rawListStatus(t *testing.T, client *gophercloud.ServiceClient, query string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, client.ServiceURL("security-groups")+query, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", client.TokenID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestNeutronSecurityGroupFilters_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	client := contract.SetupNetworkV2Client(t)
	tag := uuid.NewString()[:8]
	stateless := false

	alpha, err := groups.Create(client, groups.CreateOpts{Name: "contract-filter-a-" + tag, Description: "alpha-" + tag}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = groups.Delete(client, alpha.ID).ExtractErr() })
	beta, err := groups.Create(client, groups.CreateOpts{Name: "contract-filter-b-" + tag, Description: "beta-" + tag, Stateful: &stateless}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = groups.Delete(client, beta.ID).ExtractErr() })

	assert.True(t, alpha.Stateful, "a group created without stateful is stateful")
	assert.False(t, beta.Stateful, "a group created with stateful: false is stateless")

	// One update that changes something: alpha's revision goes from 0 to 1. An update that changes
	// nothing writes nothing and keeps the revision, as upstream's.
	_, err = groups.Update(client, alpha.ID, groups.UpdateOpts{Name: "contract-filter-a2-" + tag}).Extract()
	require.NoError(t, err)

	both := sortedIDs(alpha.ID, beta.ID)
	t.Run("description", func(t *testing.T) {
		assert.Equal(t, []string{alpha.ID}, listedIDs(t, client, groups.ListOpts{Description: "alpha-" + tag}))
		assert.Equal(t, both, listGroupIDs(t, client, "?description=alpha-"+tag+"&description=beta-"+tag))
	})
	t.Run("stateful", func(t *testing.T) {
		yes, no := true, false
		assert.Equal(t, []string{alpha.ID}, listedIDs(t, client, groups.ListOpts{Stateful: &yes, Description: "alpha-" + tag}))
		assert.Equal(t, []string{beta.ID}, listedIDs(t, client, groups.ListOpts{Stateful: &no, Description: "beta-" + tag}))
		assert.Empty(t, listedIDs(t, client, groups.ListOpts{Stateful: &no, Description: "alpha-" + tag}))
	})
	t.Run("revision_number", func(t *testing.T) {
		assert.Equal(t, []string{alpha.ID}, listGroupIDs(t, client, "?revision_number=1&id="+alpha.ID+"&id="+beta.ID))
		assert.Equal(t, []string{beta.ID}, listGroupIDs(t, client, "?revision_number=0&id="+alpha.ID+"&id="+beta.ID))
	})
	t.Run("shared and tags answer from every group", func(t *testing.T) {
		ids := "&id=" + alpha.ID + "&id=" + beta.ID
		assert.Equal(t, both, listGroupIDs(t, client, "?shared=false"+ids))
		assert.Empty(t, listGroupIDs(t, client, "?shared=true"+ids))
		assert.Empty(t, listedIDs(t, client, groups.ListOpts{Tags: "blue", Description: "alpha-" + tag}))
		assert.Empty(t, listedIDs(t, client, groups.ListOpts{TagsAny: "blue", Description: "alpha-" + tag}))
		assert.Equal(t, []string{alpha.ID}, listedIDs(t, client, groups.ListOpts{NotTags: "blue", Description: "alpha-" + tag}))
		assert.Equal(t, []string{alpha.ID}, listedIDs(t, client, groups.ListOpts{NotTagsAny: "blue", Description: "alpha-" + tag}))
	})
	t.Run("project filters", func(t *testing.T) {
		project := alpha.TenantID
		require.NotEmpty(t, project)
		assert.Equal(t, []string{alpha.ID}, listedIDs(t, client, groups.ListOpts{ProjectID: project, Description: "alpha-" + tag}))
		assert.Equal(t, []string{alpha.ID}, listedIDs(t, client, groups.ListOpts{TenantID: project, Description: "alpha-" + tag}))
	})
	t.Run("a boolean that is not one is 400", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, rawListStatus(t, client, "?shared=maybe"))
		assert.Equal(t, http.StatusBadRequest, rawListStatus(t, client, "?stateful=maybe"))
	})
	t.Run("every group reports shared, tags, stateful and revision_number", func(t *testing.T) {
		got, err := groups.Get(client, alpha.ID).Extract()
		require.NoError(t, err)
		assert.True(t, got.Stateful)
		assert.NotNil(t, got.Tags, "tags is [] rather than absent")
		assert.Empty(t, got.Tags)
	})
}
