package contract_test

import (
	"testing"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/keypairs"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Nova server-detail read-back. The unmodified OpenStack provider reads key_name and security_groups back from GET
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

	kp, err := keypairs.Create(nova, keypairs.CreateOpts{Name: "readback-key"}).Extract()
	require.NoError(t, err)
	defer func() { _ = keypairs.Delete(nova, kp.Name, nil).ExtractErr() }()

	img, err := images.Create(glance, images.CreateOpts{
		Name: "readback-img", ContainerFormat: "bare", DiskFormat: "raw",
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = images.Delete(glance, img.ID).ExtractErr() }()
	UploadTestImageData(t, glance, img.ID) // server create refuses an image that is not active

	base := servers.CreateOpts{Name: "readback-kn", FlavorRef: flavorM1Small, ImageRef: img.ID}
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
		Name: "readback-sg-img", ContainerFormat: "bare", DiskFormat: "raw",
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = images.Delete(glance, img.ID).ExtractErr() }()
	UploadTestImageData(t, glance, img.ID) // server create refuses an image that is not active

	// On a network: a server's groups are its ports' groups, and a server without a port reports
	// none (SPEC-033 FR-014, FR-015).
	network := SetupNetworkV2Client(t)
	net, err := networks.Create(network, networks.CreateOpts{Name: "readback-sg-net"}).Extract()
	require.NoError(t, err)
	defer func() { _ = networks.Delete(network, net.ID).ExtractErr() }()
	_, err = subnets.Create(network, subnets.CreateOpts{NetworkID: net.ID, CIDR: "10.245.13.0/24", IPVersion: gophercloud.IPv4}).Extract()
	require.NoError(t, err)

	srv, err := servers.Create(nova, servers.CreateOpts{
		Name: "readback-sg", FlavorRef: flavorM1Small, ImageRef: img.ID,
		SecurityGroups: []string{"default"}, Networks: []servers.Network{{UUID: net.ID}},
	}).Extract()
	require.NoError(t, err)
	defer func() { _ = servers.Delete(nova, srv.ID).ExtractErr() }()

	// The boot allocates the port after the create is answered.
	var got *servers.Server
	require.Eventually(t, func() bool {
		got, err = servers.Get(nova, srv.ID).Extract()
		return err == nil && len(got.SecurityGroups) > 0
	}, 10*time.Second, 100*time.Millisecond, "the server never reported its port's groups")
	assert.Contains(t, sgNames(got.SecurityGroups), "default",
		"server-detail must read back a non-empty security_groups")
}
