package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T104 (FR-058, FR-018; research R50, R52): an administrator updates, deletes and acts
// on another project's Networking API resources by id, and names them by id in a create, as upstream's
// admin_or_owner policy lets them; a member of the administrator's project gets 404 for each (lock). A
// resource an administrator creates belongs to the project the request names, or else to the
// administrator's own; a member naming another project is refused, as SPEC-024's rule has it.

// adminRequest is one request of a write case.
type adminRequest struct{ method, path, body string }

// adminWrite is a write an administrator makes on another project's resource. request prepares what
// the write needs — as the administrator, where that is a step of its own — and returns the request
// the member and then the administrator make. want is the administrator's answer. A create names the
// table and response key of what it creates, which is removed again; check, when set, reads what the
// administrator's write left.
type adminWrite struct {
	name       string
	request    func(t *testing.T, r *gin.Engine, own, theirs projectResources) adminRequest
	want       int
	table, key string
	check      func(t *testing.T, own, theirs projectResources, created string)
}

var (
	adminHdr  = map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	memberHdr = map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
)

func put(path, body string) adminRequest  { return adminRequest{http.MethodPut, path, body} }
func post(path, body string) adminRequest { return adminRequest{http.MethodPost, path, body} }
func del(path string) adminRequest        { return adminRequest{http.MethodDelete, path, ""} }

// createdID reads the id of the resource a create answered with, under key.
func createdID(t *testing.T, body, key string) string {
	t.Helper()
	var out map[string]map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	id, _ := out[key]["id"].(string)
	if id == "" {
		t.Fatalf("no %s id in %s", key, body)
	}
	return id
}

// adminCreate creates through the API as the administrator, removes it again when the test ends, and
// returns its id.
func adminCreate(t *testing.T, r *gin.Engine, path, body, table, key string) string {
	t.Helper()
	code, resp := doJSONReq(r, http.MethodPost, path, body, adminHdr)
	if code != http.StatusCreated {
		t.Fatalf("POST %s: code=%d body=%s; want 201", path, code, resp)
	}
	id := createdID(t, resp, key)
	t.Cleanup(func() { mustExec(t, "DELETE FROM "+table+" WHERE id = $1", id) })
	return id
}

// adminDo makes a preparatory request as the administrator.
func adminDo(t *testing.T, r *gin.Engine, req adminRequest, want int) {
	t.Helper()
	if code, body := doJSONReq(r, req.method, req.path, req.body, adminHdr); code != want {
		t.Fatalf("%s %s: code=%d body=%s; want %d", req.method, req.path, code, body, want)
	}
}

func seedBareNetwork(t *testing.T, project string) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `INSERT INTO networks (id, name, project_id, status) VALUES ($1, 'spec033-admin-bare', $2, 'ACTIVE')`, id, project)
	t.Cleanup(func() { mustExec(t, "DELETE FROM networks WHERE id = $1", id) })
	return id
}

func seedBareSubnet(t *testing.T, network, project string) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `INSERT INTO subnets (id, name, network_id, project_id, cidr) VALUES ($1, 'spec033-admin-bare', $2, $3, $4)`,
		id, network, project, fmt.Sprintf("10.58.%d.0/24", rand.IntN(250)))
	t.Cleanup(func() { mustExec(t, "DELETE FROM subnets WHERE id = $1", id) })
	return id
}

func seedBarePort(t *testing.T, network, project string) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-admin-bare', $2, $3, 'fa:16:3e:57:00:02', 'ACTIVE', '[]'::jsonb)`, id, network, project)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id = $1", id) })
	return id
}

// withGateway gives the project's subnet a gateway, the first address of its range, which a router
// interface needs.
func withGateway(t *testing.T, r projectResources) {
	t.Helper()
	gateway := netip.MustParsePrefix(r.subnetCIDR).Addr().Next().String()
	mustExec(t, "UPDATE subnets SET gateway_ip = $1 WHERE id = $2", gateway, r.subnet)
}

// projectOf reads the project of a row.
func projectOf(t *testing.T, table, id string) string {
	t.Helper()
	var project *string
	if err := testPool.QueryRow(context.Background(), "SELECT project_id::text FROM "+table+" WHERE id::text = $1", id).Scan(&project); err != nil {
		t.Fatalf("read the project of %s %s: %v", table, id, err)
	}
	if project == nil {
		return ""
	}
	return *project
}

func wantProject(t *testing.T, table, id, want string) {
	t.Helper()
	if got := projectOf(t, table, id); got != want {
		t.Errorf("%s %s belongs to %q; want %q", table, id, got, want)
	}
}

func wantGroups(t *testing.T, port string, want ...string) {
	t.Helper()
	if got := sgTestGroupsOf(t, port); !slices.Equal(got, want) {
		t.Errorf("port %s carries %v; want %v", port, got, want)
	}
}

// adminWrites are the update, delete and action handlers of R50, and the creates' references by id.
func adminWrites(external string) []adminWrite {
	type res = projectResources
	rename := func(kind string) string { return `{"` + kind + `":{"name":"spec033-admin-renamed"}}` }
	return []adminWrite{
		// Networks and subnets.
		{name: "update a network", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/networks/"+b.network, rename("network"))
		}},
		{name: "delete a network", want: http.StatusNoContent, request: func(t *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/networks/" + seedBareNetwork(t, b.project))
		}},
		{name: "update a subnet", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/subnets/"+b.subnet, rename("subnet"))
		}},
		{name: "delete a subnet", want: http.StatusNoContent, request: func(t *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/subnets/" + seedBareSubnet(t, b.network, b.project))
		}},
		{name: "create a subnet on another project's network", want: http.StatusCreated, table: "subnets", key: "subnet",
			request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
				return post("/v2.0/subnets", fmt.Sprintf(`{"subnet":{"network_id":"%s","cidr":"10.58.%d.0/24","ip_version":4}}`, b.network, rand.IntN(250)))
			}},
		// Ports and their groups.
		{name: "update a port", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/ports/"+b.port, rename("port"))
		}},
		{name: "delete a port", want: http.StatusNoContent, request: func(t *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/ports/" + seedBarePort(t, b.network, b.project))
		}},
		{name: "create a port on another project's network", want: http.StatusCreated, table: "ports", key: "port",
			request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
				return post("/v2.0/ports", `{"port":{"network_id":"`+b.network+`"}}`)
			},
			check: func(t *testing.T, _, _ res, created string) { wantProject(t, "ports", created, seededProjectID) }},
		{name: "put another project's group on an own port by id", want: http.StatusOK,
			request: func(_ *testing.T, _ *gin.Engine, a, b res) adminRequest {
				return put("/v2.0/ports/"+a.port, `{"port":{"security_groups":["`+b.group+`"]}}`)
			},
			check: func(t *testing.T, a, b res, _ string) { wantGroups(t, a.port, b.group) }},
		{name: "create a port with another project's group by id", want: http.StatusCreated, table: "ports", key: "port",
			request: func(_ *testing.T, _ *gin.Engine, a, b res) adminRequest {
				return post("/v2.0/ports", `{"port":{"network_id":"`+a.network+`","security_groups":["`+b.group+`"]}}`)
			},
			check: func(t *testing.T, _, b res, created string) { wantGroups(t, created, b.group) }},
		// Security groups and their rules.
		{name: "update a security group", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/security-groups/"+b.group, `{"security_group":{"description":"spec033-admin"}}`)
		}},
		{name: "delete a security group", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/security-groups/" + b.group)
		}},
		{name: "create a rule in another project's group", want: http.StatusCreated, table: "security_group_rules", key: "security_group_rule",
			request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
				return post("/v2.0/security-group-rules", `{"security_group_rule":{"security_group_id":"`+b.group+
					`","direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":80,"port_range_max":80}}`)
			}},
		{name: "delete a security group rule", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/security-group-rules/" + b.rule)
		}},
		// Routers, their gateways and interfaces.
		{name: "update a router", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/routers/"+b.router, rename("router"))
		}},
		{name: "set a router's gateway", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/routers/"+b.router, `{"router":{"external_gateway_info":{"network_id":"`+external+`"}}}`)
		}},
		{name: "delete a router", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/routers/" + b.router)
		}},
		{name: "add an interface to another project's router", want: http.StatusOK,
			request: func(t *testing.T, _ *gin.Engine, _, b res) adminRequest {
				withGateway(t, b)
				return put("/v2.0/routers/"+b.router+"/add_router_interface", `{"subnet_id":"`+b.subnet+`"}`)
			},
			// The interface's port is the router's project's, as upstream's (_add_interface_by_subnet).
			check: func(t *testing.T, _, b res, _ string) {
				var project string
				if err := testPool.QueryRow(context.Background(),
					"SELECT project_id::text FROM ports WHERE device_id = $1 AND device_owner = 'network:router_interface'", b.router,
				).Scan(&project); err != nil || project != b.project {
					t.Errorf("the interface's port belongs to %q (%v); want the router's project %s", project, err, b.project)
				}
			}},
		{name: "add an interface on another project's subnet to an own router", want: http.StatusOK,
			request: func(t *testing.T, _ *gin.Engine, a, b res) adminRequest {
				withGateway(t, b)
				return put("/v2.0/routers/"+a.router+"/add_router_interface", `{"subnet_id":"`+b.subnet+`"}`)
			}},
		{name: "remove an interface from another project's router", want: http.StatusOK,
			request: func(t *testing.T, r *gin.Engine, _, b res) adminRequest {
				withGateway(t, b)
				adminDo(t, r, put("/v2.0/routers/"+b.router+"/add_router_interface", `{"subnet_id":"`+b.subnet+`"}`), http.StatusOK)
				return put("/v2.0/routers/"+b.router+"/remove_router_interface", `{"subnet_id":"`+b.subnet+`"}`)
			}},
		// Floating IPs and their port forwardings.
		{name: "update a floating IP", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/floatingips/"+b.floatingIP, `{"floatingip":{"description":"spec033-admin"}}`)
		}},
		{name: "delete a floating IP", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/floatingips/" + b.floatingIP)
		}},
		{name: "create a floating IP on another project's port", want: http.StatusCreated, table: "floating_ips", key: "floatingip",
			request: func(t *testing.T, _ *gin.Engine, _, b res) adminRequest {
				withGateway(t, b)
				seedRouterOn(t, b.network, b.subnet, external)
				return post("/v2.0/floatingips", `{"floatingip":{"floating_network_id":"`+external+`","port_id":"`+b.port+`"}}`)
			},
			check: func(t *testing.T, _, _ res, created string) { wantProject(t, "floating_ips", created, seededProjectID) }},
		{name: "update a port forwarding", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/floatingips/"+b.floatingIP+"/port_forwardings/"+b.portForwarding, `{"port_forwarding":{"description":"spec033-admin"}}`)
		}},
		{name: "delete a port forwarding", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/floatingips/" + b.floatingIP + "/port_forwardings/" + b.portForwarding)
		}},
		{name: "create a port forwarding on another project's floating IP", want: http.StatusCreated, table: "port_forwardings", key: "port_forwarding",
			request: func(t *testing.T, _ *gin.Engine, _, b res) adminRequest {
				// The router joins the port's subnet to the floating network, which a forwarding needs
				// (SPEC-033 FR-049, T256): its gateway port on the external network and its interface.
				joinRouter(t, b, external)
				mustExec(t, "UPDATE floating_ips SET router_id = $1 WHERE id = $2", b.router, b.floatingIP)
				return post("/v2.0/floatingips/"+b.floatingIP+"/port_forwardings", `{"port_forwarding":{"internal_port_id":"`+b.port+
					`","internal_ip_address":"`+b.address+`","external_port":1500,"internal_port":22,"protocol":"tcp"}}`)
			},
			// The floating IP's project's, whose owner lists and changes it.
			check: func(t *testing.T, _, b res, created string) { wantProject(t, "port_forwardings", created, b.project) }},
		// QoS policies and their bandwidth limit rules.
		{name: "update a QoS policy", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/qos/policies/"+b.qosPolicy, `{"policy":{"description":"spec033-admin"}}`)
		}},
		{name: "delete a QoS policy", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/qos/policies/" + b.qosPolicy)
		}},
		{name: "create a bandwidth limit rule in another project's policy", want: http.StatusCreated,
			table: "qos_bandwidth_limit_rules", key: "bandwidth_limit_rule",
			request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
				return post("/v2.0/qos/policies/"+b.qosPolicy+"/bandwidth_limit_rules", `{"bandwidth_limit_rule":{"max_kbps":1000}}`)
			}},
		{name: "update a bandwidth limit rule", want: http.StatusOK, request: func(t *testing.T, r *gin.Engine, _, b res) adminRequest {
			path := "/v2.0/qos/policies/" + b.qosPolicy + "/bandwidth_limit_rules"
			rule := adminCreate(t, r, path, `{"bandwidth_limit_rule":{"max_kbps":1000}}`, "qos_bandwidth_limit_rules", "bandwidth_limit_rule")
			return put(path+"/"+rule, `{"bandwidth_limit_rule":{"max_kbps":2000}}`)
		}},
		{name: "delete a bandwidth limit rule", want: http.StatusNoContent, request: func(t *testing.T, r *gin.Engine, _, b res) adminRequest {
			path := "/v2.0/qos/policies/" + b.qosPolicy + "/bandwidth_limit_rules"
			rule := adminCreate(t, r, path, `{"bandwidth_limit_rule":{"max_kbps":1000}}`, "qos_bandwidth_limit_rules", "bandwidth_limit_rule")
			return del(path + "/" + rule)
		}},
		// RBAC policies.
		{name: "update an RBAC policy", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/rbac-policies/"+b.rbac, `{"rbac_policy":{"target_tenant":"`+seededProjectID+`"}}`)
		}},
		{name: "delete an RBAC policy", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/rbac-policies/" + b.rbac)
		}},
		// Trunks and their subports.
		{name: "update a trunk", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/trunks/"+b.trunk, rename("trunk"))
		}},
		{name: "delete a trunk", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/trunks/" + b.trunk)
		}},
		{name: "add a subport to another project's trunk", want: http.StatusOK, request: func(t *testing.T, _ *gin.Engine, _, b res) adminRequest {
			sub := seedBarePort(t, b.network, b.project)
			return put("/v2.0/trunks/"+b.trunk+"/add_subports", `{"sub_ports":[{"port_id":"`+sub+`","segmentation_type":"vlan","segmentation_id":101}]}`)
		}},
		{name: "remove a subport from another project's trunk", want: http.StatusOK, request: func(t *testing.T, r *gin.Engine, _, b res) adminRequest {
			sub := seedBarePort(t, b.network, b.project)
			adminDo(t, r, put("/v2.0/trunks/"+b.trunk+"/add_subports", `{"sub_ports":[{"port_id":"`+sub+`","segmentation_type":"vlan","segmentation_id":101}]}`), http.StatusOK)
			return put("/v2.0/trunks/"+b.trunk+"/remove_subports", `{"sub_ports":[{"port_id":"`+sub+`"}]}`)
		}},
		// Address scopes and subnet pools.
		{name: "update an address scope", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/address-scopes/"+b.addressScope, rename("address_scope"))
		}},
		{name: "delete an address scope", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/address-scopes/" + b.addressScope)
		}},
		{name: "update a subnet pool", want: http.StatusOK, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return put("/v2.0/subnetpools/"+b.subnetPool, rename("subnetpool"))
		}},
		{name: "delete a subnet pool", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/subnetpools/" + b.subnetPool)
		}},
		// Metering labels and their rules.
		{name: "delete a metering label", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/metering/metering-labels/" + b.label)
		}},
		{name: "create a rule in another project's metering label", want: http.StatusCreated, table: "metering_label_rules", key: "metering_label_rule",
			request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
				return post("/v2.0/metering/metering-label-rules", `{"metering_label_rule":{"metering_label_id":"`+b.label+`","remote_ip_prefix":"10.1.0.0/16"}}`)
			}},
		{name: "delete a metering label rule", want: http.StatusNoContent, request: func(_ *testing.T, _ *gin.Engine, _, b res) adminRequest {
			return del("/v2.0/metering/metering-label-rules/" + b.labelRule)
		}},
	}
}

func TestAdministratorWritesEveryProject(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	other := createTestProject(t, "spec033-admin-w")
	short := uuid.NewString()[:6]
	external := seedExternalNetwork(t, "spec033-admin-wext-"+short, seededProjectID, false, true)
	seedSubnet(t, external, "spec033-admin-wext-"+short, fmt.Sprintf("198.19.%d.0/24", rand.IntN(256)), nil, 4, false)
	own := seedProjectResources(t, seededProjectID, external)

	for _, w := range adminWrites(external) {
		t.Run(w.name, func(t *testing.T) {
			theirs := seedProjectResources(t, other, external)
			req := w.request(t, r, own, theirs)
			if code, body := doJSONReq(r, req.method, req.path, req.body, memberHdr); code != http.StatusNotFound {
				t.Fatalf("member: %s %s: code=%d body=%s; want 404 (lock)", req.method, req.path, code, body)
			}
			code, body := doJSONReq(r, req.method, req.path, req.body, adminHdr)
			if code != w.want {
				t.Fatalf("administrator: %s %s: code=%d body=%s; want %d", req.method, req.path, code, body, w.want)
			}
			var created string
			if w.table != "" {
				created = createdID(t, body, w.key)
				t.Cleanup(func() { mustExec(t, "DELETE FROM "+w.table+" WHERE id = $1", created) })
			}
			if w.check != nil {
				w.check(t, own, theirs, created)
			}
		})
	}
}

// ownedCreate is a create whose resource an administrator may give to another project.
type ownedCreate struct {
	name, path, key, table string
	attrs                  func(t *testing.T, own projectResources) map[string]any
}

func ownedCreates(external string) []ownedCreate {
	random := func() string { return uuid.NewString()[:8] }
	return []ownedCreate{
		{name: "network", path: "/v2.0/networks", key: "network", table: "networks",
			attrs: func(*testing.T, projectResources) map[string]any {
				return map[string]any{"name": "spec033-own-" + random()}
			}},
		{name: "subnet", path: "/v2.0/subnets", key: "subnet", table: "subnets",
			attrs: func(_ *testing.T, a projectResources) map[string]any {
				return map[string]any{"network_id": a.network, "ip_version": 4, "cidr": fmt.Sprintf("10.59.%d.0/24", rand.IntN(250))}
			}},
		{name: "port", path: "/v2.0/ports", key: "port", table: "ports",
			attrs: func(_ *testing.T, a projectResources) map[string]any { return map[string]any{"network_id": a.network} }},
		{name: "security group", path: "/v2.0/security-groups", key: "security_group", table: "security_groups",
			attrs: func(*testing.T, projectResources) map[string]any {
				return map[string]any{"name": "spec033-own-" + random()}
			}},
		{name: "router", path: "/v2.0/routers", key: "router", table: "routers",
			attrs: func(*testing.T, projectResources) map[string]any {
				return map[string]any{"name": "spec033-own-" + random()}
			}},
		{name: "floating IP", path: "/v2.0/floatingips", key: "floatingip", table: "floating_ips",
			attrs: func(*testing.T, projectResources) map[string]any {
				return map[string]any{"floating_network_id": external}
			}},
		{name: "QoS policy", path: "/v2.0/qos/policies", key: "policy", table: "qos_policies",
			attrs: func(*testing.T, projectResources) map[string]any {
				return map[string]any{"name": "spec033-own-" + random()}
			}},
		{name: "RBAC policy", path: "/v2.0/rbac-policies", key: "rbac_policy", table: "rbac_policies",
			attrs: func(_ *testing.T, a projectResources) map[string]any {
				return map[string]any{"object_type": "network", "object_id": a.network, "target_tenant": a.project, "action": "access_as_shared"}
			}},
		{name: "trunk", path: "/v2.0/trunks", key: "trunk", table: "trunks",
			attrs: func(t *testing.T, a projectResources) map[string]any {
				return map[string]any{"port_id": seedBarePort(t, a.network, a.project)}
			}},
		{name: "address scope", path: "/v2.0/address-scopes", key: "address_scope", table: "address_scopes",
			attrs: func(*testing.T, projectResources) map[string]any {
				return map[string]any{"name": "spec033-own-" + random(), "ip_version": 4}
			}},
		{name: "subnet pool", path: "/v2.0/subnetpools", key: "subnetpool", table: "subnet_pools",
			attrs: func(*testing.T, projectResources) map[string]any {
				return map[string]any{"name": "spec033-own-" + random(), "prefixes": []string{fmt.Sprintf("10.%d.0.0/16", 100+rand.IntN(100))}}
			}},
		{name: "metering label", path: "/v2.0/metering/metering-labels", key: "metering_label", table: "metering_labels",
			attrs: func(*testing.T, projectResources) map[string]any {
				return map[string]any{"name": "spec033-own-" + random()}
			}},
	}
}

func TestAdministratorCreatesForANamedProject(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	other := createTestProject(t, "spec033-admin-c")
	short := uuid.NewString()[:6]
	external := seedExternalNetwork(t, "spec033-admin-cext-"+short, seededProjectID, false, true)
	seedSubnet(t, external, "spec033-admin-cext-"+short, fmt.Sprintf("198.19.%d.0/24", rand.IntN(256)), nil, 4, false)
	own := seedProjectResources(t, seededProjectID, external)

	body := func(t *testing.T, c ownedCreate, project string) string {
		attrs := c.attrs(t, own)
		if project != "" {
			attrs["project_id"] = project
		}
		raw, _ := json.Marshal(map[string]any{c.key: attrs})
		return string(raw)
	}
	for _, c := range ownedCreates(external) {
		t.Run(c.name, func(t *testing.T) {
			if code, resp := doJSONReq(r, http.MethodPost, c.path, body(t, c, other), memberHdr); code != http.StatusBadRequest {
				t.Errorf("member naming another project: code=%d body=%s; want 400", code, resp)
			}
			for _, tc := range []struct{ named, want string }{{other, other}, {"", seededProjectID}} {
				code, resp := doJSONReq(r, http.MethodPost, c.path, body(t, c, tc.named), adminHdr)
				if code != http.StatusCreated {
					t.Fatalf("administrator naming %q: code=%d body=%s; want 201", tc.named, code, resp)
				}
				id := createdID(t, resp, c.key)
				t.Cleanup(func() { mustExec(t, "DELETE FROM "+c.table+" WHERE id = $1", id) })
				wantProject(t, c.table, id, tc.want)
			}
		})
	}
}

// joinRouter gives b's router a gateway port on external and an interface on b's subnet, as the router code
// writes them.
func joinRouter(t *testing.T, b projectResources, external string) {
	t.Helper()
	gateway, iface := uuid.NewString(), uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, device_id, device_owner, mac_address, status, fixed_ips)
		VALUES ($1, '', $2, $3, 'network:router_gateway', 'fa:16:3e:57:00:02', 'ACTIVE', '[]'::jsonb)`, gateway, external, b.router)
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
		VALUES ($1, '', $2, $3, $4, 'network:router_interface', 'fa:16:3e:57:00:03', 'ACTIVE', $5::jsonb)`,
		iface, b.network, b.project, b.router, `[{"subnet_id":"`+b.subnet+`","ip_address":"`+strings.TrimSuffix(b.address, ".5")+`.1"}]`)
	mustExec(t, `INSERT INTO router_interfaces (router_id, port_id, subnet_id) VALUES ($1, $2, $3)`, b.router, iface, b.subnet)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id IN ($1, $2)", gateway, iface) })
}
