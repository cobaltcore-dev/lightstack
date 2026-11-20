package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Branch review SRC-7 (2026-11-23-review.md): port create and update stored any device_owner a member
// sent. A network: owner gives a port a role its network plays — the network's DHCP port, a router's
// port — so on a shared network a member could plant the network's DHCP port, and deleting it took DHCP
// and metadata from every tenant there. Upstream's create_port:device_owner and update_port:device_owner
// admit a network: owner from administrators and the network's own project only
// (neutron/conf/policies/port.py) (SPEC-033 FR-062).

func TestNetworkDeviceOwnersAreTheNetworksOwners(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	owner := createTestProject(t, "src7-network-owner")
	shared := seedExternalNetwork(t, "src7-shared-"+uuid.NewString()[:6], owner, true, false)
	gateway := "10.88.0.1"
	seedSubnet(t, shared, "src7-shared", "10.88.0.0/24", &gateway, 4, false)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE network_id = $1", shared) })
	member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	networkOwner := map[string]string{"X-Test-Project": owner, "X-Test-Roles": "member"}
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	create := func(hdr map[string]string, deviceOwner string) (int, string) {
		return doJSONReq(r, http.MethodPost, "/v2.0/ports",
			`{"port":{"network_id":"`+shared+`","device_owner":"`+deviceOwner+`","device_id":"`+shared+`"}}`, hdr)
	}
	refused := func(t *testing.T, code int, body, rule string) {
		t.Helper()
		want := "(rule:" + rule + ":device_owner) is disallowed by policy"
		if code != http.StatusForbidden {
			t.Fatalf("code=%d body=%s; want 403", code, body)
		}
		if e := neutronError(t, body); e["type"] != "PolicyNotAuthorized" || e["message"] != want {
			t.Errorf("refusal %v: %v; want PolicyNotAuthorized: %s", e["type"], e["message"], want)
		}
	}

	t.Run("a member may not give a port on another project's network a network owner", func(t *testing.T) {
		code, body := create(member, "network:dhcp")
		refused(t, code, body, "create_port")
		var n int
		if err := testPool.QueryRow(context.Background(),
			"SELECT count(*) FROM ports WHERE network_id = $1 AND device_owner = 'network:dhcp'", shared).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d network:dhcp ports stored (%v); want none", n, err)
		}
	})

	t.Run("nor change a port's owner to one", func(t *testing.T) {
		code, body := create(member, "compute:nova")
		if code != http.StatusCreated {
			t.Fatalf("a port of the member's own: code=%d body=%s; want 201", code, body)
		}
		id := createdID(t, body, "port")
		code, body = doJSONReq(r, http.MethodPut, "/v2.0/ports/"+id, `{"port":{"device_owner":"network:router_interface"}}`, member)
		refused(t, code, body, "update_port")
		var deviceOwner string
		if err := testPool.QueryRow(context.Background(), "SELECT device_owner FROM ports WHERE id = $1", id).Scan(&deviceOwner); err != nil || deviceOwner != "compute:nova" {
			t.Errorf("device_owner after the refusal = %q (%v); want compute:nova", deviceOwner, err)
		}
	})

	t.Run("the network's owner and an administrator may", func(t *testing.T) {
		if code, body := create(networkOwner, "network:dhcp"); code != http.StatusCreated {
			t.Errorf("the network owner's network:dhcp port: code=%d body=%s; want 201", code, body)
		}
		if code, body := create(admin, "network:router_interface"); code != http.StatusCreated {
			t.Errorf("an administrator's network:router_interface port: code=%d body=%s; want 201", code, body)
		}
	})
}
