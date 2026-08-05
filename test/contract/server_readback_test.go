package contract_test

import (
	"testing"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/keypairs"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-017 (#524) — Nova server-detail read-back (finding F1). The unmodified
// OpenStack provider reads key_name and security_groups back from GET
// /v2.1/servers/{id}; populating them is what makes a repeated `terraform apply` a
// no-op. These contract tests assert both fields round-trip through real clients.

func sgNames(sgs []map[string]any) []string {
	var names []string
	for _, sg := range sgs {
		if n, ok := sg["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names
}

func TestNovaServerReadBack_KeyName_Contract(t *testing.T) {
	SkipIfLightstackNotRunning(t)
	nova := SetupComputeV2Client(t)
	glance := SetupImageV2Client(t)

	kp, err := keypairs.Create(nova, keypairs.CreateOpts{Name: "spec017-key"}).Extract()
	require.NoError(t, err)
	defer func() { _ = keypairs.Delete(nova, kp.Name, nil).ExtractErr() }()

	img, err := images.Create(glance, images.CreateOpts{
		Name: "spec017-img", ContainerFormat: "bare", DiskFormat: "raw",
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = images.Delete(glance, img.ID).ExtractErr() }()

	base := servers.CreateOpts{Name: "spec017-kn", FlavorRef: flavorM1Small, ImageRef: img.ID}
	srv, err := servers.Create(nova, keypairs.CreateOptsExt{CreateOptsBuilder: base, KeyName: kp.Name}).Extract()
	require.NoError(t, err)
	defer func() { _ = servers.Delete(nova, srv.ID).ExtractErr() }()

	got, err := servers.Get(nova, srv.ID).Extract()
	require.NoError(t, err)
	assert.Equal(t, kp.Name, got.KeyName, "server-detail must read back the key_name used at create")
}

func TestNovaServerReadBack_SecurityGroups_Contract(t *testing.T) {
	SkipIfLightstackNotRunning(t)
	nova := SetupComputeV2Client(t)
	glance := SetupImageV2Client(t)

	img, err := images.Create(glance, images.CreateOpts{
		Name: "spec017-sg-img", ContainerFormat: "bare", DiskFormat: "raw",
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = images.Delete(glance, img.ID).ExtractErr() }()

	srv, err := servers.Create(nova, servers.CreateOpts{
		Name: "spec017-sg", FlavorRef: flavorM1Small, ImageRef: img.ID,
		SecurityGroups: []string{"default"},
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = servers.Delete(nova, srv.ID).ExtractErr() }()

	got, err := servers.Get(nova, srv.ID).Extract()
	require.NoError(t, err)
	assert.Contains(t, sgNames(got.SecurityGroups), "default",
		"server-detail must read back a non-empty security_groups")
}
