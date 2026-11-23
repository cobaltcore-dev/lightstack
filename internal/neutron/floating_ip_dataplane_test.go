package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 T036, reworked by T115 and T130 (FR-007a, FR-008, FR-008a, FR-008d; research R20, R23, R25,
// R28): every floating-IP change is stored, then made in force before the answer — its router
// converged on the network node — and undone, compare-and-set, when that fails. One node, which is its
// own network node, in stub mode, with its routers replaced by the fake. The router's NAT itself is the
// router unit's (router_unit_test.go); the guests' firewall does not change with a floating IP (R32).

type fipFixture struct {
	r               *gin.Engine
	svc             *Service
	routers         *fakeRouters
	external        string
	network, subnet string
	router          string
	portX, fixedX   string
	portY, fixedY   string
	hdr             map[string]string

	mu     sync.Mutex
	events []string             // every router call, in order: "Router R"
	fail   func(e string) error // decides each call's outcome; nil lets every call through
}

func newFIPFixture(t *testing.T) *fipFixture {
	t.Helper()
	r, svc := newNeutronTestRouterWithService()
	routers := newFakeRouters()
	svc.routers = routers
	fx := &fipFixture{r: r, svc: svc, routers: routers, hdr: projectHdr(seededProjectID)}
	routers.hook = func(c routerCall) error { return fx.event("Router " + c.ID) }
	short := uuid.NewString()[:8]
	fx.external = seedExternalNetwork(t, "spec033-ext-"+short, seededProjectID, false, true)
	seedSubnet(t, fx.external, "spec033-ext-subnet-"+short, fmt.Sprintf("198.19.%d.0/24", rand.IntN(256)), nil, 4, false)
	fx.network = seedNetwork(t, "spec033-fip-net-"+short)
	gateway := "10.36.0.1" // a floating IP needs a router holding its subnet's gateway (FR-028)
	fx.subnet = seedSubnet(t, fx.network, "spec033-fip-subnet-"+short, "10.36.0.0/16", &gateway, 4, true)
	fx.router = seedRouterOn(t, fx.network, fx.subnet, fx.external)
	fx.portX, fx.fixedX = seedPortWithIP(t, fx.network, fx.subnet)
	fx.portY, fx.fixedY = seedPortWithIP(t, fx.network, fx.subnet)
	return fx
}

// seedRouterOn creates a router with a gateway on external — its gateway port — and an interface on the
// subnet holding the subnet's gateway, as the router API writes them (SPEC-033 FR-028, FR-038).
func seedRouterOn(t *testing.T, networkID, subnetID, external string) string {
	t.Helper()
	ctx := context.Background()
	routerID, portID := uuid.NewString(), uuid.NewString()
	var gateway string
	if err := testPool.QueryRow(ctx, "SELECT COALESCE(gateway_ip, '') FROM subnets WHERE id = $1", subnetID).Scan(&gateway); err != nil {
		t.Fatalf("read the subnet's gateway: %v", err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO routers (id, name, project_id, external_gateway_info) VALUES ($1, $2, $3, $4)`,
			[]any{routerID, "spec033-router-" + routerID[:8], seededProjectID, `{"network_id":"` + external + `"}`}},
		{`INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, fixed_ips)
		  VALUES ($1, '', $2, NULL, $3, 'network:router_gateway', 'fa:16:3e:33:36:00', '[]'::jsonb)`,
			[]any{uuid.NewString(), external, routerID}},
		{`INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, fixed_ips)
		  VALUES ($1, 'spec033-rif', $2, $3, $4, 'network:router_interface', 'fa:16:3e:33:36:01', $5::jsonb)`,
			[]any{portID, networkID, seededProjectID, routerID, `[{"subnet_id":"` + subnetID + `","ip_address":"` + gateway + `"}]`}},
		{`INSERT INTO router_interfaces (router_id, port_id, subnet_id) VALUES ($1, $2, $3)`,
			[]any{routerID, portID, subnetID}},
	} {
		if _, err := testPool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed router: %v", err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM ports WHERE device_id = $1", routerID) //nolint:errcheck // best-effort cleanup
		testPool.Exec(context.Background(), "DELETE FROM routers WHERE id = $1", routerID)      //nolint:errcheck // best-effort cleanup
	})
	return routerID
}

// seedPortWithIP creates a port with one fixed IP, in a group of its own.
func seedPortWithIP(t *testing.T, networkID, subnetID string) (string, string) {
	t.Helper()
	id := uuid.NewString()
	ip := fmt.Sprintf("10.36.%d.%d", rand.IntN(256), 2+rand.IntN(250))
	fixed, _ := json.Marshal([]map[string]string{{"subnet_id": subnetID, "ip_address": ip}})
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, $2, $3, $4, 'fa:16:3e:33:36:02', 'ACTIVE', $5::jsonb)`,
		id, "spec033-fip-port-"+id[:8], networkID, seededProjectID, string(fixed)); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	g := sgTestGroup(t, seededProjectID, "spec033-fip-sg-"+id[:6])
	sgTestAttach(t, id, g)
	return id, ip
}

type fipRow struct {
	ID, Project, Network, Address, Fixed, Port, Router, Status, Description string
	Created, Updated                                                        string
}

func readFIP(t *testing.T, id string) (fipRow, bool) {
	t.Helper()
	var r fipRow
	var fixed, port, router, desc *string
	var created, updated time.Time
	err := testPool.QueryRow(context.Background(), `
		SELECT id::text, project_id::text, floating_network_id::text, floating_ip_address, fixed_ip_address,
		       port_id::text, router_id::text, status, description, created_at, updated_at
		FROM floating_ips WHERE id = $1`, id).Scan(&r.ID, &r.Project, &r.Network, &r.Address, &fixed, &port,
		&router, &r.Status, &desc, &created, &updated)
	if errors.Is(err, database.ErrNoRows) {
		return fipRow{}, false
	}
	if err != nil {
		t.Fatalf("read floating IP %s: %v", id, err)
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	r.Fixed, r.Port, r.Router, r.Description = deref(fixed), deref(port), deref(router), deref(desc)
	r.Created, r.Updated = created.UTC().Format(time.RFC3339Nano), updated.UTC().Format(time.RFC3339Nano)
	return r, true
}

// create creates a floating IP on the fixture's external network, associated with portID when it is
// not "".
func (fx *fipFixture) create(t *testing.T, portID string) (string, string) {
	t.Helper()
	body := `{"floatingip":{"floating_network_id":"` + fx.external + `"`
	if portID != "" {
		body += `,"port_id":"` + portID + `"`
	}
	code, resp := doJSONReq(fx.r, http.MethodPost, "/v2.0/floatingips", body+`}}`, fx.hdr)
	if code != http.StatusCreated {
		t.Fatalf("create floating IP: code=%d body=%s; want 201", code, resp)
	}
	var out struct {
		FloatingIP struct {
			ID      string `json:"id"`
			Address string `json:"floating_ip_address"`
		} `json:"floatingip"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM floating_ips WHERE id = $1", out.FloatingIP.ID) //nolint:errcheck // best-effort cleanup
	})
	return out.FloatingIP.ID, out.FloatingIP.Address
}

// event records a call to either fake and returns what fail decides for it.
func (fx *fipFixture) event(e string) error {
	fx.mu.Lock()
	fx.events = append(fx.events, e)
	fail := fx.fail
	fx.mu.Unlock()
	if fail == nil {
		return nil
	}
	return fail(e)
}

// eventsSince returns the calls from position n on.
func (fx *fipFixture) eventsSince(n int) []string {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return slices.Clone(fx.events[n:])
}

func (fx *fipFixture) eventCount() int {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return len(fx.events)
}

// held is the router as the network node holds it.
func (fx *fipFixture) held(t *testing.T) networking.RouterState {
	t.Helper()
	s, ok := fx.routers.state(fx.router)
	if !ok {
		t.Fatalf("the network node holds no router %s", fx.router)
	}
	return s
}

// floatingOf is where the router forwards a floating address: its fixed IP, or "" for nowhere.
func (fx *fipFixture) floatingOf(t *testing.T, address string) string {
	t.Helper()
	s, ok := fx.routers.state(fx.router)
	if !ok {
		return ""
	}
	for _, f := range s.FloatingIPs {
		if f.Address == address {
			return f.FixedIP
		}
	}
	return ""
}

func (fx *fipFixture) put(id, body string) (int, string) {
	return doJSONReq(fx.r, http.MethodPut, "/v2.0/floatingips/"+id, body, fx.hdr)
}

func (fx *fipFixture) del(path string) (int, string) {
	return doJSONReq(fx.r, http.MethodDelete, path, "", fx.hdr)
}

// failFirst makes the first call whose event starts with prefix ("Router") fail with err, after
// running before.
func (fx *fipFixture) failFirst(prefix string, err error, before func()) {
	failed := false
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.fail = func(e string) error {
		if failed || !strings.HasPrefix(e, prefix) {
			return nil
		}
		failed = true
		if before != nil {
			before()
		}
		return err
	}
}

var errNode = errors.New("iptables-restore: line 3 failed")

func TestFloatingIPDataplane_Lifecycle(t *testing.T) {
	requireDB(t)

	t.Run("create on a port: the router", func(t *testing.T) {
		fx := newFIPFixture(t)
		_, addr := fx.create(t, fx.portX)
		want := []string{"Router " + fx.router}
		if got := fx.eventsSince(0); !slices.Equal(got, want) {
			t.Errorf("calls %q; want %q", got, want)
		}
		if got := fx.floatingOf(t, addr); got != fx.fixedX {
			t.Errorf("the router forwards %s to %q; want %s", addr, got, fx.fixedX)
		}
	})

	t.Run("create without a port touches nothing", func(t *testing.T) {
		fx := newFIPFixture(t)
		fx.create(t, "")
		if got := fx.eventsSince(0); len(got) != 0 {
			t.Errorf("calls %q; want none", got)
		}
	})

	t.Run("move: the router", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		if code, body := fx.put(id, `{"floatingip":{"port_id":"`+fx.portY+`"}}`); code != http.StatusOK {
			t.Fatalf("move: code=%d body=%s", code, body)
		}
		if got := fx.floatingOf(t, addr); got != fx.fixedY {
			t.Errorf("the router forwards %s to %q; want %s", addr, got, fx.fixedY)
		}
	})

	t.Run("disassociate: the router; description only: nothing", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		before := fx.eventCount()
		if code, body := fx.put(id, `{"floatingip":{"description":"spec033"}}`); code != http.StatusOK {
			t.Fatalf("description: code=%d body=%s", code, body)
		}
		if got := fx.eventsSince(before); len(got) != 0 {
			t.Errorf("a description-only update called %q", got)
		}
		if code, body := fx.put(id, `{"floatingip":{"port_id":null}}`); code != http.StatusOK {
			t.Fatalf("disassociate: code=%d body=%s", code, body)
		}
		want := []string{"Router " + fx.router}
		if got := fx.eventsSince(before); !slices.Equal(got, want) {
			t.Errorf("calls %q; want %q", got, want)
		}
		if got := fx.floatingOf(t, addr); got != "" {
			t.Errorf("the router still forwards %s to %s", addr, got)
		}
	})

	t.Run("delete: the row and its port forwarding in one step, then the node", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		pf := seedPortForwarding(t, id, fx.portX, fx.fixedX)
		before := fx.eventCount()
		if code, body := fx.del("/v2.0/floatingips/" + id); code != http.StatusNoContent {
			t.Fatalf("delete: code=%d body=%s", code, body)
		}
		if _, ok := readFIP(t, id); ok {
			t.Error("the row is still there")
		}
		if portForwardingExists(t, pf) {
			t.Error("its port forwarding is still there")
		}
		want := []string{"Router " + fx.router}
		if got := fx.eventsSince(before); !slices.Equal(got, want) {
			t.Errorf("calls %q; want %q", got, want)
		}
		if s := fx.held(t); fx.floatingOf(t, addr) != "" || len(s.PortForwardings) != 0 {
			t.Errorf("the router still holds the floating IP or its port forwarding: %+v", s)
		}

		unassociated, _ := fx.create(t, "")
		before = fx.eventCount()
		if code, body := fx.del("/v2.0/floatingips/" + unassociated); code != http.StatusNoContent {
			t.Fatalf("delete unassociated: code=%d body=%s", code, body)
		}
		if got := fx.eventsSince(before); len(got) != 0 {
			t.Errorf("deleting a floating IP through no router called %q", got)
		}
	})
}

func seedPortForwarding(t *testing.T, fipID, portID, ip string) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `INSERT INTO port_forwardings (id, project_id, floatingip_id, internal_port_id, internal_ip_address, external_port, internal_port, protocol, description)
		VALUES ($1, $2, $3, $4, $5, 8080, 80, 'tcp', 'spec033')`, id, seededProjectID, fipID, portID, ip)
	return id
}

func portForwardingExists(t *testing.T, id string) bool {
	t.Helper()
	var ok bool
	if err := testPool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM port_forwardings WHERE id = $1)", id).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestFloatingIPDataplane_Failures(t *testing.T) {
	requireDB(t)

	t.Run("create, failing the router: no row, and the router converged again", func(t *testing.T) {
		fx := newFIPFixture(t)
		fx.failFirst("Router", errNode, nil)
		body := `{"floatingip":{"floating_network_id":"` + fx.external + `","port_id":"` + fx.portX + `"}}`
		code, resp := doJSONReq(fx.r, http.MethodPost, "/v2.0/floatingips", body, fx.hdr)
		if code != http.StatusInternalServerError {
			t.Fatalf("create: code=%d body=%s; want 500", code, resp)
		}
		var n int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM floating_ips WHERE port_id = $1", fx.portX).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d rows left for the port; want none", n)
		}
		if s, ok := fx.routers.state(fx.router); !ok || len(s.FloatingIPs) != 0 {
			t.Errorf("the router after the undo: %+v (held %v); want it converged without the floating IP", s.FloatingIPs, ok)
		}
	})

	for _, change := range []string{"move", "disassociate"} {
		t.Run(change+", failing the router: the old association is back", func(t *testing.T) {
			fx := newFIPFixture(t)
			id, addr := fx.create(t, fx.portX)
			before, _ := readFIP(t, id)
			body := `{"floatingip":{"port_id":"` + fx.portY + `"}}`
			if change == "disassociate" {
				body = `{"floatingip":{"port_id":null}}`
			}
			fx.failFirst("Router", errNode, nil)
			if code, resp := fx.put(id, body); code != http.StatusInternalServerError {
				t.Fatalf("PUT %s: code=%d body=%s; want 500", body, code, resp)
			}
			after, _ := readFIP(t, id)
			if after.Port != before.Port || after.Fixed != before.Fixed || after.Router != before.Router || after.Status != before.Status {
				t.Errorf("row %+v; want the old association %+v", after, before)
			}
			if got := fx.floatingOf(t, addr); got != fx.fixedX {
				t.Errorf("the router forwards %s to %q; want it converged back to %s", addr, got, fx.fixedX)
			}
		})
	}

	t.Run("delete, failing the router: the row and its port forwarding are back as they were", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		pf := seedPortForwarding(t, id, fx.portX, fx.fixedX)
		before, _ := readFIP(t, id)
		fx.failFirst("Router", errNode, nil)
		if code, resp := fx.del("/v2.0/floatingips/" + id); code != http.StatusInternalServerError {
			t.Fatalf("delete: code=%d body=%s; want 500", code, resp)
		}
		if after, ok := readFIP(t, id); !ok || after != before {
			t.Errorf("row %+v (present %v); want it back as %+v", after, ok, before)
		}
		if !portForwardingExists(t, pf) {
			t.Error("the port forwarding did not come back")
		}
		if got := fx.floatingOf(t, addr); got != fx.fixedX {
			t.Errorf("the router forwards %s to %q; want it converged back to %s", addr, got, fx.fixedX)
		}
	})

	t.Run("a delete restored as the foreign keys would have", func(t *testing.T) {
		t.Run("its port deleted meanwhile: back unassociated, without the port forwarding", func(t *testing.T) {
			fx := newFIPFixture(t)
			id, _ := fx.create(t, fx.portX)
			pf := seedPortForwarding(t, id, fx.portX, fx.fixedX)
			fx.failFirst("Router", errNode, func() { mustExec(t, "DELETE FROM ports WHERE id = $1", fx.portX) })
			if code, resp := fx.del("/v2.0/floatingips/" + id); code != http.StatusInternalServerError {
				t.Fatalf("delete: code=%d body=%s; want 500", code, resp)
			}
			after, ok := readFIP(t, id)
			if !ok || after.Port != "" || after.Fixed != "" {
				t.Errorf("row %+v (present %v); want it back unassociated", after, ok)
			}
			if portForwardingExists(t, pf) {
				t.Error("the port forwarding came back, though its internal port is gone")
			}
		})
		t.Run("its address allocated again meanwhile: it stays deleted", func(t *testing.T) {
			fx := newFIPFixture(t)
			id, addr := fx.create(t, fx.portX)
			other := uuid.NewString()
			fx.failFirst("Router", errNode, func() {
				mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, status)
					VALUES ($1, $2, $3, $4, 'DOWN')`, other, seededProjectID, fx.external, addr)
			})
			t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM floating_ips WHERE id = $1", other) }) //nolint:errcheck // best-effort cleanup
			if code, resp := fx.del("/v2.0/floatingips/" + id); code != http.StatusInternalServerError {
				t.Fatalf("delete: code=%d body=%s; want 500", code, resp)
			}
			if _, ok := readFIP(t, id); ok {
				t.Error("the deleted floating IP came back over the new one")
			}
			if _, ok := readFIP(t, other); !ok {
				t.Error("the new floating IP is gone")
			}
		})
	})

	t.Run("compare-and-set: a change another request made meanwhile stays", func(t *testing.T) {
		t.Run("create, its description changed", func(t *testing.T) {
			fx := newFIPFixture(t)
			fx.failFirst("Router", errNode, func() {
				mustExec(t, "UPDATE floating_ips SET description = 'other request' WHERE port_id = $1", fx.portX)
			})
			body := `{"floatingip":{"floating_network_id":"` + fx.external + `","port_id":"` + fx.portX + `"}}`
			if code, resp := doJSONReq(fx.r, http.MethodPost, "/v2.0/floatingips", body, fx.hdr); code != http.StatusInternalServerError {
				t.Fatalf("create: code=%d body=%s; want 500", code, resp)
			}
			var desc string
			err := testPool.QueryRow(context.Background(), "SELECT description FROM floating_ips WHERE port_id = $1", fx.portX).Scan(&desc)
			if err != nil || desc != "other request" {
				t.Errorf("row: %q, %v; want it kept with the other request's description", desc, err)
			}
			mustExec(t, "DELETE FROM floating_ips WHERE port_id = $1", fx.portX)
		})
		t.Run("move, moved again", func(t *testing.T) {
			fx := newFIPFixture(t)
			id, _ := fx.create(t, fx.portX)
			fx.failFirst("Router", errNode, func() {
				mustExec(t, "UPDATE floating_ips SET port_id = $1, fixed_ip_address = $2 WHERE id = $3", fx.portX, fx.fixedX, id)
				mustExec(t, "UPDATE floating_ips SET port_id = $1, fixed_ip_address = $2 WHERE id = $3", fx.portY, "10.36.255.254", id)
			})
			if code, resp := fx.put(id, `{"floatingip":{"port_id":"`+fx.portY+`"}}`); code != http.StatusInternalServerError {
				t.Fatalf("move: code=%d body=%s; want 500", code, resp)
			}
			if after, _ := readFIP(t, id); after.Fixed != "10.36.255.254" {
				t.Errorf("row %+v; want the other request's association kept", after)
			}
		})
	})

	t.Run("no uplink: every change answers 500 and is undone", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, _ := fx.create(t, fx.portX)
		before, _ := readFIP(t, id)
		fx.svc.uplink = func() (string, error) { return "", errors.New("no IPv4 default route / uplink found") }

		body := `{"floatingip":{"floating_network_id":"` + fx.external + `","port_id":"` + fx.portY + `"}}`
		if code, resp := doJSONReq(fx.r, http.MethodPost, "/v2.0/floatingips", body, fx.hdr); code != http.StatusInternalServerError {
			t.Errorf("create: code=%d body=%s; want 500", code, resp)
		}
		var n int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM floating_ips WHERE port_id = $1", fx.portY).Scan(&n); err != nil || n != 0 {
			t.Errorf("create left %d rows (%v); want none", n, err)
		}
		if code, resp := fx.put(id, `{"floatingip":{"port_id":null}}`); code != http.StatusInternalServerError {
			t.Errorf("update: code=%d body=%s; want 500", code, resp)
		}
		if after, _ := readFIP(t, id); after.Port != before.Port {
			t.Errorf("update: row %+v; want the association back", after)
		}
		if code, resp := fx.del("/v2.0/floatingips/" + id); code != http.StatusInternalServerError {
			t.Errorf("delete: code=%d body=%s; want 500", code, resp)
		}
		if _, ok := readFIP(t, id); !ok {
			t.Error("delete: the floating IP did not come back")
		}
	})

	t.Run("a busy router answers 500 within about a second and puts the association back", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, _ := fx.create(t, fx.portX)
		before, _ := readFIP(t, id)
		release, err := fx.svc.units.acquire(context.Background(), unitRequest{Unit: unitRouter, IDs: []string{fx.router}}.lockKey())
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		code, resp := fx.put(id, `{"floatingip":{"port_id":"`+fx.portY+`"}}`)
		elapsed := time.Since(start)
		release()
		if code != http.StatusInternalServerError {
			t.Fatalf("PUT on a busy router: code=%d body=%s; want 500", code, resp)
		}
		if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
			t.Errorf("PUT took %v; want about one second per lock wait", elapsed)
		}
		if after, _ := readFIP(t, id); after.Port != before.Port {
			t.Errorf("row %+v; want the association back", after)
		}
	})
}

// SPEC-033 R25 (FR-008d): a deleted port takes its floating IPs' association with it, in the
// deleting transaction, and the node follows before the answer.
func TestFloatingIPDataplane_PortDeletion(t *testing.T) {
	requireDB(t)

	assertDisassociated := func(t *testing.T, id string) {
		t.Helper()
		after, ok := readFIP(t, id)
		if !ok || after.Port != "" || after.Fixed != "" || after.Router != "" || after.Status != "DOWN" {
			t.Errorf("floating IP %+v (present %v); want no port, fixed IP or router, and DOWN", after, ok)
		}
	}

	t.Run("DELETE /v2.0/ports: disassociated and withdrawn before the answer", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		if code, resp := fx.del("/v2.0/ports/" + fx.portX); code != http.StatusNoContent {
			t.Fatalf("delete port: code=%d body=%s; want 204", code, resp)
		}
		assertDisassociated(t, id)
		if got := fx.floatingOf(t, addr); got != "" {
			t.Errorf("the router still forwards %s to %s", addr, got)
		}
	})

	t.Run("the port's lock comes first", func(t *testing.T) {
		dbtest.RequirePostgres(t)
		fx := newFIPFixture(t)
		ctx := context.Background()
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // a no-op after the commit
		var locked string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM ports WHERE id = $1 FOR UPDATE", fx.portX).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		type result struct {
			code int
			body string
		}
		done := make(chan result, 1)
		go func() {
			code, body := fx.del("/v2.0/ports/" + fx.portX)
			done <- result{code, body}
		}()
		time.Sleep(300 * time.Millisecond)
		second := uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, fixed_ip_address, port_id, router_id, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE')`, second, seededProjectID, fx.external,
			fmt.Sprintf("198.18.%d.%d", rand.IntN(256), 1+rand.IntN(254)), fx.fixedX, fx.portX, fx.router); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM floating_ips WHERE id = $1", second) }) //nolint:errcheck // best-effort cleanup
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if r := <-done; r.code != http.StatusNoContent {
			t.Fatalf("delete port: code=%d body=%s; want 204", r.code, r.body)
		}
		assertDisassociated(t, second)
	})

	// SPEC-033 T223: a floating IP associated with the port after its withdrawal, while the
	// delete awaits the port's lock, is in its router by then; the delete disassociates it, and the
	// convergence after the commit takes it out of the router before the answer.
	t.Run("an association made meanwhile leaves the router after the commit", func(t *testing.T) {
		dbtest.RequirePostgres(t)
		fx := newFIPFixture(t)
		ctx := context.Background()
		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // a no-op after the commit
		// NO KEY UPDATE holds the delete's FOR UPDATE off and lets a floating IP name the port.
		var locked string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM ports WHERE id = $1 FOR NO KEY UPDATE", fx.portX).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		type result struct {
			code int
			body string
		}
		done := make(chan result, 1)
		go func() {
			code, body := fx.del("/v2.0/ports/" + fx.portX)
			done <- result{code, body}
		}()
		dbtest.WaitUntilBlocked(t, testPool, dbtest.HolderPID(t, tx))
		second, address := uuid.NewString(), fmt.Sprintf("198.18.%d.%d", rand.IntN(256), 1+rand.IntN(254))
		mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, fixed_ip_address, port_id, router_id, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE')`, second, seededProjectID, fx.external, address, fx.fixedX, fx.portX, fx.router)
		t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM floating_ips WHERE id = $1", second) }) //nolint:errcheck // best-effort cleanup
		if err := fx.svc.convergeRouterUnit(ctx, fx.router); err != nil {
			t.Fatal(err)
		}
		if got := fx.floatingOf(t, address); got != fx.fixedX {
			t.Fatalf("the router forwards %s to %q before the delete goes on; want %s", address, got, fx.fixedX)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if r := <-done; r.code != http.StatusNoContent {
			t.Fatalf("delete port: code=%d body=%s; want 204", r.code, r.body)
		}
		assertDisassociated(t, second)
		if got := fx.floatingOf(t, address); got != "" {
			t.Errorf("the router still forwards %s to %s after the delete", address, got)
		}
	})

	t.Run("a failed boot's release does the same", func(t *testing.T) {
		fx := newFIPFixture(t)
		instance := sgTestInstance(t)
		mustExec(t, "UPDATE ports SET device_id = $1, device_owner = 'compute:nova' WHERE id = $2", instance, fx.portX)
		id, addr := fx.create(t, fx.portX)
		if err := fx.svc.ReleaseInstancePorts(context.Background(), instance); err != nil {
			t.Fatalf("ReleaseInstancePorts: %v", err)
		}
		assertDisassociated(t, id)
		if got := fx.floatingOf(t, addr); got != "" {
			t.Errorf("the router still forwards %s to %s", addr, got)
		}
	})

	// The port leaves its floating IP's router before its record goes (SPEC-033 FR-040, R34): a router
	// that cannot be converged keeps the port and the association, and converges the router again.
	t.Run("a failed withdrawal: 500, the port and its floating IP kept", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		before, _ := readFIP(t, id)
		fx.failFirst("Router", errNode, nil)
		if code, resp := fx.del("/v2.0/ports/" + fx.portX); code != http.StatusInternalServerError {
			t.Fatalf("delete port: code=%d body=%s; want 500", code, resp)
		}
		if after, ok := readFIP(t, id); !ok || after.Port != before.Port || after.Fixed != before.Fixed {
			t.Errorf("floating IP %+v (present %v); want its association kept", after, ok)
		}
		if got := fx.floatingOf(t, addr); got != fx.fixedX {
			t.Errorf("the router forwards %s to %q; want it converged back to %s", addr, got, fx.fixedX)
		}
	})
}
