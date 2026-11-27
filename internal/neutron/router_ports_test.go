package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T093 (FR-038, FR-045, FR-055; research R31): a router's gateway is a port of the
// external network, as upstream's: network:router_gateway, no project, an address from the pools or the
// one an administrator requests, reported in external_gateway_info.external_fixed_ips. An interface's
// port and its router_interfaces row are stored in one transaction, a subnet the router already has or
// one without a gateway answers 400, and the gateway port is visible to administrators and the external
// network's owner only, as upstream's get_port (SPEC-033 T174).

type fixedIPOut struct {
	SubnetID  string `json:"subnet_id"`
	IPAddress string `json:"ip_address"`
}

type gatewayOut struct {
	NetworkID        string       `json:"network_id"`
	EnableSNAT       *bool        `json:"enable_snat"`
	ExternalFixedIPs []fixedIPOut `json:"external_fixed_ips"`
}

// routerRig is an external network with one subnet whose pools are .10–.20, and request headers.
type routerRig struct {
	r                 *gin.Engine
	admin, member     map[string]string
	external, subnet  string
	prefix            string
	gatewayOfExternal string
}

func newRouterRig(t *testing.T) *routerRig {
	t.Helper()
	rr := &routerRig{
		r:      newNeutronTestRouter(),
		admin:  map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"},
		member: map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"},
		prefix: fmt.Sprintf("198.18.%d", rand.IntN(250)),
	}
	rr.external = seedExternalNetwork(t, "spec033-ext-"+uuid.NewString()[:6], seededProjectID, false, true)
	rr.gatewayOfExternal = rr.prefix + ".1"
	rr.subnet = seedSubnet(t, rr.external, "spec033-ext-subnet", rr.prefix+".0/24", &rr.gatewayOfExternal, 4, false)
	storeTestPools(t, rr.subnet, [2]string{rr.prefix + ".10", rr.prefix + ".20"})
	return rr
}

// createRouter creates a router through the API with the given external_gateway_info JSON ("" for
// none) and returns status, body and id; the router is deleted in cleanup.
func (rr *routerRig) createRouter(t *testing.T, hdr map[string]string, gateway string) (int, string, string) {
	t.Helper()
	body := `{"router":{"name":"spec033-router"}}`
	if gateway != "" {
		body = `{"router":{"name":"spec033-router","external_gateway_info":` + gateway + `}}`
	}
	code, resp := doJSONReq(rr.r, http.MethodPost, "/v2.0/routers", body, hdr)
	var out struct {
		Router struct {
			ID string `json:"id"`
		} `json:"router"`
	}
	_ = json.Unmarshal([]byte(resp), &out)
	if id := out.Router.ID; id != "" {
		t.Cleanup(func() {
			mustExec(t, "DELETE FROM ports WHERE device_id = $1", id)
			mustExec(t, "DELETE FROM routers WHERE id = $1", id)
		})
	}
	return code, resp, out.Router.ID
}

func (rr *routerRig) updateGateway(t *testing.T, hdr map[string]string, routerID, gateway string) (int, string) {
	t.Helper()
	return doJSONReq(rr.r, http.MethodPut, "/v2.0/routers/"+routerID, `{"router":{"external_gateway_info":`+gateway+`}}`, hdr)
}

func gatewayOf(t *testing.T, body string) *gatewayOut {
	t.Helper()
	var out struct {
		Router struct {
			Gateway *gatewayOut `json:"external_gateway_info"`
		} `json:"router"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out.Router.Gateway
}

// gatewayPorts reads the router's gateway ports: id, project and fixed IPs.
func gatewayPorts(t *testing.T, routerID string) []struct{ id, project, network, fixedIPs string } {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT id::text, COALESCE(project_id::text, ''), network_id::text, fixed_ips::text
		  FROM ports WHERE device_id::text = $1 AND device_owner = 'network:router_gateway'`, routerID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct{ id, project, network, fixedIPs string }
	for rows.Next() {
		var p struct{ id, project, network, fixedIPs string }
		if err := rows.Scan(&p.id, &p.project, &p.network, &p.fixedIPs); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func inPool(prefix, ip string, lo, hi int) bool {
	for i := lo; i <= hi; i++ {
		if ip == fmt.Sprintf("%s.%d", prefix, i) {
			return true
		}
	}
	return false
}

func TestRouterGatewayPort(t *testing.T) {
	requireDB(t)

	t.Run("a gateway gets a port with no project and an address from the pools", func(t *testing.T) {
		rr := newRouterRig(t)
		code, body, id := rr.createRouter(t, rr.member, `{"network_id":"`+rr.external+`"}`)
		if code != http.StatusCreated {
			t.Fatalf("create: code=%d body=%s", code, body)
		}
		gw := gatewayOf(t, body)
		if gw == nil || len(gw.ExternalFixedIPs) != 1 || gw.ExternalFixedIPs[0].SubnetID != rr.subnet ||
			!inPool(rr.prefix, gw.ExternalFixedIPs[0].IPAddress, 10, 20) {
			t.Fatalf("external_gateway_info = %+v; want one address of %s's pool .10–.20", gw, rr.subnet)
		}
		if gw.EnableSNAT == nil || !*gw.EnableSNAT {
			t.Errorf("enable_snat = %v; want true, upstream's default, reported", gw.EnableSNAT)
		}
		ports := gatewayPorts(t, id)
		if len(ports) != 1 || ports[0].project != "" || ports[0].network != rr.external ||
			!strings.Contains(ports[0].fixedIPs, gw.ExternalFixedIPs[0].IPAddress) {
			t.Errorf("gateway ports %+v; want one on %s with no project, holding %s", ports, rr.external, gw.ExternalFixedIPs[0].IPAddress)
		}
		code, body = doReq(rr.r, http.MethodGet, "/v2.0/routers/"+id, rr.member)
		if got := gatewayOf(t, body); code != http.StatusOK || got == nil || len(got.ExternalFixedIPs) != 1 ||
			got.ExternalFixedIPs[0] != gw.ExternalFixedIPs[0] {
			t.Errorf("show: code=%d gateway %+v; want %+v", code, got, gw)
		}
	})

	t.Run("an administrator's requested address; a changed one moves the port", func(t *testing.T) {
		rr := newRouterRig(t)
		code, body, id := rr.createRouter(t, rr.admin,
			`{"network_id":"`+rr.external+`","external_fixed_ips":[{"ip_address":"`+rr.prefix+`.15"}]}`)
		if code != http.StatusCreated {
			t.Fatalf("create: code=%d body=%s", code, body)
		}
		if gw := gatewayOf(t, body); gw == nil || len(gw.ExternalFixedIPs) != 1 || gw.ExternalFixedIPs[0].IPAddress != rr.prefix+".15" {
			t.Fatalf("gateway %+v; want %s.15", gw, rr.prefix)
		}
		before := gatewayPorts(t, id)
		code, body = rr.updateGateway(t, rr.admin, id,
			`{"network_id":"`+rr.external+`","external_fixed_ips":[{"subnet_id":"`+rr.subnet+`","ip_address":"`+rr.prefix+`.30"}]}`)
		if code != http.StatusOK {
			t.Fatalf("update: code=%d body=%s", code, body)
		}
		if gw := gatewayOf(t, body); gw == nil || len(gw.ExternalFixedIPs) != 1 || gw.ExternalFixedIPs[0].IPAddress != rr.prefix+".30" {
			t.Errorf("gateway after the change %+v; want %s.30 (a requested address need not be in a pool)", gw, rr.prefix)
		}
		after := gatewayPorts(t, id)
		if len(before) != 1 || len(after) != 1 || after[0].id != before[0].id || strings.Contains(after[0].fixedIPs, rr.prefix+".15\"") {
			t.Errorf("gateway ports before %+v, after %+v; want the same port, moved to .30", before, after)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		rr := newRouterRig(t)
		heldIP := rr.prefix + ".16"
		seedN4Port(t, rr.external, rr.subnet, heldIP)
		for _, tc := range []struct {
			name     string
			hdr      map[string]string
			fixedIPs string
			status   int
			wantType string
		}{
			{"a member may not request one", rr.member, `[{"ip_address":"` + rr.prefix + `.15"}]`, http.StatusForbidden, "PolicyNotAuthorized"},
			{"outside the network's subnets", rr.admin, `[{"ip_address":"203.0.113.9"}]`, http.StatusBadRequest, "InvalidIpForNetwork"},
			{"held by a port", rr.admin, `[{"ip_address":"` + heldIP + `"}]`, http.StatusConflict, "IpAddressAlreadyAllocated"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				code, body, _ := rr.createRouter(t, tc.hdr, `{"network_id":"`+rr.external+`","external_fixed_ips":`+tc.fixedIPs+`}`)
				if code != tc.status {
					t.Fatalf("code=%d body=%s; want %d", code, body, tc.status)
				}
				if tc.wantType != "" {
					if typ := neutronErrorType(t, body); typ != tc.wantType {
						t.Errorf("type = %q; want %s", typ, tc.wantType)
					}
				}
			})
		}
		var routers int
		if err := testPool.QueryRow(context.Background(),
			"SELECT count(*) FROM ports WHERE network_id = $1 AND device_owner = 'network:router_gateway'", rr.external).Scan(&routers); err != nil {
			t.Fatal(err)
		}
		if routers != 0 {
			t.Errorf("%d gateway port(s) after four refusals; want none", routers)
		}
	})

	t.Run("clearing the gateway deletes its port", func(t *testing.T) {
		rr := newRouterRig(t)
		_, _, id := rr.createRouter(t, rr.member, `{"network_id":"`+rr.external+`"}`)
		code, body := rr.updateGateway(t, rr.member, id, `{}`)
		if code != http.StatusOK {
			t.Fatalf("clear: code=%d body=%s", code, body)
		}
		if gw := gatewayOf(t, body); gw != nil {
			t.Errorf("external_gateway_info after the clear = %+v; want null", gw)
		}
		if ports := gatewayPorts(t, id); len(ports) != 0 {
			t.Errorf("gateway ports after the clear: %+v; want none", ports)
		}
	})

	t.Run("the gateway port is an administrator's to see", func(t *testing.T) {
		rr := newRouterRig(t)
		// An external network of another project, as an administrator's is: its owner reads the ports on
		// it, as upstream's get_port does, and the rig's network is the member's own.
		owner := createTestProject(t, "spec033-ext-owner")
		external := seedExternalNetwork(t, "spec033-ext-foreign-"+uuid.NewString()[:6], owner, false, true)
		gateway := rr.prefix + ".1"
		subnet := seedSubnet(t, external, "spec033-ext-foreign", rr.prefix+".0/24", &gateway, 4, false)
		storeTestPools(t, subnet, [2]string{rr.prefix + ".10", rr.prefix + ".20"})
		_, _, id := rr.createRouter(t, rr.member, `{"network_id":"`+external+`"}`)
		ports := gatewayPorts(t, id)
		if len(ports) != 1 {
			t.Fatalf("gateway ports: %+v", ports)
		}
		if code, body := doReq(rr.r, http.MethodGet, "/v2.0/ports?device_id="+id, rr.member); code != http.StatusOK || strings.Contains(body, ports[0].id) {
			t.Errorf("a member's port listing: code=%d body=%s; want the gateway port left out", code, body)
		}
		if code, _ := doReq(rr.r, http.MethodGet, "/v2.0/ports/"+ports[0].id, rr.member); code != http.StatusNotFound {
			t.Errorf("a member's show of the gateway port: code=%d; want 404", code)
		}
		if code, body := doReq(rr.r, http.MethodGet, "/v2.0/ports?device_id="+id, rr.admin); code != http.StatusOK || !strings.Contains(body, ports[0].id) {
			t.Errorf("an administrator's port listing: code=%d body=%s; want the gateway port", code, body)
		}
		ownerHdr := map[string]string{"X-Test-Project": owner, "X-Test-Roles": "member"}
		if code, _ := doReq(rr.r, http.MethodGet, "/v2.0/ports/"+ports[0].id, ownerHdr); code != http.StatusOK {
			t.Errorf("the external network owner's show of the gateway port: code=%d; want 200", code)
		}
	})
}

func TestRouterInterfaces(t *testing.T) {
	requireDB(t)
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	r := newNeutronTestRouter()
	newRouter := func(t *testing.T) string {
		t.Helper()
		code, body := doJSONReq(r, http.MethodPost, "/v2.0/routers", `{"router":{"name":"spec033-if"}}`, hdr)
		if code != http.StatusCreated {
			t.Fatalf("create router: code=%d body=%s", code, body)
		}
		var out struct {
			Router struct {
				ID string `json:"id"`
			} `json:"router"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		t.Cleanup(func() {
			mustExec(t, "DELETE FROM ports WHERE device_id = $1", out.Router.ID)
			mustExec(t, "DELETE FROM routers WHERE id = $1", out.Router.ID)
		})
		return out.Router.ID
	}
	portsOn := func(t *testing.T, network string) int {
		t.Helper()
		var n int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM ports WHERE network_id = $1", network).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	addInterface := func(router, subnet string) (int, string) {
		return doJSONReq(r, http.MethodPut, "/v2.0/routers/"+router+"/add_router_interface", `{"subnet_id":"`+subnet+`"}`, hdr)
	}

	t.Run("a subnet the router already has", func(t *testing.T) {
		router := newRouter(t)
		gw := "10.95.1.1"
		network := seedNetwork(t, "spec033-if-dup")
		subnet := seedSubnet(t, network, "spec033-if-dup", "10.95.1.0/24", &gw, 4, false)
		if code, body := addInterface(router, subnet); code != http.StatusOK {
			t.Fatalf("first interface: code=%d body=%s", code, body)
		}
		if code, body := addInterface(router, subnet); code != http.StatusBadRequest {
			t.Errorf("second interface on the subnet: code=%d body=%s; want 400", code, body)
		}
		if n := portsOn(t, network); n != 1 {
			t.Errorf("%d ports on the network; want the first interface's only", n)
		}
	})

	t.Run("a subnet without a gateway", func(t *testing.T) {
		router := newRouter(t)
		network := seedNetwork(t, "spec033-if-nogw")
		subnet := seedSubnet(t, network, "spec033-if-nogw", "10.95.2.0/24", nil, 4, false)
		if code, body := addInterface(router, subnet); code != http.StatusBadRequest {
			t.Errorf("interface on a gatewayless subnet: code=%d body=%s; want 400", code, body)
		}
		if n := portsOn(t, network); n != 0 {
			t.Errorf("%d ports on the network; want none", n)
		}
	})

	t.Run("the port and the interface are stored together", func(t *testing.T) {
		router := newRouter(t)
		gw := "10.95.3.1"
		network := seedNetwork(t, "spec033-if-tx")
		subnet := seedSubnet(t, network, "spec033-if-tx", "10.95.3.0/24", &gw, 4, false)
		release := dbtest.Unavailable(t, testPool, "router_interfaces")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req := httptest.NewRequest(http.MethodPut, "/v2.0/routers/"+router+"/add_router_interface",
			strings.NewReader(`{"subnet_id":"`+subnet+`"}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		release()
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("interface whose row cannot be written: code=%d body=%s; want 500", w.Code, w.Body.String())
		}
		if n := portsOn(t, network); n != 0 {
			t.Errorf("%d port(s) left on the network; want the interface's port rolled back with its row", n)
		}
	})
}
