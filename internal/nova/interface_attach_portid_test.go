package nova

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Round 3 of the 2026-10-13 review, F1/F2/F3: the port_id path of
// os-interface.
//
// F1 — every free port has device_id NULL, and the lookup scanned it into a
// plain string. The scan failed, the error was dropped, and the attach
// "worked" only because pgx fills earlier columns before the failed one; any
// real database error on that lookup also surfaced as a bogus 200 with an
// empty net_id and MAC. The lookup must read device_id nullably and handle
// its errors.
//
// F2 — the response reported the port's first fixed IP (a hardcoded
// placeholder before, then only partially fixed) and fabricated
// {"ip_address": ""} for a port with no addresses. It must report the port's
// actual fixed_ips entries with their subnet ids, or an empty list.
//
// F3 — an explicit fixed_ip was stored unvalidated: not checked against the
// subnet, not checked against the addresses other ports hold (the explicit
// side of the N-4 collision rule), and arbitrary text reached the JSONB
// build. It must 400 out-of-subnet and 409 taken addresses.
func TestAttachInterfacePortIDReportsRealState(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNovaTestRouter()

	server, _ := seedAuthzFixtures(t)
	member := map[string]string{"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member"}

	netID := uuid.NewString()
	subnetID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, status, admin_state_up)
		 VALUES ($1, 'r3-iface-net', $2, 'ACTIVE', true)`, netID, authzProjectA); err != nil {
		t.Fatalf("seeding network: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr)
		 VALUES ($1, 'r3-iface-sub', $2, $3, '10.249.0.0/24')`, subnetID, netID, authzProjectA); err != nil {
		t.Fatalf("seeding subnet: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM interface_attachments WHERE port_id IN (SELECT id FROM ports WHERE network_id = $1)`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM ports WHERE network_id = $1`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM subnets WHERE network_id = $1`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, netID)
	})

	// A free tenant port (device_id NULL — the state every detached port is
	// in) holding one address.
	freePort := uuid.NewString()
	fixed, _ := json.Marshal([]map[string]any{
		{"subnet_id": subnetID, "ip_address": "10.249.0.55"},
	})
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		 VALUES ($1, 'r3-free', $2, $3, '52:54:00:aa:bb:01', 'DOWN', $4::jsonb)`,
		freePort, netID, authzProjectA, fixed); err != nil {
		t.Fatalf("seeding free port: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface",
		fmt.Sprintf(`{"interfaceAttachment":{"port_id":%q}}`, freePort), member)
	if code != http.StatusOK {
		t.Fatalf("attaching a free (device_id NULL) port: code=%d body=%s; want 200 — the F1 defect made this ride on a swallowed scan error", code, body)
	}
	var resp struct {
		InterfaceAttachment struct {
			NetID    string `json:"net_id"`
			MacAddr  string `json:"mac_addr"`
			FixedIPs []struct {
				IPAddress string `json:"ip_address"`
				SubnetID  string `json:"subnet_id"`
			} `json:"fixed_ips"`
		} `json:"interfaceAttachment"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, body)
	}
	if resp.InterfaceAttachment.NetID != netID || resp.InterfaceAttachment.MacAddr != "52:54:00:aa:bb:01" {
		t.Errorf("attachment reports net_id=%q mac=%q; want the port's real values (the F1 defect reported empties on any lookup error)", resp.InterfaceAttachment.NetID, resp.InterfaceAttachment.MacAddr)
	}
	if len(resp.InterfaceAttachment.FixedIPs) != 1 ||
		resp.InterfaceAttachment.FixedIPs[0].IPAddress != "10.249.0.55" ||
		resp.InterfaceAttachment.FixedIPs[0].SubnetID != subnetID {
		t.Errorf("attachment fixed_ips = %+v; want the port's actual entry with its subnet id (F2)", resp.InterfaceAttachment.FixedIPs)
	}
}

// The empty half of F2: a port with no addresses reports an empty list, not a
// fabricated {"ip_address": ""}.
func TestAttachInterfacePortIDWithoutAddressesReportsNone(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNovaTestRouter()

	server, _ := seedAuthzFixtures(t)
	member := map[string]string{"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member"}

	netID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, status, admin_state_up)
		 VALUES ($1, 'r3-iface-empty', $2, 'ACTIVE', true)`, netID, authzProjectA); err != nil {
		t.Fatalf("seeding network: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM interface_attachments WHERE port_id IN (SELECT id FROM ports WHERE network_id = $1)`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM ports WHERE network_id = $1`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, netID)
	})

	addrlessPort := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		 VALUES ($1, 'r3-addrless', $2, $3, '52:54:00:aa:bb:02', 'DOWN', '[]'::jsonb)`,
		addrlessPort, netID, authzProjectA); err != nil {
		t.Fatalf("seeding address-less port: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface",
		fmt.Sprintf(`{"interfaceAttachment":{"port_id":%q}}`, addrlessPort), member)
	if code != http.StatusOK {
		t.Fatalf("attach: code=%d body=%s", code, body)
	}
	if want := `"fixed_ips":[]`; !strings.Contains(body, want) {
		t.Errorf("a port with no addresses must report fixed_ips: [], not a fabricated entry: %s", body)
	}
}

// F3: a caller-specified fixed_ip is validated — 400 outside the subnet, 409
// when another port holds it — instead of being stored blind.
func TestAttachInterfaceExplicitFixedIPIsValidated(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNovaTestRouter()

	server, _ := seedAuthzFixtures(t)
	member := map[string]string{"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member"}

	netID := uuid.NewString()
	subnetID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, status, admin_state_up)
		 VALUES ($1, 'r3-iface-explicit', $2, 'ACTIVE', true)`, netID, authzProjectA); err != nil {
		t.Fatalf("seeding network: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip)
		 VALUES ($1, 'r3-iface-explicit-sub', $2, $3, '10.250.0.0/24', '10.250.0.1')`,
		subnetID, netID, authzProjectA); err != nil {
		t.Fatalf("seeding subnet: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM ports WHERE network_id = $1`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM subnets WHERE network_id = $1`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, netID)
	})

	// A port already holding the address the caller will ask for.
	holder := uuid.NewString()
	fixed, _ := json.Marshal([]map[string]any{
		{"subnet_id": subnetID, "ip_address": "10.250.0.77"},
	})
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		 VALUES ($1, 'r3-holder', $2, $3, '52:54:00:aa:bb:03', 'DOWN', $4::jsonb)`,
		holder, netID, authzProjectA, fixed); err != nil {
		t.Fatalf("seeding holder port: %v", err)
	}

	attach := func(body string) (int, string) {
		return doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface", body, member)
	}

	if code, body := attach(fmt.Sprintf(`{"interfaceAttachment":{"net_id":%q,"fixed_ip":"10.250.0.77"}}`, netID)); code != http.StatusConflict {
		t.Errorf("explicit fixed_ip held by another port: code=%d body=%s; want 409 (the explicit side of N-4)", code, body)
	}
	if code, body := attach(fmt.Sprintf(`{"interfaceAttachment":{"net_id":%q,"fixed_ip":"192.0.2.1"}}`, netID)); code != http.StatusBadRequest {
		t.Errorf("explicit fixed_ip outside the subnet: code=%d body=%s; want 400", code, body)
	}
	if code, body := attach(fmt.Sprintf(`{"interfaceAttachment":{"net_id":%q,"fixed_ip":"not-an-ip"}}`, netID)); code != http.StatusBadRequest {
		t.Errorf("non-IP fixed_ip: code=%d body=%s; want 400 (not a 500 from the JSONB cast)", code, body)
	}

	// A free, in-subnet address still works.
	code, body := attach(fmt.Sprintf(`{"interfaceAttachment":{"net_id":%q,"fixed_ip":"10.250.0.99"}}`, netID))
	if code != http.StatusOK {
		t.Fatalf("valid explicit fixed_ip: code=%d body=%s; want 200", code, body)
	}
	if !strings.Contains(body, `"ip_address": "10.250.0.99"`) && !strings.Contains(body, `"ip_address":"10.250.0.99"`) {
		t.Errorf("response must report the requested address: %s", body)
	}
}
