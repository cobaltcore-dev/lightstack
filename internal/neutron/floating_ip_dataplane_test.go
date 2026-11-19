package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 T036 (FR-007a, FR-008, FR-008a, FR-008d; research R20, R23, R25;
// contracts/floating-ip-dataplane.md): every floating-IP change is stored, converged on the serving
// node before the answer, and undone, compare-and-set, when the node cannot apply it. Stub mode, with
// the service's dataplane replaced by the fake.

type fipFixture struct {
	r               *gin.Engine
	svc             *Service
	fake            *fakeDataplane
	external        string
	network, subnet string
	router          string
	portX, fixedX   string
	portY, fixedY   string
	hdr             map[string]string
}

func newFIPFixture(t *testing.T) *fipFixture {
	t.Helper()
	r, svc := newNeutronTestRouterWithService()
	fake := newFakeDataplane()
	svc.dataplane = fake
	fx := &fipFixture{r: r, svc: svc, fake: fake, hdr: projectHdr(seededProjectID)}
	short := uuid.NewString()[:8]
	fx.external = seedExternalNetwork(t, "spec033-ext-"+short, seededProjectID, false, true)
	seedSubnet(t, fx.external, "spec033-ext-subnet-"+short, fmt.Sprintf("198.19.%d.0/24", rand.IntN(256)), nil, 4, false)
	fx.network = seedNetwork(t, "spec033-fip-net-"+short)
	fx.subnet = seedSubnet(t, fx.network, "spec033-fip-subnet-"+short, "10.36.0.0/16", nil, 4, true)
	fx.router = seedRouterOn(t, fx.network, fx.subnet, fx.external)
	fx.portX, fx.fixedX = seedPortWithIP(t, fx.network, fx.subnet)
	fx.portY, fx.fixedY = seedPortWithIP(t, fx.network, fx.subnet)
	return fx
}

// seedRouterOn creates a router with a gateway on external and an interface on the network.
func seedRouterOn(t *testing.T, networkID, subnetID, external string) string {
	t.Helper()
	ctx := context.Background()
	routerID, portID := uuid.NewString(), uuid.NewString()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO routers (id, name, project_id, external_gateway_info) VALUES ($1, $2, $3, $4)`,
			[]any{routerID, "spec033-router-" + routerID[:8], seededProjectID, `{"network_id":"` + external + `"}`}},
		{`INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, fixed_ips)
		  VALUES ($1, 'spec033-rif', $2, $3, $4, 'network:router_interface', 'fa:16:3e:33:36:01', '[]'::jsonb)`,
			[]any{portID, networkID, seededProjectID, routerID}},
		{`INSERT INTO router_interfaces (router_id, port_id, subnet_id) VALUES ($1, $2, $3)`,
			[]any{routerID, portID, subnetID}},
	} {
		if _, err := testPool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed router: %v", err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM routers WHERE id = $1", routerID) //nolint:errcheck // best-effort cleanup
	})
	return routerID
}

// seedPortWithIP creates a port with one fixed IP, in a group with a tcp/22 rule.
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
	sgTestRule(t, g, "tcp", 22)
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

func (fx *fipFixture) put(id, body string) (int, string) {
	return doJSONReq(fx.r, http.MethodPut, "/v2.0/floatingips/"+id, body, fx.hdr)
}

func (fx *fipFixture) del(path string) (int, string) {
	return doJSONReq(fx.r, http.MethodDelete, path, "", fx.hdr)
}

// failFirst makes the first call of method fail with err, after running before.
func (fx *fipFixture) failFirst(method string, err error, before func()) {
	failed := false
	fx.fake.setHook(func(c dataplaneCall) error {
		if c.Method != method || failed {
			return nil
		}
		failed = true
		if before != nil {
			before()
		}
		return err
	})
}

var errNode = errors.New("iptables-restore: line 3 failed")

func TestFloatingIPDataplane_Lifecycle(t *testing.T) {
	requireDB(t)

	t.Run("create on a port: the filter, then the NAT and the address", func(t *testing.T) {
		fx := newFIPFixture(t)
		_, addr := fx.create(t, fx.portX)
		want := []string{"EnsureIngressFilter " + fx.fixedX, "EnsureFloatingIP " + addr + "->" + fx.fixedX}
		if got := fx.fake.writes(); !slices.Equal(got, want) {
			t.Errorf("writes %q; want %q", got, want)
		}
		if !fx.fake.hasAddress(addr) {
			t.Errorf("the address %s is not on the uplink", addr)
		}
	})

	t.Run("create without a port touches nothing", func(t *testing.T) {
		fx := newFIPFixture(t)
		fx.create(t, "")
		if got := fx.fake.writes(); len(got) != 0 {
			t.Errorf("writes %q; want none", got)
		}
	})

	t.Run("move: the new filter and NAT, the old filter gone", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		if code, body := fx.put(id, `{"floatingip":{"port_id":"`+fx.portY+`"}}`); code != http.StatusOK {
			t.Fatalf("move: code=%d body=%s", code, body)
		}
		if nat := fx.fake.natOf(addr); len(nat) != 1 || nat[0].fixed != fx.fixedY {
			t.Errorf("NAT of %s: %+v; want to %s only", addr, nat, fx.fixedY)
		}
		if _, ok := fx.fake.filterOf(fx.fixedY); !ok {
			t.Errorf("no filter for the new fixed IP %s", fx.fixedY)
		}
		if _, ok := fx.fake.filterOf(fx.fixedX); ok {
			t.Errorf("the old fixed IP %s keeps its filter", fx.fixedX)
		}
	})

	t.Run("move: the old filter stays while something still reaches it", func(t *testing.T) {
		for _, keep := range []string{"a second floating IP", "an unmarked rule"} {
			fx := newFIPFixture(t)
			id, _ := fx.create(t, fx.portX)
			if keep == "a second floating IP" {
				fx.create(t, fx.portX)
			} else {
				fx.fake.seedNAT("198.18.250.1", fx.fixedX, false)
			}
			if code, body := fx.put(id, `{"floatingip":{"port_id":"`+fx.portY+`"}}`); code != http.StatusOK {
				t.Fatalf("%s: move: code=%d body=%s", keep, code, body)
			}
			if _, ok := fx.fake.filterOf(fx.fixedX); !ok {
				t.Errorf("%s: the old fixed IP's filter was removed", keep)
			}
		}
	})

	t.Run("disassociate: the address and NAT, then the filter; description only: nothing", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		before := len(fx.fake.writes())
		if code, body := fx.put(id, `{"floatingip":{"description":"spec033"}}`); code != http.StatusOK {
			t.Fatalf("description: code=%d body=%s", code, body)
		}
		if got := fx.fake.writes(); len(got) != before {
			t.Errorf("a description-only update wrote %q", got[before:])
		}
		if code, body := fx.put(id, `{"floatingip":{"port_id":null}}`); code != http.StatusOK {
			t.Fatalf("disassociate: code=%d body=%s", code, body)
		}
		want := []string{"WithdrawFloatingIP " + addr, "RemoveIngressFilter " + fx.fixedX}
		if got := fx.fake.writes()[before:]; !slices.Equal(got, want) {
			t.Errorf("writes %q; want %q", got, want)
		}
	})

	t.Run("delete: the row and its port forwarding in one step, then the node", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		pf := seedPortForwarding(t, id, fx.portX, fx.fixedX)
		before := len(fx.fake.writes())
		if code, body := fx.del("/v2.0/floatingips/" + id); code != http.StatusNoContent {
			t.Fatalf("delete: code=%d body=%s", code, body)
		}
		if _, ok := readFIP(t, id); ok {
			t.Error("the row is still there")
		}
		if portForwardingExists(t, pf) {
			t.Error("its port forwarding is still there")
		}
		want := []string{"WithdrawFloatingIP " + addr, "RemoveIngressFilter " + fx.fixedX}
		if got := fx.fake.writes()[before:]; !slices.Equal(got, want) {
			t.Errorf("writes %q; want %q", got, want)
		}

		unassociated, _ := fx.create(t, "")
		before = len(fx.fake.writes())
		if code, body := fx.del("/v2.0/floatingips/" + unassociated); code != http.StatusNoContent {
			t.Fatalf("delete unassociated: code=%d body=%s", code, body)
		}
		if got := fx.fake.writes()[before:]; len(got) != 0 {
			t.Errorf("deleting an unassociated floating IP wrote %q", got)
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

	for _, method := range []string{"EnsureIngressFilter", "EnsureFloatingIP"} {
		t.Run("create, failing "+method+": no row, and the node converged again", func(t *testing.T) {
			fx := newFIPFixture(t)
			fx.failFirst(method, errNode, nil)
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
			if writes := fx.fake.writes(); !hasPrefixAny(writes, "WithdrawFloatingIP ") {
				t.Errorf("writes %q; want the floating IP withdrawn again after the undo", writes)
			}
		})
	}

	for _, method := range []string{"EnsureIngressFilter", "EnsureFloatingIP", "WithdrawFloatingIP"} {
		t.Run("move and disassociate, failing "+method+": the old association is back", func(t *testing.T) {
			fx := newFIPFixture(t)
			id, addr := fx.create(t, fx.portX)
			before, _ := readFIP(t, id)
			body := `{"floatingip":{"port_id":"` + fx.portY + `"}}`
			if method == "WithdrawFloatingIP" {
				body = `{"floatingip":{"port_id":null}}`
			}
			fx.failFirst(method, errNode, nil)
			if code, resp := fx.put(id, body); code != http.StatusInternalServerError {
				t.Fatalf("PUT %s: code=%d body=%s; want 500", body, code, resp)
			}
			after, _ := readFIP(t, id)
			if after.Port != before.Port || after.Fixed != before.Fixed || after.Router != before.Router || after.Status != before.Status {
				t.Errorf("row %+v; want the old association %+v", after, before)
			}
			if nat := fx.fake.natOf(addr); len(nat) != 1 || nat[0].fixed != fx.fixedX {
				t.Errorf("NAT of %s: %+v; want converged back to %s", addr, nat, fx.fixedX)
			}
		})
	}

	t.Run("delete, failing the withdrawal: the row and its port forwarding are back as they were", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, addr := fx.create(t, fx.portX)
		pf := seedPortForwarding(t, id, fx.portX, fx.fixedX)
		before, _ := readFIP(t, id)
		fx.failFirst("WithdrawFloatingIP", errNode, nil)
		if code, resp := fx.del("/v2.0/floatingips/" + id); code != http.StatusInternalServerError {
			t.Fatalf("delete: code=%d body=%s; want 500", code, resp)
		}
		if after, ok := readFIP(t, id); !ok || after != before {
			t.Errorf("row %+v (present %v); want it back as %+v", after, ok, before)
		}
		if !portForwardingExists(t, pf) {
			t.Error("the port forwarding did not come back")
		}
		if nat := fx.fake.natOf(addr); len(nat) != 1 || nat[0].fixed != fx.fixedX {
			t.Errorf("NAT of %s: %+v; want converged back to %s", addr, nat, fx.fixedX)
		}
	})

	t.Run("a delete restored as the foreign keys would have", func(t *testing.T) {
		t.Run("its port deleted meanwhile: back unassociated, without the port forwarding", func(t *testing.T) {
			fx := newFIPFixture(t)
			id, _ := fx.create(t, fx.portX)
			pf := seedPortForwarding(t, id, fx.portX, fx.fixedX)
			fx.failFirst("WithdrawFloatingIP", errNode, func() { mustExec(t, "DELETE FROM ports WHERE id = $1", fx.portX) })
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
			fx.failFirst("WithdrawFloatingIP", errNode, func() {
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
			fx.failFirst("EnsureFloatingIP", errNode, func() {
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
			fx.failFirst("EnsureFloatingIP", errNode, func() {
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

	t.Run("a busy node answers 500 within about a second and puts the association back", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, _ := fx.create(t, fx.portX)
		before, _ := readFIP(t, id)
		otherPort, otherFixed, _ := portWithFloatingIP(t, 22)
		entered, release := fx.fake.blockFirst(otherFixed)
		t.Cleanup(release)
		done := make(chan error, 1)
		go func() { done <- fx.svc.RefreshPortFilters(context.Background(), []string{otherPort}) }()
		waitEntered(t, entered)

		start := time.Now()
		code, resp := fx.put(id, `{"floatingip":{"port_id":"`+fx.portY+`"}}`)
		elapsed := time.Since(start)
		release()
		if code != http.StatusInternalServerError {
			t.Fatalf("PUT on a busy node: code=%d body=%s; want 500", code, resp)
		}
		if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
			t.Errorf("PUT took %v; want about one second per lock wait", elapsed)
		}
		if after, _ := readFIP(t, id); after.Port != before.Port {
			t.Errorf("row %+v; want the association back", after)
		}
		if err := <-done; err != nil {
			t.Errorf("the blocked rebuild: %v", err)
		}
	})
}

func hasPrefixAny(ss []string, prefix string) bool {
	for _, s := range ss {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return true
		}
	}
	return false
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
		if fx.fake.hasAddress(addr) || len(fx.fake.natOf(addr)) != 0 {
			t.Errorf("the node still forwards %s", addr)
		}
		if _, ok := fx.fake.filterOf(fx.fixedX); ok {
			t.Errorf("the filter of %s is still there", fx.fixedX)
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

	t.Run("a failed boot's release does the same", func(t *testing.T) {
		fx := newFIPFixture(t)
		instance := sgTestInstance(t)
		mustExec(t, "UPDATE ports SET device_id = $1, device_owner = 'compute:nova' WHERE id = $2", instance, fx.portX)
		id, addr := fx.create(t, fx.portX)
		if err := fx.svc.ReleaseInstancePorts(context.Background(), instance); err != nil {
			t.Fatalf("ReleaseInstancePorts: %v", err)
		}
		assertDisassociated(t, id)
		if fx.fake.hasAddress(addr) {
			t.Errorf("the node still holds %s", addr)
		}
	})

	t.Run("a failed removal: the deletion stands, the floating IP stays disassociated", func(t *testing.T) {
		fx := newFIPFixture(t)
		id, _ := fx.create(t, fx.portX)
		fx.fake.failMethod("WithdrawFloatingIP", errNode)
		if code, resp := fx.del("/v2.0/ports/" + fx.portX); code != http.StatusNoContent {
			t.Fatalf("delete port: code=%d body=%s; want 204", code, resp)
		}
		assertDisassociated(t, id)
	})
}
