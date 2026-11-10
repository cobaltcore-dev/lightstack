package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"

	"github.com/google/uuid"
)

func TestAllocateFreeIPFindsLastAddress(t *testing.T) {
	requireDB(t)
	networkID, subnetID := seedN4Subnet(t, "192.168.249.0/24", "192.168.249.1")
	for i := 2; i < 254; i++ {
		seedN4Port(t, networkID, subnetID, fmt.Sprintf("192.168.249.%d", i))
	}
	for range 10 {
		ip, err := AllocateFreeIP(t.Context(), testPool, subnetID, "192.168.249.0/24")
		if err != nil || ip != "192.168.249.254" {
			t.Fatalf("last address: ip=%q err=%v; want 192.168.249.254", ip, err)
		}
	}
}

func TestIPAvailableCanonicalIPv6(t *testing.T) {
	requireDB(t)
	networkID, subnetID := seedN4Subnet(t, "2001:db8::/64", "2001:0db8:0:0:0:0:0:1")
	seedN4Port(t, networkID, subnetID, "2001:0db8:0:0:0:0:0:2")
	for _, ip := range []string{"2001:db8::1", "2001:db8::2", "2001:DB8::2"} {
		available, err := IPAvailable(t.Context(), testPool, subnetID, ip)
		if err != nil || available {
			t.Errorf("IPAvailable(%s)=%v, %v; want occupied", ip, available, err)
		}
	}
}

// seedN4Subnet inserts a network + subnet and returns their ids.
func seedN4Subnet(t *testing.T, cidr, gateway string) (networkID, subnetID string) {
	t.Helper()
	ctx := context.Background()
	if err := testPool.QueryRow(ctx,
		`INSERT INTO networks (name, project_id, status, admin_state_up)
		 VALUES ($1, $2, 'ACTIVE', true) RETURNING id::text`,
		"n4-net-"+uuid.NewString()[:8], seededProjectID).Scan(&networkID); err != nil {
		t.Fatalf("seeding network: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id::text = $1`, networkID)
	})
	if err := testPool.QueryRow(ctx,
		`INSERT INTO subnets (name, network_id, project_id, cidr, gateway_ip)
		 VALUES ('n4-sub', $1, $2, $3, NULLIF($4, '')) RETURNING id::text`,
		networkID, seededProjectID, cidr, gateway).Scan(&subnetID); err != nil {
		t.Fatalf("seeding subnet: %v", err)
	}
	return networkID, subnetID
}

// seedN4Port inserts a port holding one fixed_ip in the subnet.
func seedN4Port(t *testing.T, networkID, subnetID, ip string) {
	t.Helper()
	fixed, _ := json.Marshal([]map[string]any{
		{"subnet_id": subnetID, "ip_address": ip},
	})
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO ports (name, network_id, project_id, mac_address, status, fixed_ips)
		 VALUES ('n4-port', $1, $2, $3, 'ACTIVE', $4)`,
		networkID, seededProjectID, "52:54:00:aa:bb:cc", fixed); err != nil {
		t.Fatalf("seeding port: %v", err)
	}
}

// TestAllocateFreeIPAvoidsUsedAddresses pins review 2026-10-13 N-4: allocation
// used to be a purely random draw, so two ports in one subnet landed on the
// same address at birthday-problem rates — two MACs claiming one IP. On a /28
// with 12 of 14 candidates already held, the old code duplicated an address
// ~86% of the time; the fixed allocator must never return a held one.
func TestAllocateFreeIPAvoidsUsedAddresses(t *testing.T) {
	requireDB(t)
	_, subnetID := seedN4Subnet(t, "192.168.250.0/28", "")

	held := map[string]bool{"192.168.250.1": true, "192.168.250.14": true}
	// Hold addresses across the range the allocator may now draw from — it is
	// upstream-conformant and no longer biased to offsets 10..13.
	for _, ip := range []string{"192.168.250.1", "192.168.250.10", "192.168.250.11", "192.168.250.12", "192.168.250.14"} {
		held[ip] = true
		seedN4Port(t, mustN4NetworkID(t, subnetID), subnetID, ip)
	}

	for range 20 {
		ip, err := AllocateFreeIP(context.Background(), testPool, subnetID, "192.168.250.0/28")
		if err != nil {
			t.Fatalf("allocateFreeIP: %v", err)
		}
		if held[ip] {
			t.Fatalf("allocated %s, which another port already holds — the N-4 collision", ip)
		}
	}
}

// TestAllocateFreeIPSkipsGateway: the gateway belongs to the subnet; a port
// claiming it breaks routing for everyone in the subnet.
func TestAllocateFreeIPSkipsGateway(t *testing.T) {
	requireDB(t)
	_, subnetID := seedN4Subnet(t, "192.168.251.0/29", "192.168.251.1")

	for range 20 {
		ip, err := AllocateFreeIP(context.Background(), testPool, subnetID, "192.168.251.0/29")
		if err != nil {
			t.Fatalf("allocateFreeIP: %v", err)
		}
		if ip == "192.168.251.1" {
			t.Fatalf("allocated the subnet's own gateway address")
		}
	}
}

// TestAllocateFreeIPExhaustedSubnet: a full subnet is an error, not a silent
// duplicate or an infinite loop. A /30 has two usable addresses; holding both
// must exhaust it.
func TestAllocateFreeIPExhaustedSubnet(t *testing.T) {
	requireDB(t)
	networkID, subnetID := seedN4Subnet(t, "192.168.252.0/30", "")
	seedN4Port(t, networkID, subnetID, "192.168.252.1")
	seedN4Port(t, networkID, subnetID, "192.168.252.2")

	if _, err := AllocateFreeIP(context.Background(), testPool, subnetID, "192.168.252.0/30"); err == nil {
		t.Fatal("allocating from a subnet whose every usable address is held must fail")
	}
}

// TestAllocateIPTinySubnetDoesNotPanic pins the adjacent defect found while
// fixing N-4: the old candidate range [10, maxOffset) went negative for
// subnets with fewer than ~12 addresses, and crypto/rand.Int PANICS on a
// non-positive bound.
func TestAllocateIPTinySubnetDoesNotPanic(t *testing.T) {
	for _, cidr := range []string{
		"192.168.253.0/30",
		"192.168.253.0/29",
		"192.168.253.0/28",
		"192.168.253.0/31",
		"192.168.253.1/32",
		"fd00::/64",
		"fd00::/126",
	} {
		if ip := allocateIP(cidr); ip == "" {
			t.Errorf("allocateIP(%s) = \"\"; want a usable address", cidr)
		}
	}
}

// TestAllocateFreeIPv6Subnet: an IPv6 subnet is allocated from normally. The
// old allocator shifted by hostBits, so a /64 overflowed int64 and panicked —
// IPv6 could not be used at all.
func TestAllocateFreeIPv6Subnet(t *testing.T) {
	requireDB(t)
	_, subnetID := seedN4Subnet(t, "fd00:1::/64", "")
	for range 10 {
		ip, err := AllocateFreeIP(context.Background(), testPool, subnetID, "fd00:1::/64")
		if err != nil {
			t.Fatalf("allocateFreeIP on IPv6: %v", err)
		}
		if parsed := net.ParseIP(ip); parsed == nil {
			t.Fatalf("allocated %q, which is not an address", ip)
		}
	}
}

// TestAllocateFreeIPToleratesLegacyNullFixedIPs is the regression for the
// tenant-triggerable deployment-wide allocation DoS. A port on a subnet-less
// network used to persist JSON scalar `null` in ports.fixed_ips. Because
// jsonb_array_elements was an unguarded FROM-clause SRF, one such row made
// every allocation in every subnet fail with SQLSTATE 22023.
func TestAllocateFreeIPToleratesLegacyNullFixedIPs(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	// A network with no subnet and a LEGACY row whose fixed_ips is JSON null,
	// exactly as the old writer left it.
	var bareNetworkID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO networks (name, project_id, status, admin_state_up)
		 VALUES ($1, $2, 'ACTIVE', true) RETURNING id::text`,
		"n4-null-net-"+uuid.NewString()[:8], seededProjectID).Scan(&bareNetworkID); err != nil {
		t.Fatalf("seeding subnet-less network: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id::text = $1`, bareNetworkID)
	})
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (name, network_id, project_id, mac_address, status, fixed_ips)
		 VALUES ('n4-null-port', $1, $2, '52:54:00:aa:bb:cc', 'ACTIVE', 'null'::jsonb)`,
		bareNetworkID, seededProjectID); err != nil {
		t.Fatalf("seeding legacy null-fixed_ips port: %v", err)
	}

	// A different, healthy subnet must still allocate. Before the guard this
	// failed for every subnet in the deployment.
	_, subnetID := seedN4Subnet(t, "192.168.254.0/29", "")
	ip, err := AllocateFreeIP(ctx, testPool, subnetID, "192.168.254.0/29")
	if err != nil {
		t.Fatalf("allocation on a healthy subnet failed because another port has JSON-null fixed_ips: %v", err)
	}
	if ip == "" {
		t.Fatal("allocated an empty address")
	}
}

// TestCreatePortOnSubnetlessNetworkWritesEmptyArray: the writer side. A port on
// a network with no subnet must persist `[]`, never JSON null, so no NEW poison
// rows are created even though readers still tolerate old ones.
func TestCreatePortOnSubnetlessNetworkWritesEmptyArray(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()

	var networkID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO networks (name, project_id, status, admin_state_up)
		 VALUES ($1, $2, 'ACTIVE', true) RETURNING id::text`,
		"n4-empty-net-"+uuid.NewString()[:8], seededProjectID).Scan(&networkID); err != nil {
		t.Fatalf("seeding subnet-less network: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM ports WHERE network_id = $1`, networkID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, networkID)
	})

	body := `{"port":{"network_id":"` + networkID + `","name":"n4-empty-port"}}`
	code, resp := doReqBody(r, "POST", "/v2.0/ports", body,
		map[string]string{"X-Test-Project": seededProjectID})
	if code != 201 {
		t.Fatalf("create port on subnet-less network: code=%d body=%s; want 201", code, resp)
	}

	var fixedIPsJSON []byte
	if err := testPool.QueryRow(ctx,
		`SELECT fixed_ips FROM ports WHERE network_id = $1`, networkID).Scan(&fixedIPsJSON); err != nil {
		t.Fatalf("reading the created port's fixed_ips: %v", err)
	}
	var fixedIPs []json.RawMessage
	if err := json.Unmarshal(fixedIPsJSON, &fixedIPs); err != nil {
		t.Fatalf("decoding the created port's fixed_ips %s: %v", fixedIPsJSON, err)
	}
	if fixedIPs == nil || len(fixedIPs) != 0 {
		t.Fatalf("fixed_ips is %s; want an empty JSON array so every reader can unpack it", fixedIPsJSON)
	}
}

func mustN4NetworkID(t *testing.T, subnetID string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(),
		`SELECT network_id::text FROM subnets WHERE id::text = $1`, subnetID).Scan(&id); err != nil {
		t.Fatalf("resolving the subnet's network: %v", err)
	}
	return id
}
