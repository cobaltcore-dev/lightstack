package neutron

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T135 (FR-040; research R34): a deletion checks its refusals first, converges the
// units it touches as if its record were gone while the record still exists, then deletes the record in
// a transaction that checks the refusals again. A unit that cannot be converged answers 500 and keeps
// the record; a refusal in the final transaction (a dependant added meanwhile) answers that refusal.
// Either way the units are converged from the records again.

const (
	delAPINode     = "spec033-dapi"
	delNetworkNode = "spec033-dnet"
	delComputeNode = "spec033-dcmp"
)

// delRig is three nodes on one database: the API node, the network node holding routers and DHCP, and
// a compute node holding bound ports. Every unit is a fake.
type delRig struct {
	r                        *gin.Engine
	api, net, cmp            *Service
	routers                  *fakeRouters  // the network node's
	dhcp                     *fakeDHCP     // the network node's
	fw                       *fakeFirewall // the compute node's
	hdr                      map[string]string
	external, externalPrefix string
}

func newDelRig(t *testing.T) *delRig {
	t.Helper()
	rig := &delRig{hdr: map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}}
	var services []*Service
	for _, name := range []string{delAPINode, delNetworkNode, delComputeNode} {
		r, svc := newNeutronTestRouterWithService()
		svc.SetNodes(NodeOptions{Name: name, NetworkNode: delNetworkNode})
		routers, dhcp, fw := newFakeRouters(), newFakeDHCP(), newFakeFirewall()
		svc.routers, svc.dhcp, svc.firewall = routers, dhcp, fw
		services = append(services, svc)
		switch name {
		case delAPINode:
			rig.r, rig.api = r, svc
		case delNetworkNode:
			rig.net, rig.routers, rig.dhcp = svc, routers, dhcp
		case delComputeNode:
			rig.cmp, rig.fw = svc, fw
		}
	}
	caller := newFakeNodeCaller(services...)
	for _, svc := range services {
		svc.caller = caller
	}
	rig.externalPrefix = fmt.Sprintf("198.18.%d", rand.IntN(250))
	rig.external = seedExternalNetwork(t, "spec033-del-ext-"+uuid.NewString()[:6], seededProjectID, false, true)
	gateway := rig.externalPrefix + ".1"
	subnet := seedSubnet(t, rig.external, "spec033-del-ext", rig.externalPrefix+".0/24", &gateway, 4, false)
	storeTestPools(t, subnet, [2]string{rig.externalPrefix + ".10", rig.externalPrefix + ".99"})
	return rig
}

func (rig *delRig) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	return doJSONReq(rig.r, method, path, body, rig.hdr)
}

func (rig *delRig) must(t *testing.T, want int, method, path, body string) string {
	t.Helper()
	code, resp := rig.do(t, method, path, body)
	if code != want {
		t.Fatalf("%s %s: code=%d body=%s; want %d", method, path, code, resp, want)
	}
	return resp
}

// topology is a network with a DHCP subnet, a router with a gateway and an interface on the subnet,
// and a port bound to the compute node with a floating IP through the router.
type topology struct {
	network, subnet, router, port, fixed, floatingIP, floatingAddress string
}

func (rig *delRig) topology(t *testing.T) topology {
	t.Helper()
	var tp topology
	tp.network = seedNetwork(t, "spec033-del-net-"+uuid.NewString()[:6])
	tp.subnet = createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/subnets",
		`{"subnet":{"network_id":"`+tp.network+`","ip_version":4,"cidr":"10.78.0.0/24"}}`), "subnet")
	tp.router = createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/routers",
		`{"router":{"name":"spec033-del","external_gateway_info":{"network_id":"`+rig.external+`"}}}`), "router")
	t.Cleanup(func() {
		mustExec(t, "DELETE FROM floating_ips WHERE router_id = $1", tp.router)
		mustExec(t, "DELETE FROM ports WHERE device_id = $1", tp.router)
		mustExec(t, "DELETE FROM routers WHERE id = $1", tp.router)
	})
	rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+tp.router+"/add_router_interface", `{"subnet_id":"`+tp.subnet+`"}`)
	tp.port, tp.fixed = createPortOn(t, rig.r, rig.hdr, tp.network)
	if err := rig.api.BindPort(context.Background(), tp.port, delComputeNode); err != nil {
		t.Fatal(err)
	}
	tp.floatingIP, tp.floatingAddress = createFloatingIPFor(t, rig.r, rig.hdr, rig.external, tp.port)
	return tp
}

// createPortOn creates a port on network through the API and returns it and its address.
func createPortOn(t *testing.T, r *gin.Engine, hdr map[string]string, network string) (string, string) {
	t.Helper()
	code, resp := doJSONReq(r, http.MethodPost, "/v2.0/ports", `{"port":{"network_id":"`+network+`"}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create port: code=%d body=%s", code, resp)
	}
	id := createdID(t, resp, "port")
	var fixed []struct {
		IP string `json:"ip_address"`
	}
	if err := testPool.QueryRow(context.Background(), "SELECT fixed_ips FROM ports WHERE id = $1", id).Scan(&fixed); err != nil || len(fixed) == 0 {
		t.Fatalf("port %s's address: %v", id, err)
	}
	return id, fixed[0].IP
}

// createFloatingIPFor creates a floating IP on external for port and returns it and its address.
func createFloatingIPFor(t *testing.T, r *gin.Engine, hdr map[string]string, external, port string) (string, string) {
	t.Helper()
	code, resp := doJSONReq(r, http.MethodPost, "/v2.0/floatingips",
		`{"floatingip":{"floating_network_id":"`+external+`","port_id":"`+port+`"}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create floating IP: code=%d body=%s", code, resp)
	}
	id := createdID(t, resp, "floatingip")
	var address string
	if err := testPool.QueryRow(context.Background(), "SELECT floating_ip_address FROM floating_ips WHERE id = $1", id).Scan(&address); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustExec(t, "DELETE FROM floating_ips WHERE id = $1", id) })
	return id, address
}

// exists reports whether table has a row of id.
func exists(t *testing.T, table, id string) bool {
	t.Helper()
	var ok bool
	if err := testPool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE id::text = $1)", id).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func holdsPort(ports []networking.FirewallPort, id string) bool {
	return slices.ContainsFunc(ports, func(p networking.FirewallPort) bool { return p.ID == id })
}

func servesHost(s *networking.DHCPState, ip string) bool {
	return s != nil && slices.ContainsFunc(s.Hosts, func(h networking.DHCPHost) bool { return h.IP == ip })
}

func forwardsFloating(s *networking.RouterState, address string) bool {
	return s != nil && slices.ContainsFunc(s.FloatingIPs, func(f networking.RouterFloatingIP) bool { return f.Address == address })
}

var errUnitDown = errors.New("the node could not converge the unit")

func TestDeletionOrder_Port(t *testing.T) {
	requireDB(t)

	t.Run("its firewall, its floating IP's router and its DHCP reservation go before its record", func(t *testing.T) {
		rig := newDelRig(t)
		tp := rig.topology(t)
		var firewallFirst, routerFirst, dhcpFirst bool
		rig.fw.hook = func(ports []networking.FirewallPort) error {
			if !holdsPort(ports, tp.port) && !firewallFirst {
				firewallFirst = exists(t, "ports", tp.port)
			}
			return nil
		}
		rig.routers.hook = func(c routerCall) error {
			if c.State != nil && !forwardsFloating(c.State, tp.floatingAddress) && !routerFirst {
				routerFirst = exists(t, "ports", tp.port)
			}
			return nil
		}
		rig.dhcp.hook = func(c holderCall[networking.DHCPState]) error {
			if c.ID == tp.network && !servesHost(c.State, tp.fixed) && !dhcpFirst {
				dhcpFirst = exists(t, "ports", tp.port)
			}
			return nil
		}
		rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/ports/"+tp.port, "")
		if !firewallFirst || !routerFirst || !dhcpFirst {
			t.Errorf("converged while the record existed: firewall %v, router %v, DHCP %v; want all three", firewallFirst, routerFirst, dhcpFirst)
		}
		if exists(t, "ports", tp.port) {
			t.Error("the port's record is still there")
		}
	})

	t.Run("a failing unit keeps the record and converges the units from it again", func(t *testing.T) {
		rig := newDelRig(t)
		tp := rig.topology(t)
		rig.fw.failFirst(1, errUnitDown)
		rig.must(t, http.StatusInternalServerError, http.MethodDelete, "/v2.0/ports/"+tp.port, "")
		if !exists(t, "ports", tp.port) {
			t.Fatal("the port's record is gone; want it kept")
		}
		if _, ok := rig.fw.port(tp.port); !ok {
			t.Error("the compute node holds no firewall for the port after the failure; want it converged again")
		}
		if s, ok := rig.routers.state(tp.router); !ok || !forwardsFloating(&s, tp.floatingAddress) {
			t.Error("the router lost the port's floating IP; want it converged again")
		}
		if s, ok := rig.dhcp.state(tp.network); !ok || !servesHost(&s, tp.fixed) {
			t.Error("the network's DHCP lost the port's reservation; want it converged again")
		}
	})

	t.Run("a failing delete keeps the record and converges the units from it again", func(t *testing.T) {
		rig := newDelRig(t)
		tp := rig.topology(t)
		rig.api.pool = failingStatement{DB: testPool, statement: "DELETE FROM ports WHERE id = $1"}
		defer func() { rig.api.pool = testPool }()
		rig.must(t, http.StatusInternalServerError, http.MethodDelete, "/v2.0/ports/"+tp.port, "")
		if !exists(t, "ports", tp.port) {
			t.Fatal("the port's record is gone; want it kept")
		}
		if _, ok := rig.fw.port(tp.port); !ok {
			t.Error("the compute node holds no firewall for the port after the failure; want it converged again")
		}
		if s, ok := rig.routers.state(tp.router); !ok || !forwardsFloating(&s, tp.floatingAddress) {
			t.Error("the router lost the port's floating IP; want it converged again")
		}
		if s, ok := rig.dhcp.state(tp.network); !ok || !servesHost(&s, tp.fixed) {
			t.Error("the network's DHCP lost the port's reservation; want it converged again")
		}
	})
}

// failingStatement is a database whose transactions fail one statement: the fault a deletion's final
// transaction can meet after its units were converged away — a lost connection, say.
type failingStatement struct {
	database.DB
	statement string
}

func (f failingStatement) Begin(ctx context.Context) (database.Tx, error) {
	tx, err := f.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failingTx{Tx: tx, statement: f.statement}, nil
}

type failingTx struct {
	database.Tx
	statement string
}

func (t failingTx) Exec(ctx context.Context, sql string, args ...any) (database.Result, error) {
	if sql == t.statement {
		return nil, errors.New("the connection was lost")
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func TestDeletionOrder_Subnet(t *testing.T) {
	requireDB(t)
	rig := newDelRig(t)
	network := seedNetwork(t, "spec033-del-sub-"+uuid.NewString()[:6])
	subnet := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/subnets",
		`{"subnet":{"network_id":"`+network+`","ip_version":4,"cidr":"10.79.0.0/24"}}`), "subnet")

	// A port is given an address on the subnet while its DHCP converges without it: the final
	// transaction refuses the deletion, and the DHCP is converged with the subnet again.
	added := false
	rig.dhcp.hook = func(c holderCall[networking.DHCPState]) error {
		if c.ID == network && c.State == nil && !added {
			added = true
			if !exists(t, "subnets", subnet) {
				t.Error("the subnet's DHCP went after its record")
			}
			mustExec(t, `INSERT INTO ports (id, network_id, project_id, mac_address, status, fixed_ips)
				VALUES ($1, $2, $3, 'fa:16:3e:79:00:05', 'ACTIVE', $4::jsonb)`, uuid.NewString(), network, seededProjectID,
				`[{"subnet_id":"`+subnet+`","ip_address":"10.79.0.5"}]`)
		}
		return nil
	}
	code, body := rig.do(t, http.MethodDelete, "/v2.0/subnets/"+subnet, "")
	if code != http.StatusConflict || neutronErrorType(t, body) != "SubnetInUse" {
		t.Fatalf("delete with a port added meanwhile: code=%d body=%s; want 409 SubnetInUse", code, body)
	}
	if s, ok := rig.dhcp.state(network); !ok || len(s.Subnets) != 1 || s.Subnets[0].ID != subnet {
		t.Errorf("the network's DHCP after the refusal: %+v (held %v); want the subnet back", s.Subnets, ok)
	}
}

func TestDeletionOrder_Network(t *testing.T) {
	requireDB(t)
	rig := newDelRig(t)
	network := seedNetwork(t, "spec033-del-nw-"+uuid.NewString()[:6])
	rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/subnets",
		`{"subnet":{"network_id":"`+network+`","ip_version":4,"cidr":"10.80.0.0/24"}}`)

	added := false
	rig.dhcp.hook = func(c holderCall[networking.DHCPState]) error {
		if c.ID == network && c.State == nil && !added {
			added = true
			if !exists(t, "networks", network) {
				t.Error("the network's DHCP went after its record")
			}
			mustExec(t, `INSERT INTO ports (id, network_id, project_id, mac_address, status, fixed_ips)
				VALUES ($1, $2, $3, 'fa:16:3e:80:00:05', 'ACTIVE', '[]'::jsonb)`, uuid.NewString(), network, seededProjectID)
		}
		return nil
	}
	code, body := rig.do(t, http.MethodDelete, "/v2.0/networks/"+network, "")
	if code != http.StatusConflict || neutronErrorType(t, body) != "NetworkInUse" {
		t.Fatalf("delete with a port added meanwhile: code=%d body=%s; want 409 NetworkInUse", code, body)
	}
	if !exists(t, "networks", network) {
		t.Error("the network is gone; want it kept with the port added meanwhile")
	}
	if _, ok := rig.dhcp.state(network); !ok {
		t.Error("the network's DHCP after the refusal: absent; want it converged again")
	}
}

func TestDeletionOrder_Router(t *testing.T) {
	requireDB(t)

	t.Run("the router, with an interface added meanwhile", func(t *testing.T) {
		rig := newDelRig(t)
		router := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/routers",
			`{"router":{"name":"spec033-del-r","external_gateway_info":{"network_id":"`+rig.external+`"}}}`), "router")
		t.Cleanup(func() {
			mustExec(t, "DELETE FROM router_interfaces WHERE router_id = $1", router)
			mustExec(t, "DELETE FROM ports WHERE device_id = $1", router)
			mustExec(t, "DELETE FROM routers WHERE id = $1", router)
		})
		network := seedNetwork(t, "spec033-del-rn-"+uuid.NewString()[:6])
		gateway := "10.81.0.1"
		subnet := seedSubnet(t, network, "spec033-del-rs", "10.81.0.0/24", &gateway, 4, false)
		added := false
		rig.routers.hook = func(c routerCall) error {
			if c.ID == router && c.State == nil && !added {
				added = true
				if !exists(t, "routers", router) {
					t.Error("the router's namespace went after its record")
				}
				port := uuid.NewString()
				mustExec(t, `INSERT INTO ports (id, network_id, device_id, device_owner, mac_address, status, fixed_ips)
					VALUES ($1, $2, $3, 'network:router_interface', 'fa:16:3e:81:00:01', 'ACTIVE', $4::jsonb)`,
					port, network, router, `[{"subnet_id":"`+subnet+`","ip_address":"10.81.0.1"}]`)
				mustExec(t, "INSERT INTO router_interfaces (router_id, port_id, subnet_id) VALUES ($1, $2, $3)", router, port, subnet)
			}
			return nil
		}
		code, body := rig.do(t, http.MethodDelete, "/v2.0/routers/"+router, "")
		if code != http.StatusConflict {
			t.Fatalf("delete with an interface added meanwhile: code=%d body=%s; want 409", code, body)
		}
		if s, ok := rig.routers.state(router); !ok || len(s.Interfaces) != 1 {
			t.Errorf("the router after the refusal: %+v (held %v); want it converged with the interface", s.Interfaces, ok)
		}
	})

	t.Run("a gateway clear, with a floating IP added meanwhile", func(t *testing.T) {
		rig := newDelRig(t)
		tp := rig.topology(t)
		mustExec(t, "DELETE FROM floating_ips WHERE id = $1", tp.floatingIP)
		added := false
		rig.routers.hook = func(c routerCall) error {
			if c.ID == tp.router && c.State != nil && c.State.Gateway == nil && !added {
				added = true
				mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, fixed_ip_address, port_id, router_id, status)
					VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE')`, uuid.NewString(), seededProjectID, rig.external,
					rig.externalPrefix+".200", tp.fixed, tp.port, tp.router)
			}
			return nil
		}
		code, body := rig.do(t, http.MethodPut, "/v2.0/routers/"+tp.router, `{"router":{"external_gateway_info":{}}}`)
		if code != http.StatusConflict || neutronErrorType(t, body) != "RouterExternalGatewayInUseByFloatingIp" {
			t.Fatalf("clear with a floating IP added meanwhile: code=%d body=%s; want 409", code, body)
		}
		if s, ok := rig.routers.state(tp.router); !ok || s.Gateway == nil {
			t.Errorf("the router after the refusal: gateway %+v (held %v); want it converged with the gateway", s.Gateway, ok)
		}
	})

	t.Run("an interface and a port forwarding go from the router before their records", func(t *testing.T) {
		rig := newDelRig(t)
		tp := rig.topology(t)
		// A forwarding goes on a floating IP no port uses, to a port without one of its own, as upstream's
		// (SPEC-033 T192).
		other, otherFixed := createPortOn(t, rig.r, rig.hdr, tp.network)
		free := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips",
			`{"floatingip":{"floating_network_id":"`+rig.external+`"}}`), "floatingip")
		t.Cleanup(func() { mustExec(t, "DELETE FROM floating_ips WHERE id = $1", free) })
		pf := createdID(t, rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/floatingips/"+free+"/port_forwardings",
			`{"port_forwarding":{"internal_port_id":"`+other+`","internal_ip_address":"`+otherFixed+`","external_port":2201,"internal_port":22,"protocol":"tcp"}}`), "port_forwarding")
		first := false
		rig.routers.hook = func(c routerCall) error {
			if c.State != nil && len(c.State.PortForwardings) == 0 && !first {
				first = true
				if !exists(t, "port_forwardings", pf) {
					t.Error("the port forwarding left the router after its record")
				}
			}
			return nil
		}
		rig.must(t, http.StatusNoContent, http.MethodDelete, "/v2.0/floatingips/"+free+"/port_forwardings/"+pf, "")
		if !first {
			t.Error("the router never converged without the port forwarding")
		}

		mustExec(t, "DELETE FROM floating_ips WHERE id = $1", tp.floatingIP)
		var iface string
		if err := testPool.QueryRow(context.Background(), "SELECT port_id::text FROM router_interfaces WHERE router_id = $1", tp.router).Scan(&iface); err != nil {
			t.Fatal(err)
		}
		mustExec(t, "DELETE FROM ports WHERE id::text = ANY($1)", []string{tp.port, other}) // the interface's subnet must be free of them
		first = false
		rig.routers.hook = func(c routerCall) error {
			if c.State != nil && len(c.State.Interfaces) == 0 && !first {
				first = true
				if !exists(t, "ports", iface) {
					t.Error("the interface left the router after its port's record")
				}
			}
			return nil
		}
		rig.must(t, http.StatusOK, http.MethodPut, "/v2.0/routers/"+tp.router+"/remove_router_interface", `{"subnet_id":"`+tp.subnet+`"}`)
		if !first {
			t.Error("the router never converged without the interface")
		}
	})
}
