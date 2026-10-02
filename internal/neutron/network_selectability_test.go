package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// SPEC-020 (cloud-storage#533) F7 — the launch wizard's Networks step rendered no
// selectable rows. These guard the two data-shape properties it depends on.

// doReqBody issues a request with a JSON body. The package's shared doReq takes no
// body, and extending it would touch every existing caller, so the create-path test
// uses this local helper instead.
func doReqBody(r http.Handler, method, path, body string, headers map[string]string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestNetworkResponses_CarryProjectID: every build site must emit project_id
// alongside the legacy tenant_id. The object is hand-built in three places
// (CreateNetwork, ListNetworks, GetNetwork); adding it to only some is the defect.
func TestNetworkResponses_CarryProjectID(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	// create
	code, body := doReqBody(r, http.MethodPost, "/v2.0/networks",
		`{"network":{"name":"spec020-selectability","admin_state_up":true}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create network: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		Network map[string]any `json:"network"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("parse create: %v", err)
	}
	netID, _ := created.Network["id"].(string)
	if netID == "" {
		t.Fatalf("create returned no id: %s", body)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM subnets WHERE network_id = $1", netID) //nolint:errcheck // best-effort cleanup
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID)        //nolint:errcheck // best-effort cleanup
	})
	assertProjectID(t, "create", created.Network)

	// show
	code, body = doReq(r, http.MethodGet, "/v2.0/networks/"+netID, hdr)
	if code != http.StatusOK {
		t.Fatalf("show network: code=%d body=%s", code, body)
	}
	var shown struct {
		Network map[string]any `json:"network"`
	}
	if err := json.Unmarshal([]byte(body), &shown); err != nil {
		t.Fatalf("parse show: %v", err)
	}
	assertProjectID(t, "show", shown.Network)

	// list
	code, body = doReq(r, http.MethodGet, "/v2.0/networks", hdr)
	if code != http.StatusOK {
		t.Fatalf("list networks: code=%d body=%s", code, body)
	}
	var listed struct {
		Networks []map[string]any `json:"networks"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("parse list: %v", err)
	}
	if len(listed.Networks) == 0 {
		t.Fatal("list returned no networks")
	}
	for _, n := range listed.Networks {
		assertProjectID(t, "list", n)
	}
}

func assertProjectID(t *testing.T, site string, network map[string]any) {
	t.Helper()
	projectID, ok := network["project_id"].(string)
	if !ok || projectID == "" {
		t.Errorf("%s: network is missing project_id (the launch wizard reads it); got keys %v",
			site, keysOf(network))
		return
	}
	if tenantID, _ := network["tenant_id"].(string); tenantID != projectID {
		t.Errorf("%s: tenant_id=%q but project_id=%q; they must agree", site, tenantID, projectID)
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestForeignSubnetOnOwnNetworkIsListableAndResolvable pins the one case that only
// `n.project_id = $1` satisfies: a subnet owned by **another** project sitting on a
// network owned by the caller. It asserts all three views agree — the network's subnets
// array, the subnet listing, and the by-id show.
//
// TestSubnetReferenceIntegrity does not reach it — it inserts network and subnet under the
// same project, so the older `s.project_id = $1` predicate already matched, and a shared
// network is covered by `n.shared = true`. Drop the disjunct and every existing test still
// passes while FR-007 breaks for this shape: networkSubnetIDs applies no project predicate,
// so the network response advertises the foreign subnet's id, and the subnet listing then
// cannot resolve it — Horizon renders a null row and the wizard's network step fails.
//
// The show arm was raised on PR #79 by Copilot: widening the listing alone left GET
// /v2.0/subnets/{id} on the narrower `s.project_id = $2 OR n.shared` predicate, so the
// listing handed out an id that show answered 404 for. Upstream settles the direction —
// neutron's get_subnet policy carries ADMIN_OR_NET_OWNER_READER
// (`project_id:%(network:project_id)s`), so the network's owner may read a subnet on it
// whoever created it.
//
// The shape is reachable because CreateSubnet accepts any `network_id` without checking
// that the caller owns the network — pre-existing on 71aa00d and left alone here, since
// rejecting those requests would be a new failure mode (FR-016). It is also reachable
// without that defect: an admin creating a subnet on a tenant's network records the
// admin's project, because CreateSubnet ignores any project_id in the body.
func TestForeignSubnetOnOwnNetworkIsListableAndResolvable(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	otherProject := createTestProject(t, "spec020-foreign-subnet-owner")
	netID, subnetID := uuid.NewString(), uuid.NewString()

	// Network: the caller's. Subnet on it: the other project's.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared) VALUES ($1, $2, $3, false)`,
		netID, "spec020-foreign-"+netID[:8], seededProjectID); err != nil {
		t.Fatalf("insert network: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version,
		                      enable_dhcp, dns_nameservers, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, '10.44.0.0/24', '10.44.0.1', 4, true, '{}', NOW(), NOW())`,
		subnetID, "spec020-foreign-sub", netID, otherProject); err != nil {
		t.Fatalf("insert foreign subnet: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM subnets WHERE id = $1", subnetID) //nolint:errcheck // best-effort cleanup
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID)   //nolint:errcheck // best-effort cleanup
	})

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	// The network response advertises it...
	_, netBody := doReq(r, http.MethodGet, "/v2.0/networks", hdr)
	var nets struct {
		Networks []struct {
			ID      string   `json:"id"`
			Subnets []string `json:"subnets"`
		} `json:"networks"`
	}
	if err := json.Unmarshal([]byte(netBody), &nets); err != nil {
		t.Fatalf("parse networks: %v", err)
	}
	var advertised bool
	for _, n := range nets.Networks {
		if n.ID != netID {
			continue
		}
		for _, id := range n.Subnets {
			if id == subnetID {
				advertised = true
			}
		}
	}
	if !advertised {
		t.Fatalf("network %s does not advertise the foreign subnet %s; the premise of this test "+
			"is that networkSubnetIDs applies no project predicate", netID, subnetID)
	}

	// ...so the subnet listing must resolve it.
	_, subBody := doReq(r, http.MethodGet, "/v2.0/subnets", hdr)
	var subs struct {
		Subnets []struct {
			ID string `json:"id"`
		} `json:"subnets"`
	}
	if err := json.Unmarshal([]byte(subBody), &subs); err != nil {
		t.Fatalf("parse subnets: %v", err)
	}
	var listed bool
	for _, s := range subs.Subnets {
		if s.ID == subnetID {
			listed = true
		}
	}
	if !listed {
		t.Errorf("GET /v2.0/subnets omits subnet %s, which network %s advertises — the caller owns the "+
			"network but not the subnet, so only the `n.project_id = $1` disjunct covers it (FR-007)",
			subnetID, netID)
	}

	// ...and the by-id show must resolve it too, or the listing hands out an id that 404s.
	code, showBody := doReq(r, http.MethodGet, "/v2.0/subnets/"+subnetID, hdr)
	if code != http.StatusOK {
		t.Errorf("GET /v2.0/subnets/%s returned %d (%s); the subnet listing returns this id for the "+
			"same caller, so show must resolve it — GetSubnet needs the same `n.project_id` disjunct",
			subnetID, code, showBody)
	}
}

// TestSubnetReferenceIntegrity: every id in a network's subnets array must resolve
// in the subnet listing the same caller can read. Horizon expands those ids against
// a separate subnet list and substitutes the resolved object; an unresolvable id
// becomes a null entry and the wizard row fails to render.
func TestSubnetReferenceIntegrity(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	netID, subnetID := uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared) VALUES ($1, $2, $3, false)`,
		netID, "spec020-integrity-"+netID[:8], seededProjectID); err != nil {
		t.Fatalf("insert network: %v", err)
	}
	// Populate the columns the list query scans: ListSubnets skips any row whose scan
	// fails, so a NULL in a non-nullable-scanned column would silently drop it.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version,
		                      enable_dhcp, dns_nameservers, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, '10.43.0.0/24', '10.43.0.1', 4, true, '{}', NOW(), NOW())`,
		subnetID, "spec020-integrity-sub", netID, seededProjectID); err != nil {
		t.Fatalf("insert subnet: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM subnets WHERE id = $1", subnetID) //nolint:errcheck // best-effort cleanup
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID)   //nolint:errcheck // best-effort cleanup
	})

	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	_, netBody := doReq(r, http.MethodGet, "/v2.0/networks", hdr)
	var nets struct {
		Networks []struct {
			ID      string   `json:"id"`
			Subnets []string `json:"subnets"`
		} `json:"networks"`
	}
	if err := json.Unmarshal([]byte(netBody), &nets); err != nil {
		t.Fatalf("parse networks: %v", err)
	}

	_, subBody := doReq(r, http.MethodGet, "/v2.0/subnets", hdr)
	var subs struct {
		Subnets []struct {
			ID string `json:"id"`
		} `json:"subnets"`
	}
	if err := json.Unmarshal([]byte(subBody), &subs); err != nil {
		t.Fatalf("parse subnets: %v", err)
	}
	listable := make(map[string]bool, len(subs.Subnets))
	for _, s := range subs.Subnets {
		listable[s.ID] = true
	}

	for _, n := range nets.Networks {
		for _, id := range n.Subnets {
			if !listable[id] {
				t.Errorf("network %s references subnet %s, which GET /v2.0/subnets does not return "+
					"for the same caller — Horizon renders that as a null row", n.ID, id)
			}
		}
	}
}

// TestNetworkSubnetIDsPropagatesFailure: the subnet-id list must surface a failure rather
// than return a short list.
//
// The helper used to log and return whatever it had gathered, on the reasoning that its
// callers had no error channel and the `subnets` field was advisory. It is not advisory —
// Horizon expands each id against the subnet listing and renders a null row for any it
// cannot resolve, which is the launch-wizard failure FR-007 exists to fix. A truncated
// list is therefore indistinguishable from a network whose subnets were deleted.
//
// Raised on PR #79 as ERR-2. The reviewer's two factual claims (silent scan drop, no
// rows.Err check) did not hold — both were already logged — but the design point did.
func TestNetworkSubnetIDsPropagatesFailure(t *testing.T) {
	requireDB(t)
	svc := NewService(testPool, "stub", nil)
	ctx := context.Background()

	// A malformed network id reaches a uuid-typed column: Postgres rejects it, which is a
	// stand-in for any query/scan/iteration failure. Previously this logged and returned
	// an empty list, which a caller could not tell apart from "no subnets".
	ids, err := svc.networkSubnetIDs(ctx, "not-a-uuid")
	if err == nil {
		t.Errorf("networkSubnetIDs(bad id) = (%v, nil); want an error, not a short list", ids)
	}
	if ids != nil {
		t.Errorf("networkSubnetIDs(bad id) returned %v alongside the error; want nil so a caller cannot use a partial list", ids)
	}

	// And the happy path still yields a non-nil slice, so the JSON stays a list.
	netID := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared) VALUES ($1, $2, $3, false)`,
		netID, "spec020-subnetids-"+netID[:8], seededProjectID); err != nil {
		t.Fatalf("insert network: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID) //nolint:errcheck // best-effort cleanup
	})
	got, err := svc.networkSubnetIDs(ctx, netID)
	if err != nil {
		t.Fatalf("networkSubnetIDs(valid id): unexpected error %v", err)
	}
	if got == nil {
		t.Error("networkSubnetIDs returned nil for a subnet-less network; want an empty slice so JSON emits []")
	}
}
