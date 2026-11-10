package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestListingsAreNotCappedByDefault: a listing without `limit` returns every visible row, as
// upstream does (pagination_max_limit defaults to -1, no limit). Networks, subnets, ports,
// routers and floating IPs were each cut at 1000 rows with no next link, so past that size a
// client saw a silently partial list — and network.subnets named ids the subnet listing no
// longer contained, the dangling references Horizon renders as null rows (FR-007). An explicit
// limit still pages. Raised on PR #79.
//
// Rows are seeded in a project of their own and counted by owner or network, so rows other
// tests (or packages running in parallel) create elsewhere cannot move the counts.
func TestListingsAreNotCappedByDefault(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	const count = 1001
	project := createTestProject(t, "spec020-page-size") // deleting it cascades every row below

	var netID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO networks (name, project_id, network_type) VALUES ('page-size-base', $1, 'vxlan') RETURNING id`,
		project).Scan(&netID); err != nil {
		t.Fatalf("create base network: %v", err)
	}
	seed := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := testPool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %d %s: %v", count, what, err)
		}
	}
	// series counts g from 1 to the placeholder's value, a row source both backends have.
	series := func(n string) string {
		return `WITH RECURSIVE seq(g) AS (SELECT 1 UNION ALL SELECT g + 1 FROM seq WHERE g < ` + n + `::int) `
	}
	seed("networks", series("$2")+`INSERT INTO networks (name, project_id, network_type)
		SELECT 'page-' || g, $1, 'vxlan' FROM seq`, project, count)
	seed("subnets", series("$3")+`INSERT INTO subnets (name, network_id, project_id, cidr, gateway_ip, ip_version,
		                                  enable_dhcp, dns_nameservers, created_at, updated_at)
		SELECT 'page-' || g, $1, $2, '10.' || (g / 256) || '.' || (g % 256) || '.0/24',
		       '10.' || (g / 256) || '.' || (g % 256) || '.1', 4, false, $4::text[], NOW(), NOW()
		FROM seq`, netID, project, count, []string{})
	seed("ports", series("$3")+`INSERT INTO ports (name, network_id, project_id, mac_address)
		SELECT 'page-' || g, $1, $2, 'fa:16:3e:00:00:01' FROM seq`,
		netID, project, count)
	seed("routers", series("$2")+`INSERT INTO routers (name, project_id)
		SELECT 'page-' || g, $1 FROM seq`, project, count)
	seed("floating IPs", series("$3")+`INSERT INTO floating_ips (project_id, floating_network_id, floating_ip_address)
		SELECT $1, $2, '198.18.' || (g / 256) || '.' || (g % 256) FROM seq`,
		project, netID, count)

	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(project, false)
	for _, tc := range []struct {
		name, path, key string
		ours            func(row map[string]any) bool
		want            int
	}{
		// The base network is the project's own too, hence count+1.
		{"networks", "/v2.0/networks", "networks", func(r map[string]any) bool { return r["tenant_id"] == project }, count + 1},
		{"subnets", "/v2.0/subnets", "subnets", func(r map[string]any) bool { return r["network_id"] == netID }, count},
		{"ports", "/v2.0/ports", "ports", func(r map[string]any) bool { return r["network_id"] == netID }, count},
		{"routers", "/v2.0/routers", "routers", func(map[string]any) bool { return true }, count},
		{"floating IPs", "/v2.0/floatingips", "floatingips", func(map[string]any) bool { return true }, count},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := func(path string) []map[string]any {
				t.Helper()
				code, body := doReq(r, http.MethodGet, path, hdr)
				if code != http.StatusOK {
					t.Fatalf("GET %s: code=%d body=%.200s", path, code, body)
				}
				var env map[string][]map[string]any
				if err := json.Unmarshal([]byte(body), &env); err != nil {
					t.Fatalf("parse %s: %v", path, err)
				}
				return env[tc.key]
			}
			got := 0
			for _, row := range rows(tc.path) {
				if tc.ours(row) {
					got++
				}
			}
			if got != tc.want {
				t.Errorf("listing without limit returned %d of the project's %d %s; want all of them", got, tc.want, tc.name)
			}
			if n := len(rows(tc.path + "?limit=5")); n != 5 {
				t.Errorf("listing with limit=5 returned %d rows; an explicit limit must still page", n)
			}
		})
	}
}
