package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Two tenants for the entitlement matrix. The seeded project stands in for tenant A
// so the fixtures reference a project that really exists (subnets.project_id and
// networks.project_id are both foreign keys).
const (
	tenantA = seededProjectID
	tenantB = "00000000-0000-0000-0000-0000000b0b0b"
)

// ensureTenantB creates the second project once; the seeded one is tenant A.
func ensureTenantB(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(t.Context(),
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
	err := testPool.QueryRow(t.Context(),
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
		if _, err := testPool.Exec(ctx, `DELETE FROM subnets WHERE network_id = $1`, id); err != nil {
			t.Logf("cleanup subnets of %s: %v", id, err)
		}
		if _, err := testPool.Exec(ctx, `DELETE FROM networks WHERE id = $1`, id); err != nil {
			t.Logf("cleanup network %s: %v", id, err)
		}
	})
	return id
}

func hdr(project string, roles ...string) map[string]string {
	h := map[string]string{"X-Test-Project": project}
	if len(roles) > 0 {
		h["X-Test-Roles"] = strings.Join(roles, ",")
	}
	return h
}

// insertSubnetOwnedBy attaches a subnet owned by `owner` to `networkID`, the way an
// administrator's create does: CreateSubnet attributes the row to the *caller's*
// project, matching upstream's populate_project_id, so the subnet and the network it
// sits on can legitimately belong to different projects.
func insertSubnetOwnedBy(t *testing.T, networkID, owner, name, cidr string) string {
	t.Helper()
	var id string
	err := testPool.QueryRow(t.Context(),
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

// TestForeignSubnetReportsItsOwnTenantID — GetSubnet and ListSubnets never selected
// s.project_id; they filled tenant_id from c.GetString("project_id"), the caller. That was
// true-by-construction only while both handlers returned the caller's own rows, and they do
// not: both match subnets on a shared network, and SPEC-020 added subnets on the caller's
// own network. Moved here from SPEC-022 (cloud-storage#545) on PR #79 review. Every read
// path is asserted: a PUT response alone would not have caught it.
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
