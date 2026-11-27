package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T097 (FR-027, FR-031, FR-032, FR-054; research R36): a floating IP's address is
// chosen inside its create's transaction, under the allocation lock and the subnets' locks, from the
// pools of the network's IPv4 subnets, lowest first. Creates sent together all succeed with distinct
// addresses; a network with no IPv4 subnet, and pools that are taken, answer 400; subnet_id and
// floating_ip_address are honoured, a taken one answering 409.

// fipNetwork is an external network with one subnet whose pools are first–last.
func fipNetwork(t *testing.T, cidr string, first, last string) (network, subnet string) {
	t.Helper()
	network = seedExternalNetwork(t, "spec033-fipnet-"+uuid.NewString()[:6], seededProjectID, false, true)
	subnet = seedSubnet(t, network, "spec033-fipnet", cidr, nil, 4, false)
	storeTestPools(t, subnet, [2]string{first, last})
	return network, subnet
}

func createFloatingIP(r *gin.Engine, hdr map[string]string, body string) (int, string, string) {
	code, resp := doJSONReq(r, http.MethodPost, "/v2.0/floatingips", body, hdr)
	var out struct {
		FloatingIP struct {
			ID      string `json:"id"`
			Address string `json:"floating_ip_address"`
		} `json:"floatingip"`
	}
	_ = json.Unmarshal([]byte(resp), &out)
	if out.FloatingIP.ID != "" {
		return code, resp, out.FloatingIP.Address
	}
	return code, resp, ""
}

func cleanupFloatingIPsOn(t *testing.T, network string) {
	t.Helper()
	t.Cleanup(func() { mustExec(t, "DELETE FROM floating_ips WHERE floating_network_id = $1", network) })
}

func TestFloatingIPAllocation(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	t.Run("ten at once: ten addresses, distinct, inside the pools", func(t *testing.T) {
		prefix := fmt.Sprintf("198.51.%d", rand.IntN(250))
		network, _ := fipNetwork(t, prefix+".0/24", prefix+".40", prefix+".59")
		cleanupFloatingIPsOn(t, network)
		var mu sync.Mutex
		got := map[string]int{}
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, body, address := createFloatingIP(r, hdr, `{"floatingip":{"floating_network_id":"`+network+`"}}`)
				mu.Lock()
				defer mu.Unlock()
				if code != http.StatusCreated {
					t.Errorf("create: code=%d body=%s; want 201", code, body)
					return
				}
				got[address]++
			}()
		}
		wg.Wait()
		if len(got) != 10 {
			t.Errorf("%d distinct addresses: %v; want 10", len(got), got)
		}
		for address, n := range got {
			if n > 1 || !inPool(prefix, address, 40, 59) {
				t.Errorf("%s handed out %d times; want once, inside .40–.59", address, n)
			}
		}
	})

	t.Run("two networks with overlapping subnets, at once", func(t *testing.T) {
		prefix := fmt.Sprintf("198.51.%d", rand.IntN(250))
		a, _ := fipNetwork(t, prefix+".0/24", prefix+".10", prefix+".20")
		b, _ := fipNetwork(t, prefix+".0/24", prefix+".10", prefix+".20")
		cleanupFloatingIPsOn(t, a)
		cleanupFloatingIPsOn(t, b)
		var wg sync.WaitGroup
		for i := range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				network := a
				if i%2 == 1 {
					network = b
				}
				if code, body, _ := createFloatingIP(r, hdr, `{"floatingip":{"floating_network_id":"`+network+`"}}`); code != http.StatusCreated {
					t.Errorf("create on %s: code=%d body=%s; want 201", network, code, body)
				}
			}()
		}
		wg.Wait()
	})

	t.Run("the lowest free, then exhaustion", func(t *testing.T) {
		prefix := fmt.Sprintf("198.51.%d", rand.IntN(250))
		network, _ := fipNetwork(t, prefix+".0/24", prefix+".7", prefix+".8")
		cleanupFloatingIPsOn(t, network)
		for _, want := range []string{prefix + ".7", prefix + ".8"} {
			if code, body, address := createFloatingIP(r, hdr, `{"floatingip":{"floating_network_id":"`+network+`"}}`); code != http.StatusCreated || address != want {
				t.Errorf("create: code=%d address=%s body=%s; want %s", code, address, body, want)
			}
		}
		code, body, _ := createFloatingIP(r, hdr, `{"floatingip":{"floating_network_id":"`+network+`"}}`)
		if code != http.StatusBadRequest || neutronErrorType(t, body) != "ExternalIpAddressExhausted" {
			t.Errorf("create on taken pools: code=%d body=%s; want 400 ExternalIpAddressExhausted", code, body)
		}
	})

	t.Run("a network without an IPv4 subnet", func(t *testing.T) {
		network := seedExternalNetwork(t, "spec033-fip-nosubnet-"+uuid.NewString()[:6], seededProjectID, false, true)
		code, body, _ := createFloatingIP(r, hdr, `{"floatingip":{"floating_network_id":"`+network+`"}}`)
		if code != http.StatusBadRequest {
			t.Errorf("create: code=%d body=%s; want 400 — no 192.0.2.x any more", code, body)
		}
		var n int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM floating_ips WHERE floating_network_id = $1", network).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d floating IP rows (%v); want none", n, err)
		}
	})

	t.Run("subnet_id and floating_ip_address are honoured", func(t *testing.T) {
		prefix := fmt.Sprintf("198.51.%d", rand.IntN(250))
		network, _ := fipNetwork(t, prefix+".0/25", prefix+".10", prefix+".20")
		second := seedSubnet(t, network, "spec033-fipnet-2", prefix+".128/25", nil, 4, false)
		storeTestPools(t, second, [2]string{prefix + ".200", prefix + ".210"})
		cleanupFloatingIPsOn(t, network)
		// Choosing the address is a manager's or an administrator's (SPEC-033 T183).
		manager := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member,manager"}

		if code, body, address := createFloatingIP(r, hdr, `{"floatingip":{"floating_network_id":"`+network+`","subnet_id":"`+second+`"}}`); code != http.StatusCreated || address != prefix+".200" {
			t.Errorf("create with subnet_id: code=%d address=%s body=%s; want %s.200", code, address, body, prefix)
		}
		if code, body, address := createFloatingIP(r, manager, `{"floatingip":{"floating_network_id":"`+network+`","floating_ip_address":"`+prefix+`.99"}}`); code != http.StatusCreated || address != prefix+".99" {
			t.Errorf("create with floating_ip_address outside the pools: code=%d address=%s body=%s; want %s.99", code, address, body, prefix)
		}
		code, body, _ := createFloatingIP(r, manager, `{"floatingip":{"floating_network_id":"`+network+`","floating_ip_address":"`+prefix+`.99"}}`)
		if code != http.StatusConflict || neutronErrorType(t, body) != "IpAddressAlreadyAllocated" {
			t.Errorf("a taken floating_ip_address: code=%d body=%s; want 409 IpAddressAlreadyAllocated", code, body)
		}
		code, body, _ = createFloatingIP(r, manager, `{"floatingip":{"floating_network_id":"`+network+`","floating_ip_address":"203.0.113.250"}}`)
		if code != http.StatusBadRequest || neutronErrorType(t, body) != "InvalidIpForNetwork" {
			t.Errorf("a floating_ip_address on no subnet of the network: code=%d body=%s; want 400 InvalidIpForNetwork", code, body)
		}
	})
}

// Every owner of an address on one external subnet takes the subnet's lock, so a floating IP, a
// router's gateway and a port allocated at once never share one (FR-032). Moved here from T087: the
// first two allocate through the one allocator only once T096 and T100 are in.
func TestMixedOwnersOnOneExternalSubnet(t *testing.T) {
	requireDB(t)
	dbtest.RequirePostgres(t) // SQLite's writers are serialised anyway
	r := newNeutronTestRouter()
	member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	prefix := fmt.Sprintf("198.51.%d", rand.IntN(250))
	network, _ := fipNetwork(t, prefix+".0/24", prefix+".30", prefix+".39")
	cleanupFloatingIPsOn(t, network)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	for i := range 9 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var code int
			var body string
			switch i % 3 {
			case 0:
				code, body, _ = createFloatingIP(r, member, `{"floatingip":{"floating_network_id":"`+network+`"}}`)
			case 1:
				code, body = doJSONReq(r, http.MethodPost, "/v2.0/routers",
					`{"router":{"name":"spec033-mixed","external_gateway_info":{"network_id":"`+network+`"}}}`, member)
			case 2:
				code, body = doJSONReq(r, http.MethodPost, "/v2.0/ports", `{"port":{"network_id":"`+network+`"}}`, member)
			}
			if code != http.StatusCreated {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%d %s", code, body))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	t.Cleanup(func() {
		mustExec(t, "DELETE FROM routers WHERE id IN (SELECT device_id FROM ports WHERE network_id = $1)", network)
	})
	if len(failures) > 0 {
		t.Fatalf("allocations failed: %v", failures)
	}
	held := map[string]int{}
	rows, err := testPool.Query(context.Background(), `SELECT floating_ip_address FROM floating_ips WHERE floating_network_id = $1`, network)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		held[a]++
	}
	rows.Close()
	ports, err := testPool.Query(context.Background(), `SELECT fixed_ips FROM ports WHERE network_id = $1`, network)
	if err != nil {
		t.Fatal(err)
	}
	for ports.Next() {
		var fixed []struct {
			IPAddress string `json:"ip_address"`
		}
		var raw []byte
		_ = ports.Scan(&raw)
		_ = json.Unmarshal(raw, &fixed)
		for _, f := range fixed {
			held[f.IPAddress]++
		}
	}
	ports.Close()
	if len(held) != 9 {
		t.Errorf("%d distinct addresses among nine owners: %v; want 9", len(held), held)
	}
	for a, n := range held {
		if n > 1 {
			t.Errorf("%s held %d times", a, n)
		}
	}
}
