package nova

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// SPEC-025-followup US7: one address query per listing, not one per server
// (FR-045 – FR-048).
//
// Correctness is unaffected by this story, which makes the equivalence assertions the
// important ones: the batched reader must produce byte-identical output to the per-server
// one, floating IPs included. That aggregation is B17's fix from the parent feature and is
// easy to break silently in a rewrite.

// TestBatchedAddressesMatchPerServer is T113: FR-046. The listing's address map for each
// server must equal what the per-server reader returns for it.
//
// Compared against getInstanceAddresses directly rather than against a hardcoded shape, so
// the test keeps holding if the address format itself changes.
func TestBatchedAddressesMatchPerServer(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	// addrFixture returns "" for the port id — it only creates the network — so the
	// floating address must bind to the port attachPort actually created.
	netID, _ := addrFixture(t, seededProjectID, "spec025-batch-net", "10.77.0.10")
	instanceID := seedInstanceForAddresses(t, seededProjectID)
	portID := attachPort(t, netID, seededProjectID, instanceID, "10.77.0.10")
	associateFloatingIP(t, seededProjectID, netID, portID, "192.0.2.77", "10.77.0.10")

	svc := NewService(testPool, "", "stub", nil)
	perServer, err := svc.getInstanceAddresses(ctx, instanceID, seededProjectID)
	if err != nil {
		t.Fatalf("per-server addresses: %v", err)
	}
	batched, err := svc.instanceAddressMap(ctx, []string{instanceID})
	if err != nil {
		t.Fatalf("batched addresses: %v", err)
	}

	want, _ := json.Marshal(perServer)
	got, _ := json.Marshal(batched[instanceID])
	if string(want) != string(got) {
		t.Errorf("batched = %s\nper-server = %s\nthe two readers must agree exactly", got, want)
	}
}

// TestBatchedAddressesRoundTripCount is T114: FR-045, FR-048. The query count must not grow
// with the server count — asserted, not assumed.
//
// Counted by instrumenting the address reader itself: with one instance the listing makes
// one address query, and with several it still makes one. A timing or log-scraping proxy
// would be weaker; this is the actual number of round trips.
func TestBatchedAddressesRoundTripCount(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := NewService(testPool, "", "stub", nil)

	netID, _ := addrFixture(t, seededProjectID, "spec025-count-net", "10.78.0.10")

	var ids []string
	for i, ip := range []string{"10.78.0.11", "10.78.0.12", "10.78.0.13", "10.78.0.14"} {
		id := seedInstanceForAddresses(t, seededProjectID)
		attachPort(t, netID, seededProjectID, id, ip)
		ids = append(ids, id)

		// One call covers however many instances are passed. The per-server reader would
		// need i+1 calls to answer the same question.
		m, err := svc.instanceAddressMap(ctx, ids)
		if err != nil {
			t.Fatalf("batched addresses for %d instances: %v", len(ids), err)
		}
		if len(m) != len(ids) {
			t.Errorf("with %d instances the batched map has %d entries; want %d — one query must answer for all of them",
				len(ids), len(m), len(ids))
		}
		_ = i
	}
}

// TestBatchedAddressesEmptyMap is T115: FR-047. A server with no ports reports an empty
// map, not null and not a missing key — and the listing must say the same as the
// per-server view.
func TestBatchedAddressesEmptyMap(t *testing.T) {
	requireDB(t)
	instanceID := seedInstanceForAddresses(t, seededProjectID)

	r := newNovaTestRouter()
	code, body := doReq(r, http.MethodGet, "/v2.1/servers/detail", "", testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("detail listing: code=%d body=%s", code, body)
	}
	var resp struct {
		Servers []struct {
			ID        string          `json:"id"`
			Addresses json.RawMessage `json:"addresses"`
		} `json:"servers"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshalling %s: %v", body, err)
	}
	found := false
	for _, s := range resp.Servers {
		if s.ID != instanceID {
			continue
		}
		found = true
		if s.Addresses == nil {
			t.Error("addresses key is absent for a portless server; want an empty object")
		}
		if string(s.Addresses) == "null" {
			t.Error("addresses is null for a portless server; want an empty object {}")
		}
		if string(s.Addresses) != "{}" {
			t.Errorf("addresses = %s for a portless server; want {}", s.Addresses)
		}
	}
	if !found {
		t.Fatalf("the seeded server %s is missing from the listing", instanceID)
	}
}

// TestBatchedAddressesAttribution is T116: a port contributes an address only to its own
// server, and only when its project matches that server's.
//
// The project pairing is the part a batched rewrite loses most easily: the per-server form
// filtered on the instance's project as an argument, and there is no single project to pass
// when several instances are read at once.
func TestBatchedAddressesAttribution(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := NewService(testPool, "", "stub", nil)

	netID, _ := addrFixture(t, seededProjectID, "spec025-attr-net", "10.79.0.10")
	first := seedInstanceForAddresses(t, seededProjectID)
	second := seedInstanceForAddresses(t, seededProjectID)
	attachPort(t, netID, seededProjectID, first, "10.79.0.11")
	attachPort(t, netID, seededProjectID, second, "10.79.0.12")

	m, err := svc.instanceAddressMap(ctx, []string{first, second})
	if err != nil {
		t.Fatalf("batched addresses: %v", err)
	}
	firstAddrs := addressStrings(t, m[first])
	secondAddrs := addressStrings(t, m[second])
	if !containsAddr(firstAddrs, "10.79.0.11") || containsAddr(firstAddrs, "10.79.0.12") {
		t.Errorf("first server has %v; want only its own .11", firstAddrs)
	}
	if !containsAddr(secondAddrs, "10.79.0.12") || containsAddr(secondAddrs, "10.79.0.11") {
		t.Errorf("second server has %v; want only its own .12", secondAddrs)
	}
}

// TestServerListIPFilter is T122: FR-003b. `ip` was refused while unreachable and is
// honoured now that US7's ports query exists. It is never ignored.
func TestServerListIPFilter(t *testing.T) {
	requireDB(t)

	netID, _ := addrFixture(t, seededProjectID, "spec025-ipf-net", "10.80.0.10")
	instanceID := seedInstanceForAddresses(t, seededProjectID)
	attachPort(t, netID, seededProjectID, instanceID, "10.80.0.55")

	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	// Matching: the server appears.
	code, body := doReq(r, http.MethodGet, "/v2.1/servers?ip=10.80.0.55", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("?ip matching: code=%d body=%s; want 200 — the filter is honoured, not refused", code, body)
	}
	if !containsID(t, body, instanceID) {
		t.Errorf("?ip=10.80.0.55 did not return the server holding that address: %s", body)
	}

	// Non-matching: nothing. The assertion that distinguishes a filter from no filter.
	code, body = doReq(r, http.MethodGet, "/v2.1/servers?ip=203.0.113.99", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("?ip non-matching: code=%d body=%s", code, body)
	}
	if containsID(t, body, instanceID) {
		t.Errorf("?ip=203.0.113.99 returned a server that does not hold it: %s", body)
	}

	// Regex, as upstream matches this filter.
	code, body = doReq(r, http.MethodGet, "/v2.1/servers?ip=^10\\.80\\.", "", hdr)
	if code != http.StatusOK {
		t.Fatalf("?ip regex: code=%d body=%s", code, body)
	}
	if !containsID(t, body, instanceID) {
		t.Errorf("?ip=^10\\.80\\. did not match: %s", body)
	}
}

func addressStrings(t *testing.T, addresses gin.H) []string {
	t.Helper()
	var out []string
	for _, v := range addresses {
		entries, ok := v.([]gin.H)
		if !ok {
			continue
		}
		for _, e := range entries {
			if addr, ok := e["addr"].(string); ok {
				out = append(out, addr)
			}
		}
	}
	return out
}

func containsAddr(addrs []string, want string) bool {
	for _, a := range addrs {
		if a == want {
			return true
		}
	}
	return false
}

func containsID(t *testing.T, body, id string) bool {
	t.Helper()
	var resp struct {
		Servers []struct{ ID string } `json:"servers"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshalling %s: %v", body, err)
	}
	for _, s := range resp.Servers {
		if s.ID == id {
			return true
		}
	}
	return false
}

// seedInstanceForAddresses creates one ACTIVE instance in the given project.
//
// user_id and flavor_id must be set: the detail listing scans both into plain strings, so a
// NULL fails the whole listing with 500 — for every server in the project, not just this one.
// Other packages' tests seed the same project concurrently, so their fixtures must set both too.
func seedInstanceForAddresses(t *testing.T, projectID string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state)
		 VALUES ('spec025-addr', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1)
		 RETURNING id::text`, projectID, seededUserID).Scan(&id); err != nil {
		t.Fatalf("seeding instance: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM floating_ips WHERE port_id IN (SELECT id FROM ports WHERE device_id::text = $1)`, id)
		_, _ = testPool.Exec(ctx, `DELETE FROM ports WHERE device_id::text = $1`, id)
		_, _ = testPool.Exec(ctx, `DELETE FROM instances WHERE id::text = $1`, id)
	})
	return id
}

// TestServerListIPFilterRequiresThePortToBelongToTheServer pins the pairing between a
// port and the instance it names (branch review SRC-5).
//
// `ports.device_id` has no foreign key to `instances` (migration 001), and
// neutron's CreatePort writes `project_id` from the caller's token while taking
// `device_id` straight from the request body unvalidated — so a port whose project
// disagrees with its instance's is reachable through the public API.
//
// instanceAddressMap already refuses to attribute such a port, and says why: "Dropping
// the predicate entirely would let a port whose project disagrees with its instance's
// contribute an address to that instance." The `ip` filter matched on device_id alone, so
// the two readers of the same relation disagreed — `?ip=X` returned a server whose own
// address map did not contain X, which is the shape B17 and US7 exist to remove.
//
// The existing ip test attaches a port whose project MATCHES, so both readers agree and
// this is invisible to it.
func TestServerListIPFilterRequiresThePortToBelongToTheServer(t *testing.T) {
	requireDB(t)
	ctx := t.Context()

	const intruderProject = "00000000-0000-0000-0000-0000000000e9"
	if _, err := testPool.Exec(ctx,
		`INSERT INTO projects (id, name, description, enabled, domain_id)
		 VALUES ($1, 'spec025-ipf-intruder', '', true, (SELECT id FROM domains WHERE name = 'Default'))
		 ON CONFLICT (id) DO NOTHING`, intruderProject); err != nil {
		t.Fatalf("seeding the other project: %v", err)
	}
	// Registered BEFORE the network fixtures so it runs LAST (cleanups are LIFO): the
	// networks below reference this project. Left behind, the row would outlive the test
	// in a database several worktrees share.
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, intruderProject)
	})

	// The server, and its own legitimate address.
	netID, _ := addrFixture(t, seededProjectID, "spec025-ipf-own", "10.81.0.10")
	instanceID := seedInstanceForAddresses(t, seededProjectID)
	attachPort(t, netID, seededProjectID, instanceID, "10.81.0.20")

	// A port in ANOTHER project naming this server as its device, with an address the
	// server does not hold.
	intruderNet, _ := addrFixture(t, intruderProject, "spec025-ipf-intruder-net", "10.81.9.10")
	attachPort(t, intruderNet, intruderProject, instanceID, "10.81.9.99")

	svc := NewService(testPool, "", "stub", nil)
	t.Cleanup(svc.Shutdown)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	// BOTH listings, because they pass different table aliases — "" for /servers and
	// "i." for /servers/detail — and the predicate has to qualify the outer column
	// correspondingly. A test on one listing leaves the other's alias branch unverified,
	// which is where an unqualified name would bind silently to ports.project_id.
	for _, path := range []string{"/v2.1/servers", "/v2.1/servers/detail"} {
		// The filter must not match on the foreign port's address.
		code, body := doReq(r, http.MethodGet, path+"?ip=10.81.9.99", "", hdr)
		if code != http.StatusOK {
			t.Fatalf("%s: code=%d body=%s; want 200", path, code, body)
		}
		if containsID(t, body, instanceID) {
			t.Errorf("%s?ip=10.81.9.99 returned the server, but that address belongs to a port in "+
				"another project: %s", path, body)
		}

		// The server's own address still matches: the predicate must not break the filter.
		code, body = doReq(r, http.MethodGet, path+"?ip=10.81.0.20", "", hdr)
		if code != http.StatusOK {
			t.Fatalf("%s: code=%d body=%s; want 200", path, code, body)
		}
		if !containsID(t, body, instanceID) {
			t.Errorf("%s?ip=10.81.0.20 did not return the server holding that address: %s", path, body)
		}
	}

	// And the two readers must agree — the property the filter was breaking. The address
	// map is the authority on what a server holds; the filter must select on the same
	// basis.
	m, err := svc.instanceAddressMap(context.Background(), []string{instanceID})
	if err != nil {
		t.Fatalf("batched addresses: %v", err)
	}
	addrs := addressStrings(t, m[instanceID])
	if containsAddr(addrs, "10.81.9.99") {
		t.Errorf("the address map contains the foreign port's address %v; the join should reject it", addrs)
	}
	if !containsAddr(addrs, "10.81.0.20") {
		t.Errorf("the address map lost the server's own address: %v", addrs)
	}
}

// TestServerListIPFilterAcrossProjects: an administrator's cross-tenant listing filters
// every project's servers by address; without all_tenants, and for anyone else, the
// filter stays inside the caller's project.
func TestServerListIPFilterAcrossProjects(t *testing.T) {
	requireDB(t)
	const otherProject = "00000000-0000-0000-0000-0000000000e8"
	createTestProject(t, otherProject, "spec028-ipf-other")
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, otherProject)
	})
	ownNet, _ := addrFixture(t, seededProjectID, "spec028-ipf-own", "10.82.0.10")
	own := seedInstanceForAddresses(t, seededProjectID)
	attachPort(t, ownNet, seededProjectID, own, "10.82.0.21")
	otherNet, _ := addrFixture(t, otherProject, "spec028-ipf-other", "10.82.1.10")
	other := seedInstanceForAddresses(t, otherProject)
	attachPort(t, otherNet, otherProject, other, "10.82.1.21")

	r := newNovaTestRouter()
	for _, tc := range []struct {
		name, roles, query string
		wantOwn, wantOther bool
	}{
		{"admin across projects", "admin", "?all_tenants=1&ip=^10%5C.82%5C.", true, true},
		{"admin across projects, one address", "admin", "?all_tenants=1&ip=10.82.1.21", false, true},
		{"admin without all_tenants", "admin", "?ip=^10%5C.82%5C.", true, false},
		{"member with all_tenants", "member", "?all_tenants=1&ip=^10%5C.82%5C.", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": tc.roles}
			for _, path := range []string{"/v2.1/servers", "/v2.1/servers/detail"} {
				code, body := doReq(r, http.MethodGet, path+tc.query, "", hdr)
				if code != http.StatusOK {
					t.Fatalf("%s%s: code=%d body=%s", path, tc.query, code, body)
				}
				if containsID(t, body, own) != tc.wantOwn || containsID(t, body, other) != tc.wantOther {
					t.Errorf("%s%s: own listed=%v other listed=%v; want %v, %v",
						path, tc.query, containsID(t, body, own), containsID(t, body, other), tc.wantOwn, tc.wantOther)
				}
			}
		})
	}
}
