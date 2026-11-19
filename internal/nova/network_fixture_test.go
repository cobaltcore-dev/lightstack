package nova

import (
	"context"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 T005: Nova tests against the real Neutron service in stub mode, on the same test database
// (research R16). A server's groups are its ports' groups, so these tests build servers that have
// ports. Stub mode has no hypervisor and so no boot goroutine: the tests allocate the ports themselves.

// newNovaWithNetwork returns Nova's routes behind the test auth middleware, the Nova service, and the
// Neutron service it uses. A test that needs to inject a failure wraps the Neutron service and hands
// the wrapper to svc.SetNeutronService.
func newNovaWithNetwork(t *testing.T) (*gin.Engine, *Service, *neutron.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := NewService(testPool, "", "stub", nil)
	network := neutron.NewService(testPool, "stub", nil)
	svc.SetNeutronService(network)
	r := gin.New()
	r.Use(testAuthMiddleware())
	svc.RegisterRoutes(&r.RouterGroup)
	return r, svc, network
}

// createTestNetwork inserts a network and a /24 subnet in projectID. Its cleanup removes the ports on
// the network with their interface attachments, then the subnet and the network.
func createTestNetwork(t *testing.T, projectID string) (networkID, subnetID string) {
	t.Helper()
	ctx := t.Context()
	networkID, subnetID = uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(ctx, `INSERT INTO networks (id, name, project_id, status)
		VALUES ($1, $2, $3, 'ACTIVE')`, networkID, "spec033-net-"+networkID[:8], projectID); err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, q := range []string{
			"DELETE FROM interface_attachments WHERE port_id IN (SELECT id FROM ports WHERE network_id = $1)",
			"DELETE FROM ports WHERE network_id = $1",
			"DELETE FROM subnets WHERE network_id = $1",
			"DELETE FROM networks WHERE id = $1",
		} {
			_, _ = testPool.Exec(bg, q, networkID)
		}
	})
	if _, err := testPool.Exec(ctx, `INSERT INTO subnets (id, name, network_id, project_id, cidr)
		VALUES ($1, $2, $3, $4, '10.33.0.0/24')`, subnetID, "spec033-subnet-"+subnetID[:8], networkID, projectID); err != nil {
		t.Fatalf("create subnet: %v", err)
	}
	return networkID, subnetID
}

// createTestGroup inserts a security group in projectID and removes it again in cleanup.
func createTestGroup(t *testing.T, projectID, name string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(t.Context(),
		"INSERT INTO security_groups (id, name, project_id, description) VALUES ($1, $2, $3, 'spec033 test')",
		id, name, projectID); err != nil {
		t.Fatalf("create security group %q: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM security_groups WHERE id = $1", id)
	})
	return id
}

// createTestServer creates a server through the API in projectID, booting an image seeded for it, and
// removes it in cleanup.
func createTestServer(t *testing.T, r *gin.Engine, projectID, body string) string {
	t.Helper()
	code, resp := doReq(r, http.MethodPost, "/v2.1/servers", withImage(t, projectID, body), testAuthHdr(projectID, seededUserID))
	if code != http.StatusAccepted {
		t.Fatalf("create server: code=%d body=%s", code, resp)
	}
	id, _ := serverBody(t, resp)["id"].(string)
	if id == "" {
		t.Fatalf("create server: no id in %s", resp)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id)
	})
	return id
}

// allocateTestPort allocates a port for serverID on networkID, the way the boot does, carrying
// groupIDs (none gives the project's default).
func allocateTestPort(t *testing.T, network *neutron.Service, networkID, projectID, serverID string, groupIDs []string) string {
	t.Helper()
	info, err := network.AllocatePortForInstance(t.Context(), networkID, projectID, serverID, groupIDs, "", false)
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	port, ok := info.(*neutron.PortInfo)
	if !ok || port.ID == "" {
		t.Fatalf("allocate port: unexpected result %#v", info)
	}
	return port.ID
}

// portGroupIDs reads a port's groups straight from the table the filter is built from.
func portGroupIDs(t *testing.T, portID string) []string {
	t.Helper()
	ids, err := neutron.PortSecurityGroupIDs(t.Context(), testPool, portID)
	if err != nil {
		t.Fatalf("read the groups of port %s: %v", portID, err)
	}
	return ids
}
