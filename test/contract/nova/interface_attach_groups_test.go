package nova_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
)

// SPEC-033 T023 (FR-013): an interface attached on a network gets the project's default group, as
// upstream, and the server then reports it.
func TestNovaInterfaceAttachOnNetworkGetsDefault_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)

	// The group and the second network first: cleanups run last in, first out, so the server, and the
	// port attached to it, are deleted before them. The server starts in the test's group, not in
	// default, so that the report names default only once the attached port carries it.
	sgName, _ := newTestSecurityGroup(t)
	second := createNetworkWithSubnet(t, token, "spec033-attach-net2", "10.244.100.0/24")
	first := createNetworkWithSubnet(t, token, "spec033-attach-net1", "10.244.101.0/24")
	server := createServerOnNetworkInGroup(t, token, "spec033-attach", first, sgName)
	require.NotEmpty(t, awaitServerPorts(t, server), "the server never got a port")
	require.NotContains(t, reportedGroups(t, server), "default", "before the attach")

	req, err := http.NewRequest(http.MethodPost, novaRootURL(t)+"/v2.1/servers/"+server+"/os-interface",
		bytes.NewBufferString(`{"interfaceAttachment":{"net_id":"`+second+`"}}`))
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, buf.String())
	var out struct {
		InterfaceAttachment struct {
			PortID string `json:"port_id"`
		} `json:"interfaceAttachment"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	require.NotEmpty(t, out.InterfaceAttachment.PortID)

	s, err := servers.Get(setupNovaClient(t), server).Extract()
	require.NoError(t, err)
	netClient := contract.SetupNeutronClient(t)
	pages, err := groups.List(netClient, groups.ListOpts{Name: "default", ProjectID: s.TenantID}).AllPages()
	require.NoError(t, err)
	defaults, err := groups.ExtractGroups(pages)
	require.NoError(t, err)
	require.Len(t, defaults, 1, "the project's default group")

	port, err := ports.Get(netClient, out.InterfaceAttachment.PortID).Extract()
	require.NoError(t, err)
	assert.Equal(t, []string{defaults[0].ID}, port.SecurityGroups, "the attached port's groups")
	assert.Contains(t, reportedGroups(t, server), "default")
}

// createServerOnNetworkInGroup boots a server on a network in one security group.
func createServerOnNetworkInGroup(t *testing.T, token, name, networkID, group string) string {
	t.Helper()
	return createServer(t, token, name, `,"networks":[{"uuid":"`+networkID+`"}],"security_groups":[{"name":"`+group+`"}]`)
}
