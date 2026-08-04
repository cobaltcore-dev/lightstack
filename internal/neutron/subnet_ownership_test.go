package neutron

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Two tenants for the entitlement matrix. The seeded project stands in for tenant A
// so the fixtures reference a project that really exists (subnets.project_id and
// networks.project_id are both foreign keys).
const (
	tenantA = seededProjectID
	tenantB = "00000000-0000-0000-0000-0000000b0b0b"
)

func doJSONReq(r *gin.Engine, method, path, body string, headers map[string]string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// ensureTenantB creates the second project once; the seeded one is tenant A.
func ensureTenantB(t *testing.T) {
	t.Helper()
	_, err := database.DB.Exec(t.Context(),
		`INSERT INTO projects (id, name, description, enabled, domain_id)
		 VALUES ($1, 'spec022-tenant-b', 'spec022 fixture', true,
		         (SELECT id FROM domains WHERE name = 'Default'))
		 ON CONFLICT (id) DO NOTHING`, tenantB)
	if err != nil {
		t.Fatalf("create tenant B: %v", err)
	}
}

// makeNetwork inserts a network owned by owner, shared or not, and removes it (with
// any subnets that leaked onto it) when the test ends.
func makeNetwork(t *testing.T, owner string, shared bool) string {
	t.Helper()
	ensureTenantB(t)
	var id string
	err := database.DB.QueryRow(t.Context(),
		`INSERT INTO networks (name, project_id, admin_state_up, status, shared, network_type, mtu)
		 VALUES ('spec022-net', $1, true, 'ACTIVE', $2, 'vxlan', 1500) RETURNING id`,
		owner, shared,
	).Scan(&id)
	if err != nil {
		t.Fatalf("create network for %s (shared=%v): %v", owner, shared, err)
	}
	t.Cleanup(func() {
		// context.Background(), not t.Context(): the test context is cancelled just
		// before Cleanup-registered functions run (testing.T.Context).
		ctx := context.Background()
		if _, err := database.DB.Exec(ctx, `DELETE FROM subnets WHERE network_id = $1`, id); err != nil {
			t.Logf("cleanup subnets of %s: %v", id, err)
		}
		if _, err := database.DB.Exec(ctx, `DELETE FROM networks WHERE id = $1`, id); err != nil {
			t.Logf("cleanup network %s: %v", id, err)
		}
	})
	return id
}

func subnetCountOn(t *testing.T, networkID string) int {
	t.Helper()
	var n int
	if err := database.DB.QueryRow(t.Context(),
		`SELECT count(*) FROM subnets WHERE network_id = $1`, networkID).Scan(&n); err != nil {
		t.Fatalf("counting subnets on %s: %v", networkID, err)
	}
	return n
}

func hdr(project string, roles ...string) map[string]string {
	h := map[string]string{"X-Test-Project": project}
	if len(roles) > 0 {
		h["X-Test-Roles"] = strings.Join(roles, ",")
	}
	return h
}

// TestCreateSubnetEntitlement is the whole of B8 (FR-009 – FR-013, FR-022): the eight
// rows of contracts/neutron-subnet-create.md.
//
// Rows 2 and 3 are the defect — both returned 201 before this feature, letting any
// tenant define the address range, gateway and DHCP behaviour on another tenant's
// network. Rows 1, 4 and 5 exist so a handler that simply refused everything could not
// pass: the fix must cost no legitimate capability.
func TestCreateSubnetEntitlement(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	ensureTenantB(t)

	ownB := makeNetwork(t, tenantB, false)
	foreignPrivate := makeNetwork(t, tenantA, false)
	foreignShared := makeNetwork(t, tenantA, true)
	ownBForAdmin := makeNetwork(t, tenantB, false)

	// wantKey is the top-level JSON key the refusal must carry. The status alone pins only
	// half of FR-010a: 404-for-unseen vs 403-for-shared is the deliberate decision, and a
	// handler answering the right status under the wrong envelope would pass a
	// status-only check. Left empty for the 201 rows — their shape is covered by
	// TestCreateSubnetOwnNetworkResponseShape.
	//
	// These are Nova-shaped (`itemNotFound`/`forbidden`/`badRequest`) because Neutron
	// currently uses the shared envelope. That is a tracked defect — cloud-storage#546 B10,
	// upstream emits `{"NeutronError": {type, message, detail}}` — so when B10 lands these
	// three values change with it, deliberately and visibly rather than silently.
	tests := []struct {
		name      string
		networkID string
		headers   map[string]string
		cidr      string
		wantCode  int
		wantKey   string
	}{
		{"1: own network", ownB, hdr(tenantB, "member"), "10.90.1.0/24", http.StatusCreated, ""},
		{"2: foreign private network", foreignPrivate, hdr(tenantB, "member"), "10.90.2.0/24", http.StatusNotFound, "itemNotFound"},
		{"3: foreign shared network", foreignShared, hdr(tenantB, "member"), "10.90.3.0/24", http.StatusForbidden, "forbidden"},
		{"4: admin on a foreign network", foreignPrivate, hdr(tenantB, "member", "admin"), "10.90.4.0/24", http.StatusCreated, ""},
		{"5: admin on own network", ownBForAdmin, hdr(tenantB, "admin"), "10.90.5.0/24", http.StatusCreated, ""},
		{"6: well-formed id naming nothing", "11111111-1111-1111-1111-111111111111", hdr(tenantB, "member"), "10.90.6.0/24", http.StatusNotFound, "itemNotFound"},
		{"7: malformed id", "not-a-uuid", hdr(tenantB, "member"), "10.90.7.0/24", http.StatusBadRequest, "badRequest"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"subnet":{"name":"s","network_id":%q,"cidr":%q,"ip_version":4}}`, tc.networkID, tc.cidr)
			code, resp := doJSONReq(r, http.MethodPost, "/v2.0/subnets", body, tc.headers)
			if code != tc.wantCode {
				t.Fatalf("POST /v2.0/subnets (%s): code=%d body=%s; want %d", tc.name, code, resp, tc.wantCode)
			}
			if tc.wantKey == "" {
				return
			}
			var env map[string]json.RawMessage
			if err := json.Unmarshal([]byte(resp), &env); err != nil {
				t.Fatalf("POST /v2.0/subnets (%s): unmarshal %s: %v", tc.name, resp, err)
			}
			if _, ok := env[tc.wantKey]; !ok {
				keys := make([]string, 0, len(env))
				for k := range env {
					keys = append(keys, k)
				}
				t.Fatalf("POST /v2.0/subnets (%s): %d body has keys %v, want %q — right status, wrong error envelope",
					tc.name, code, keys, tc.wantKey)
			}
		})
	}

	// Row 8: network_id absent entirely — the existing binding:"required" answers.
	code, resp := doJSONReq(r, http.MethodPost, "/v2.0/subnets",
		`{"subnet":{"name":"s","cidr":"10.90.8.0/24"}}`, hdr(tenantB, "member"))
	if code != http.StatusBadRequest {
		t.Fatalf("POST /v2.0/subnets (8: no network_id): code=%d body=%s; want 400", code, resp)
	}
}

// TestCreateSubnetRefusalLeavesNoResidue — FR-010, SC-003. A refusal that still wrote
// a row would be worse than the defect: the victim's views would show a subnet nobody
// could account for.
func TestCreateSubnetRefusalLeavesNoResidue(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	ensureTenantB(t)

	for _, tc := range []struct {
		name   string
		shared bool
	}{
		{"foreign private", false},
		{"foreign shared", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			netID := makeNetwork(t, tenantA, tc.shared)
			if n := subnetCountOn(t, netID); n != 0 {
				t.Fatalf("fixture network already has %d subnets", n)
			}

			body := fmt.Sprintf(`{"subnet":{"name":"intruder","network_id":%q,"cidr":"10.91.0.0/24"}}`, netID)
			code, resp := doJSONReq(r, http.MethodPost, "/v2.0/subnets", body, hdr(tenantB, "member"))
			if code < 400 {
				t.Fatalf("cross-tenant write was accepted: code=%d body=%s", code, resp)
			}
			if n := subnetCountOn(t, netID); n != 0 {
				t.Fatalf("a refused request left %d subnet(s) on the target network", n)
			}

			// The victim's own views must be clean too — the defect's real damage was
			// that tenant A's network and subnet listing advertised tenant B's subnet.
			code, listResp := doReq(r, http.MethodGet, "/v2.0/subnets", hdr(tenantA, "member"))
			if code != http.StatusOK {
				t.Fatalf("victim GET /v2.0/subnets: code=%d body=%s; want 200", code, listResp)
			}
			if strings.Contains(listResp, "intruder") {
				t.Fatalf("victim's subnet list shows the refused subnet: %s", listResp)
			}
		})
	}
}

// TestCreateSubnetOwnNetworkResponseShape — FR-013. "Same result" is easy to satisfy
// accidentally; "same response shape" is what a working client actually depends on, so
// assert the full field set rather than just the 201.
func TestCreateSubnetOwnNetworkResponseShape(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	netID := makeNetwork(t, tenantB, false)

	body := fmt.Sprintf(`{"subnet":{"name":"shape","network_id":%q,"cidr":"10.92.0.0/24","ip_version":4}}`, netID)
	code, resp := doJSONReq(r, http.MethodPost, "/v2.0/subnets", body, hdr(tenantB, "member"))
	if code != http.StatusCreated {
		t.Fatalf("POST /v2.0/subnets on own network: code=%d body=%s; want 201", code, resp)
	}

	var got struct {
		Subnet map[string]json.RawMessage `json:"subnet"`
	}
	if err := json.Unmarshal([]byte(resp), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", resp, err)
	}
	for _, field := range []string{
		"id", "name", "network_id", "tenant_id", "cidr", "gateway_ip", "ip_version",
		"enable_dhcp", "dns_nameservers", "allocation_pools", "host_routes",
		"service_types", "created_at", "updated_at",
	} {
		if _, ok := got.Subnet[field]; !ok {
			t.Fatalf("response lost the %q field: %s", field, resp)
		}
	}
	// OpenStack clients format this as a list and fail on JSON null.
	if string(got.Subnet["dns_nameservers"]) == "null" {
		t.Fatalf("dns_nameservers must be an empty list, not null: %s", resp)
	}
	if string(got.Subnet["tenant_id"]) != fmt.Sprintf("%q", tenantB) {
		t.Fatalf("tenant_id = %s, want the caller's project %q", got.Subnet["tenant_id"], tenantB)
	}
}

// TestCreateSubnetRefusalIsAudited — FR-011b, SC-004b. A refused cross-tenant write is
// the event an operator would go looking for afterwards; a legitimate creation is not
// an event at all. Rows 6 and 7 target no existing network, so there is no cross-tenant
// attempt to record.
func TestCreateSubnetRefusalIsAudited(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	ensureTenantB(t)

	foreignPrivate := makeNetwork(t, tenantA, false)
	foreignShared := makeNetwork(t, tenantA, true)
	ownB := makeNetwork(t, tenantB, false)

	tests := []struct {
		name      string
		networkID string
		cidr      string
		wantAudit bool
	}{
		{"foreign private network is audited", foreignPrivate, "10.93.1.0/24", true},
		{"foreign shared network is audited", foreignShared, "10.93.2.0/24", true},
		{"own network is not an event", ownB, "10.93.3.0/24", false},
		{"unknown network is not a cross-tenant attempt", "11111111-1111-1111-1111-111111111111", "10.93.4.0/24", false},
		{"malformed id is not a cross-tenant attempt", "not-a-uuid", "10.93.5.0/24", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := log.Logger
			log.Logger = zerolog.New(&buf)
			defer func() { log.Logger = restore }()

			body := fmt.Sprintf(`{"subnet":{"name":"s","network_id":%q,"cidr":%q}}`, tc.networkID, tc.cidr)
			doJSONReq(r, http.MethodPost, "/v2.0/subnets", body, hdr(tenantB, "member"))

			warns := 0
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if line == "" {
					continue
				}
				var rec struct {
					Level         string `json:"level"`
					CallerProject string `json:"caller_project"`
					NetworkID     string `json:"network_id"`
					NetworkOwner  string `json:"network_owner"`
				}
				if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Level != "warn" {
					continue
				}
				warns++
				// The record has to name all three parties or it cannot answer the
				// question an operator brings to it.
				if rec.CallerProject != tenantB || rec.NetworkID != tc.networkID || rec.NetworkOwner != tenantA {
					t.Fatalf("audit record is incomplete: caller=%q network=%q owner=%q; want %q/%q/%q",
						rec.CallerProject, rec.NetworkID, rec.NetworkOwner, tenantB, tc.networkID, tenantA)
				}
			}
			want := 0
			if tc.wantAudit {
				want = 1
			}
			if warns != want {
				t.Fatalf("%s: %d warn record(s), want %d; log was:\n%s", tc.name, warns, want, buf.String())
			}
		})
	}
}

// insertSubnetOwnedBy attaches a subnet owned by `owner` to `networkID`, the way an
// administrator's create does: CreateSubnet attributes the row to the *caller's*
// project, matching upstream's populate_project_id, so the subnet and the network it
// sits on can legitimately belong to different projects.
func insertSubnetOwnedBy(t *testing.T, networkID, owner, name, cidr string) string {
	t.Helper()
	var id string
	err := database.DB.QueryRow(t.Context(),
		`INSERT INTO subnets (name, network_id, project_id, cidr, gateway_ip, ip_version,
		                      enable_dhcp, dns_nameservers, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, '10.0.0.1', 4, false, '{}', NOW(), NOW()) RETURNING id`,
		name, networkID, owner, cidr,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert subnet %q owned by %s: %v", name, owner, err)
	}
	return id
}

// TestNetworkOwnerMaySeeAndRemoveForeignSubnet — FR-013a. FR-012 lets an administrator
// add a subnet to any tenant's network, and CreateSubnet attributes it to the admin's
// project. Before this, the three subnet paths disagreed about the result: the network
// owner saw the subnet in their listing (ListSubnets matches n.project_id), got a 404
// fetching it by id, and could not delete it — an address range on their own network
// they could see, not inspect, and not remove.
//
// Upstream grants the network's owner both: get_subnet is
// `PROJECT_READER or rule:shared or rule:external_network or ADMIN_OR_NET_OWNER_READER
// or SERVICE` and delete_subnet is `PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER`, where
// network_owner is `project_id:%(network:project_id)s`. O3K was stricter than upstream
// on exactly these two paths.
func TestNetworkOwnerMaySeeAndRemoveForeignSubnet(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	ensureTenantB(t)

	// Tenant A's network, carrying a subnet owned by tenant B — what an admin create
	// leaves behind.
	netA := makeNetwork(t, tenantA, false)
	subnetID := insertSubnetOwnedBy(t, netA, tenantB, "spec022-foreign-sub", "10.94.0.0/24")

	// The listing already showed it; the fetch must now agree.
	code, resp := doReq(r, http.MethodGet, "/v2.0/subnets/"+subnetID, hdr(tenantA, "member"))
	if code != http.StatusOK {
		t.Fatalf("network owner GET /v2.0/subnets/%s: code=%d body=%s; want 200", subnetID, code, resp)
	}
	if !strings.Contains(resp, "spec022-foreign-sub") {
		t.Fatalf("network owner GET: body %s is not the subnet", resp)
	}

	// A third party — neither the subnet's owner nor the network's — still sees nothing.
	stranger := createTestProject(t, "spec022-subnet-stranger")
	if code, resp = doReq(r, http.MethodGet, "/v2.0/subnets/"+subnetID, hdr(stranger, "member")); code != http.StatusNotFound {
		t.Fatalf("stranger GET /v2.0/subnets/%s: code=%d body=%s; want 404", subnetID, code, resp)
	}
	if code, resp = doJSONReq(r, http.MethodDelete, "/v2.0/subnets/"+subnetID, "", hdr(stranger, "member")); code != http.StatusNotFound {
		t.Fatalf("stranger DELETE /v2.0/subnets/%s: code=%d body=%s; want 404", subnetID, code, resp)
	}
	if n := subnetCountOn(t, netA); n != 1 {
		t.Fatalf("a refused stranger delete removed the subnet: %d left on the network", n)
	}

	// The network's owner may remove it — otherwise the address range on their own
	// network is theirs to live with and nobody's to clean up.
	if code, resp = doJSONReq(r, http.MethodDelete, "/v2.0/subnets/"+subnetID, "", hdr(tenantA, "member")); code != http.StatusNoContent {
		t.Fatalf("network owner DELETE /v2.0/subnets/%s: code=%d body=%s; want 204", subnetID, code, resp)
	}
	if n := subnetCountOn(t, netA); n != 0 {
		t.Fatalf("network owner delete left %d subnet(s) behind", n)
	}
}

// TestNetworkOwnerMayRenameForeignSubnet — FR-013a for update_subnet, which upstream
// scopes exactly like delete_subnet (`PROJECT_MEMBER or ADMIN_OR_NET_OWNER_MEMBER`).
// Renaming is strictly weaker than removing, so allowing the delete and refusing the
// rename would be an odd line to draw.
//
// Two things beyond the status code matter here. The UPDATE must actually take effect —
// re-filtering it on project_id returns 200 while storing nothing — and the response's
// tenant_id must name the subnet's owner, not the caller, now that they can differ.
func TestNetworkOwnerMayRenameForeignSubnet(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	ensureTenantB(t)

	netA := makeNetwork(t, tenantA, false)
	subnetID := insertSubnetOwnedBy(t, netA, tenantB, "spec022-rename-me", "10.96.0.0/24")

	code, resp := doJSONReq(r, http.MethodPut, "/v2.0/subnets/"+subnetID,
		`{"subnet":{"name":"renamed-by-owner"}}`, hdr(tenantA, "member"))
	if code != http.StatusOK {
		t.Fatalf("network owner PUT /v2.0/subnets/%s: code=%d body=%s; want 200", subnetID, code, resp)
	}

	// The rename must have reached the table, not just the response body.
	var stored string
	if err := database.DB.QueryRow(t.Context(),
		`SELECT name FROM subnets WHERE id = $1`, subnetID).Scan(&stored); err != nil {
		t.Fatalf("read back subnet %s: %v", subnetID, err)
	}
	if stored != "renamed-by-owner" {
		t.Fatalf("stored name = %q, want %q — the update answered 200 and changed nothing", stored, "renamed-by-owner")
	}

	// tenant_id names the owner, not the caller.
	var got struct {
		Subnet struct {
			TenantID string `json:"tenant_id"`
		} `json:"subnet"`
	}
	if err := json.Unmarshal([]byte(resp), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", resp, err)
	}
	if got.Subnet.TenantID != tenantB {
		t.Fatalf("tenant_id = %q, want the subnet's owner %q — not the caller", got.Subnet.TenantID, tenantB)
	}

	// A third party still cannot rename it.
	stranger := createTestProject(t, "spec022-rename-stranger")
	if code, resp = doJSONReq(r, http.MethodPut, "/v2.0/subnets/"+subnetID,
		`{"subnet":{"name":"hijacked"}}`, hdr(stranger, "member")); code != http.StatusNotFound {
		t.Fatalf("stranger PUT /v2.0/subnets/%s: code=%d body=%s; want 404", subnetID, code, resp)
	}
	if err := database.DB.QueryRow(t.Context(),
		`SELECT name FROM subnets WHERE id = $1`, subnetID).Scan(&stored); err != nil {
		t.Fatalf("read back subnet %s: %v", subnetID, err)
	}
	if stored != "renamed-by-owner" {
		t.Fatalf("a refused rename still changed the name to %q", stored)
	}
}

// TestForeignSubnetReportsItsOwnTenantID — found by the host pass, not by any unit test
// here, which is the reason it exists.
//
// GetSubnet and ListSubnets never selected s.project_id; they filled tenant_id from
// c.GetString("project_id"), the caller. That was true-by-construction while both
// handlers could only ever return the caller's own rows. It stopped being true twice
// over: SPEC-020 gave ListSubnets the `n.project_id = $1` disjunct, and FR-013a gave
// GetSubnet the same — so both can now return a subnet the caller does not own, and both
// were telling the caller it was theirs.
//
// The rename test above missed it because it asserts on the PUT response, and
// UpdateSubnet had already been fixed. The runbook line that caught it reads the value
// back with a GET. Assert every read path here.
func TestForeignSubnetReportsItsOwnTenantID(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	ensureTenantB(t)

	// Tenant A's network, subnet owned by tenant B — what an admin create leaves.
	netA := makeNetwork(t, tenantA, false)
	subnetID := insertSubnetOwnedBy(t, netA, tenantB, "spec022-attrib", "10.97.0.0/24")

	tenantIDFrom := func(t *testing.T, body string) string {
		t.Helper()
		var got struct {
			Subnet struct {
				TenantID string `json:"tenant_id"`
			} `json:"subnet"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
		return got.Subnet.TenantID
	}

	// GET by id, as tenant A — the network owner, not the subnet's.
	code, resp := doReq(r, http.MethodGet, "/v2.0/subnets/"+subnetID, hdr(tenantA, "member"))
	if code != http.StatusOK {
		t.Fatalf("GET /v2.0/subnets/%s as the network owner: code=%d body=%s; want 200", subnetID, code, resp)
	}
	if got := tenantIDFrom(t, resp); got != tenantB {
		t.Fatalf("GET tenant_id = %q, want the subnet's owner %q — not the caller %q", got, tenantB, tenantA)
	}

	// The same subnet in tenant A's listing.
	code, listResp := doReq(r, http.MethodGet, "/v2.0/subnets", hdr(tenantA, "member"))
	if code != http.StatusOK {
		t.Fatalf("GET /v2.0/subnets as the network owner: code=%d body=%s; want 200", code, listResp)
	}
	var list struct {
		Subnets []struct {
			ID       string `json:"id"`
			TenantID string `json:"tenant_id"`
		} `json:"subnets"`
	}
	if err := json.Unmarshal([]byte(listResp), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	var seen bool
	for _, s := range list.Subnets {
		if s.ID != subnetID {
			continue
		}
		seen = true
		if s.TenantID != tenantB {
			t.Fatalf("LIST tenant_id = %q, want the subnet's owner %q — not the caller %q", s.TenantID, tenantB, tenantA)
		}
	}
	if !seen {
		t.Fatalf("the network owner's listing does not contain the foreign subnet %s: %s", subnetID, listResp)
	}

	// And a caller who genuinely owns the subnet still sees its own id — the fix must
	// not have swapped one wrong answer for another.
	code, ownResp := doReq(r, http.MethodGet, "/v2.0/subnets/"+subnetID, hdr(tenantB, "member"))
	if code != http.StatusOK {
		t.Fatalf("GET as the subnet's own project: code=%d body=%s; want 200", code, ownResp)
	}
	if got := tenantIDFrom(t, ownResp); got != tenantB {
		t.Fatalf("owner's own GET tenant_id = %q, want %q", got, tenantB)
	}
}

// TestSharedNetworkGrantsReadNotDelete — FR-013a's other half. Upstream puts
// `rule:shared` in get_subnet and deliberately not in delete_subnet, so sharing a
// network lets other tenants read its subnets and never remove them. Without this the
// obvious "make the two queries match" refactor would quietly hand every tenant a
// delete on every shared network's subnets.
func TestSharedNetworkGrantsReadNotDelete(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	ensureTenantB(t)

	sharedNet := makeNetwork(t, tenantA, true)
	subnetID := insertSubnetOwnedBy(t, sharedNet, tenantA, "spec022-shared-sub", "10.95.0.0/24")

	if code, resp := doReq(r, http.MethodGet, "/v2.0/subnets/"+subnetID, hdr(tenantB, "member")); code != http.StatusOK {
		t.Fatalf("read on a shared network: code=%d body=%s; want 200", code, resp)
	}
	if code, resp := doJSONReq(r, http.MethodDelete, "/v2.0/subnets/"+subnetID, "", hdr(tenantB, "member")); code != http.StatusNotFound {
		t.Fatalf("delete on a shared network: code=%d body=%s; want 404 — sharing grants read, not delete", code, resp)
	}
	if n := subnetCountOn(t, sharedNet); n != 1 {
		t.Fatalf("a shared-network delete succeeded: %d subnet(s) left", n)
	}
}
