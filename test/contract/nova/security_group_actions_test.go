package nova_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	contract "github.com/cobaltcore-dev/lightstack/test/contract"
	"github.com/google/uuid"
)

// SPEC-033: addSecurityGroup and removeSecurityGroup change the server's ports, the only list of a
// server's groups and the one its firewall is built from. So every server here boots on a network,
// and each change is checked on the ports Neutron reports (contracts/compute-security-groups.md).

// newTestSecurityGroup creates a security group of the test's own and returns its name and id.
//
// Not "default": a server created without security groups is given "default", as upstream Nova does,
// so adding it again is a duplicate rather than an add. A unique name keeps a group left over by an
// earlier run from making the name ambiguous. Create the group BEFORE the server: cleanups run last
// in, first out, so the group is deleted after the server, once no port carries it (a group a port
// carries cannot be deleted).
func newTestSecurityGroup(t *testing.T) (name, id string) {
	t.Helper()
	netClient := contract.SetupNeutronClient(t)
	name = "contract-sg-" + uuid.NewString()[:8]
	sg, err := groups.Create(netClient, groups.CreateOpts{Name: name}).Extract()
	require.NoError(t, err, "creating the test security group")
	t.Cleanup(func() { _ = groups.Delete(netClient, sg.ID).ExtractErr() })
	return name, sg.ID
}

// serverOnNetwork boots a server on a network of its own and waits until its ports exist: the boot
// allocates them after the create is answered.
func serverOnNetwork(t *testing.T, token, name, cidr string) string {
	t.Helper()
	netID := createNetworkWithSubnet(t, token, name+"-net", cidr)
	id := createNamedServerOnNetwork(t, token, name, netID)
	require.NotEmpty(t, awaitServerPorts(t, id), "the server never got a port")
	return id
}

// serverPorts lists the server's ports as Neutron reports them.
func serverPorts(t *testing.T, serverID string) []ports.Port {
	t.Helper()
	pages, err := ports.List(contract.SetupNeutronClient(t), ports.ListOpts{DeviceID: serverID}).AllPages()
	require.NoError(t, err)
	list, err := ports.ExtractPorts(pages)
	require.NoError(t, err)
	return list
}

func awaitServerPorts(t *testing.T, serverID string) []ports.Port {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if list := serverPorts(t, serverID); len(list) > 0 || time.Now().After(deadline) {
			return list
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// reportedGroups returns the names in the server's security_groups.
func reportedGroups(t *testing.T, serverID string) []string {
	t.Helper()
	s, err := servers.Get(setupNovaClient(t), serverID).Extract()
	require.NoError(t, err)
	var names []string
	for _, g := range s.SecurityGroups {
		if n, ok := g["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names
}

func sgActionBody(verb, ref string) string {
	return `{"` + verb + `":{"name":"` + ref + `"}}`
}

// TestNovaAddSecurityGroup_Contract: the group reaches every port, and the server reports it.
func TestNovaAddSecurityGroup_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	sgName, sgID := newTestSecurityGroup(t)
	server := serverOnNetwork(t, token, "spec033-sg-add", "10.244.91.0/24")

	code, body := postAction(t, token, server, sgActionBody("addSecurityGroup", sgName))
	require.Equal(t, http.StatusAccepted, code, body)
	for _, p := range serverPorts(t, server) {
		assert.Contains(t, p.SecurityGroups, sgID, "port %s does not carry the added group", p.ID)
	}
	assert.Contains(t, reportedGroups(t, server), sgName)
}

// TestNovaAddSecurityGroupByID_Contract: a group named by its id is the same group.
func TestNovaAddSecurityGroupByID_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	sgName, sgID := newTestSecurityGroup(t)
	server := serverOnNetwork(t, token, "spec033-sg-add-id", "10.244.92.0/24")

	code, body := postAction(t, token, server, sgActionBody("addSecurityGroup", sgID))
	require.Equal(t, http.StatusAccepted, code, body)
	for _, p := range serverPorts(t, server) {
		assert.Contains(t, p.SecurityGroups, sgID, "port %s does not carry the added group", p.ID)
	}
	assert.Contains(t, reportedGroups(t, server), sgName)
}

// TestNovaAddSecurityGroupOfAnotherProject_Contract (SPEC-033 round 2, T105; FR-004, research R52): the
// administrator adds another project's group by id, as upstream's; by name it is not found, since a
// name resolves in the server's project (register entry 5).
func TestNovaAddSecurityGroupOfAnotherProject_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	identity, err := openstack.NewIdentityV3(contract.SetupOpenStackClient(t), gophercloud.EndpointOpts{})
	require.NoError(t, err)
	project, err := projects.Create(identity, projects.CreateOpts{
		Name: "spec033-sg-owner-" + uuid.NewString()[:8], DomainID: "default",
	}).Extract()
	require.NoError(t, err, "creating the group's project")
	t.Cleanup(func() { _ = projects.Delete(identity, project.ID).ExtractErr() })
	netClient := contract.SetupNeutronClient(t)
	name := "contract-sg-foreign-" + uuid.NewString()[:8]
	sg, err := groups.Create(netClient, groups.CreateOpts{Name: name, ProjectID: project.ID}).Extract()
	require.NoError(t, err, "creating the other project's group")
	t.Cleanup(func() { _ = groups.Delete(netClient, sg.ID).ExtractErr() })
	server := serverOnNetwork(t, token, "spec033-sg-foreign", "10.244.104.0/24")

	code, body := postAction(t, token, server, sgActionBody("addSecurityGroup", name))
	assert.Equal(t, http.StatusNotFound, code, "by name: %s", body)
	code, body = postAction(t, token, server, sgActionBody("addSecurityGroup", sg.ID))
	require.Equal(t, http.StatusAccepted, code, "by id: %s", body)
	for _, p := range serverPorts(t, server) {
		assert.Contains(t, p.SecurityGroups, sg.ID, "port %s does not carry the other project's group", p.ID)
	}
	code, body = postAction(t, token, server, sgActionBody("removeSecurityGroup", sg.ID))
	require.Equal(t, http.StatusAccepted, code, "remove by id: %s", body)
}

// TestNovaAddSecurityGroupDuplicate_Contract: adding a group a port already carries answers 400.
//
// Upstream answers 400, not 409: Nova appends the group's id to the port's list, Neutron rejects the
// duplicate and Nova maps that to SecurityGroupCannotBeApplied. A server created without groups gets
// "default" on its port, so adding it once is the duplicate.
func TestNovaAddSecurityGroupDuplicate_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	server := serverOnNetwork(t, token, "spec033-sg-dup", "10.244.93.0/24")

	code, body := postAction(t, token, server, sgActionBody("addSecurityGroup", "default"))
	assert.Equal(t, http.StatusBadRequest, code, body)
}

// TestNovaRemoveSecurityGroup_Contract: the group leaves every port, and "default" stays.
func TestNovaRemoveSecurityGroup_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	sgName, sgID := newTestSecurityGroup(t)
	server := serverOnNetwork(t, token, "spec033-sg-remove", "10.244.94.0/24")

	code, body := postAction(t, token, server, sgActionBody("addSecurityGroup", sgName))
	require.Equal(t, http.StatusAccepted, code, body)
	for _, p := range serverPorts(t, server) {
		require.Contains(t, p.SecurityGroups, sgID, "before the remove, port %s must carry the group", p.ID)
	}
	code, body = postAction(t, token, server, sgActionBody("removeSecurityGroup", sgName))
	require.Equal(t, http.StatusAccepted, code, body)
	for _, p := range serverPorts(t, server) {
		assert.NotContains(t, p.SecurityGroups, sgID, "port %s still carries the removed group", p.ID)
		assert.NotEmpty(t, p.SecurityGroups, "port %s lost default too", p.ID)
	}
	assert.NotContains(t, reportedGroups(t, server), sgName)
}

// TestNovaSecurityGroupActionsWithoutPorts_Contract: a server with no port has nothing to change,
// and both actions answer 404, as upstream ("could not be found as device id on any ports").
func TestNovaSecurityGroupActionsWithoutPorts_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	sgName, _ := newTestSecurityGroup(t)
	server := createNamedServer(t, token, "spec033-sg-noport")

	for _, verb := range []string{"addSecurityGroup", "removeSecurityGroup"} {
		code, body := postAction(t, token, server, sgActionBody(verb, sgName))
		assert.Equal(t, http.StatusNotFound, code, "%s: %s", verb, body)
		assert.Contains(t, body, "could not be found as device id on any ports", verb)
	}
}

// hasSecurityGroupsKey reports whether the server's show carries a security_groups key at all.
func hasSecurityGroupsKey(t *testing.T, token, serverID string) bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, novaRootURL(t)+"/v2.1/servers/"+serverID, nil)
	require.NoError(t, err)
	req.Header.Set("X-Auth-Token", token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Server map[string]json.RawMessage `json:"server"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	_, present := out.Server["security_groups"]
	return present
}

// TestNovaRemoveLastSecurityGroup_Contract: removing a server's only group answers 202, as upstream,
// and leaves its ports with no group (SPEC-033 FR-005).
func TestNovaRemoveLastSecurityGroup_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	server := serverOnNetwork(t, token, "spec033-sg-last", "10.244.98.0/24")

	code, body := postAction(t, token, server, sgActionBody("removeSecurityGroup", "default"))
	require.Equal(t, http.StatusAccepted, code, body)
	netClient := contract.SetupNeutronClient(t)
	for _, p := range serverPorts(t, server) {
		port, err := ports.Get(netClient, p.ID).Extract()
		require.NoError(t, err)
		assert.Empty(t, port.SecurityGroups, "port %s still carries a group", p.ID)
	}
	assert.False(t, hasSecurityGroupsKey(t, token, server), "show still has security_groups")
}

// TestNovaReplaceOnlySecurityGroup_Contract replaces a server's only group in Terraform's order:
// remove the old one, then add the new one. Both answer 202, and the port ends with only the new one.
func TestNovaReplaceOnlySecurityGroup_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	aName, aID := newTestSecurityGroup(t)
	bName, bID := newTestSecurityGroup(t)
	server := serverOnNetwork(t, token, "spec033-sg-replace", "10.244.99.0/24")

	// Make a the only group.
	code, body := postAction(t, token, server, sgActionBody("addSecurityGroup", aName))
	require.Equal(t, http.StatusAccepted, code, body)
	code, body = postAction(t, token, server, sgActionBody("removeSecurityGroup", "default"))
	require.Equal(t, http.StatusAccepted, code, body)
	for _, p := range serverPorts(t, server) {
		require.Equal(t, []string{aID}, p.SecurityGroups, "port %s before the replace", p.ID)
	}

	code, body = postAction(t, token, server, sgActionBody("removeSecurityGroup", aName))
	require.Equal(t, http.StatusAccepted, code, "remove the only group: %s", body)
	code, body = postAction(t, token, server, sgActionBody("addSecurityGroup", bName))
	require.Equal(t, http.StatusAccepted, code, "add the new group: %s", body)
	for _, p := range serverPorts(t, server) {
		assert.Equal(t, []string{bID}, p.SecurityGroups, "port %s after the replace", p.ID)
	}
}

// TestNovaCreateWithSecurityGroupReachesPort_Contract (locks, SPEC-033 FR-012): a server created on a
// network with a group gets that group on its port, and not default; one created without groups gets
// default only.
func TestNovaCreateWithSecurityGroupReachesPort_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)
	token := issueToken(t)
	sgName, sgID := newTestSecurityGroup(t)

	t.Run("with a group", func(t *testing.T) {
		netID := createNetworkWithSubnet(t, token, "spec033-create-sg-net", "10.244.102.0/24")
		server := createServerOnNetworkInGroup(t, token, "spec033-create-sg", netID, sgName)
		list := awaitServerPorts(t, server)
		require.NotEmpty(t, list, "the server never got a port")
		for _, p := range list {
			assert.Equal(t, []string{sgID}, p.SecurityGroups, "port %s", p.ID)
		}
	})

	t.Run("without groups", func(t *testing.T) {
		server := serverOnNetwork(t, token, "spec033-create-default", "10.244.103.0/24")
		names := reportedGroups(t, server)
		assert.Equal(t, []string{"default"}, names)
		for _, p := range serverPorts(t, server) {
			assert.Len(t, p.SecurityGroups, 1, "port %s carries default only", p.ID)
			assert.NotContains(t, p.SecurityGroups, sgID, "port %s", p.ID)
		}
	})
}

// TestNovaChangePassword_Contract tests POST /v2.1/servers/:id/action (changePassword)
func TestNovaChangePassword_Contract(t *testing.T) {
	skipIfLightstackNotRunning(t)

	client := setupNovaClient(t)

	// Create test server
	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "password-test",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  testImageID(t),
	}).Extract()
	require.NoError(t, err)
	defer servers.Delete(client, server.ID)

	// Wait for server to be ACTIVE (needed for password change)
	for i := 0; i < 30; i++ {
		s, _ := servers.Get(client, server.ID).Extract()
		if s.Status == "ACTIVE" {
			break
		}
		if i == 29 {
			t.Skip("Server did not become ACTIVE in time")
		}
		time.Sleep(time.Second)
	}

	// Change admin password
	actionBody := `{
		"changePassword": {
			"adminPass": "newSecurePassword123"
		}
	}`

	url := client.ServiceURL("servers", server.ID, "action")
	req, err := http.NewRequest("POST", url, strings.NewReader(actionBody))
	require.NoError(t, err)

	req.Header.Set("X-Auth-Token", client.TokenID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Verify response contains no error
	respBody, _ := io.ReadAll(resp.Body)
	if len(respBody) > 0 {
		var result map[string]interface{}
		json.Unmarshal(respBody, &result)
		assert.Nil(t, result["error"])
	}
}
