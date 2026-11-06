package nova_test

import (
	"os"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/stretchr/testify/require"
)

// SPEC-031 FR-019, through an OpenStack client: on a real-mode Lightstack whose libvirt socket
// does not exist, a create is accepted, ends ERROR with a fault saying why, and never turns
// ACTIVE. It used to turn ACTIVE with no guest behind it.
//
// Gated: the suite's usual target is a stub-mode Lightstack, where ACTIVE is right. Run it
// against the server quickstart.md §2 describes, with LIGHTSTACK_CONTRACT_NO_HYPERVISOR=1.
func TestNoHypervisorCreateEndsErrorWithFault(t *testing.T) {
	if os.Getenv("LIGHTSTACK_CONTRACT_NO_HYPERVISOR") != "1" {
		t.Skip("set LIGHTSTACK_CONTRACT_NO_HYPERVISOR=1 against a real-mode Lightstack with no libvirt socket")
	}
	client := setupNovaClient(t)

	server, err := servers.Create(client, servers.CreateOpts{
		Name:      "spec031-no-hypervisor",
		FlavorRef: "00000000-0000-0000-0000-000000000010",
		ImageRef:  testImageID(t),
	}).Extract()
	require.NoError(t, err)
	t.Cleanup(func() { _ = servers.Delete(client, server.ID).ExtractErr() })

	var s *servers.Server
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		s, err = servers.Get(client, server.ID).Extract()
		require.NoError(t, err)
		require.NotEqual(t, "ACTIVE", s.Status, "a server with no hypervisor behind it must never turn ACTIVE")
		if s.Status == "ERROR" {
			break
		}
	}
	require.Equal(t, "ERROR", s.Status, "the create must end in ERROR")
	require.NotEmpty(t, s.Fault.Message, "an ERROR server must say why in its fault")
}
