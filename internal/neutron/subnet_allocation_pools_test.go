package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// SPEC-033 round 2, T086 (FR-030, FR-032a, FR-052; research R30): a subnet stores its allocation pools.
// Create and update take allocation_pools and validate them as upstream does, in upstream's order;
// every read reports them; an absent field gives upstream's default pool, the gateway taken out; []
// gives none. A subnet from before migration 084 reads as the default pool. A subnet that a port other
// than its network's DHCP port uses cannot be deleted. (Named for allocation pools: subnet_pools.go is
// the subnet-pool resource.)

type poolOut struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func poolList(pairs ...string) []poolOut {
	out := []poolOut{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, poolOut{pairs[i], pairs[i+1]})
	}
	return out
}

func equalPools(a, b []poolOut) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// poolsIn reads allocation_pools out of a subnet response body; the key must be a list.
func poolsIn(t *testing.T, body, key string) []poolOut {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var subnet map[string]json.RawMessage
	if err := json.Unmarshal(out[key], &subnet); err != nil {
		t.Fatalf("decode %s: %v", out[key], err)
	}
	var pools []poolOut
	if err := json.Unmarshal(subnet["allocation_pools"], &pools); err != nil || pools == nil {
		t.Fatalf("allocation_pools = %s (%v); want a list", subnet["allocation_pools"], err)
	}
	return pools
}

// poolRig makes subnets through the API, each on a network of its own.
type poolRig struct {
	r   *gin.Engine
	hdr map[string]string
	net int
}

func newPoolRig() *poolRig {
	return &poolRig{
		r:   newNeutronTestRouter(),
		hdr: map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"},
		net: rand.IntN(200),
	}
}

// prefix returns a fresh 10.77.N /24's first three octets.
func (p *poolRig) prefix() string {
	p.net = (p.net + 1) % 250
	return fmt.Sprintf("10.77.%d", p.net)
}

// create POSTs a subnet of prefix.0/24 with the extra JSON fields on a network of its own.
func (p *poolRig) create(t *testing.T, prefix, extra string) (int, string) {
	t.Helper()
	network := seedNetwork(t, "spec033-pools")
	body := fmt.Sprintf(`{"subnet":{"network_id":%q,"cidr":"%s.0/24","ip_version":4%s}}`, network, prefix, extra)
	return doJSONReq(p.r, http.MethodPost, "/v2.0/subnets", body, p.hdr)
}

// mustCreate creates a subnet and returns its id and network.
func (p *poolRig) mustCreate(t *testing.T, prefix, extra string) (id, network string) {
	t.Helper()
	code, body := p.create(t, prefix, extra)
	if code != http.StatusCreated {
		t.Fatalf("create subnet %s.0/24 %s: code=%d body=%s", prefix, extra, code, body)
	}
	var out struct {
		Subnet struct {
			ID        string `json:"id"`
			NetworkID string `json:"network_id"`
		} `json:"subnet"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Subnet.ID, out.Subnet.NetworkID
}

func TestSubnetAllocationPools(t *testing.T) {
	requireDB(t)
	p := newPoolRig()

	t.Run("given pools are stored and read back", func(t *testing.T) {
		pre := p.prefix()
		code, body := p.create(t, pre, fmt.Sprintf(`,"allocation_pools":[{"start":"%[1]s.10","end":"%[1]s.20"},{"start":"%[1]s.30","end":"%[1]s.40"}]`, pre))
		if code != http.StatusCreated {
			t.Fatalf("create: code=%d body=%s", code, body)
		}
		want := poolList(pre+".10", pre+".20", pre+".30", pre+".40")
		if got := poolsIn(t, body, "subnet"); !equalPools(got, want) {
			t.Errorf("create response pools %v; want %v", got, want)
		}
		id := subnetIDOf(t, body)
		code, body = doReq(p.r, http.MethodGet, "/v2.0/subnets/"+id, p.hdr)
		if got := poolsIn(t, body, "subnet"); code != http.StatusOK || !equalPools(got, want) {
			t.Errorf("show: code=%d pools %v; want %v", code, got, want)
		}
		code, body = doReq(p.r, http.MethodGet, "/v2.0/subnets?id="+id, p.hdr)
		var list struct {
			Subnets []struct {
				Pools []poolOut `json:"allocation_pools"`
			} `json:"subnets"`
		}
		if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil || len(list.Subnets) != 1 ||
			!equalPools(list.Subnets[0].Pools, want) {
			t.Errorf("list: code=%d body=%s; want one subnet with pools %v", code, body, want)
		}
	})

	t.Run("absent gives upstream's default, the gateway taken out", func(t *testing.T) {
		pre := p.prefix()
		code, body := p.create(t, pre, "")
		if got, want := poolsIn(t, body, "subnet"), poolList(pre+".2", pre+".254"); code != http.StatusCreated || !equalPools(got, want) {
			t.Errorf("default pools with the gateway at .1: code=%d %v; want %v", code, got, want)
		}
		pre = p.prefix()
		code, body = p.create(t, pre, fmt.Sprintf(`,"gateway_ip":"%s.100"`, pre))
		if got, want := poolsIn(t, body, "subnet"), poolList(pre+".1", pre+".99", pre+".101", pre+".254"); code != http.StatusCreated || !equalPools(got, want) {
			t.Errorf("default pools with the gateway at .100: code=%d %v; want %v (split)", code, got, want)
		}
		pre = p.prefix()
		code, body = p.create(t, pre, `,"gateway_ip":null`)
		if got, want := poolsIn(t, body, "subnet"), poolList(pre+".1", pre+".254"); code != http.StatusCreated || !equalPools(got, want) {
			t.Errorf("default pools without a gateway: code=%d %v; want %v", code, got, want)
		}
	})

	t.Run("[] gives none", func(t *testing.T) {
		pre := p.prefix()
		code, body := p.create(t, pre, `,"allocation_pools":[]`)
		if got := poolsIn(t, body, "subnet"); code != http.StatusCreated || len(got) != 0 {
			t.Errorf("create with []: code=%d pools %v; want none", code, got)
		}
		id := subnetIDOf(t, body)
		_, body = doReq(p.r, http.MethodGet, "/v2.0/subnets/"+id, p.hdr)
		if got := poolsIn(t, body, "subnet"); len(got) != 0 {
			t.Errorf("show of a subnet created with []: %v; want none", got)
		}
	})

	t.Run("a subnet from before migration 084 reads as the default pool", func(t *testing.T) {
		pre := p.prefix()
		network := seedNetwork(t, "spec033-pools-legacy")
		gw := pre + ".1"
		id := seedSubnet(t, network, "spec033-legacy", pre+".0/24", &gw, 4, false)
		for _, stored := range []any{nil, false} {
			mustExec(t, "UPDATE subnets SET pools_stored = $1 WHERE id = $2", stored, id)
			code, body := doReq(p.r, http.MethodGet, "/v2.0/subnets/"+id, p.hdr)
			if got, want := poolsIn(t, body, "subnet"), poolList(pre+".2", pre+".254"); code != http.StatusOK || !equalPools(got, want) {
				t.Errorf("pools_stored %v: code=%d %v; want the default %v", stored, code, got, want)
			}
		}
	})

	t.Run("refusals, in upstream's order", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			extra    func(pre string) string
			status   int
			wantType string
		}{
			{"malformed",
				func(pre string) string {
					return fmt.Sprintf(`,"allocation_pools":[{"start":"%s.300","end":"%s.20"}]`, pre, pre)
				},
				http.StatusBadRequest, "InvalidAllocationPool"},
			{"start after end",
				func(pre string) string {
					return fmt.Sprintf(`,"allocation_pools":[{"start":"%s.20","end":"%s.10"}]`, pre, pre)
				},
				http.StatusBadRequest, "InvalidAllocationPool"},
			{"another IP version",
				func(string) string { return `,"allocation_pools":[{"start":"fd00::1","end":"fd00::5"}]` },
				http.StatusBadRequest, "InvalidAllocationPool"},
			{"the network address",
				func(pre string) string {
					return fmt.Sprintf(`,"allocation_pools":[{"start":"%s.0","end":"%s.10"}]`, pre, pre)
				},
				http.StatusBadRequest, "OutOfBoundsAllocationPool"},
			{"the broadcast address",
				func(pre string) string {
					return fmt.Sprintf(`,"allocation_pools":[{"start":"%s.250","end":"%s.255"}]`, pre, pre)
				},
				http.StatusBadRequest, "OutOfBoundsAllocationPool"},
			{"outside the subnet",
				func(string) string { return `,"allocation_pools":[{"start":"10.78.0.1","end":"10.78.0.5"}]` },
				http.StatusBadRequest, "OutOfBoundsAllocationPool"},
			{"out of bounds is checked before overlap",
				func(pre string) string {
					return fmt.Sprintf(`,"allocation_pools":[{"start":"%[1]s.5","end":"%[1]s.20"},{"start":"%[1]s.0","end":"%[1]s.10"}]`, pre)
				},
				http.StatusBadRequest, "OutOfBoundsAllocationPool"},
			{"overlapping",
				func(pre string) string {
					return fmt.Sprintf(`,"allocation_pools":[{"start":"%[1]s.10","end":"%[1]s.20"},{"start":"%[1]s.15","end":"%[1]s.25"}]`, pre)
				},
				http.StatusConflict, "OverlappingAllocationPools"},
			{"overlap is checked before the gateway",
				func(pre string) string {
					return fmt.Sprintf(`,"gateway_ip":"%[1]s.12","allocation_pools":[{"start":"%[1]s.10","end":"%[1]s.20"},{"start":"%[1]s.15","end":"%[1]s.25"}]`, pre)
				},
				http.StatusConflict, "OverlappingAllocationPools"},
			{"the gateway inside a pool",
				func(pre string) string {
					return fmt.Sprintf(`,"gateway_ip":"%[1]s.15","allocation_pools":[{"start":"%[1]s.10","end":"%[1]s.20"}]`, pre)
				},
				http.StatusConflict, "GatewayConflictWithAllocationPools"},
		} {
			pre := p.prefix()
			t.Run(tc.name, func(t *testing.T) {
				code, body := p.create(t, pre, tc.extra(pre))
				if code != tc.status {
					t.Fatalf("code=%d body=%s; want %d", code, body, tc.status)
				}
				if typ := neutronErrorType(t, body); typ != tc.wantType {
					t.Errorf("type = %q; want %s", typ, tc.wantType)
				}
			})
		}
	})

	t.Run("updates are checked against the gateway", func(t *testing.T) {
		pre := p.prefix()
		id, _ := p.mustCreate(t, pre, fmt.Sprintf(`,"allocation_pools":[{"start":"%[1]s.10","end":"%[1]s.20"}]`, pre))
		code, body := doJSONReq(p.r, http.MethodPut, "/v2.0/subnets/"+id,
			fmt.Sprintf(`{"subnet":{"allocation_pools":[{"start":"%[1]s.1","end":"%[1]s.5"}]}}`, pre), p.hdr)
		if code != http.StatusConflict || neutronErrorType(t, body) != "GatewayConflictWithAllocationPools" {
			t.Errorf("pools holding the gateway: code=%d body=%s; want 409 GatewayConflictWithAllocationPools", code, body)
		}
		code, body = doJSONReq(p.r, http.MethodPut, "/v2.0/subnets/"+id, fmt.Sprintf(`{"subnet":{"gateway_ip":"%s.15"}}`, pre), p.hdr)
		if code != http.StatusConflict || neutronErrorType(t, body) != "GatewayConflictWithAllocationPools" {
			t.Errorf("a gateway inside the pools: code=%d body=%s; want 409 GatewayConflictWithAllocationPools", code, body)
		}
		_, body = doReq(p.r, http.MethodGet, "/v2.0/subnets/"+id, p.hdr)
		if got, want := poolsIn(t, body, "subnet"), poolList(pre+".10", pre+".20"); !equalPools(got, want) {
			t.Errorf("pools after two refusals: %v; want %v unchanged", got, want)
		}
		var gateway string
		if err := testPool.QueryRow(context.Background(), "SELECT gateway_ip FROM subnets WHERE id = $1", id).Scan(&gateway); err != nil || gateway != pre+".1" {
			t.Errorf("gateway after the refusal: %q (%v); want %s.1", gateway, err, pre)
		}
		if code, body := doJSONReq(p.r, http.MethodPut, "/v2.0/subnets/"+id, fmt.Sprintf(`{"subnet":{"gateway_ip":"%s.30"}}`, pre), p.hdr); code != http.StatusOK {
			t.Errorf("a gateway outside the pools: code=%d body=%s; want 200", code, body)
		}
	})

	t.Run("an update replaces the pools; ports keep their addresses; the next comes from the new pools", func(t *testing.T) {
		pre := p.prefix()
		id, network := p.mustCreate(t, pre, fmt.Sprintf(`,"allocation_pools":[{"start":"%[1]s.10","end":"%[1]s.10"}]`, pre))
		first := createPortAddress(t, p.r, p.hdr, network)
		if first != pre+".10" {
			t.Fatalf("the first port got %s; want %s.10, the pool's only address", first, pre)
		}
		code, body := doJSONReq(p.r, http.MethodPut, "/v2.0/subnets/"+id,
			fmt.Sprintf(`{"subnet":{"allocation_pools":[{"start":"%[1]s.50","end":"%[1]s.50"}]}}`, pre), p.hdr)
		if code != http.StatusOK {
			t.Fatalf("replace the pools: code=%d body=%s", code, body)
		}
		if got, want := poolsIn(t, body, "subnet"), poolList(pre+".50", pre+".50"); !equalPools(got, want) {
			t.Errorf("pools after the update: %v; want %v", got, want)
		}
		if second := createPortAddress(t, p.r, p.hdr, network); second != pre+".50" {
			t.Errorf("the next port got %s; want %s.50 from the new pool", second, pre)
		}
	})

	t.Run("a subnet a port uses cannot be deleted", func(t *testing.T) {
		pre := p.prefix()
		id, network := p.mustCreate(t, pre, "")
		createPortAddress(t, p.r, p.hdr, network)
		code, body := doReq(p.r, http.MethodDelete, "/v2.0/subnets/"+id, p.hdr)
		if code != http.StatusConflict || neutronErrorType(t, body) != "SubnetInUse" {
			t.Fatalf("delete a subnet in use: code=%d body=%s; want 409 SubnetInUse", code, body)
		}
		if code, _ := doReq(p.r, http.MethodGet, "/v2.0/subnets/"+id, p.hdr); code != http.StatusOK {
			t.Errorf("the refused delete removed the subnet (show answered %d)", code)
		}
		mustExec(t, "DELETE FROM ports WHERE network_id = $1", network)
		if code, body := doReq(p.r, http.MethodDelete, "/v2.0/subnets/"+id, p.hdr); code != http.StatusNoContent {
			t.Errorf("delete once no port uses it: code=%d body=%s; want 204", code, body)
		}
	})
}

func subnetIDOf(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		Subnet struct {
			ID string `json:"id"`
		} `json:"subnet"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Subnet.ID == "" {
		t.Fatalf("no subnet id in %s (%v)", body, err)
	}
	return out.Subnet.ID
}

// createPortAddress creates a port on network through the API and returns its first fixed IP.
func createPortAddress(t *testing.T, r *gin.Engine, hdr map[string]string, network string) string {
	t.Helper()
	code, body := doJSONReq(r, http.MethodPost, "/v2.0/ports", fmt.Sprintf(`{"port":{"network_id":%q}}`, network), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create port: code=%d body=%s", code, body)
	}
	var out struct {
		Port struct {
			FixedIPs []struct {
				IPAddress string `json:"ip_address"`
			} `json:"fixed_ips"`
		} `json:"port"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Port.FixedIPs) == 0 {
		t.Fatalf("no fixed IP in %s (%v)", body, err)
	}
	return out.Port.FixedIPs[0].IPAddress
}
