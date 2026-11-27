package neutron

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T224: a subnet's create and update answer the body its show does — create,
// show, update and listing render a subnet through one function — read back as stored.
func TestSubnetCreateAndUpdateAnswerTheirShow(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}
	network := seedNetwork(t, "spec033-src47-"+uuid.NewString()[:6])
	subnet := func(want int, method, path, request string) map[string]any {
		t.Helper()
		code, body := doJSONReq(r, method, path, request, hdr)
		var out struct {
			Subnet map[string]any `json:"subnet"`
		}
		if code != want || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("%s %s: code=%d body=%s; want %d and a subnet", method, path, code, body, want)
		}
		return out.Subnet
	}
	created := subnet(http.StatusCreated, http.MethodPost, "/v2.0/subnets",
		`{"subnet":{"network_id":"`+network+`","name":"src47","ip_version":4,"cidr":"10.95.47.0/24","enable_dhcp":false,"allocation_pools":[{"start":"10.95.47.10","end":"10.95.47.20"}]}}`)
	id := created["id"].(string)
	if shown := subnet(http.StatusOK, http.MethodGet, "/v2.0/subnets/"+id, ""); !reflect.DeepEqual(created, shown) {
		t.Errorf("the create's subnet:\n%v\nits show:\n%v", created, shown)
	}
	updated := subnet(http.StatusOK, http.MethodPut, "/v2.0/subnets/"+id, `{"subnet":{"name":"src47-renamed"}}`)
	if shown := subnet(http.StatusOK, http.MethodGet, "/v2.0/subnets/"+id, ""); !reflect.DeepEqual(updated, shown) || updated["name"] != "src47-renamed" {
		t.Errorf("the update's subnet:\n%v\nits show:\n%v", updated, shown)
	}
}
