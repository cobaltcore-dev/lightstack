package neutron

import (
	"encoding/json"
	"net/http"
	"testing"
)

// SPEC-033 T265 (FR-072): a port whose device_owner is network:* — a router's or a DHCP port, trusted —
// carries no security group, as upstream's _ensure_default_security_group_on_port and
// _get_security_groups_on_port leave it: neither default when none is given, nor the groups given. It was
// given default, and so became a member every guest in default admitted all traffic from.
func TestTrustedPortCarriesNoGroup(t *testing.T) {
	requireDB(t)
	r, _, _ := newPortAPIRig(t)
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	network := seedNetwork(t, "spec033-t265")
	web := sgTestGroup(t, seededProjectID, "spec033-t265-web")
	for _, body := range []string{
		`{"port":{"network_id":"` + network + `","device_owner":"network:router_interface"}}`,
		`{"port":{"network_id":"` + network + `","device_owner":"network:router_interface","security_groups":` + quoted(web) + `}}`,
	} {
		code, resp := doJSONReq(r, http.MethodPost, "/v2.0/ports", body, admin)
		var out struct {
			Port struct {
				SecurityGroups []string `json:"security_groups"`
			} `json:"port"`
		}
		_ = json.Unmarshal([]byte(resp), &out)
		if code != http.StatusCreated || len(out.Port.SecurityGroups) != 0 {
			t.Errorf("%s: code=%d body=%s; want 201 with no security group", body, code, resp)
		}
	}
}

// SPEC-033 T266 (FR-059): an empty remote_group_id is no remote group, as upstream's rule validation reads a
// falsy one. It was stored as an empty string — 404 for an existing group on SQLite, 500 on PostgreSQL.
func TestEmptyRemoteGroupIsNone(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	group := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-t266"}}`)["id"].(string)
	code, body := doJSONReq(r, http.MethodPost, "/v2.0/security-group-rules",
		`{"security_group_rule":{"security_group_id":"`+group+`","direction":"ingress","ethertype":"IPv4","remote_group_id":""}}`, hdr)
	var out struct {
		Rule struct {
			RemoteGroupID *string `json:"remote_group_id"`
		} `json:"security_group_rule"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if code != http.StatusCreated || out.Rule.RemoteGroupID != nil {
		t.Errorf("code=%d body=%s; want 201 with remote_group_id null", code, body)
	}
}
