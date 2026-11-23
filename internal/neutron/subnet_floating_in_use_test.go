package neutron

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T186 (FR-052): a floating IP holds an address of its external subnet as a port
// does. Lightstack's floating IP is a row of its own where upstream's is a network:floatingip port, which
// upstream's in-use check counts; deleting a subnet one of them holds an address of answers 409
// SubnetInUse, and once it is gone the subnet deletes.

func TestSubnetHeldByAFloatingIPIsInUse(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	prefix := fmt.Sprintf("198.51.%d", rand.IntN(250))
	network := seedExternalNetwork(t, "spec033-src14-"+uuid.NewString()[:6], seededProjectID, false, true)
	subnet := seedSubnet(t, network, "spec033-src14", prefix+".0/25", nil, 4, false)
	other := seedSubnet(t, network, "spec033-src14-other", prefix+".128/25", nil, 4, false)
	cleanupFloatingIPsOn(t, network)
	floating := uuid.NewString()
	mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, status)
		VALUES ($1, $2, $3, $4, 'DOWN')`, floating, seededProjectID, network, prefix+".5")

	code, body := doReq(r, http.MethodDelete, "/v2.0/subnets/"+subnet, admin)
	if code != http.StatusConflict || neutronErrorType(t, body) != "SubnetInUse" {
		t.Errorf("delete the floating IP's subnet: code=%d body=%s; want 409 SubnetInUse", code, body)
	}
	if code, body := doReq(r, http.MethodDelete, "/v2.0/subnets/"+other, admin); code != http.StatusNoContent {
		t.Errorf("delete the network's other subnet: code=%d body=%s; want 204, the floating IP holds none of it", code, body)
	}
	mustExec(t, "DELETE FROM floating_ips WHERE id = $1", floating)
	if code, body := doReq(r, http.MethodDelete, "/v2.0/subnets/"+subnet, admin); code != http.StatusNoContent {
		t.Errorf("delete once the floating IP is gone: code=%d body=%s; want 204", code, body)
	}
}
