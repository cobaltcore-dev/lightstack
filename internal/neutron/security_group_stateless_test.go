package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// SPEC-033 round 2, T074 (FR-051; research R47): stateless groups, as upstream's. A group may be
// created with "stateful": false; its statefulness changes only while no port carries it (409
// SecurityGroupInUse otherwise); and no port carries stateful and stateless groups at once: a port
// create or update that would mix them answers 409 SecurityGroupConflict and changes nothing.

func groupStateful(t *testing.T, id string) bool {
	t.Helper()
	var stateful bool
	if err := testPool.QueryRow(context.Background(), "SELECT stateful FROM security_groups WHERE id = $1", id).Scan(&stateful); err != nil {
		t.Fatalf("read stateful of %s: %v", id, err)
	}
	return stateful
}

func TestStatelessSecurityGroups(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	t.Run("created stateless and reported so", func(t *testing.T) {
		g := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-stateless","stateful":false}}`)
		if g["stateful"] != false {
			t.Errorf("create response: stateful = %v; want false", g["stateful"])
		}
		id := g["id"].(string)
		if groupStateful(t, id) {
			t.Error("stored stateful = true; want false")
		}
		code, body := doReq(r, http.MethodGet, "/v2.0/security-groups/"+id, hdr)
		var out struct {
			Group map[string]any `json:"security_group"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || out.Group["stateful"] != false {
			t.Errorf("show: code=%d body=%s; want stateful false", code, body)
		}
	})

	t.Run("statefulness changes only while no port carries the group", func(t *testing.T) {
		g := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-flip"}}`)
		id := g["id"].(string)
		net := seedNetwork(t, "spec033-flip-net")
		sub := seedSubnet(t, net, "spec033-flip-subnet", "10.51.0.0/24", nil, 4, true)
		port, _ := seedPortWithIP(t, net, sub)
		mustExec(t, "DELETE FROM port_security_groups WHERE port_id = $1", port)
		sgTestAttach(t, port, id)

		code, body := doJSONReq(r, http.MethodPut, "/v2.0/security-groups/"+id, `{"security_group":{"stateful":false}}`, hdr)
		if code != http.StatusConflict {
			t.Fatalf("flip while in use: code=%d body=%s; want 409", code, body)
		}
		assertNeutronError(t, body, "SecurityGroupInUse", "Security Group "+id+" in use.") // SPEC-033 T216
		if !groupStateful(t, id) {
			t.Error("a refused flip changed the group")
		}
		// Sending the value it already has is not a change.
		if code, body := doJSONReq(r, http.MethodPut, "/v2.0/security-groups/"+id, `{"security_group":{"stateful":true}}`, hdr); code != http.StatusOK {
			t.Errorf("stateful unchanged while in use: code=%d body=%s; want 200", code, body)
		}

		mustExec(t, "DELETE FROM port_security_groups WHERE port_id = $1", port)
		if code, body := doJSONReq(r, http.MethodPut, "/v2.0/security-groups/"+id, `{"security_group":{"stateful":false}}`, hdr); code != http.StatusOK {
			t.Fatalf("flip while unused: code=%d body=%s; want 200", code, body)
		}
		if groupStateful(t, id) {
			t.Error("the flip did not reach the group")
		}
	})

	t.Run("a port never mixes stateful and stateless groups", func(t *testing.T) {
		stateful := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-sf"}}`)["id"].(string)
		stateless := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-sl","stateful":false}}`)["id"].(string)
		stateless2 := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-sl2","stateful":false}}`)["id"].(string)
		net := seedNetwork(t, "spec033-mix-net")
		seedSubnet(t, net, "spec033-mix-subnet", "10.52.0.0/24", nil, 4, true)

		var before int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM ports WHERE network_id = $1", net).Scan(&before); err != nil {
			t.Fatal(err)
		}
		code, body := doJSONReq(r, http.MethodPost, "/v2.0/ports",
			`{"port":{"network_id":"`+net+`","security_groups":`+quoted(stateful, stateless)+`}}`, hdr)
		if code != http.StatusConflict {
			t.Fatalf("create mixing: code=%d body=%s; want 409", code, body)
		}
		if typ := neutronErrorType(t, body); typ != "SecurityGroupConflict" {
			t.Errorf("type = %q; want SecurityGroupConflict", typ)
		}
		var after int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM ports WHERE network_id = $1", net).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Errorf("a refused create left %d port(s)", after-before)
		}

		code, body = doJSONReq(r, http.MethodPost, "/v2.0/ports",
			`{"port":{"network_id":"`+net+`","security_groups":`+quoted(stateful)+`}}`, hdr)
		if code != http.StatusCreated {
			t.Fatalf("create stateful only: code=%d body=%s", code, body)
		}
		var created struct {
			Port struct {
				ID string `json:"id"`
			} `json:"port"`
		}
		_ = json.Unmarshal([]byte(body), &created)
		port := created.Port.ID

		code, body = putPort(t, r, hdr, port, sgBody(quoted(stateful, stateless)))
		if code != http.StatusConflict {
			t.Fatalf("update mixing: code=%d body=%s; want 409", code, body)
		}
		if typ := neutronErrorType(t, body); typ != "SecurityGroupConflict" {
			t.Errorf("type = %q; want SecurityGroupConflict", typ)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{stateful}) {
			t.Errorf("groups after a refused update: %v; want [%s]", got, stateful)
		}

		// All stateless is no mix.
		if code, body := putPort(t, r, hdr, port, sgBody(quoted(stateless, stateless2))); code != http.StatusOK {
			t.Errorf("update to stateless only: code=%d body=%s; want 200", code, body)
		}
	})
}
