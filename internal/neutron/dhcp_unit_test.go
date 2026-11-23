package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T120 (FR-008, FR-037, FR-040; research R20, R27, R29, R34): a network's DHCP is a
// unit on the network node. The network's first DHCP subnet gives it a network:dhcp port of the
// network's project, which holds the lowest free pool address of each DHCP subnet; its last takes the
// port away. Every change to what the network's dnsmasq serves — a DHCP subnet created, its gateway
// changed or deleted, a port with an address on one created or deleted, through the API or for a
// server, the network deleted — converges the unit on the network node before the answer, metadata
// listener included. A unit that cannot be converged answers 500: a create or an update is undone,
// compare-and-set; a deletion keeps its record and the unit is converged from it again.

// dhcpRig is two nodes on one database, as unitRig: the API node answers, the network node holds the
// networks' DHCP and their metadata listeners.
type dhcpRig struct {
	r                       *gin.Engine // the API node's routes
	api, net                *Service
	dhcp, apiDHCP           *fakeDHCP      // the network node's; the API node's, which must stay untouched
	listeners, apiListeners *fakeListeners // likewise
	hdr                     map[string]string
}

func newDHCPRig(t *testing.T) *dhcpRig {
	t.Helper()
	r, api := newNeutronTestRouterWithService()
	_, net := newNeutronTestRouterWithService()
	api.SetNodes(NodeOptions{Name: unitAPINode, NetworkNode: unitNetworkNode})
	net.SetNodes(NodeOptions{Name: unitNetworkNode, NetworkNode: unitNetworkNode})
	rig := &dhcpRig{r: r, api: api, net: net, dhcp: newFakeDHCP(), apiDHCP: newFakeDHCP(),
		listeners: newFakeListeners(), apiListeners: newFakeListeners(),
		hdr: map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}}
	net.dhcp, api.dhcp = rig.dhcp, rig.apiDHCP
	net.SetMetadataListeners(rig.listeners)
	api.SetMetadataListeners(rig.apiListeners)
	caller := newFakeNodeCaller(api, net)
	api.caller, net.caller = caller, caller
	t.Cleanup(func() {
		if calls := rig.apiDHCP.recorded(); len(calls) != 0 {
			t.Errorf("the API node converged %d network DHCP(s) itself; the network node holds them", len(calls))
		}
		if calls := rig.apiListeners.recorded(); len(calls) != 0 {
			t.Errorf("the API node opened or closed metadata listeners %q; the network node holds them", calls)
		}
	})
	return rig
}

func (rig *dhcpRig) do(t *testing.T, hdr map[string]string, method, path, body string) (int, string) {
	t.Helper()
	return doJSONReq(rig.r, method, path, body, hdr)
}

func (rig *dhcpRig) must(t *testing.T, hdr map[string]string, want int, method, path, body string) string {
	t.Helper()
	code, resp := rig.do(t, hdr, method, path, body)
	if code != want {
		t.Fatalf("%s %s: code=%d body=%s; want %d", method, path, code, resp, want)
	}
	return resp
}

// subnet creates an IPv4 subnet on network with the given fields and returns it.
func (rig *dhcpRig) subnet(t *testing.T, hdr map[string]string, network, fields string) string {
	t.Helper()
	return createdID(t, rig.must(t, hdr, http.StatusCreated, http.MethodPost, "/v2.0/subnets",
		`{"subnet":{"network_id":"`+network+`","ip_version":4,`+fields+`}}`), "subnet")
}

// port creates a port on network and returns it, its address and its MAC.
func (rig *dhcpRig) port(t *testing.T, network string) (id, address, mac string) {
	t.Helper()
	resp := rig.must(t, rig.hdr, http.StatusCreated, http.MethodPost, "/v2.0/ports", `{"port":{"network_id":"`+network+`"}}`)
	var out struct {
		Port struct {
			ID       string          `json:"id"`
			MAC      string          `json:"mac_address"`
			FixedIPs common.FixedIPs `json:"fixed_ips"`
		} `json:"port"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.Port.ID == "" || len(out.Port.FixedIPs) == 0 {
		t.Fatalf("no port and address in %s (%v)", resp, err)
	}
	return out.Port.ID, out.Port.FixedIPs[0].IPAddress, out.Port.MAC
}

// allocate allocates a server's port on network, as Nova's boot does.
func (rig *dhcpRig) allocate(t *testing.T, network, instance string) *PortInfo {
	t.Helper()
	res, err := rig.api.AllocatePortForInstance(context.Background(), InstancePort{NetworkID: network, ProjectID: seededProjectID, InstanceID: instance, SecurityGroupIDs: nil, CopyGroupsFrom: "", Admin: false})
	if err != nil {
		t.Fatalf("allocate a port for server %s: %v", instance, err)
	}
	info, ok := res.(*PortInfo)
	if !ok {
		t.Fatalf("the allocation answered %T; want *PortInfo", res)
	}
	return info
}

// held is the network's DHCP as the network node holds it.
func (rig *dhcpRig) held(t *testing.T, network string) networking.DHCPState {
	t.Helper()
	s, ok := rig.dhcp.state(network)
	if !ok {
		t.Fatalf("the network node holds no DHCP of network %s", network)
	}
	return s
}

// heldSubnets is the network's DHCP subnets as the network node holds them, "<id> <address> <gateway>".
func (rig *dhcpRig) heldSubnets(t *testing.T, network string) []string {
	t.Helper()
	var out []string
	for _, s := range rig.held(t, network).Subnets {
		out = append(out, s.ID+" "+s.Address+" "+s.Gateway)
	}
	slices.Sort(out)
	return out
}

func (rig *dhcpRig) serves(t *testing.T, network string, host networking.DHCPHost) bool {
	t.Helper()
	s, ok := rig.dhcp.state(network)
	return ok && slices.Contains(s.Hosts, host)
}

// dhcpPortRecord is a network's network:dhcp port as stored.
type dhcpPortRecord struct {
	id, project, deviceID string
	fixed                 common.FixedIPs
}

func readDHCPPort(t *testing.T, network string) (dhcpPortRecord, bool) {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT id::text, COALESCE(project_id::text, ''), COALESCE(device_id::text, ''), fixed_ips
		  FROM ports WHERE network_id = $1 AND device_owner = 'network:dhcp'`, network)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ports []dhcpPortRecord
	for rows.Next() {
		var p dhcpPortRecord
		if err := rows.Scan(&p.id, &p.project, &p.deviceID, &p.fixed); err != nil {
			t.Fatal(err)
		}
		ports = append(ports, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ports) > 1 {
		t.Fatalf("network %s has %d DHCP ports; want at most one", network, len(ports))
	}
	if len(ports) == 0 {
		return dhcpPortRecord{}, false
	}
	return ports[0], true
}

// addresses is a port's fixed IPs as "<subnet> <address>", sorted.
func addresses(fixed common.FixedIPs) []string {
	var out []string
	for _, f := range fixed {
		out = append(out, f.SubnetID+" "+f.IPAddress)
	}
	slices.Sort(out)
	return out
}

// seedNetworkOwnedBy seeds a network of project, as seedNetwork does for the seeded one.
func seedNetworkOwnedBy(t *testing.T, project string) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, network_type, mtu, created_at, updated_at)
		VALUES ($1, 'spec033-dhcp', $2, true, 'ACTIVE', false, 'flat', 1500, now(), now())`, id, project)
	t.Cleanup(func() { mustExec(t, "DELETE FROM networks WHERE id = $1", id) })
	return id
}

func TestDHCPPortFollowsTheNetworksDHCPSubnets(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	rig := newDHCPRig(t)
	// The administrator's subnets are of the administrator's project; the network, and so its DHCP
	// port, is another's.
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	network := seedNetworkOwnedBy(t, otherProjectID)

	a := rig.subnet(t, admin, network, `"cidr":"10.91.0.0/24","allocation_pools":[{"start":"10.91.0.20","end":"10.91.0.30"}]`)
	port, ok := readDHCPPort(t, network)
	if !ok {
		t.Fatal("the network's first DHCP subnet created no DHCP port")
	}
	if port.project != otherProjectID || port.deviceID != network {
		t.Errorf("DHCP port of project %q, device %q; want the network's project %s and the network as its device", port.project, port.deviceID, otherProjectID)
	}
	if got, want := addresses(port.fixed), []string{a + " 10.91.0.20"}; !slices.Equal(got, want) {
		t.Errorf("DHCP port addresses %q; want the lowest free pool address %q", got, want)
	}
	if held := rig.held(t, network); held.PortID != port.id || held.Bridge != networking.BridgeNameForNetwork(network) {
		t.Errorf("held DHCP of port %s on %s; want port %s on the network's bridge", held.PortID, held.Bridge, port.id)
	}
	if got, want := rig.heldSubnets(t, network), []string{a + " 10.91.0.20 10.91.0.1"}; !slices.Equal(got, want) {
		t.Errorf("held subnets %q; want %q", got, want)
	}
	if !rig.listeners.serving(network) {
		t.Error("no metadata listener for the network once it has DHCP")
	}

	b := rig.subnet(t, admin, network, `"cidr":"10.92.0.0/24"`)
	again, _ := readDHCPPort(t, network)
	if again.id != port.id {
		t.Errorf("a second DHCP subnet gave the network DHCP port %s; want its port %s kept", again.id, port.id)
	}
	if got, want := addresses(again.fixed), sorted(a+" 10.91.0.20", b+" 10.92.0.2"); !slices.Equal(got, want) {
		t.Errorf("DHCP port addresses %q; want an address added on the second subnet: %q", got, want)
	}
	if got, want := rig.heldSubnets(t, network), sorted(a+" 10.91.0.20 10.91.0.1", b+" 10.92.0.2 10.92.0.1"); !slices.Equal(got, want) {
		t.Errorf("held subnets %q; want %q", got, want)
	}

	rig.subnet(t, admin, network, `"cidr":"10.93.0.0/24","enable_dhcp":false`)
	if p, _ := readDHCPPort(t, network); len(p.fixed) != 2 {
		t.Errorf("a subnet without DHCP gave the DHCP port an address: %q", addresses(p.fixed))
	}

	rig.must(t, admin, http.StatusNoContent, http.MethodDelete, "/v2.0/subnets/"+a, "")
	if p, _ := readDHCPPort(t, network); !slices.Equal(addresses(p.fixed), []string{b + " 10.92.0.2"}) {
		t.Errorf("DHCP port addresses after the first subnet's delete: %q; want only the second's", addresses(p.fixed))
	}
	if got, want := rig.heldSubnets(t, network), []string{b + " 10.92.0.2 10.92.0.1"}; !slices.Equal(got, want) {
		t.Errorf("held subnets %q; want %q", got, want)
	}

	rig.must(t, admin, http.StatusNoContent, http.MethodDelete, "/v2.0/subnets/"+b, "")
	if p, ok := readDHCPPort(t, network); ok {
		t.Errorf("the last DHCP subnet's delete left DHCP port %s", p.id)
	}
	if _, ok := rig.dhcp.state(network); ok {
		t.Error("the network node still holds the network's DHCP after its last DHCP subnet went")
	}
	if rig.listeners.serving(network) {
		t.Error("the network's metadata listener is still open after its last DHCP subnet went")
	}
}

func TestDHCPUnitConvergesBeforeTheAnswer(t *testing.T) {
	requireDB(t)
	rig := newDHCPRig(t)
	network := seedNetwork(t, "spec033-dhcp-"+uuid.NewString()[:6])
	s := rig.subnet(t, rig.hdr, network, `"cidr":"10.94.0.0/24"`)

	p, ip, mac := rig.port(t, network)
	if !rig.serves(t, network, networking.DHCPHost{MAC: mac, IP: ip}) {
		t.Errorf("a port's create: hosts %+v; want its reservation %s %s before the 201", rig.held(t, network).Hosts, mac, ip)
	}

	rig.must(t, rig.hdr, http.StatusOK, http.MethodPut, "/v2.0/subnets/"+s, `{"subnet":{"gateway_ip":null}}`)
	if got, want := rig.heldSubnets(t, network), []string{s + " 10.94.0.2 "}; !slices.Equal(got, want) {
		t.Errorf("a gateway cleared: held subnets %q; want %q before the 200", got, want)
	}
	rig.must(t, rig.hdr, http.StatusOK, http.MethodPut, "/v2.0/subnets/"+s, `{"subnet":{"gateway_ip":"10.94.0.1"}}`)
	if got, want := rig.heldSubnets(t, network), []string{s + " 10.94.0.2 10.94.0.1"}; !slices.Equal(got, want) {
		t.Errorf("a gateway set: held subnets %q; want %q before the 200", got, want)
	}

	instance := uuid.NewString()
	info := rig.allocate(t, network, instance)
	server := networking.DHCPHost{MAC: info.MAC, IP: info.IPAddress}
	if !rig.serves(t, network, server) {
		t.Errorf("a server's port: hosts %+v; want its reservation before the allocation returns", rig.held(t, network).Hosts)
	}
	if err := rig.api.ReleaseInstancePorts(context.Background(), instance); err != nil {
		t.Fatalf("release the server's ports: %v", err)
	}
	if rig.serves(t, network, server) {
		t.Error("a server's ports released: its reservation is still served")
	}

	rig.must(t, rig.hdr, http.StatusNoContent, http.MethodDelete, "/v2.0/ports/"+p, "")
	if rig.serves(t, network, networking.DHCPHost{MAC: mac, IP: ip}) {
		t.Error("a port's delete: its reservation is still served after the 204")
	}

	rig.must(t, rig.hdr, http.StatusNoContent, http.MethodDelete, "/v2.0/networks/"+network, "")
	if _, ok := rig.dhcp.state(network); ok {
		t.Error("a network's delete left its DHCP on the network node")
	}
	if rig.listeners.serving(network) {
		t.Error("a network's delete left its metadata listener open")
	}
}

// 'subnet set --no-dhcp' and '--dhcp' (SPEC-031) change what the network's DHCP serves (R29): the flag
// turned off takes the DHCP port's address on the subnet away, or the port with the network's last DHCP
// subnet, and the network node stops serving the subnet before the answer; turned on, both come back.
func TestDHCPFollowsASubnetsDHCPFlag(t *testing.T) {
	requireDB(t)
	rig := newDHCPRig(t)
	network := seedNetwork(t, "spec033-dhcp-"+uuid.NewString()[:6])
	a := rig.subnet(t, rig.hdr, network, `"cidr":"10.97.0.0/24"`)
	b := rig.subnet(t, rig.hdr, network, `"cidr":"10.98.0.0/24"`)
	flag := func(subnet, on string) {
		t.Helper()
		rig.must(t, rig.hdr, http.StatusOK, http.MethodPut, "/v2.0/subnets/"+subnet, `{"subnet":{"enable_dhcp":`+on+`}}`)
	}

	flag(b, "false")
	if p, _ := readDHCPPort(t, network); !slices.Equal(addresses(p.fixed), []string{a + " 10.97.0.2"}) {
		t.Errorf("DHCP port addresses with DHCP off on the second subnet: %q; want only the first's", addresses(p.fixed))
	}
	if got, want := rig.heldSubnets(t, network), []string{a + " 10.97.0.2 10.97.0.1"}; !slices.Equal(got, want) {
		t.Errorf("DHCP off: held subnets %q; want %q before the 200", got, want)
	}

	flag(b, "true")
	if p, _ := readDHCPPort(t, network); !slices.Equal(addresses(p.fixed), sorted(a+" 10.97.0.2", b+" 10.98.0.2")) {
		t.Errorf("DHCP port addresses with DHCP on again: %q; want an address on each subnet", addresses(p.fixed))
	}
	if got, want := rig.heldSubnets(t, network), sorted(a+" 10.97.0.2 10.97.0.1", b+" 10.98.0.2 10.98.0.1"); !slices.Equal(got, want) {
		t.Errorf("DHCP on again: held subnets %q; want %q before the 200", got, want)
	}

	flag(a, "false")
	flag(b, "false")
	if p, ok := readDHCPPort(t, network); ok {
		t.Errorf("DHCP port %s left with DHCP off on every subnet", p.id)
	}
	if _, ok := rig.dhcp.state(network); ok {
		t.Error("the network node still holds the network's DHCP with DHCP off on every subnet")
	}
}

func TestDHCPUnitFailuresUndoOrReapply(t *testing.T) {
	requireDB(t)
	errUnit := errors.New("the network node could not converge the network's DHCP")
	count := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := testPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// fails runs a request whose first convergence fails, and wants 500.
	fails := func(t *testing.T, rig *dhcpRig, method, path, body string) {
		t.Helper()
		rig.dhcp.failFirst(1, errUnit)
		if code, resp := rig.do(t, rig.hdr, method, path, body); code != http.StatusInternalServerError {
			t.Fatalf("%s %s with a failing DHCP unit: code=%d body=%s; want 500", method, path, code, resp)
		}
		rig.dhcp.failFirst(0, nil)
	}
	setup := func(t *testing.T) (*dhcpRig, string, string) {
		rig := newDHCPRig(t)
		network := seedNetwork(t, "spec033-dhcp-"+uuid.NewString()[:6])
		return rig, network, rig.subnet(t, rig.hdr, network, `"cidr":"10.95.0.0/24"`)
	}

	t.Run("the first DHCP subnet's create is undone", func(t *testing.T) {
		rig := newDHCPRig(t)
		network := seedNetwork(t, "spec033-dhcp-"+uuid.NewString()[:6])
		fails(t, rig, http.MethodPost, "/v2.0/subnets", `{"subnet":{"network_id":"`+network+`","ip_version":4,"cidr":"10.95.0.0/24"}}`)
		if n := count(t, "SELECT count(*) FROM subnets WHERE network_id = $1", network); n != 0 {
			t.Errorf("%d subnet(s) left; want the create undone", n)
		}
		if p, ok := readDHCPPort(t, network); ok {
			t.Errorf("DHCP port %s left; want it undone with the subnet", p.id)
		}
		if _, ok := rig.dhcp.state(network); ok {
			t.Error("the network node holds DHCP after the undo; want it converged from the restored records")
		}
	})

	t.Run("a second DHCP subnet's create is undone", func(t *testing.T) {
		rig, network, a := setup(t)
		fails(t, rig, http.MethodPost, "/v2.0/subnets", `{"subnet":{"network_id":"`+network+`","ip_version":4,"cidr":"10.96.0.0/24"}}`)
		if n := count(t, "SELECT count(*) FROM subnets WHERE network_id = $1", network); n != 1 {
			t.Errorf("%d subnets; want the second's create undone", n)
		}
		if p, _ := readDHCPPort(t, network); !slices.Equal(addresses(p.fixed), []string{a + " 10.95.0.2"}) {
			t.Errorf("DHCP port addresses after the undo: %q; want only the first subnet's", addresses(p.fixed))
		}
		if got, want := rig.heldSubnets(t, network), []string{a + " 10.95.0.2 10.95.0.1"}; !slices.Equal(got, want) {
			t.Errorf("held subnets after the undo: %q; want %q", got, want)
		}
	})

	t.Run("a metadata listener that cannot be opened fails the unit", func(t *testing.T) {
		rig := newDHCPRig(t)
		network := seedNetwork(t, "spec033-dhcp-"+uuid.NewString()[:6])
		rig.listeners.hook = func(call string) error {
			if call == "open "+network {
				return errUnit
			}
			return nil
		}
		if code, resp := rig.do(t, rig.hdr, http.MethodPost, "/v2.0/subnets",
			`{"subnet":{"network_id":"`+network+`","ip_version":4,"cidr":"10.95.0.0/24"}}`); code != http.StatusInternalServerError {
			t.Fatalf("a failing listener: code=%d body=%s; want 500", code, resp)
		}
		if n := count(t, "SELECT count(*) FROM subnets WHERE network_id = $1", network); n != 0 {
			t.Errorf("%d subnet(s) left; want the create undone", n)
		}
	})

	t.Run("a gateway change is undone", func(t *testing.T) {
		rig, network, s := setup(t)
		fails(t, rig, http.MethodPut, "/v2.0/subnets/"+s, `{"subnet":{"gateway_ip":null}}`)
		var gateway *string
		if err := testPool.QueryRow(context.Background(), "SELECT gateway_ip FROM subnets WHERE id = $1", s).Scan(&gateway); err != nil ||
			gateway == nil || *gateway != "10.95.0.1" {
			t.Errorf("the gateway after the undo: %v, %v; want 10.95.0.1 back", gateway, err)
		}
		if got, want := rig.heldSubnets(t, network), []string{s + " 10.95.0.2 10.95.0.1"}; !slices.Equal(got, want) {
			t.Errorf("held subnets after the undo: %q; want the gateway converged back: %q", got, want)
		}
	})

	t.Run("a DHCP flag change is undone", func(t *testing.T) {
		rig, network, s := setup(t)
		fails(t, rig, http.MethodPut, "/v2.0/subnets/"+s, `{"subnet":{"enable_dhcp":false}}`)
		var enabled bool
		if err := testPool.QueryRow(context.Background(), "SELECT enable_dhcp FROM subnets WHERE id = $1", s).Scan(&enabled); err != nil || !enabled {
			t.Errorf("enable_dhcp after the undo: %v, %v; want true back", enabled, err)
		}
		if p, _ := readDHCPPort(t, network); !slices.Equal(addresses(p.fixed), []string{s + " 10.95.0.2"}) {
			t.Errorf("DHCP port addresses after the undo: %q; want its address on the subnet back", addresses(p.fixed))
		}
		if got, want := rig.heldSubnets(t, network), []string{s + " 10.95.0.2 10.95.0.1"}; !slices.Equal(got, want) {
			t.Errorf("held subnets after the undo: %q; want the subnet converged back: %q", got, want)
		}
	})

	t.Run("a port's create is undone", func(t *testing.T) {
		rig, network, _ := setup(t)
		fails(t, rig, http.MethodPost, "/v2.0/ports", `{"port":{"network_id":"`+network+`","name":"spec033-undone"}}`)
		if n := count(t, "SELECT count(*) FROM ports WHERE network_id = $1 AND name = 'spec033-undone'", network); n != 0 {
			t.Errorf("%d port(s) left; want the create undone", n)
		}
		if hosts := rig.held(t, network).Hosts; len(hosts) != 0 {
			t.Errorf("hosts after the undo: %+v; want none", hosts)
		}
	})

	t.Run("a server's port is undone", func(t *testing.T) {
		rig, network, _ := setup(t)
		instance := uuid.NewString()
		rig.dhcp.failFirst(1, errUnit)
		if _, err := rig.api.AllocatePortForInstance(context.Background(), InstancePort{NetworkID: network, ProjectID: seededProjectID, InstanceID: instance, SecurityGroupIDs: nil, CopyGroupsFrom: "", Admin: false}); err == nil {
			t.Fatal("a failing DHCP unit: the server's port was allocated")
		}
		if n := count(t, "SELECT count(*) FROM ports WHERE device_id = $1", instance); n != 0 {
			t.Errorf("%d port(s) of the server left; want the allocation undone", n)
		}
		if hosts := rig.held(t, network).Hosts; len(hosts) != 0 {
			t.Errorf("hosts after the undo: %+v; want none", hosts)
		}
	})

	t.Run("a subnet's delete is kept and re-applied", func(t *testing.T) {
		rig, network, a := setup(t)
		b := rig.subnet(t, rig.hdr, network, `"cidr":"10.96.0.0/24"`)
		fails(t, rig, http.MethodDelete, "/v2.0/subnets/"+b, "")
		if n := count(t, "SELECT count(*) FROM subnets WHERE id = $1", b); n != 1 {
			t.Error("the subnet is gone; want its record kept")
		}
		if got, want := rig.heldSubnets(t, network), sorted(a+" 10.95.0.2 10.95.0.1", b+" 10.96.0.2 10.96.0.1"); !slices.Equal(got, want) {
			t.Errorf("held subnets after the failure: %q; want both re-applied: %q", got, want)
		}
	})

	t.Run("a port's delete is kept and re-applied", func(t *testing.T) {
		rig, network, _ := setup(t)
		p, ip, mac := rig.port(t, network)
		fails(t, rig, http.MethodDelete, "/v2.0/ports/"+p, "")
		if n := count(t, "SELECT count(*) FROM ports WHERE id = $1", p); n != 1 {
			t.Error("the port is gone; want its record kept")
		}
		if !rig.serves(t, network, networking.DHCPHost{MAC: mac, IP: ip}) {
			t.Errorf("hosts after the failure: %+v; want the port's reservation re-applied", rig.held(t, network).Hosts)
		}
	})

	t.Run("a server's ports' release is kept and re-applied", func(t *testing.T) {
		rig, network, _ := setup(t)
		instance := uuid.NewString()
		info := rig.allocate(t, network, instance)
		rig.dhcp.failFirst(1, errUnit)
		if err := rig.api.ReleaseInstancePorts(context.Background(), instance); err == nil {
			t.Fatal("a failing DHCP unit: the server's ports were released")
		}
		if n := count(t, "SELECT count(*) FROM ports WHERE id = $1", info.ID); n != 1 {
			t.Error("the server's port is gone; want its record kept")
		}
		if !rig.serves(t, network, networking.DHCPHost{MAC: info.MAC, IP: info.IPAddress}) {
			t.Errorf("hosts after the failure: %+v; want the reservation re-applied", rig.held(t, network).Hosts)
		}
	})

	t.Run("a network's delete is kept and re-applied", func(t *testing.T) {
		rig, network, a := setup(t)
		fails(t, rig, http.MethodDelete, "/v2.0/networks/"+network, "")
		if n := count(t, "SELECT count(*) FROM networks WHERE id = $1", network); n != 1 {
			t.Error("the network is gone; want its record kept")
		}
		if got, want := rig.heldSubnets(t, network), []string{a + " 10.95.0.2 10.95.0.1"}; !slices.Equal(got, want) {
			t.Errorf("held subnets after the failure: %q; want them re-applied: %q", got, want)
		}
		if !rig.listeners.serving(network) {
			t.Error("the metadata listener after the failure: closed; want it open again")
		}
	})
}

// The reconciliation pass on the network node makes a network's DHCP port when it has none — deleted
// through the API, as upstream's agent has a new one made when it resyncs, or never made for a subnet
// stored before this release — and converges the network's DHCP with it.
func TestReconcilerMakesTheDHCPPortAgain(t *testing.T) {
	requireDB(t)
	rig := newDHCPRig(t)
	reconciler := NewNetworkReconciler(rig.net, 0)
	ctx := context.Background()

	t.Run("deleted through the API", func(t *testing.T) {
		network := seedNetwork(t, "spec033-dhcp-"+uuid.NewString()[:6])
		s := rig.subnet(t, rig.hdr, network, `"cidr":"10.97.0.0/24"`)
		old, _ := readDHCPPort(t, network)
		rig.must(t, rig.hdr, http.StatusNoContent, http.MethodDelete, "/v2.0/ports/"+old.id, "")
		if _, held := rig.dhcp.state(network); held {
			t.Fatal("the network's DHCP is still held after its DHCP port's delete")
		}
		reconciler.reconcileNetworkDHCP(ctx, network)
		port, ok := readDHCPPort(t, network)
		if !ok || port.id == old.id || !slices.Equal(addresses(port.fixed), []string{s + " 10.97.0.2"}) {
			t.Fatalf("after the pass: port %+v (found %v); want a new one at 10.97.0.2", port, ok)
		}
		if rig.held(t, network).PortID != port.id {
			t.Errorf("the network node holds DHCP of port %s; want the new port %s", rig.held(t, network).PortID, port.id)
		}
	})

	t.Run("a subnet stored before the port existed", func(t *testing.T) {
		network := seedNetwork(t, "spec033-dhcp-"+uuid.NewString()[:6])
		gateway := "10.98.0.1"
		s := seedSubnet(t, network, "spec033-dhcp-old", "10.98.0.0/24", &gateway, 4, true)
		reconciler.reconcileNetworkDHCP(ctx, network)
		if port, ok := readDHCPPort(t, network); !ok || !slices.Equal(addresses(port.fixed), []string{s + " 10.98.0.2"}) {
			t.Fatalf("after the pass: port %+v (found %v); want one at 10.98.0.2", port, ok)
		}
		if got, want := rig.heldSubnets(t, network), []string{s + " 10.98.0.2 10.98.0.1"}; !slices.Equal(got, want) {
			t.Errorf("held subnets %q; want %q", got, want)
		}
	})
}
