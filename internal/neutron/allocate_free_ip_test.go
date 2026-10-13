package neutron

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

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
	// Hold every candidate except .13 (offset 10..13 in a /28: 10,11,12,13).
	for _, ip := range []string{"192.168.250.10", "192.168.250.11", "192.168.250.12"} {
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
	for _, cidr := range []string{"192.168.253.0/30", "192.168.253.0/29", "192.168.253.0/28"} {
		if ip := allocateIP(cidr); ip == "" {
			t.Errorf("allocateIP(%s) = \"\"; want a usable address", cidr)
		}
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
