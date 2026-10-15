package neutron

// SPEC-028 conformance fixes for the Neutron list surface, found by the CLI
// conformance suites (issue #105).

import (
	"context"
	"encoding/json"
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
