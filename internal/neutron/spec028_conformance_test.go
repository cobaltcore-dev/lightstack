package neutron

// SPEC-028 conformance fixes for the Neutron list surface, found by the CLI
// conformance suites (issue #105).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/cache"
	"github.com/gin-gonic/gin"
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

// TestSubnetPoolReportsOwningProjectForSharedPool_DB — shared pools are
// visible to every project, so ListSubnetPools/GetSubnetPool serve rows whose
// project_id is not the caller's. Both used to render the CALLER's project as
// tenant_id; the response must name the row's owner (the same round-8 bug
// fixed for cinder volumes and neutron ports). Visibility semantics stay:
// another project's private pool remains invisible and 404s by id.
func TestSubnetPoolReportsOwningProjectForSharedPool_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	ensureTenantB(t)

	ownerHdr := hdr(tenantA, "member")
	otherHdr := hdr(tenantB, "member")

	createPool := func(name string, shared bool) string {
		t.Helper()
		code, body := doReqBody(r, http.MethodPost, "/v2.0/subnetpools",
			fmt.Sprintf(`{"subnetpool":{"name":%q,"prefixes":["198.51.100.0/24"],"shared":%t}}`, name, shared), ownerHdr)
		if code != http.StatusCreated {
			t.Fatalf("create pool %s: code=%d (body=%s)", name, code, body)
		}
		var created struct {
			Pool struct {
				ID string `json:"id"`
			} `json:"subnetpool"`
		}
		if err := json.Unmarshal([]byte(body), &created); err != nil {
			t.Fatalf("unmarshal pool: %v (body=%s)", err, body)
		}
		return created.Pool.ID
	}
	sharedID := createPool("spec028-shared-owner", true)
	privateID := createPool("spec028-private-owner", false)
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM subnet_pools WHERE id = ANY($1)", []string{sharedID, privateID})
	})

	// The other project's list sees the shared pool, attributed to its owner,
	// and does not see the private one.
	code, body := doReq(r, http.MethodGet, "/v2.0/subnetpools", otherHdr)
	if code != http.StatusOK {
		t.Fatalf("list subnet pools as the other project: code=%d (body=%s)", code, body)
	}
	var list struct {
		Pools []struct {
			ID       string `json:"id"`
			TenantID string `json:"tenant_id"`
		} `json:"subnetpools"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("unmarshal list: %v (body=%s)", err, body)
	}
	var sawShared, sawPrivate bool
	for _, p := range list.Pools {
		switch p.ID {
		case sharedID:
			sawShared = true
			if p.TenantID != tenantA {
				t.Errorf("shared pool list tenant_id = %q; want its owner %q, not the caller %q", p.TenantID, tenantA, tenantB)
			}
		case privateID:
			sawPrivate = true
		}
	}
	if !sawShared {
		t.Errorf("other project's list does not contain the shared pool %s: %s", sharedID, body)
	}
	if sawPrivate {
		t.Errorf("other project's list leaked the private pool %s: %s", privateID, body)
	}

	// Show the shared pool from the other project: 200, owner attribution.
	code, body = doReq(r, http.MethodGet, "/v2.0/subnetpools/"+sharedID, otherHdr)
	if code != http.StatusOK {
		t.Fatalf("show shared pool as the other project: code=%d (body=%s)", code, body)
	}
	var got struct {
		Pool struct {
			TenantID string `json:"tenant_id"`
		} `json:"subnetpool"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("unmarshal shared pool: %v (body=%s)", err, body)
	}
	if got.Pool.TenantID != tenantA {
		t.Errorf("shared pool show tenant_id = %q; want its owner %q, not the caller %q", got.Pool.TenantID, tenantA, tenantB)
	}

	// A foreign private pool keeps the existing 404 behaviour.
	if code, body := doReq(r, http.MethodGet, "/v2.0/subnetpools/"+privateID, otherHdr); code != http.StatusNotFound {
		t.Errorf("show foreign private pool: code=%d (body=%s); want 404", code, body)
	}

	// The owner's own read is unchanged.
	code, body = doReq(r, http.MethodGet, "/v2.0/subnetpools/"+sharedID, ownerHdr)
	if code != http.StatusOK {
		t.Fatalf("owner shows shared pool: code=%d (body=%s)", code, body)
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
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM ports WHERE network_id=$1", created.Network.ID)
		testPool.Exec(ctx, "DELETE FROM networks WHERE id=$1", created.Network.ID)
	})
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

// TestGetNetworkAdminCarveOut_DB — the admin tenant_id LIST filter is
// honoured (see TestListNetworksAdminTenantFilter_DB), but GET
// /v2.0/networks/{id} kept the caller's read scope unconditionally, so an
// admin could list another project's private network and then get a 404
// showing the id it just listed. Upstream's get_network policy is
// "admin_or_owner or shared or external" (SPEC-028).
func TestGetNetworkAdminCarveOut_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	otherProject := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, domain_id, enabled) VALUES ($1,$2,NULL,true)`,
		otherProject, "spec028-show-proj-"+otherProject[:8]); err != nil {
		t.Fatalf("insert other project: %v", err)
	}
	netID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,false,false)`,
		netID, "spec028-show-net-"+netID[:8], otherProject); err != nil {
		t.Fatalf("insert network: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM networks WHERE id=$1", netID)
		testPool.Exec(ctx, "DELETE FROM projects WHERE id=$1", otherProject)
	})

	r := newNeutronTestRouter()
	adminHdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	memberHdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	if code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+netID, adminHdr); code != http.StatusOK {
		t.Errorf("admin shows other project's private network: code=%d; want 200 (body=%s)", code, body)
	}
	// A plain member of ANOTHER project still cannot: 404, not 403, so the
	// network's existence stays private.
	if code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+netID, memberHdr); code != http.StatusNotFound {
		t.Errorf("member shows other project's private network: code=%d; want 404 (body=%s)", code, body)
	}
}

// TestAddRouterInterfaceSubnetScope_DB — the subnet lookup behind
// add_router_interface was unscoped: a caller who owned a router could attach
// an interface to ANOTHER project's private subnet by id. The port path was
// already project-scoped; now the subnet row must be the caller's, or sit on
// a shared/external network (SPEC-028).
func TestAddRouterInterfaceSubnetScope_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	otherProject := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, domain_id, enabled) VALUES ($1,$2,NULL,true)`,
		otherProject, "spec028-ifscope-proj-"+otherProject[:8]); err != nil {
		t.Fatalf("insert other project: %v", err)
	}
	// A private and a shared network, both owned by the other project.
	privateNet, sharedNet := uuid.NewString(), uuid.NewString()
	for _, n := range []struct {
		id     string
		shared bool
	}{{privateNet, false}, {sharedNet, true}} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,$4,false)`,
			n.id, "spec028-ifscope-net-"+n.id[:8], otherProject, n.shared); err != nil {
			t.Fatalf("insert network: %v", err)
		}
	}
	privateSub, sharedSub := uuid.NewString(), uuid.NewString()
	for _, s := range []struct{ id, net string }{{privateSub, privateNet}, {sharedSub, sharedNet}} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version) VALUES ($1,$2,$3,$4,$5,$6,4)`,
			s.id, "spec028-ifscope-sub-"+s.id[:8], s.net, otherProject, "10.231.0.0/24", "10.231.0.1"); err != nil {
			t.Fatalf("insert subnet: %v", err)
		}
	}
	routerID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO routers (id, name, project_id) VALUES ($1,$2,$3)`,
		routerID, "spec028-ifscope-rt-"+routerID[:8], seededProjectID); err != nil {
		t.Fatalf("insert router: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM router_interfaces WHERE router_id=$1", routerID)
		testPool.Exec(ctx, "DELETE FROM ports WHERE network_id = ANY($1)", []string{privateNet, sharedNet})
		testPool.Exec(ctx, "DELETE FROM subnets WHERE id = ANY($1)", []string{privateSub, sharedSub})
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = ANY($1)", []string{privateNet, sharedNet})
		testPool.Exec(ctx, "DELETE FROM routers WHERE id=$1", routerID)
		testPool.Exec(ctx, "DELETE FROM projects WHERE id=$1", otherProject)
	})

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	// Another project's PRIVATE subnet: refused, and as a 404 so the subnet's
	// existence stays private.
	code, body := doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID+"/add_router_interface",
		fmt.Sprintf(`{"subnet_id":"%s"}`, privateSub), hdr)
	if code != http.StatusNotFound {
		t.Errorf("attach other project's private subnet: code=%d; want 404 (body=%s)", code, body)
	}

	// A subnet on a SHARED network is legitimately attachable.
	code, body = doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID+"/add_router_interface",
		fmt.Sprintf(`{"subnet_id":"%s"}`, sharedSub), hdr)
	if code != http.StatusOK {
		t.Errorf("attach shared-network subnet: code=%d; want 200 (body=%s)", code, body)
	}

	// A subnet ANOTHER project created on a network the caller owns is
	// attachable too — the same entitlement subnetReadScope grants, so the
	// network owner is not locked out of their own network by an admin
	// subnet (e.g. the seeded public net's subnets).
	adminSub := uuid.NewString()
	ownNet := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,false,false)`,
		ownNet, "spec028-ifscope-own-"+ownNet[:8], seededProjectID); err != nil {
		t.Fatalf("insert own network: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version) VALUES ($1,$2,$3,$4,$5,$6,4)`,
		adminSub, "spec028-ifscope-asub-"+adminSub[:8], ownNet, otherProject, "10.232.0.0/24", "10.232.0.1"); err != nil {
		t.Fatalf("insert admin subnet on own network: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM subnets WHERE id=$1", adminSub)
		testPool.Exec(ctx, "DELETE FROM networks WHERE id=$1", ownNet)
	})
	code, body = doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID+"/add_router_interface",
		fmt.Sprintf(`{"subnet_id":"%s"}`, adminSub), hdr)
	if code != http.StatusOK {
		t.Errorf("attach admin-created subnet on own network: code=%d; want 200 (body=%s)", code, body)
	}
}

// TestGetNetworkCacheDoesNotCrossRoles_DB — the admin carve-out (see
// TestGetNetworkAdminCarveOut_DB) meets the project-scoped network cache:
// the cache key names only (project, network), and the lookup precedes any
// visibility check, so an ADMIN's fetch of another project's private network
// must not leave an entry that a plain member of the admin's project then
// reads as a 200 where the uncached answer is 404. Only rows the project is
// entitled to by the read scope may be cached (SPEC-028).
func TestGetNetworkCacheDoesNotCrossRoles_DB(t *testing.T) {
	requireDB(t)

	// The poisoning only exists with a real cache behind the service.
	cacheInst, err := cache.NewCache(cache.Config{
		RedisURL: "redis://127.0.0.1:6379", Enabled: true,
		KeyPrefix: "spec028-cachetest:", DefaultTTL: time.Minute,
	})
	if err != nil {
		t.Skipf("no local Redis for the cache regression: %v", err)
	}
	t.Cleanup(func() {
		// DeletePattern prepends the instance's KeyPrefix itself, so the
		// pattern is just "*" — a prefixed pattern would double the prefix
		// and match nothing (round 9).
		cacheInst.DeletePattern(context.Background(), "*")
	})

	ctx := context.Background()
	otherProject := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, domain_id, enabled) VALUES ($1,$2,NULL,true)`,
		otherProject, "spec028-cache-proj-"+otherProject[:8]); err != nil {
		t.Fatalf("insert other project: %v", err)
	}
	foreignNet := uuid.NewString() // other project's PRIVATE network
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,false,false)`,
		foreignNet, "spec028-cache-net-"+foreignNet[:8], otherProject); err != nil {
		t.Fatalf("insert foreign network: %v", err)
	}
	ownNet := uuid.NewString() // the caller project's own network
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,false,false)`,
		ownNet, "spec028-cache-own-"+ownNet[:8], seededProjectID); err != nil {
		t.Fatalf("insert own network: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = ANY($1)", []string{foreignNet, ownNet})
		testPool.Exec(ctx, "DELETE FROM projects WHERE id=$1", otherProject)
	})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	NewService(testPool, "stub", cacheInst).RegisterRoutes(&r.RouterGroup)

	adminHdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	memberHdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	// The admin may fetch the foreign private network...
	if code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+foreignNet, adminHdr); code != http.StatusOK {
		t.Fatalf("admin fetches foreign private network: code=%d; want 200 (body=%s)", code, body)
	}
	// ...but that fetch must not poison the project's cache: a plain member
	// of the same project still gets the uncached answer, 404.
	if code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+foreignNet, memberHdr); code != http.StatusNotFound {
		t.Errorf("member reads foreign private network after admin fetch: code=%d; want 404 — cache poisoned (body=%s)", code, body)
	}

	// The entitlement-positive path still caches: two member reads of the
	// project's own network both succeed (the second is a cache hit), and
	// the entry is provably present after the first — so the guard above
	// suppresses only the foreign-private rows, not caching itself.
	for i := 1; i <= 2; i++ {
		if code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+ownNet, memberHdr); code != http.StatusOK {
			t.Errorf("member read #%d of own network: code=%d; want 200 (body=%s)", i, code, body)
		}
	}
	var cached gin.H
	if err := cacheInst.Get(ctx, "network:"+seededProjectID+":"+ownNet, &cached); err != nil {
		t.Errorf("own network not cached after member read: %v", err)
	} else if cached["id"] != ownNet {
		t.Errorf("cached entry holds the wrong network: %v", cached["id"])
	}
}

// TestAddRouterInterfaceNullGateway_DB — a subnet without a gateway (an
// explicit null clears it, S14) made add_router_interface 500: the lookup
// scanned gateway_ip into a plain string and NULL cannot land in one.
// Upstream attaches with an address allocated from the subnet; the fix
// falls back to the same allocator CreatePort uses (SPEC-028).
func TestAddRouterInterfaceNullGateway_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	netID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,false,false)`,
		netID, "spec028-nogw-net-"+netID[:8], seededProjectID); err != nil {
		t.Fatalf("insert network: %v", err)
	}
	// gateway_ip deliberately omitted: NULL.
	subID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr, ip_version) VALUES ($1,$2,$3,$4,$5,4)`,
		subID, "spec028-nogw-sub-"+subID[:8], netID, seededProjectID, "10.233.0.0/24"); err != nil {
		t.Fatalf("insert subnet: %v", err)
	}
	routerID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO routers (id, name, project_id) VALUES ($1,$2,$3)`,
		routerID, "spec028-nogw-rt-"+routerID[:8], seededProjectID); err != nil {
		t.Fatalf("insert router: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM router_interfaces WHERE router_id=$1", routerID)
		testPool.Exec(ctx, "DELETE FROM ports WHERE network_id=$1", netID)
		testPool.Exec(ctx, "DELETE FROM subnets WHERE id=$1", subID)
		testPool.Exec(ctx, "DELETE FROM networks WHERE id=$1", netID)
		testPool.Exec(ctx, "DELETE FROM routers WHERE id=$1", routerID)
	})

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	code, body := doReqBody(r, http.MethodPut, "/v2.0/routers/"+routerID+"/add_router_interface",
		fmt.Sprintf(`{"subnet_id":"%s"}`, subID), hdr)
	if code != http.StatusOK {
		t.Errorf("attach subnet without gateway: code=%d; want 200 (body=%s)", code, body)
	}
}

// TestSubnetPortAdminCarveOut_DB — GetNetwork's admin carve-out (see
// TestGetNetworkAdminCarveOut_DB) stopped at networks: an admin could show
// a foreign private NETWORK but 404 on the subnet ids its own body lists,
// and list [] for them. Upstream's get_subnet policy includes
// ADMIN_OR_NET_OWNER_READER; the carve-out now covers subnets and ports,
// their listings under an explicit network_id pointer, and a member of
// another project still gets 404 (SPEC-028).
func TestSubnetPortAdminCarveOut_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	otherProject := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, domain_id, enabled) VALUES ($1,$2,NULL,true)`,
		otherProject, "spec028-sub-carve-"+otherProject[:8]); err != nil {
		t.Fatalf("insert other project: %v", err)
	}
	netID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared, external) VALUES ($1,$2,$3,false,false)`,
		netID, "spec028-sub-carve-net-"+netID[:8], otherProject); err != nil {
		t.Fatalf("insert network: %v", err)
	}
	subID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version) VALUES ($1,$2,$3,$4,$5,$6,4)`,
		subID, "spec028-sub-carve-sub-"+subID[:8], netID, otherProject, "10.234.0.0/24", "10.234.0.1"); err != nil {
		t.Fatalf("insert subnet: %v", err)
	}
	portID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, mac_address, admin_state_up, status, fixed_ips, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,true,'ACTIVE','[]'::jsonb,now(),now())`,
		portID, "spec028-sub-carve-port-"+portID[:8], netID, otherProject, "02:00:00:00:23:28"); err != nil {
		t.Fatalf("insert port: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM ports WHERE id=$1", portID)
		testPool.Exec(ctx, "DELETE FROM subnets WHERE id=$1", subID)
		testPool.Exec(ctx, "DELETE FROM networks WHERE id=$1", netID)
		testPool.Exec(ctx, "DELETE FROM projects WHERE id=$1", otherProject)
	})

	r := newNeutronTestRouter()
	adminHdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	memberHdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	if code, body := doReq(r, http.MethodGet, "/v2.0/subnets/"+subID, adminHdr); code != http.StatusOK {
		t.Errorf("admin shows foreign subnet: code=%d; want 200 (body=%s)", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/subnets/"+subID, memberHdr); code != http.StatusNotFound {
		t.Errorf("member shows foreign subnet: code=%d; want 404 (body=%s)", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/subnets?network_id="+netID, adminHdr); code != http.StatusOK || !strings.Contains(body, subID) {
		t.Errorf("admin lists foreign network's subnets: code=%d body=%s; want 200 containing the subnet", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/ports/"+portID, adminHdr); code != http.StatusOK {
		t.Errorf("admin shows foreign port: code=%d; want 200 (body=%s)", code, body)
	}
	// Round 8: the row's OWN project is rendered, never the caller's —
	// stamping the admin's project on a foreign port tells a client a
	// resource is theirs when it is not.
	if code, body := doReq(r, http.MethodGet, "/v2.0/ports/"+portID, adminHdr); code != http.StatusOK || !strings.Contains(body, `"tenant_id":"`+otherProject+`"`) {
		t.Errorf("admin shows foreign port: tenant_id must be the row's owner %s (code=%d body=%s)", otherProject, code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/ports/"+portID, memberHdr); code != http.StatusNotFound {
		t.Errorf("member shows foreign port: code=%d; want 404 (body=%s)", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/ports?network_id="+netID, adminHdr); code != http.StatusOK || !strings.Contains(body, portID) {
		t.Errorf("admin lists foreign network's ports: code=%d body=%s; want 200 containing the port", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/ports?network_id="+netID, adminHdr); code != http.StatusOK || !strings.Contains(body, `"tenant_id":"`+otherProject+`"`) {
		t.Errorf("admin lists foreign network's ports: tenant_id must be the row's owner %s (code=%d body=%s)", otherProject, code, body)
	}
}

// TestListTenantFilterHonoredForAdmin_DB — the admin tenant_id filter was
// honored by the NETWORKS listing only; every other list silently ignored
// it and presented the ADMIN's rows as the target project's. Router here
// stands for the fixed pattern (subnets/ports/FIPs/cinder share it);
// upstream honours the filter for admin tokens (SPEC-028).
func TestListTenantFilterHonoredForAdmin_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	otherProject := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, domain_id, enabled) VALUES ($1,$2,NULL,true)`,
		otherProject, "spec028-rt-tenant-"+otherProject[:8]); err != nil {
		t.Fatalf("insert other project: %v", err)
	}
	otherRouter := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO routers (id, name, project_id) VALUES ($1,$2,$3)`,
		otherRouter, "spec028-rt-tenant-rt-"+otherRouter[:8], otherProject); err != nil {
		t.Fatalf("insert other router: %v", err)
	}
	ownRouter := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO routers (id, name, project_id) VALUES ($1,$2,$3)`,
		ownRouter, "spec028-rt-tenant-own-"+ownRouter[:8], seededProjectID); err != nil {
		t.Fatalf("insert own router: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM routers WHERE id = ANY($1)", []string{otherRouter, ownRouter})
		testPool.Exec(ctx, "DELETE FROM projects WHERE id=$1", otherProject)
	})

	r := newNeutronTestRouter()
	adminHdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	memberHdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	// Admin + tenant filter: the OTHER project's router appears, the
	// admin's own does not.
	code, body := doReq(r, http.MethodGet, "/v2.0/routers?tenant_id="+otherProject, adminHdr)
	if code != http.StatusOK || !strings.Contains(body, otherRouter) || strings.Contains(body, ownRouter) {
		t.Errorf("admin routers?tenant_id=<other>: code=%d body=%s; want only the other project's router", code, body)
	}
	// A non-admin's tenant filter stays intersected with their scope: the
	// foreign router never appears.
	code, body = doReq(r, http.MethodGet, "/v2.0/routers?tenant_id="+otherProject, memberHdr)
	if code != http.StatusOK || strings.Contains(body, otherRouter) {
		t.Errorf("member routers?tenant_id=<other>: code=%d body=%s; must not contain the foreign router", code, body)
	}

	// Round 8: the show must not dead-end the listing's rows — an admin
	// follow-up `router show` on the foreign router answers 200, a member's
	// 404 (the same carve-out GetNetwork/GetSubnet/GetPort carry).
	if code, body := doReq(r, http.MethodGet, "/v2.0/routers/"+otherRouter, adminHdr); code != http.StatusOK {
		t.Errorf("admin shows foreign router: code=%d; want 200 (body=%s)", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.0/routers/"+otherRouter, memberHdr); code != http.StatusNotFound {
		t.Errorf("member shows foreign router: code=%d; want 404 (body=%s)", code, body)
	}
}

// TestAllocateIPHandlesTinyAndIPv6Pools — upstream Neutron's generate_pools()
// allocates from /30, /31, /32 and every IPv6 subnet, excluding only the
// network/broadcast addresses where they exist; it refuses nothing as "too
// small". The old allocator returned "" for maxOffset <= 10 (every /29–/30)
// and for IPv6 (int64 overflow), turning valid subnets into 400s. Also guards
// the old failure mode: rand.Int panics on a non-positive bound.
func TestAllocateIPHandlesTinyAndIPv6Pools(t *testing.T) {
	for _, cidr := range []string{
		"10.0.0.0/30",
		"10.0.0.0/29",
		"10.0.0.0/31",
		"10.0.0.0/32",
		"fd00::/64",
		"fd00::/126",
		"10.0.0.0/24", // healthy pool, unchanged
	} {
		cidr := cidr
		t.Run(cidr, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("allocateIP(%q) panicked: %v", cidr, r)
				}
			}()
			if got := allocateIP(cidr); got == "" {
				t.Errorf("allocateIP(%q) = \"\"; want an address (upstream allocates from this pool)", cidr)
			}
		})
	}

	// Invalid input still fails closed rather than panicking.
	if got := allocateIP("not-a-cidr"); got != "" {
		t.Errorf("allocateIP(\"not-a-cidr\") = %q; want \"\"", got)
	}
}

// TestCreatePortAllocatesFromTinyPool — a /30 (two usable hosts, both
// candidates when the subnet has no gateway) is a valid upstream pool. The old
// allocator refused it with a 400; now the port must be created with a real
// fixed IP and nothing blank may be persisted. The same must hold for IPv6,
// which the old int64-overflow guard refused wholesale (SPEC-028, round 9).
func TestCreatePortAllocatesFromTinyPool(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	// createNetSubnet creates a network plus one subnet and returns the network
	// id. Cidr/ipVersion are the only varying inputs between the IPv4 and IPv6
	// halves of this test.
	createNetSubnet := func(name, cidr string, ipVersion int) string {
		t.Helper()
		code, body := doReqBody(r, http.MethodPost, "/v2.0/networks",
			fmt.Sprintf(`{"network":{"name":%q}}`, name+"-net"), hdr)
		if code != http.StatusCreated {
			t.Fatalf("create network %s: code=%d (body=%s)", name, code, body)
		}
		var net struct {
			Network struct {
				ID string `json:"id"`
			} `json:"network"`
		}
		if err := json.Unmarshal([]byte(body), &net); err != nil {
			t.Fatalf("unmarshal network: %v (body=%s)", err, body)
		}
		t.Cleanup(func() {
			testPool.Exec(ctx, "DELETE FROM ports WHERE network_id=$1", net.Network.ID)
			testPool.Exec(ctx, "DELETE FROM subnets WHERE network_id=$1", net.Network.ID)
			testPool.Exec(ctx, "DELETE FROM networks WHERE id=$1", net.Network.ID)
		})

		// gateway_ip null makes BOTH /30 host addresses candidates, which is
		// exactly the pool the old allocator used to refuse.
		code, body = doReqBody(r, http.MethodPost, "/v2.0/subnets",
			fmt.Sprintf(`{"subnet":{"name":%q,"network_id":%q,"cidr":%q,"gateway_ip":null,"ip_version":%d}}`,
				name+"-sub", net.Network.ID, cidr, ipVersion), hdr)
		if code != http.StatusCreated {
			t.Fatalf("create %s subnet: code=%d (body=%s)", cidr, code, body)
		}
		return net.Network.ID
	}

	assertAllocated := func(netID, label string) {
		t.Helper()
		code, body := doReqBody(r, http.MethodPost, "/v2.0/ports",
			fmt.Sprintf(`{"port":{"network_id":%q,"name":%q}}`, netID, label), hdr)
		if code != http.StatusCreated {
			t.Fatalf("%s port create: code=%d body=%s; want 201 (the pool is valid upstream)", label, code, body)
		}
		var created struct {
			Port struct {
				FixedIPs []struct {
					IPAddress string `json:"ip_address"`
				} `json:"fixed_ips"`
			} `json:"port"`
		}
		if err := json.Unmarshal([]byte(body), &created); err != nil {
			t.Fatalf("%s: unmarshal port: %v (body=%s)", label, err, body)
		}
		if len(created.Port.FixedIPs) == 0 || strings.TrimSpace(created.Port.FixedIPs[0].IPAddress) == "" {
			t.Fatalf("%s: fixed_ips = %+v; want a non-blank ip_address (body=%s)", label, created.Port.FixedIPs, body)
		}
		// The response is not enough: a blank address must never reach the table.
		var stored sql.NullString
		if err := testPool.QueryRow(ctx,
			`SELECT fixed_ips->0->>'ip_address' FROM ports WHERE network_id=$1`, netID).Scan(&stored); err != nil {
			t.Fatalf("%s: read back fixed_ips: %v", label, err)
		}
		if !stored.Valid || strings.TrimSpace(stored.String) == "" {
			t.Fatalf("%s: persisted ip_address is %v; want a non-blank address", label, stored)
		}
	}

	assertAllocated(createNetSubnet("spec028-r9-tiny", "10.198.0.0/30", 4), "tiny-ipv4")
	assertAllocated(createNetSubnet("spec028-r9-tiny6", "fd00:198::/64", 6), "tiny-ipv6")
}
