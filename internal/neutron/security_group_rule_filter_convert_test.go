package neutron

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// SPEC-033 T267 (FR-019a): the rule listing's filters read their values as upstream's
// get_filters_from_dict does, through each attribute's converter: ethertype in any case, a protocol by any
// case of its name or "any" for none, a remote prefix as netaddr prints it, a port as a number in range
// (400 otherwise). The raw values were compared with the stored canonical ones, and listed nothing.
func TestRuleListingFiltersConvertTheirValues(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	group := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-t267"}}`)["id"].(string)
	rule := func(body string) string {
		code, resp := doJSONReq(r, http.MethodPost, "/v2.0/security-group-rules",
			`{"security_group_rule":{"security_group_id":"`+group+`",`+body+`}}`, hdr)
		if code != http.StatusCreated {
			t.Fatalf("rule %s: code=%d body=%s", body, code, resp)
		}
		return createdID(t, resp, "security_group_rule")
	}
	ssh := rule(`"direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":22,"port_range_max":22,"remote_ip_prefix":"10.0.0.5"`)
	anyProtocol := rule(`"direction":"ingress","ethertype":"IPv6"`)
	// New groups start with two egress rules; ingress alone keeps the listing to the two made here.
	listed := func(t *testing.T, query string) (int, []string) {
		t.Helper()
		code, body := doReq(r, http.MethodGet, "/v2.0/security-group-rules?direction=ingress&security_group_id="+group+"&"+query, hdr)
		var out struct {
			Rules []struct {
				ID string `json:"id"`
			} `json:"security_group_rules"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		var ids []string
		for _, rl := range out.Rules {
			ids = append(ids, rl.ID)
		}
		return code, ids
	}
	for query, want := range map[string]string{
		"ethertype=ipv4": ssh, "protocol=TCP": ssh, "remote_ip_prefix=10.0.0.5": ssh, "port_range_min=022": ssh,
		"protocol=any": anyProtocol,
	} {
		if code, ids := listed(t, query); code != http.StatusOK || !slices.Equal(ids, []string{want}) {
			t.Errorf("?%s: code=%d listed %v; want [%s]", query, code, ids, want)
		}
	}
	if code, _ := listed(t, "port_range_min=abc"); code != http.StatusBadRequest {
		t.Errorf("?port_range_min=abc: code=%d; want 400", code)
	}
}
