package neutron

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
)

// A subnet's allocation pools and the one allocator every automatic address on a subnet comes from
// (SPEC-033 round 2: FR-030–FR-032a, FR-053; research R30).
//
// A subnet whose pools_stored is true has its pools in subnet_allocation_pools, none meaning none, as
// upstream's "allocation_pools": []. Any other subnet — every one from before migration 084 — has
// upstream's default pool, computed from its CIDR and gateway (FR-032a).

// ipRange is one allocation pool, both ends included.
type ipRange struct{ first, last netip.Addr }

func (r ipRange) String() string { return r.first.String() + "-" + r.last.String() }

func (r ipRange) contains(a netip.Addr) bool {
	return r.first.Compare(a) <= 0 && a.Compare(r.last) <= 0
}

func (r ipRange) overlaps(o ipRange) bool {
	return r.first.Compare(o.last) <= 0 && o.first.Compare(r.last) <= 0
}

// renderPools is the pools as upstream's API lists them; never nil, since clients iterate it.
func renderPools(pools []ipRange) []gin.H {
	out := make([]gin.H, 0, len(pools))
	for _, p := range pools {
		out = append(out, gin.H{"start": p.first.String(), "end": p.last.String()})
	}
	return out
}

// lastAddr is the highest address of the prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr()
	if a.Is4() {
		b := a.As4()
		for i := p.Bits(); i < 32; i++ {
			b[i/8] |= 1 << (7 - uint(i%8))
		}
		return netip.AddrFrom4(b)
	}
	b := a.As16()
	for i := p.Bits(); i < 128; i++ {
		b[i/8] |= 1 << (7 - uint(i%8))
	}
	return netip.AddrFrom16(b)
}

// parsePrefix reads a subnet's CIDR.
func parsePrefix(cidr string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid subnet CIDR %q: %w", cidr, err)
	}
	return p.Masked(), nil
}

// parseGateway reads a subnet's gateway: the zero Addr for none.
func parseGateway(gateway *string) netip.Addr {
	if gateway == nil {
		return netip.Addr{}
	}
	a, err := netip.ParseAddr(strings.TrimSpace(*gateway))
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// generatePools is upstream's generate_pools (neutron/ipam/utils.py): IPv4 up to /30 from the second
// to the next-to-last address, /31 and /32 whole; IPv6 from the second address to the last; the
// gateway taken out, which can split the range in two.
func generatePools(prefix netip.Prefix, gateway netip.Addr) []ipRange {
	first, last := prefix.Masked().Addr(), lastAddr(prefix)
	if first == last {
		return []ipRange{{first, last}} // a single-address subnet
	}
	lo, hi := first, last
	switch {
	case first.Is4() && prefix.Bits() <= 30:
		lo, hi = first.Next(), last.Prev()
	case first.Is6():
		lo = first.Next()
	}
	if !gateway.IsValid() || gateway.Compare(lo) < 0 || gateway.Compare(hi) > 0 {
		return []ipRange{{lo, hi}}
	}
	var out []ipRange
	if gateway != lo {
		out = append(out, ipRange{lo, gateway.Prev()})
	}
	if gateway != hi {
		out = append(out, ipRange{gateway.Next(), hi})
	}
	return out
}

// Upstream's errors for allocation pools (neutron_lib/exceptions/__init__.py), with their types.
func errInvalidAllocationPool(pool string) *common.OpenStackError {
	return common.NewNeutronError(http.StatusBadRequest, "InvalidAllocationPool",
		fmt.Sprintf("The allocation pool %s is not valid.", pool))
}

func errOutOfBoundsAllocationPool(pool ipRange, cidr netip.Prefix) *common.OpenStackError {
	return common.NewNeutronError(http.StatusBadRequest, "OutOfBoundsAllocationPool",
		fmt.Sprintf("The allocation pool %s spans beyond the subnet cidr %s.", pool, cidr))
}

func errOverlappingAllocationPools(a, b ipRange, cidr netip.Prefix) *common.OpenStackError {
	return common.NewNeutronError(http.StatusConflict, "OverlappingAllocationPools",
		fmt.Sprintf("Found overlapping allocation pools: %s %s for subnet %s.", a, b, cidr))
}

func errGatewayConflictWithAllocationPools(gateway netip.Addr, pool ipRange) *common.OpenStackError {
	return common.NewNeutronError(http.StatusConflict, "GatewayConflictWithAllocationPools",
		fmt.Sprintf("Gateway ip %s conflicts with allocation pool %s.", gateway, pool))
}

// parsePoolsField reads a body's allocation_pools: absent is "not given"; null and [] give no pool;
// anything else must be a list of {"start", "end"} addresses, start not after end, as upstream's
// pools_to_ip_range has it (InvalidAllocationPool otherwise).
func parsePoolsField(raw json.RawMessage) (pools []ipRange, given bool, refusal *common.OpenStackError) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, false, nil
	}
	if trimmed == "null" {
		return []ipRange{}, true, nil
	}
	var items []struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return nil, true, invalidInput(fmt.Sprintf("Invalid data format for IP pool: '%s'", trimmed))
	}
	pools = make([]ipRange, 0, len(items))
	for _, item := range items {
		first, err1 := netip.ParseAddr(strings.TrimSpace(item.Start))
		last, err2 := netip.ParseAddr(strings.TrimSpace(item.End))
		name := item.Start + "-" + item.End
		if err1 != nil || err2 != nil || first.Is4() != last.Is4() || first.Compare(last) > 0 {
			return nil, true, errInvalidAllocationPool(name)
		}
		pools = append(pools, ipRange{first.Unmap(), last.Unmap()})
	}
	return pools, true, nil
}

// validatePools is upstream's validation of given pools, in upstream's order
// (neutron/db/ipam_backend_mixin.py validate_allocation_pools, then validate_gw_out_of_pools): each
// pool of the subnet's IP version, then inside its usable range — from the second address to the last,
// or the next-to-last in IPv4 — then no two overlapping, then the gateway in none.
func validatePools(prefix netip.Prefix, gateway netip.Addr, pools []ipRange) *common.OpenStackError {
	subnetFirst, subnetLast := prefix.Masked().Addr().Next(), lastAddr(prefix)
	if subnetLast.Is4() {
		subnetLast = subnetLast.Prev()
	}
	for _, p := range pools {
		if p.first.Is4() != prefix.Addr().Is4() {
			return errInvalidAllocationPool(p.String())
		}
		if p.first.Compare(subnetFirst) < 0 || p.last.Compare(subnetLast) > 0 {
			return errOutOfBoundsAllocationPool(p, prefix)
		}
	}
	for i := range pools {
		for j := i + 1; j < len(pools); j++ {
			if pools[i].overlaps(pools[j]) {
				return errOverlappingAllocationPools(pools[i], pools[j], prefix)
			}
		}
	}
	return gatewayOutOfPools(gateway, pools)
}

// gatewayOutOfPools refuses a gateway inside a pool (GatewayConflictWithAllocationPools).
func gatewayOutOfPools(gateway netip.Addr, pools []ipRange) *common.OpenStackError {
	if !gateway.IsValid() {
		return nil
	}
	for _, p := range pools {
		if p.contains(gateway) {
			return errGatewayConflictWithAllocationPools(gateway, p)
		}
	}
	return nil
}

// sortPools orders pools by their first address, as a read reports them.
func sortPools(pools []ipRange) {
	slices.SortFunc(pools, func(a, b ipRange) int { return a.first.Compare(b.first) })
}

// subnetRecord is what the allocator reads of a subnet.
type subnetRecord struct {
	id, network string
	prefix      netip.Prefix
	gateway     netip.Addr
	pools       []ipRange
}

// readSubnet reads a subnet with its pools: the stored ones when pools_stored, the default otherwise.
func readSubnet(ctx context.Context, q subnetQuerier, subnetID string) (subnetRecord, error) {
	var cidr string
	var gateway *string
	var stored *bool
	s := subnetRecord{id: subnetID}
	if err := q.QueryRow(ctx,
		"SELECT network_id::text, cidr, gateway_ip, pools_stored FROM subnets WHERE id::text = $1", subnetID,
	).Scan(&s.network, &cidr, &gateway, &stored); err != nil {
		return s, fmt.Errorf("reading subnet %s: %w", subnetID, err)
	}
	prefix, err := parsePrefix(cidr)
	if err != nil {
		return s, err
	}
	s.prefix, s.gateway = prefix, parseGateway(gateway)
	byID, err := storedPools(ctx, q, []string{subnetID})
	if err != nil {
		return s, err
	}
	s.pools = poolsFor(prefix, s.gateway, stored != nil && *stored, byID[subnetID])
	return s, nil
}

// poolsFor is a subnet's pools: the stored ones when it stores them, the default otherwise.
func poolsFor(prefix netip.Prefix, gateway netip.Addr, stored bool, rows []ipRange) []ipRange {
	if !stored {
		return generatePools(prefix, gateway)
	}
	if rows == nil {
		return []ipRange{}
	}
	return rows
}

// subnetPoolsFor renders a subnet row's pools for a response, reading the stored ones when it has
// them. A CIDR that does not parse has none.
func subnetPoolsFor(ctx context.Context, q subnetQuerier, subnetID, cidr string, gateway *string, stored *bool) ([]gin.H, error) {
	prefix, err := parsePrefix(cidr)
	if err != nil {
		return []gin.H{}, nil
	}
	var rows []ipRange
	if stored != nil && *stored {
		byID, err := storedPools(ctx, q, []string{subnetID})
		if err != nil {
			return nil, err
		}
		rows = byID[subnetID]
	}
	return renderPools(poolsFor(prefix, parseGateway(gateway), stored != nil && *stored, rows)), nil
}

// storedPools reads the stored pools of the given subnets, keyed by subnet and sorted.
func storedPools(ctx context.Context, q subnetQuerier, subnetIDs []string) (map[string][]ipRange, error) {
	out := map[string][]ipRange{}
	if len(subnetIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx,
		"SELECT subnet_id::text, first_ip, last_ip FROM subnet_allocation_pools WHERE subnet_id::text = ANY($1)", subnetIDs)
	if err != nil {
		return nil, fmt.Errorf("reading allocation pools: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var subnet, first, last string
		if err := rows.Scan(&subnet, &first, &last); err != nil {
			return nil, fmt.Errorf("reading an allocation pool: %w", err)
		}
		f, err1 := netip.ParseAddr(first)
		l, err2 := netip.ParseAddr(last)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("subnet %s has an unreadable allocation pool %s-%s", subnet, first, last)
		}
		out[subnet] = append(out[subnet], ipRange{f.Unmap(), l.Unmap()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading allocation pools: %w", err)
	}
	for _, pools := range out {
		sortPools(pools)
	}
	return out, nil
}

// storePools replaces a subnet's pools in the caller's transaction and marks them stored.
func storePools(ctx context.Context, tx database.Tx, subnetID string, pools []ipRange) error {
	if _, err := tx.Exec(ctx, "DELETE FROM subnet_allocation_pools WHERE subnet_id = $1", subnetID); err != nil {
		return fmt.Errorf("clearing the allocation pools of subnet %s: %w", subnetID, err)
	}
	for _, p := range pools {
		if _, err := tx.Exec(ctx, "INSERT INTO subnet_allocation_pools (subnet_id, first_ip, last_ip) VALUES ($1, $2, $3)",
			subnetID, p.first.String(), p.last.String()); err != nil {
			return fmt.Errorf("storing an allocation pool of subnet %s: %w", subnetID, err)
		}
	}
	if _, err := tx.Exec(ctx, "UPDATE subnets SET pools_stored = $1 WHERE id = $2", true, subnetID); err != nil {
		return fmt.Errorf("marking the allocation pools of subnet %s stored: %w", subnetID, err)
	}
	return nil
}

// usedAddresses is every address of the subnet that is not free: the gateway, a port's fixed IP on the
// subnet, and a floating IP's address on the subnet's network (FR-032).
func usedAddresses(ctx context.Context, q subnetQuerier, s subnetRecord) (map[netip.Addr]bool, error) {
	used, err := portAddresses(ctx, q, s.network, s.id)
	if err != nil {
		return nil, err
	}
	if s.gateway.IsValid() {
		used[s.gateway] = true
	}
	fips, err := q.Query(ctx, "SELECT floating_ip_address FROM floating_ips WHERE floating_network_id::text = $1", s.network)
	if err != nil {
		return nil, fmt.Errorf("reading the network's floating addresses: %w", err)
	}
	defer fips.Close()
	for fips.Next() {
		var address string
		if err := fips.Scan(&address); err != nil {
			return nil, fmt.Errorf("scanning a floating address: %w", err)
		}
		if a, err := netip.ParseAddr(address); err == nil && s.prefix.Contains(a.Unmap()) {
			used[a.Unmap()] = true
		}
	}
	return used, fips.Err()
}

// portAddresses is every address of the subnet that a port of its network holds.
func portAddresses(ctx context.Context, q subnetQuerier, networkID, subnetID string) (map[netip.Addr]bool, error) {
	held := map[netip.Addr]bool{}
	rows, err := q.Query(ctx, "SELECT fixed_ips FROM ports WHERE network_id::text = $1", networkID)
	if err != nil {
		return nil, fmt.Errorf("reading the subnet's used addresses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fixed common.FixedIPs
		if err := rows.Scan(&fixed); err != nil {
			return nil, fmt.Errorf("scanning a port's fixed addresses: %w", err)
		}
		for _, entry := range fixed {
			if entry.SubnetID != subnetID {
				continue
			}
			if a, err := netip.ParseAddr(entry.IPAddress); err == nil {
				held[a.Unmap()] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating ports in the subnet's network: %w", err)
	}
	return held, nil
}

// allocationPolicy is how the allocator chooses among free addresses (data-model.md, Addresses on a
// subnet): at random for a port's fixed IP and a router's gateway, as upstream's IPAM does; the
// lowest for a DHCP port and a floating IP.
type allocationPolicy int

const (
	pickRandom allocationPolicy = iota
	pickLowest
)

func (p allocationPolicy) String() string {
	if p == pickLowest {
		return "lowest"
	}
	return "random"
}

// allocateAddress chooses a free address of the subnet's pools by policy. The caller holds the
// subnet's row lock (LockSubnet), whatever it allocates for, and stores the address in the same
// transaction, so no two allocations on a subnet ever hand out one address (FR-032). An exhausted
// subnet is ErrNoAvailableIP.
func allocateAddress(ctx context.Context, q subnetQuerier, subnetID string, policy allocationPolicy) (string, error) {
	return allocateAddressExcluding(ctx, q, subnetID, policy, nil)
}

// allocateAddressExcluding is allocateAddress with more addresses taken out: a floating IP's
// allocation passes every floating address of every network, since floating_ips.floating_ip_address
// is unique across networks — one network node holds them all (FR-033).
func allocateAddressExcluding(ctx context.Context, q subnetQuerier, subnetID string, policy allocationPolicy, excluded map[netip.Addr]bool) (string, error) {
	s, err := readSubnet(ctx, q, subnetID)
	if err != nil {
		return "", err
	}
	used, err := usedAddresses(ctx, q, s)
	if err != nil {
		return "", err
	}
	for a := range excluded {
		used[a] = true
	}
	if policy == pickRandom {
		if a, ok := randomFree(s.pools, used); ok {
			return a.String(), nil
		}
	}
	if a, ok := lowestFree(s.pools, used); ok {
		return a.String(), nil
	}
	return "", fmt.Errorf("%w: %s", ErrNoAvailableIP, subnetID)
}

// lowestFree walks the pools in address order. Each pool is walked at most len(used)+1 steps, so even
// an IPv6 pool of 2^64 addresses is searched in bounded time.
func lowestFree(pools []ipRange, used map[netip.Addr]bool) (netip.Addr, bool) {
	ordered := slices.Clone(pools)
	sortPools(ordered)
	for _, p := range ordered {
		a := p.first
		for steps := 0; steps <= len(used) && a.IsValid() && a.Compare(p.last) <= 0; steps++ {
			if !used[a] {
				return a, true
			}
			a = a.Next()
		}
	}
	return netip.Addr{}, false
}

// randomFree draws up to 100 addresses uniformly across the pools and returns the first free one. A
// sparse subnet answers on the first draw; a dense one falls back to lowestFree, so the last free
// address is never missed by chance.
func randomFree(pools []ipRange, used map[netip.Addr]bool) (netip.Addr, bool) {
	sizes := make([]*big.Int, len(pools))
	total := new(big.Int)
	for i, p := range pools {
		sizes[i] = new(big.Int).Sub(addrInt(p.last), addrInt(p.first))
		sizes[i].Add(sizes[i], big.NewInt(1))
		total.Add(total, sizes[i])
	}
	if total.Sign() <= 0 {
		return netip.Addr{}, false
	}
	for range 100 {
		n, err := rand.Int(rand.Reader, total)
		if err != nil {
			return netip.Addr{}, false
		}
		for i, p := range pools {
			if n.Cmp(sizes[i]) < 0 {
				a := intAddr(new(big.Int).Add(addrInt(p.first), n), p.first.Is4())
				if !used[a] {
					return a, true
				}
				break
			}
			n.Sub(n, sizes[i])
		}
	}
	return netip.Addr{}, false
}

func addrInt(a netip.Addr) *big.Int {
	b := a.As16()
	if a.Is4() {
		v4 := a.As4()
		return new(big.Int).SetBytes(v4[:])
	}
	return new(big.Int).SetBytes(b[:])
}

func intAddr(n *big.Int, v4 bool) netip.Addr {
	if v4 {
		var b [4]byte
		n.FillBytes(b[:])
		return netip.AddrFrom4(b)
	}
	var b [16]byte
	n.FillBytes(b[:])
	return netip.AddrFrom16(b)
}

// AllocateFreeIP chooses a port's fixed IP: a free address of the subnet's pools, at random, as
// upstream's IPAM does. Nova's interface attach allocates with it too, so one allocator serves every
// owner. The caller holds LockSubnet and inserts the port in the same transaction.
func AllocateFreeIP(ctx context.Context, q subnetQuerier, subnetID string) (string, error) {
	return allocateAddress(ctx, q, subnetID, pickRandom)
}

// InvalidIPForSubnetError is upstream's InvalidIpForSubnet (400): a requested address that is not a
// host address of the subnet.
type InvalidIPForSubnetError struct{ IP string }

func (e *InvalidIPForSubnetError) Error() string {
	return fmt.Sprintf("IP address %s is not a valid IP for the specified subnet.", e.IP)
}

// checkSubnetIP is upstream's check_subnet_ip (neutron/ipam/utils.py): inside the subnet and not its
// network address; in IPv4 up to /30 not its broadcast address either. A router's port may hold an
// IPv6 subnet's network address, the subnet-router anycast address; no other port may.
func checkSubnetIP(prefix netip.Prefix, a netip.Addr, routerPort bool) bool {
	if !a.IsValid() || a.Is4() != prefix.Addr().Is4() || !prefix.Contains(a) {
		return false
	}
	network := prefix.Masked().Addr()
	if a.Is4() {
		return a != network && (prefix.Bits() > 30 || a != lastAddr(prefix))
	}
	return routerPort || a != network
}

// IPAvailable reports whether a requested address may be given to a port on the subnet. One that is
// not a host address of the subnet is an *InvalidIPForSubnetError, as upstream's check_subnet_ip
// refuses it (FR-053). Otherwise it is available when nothing holds it: not the gateway, a port's fixed
// IP or a floating IP of the network. It need not lie in a pool (FR-032).
func IPAvailable(ctx context.Context, q subnetQuerier, subnetID, ip string) (bool, error) {
	s, err := readSubnet(ctx, q, subnetID)
	if err != nil {
		return false, err
	}
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil || !checkSubnetIP(s.prefix, a.Unmap(), false) {
		return false, &InvalidIPForSubnetError{IP: ip}
	}
	used, err := usedAddresses(ctx, q, s)
	if err != nil {
		return false, err
	}
	return !used[a.Unmap()], nil
}

// errSubnetInUse is upstream's SubnetInUse for a subnet a port other than its network's DHCP port has
// an address on (FR-052).
func errSubnetInUse(subnetID string) *common.OpenStackError {
	return common.NewNeutronError(http.StatusConflict, "SubnetInUse",
		fmt.Sprintf("Unable to complete operation on subnet %s: One or more ports have an IP allocation from this subnet.", subnetID))
}

// subnetInUse reports whether a port other than the network's DHCP port has an address on the subnet.
func subnetInUse(ctx context.Context, q subnetQuerier, subnetID, networkID string) (bool, error) {
	rows, err := q.Query(ctx,
		"SELECT COALESCE(device_owner, ''), fixed_ips FROM ports WHERE network_id::text = $1", networkID)
	if err != nil {
		return false, fmt.Errorf("reading the ports of network %s: %w", networkID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var owner string
		var fixed common.FixedIPs
		if err := rows.Scan(&owner, &fixed); err != nil {
			return false, fmt.Errorf("reading a port of network %s: %w", networkID, err)
		}
		if owner == "network:dhcp" {
			continue
		}
		for _, entry := range fixed {
			if entry.SubnetID == subnetID {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}
