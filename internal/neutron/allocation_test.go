package neutron

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T087 (FR-031, FR-032, FR-053; research R30): one allocator for every automatic
// address on a subnet. It hands out only pool addresses that are not the gateway, a port's fixed IP or
// a floating IP of the network, under the subnet's lock. A requested address need only be inside the
// subnet, not its network or broadcast address, and free.

// storeTestPools gives a subnet stored pools.
func storeTestPools(t *testing.T, subnetID string, ranges ...[2]string) {
	t.Helper()
	mustExec(t, "UPDATE subnets SET pools_stored = $1 WHERE id = $2", true, subnetID)
	mustExec(t, "DELETE FROM subnet_allocation_pools WHERE subnet_id = $1", subnetID)
	for _, r := range ranges {
		mustExec(t, "INSERT INTO subnet_allocation_pools (subnet_id, first_ip, last_ip) VALUES ($1, $2, $3)", subnetID, r[0], r[1])
	}
}

func seedFloatingAddress(t *testing.T, networkID, address string) {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, status)
		VALUES ($1, $2, $3, $4, 'DOWN')`, id, seededProjectID, networkID, address)
	t.Cleanup(func() { mustExec(t, "DELETE FROM floating_ips WHERE id = $1", id) })
}

// allocateUnderLock allocates one address with the subnet locked, as every caller does.
func allocateUnderLock(t *testing.T, subnetID string, policy allocationPolicy) (string, error) {
	t.Helper()
	var ip string
	err := database.WithTx(context.Background(), testPool, func(tx database.Tx) error {
		if _, err := LockSubnet(context.Background(), tx, subnetID); err != nil {
			return err
		}
		var err error
		ip, err = allocateAddress(context.Background(), tx, subnetID, policy)
		return err
	})
	return ip, err
}

func TestAllocatorStaysInsideThePools(t *testing.T) {
	requireDB(t)
	for i, policy := range []allocationPolicy{pickRandom, pickLowest} {
		prefix := []string{"10.87.1", "10.87.11"}[i]
		networkID, subnetID := seedN4Subnet(t, prefix+".0/28", prefix+".1")
		storeTestPools(t, subnetID, [2]string{prefix + ".4", prefix + ".8"})
		seedN4Port(t, networkID, subnetID, prefix+".5")
		seedFloatingAddress(t, networkID, prefix+".6")

		var free []string
		for range 3 {
			ip, err := allocateUnderLock(t, subnetID, policy)
			if err != nil {
				t.Fatalf("policy %v: allocation %d: %v", policy, len(free)+1, err)
			}
			free = append(free, ip)
			seedN4Port(t, networkID, subnetID, ip)
		}
		sort.Strings(free)
		if want := []string{prefix + ".4", prefix + ".7", prefix + ".8"}; !equalIDs(free, want) {
			t.Errorf("policy %v allocated %v; want the pool's free addresses %v (not the port's .5, the floating IP's .6, nor outside the pool)",
				policy, free, want)
		}
		if _, err := allocateUnderLock(t, subnetID, policy); !errors.Is(err, ErrNoAvailableIP) {
			t.Errorf("policy %v on an exhausted pool: %v; want ErrNoAvailableIP", policy, err)
		}
	}
}

func TestAllocatorLowestFirst(t *testing.T) {
	requireDB(t)
	networkID, subnetID := seedN4Subnet(t, "10.87.2.0/24", "10.87.2.1")
	storeTestPools(t, subnetID, [2]string{"10.87.2.50", "10.87.2.60"}, [2]string{"10.87.2.10", "10.87.2.20"})
	seedN4Port(t, networkID, subnetID, "10.87.2.10")
	ip, err := allocateUnderLock(t, subnetID, pickLowest)
	if err != nil || ip != "10.87.2.11" {
		t.Errorf("lowest free = %q, %v; want 10.87.2.11", ip, err)
	}
}

func TestAllocatorDefaultPoolSkipsTheGateway(t *testing.T) {
	requireDB(t)
	networkID, subnetID := seedN4Subnet(t, "10.87.3.0/29", "10.87.3.3")
	var got []string
	for {
		ip, err := allocateUnderLock(t, subnetID, pickRandom)
		if errors.Is(err, ErrNoAvailableIP) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ip)
		seedN4Port(t, networkID, subnetID, ip)
		if len(got) > 8 {
			t.Fatal("the allocator never ran out")
		}
	}
	sort.Strings(got)
	if want := []string{"10.87.3.1", "10.87.3.2", "10.87.3.4", "10.87.3.5", "10.87.3.6"}; !equalIDs(got, want) {
		t.Errorf("a subnet from before 084 allocated %v; want its default pool %v, the gateway left out", got, want)
	}
}

func TestRequestedAddresses(t *testing.T) {
	requireDB(t)
	networkID, subnetID := seedN4Subnet(t, "10.87.4.0/28", "10.87.4.1")
	storeTestPools(t, subnetID, [2]string{"10.87.4.4", "10.87.4.8"})
	seedN4Port(t, networkID, subnetID, "10.87.4.5")
	seedFloatingAddress(t, networkID, "10.87.4.6")

	for _, tc := range []struct {
		ip        string
		available bool
		invalid   bool
	}{
		{"10.87.4.12", true, false}, // outside the pools, inside the subnet
		{"10.87.4.4", true, false},
		{"10.87.4.5", false, false}, // a port's
		{"10.87.4.6", false, false}, // a floating IP's
		{"10.87.4.1", false, false}, // the gateway
		{"10.87.4.0", false, true},  // the network address
		{"10.87.4.15", false, true}, // the broadcast address
		{"10.99.0.1", false, true},  // outside the subnet
		{"fd00::1", false, true},    // another version
		{"not-an-ip", false, true},
	} {
		ok, err := IPAvailable(context.Background(), testPool, subnetID, tc.ip)
		var invalid *InvalidIPForSubnetError
		switch {
		case tc.invalid && !errors.As(err, &invalid):
			t.Errorf("IPAvailable(%s) = %v, %v; want InvalidIpForSubnet", tc.ip, ok, err)
		case !tc.invalid && (err != nil || ok != tc.available):
			t.Errorf("IPAvailable(%s) = %v, %v; want %v", tc.ip, ok, err, tc.available)
		}
	}
}

// Concurrent allocations on one subnet never share an address (lock).
func TestConcurrentPortAllocationsAreDistinct(t *testing.T) {
	requireDB(t)
	networkID, _ := seedN4Subnet(t, "10.87.5.0/24", "10.87.5.1")
	svc := NewService(testPool, "stub", nil)
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := svc.AllocatePortForInstance(context.Background(), networkID, seededProjectID, uuid.NewString(), nil, "", false)
			if err != nil {
				t.Errorf("allocate: %v", err)
				return
			}
			mu.Lock()
			got[info.(*PortInfo).IPAddress]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	for ip, n := range got {
		if n > 1 {
			t.Errorf("%s handed out %d times", ip, n)
		}
	}
	if len(got) != 10 {
		t.Errorf("%d distinct addresses; want 10", len(got))
	}
}
