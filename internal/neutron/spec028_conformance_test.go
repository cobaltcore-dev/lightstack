package neutron

// SPEC-028 conformance fixes for the Neutron list surface, found by the CLI
// conformance suites (issue #105).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestListNetworksAdminTenantFilter_DB — an administrator filtering
// `GET /v2.0/networks?tenant_id=<project>` used to get the caller's read
// scope ANDed in (`project_id = caller OR shared OR external`), so another
// project's private networks could never appear and the answer was [].
// Real Neutron honours the tenant filter for admin tokens — the CLI's
// `network list --project <id>` depends on it.
func TestListNetworksAdminTenantFilter_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	otherProject := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, domain_id, enabled) VALUES ($1,$2,NULL,true)`,
		otherProject, "spec028-tenant-proj-"+otherProject[:8]); err != nil {
		t.Fatalf("insert other project: %v", err)
	}
	netID := uuid.NewString()
	netName := "spec028-tenant-net-" + netID[:8]
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,false,false)`,
		netID, netName, otherProject); err != nil {
		t.Fatalf("insert other-tenant net: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM networks WHERE id=$1", netID)
		testPool.Exec(ctx, "DELETE FROM projects WHERE id=$1", otherProject)
	})

	r := newNeutronTestRouter()

	// Admin: the tenant filter is the authoritative owner scope.
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	_, body := doReq(r, http.MethodGet, "/v2.0/networks?tenant_id="+otherProject, hdr)
	if nets := networksFromBody(t, body); !hasNet(nets, netName) {
		t.Errorf("admin ?tenant_id=<other> did not return the other project's private network; body=%s", body)
	}

	// project_id is an alias for tenant_id.
	_, body = doReq(r, http.MethodGet, "/v2.0/networks?project_id="+otherProject, hdr)
	if nets := networksFromBody(t, body); !hasNet(nets, netName) {
		t.Errorf("admin ?project_id=<other> did not return the other project's private network; body=%s", body)
	}

	// Non-admin: the read scope still applies, so another project's private
	// network stays invisible even with the filter.
	hdr["X-Test-Roles"] = "member"
	_, body = doReq(r, http.MethodGet, "/v2.0/networks?tenant_id="+otherProject, hdr)
	if nets := networksFromBody(t, body); hasNet(nets, netName) {
		t.Errorf("non-admin ?tenant_id=<other> leaked another project's private network; body=%s", body)
	}
}

// TestListFloatingIPsFilters_DB — `GET /v2.0/floatingips` ignored every list
// filter, so `openstack floating ip list --network <net>` returned addresses
// from all networks (SPEC-028). The standard exact-match filters must hold.
func TestListFloatingIPsFilters_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	netA, netB := uuid.NewString(), uuid.NewString()
	for _, n := range []struct{ id, name string }{{netA, "spec028-fipnet-a-" + netA[:8]}, {netB, "spec028-fipnet-b-" + netB[:8]}} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,false,false)`,
			n.id, n.name, seededProjectID); err != nil {
			t.Fatalf("insert net %s: %v", n.name, err)
		}
	}
	fipA, fipB := uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, status, description)
		 VALUES ($1,$2,$3,$4,'ACTIVE','spec028 fip a')`,
		fipA, seededProjectID, netA, "203.0.113."+randByte(t)); err != nil {
		t.Fatalf("insert fip a: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, status, description)
		 VALUES ($1,$2,$3,$4,'DOWN','spec028 fip b')`,
		fipB, seededProjectID, netB, "203.0.113."+randByte(t)); err != nil {
		t.Fatalf("insert fip b: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM floating_ips WHERE id = ANY($1)", []string{fipA, fipB})
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = ANY($1)", []string{netA, netB})
	})

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	fips := func(body string) []map[string]any {
		t.Helper()
		var resp struct {
			FloatingIPs []map[string]any `json:"floatingips"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal floatingips: %v (body=%s)", err, body)
		}
		return resp.FloatingIPs
	}
	has := func(list []map[string]any, id string) bool {
		for _, f := range list {
			if f["id"] == id {
				return true
			}
		}
		return false
	}

	// floating_network_id keeps only the address on that network.
	_, body := doReq(r, http.MethodGet, "/v2.0/floatingips?floating_network_id="+netA, hdr)
	if got := fips(body); !has(got, fipA) || has(got, fipB) {
		t.Errorf("?floating_network_id=netA returned a=%v b=%v; want a only", has(got, fipA), has(got, fipB))
	}

	// status filters too.
	_, body = doReq(r, http.MethodGet, "/v2.0/floatingips?status=DOWN", hdr)
	if got := fips(body); !has(got, fipB) || has(got, fipA) {
		t.Errorf("?status=DOWN returned a=%v b=%v; want b only", has(got, fipA), has(got, fipB))
	}

	// A non-UUID value on a UUID column answers the same empty list a
	// well-formed unknown id gets, not a 500.
	code, body := doReq(r, http.MethodGet, "/v2.0/floatingips?port_id=not-a-uuid", hdr)
	if code != http.StatusOK {
		t.Errorf("?port_id=not-a-uuid: code=%d; want 200", code)
	}
	if got := fips(body); len(got) != 0 {
		t.Errorf("?port_id=not-a-uuid returned %d fips; want 0", len(got))
	}
}

// randByte derives a stable, collision-free last octet for a test fip's
// address (floating_ip_address is UNIQUE).
func randByte(t *testing.T) string {
	t.Helper()
	b, err := uuid.NewRandom()
	if err != nil {
		t.Fatalf("rand: %v", err)
	}
	n := int(b[0])%200 + 10 // 10..209
	return string(rune('0'+n/100)) + string(rune('0'+(n/10)%10)) + string(rune('0'+n%10))
}

// ---------------------------------------------------------------------------
// CLI conformance round two (SPEC-028): the network suite's findings.

// seedSpec028NetSubnet creates a network + subnet owned by the seeded
// project, with a random third octet so concurrent runs cannot collide.
func seedSpec028NetSubnet(t *testing.T, ctx context.Context, label string) (netID, subnetID, cidr string) {
	t.Helper()
	b, _ := uuid.NewRandom()
	octet := int(b[0])%200 + 10
	netID, subnetID = uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,false,false)`,
		netID, "spec028-"+label+"-net-"+netID[:8], seededProjectID); err != nil {
		t.Fatalf("insert network: %v", err)
	}
	cidr = fmt.Sprintf("10.%d.0.0/24", octet)
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version) VALUES ($1,$2,$3,$4,$5,$6,4)`,
		subnetID, "spec028-"+label+"-sub-"+subnetID[:8], netID, seededProjectID, cidr, fmt.Sprintf("10.%d.0.1", octet)); err != nil {
		t.Fatalf("insert subnet: %v", err)
	}
	return netID, subnetID, cidr
}

// cleanupSpec028Net removes everything a seed + router-interface test can
// leave on a network, then the network itself.
func cleanupSpec028Net(t *testing.T, ctx context.Context, netID string) {
	t.Helper()
	testPool.Exec(ctx, `DELETE FROM ports WHERE network_id = $1`, netID)
	testPool.Exec(ctx, `DELETE FROM subnets WHERE network_id = $1`, netID)
	testPool.Exec(ctx, `DELETE FROM networks WHERE id = $1`, netID)
}

// TestPortShowAndUpdateByName404_DB — GET/PUT /v2.0/ports/{name} used to
// surface the uuid cast error as a 500, which breaks the SDK's name-fallback
// for 'port show/set/delete <name>' (it only falls back on 404). Real
// Neutron answers 404; now so does this (SPEC-028).
func TestPortShowAndUpdateByName404_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	netID, _, _ := seedSpec028NetSubnet(t, ctx, "portname")
	t.Cleanup(func() { cleanupSpec028Net(t, ctx, netID) })

	portID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, mac_address, admin_state_up, status, fixed_ips)
		 VALUES ($1,$2,$3,$4,$5,true,'ACTIVE','[]'::jsonb)`,
		portID, "spec028-byname-port", netID, seededProjectID, "02:00:00:00:00:01"); err != nil {
		t.Fatalf("insert port: %v", err)
	}

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	if code, body := doReq(r, http.MethodGet, "/v2.0/ports/spec028-byname-port", hdr); code != http.StatusNotFound {
		t.Errorf("GET port by name: code=%d; want 404 (body=%s)", code, body)
	}
	if code, body := doReqBody(r, http.MethodPut, "/v2.0/ports/spec028-byname-port",
		`{"port":{"name":"renamed"}}`, hdr); code != http.StatusNotFound {
		t.Errorf("PUT port by name: code=%d; want 404 (body=%s)", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/ports/"+portID, hdr); code != http.StatusOK {
		t.Errorf("GET port by id: code=%d; want 200 (body=%s)", code, body)
	}
}

// TestSubnetPoolShowByName404_DB — same divergence on subnet pools: a
// by-name GET/PUT was a 500, and the CLI's name resolution needs the 404
// (SPEC-028).
func TestSubnetPoolShowByName404_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	poolID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnet_pools (id, project_id, name, prefixes, min_prefixlen, max_prefixlen, ip_version)
		 VALUES ($1,$2,$3,$4,8,32,4)`,
		poolID, seededProjectID, "spec028-byname-pool", []string{"198.51.100.0/24"}); err != nil {
		t.Fatalf("insert subnet pool: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM subnet_pools WHERE id=$1", poolID) })

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	if code, body := doReq(r, http.MethodGet, "/v2.0/subnetpools/spec028-byname-pool", hdr); code != http.StatusNotFound {
		t.Errorf("GET subnet pool by name: code=%d; want 404 (body=%s)", code, body)
	}
	if code, body := doReqBody(r, http.MethodPut, "/v2.0/subnetpools/spec028-byname-pool",
		`{"subnetpool":{"name":"renamed"}}`, hdr); code != http.StatusNotFound {
		t.Errorf("PUT subnet pool by name: code=%d; want 404 (body=%s)", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/subnetpools/"+poolID, hdr); code != http.StatusOK {
		t.Errorf("GET subnet pool by id: code=%d; want 200 (body=%s)", code, body)
	}
}

// TestRouterInterfaceByPortAndDuplicate409_DB — three coupled defects in
// add_router_interface (SPEC-028):
//   - port_id was ignored: 'router add port' read the empty subnet_id and
//     answered 500 "failed to get subnet".
//   - a duplicate interface hit the (router_id, subnet_id) unique constraint
//     as a 500 that LEAKED the port it had already inserted, which then
//     blocked the network's deletion with a 409 nothing could remove.
//   - a routed subnet could be deleted (upstream: 409 SubnetInUse), orphaning
//     the interface port.
func TestRouterInterfaceByPortAndDuplicate409_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	netID, subnetID, _ := seedSpec028NetSubnet(t, ctx, "if")
	t.Cleanup(func() { cleanupSpec028Net(t, ctx, netID) })

	routerID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO routers (id, name, project_id) VALUES ($1,$2,$3)`,
		routerID, "spec028-if-router-"+routerID[:8], seededProjectID); err != nil {
		t.Fatalf("insert router: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM routers WHERE id=$1", routerID) })

	portID := uuid.NewString()
	fixedIPs := fmt.Sprintf(`[{"subnet_id":"%s","ip_address":"10.99.0.5"}]`, subnetID)
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, mac_address, admin_state_up, status, fixed_ips)
		 VALUES ($1,$2,$3,$4,$5,true,'ACTIVE',$6::jsonb)`,
		portID, "spec028-if-port", netID, seededProjectID, "02:00:00:00:00:02", fixedIPs); err != nil {
		t.Fatalf("insert port: %v", err)
	}

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	add := func(body string) (int, string) {
		return doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID+"/add_router_interface", body, hdr)
	}

	// Neither selector is an error the caller cannot diagnose.
	if code, body := add(`{}`); code != http.StatusBadRequest {
		t.Errorf("add with no selector: code=%d; want 400 (body=%s)", code, body)
	}
	if code, body := add(`{"port_id":"not-a-uuid"}`); code != http.StatusNotFound {
		t.Errorf("add with non-UUID port_id: code=%d; want 404 (body=%s)", code, body)
	}
	if code, body := add(fmt.Sprintf(`{"port_id":"%s"}`, uuid.NewString())); code != http.StatusNotFound {
		t.Errorf("add with unknown port_id: code=%d; want 404 (body=%s)", code, body)
	}

	// The port path: 200, and the answer names the port and its subnet.
	code, body := add(fmt.Sprintf(`{"port_id":"%s"}`, portID))
	if code != http.StatusOK {
		t.Fatalf("add by port_id: code=%d; want 200 (body=%s)", code, body)
	}
	var resp struct {
		SubnetID string `json:"subnet_id"`
		PortID   string `json:"port_id"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal add response: %v (body=%s)", err, body)
	}
	if resp.PortID != portID || resp.SubnetID != subnetID {
		t.Errorf("add by port_id: port=%s subnet=%s; want port=%s subnet=%s", resp.PortID, resp.SubnetID, portID, subnetID)
	}

	// The port is claimed by the router now.
	var owner string
	if err := testPool.QueryRow(ctx,
		`SELECT device_owner FROM ports WHERE id=$1`, portID).Scan(&owner); err != nil || owner != "network:router_interface" {
		t.Errorf("claimed port device_owner=%q err=%v; want network:router_interface", owner, err)
	}

	// An already-attached port is a 409, not a second interface.
	if code, body := add(fmt.Sprintf(`{"port_id":"%s"}`, portID)); code != http.StatusConflict {
		t.Errorf("add attached port again: code=%d; want 409 (body=%s)", code, body)
	}

	// A second interface for the same subnet is a 409 that leaks nothing.
	if code, body := add(fmt.Sprintf(`{"subnet_id":"%s"}`, subnetID)); code != http.StatusConflict {
		t.Errorf("duplicate interface: code=%d; want 409 (body=%s)", code, body)
	}
	var leaked int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM ports WHERE network_id=$1 AND device_owner='network:router_interface'`,
		netID).Scan(&leaked); err != nil || leaked != 1 {
		t.Errorf("router-interface ports on network after duplicate: count=%d err=%v; want 1", leaked, err)
	}

	// The routed subnet cannot be deleted (upstream: 409 SubnetInUse).
	if code, body := doReq(r, http.MethodDelete, "/v2.0/subnets/"+subnetID, hdr); code != http.StatusConflict {
		t.Errorf("delete routed subnet: code=%d; want 409 (body=%s)", code, body)
	}

	// Removing the interface frees the subnet for deletion.
	if code, body := doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID+"/remove_router_interface",
		fmt.Sprintf(`{"port_id":"%s"}`, portID), hdr); code != http.StatusOK {
		t.Fatalf("remove interface by port: code=%d; want 200 (body=%s)", code, body)
	}
	if code, body := doReq(r, http.MethodDelete, "/v2.0/subnets/"+subnetID, hdr); code != http.StatusNoContent {
		t.Errorf("delete subnet after interface removal: code=%d; want 204 (body=%s)", code, body)
	}
}

// TestNetworkDescriptionRoundTrip_DB — networks had no description column,
// so 'network set --description' was a 400 "no updates provided"
// (SPEC-028, migration 075).
func TestNetworkDescriptionRoundTrip_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	code, body := doReqBody(r, http.MethodPost, "/v2.0/networks",
		`{"network":{"name":"spec028-desc-net","description":"first"}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create network with description: code=%d (body=%s)", code, body)
	}
	var created struct {
		Network struct {
			ID          string `json:"id"`
			Description string `json:"description"`
		} `json:"network"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal create: %v (body=%s)", err, body)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM ports WHERE network_id=$1", created.Network.ID); testPool.Exec(ctx, "DELETE FROM networks WHERE id=$1", created.Network.ID) })
	if created.Network.Description != "first" {
		t.Errorf("create: description=%q; want first", created.Network.Description)
	}

	if code, body := doReqBody(r, http.MethodPut, "/v2.0/networks/"+created.Network.ID,
		`{"network":{"description":"second"}}`, hdr); code != http.StatusOK {
		t.Fatalf("set description: code=%d (body=%s)", code, body)
	} else {
		var updated struct {
			Network struct {
				Description string `json:"description"`
			} `json:"network"`
		}
		json.Unmarshal([]byte(body), &updated)
		if updated.Network.Description != "second" {
			t.Errorf("set description response: %q; want second", updated.Network.Description)
		}
	}

	_, body = doReq(r, http.MethodGet, "/v2.0/networks/"+created.Network.ID, hdr)
	var got struct {
		Network struct {
			Description string `json:"description"`
		} `json:"network"`
	}
	json.Unmarshal([]byte(body), &got)
	if got.Network.Description != "second" {
		t.Errorf("show after set: description=%q; want second (body=%s)", got.Network.Description, body)
	}
}

// TestSubnetSetDHCP_DB — 'subnet set --no-dhcp' PUTs enable_dhcp, which the
// update handler had no binding for: the flag was a 200 no-op (SPEC-028).
func TestSubnetSetDHCP_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	netID, subnetID, _ := seedSpec028NetSubnet(t, ctx, "dhcp")
	t.Cleanup(func() { cleanupSpec028Net(t, ctx, netID) })

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	if code, body := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID,
		`{"subnet":{"enable_dhcp":false}}`, hdr); code != http.StatusOK {
		t.Fatalf("subnet set --no-dhcp: code=%d (body=%s)", code, body)
	}

	var enableDHCP bool
	if err := testPool.QueryRow(ctx, `SELECT enable_dhcp FROM subnets WHERE id=$1`, subnetID).Scan(&enableDHCP); err != nil || enableDHCP {
		t.Errorf("enable_dhcp after update: %v err=%v; want false", enableDHCP, err)
	}

	// And back on, together with a rename, in one request.
	if code, body := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID,
		`{"subnet":{"enable_dhcp":true,"name":"spec028-dhcp-renamed"}}`, hdr); code != http.StatusOK {
		t.Fatalf("subnet set dhcp+name: code=%d (body=%s)", code, body)
	}
	if err := testPool.QueryRow(ctx, `SELECT enable_dhcp FROM subnets WHERE id=$1`, subnetID).Scan(&enableDHCP); err != nil || !enableDHCP {
		t.Errorf("enable_dhcp after re-enable: %v err=%v; want true", enableDHCP, err)
	}
}

// TestSubnetPoolPrefixValidationAndDescription_DB — a malformed prefix was
// stored as-is (upstream 400s), and pools had no description column
// (SPEC-028, migration 075).
func TestSubnetPoolPrefixValidationAndDescription_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	if code, body := doReqBody(r, http.MethodPost, "/v2.0/subnetpools",
		`{"subnetpool":{"name":"spec028-badpool","prefixes":["not-a-cidr"]}}`, hdr); code != http.StatusBadRequest {
		t.Errorf("create pool with malformed prefix: code=%d; want 400 (body=%s)", code, body)
	}

	code, body := doReqBody(r, http.MethodPost, "/v2.0/subnetpools",
		`{"subnetpool":{"name":"spec028-desc-pool","prefixes":["198.51.100.0/24"],"description":"first"}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create pool with description: code=%d (body=%s)", code, body)
	}
	var created struct {
		Pool struct {
			ID          string `json:"id"`
			Description string `json:"description"`
		} `json:"subnetpool"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal create: %v (body=%s)", err, body)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM subnet_pools WHERE id=$1", created.Pool.ID) })
	if created.Pool.Description != "first" {
		t.Errorf("create: description=%q; want first", created.Pool.Description)
	}

	if code, body := doReqBody(r, http.MethodPut, "/v2.0/subnetpools/"+created.Pool.ID,
		`{"subnetpool":{"description":"second"}}`, hdr); code != http.StatusOK {
		t.Fatalf("pool set description: code=%d (body=%s)", code, body)
	}
	_, body = doReq(r, http.MethodGet, "/v2.0/subnetpools/"+created.Pool.ID, hdr)
	var got struct {
		Pool struct {
			Description string `json:"description"`
		} `json:"subnetpool"`
	}
	json.Unmarshal([]byte(body), &got)
	if got.Pool.Description != "second" {
		t.Errorf("show after set: description=%q; want second (body=%s)", got.Pool.Description, body)
	}

	if code, body := doReqBody(r, http.MethodPut, "/v2.0/subnetpools/"+created.Pool.ID,
		`{"subnetpool":{"prefixes":["still-not-a-cidr"]}}`, hdr); code != http.StatusBadRequest {
		t.Errorf("pool set malformed prefix: code=%d; want 400 (body=%s)", code, body)
	}
}

// TestSecurityGroupCreateRulesEmpty_DB — the create response omitted
// security_group_rules, so OSC rendered it as null and its table formatter
// crashed ('NoneType' object is not iterable). Upstream reports [] for a
// fresh group (SPEC-028).
func TestSecurityGroupCreateRulesEmpty_DB(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	code, body := doReqBody(r, http.MethodPost, "/v2.0/security-groups",
		`{"security_group":{"name":"spec028-rules-sg","description":"SPEC-028 rules key"}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create security group: code=%d (body=%s)", code, body)
	}
	var created struct {
		SG struct {
			ID    string `json:"id"`
			Rules []any  `json:"security_group_rules"`
		} `json:"security_group"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("unmarshal create: %v (body=%s)", err, body)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM security_groups WHERE id=$1", created.SG.ID) })
	if created.SG.Rules == nil {
		t.Errorf("create: security_group_rules is null; want [] (body=%s)", body)
	} else if len(created.SG.Rules) != 0 {
		t.Errorf("create: security_group_rules has %d entries; want 0", len(created.SG.Rules))
	}
}
