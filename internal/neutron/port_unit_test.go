package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T127 (FR-008, FR-041, FR-042, FR-045, FR-047; research R27, R32): a port's
// firewall lives on the node it is bound to. A membership change, a rule added or deleted, a change of
// a remote group's members and a binding change each converge the affected ports' firewall on their
// nodes before the answer, one call per node; a failure answers 500 and is undone. An unbound port has
// none; a binding that moves takes the port's chains from the old node and gives them to the new one.

const (
	portAPINode = "spec033-papi"
	portNode1   = "spec033-cmp1"
	portNode2   = "spec033-cmp2"
)

// portRig is three nodes on one database: the API node, which answers, and two compute nodes holding
// bound ports. The fake node caller carries the units between them.
type portRig struct {
	r            *gin.Engine
	api          *Service
	nodes        map[string]*Service
	firewalls    map[string]*fakeFirewall
	caller       *fakeNodeCaller
	member       map[string]string
	admin        map[string]string
	network      string
	subnetPrefix string
}

func newPortRig(t *testing.T, mode string) *portRig {
	t.Helper()
	rig := &portRig{nodes: map[string]*Service{}, firewalls: map[string]*fakeFirewall{},
		member: map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"},
		admin:  map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}}
	var services []*Service
	for _, name := range []string{portAPINode, portNode1, portNode2} {
		var r *gin.Engine
		var svc *Service
		if mode == "stub" {
			r, svc = newNeutronTestRouterWithService()
		} else {
			r, svc = newNeutronTestRouterInMode(mode)
		}
		svc.SetNodes(NodeOptions{Name: name, NetworkNode: portAPINode})
		// Every seam that reaches the host is the test's, the bridges included.
		svc.dhcp, svc.routers, svc.brManager = newFakeDHCP(), newFakeRouters(), networking.NewBridgeManager("stub")
		rig.firewalls[name] = newFakeFirewall()
		svc.firewall = rig.firewalls[name]
		rig.nodes[name] = svc
		services = append(services, svc)
		if name == portAPINode {
			rig.r, rig.api = r, svc
		}
	}
	rig.caller = newFakeNodeCaller(services...)
	for _, svc := range services {
		svc.caller = rig.caller
	}
	rig.network = seedNetwork(t, "spec033-port-"+uuid.NewString()[:6])
	rig.subnetPrefix = "10.88.0"
	seedSubnet(t, rig.network, "spec033-port", rig.subnetPrefix+".0/24", nil, 4, false)
	return rig
}

// newNeutronTestRouterInMode is newNeutronTestRouterWithService in another networking mode.
func newNeutronTestRouterInMode(mode string) (*gin.Engine, *Service) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ErrorEnvelope())
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	svc := NewService(testPool, mode, nil)
	svc.RegisterNodeRoutes(&r.RouterGroup)
	svc.RegisterRoutes(&r.RouterGroup)
	return r, svc
}

func (rig *portRig) must(t *testing.T, hdr map[string]string, want int, method, path, body string) string {
	t.Helper()
	code, resp := doJSONReq(rig.r, method, path, body, hdr)
	if code != want {
		t.Fatalf("%s %s: code=%d body=%s; want %d", method, path, code, resp, want)
	}
	return resp
}

// group creates a security group with the given rules (JSON objects without the group's id).
func (rig *portRig) group(t *testing.T, rules ...string) string {
	t.Helper()
	id := createdID(t, rig.must(t, rig.member, http.StatusCreated, http.MethodPost, "/v2.0/security-groups",
		`{"security_group":{"name":"spec033-port-`+uuid.NewString()[:6]+`"}}`), "security_group")
	t.Cleanup(func() { mustExec(t, "DELETE FROM security_groups WHERE id = $1", id) })
	for _, r := range rules {
		rig.rule(t, id, r)
	}
	return id
}

func (rig *portRig) rule(t *testing.T, group, rule string) string {
	t.Helper()
	body := `{"security_group_rule":{"security_group_id":"` + group + `",` + strings.TrimPrefix(rule, "{") + `}`
	return createdID(t, rig.must(t, rig.member, http.StatusCreated, http.MethodPost, "/v2.0/security-group-rules", body), "security_group_rule")
}

// port creates a port on the rig's network with the given groups and returns it and its address.
func (rig *portRig) port(t *testing.T, groups ...string) (string, string) {
	t.Helper()
	list, _ := json.Marshal(groups)
	resp := rig.must(t, rig.member, http.StatusCreated, http.MethodPost, "/v2.0/ports",
		`{"port":{"network_id":"`+rig.network+`","security_groups":`+string(list)+`}}`)
	var out struct {
		Port struct {
			ID       string          `json:"id"`
			FixedIPs common.FixedIPs `json:"fixed_ips"`
		} `json:"port"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.Port.ID == "" || len(out.Port.FixedIPs) == 0 {
		t.Fatalf("no port and address in %s (%v)", resp, err)
	}
	return out.Port.ID, out.Port.FixedIPs[0].IPAddress
}

// bind records a port's binding, as Nova's boot would before converging it.
func bind(t *testing.T, port, node string) {
	t.Helper()
	mustExec(t, "UPDATE ports SET binding_host_id = $1 WHERE id = $2", node, port)
}

func (rig *portRig) setGroups(t *testing.T, want int, port string, groups ...string) {
	t.Helper()
	list, _ := json.Marshal(groups)
	rig.must(t, rig.member, want, http.MethodPut, "/v2.0/ports/"+port, `{"port":{"security_groups":`+string(list)+`}}`)
}

// held is a port as a node's firewall holds it.
func (rig *portRig) held(t *testing.T, node, port string) networking.FirewallPort {
	t.Helper()
	p, ok := rig.firewalls[node].port(port)
	if !ok {
		t.Fatalf("%s holds no firewall for port %s", node, port)
	}
	return p
}

// allows reports whether a port's firewall holds a rule of direction, protocol and first port, from
// remote ("" for any).
func allows(p networking.FirewallPort, direction, protocol string, portMin int, remote string) bool {
	for _, r := range p.Rules {
		if r.Direction == direction && r.Protocol == protocol && r.PortMin != nil && *r.PortMin == portMin && r.Remote == remote {
			return true
		}
	}
	return false
}

// reaches reports whether a port's firewall lets remote in, by a rule naming it alone.
func reaches(p networking.FirewallPort, remote string) bool {
	for _, r := range p.Rules {
		if r.Direction == "ingress" && r.Remote == remote {
			return true
		}
	}
	return false
}

// callsTo is how many units the rig's caller carried to node.
func (rig *portRig) callsTo(node string) int {
	n := 0
	for _, c := range rig.caller.recorded() {
		if c.Node == node && c.Req.Unit == unitPorts {
			n++
		}
	}
	return n
}

const (
	ruleSSH = `{"direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":22,"port_range_max":22}`
	ruleWeb = `{"direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":80,"port_range_max":80}`
)

func TestPortUnitMembershipChange(t *testing.T) {
	requireDB(t)
	rig := newPortRig(t, "stub")
	g1, g2 := rig.group(t, ruleSSH), rig.group(t, ruleWeb)
	p, _ := rig.port(t, g1)
	bind(t, p, portNode1)

	before := rig.callsTo(portNode1)
	rig.setGroups(t, http.StatusOK, p, g2)
	held := rig.held(t, portNode1, p)
	if !allows(held, "ingress", "tcp", 80, "") || allows(held, "ingress", "tcp", 22, "") {
		t.Errorf("port %s's firewall after the change: %+v; want port 80's rule and not port 22's", p, held.Rules)
	}
	if n := rig.callsTo(portNode1) - before; n != 1 {
		t.Errorf("%d calls to the port's node; want one", n)
	}
	if rig.firewalls[portNode2].converged() != 0 || rig.firewalls[portAPINode].converged() != 0 {
		t.Error("a node the port is not bound to converged its firewall")
	}
}

func TestPortUnitRuleChanges(t *testing.T) {
	requireDB(t)
	rig := newPortRig(t, "stub")
	g := rig.group(t)
	p, _ := rig.port(t, g)
	bind(t, p, portNode1)

	dns := rig.rule(t, g, `{"direction":"ingress","ethertype":"IPv4","protocol":"udp","port_range_min":53,"port_range_max":53}`)
	if !allows(rig.held(t, portNode1, p), "ingress", "udp", 53, "") {
		t.Errorf("a rule added: %+v; want it held before the 201", rig.held(t, portNode1, p).Rules)
	}
	rig.must(t, rig.member, http.StatusNoContent, http.MethodDelete, "/v2.0/security-group-rules/"+dns, "")
	if allows(rig.held(t, portNode1, p), "ingress", "udp", 53, "") {
		t.Errorf("a rule deleted: %+v; want it gone before the 204", rig.held(t, portNode1, p).Rules)
	}
}

func TestPortUnitRemoteGroupMembers(t *testing.T) {
	requireDB(t)
	rig := newPortRig(t, "stub")
	members, other := rig.group(t), rig.group(t)
	referrer := rig.group(t, `{"direction":"ingress","ethertype":"IPv4","remote_group_id":"`+members+`"}`)
	q, qAddress := rig.port(t, referrer, members)
	bind(t, q, portNode2)

	r, rAddress := rig.port(t, other)
	rig.setGroups(t, http.StatusOK, r, members)
	if !reaches(rig.held(t, portNode2, q), rAddress+"/32") && !reaches(rig.held(t, portNode2, q), rAddress) {
		t.Errorf("a port joined the remote group: %+v; want its address let in", rig.held(t, portNode2, q).Rules)
	}
	if reaches(rig.held(t, portNode2, q), qAddress) || reaches(rig.held(t, portNode2, q), qAddress+"/32") {
		t.Error("a port's own address is expanded into its rules; upstream leaves it out")
	}
	rig.setGroups(t, http.StatusOK, r, other)
	if reaches(rig.held(t, portNode2, q), rAddress) || reaches(rig.held(t, portNode2, q), rAddress+"/32") {
		t.Errorf("a port left the remote group: %+v; want its address gone", rig.held(t, portNode2, q).Rules)
	}

	// A port created into the group adds an address too.
	_, sAddress := rig.port(t, members)
	if !reaches(rig.held(t, portNode2, q), sAddress) && !reaches(rig.held(t, portNode2, q), sAddress+"/32") {
		t.Errorf("a port created in the remote group: %+v; want its address let in", rig.held(t, portNode2, q).Rules)
	}
}

func TestPortUnitBinding(t *testing.T) {
	requireDB(t)
	rig := newPortRig(t, "stub")
	p, _ := rig.port(t, rig.group(t, ruleSSH))
	ctx := context.Background()
	binding := func() string {
		var node *string
		if err := testPool.QueryRow(ctx, "SELECT binding_host_id FROM ports WHERE id = $1", p).Scan(&node); err != nil {
			t.Fatal(err)
		}
		if node == nil {
			return ""
		}
		return *node
	}
	if _, ok := rig.firewalls[portNode1].port(p); ok {
		t.Fatal("an unbound port has a firewall")
	}

	if err := rig.api.BindPort(ctx, p, portNode1); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if binding() != portNode1 || !allows(rig.held(t, portNode1, p), "ingress", "tcp", 22, "") {
		t.Errorf("bound to %q; want %s holding the port's firewall", binding(), portNode1)
	}

	before := len(rig.caller.recorded())
	if err := rig.api.BindPort(ctx, p, portNode2); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, ok := rig.firewalls[portNode1].port(p); ok {
		t.Error("the old node still holds the port's firewall after the binding moved")
	}
	if binding() != portNode2 || !allows(rig.held(t, portNode2, p), "ingress", "tcp", 22, "") {
		t.Errorf("moved to %q; want %s holding the port's firewall", binding(), portNode2)
	}
	calls := rig.caller.recorded()[before:]
	if len(calls) != 2 {
		t.Fatalf("calls for the move: %+v; want one to each node", calls)
	}
	for _, c := range calls {
		switch c.Node {
		case portNode1:
			// The records no longer bind the port there; it is not absent, as it stays a member of its
			// groups.
			if len(c.Req.IDs)+len(c.Req.AbsentIDs) != 0 {
				t.Errorf("the old node's call %+v; want it converging from the records, naming no port", c.Req)
			}
		case portNode2:
			if len(c.Req.IDs) != 1 || c.Req.IDs[0] != p {
				t.Errorf("the new node's call %+v; want the port present", c.Req)
			}
		default:
			t.Errorf("a call to %s for the move", c.Node)
		}
	}

	if err := rig.api.BindPort(ctx, p, ""); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if _, ok := rig.firewalls[portNode2].port(p); ok || binding() != "" {
		t.Errorf("unbound: binding %q, the node holding the port %v; want neither", binding(), ok)
	}

	// The binding is an administrator's to read, as upstream's policy has it.
	if err := rig.api.BindPort(ctx, p, portNode1); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		hdr  map[string]string
		want bool
	}{{rig.admin, true}, {rig.member, false}} {
		var out struct {
			Port map[string]any `json:"port"`
		}
		_ = json.Unmarshal([]byte(rig.must(t, c.hdr, http.StatusOK, http.MethodGet, "/v2.0/ports/"+p, "")), &out)
		host, shown := out.Port["binding:host_id"]
		if shown != c.want || (c.want && host != portNode1) {
			t.Errorf("binding:host_id for roles %s: %v (shown %v); want shown %v", c.hdr["X-Test-Roles"], host, shown, c.want)
		}
	}
}

func TestPortUnitFailuresUndo(t *testing.T) {
	requireDB(t)
	errNode := errors.New("the node could not converge its ports")
	ctx := context.Background()

	t.Run("a membership change is undone", func(t *testing.T) {
		rig := newPortRig(t, "stub")
		g1, g2 := rig.group(t, ruleSSH), rig.group(t, ruleWeb)
		p, _ := rig.port(t, g1)
		bind(t, p, portNode1)
		rig.firewalls[portNode1].failFirst(1, errNode)
		rig.setGroups(t, http.StatusInternalServerError, p, g2)
		groups, err := PortSecurityGroupIDs(ctx, testPool, p)
		if err != nil || len(groups) != 1 || groups[0] != g1 {
			t.Errorf("the port's groups after the undo: %v, %v; want %s back", groups, err, g1)
		}
		if !allows(rig.held(t, portNode1, p), "ingress", "tcp", 22, "") {
			t.Errorf("the port's firewall after the undo: %+v; want it converged back", rig.held(t, portNode1, p).Rules)
		}
	})

	t.Run("a rule's create is undone", func(t *testing.T) {
		rig := newPortRig(t, "stub")
		g := rig.group(t)
		p, _ := rig.port(t, g)
		bind(t, p, portNode1)
		rig.firewalls[portNode1].failFirst(1, errNode)
		body := `{"security_group_rule":{"security_group_id":"` + g + `","direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":25,"port_range_max":25}}`
		rig.must(t, rig.member, http.StatusInternalServerError, http.MethodPost, "/v2.0/security-group-rules", body)
		var n int
		if err := testPool.QueryRow(ctx, "SELECT count(*) FROM security_group_rules WHERE security_group_id = $1 AND port_range_min = 25", g).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d rule(s) left (%v); want the create undone", n, err)
		}
	})

	t.Run("a binding is undone", func(t *testing.T) {
		rig := newPortRig(t, "stub")
		p, _ := rig.port(t, rig.group(t, ruleSSH))
		rig.firewalls[portNode1].failFirst(1, errNode)
		if err := rig.api.BindPort(ctx, p, portNode1); err == nil {
			t.Fatal("a failing node: the binding stood")
		}
		var node *string
		if err := testPool.QueryRow(ctx, "SELECT binding_host_id FROM ports WHERE id = $1", p).Scan(&node); err != nil || node != nil {
			t.Errorf("the binding after the undo: %v, %v; want none", node, err)
		}
	})
}

func TestPortUnitUnboundPortHasNoChains(t *testing.T) {
	requireDB(t)
	rig := newPortRig(t, "stub")
	g1, g2 := rig.group(t, ruleSSH), rig.group(t, ruleWeb)
	u, _ := rig.port(t, g1)
	rig.setGroups(t, http.StatusOK, u, g2)
	for name, fw := range rig.firewalls {
		if _, ok := fw.port(u); ok {
			t.Errorf("%s holds a firewall for an unbound port", name)
		}
	}
}

// The ebpf mode runs the same chains (FR-047), and Neutron no longer makes a TAP of its own, nor
// attaches eBPF to one (FR-045): a port create touches no device, so this non-root run answers 201.
func TestPortUnitEBPFMode(t *testing.T) {
	requireDB(t)
	rig := newPortRig(t, "ebpf")
	g1, g2 := rig.group(t, ruleSSH), rig.group(t, ruleWeb)
	p, _ := rig.port(t, g1)
	bind(t, p, portNode1)
	rig.setGroups(t, http.StatusOK, p, g2)
	if !allows(rig.held(t, portNode1, p), "ingress", "tcp", 80, "") {
		t.Errorf("ebpf mode: %+v; want the change converged as in iptables mode", rig.held(t, portNode1, p).Rules)
	}
}

// A network's connection-tracking zone is assigned with it: the lowest free from 4097, as upstream's
// bridge driver numbers them.
func TestNetworkCreateAssignsTheLowestFreeZone(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	code, network := createNetwork(t, r, hdr, `{"network":{"name":"spec033-zone"}}`)
	if code != http.StatusCreated {
		t.Fatalf("create network: %d %v", code, network)
	}
	var zone *int
	if err := testPool.QueryRow(context.Background(), "SELECT ct_zone FROM networks WHERE id = $1", network["id"]).Scan(&zone); err != nil || zone == nil {
		t.Fatalf("the network's zone: %v, %v; want one assigned", zone, err)
	}
	var below int
	if err := testPool.QueryRow(context.Background(),
		"SELECT count(*) FROM networks WHERE ct_zone >= 4097 AND ct_zone < $1", *zone).Scan(&below); err != nil {
		t.Fatal(err)
	}
	if *zone < 4097 || below != *zone-4097 {
		t.Errorf("zone %d with %d zones taken below it; want the lowest free from 4097", *zone, below)
	}
}
