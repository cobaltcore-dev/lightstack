package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// SPEC-024 (cloud-storage#546) US4 / B6 — the gateway is a three-valued attribute.
//
// | request                     | meaning        | stored | reported |
// |-----------------------------|----------------|--------|----------|
// | attribute omitted           | choose for me  | .1     | .1       |
// | "gateway_ip": null          | none           | NULL   | null     |
// | "gateway_ip": "10.0.0.1"    | explicit       | value  | value    |
//
// The request field was a plain `string`, so null and omitted were indistinguishable and both got an
// invented address — which is why `--gateway none` was silently overwritten and why a NULL gateway was
// unreachable through the API at all (spec A5). Fixing the write side is what makes the read side
// testable, so the two land together.

// createSubnet posts a raw subnet body and returns status + decoded subnet object.
func createSubnet(t *testing.T, r http.Handler, hdr map[string]string, body string) (int, map[string]any) {
	t.Helper()
	code, raw := doReqBody(r, http.MethodPost, "/v2.0/subnets", body, hdr)
	var resp struct {
		Subnet map[string]any `json:"subnet"`
	}
	if code == http.StatusCreated {
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			t.Fatalf("unmarshal subnet: %v (body=%s)", err, raw)
		}
		id, _ := resp.Subnet["id"].(string)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), "DELETE FROM subnets WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
		})
	}
	if resp.Subnet == nil {
		resp.Subnet = map[string]any{"_raw": raw}
	}
	return code, resp.Subnet
}

// storedGateway reads gateway_ip straight from storage, so a correct-looking echo over a wrong stored
// value cannot pass.
func storedGateway(t *testing.T, subnetID string) *string {
	t.Helper()
	var gw *string
	if err := testPool.QueryRow(context.Background(),
		"SELECT gateway_ip FROM subnets WHERE id = $1", subnetID).Scan(&gw); err != nil {
		t.Fatalf("read stored gateway: %v", err)
	}
	return gw
}

// TestOmittedGatewayIsStillAutoAssigned — contract S8. Today's behaviour, which must not change.
func TestOmittedGatewayIsStillAutoAssigned(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	netID := seedNetwork(t, "spec024-gw-omitted")

	code, subnet := createSubnet(t, r, hdr,
		fmt.Sprintf(`{"subnet":{"network_id":%q,"cidr":"10.80.1.0/24","name":"gw-omitted"}}`, netID))
	if code != http.StatusCreated {
		t.Fatalf("create subnet: code=%d body=%v; want 201", code, subnet)
	}
	if got := subnet["gateway_ip"]; got != "10.80.1.1" {
		t.Errorf("gateway_ip = %#v, want the first host address 10.80.1.1 when the attribute is omitted", got)
	}
}

// TestExplicitNullGatewayIsHonoured — contracts S9 and S10.
func TestExplicitNullGatewayIsHonoured(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	netID := seedNetwork(t, "spec024-gw-null")

	code, subnet := createSubnet(t, r, hdr,
		fmt.Sprintf(`{"subnet":{"network_id":%q,"cidr":"10.81.1.0/24","name":"gw-null","gateway_ip":null}}`, netID))
	if code != http.StatusCreated {
		t.Fatalf("create subnet: code=%d body=%v; want 201", code, subnet)
	}
	if got := subnet["gateway_ip"]; got != nil {
		t.Errorf("gateway_ip = %#v, want null — an explicit request for no gateway must be honoured, not overwritten (B6)", got)
	}
	id, _ := subnet["id"].(string)
	if gw := storedGateway(t, id); gw != nil {
		t.Errorf("stored gateway_ip = %q, want NULL — the response and storage must agree", *gw)
	}
}

// TestExplicitGatewayIsStored — the third value, for completeness.
func TestExplicitGatewayIsStored(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	netID := seedNetwork(t, "spec024-gw-explicit")

	code, subnet := createSubnet(t, r, hdr,
		fmt.Sprintf(`{"subnet":{"network_id":%q,"cidr":"10.82.1.0/24","name":"gw-explicit","gateway_ip":"10.82.1.254"}}`, netID))
	if code != http.StatusCreated {
		t.Fatalf("create subnet: code=%d body=%v; want 201", code, subnet)
	}
	if got := subnet["gateway_ip"]; got != "10.82.1.254" {
		t.Errorf("gateway_ip = %#v, want 10.82.1.254", got)
	}
}

// TestGatewaylessSubnetIsReadableAndUpdatable — contracts S11, S12 and S13.
//
// Before the fix: absent from the listing, and 500 on show and update, because all three read sites
// scanned a nullable column into a plain string.
func TestGatewaylessSubnetIsReadableAndUpdatable(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	netID := seedNetwork(t, "spec024-gw-readable")
	subnetID := seedSubnet(t, netID, "gw-readable", "10.83.1.0/24", nil, 4, true)

	t.Run("S12 show by id", func(t *testing.T) {
		code, body := doReq(r, http.MethodGet, "/v2.0/subnets/"+subnetID, hdr)
		if code != http.StatusOK {
			t.Fatalf("show subnet: code=%d body=%s; want 200 — a NULL gateway must not be a server error", code, body)
		}
		var resp struct {
			Subnet map[string]any `json:"subnet"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := resp.Subnet["gateway_ip"]; got != nil {
			t.Errorf("gateway_ip = %#v, want null", got)
		}
	})

	t.Run("S13 update by id", func(t *testing.T) {
		code, body := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID,
			`{"subnet":{"name":"gw-readable-renamed"}}`, hdr)
		if code != http.StatusOK {
			t.Fatalf("update subnet: code=%d body=%s; want 200", code, body)
		}
	})
}

// TestUpdateCanClearTheGateway — contract S14.
func TestUpdateCanClearTheGateway(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	netID := seedNetwork(t, "spec024-gw-clear")
	gw := "10.84.1.1"
	subnetID := seedSubnet(t, netID, "gw-clear", "10.84.1.0/24", &gw, 4, true)

	code, body := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID,
		`{"subnet":{"gateway_ip":null}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("clear gateway: code=%d body=%s; want 200", code, body)
	}
	if stored := storedGateway(t, subnetID); stored != nil {
		t.Errorf("stored gateway_ip = %q after clearing, want NULL", *stored)
	}
}
