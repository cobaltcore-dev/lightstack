package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/google/uuid"
)

// SPEC-024 (cloud-storage#546) US4 / B6 — the subnet listing answers the question asked.
//
// Two defects in one handler. It applied NO client filter at all, so `openstack subnet list --network
// X` returned every visible subnet; and it dropped any row whose Scan failed with a bare `continue`,
// so a resource that exists and is authorized simply did not appear. A wrong list is worse than an
// error because it is acted on, and the silent-drop half is the more dangerous of the two.

func subnetsFromBody(t *testing.T, body string) []map[string]any {
	t.Helper()
	var resp struct {
		Subnets []map[string]any `json:"subnets"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal subnets: %v (body=%s)", err, body)
	}
	return resp.Subnets
}

func subnetNames(subnets []map[string]any) []string {
	out := make([]string, 0, len(subnets))
	for _, s := range subnets {
		name, _ := s["name"].(string)
		out = append(out, name)
	}
	return out
}

func hasSubnet(subnets []map[string]any, name string) bool {
	for _, n := range subnetNames(subnets) {
		if n == name {
			return true
		}
	}
	return false
}

// seedSubnet inserts a subnet directly so the fixture is independent of CreateSubnet's own behaviour.
// gateway == nil stores SQL NULL, which is the state the read path used to drop.
func seedSubnet(t *testing.T, networkID, name, cidr string, gateway *string, ipVersion int, dhcp bool) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.New().String()

	_, err := database.DB.Exec(ctx, `
		INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now())
	`, id, name, networkID, seededProjectID, cidr, gateway, ipVersion, dhcp)
	if err != nil {
		t.Fatalf("seed subnet %s: %v", name, err)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, "DELETE FROM subnets WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
	})
	return id
}

// TestSubnetListFiltersByNetwork — contracts S1 and S2, the headline defect.
func TestSubnetListFiltersByNetwork(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}

	netA := seedNetwork(t, "spec024-b6-a")
	netB := seedNetwork(t, "spec024-b6-b")
	gw := "10.60.1.1"
	seedSubnet(t, netA, "b6-sub-a", "10.60.1.0/24", &gw, 4, true)
	gwB := "10.60.2.1"
	seedSubnet(t, netB, "b6-sub-b", "10.60.2.0/24", &gwB, 4, true)

	code, body := doReq(r, http.MethodGet, "/v2.0/subnets?network_id="+netA, hdr)
	if code != http.StatusOK {
		t.Fatalf("list subnets: code=%d body=%s; want 200", code, body)
	}
	got := subnetsFromBody(t, body)

	if !hasSubnet(got, "b6-sub-a") {
		t.Errorf("filtered listing lost the matching subnet; got %v", subnetNames(got))
	}
	if hasSubnet(got, "b6-sub-b") {
		t.Errorf("filtered listing returned another network's subnet — the network_id filter was ignored (B6); got %v", subnetNames(got))
	}
}

// TestSubnetListHonoursEveryContractedFilter — contract S3, the remaining six.
func TestSubnetListHonoursEveryContractedFilter(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}

	netID := seedNetwork(t, "spec024-b6-filters")
	gw := "10.61.1.1"
	wantID := seedSubnet(t, netID, "b6-want", "10.61.1.0/24", &gw, 4, true)
	gwOther := "10.61.2.1"
	seedSubnet(t, netID, "b6-other", "10.61.2.0/24", &gwOther, 4, false)

	for _, tc := range []struct{ name, query, want, exclude string }{
		{"id", "id=" + wantID, "b6-want", "b6-other"},
		{"name", "name=b6-want", "b6-want", "b6-other"},
		{"cidr", "cidr=10.61.1.0/24", "b6-want", "b6-other"},
		{"enable_dhcp", "network_id=" + netID + "&enable_dhcp=false", "b6-other", "b6-want"},
		{"ip_version", "network_id=" + netID + "&ip_version=4", "b6-want", ""},
		{"project_id", "network_id=" + netID + "&project_id=" + seededProjectID, "b6-want", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, http.MethodGet, "/v2.0/subnets?"+tc.query, hdr)
			if code != http.StatusOK {
				t.Fatalf("list: code=%d body=%s; want 200", code, body)
			}
			got := subnetsFromBody(t, body)
			if !hasSubnet(got, tc.want) {
				t.Errorf("filter %q lost %q; got %v", tc.query, tc.want, subnetNames(got))
			}
			if tc.exclude != "" && hasSubnet(got, tc.exclude) {
				t.Errorf("filter %q did not exclude %q; got %v", tc.query, tc.exclude, subnetNames(got))
			}
		})
	}
}

// TestUnknownSubnetFilterDoesNotNarrow — contract S4 / FR-020a.
//
// An unrecognized parameter must leave the result untouched. Quietly returning fewer rows than asked
// for is the defect class this whole feature exists to remove, so an unknown filter must not become an
// accidental predicate.
func TestUnknownSubnetFilterDoesNotNarrow(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}

	netID := seedNetwork(t, "spec024-b6-unknown")
	gw := "10.62.1.1"
	seedSubnet(t, netID, "b6-unknown", "10.62.1.0/24", &gw, 4, true)

	_, plain := doReq(r, http.MethodGet, "/v2.0/subnets?network_id="+netID, hdr)
	_, bogus := doReq(r, http.MethodGet, "/v2.0/subnets?network_id="+netID+"&bogus_filter=x", hdr)

	if len(subnetsFromBody(t, plain)) != len(subnetsFromBody(t, bogus)) {
		t.Errorf("an unknown parameter changed the result: %d vs %d rows",
			len(subnetsFromBody(t, plain)), len(subnetsFromBody(t, bogus)))
	}
}

// TestSubnetListPagesWithoutRepeatingOrLosingRows — contracts S6 and S7.
//
// `ORDER BY created_at DESC` alone is not a total order. Rows sharing a created_at make the page
// boundary unstable, which is the same defect #549 found in the compute listings, where it returned
// some rows two to four times. ListNetworks was already fixed to (created_at, id).
func TestSubnetListPagesWithoutRepeatingOrLosingRows(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}

	netID := seedNetwork(t, "spec024-b6-paging")

	// Five subnets sharing one created_at — the tie that breaks a non-total order.
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("b6-page-%d", i)
		id := uuid.New().String()
		if _, err := database.DB.Exec(ctx, `
			INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, NULL, 4, true, '2026-01-01 00:00:00', '2026-01-01 00:00:00')
		`, id, name, netID, seededProjectID, fmt.Sprintf("10.70.%d.0/24", i)); err != nil {
			t.Fatalf("seed paged subnet: %v", err)
		}
		want[name] = true
		t.Cleanup(func() {
			database.DB.Exec(ctx, "DELETE FROM subnets WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
		})
	}

	seen := map[string]int{}
	marker := ""
	for page := 0; page < 10; page++ {
		q := "/v2.0/subnets?network_id=" + netID + "&limit=2"
		if marker != "" {
			q += "&marker=" + marker
		}
		code, body := doReq(r, http.MethodGet, q, hdr)
		if code != http.StatusOK {
			t.Fatalf("page %d: code=%d body=%s", page, code, body)
		}
		got := subnetsFromBody(t, body)
		if len(got) == 0 {
			break
		}
		for _, s := range got {
			name, _ := s["name"].(string)
			seen[name]++
		}
		marker, _ = got[len(got)-1]["id"].(string)
	}

	for name := range want {
		switch seen[name] {
		case 0:
			t.Errorf("subnet %q never appeared while paging — a row was lost", name)
		case 1:
			// correct
		default:
			t.Errorf("subnet %q appeared %d times while paging — the marker is not a total order", name, seen[name])
		}
	}
}

// TestUnreadableSubnetRowIsLoggedNotDropped — contracts S15 and S16 / FR-024.
//
// A NULL gateway_ip is the reachable instance of "a row the reader cannot handle": the old code scanned
// it into a plain string, the Scan failed, and the bare `continue` removed the subnet from the listing
// with no log and no error. Failing the whole request instead would blank every caller's subnet list
// including the dashboard's, so the rule is: log it, keep serving, and add no non-upstream field.
func TestUnreadableSubnetRowIsLoggedNotDropped(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}

	netID := seedNetwork(t, "spec024-b6-nullgw")
	seedSubnet(t, netID, "b6-nogw", "10.63.1.0/24", nil, 4, true) // NULL gateway
	gw := "10.63.2.1"
	seedSubnet(t, netID, "b6-withgw", "10.63.2.0/24", &gw, 4, true)

	code, body := doReq(r, http.MethodGet, "/v2.0/subnets?network_id="+netID, hdr)
	if code != http.StatusOK {
		t.Fatalf("list subnets: code=%d body=%s; want 200 — one bad row must not fail the listing", code, body)
	}
	got := subnetsFromBody(t, body)

	if !hasSubnet(got, "b6-nogw") {
		t.Errorf("the gatewayless subnet vanished from the listing (B6); got %v", subnetNames(got))
	}
	if !hasSubnet(got, "b6-withgw") {
		t.Errorf("a sibling subnet was lost too; got %v", subnetNames(got))
	}

	// No non-upstream signalling field (FR-024a).
	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if len(envelope) != 1 {
		t.Errorf("the listing response gained a non-upstream key: %v", envelope)
	}
}
