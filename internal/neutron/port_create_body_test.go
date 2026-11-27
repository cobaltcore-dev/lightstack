package neutron

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T217: a port's create answers the body its show does — create, show, update and
// list render a port through one function — with its project_id, and its binding:host_id for an
// administrator.
func TestPortCreateAnswersItsShow(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	for _, tc := range []struct {
		name    string
		hdr     map[string]string
		binding bool
	}{
		{"a member", map[string]string{"X-Test-Project": seededProjectID}, false},
		{"an administrator", map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := seedNetwork(t, "spec033-src40-"+uuid.NewString()[:6])
			seedSubnet(t, network, "spec033-src40", "10.95.40.0/24", nil, 4, false)
			port := func(want int, method, path, request string) map[string]any {
				t.Helper()
				code, body := doJSONReq(r, method, path, request, tc.hdr)
				var out struct {
					Port map[string]any `json:"port"`
				}
				if code != want || json.Unmarshal([]byte(body), &out) != nil {
					t.Fatalf("%s %s: code=%d body=%s; want %d and a port", method, path, code, body, want)
				}
				return out.Port
			}
			created := port(http.StatusCreated, http.MethodPost, "/v2.0/ports", `{"port":{"network_id":"`+network+`","name":"src40"}}`)
			shown := port(http.StatusOK, http.MethodGet, "/v2.0/ports/"+created["id"].(string), "")
			if !reflect.DeepEqual(created, shown) {
				t.Errorf("the create's port:\n%v\nits show:\n%v", created, shown)
			}
			_, binding := created["binding:host_id"]
			if created["project_id"] != seededProjectID || binding != tc.binding {
				t.Errorf("the create's project_id %v, binding:host_id given %v; want %s and %v", created["project_id"], binding, seededProjectID, tc.binding)
			}
		})
	}
}
