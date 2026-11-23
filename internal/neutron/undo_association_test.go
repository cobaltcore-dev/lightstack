package neutron

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T270 (FR-008, FR-028, FR-029, FR-039): an undo or a restore puts back only what the association
// rules still allow when the node call failed. A floating IP's deletion restored the floating IP on a fixed
// IP another floating IP of the network had taken meanwhile; a gateway's undo removed a gateway a floating IP
// had come to go through. The later change stays: the floating IP comes back unassociated, the gateway stays.
func TestUndoKeepsWhatTheAssociationRulesForbidPuttingBack(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	onceDuring := func(during func()) func(holderCall[networking.RouterState]) error {
		var once sync.Once
		return func(holderCall[networking.RouterState]) error {
			var err error
			once.Do(func() {
				during()
				err = errNode
			})
			return err
		}
	}

	t.Run("a floating IP's deletion is restored unassociated when its fixed IP has another", func(t *testing.T) {
		rig := newUnitRig(t)
		router, network := rig.router(t)
		p, address := rig.port(t, network)
		fip, _ := rig.floatingIP(t, p)
		rig.routers.hook = onceDuring(func() {
			mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, port_id, fixed_ip_address, router_id, status)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE')`,
				uuid.NewString(), seededProjectID, rig.external, rig.externalPrefix+".98", p, address, router)
		})
		if code, body := rig.do(t, http.MethodDelete, "/v2.0/floatingips/"+fip, ""); code != http.StatusInternalServerError {
			t.Fatalf("delete with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		var port *string
		if err := testPool.QueryRow(ctx, "SELECT port_id::text FROM floating_ips WHERE id = $1", fip).Scan(&port); err != nil {
			t.Fatalf("the floating IP was not restored: %v", err)
		}
		if port != nil {
			t.Errorf("restored on port %s; want unassociated: its fixed IP has another floating IP of the network", *port)
		}
	})

	t.Run("a gateway's undo keeps a gateway a floating IP has come to go through", func(t *testing.T) {
		rig := newUnitRig(t)
		resp := rig.must(t, http.StatusCreated, http.MethodPost, "/v2.0/routers", `{"router":{"name":"spec033-t270"}}`)
		router := createdID(t, resp, "router")
		t.Cleanup(func() {
			mustExec(t, "DELETE FROM floating_ips WHERE router_id = $1", router)
			mustExec(t, "DELETE FROM ports WHERE device_id = $1", router)
			mustExec(t, "DELETE FROM routers WHERE id = $1", router)
		})
		rig.routers.hook = onceDuring(func() {
			mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, router_id, status)
				VALUES ($1, $2, $3, $4, $5, 'ACTIVE')`, uuid.NewString(), seededProjectID, rig.external, rig.externalPrefix+".97", router)
		})
		admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
		if code, body := doJSONReq(rig.r, http.MethodPut, "/v2.0/routers/"+router,
			`{"router":{"external_gateway_info":{"network_id":"`+rig.external+`"}}}`, admin); code != http.StatusInternalServerError {
			t.Fatalf("set the gateway with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		if n := len(gatewayPorts(t, router)); n != 1 {
			t.Errorf("%d gateway port(s) after the undo; want the gateway kept for the floating IP that goes through it", n)
		}
	})

	// SPEC-033 T313: an update's undo re-checks the association it puts back, as the deletion's restore does.
	portOf := func(t *testing.T, fip string) *string {
		t.Helper()
		var port *string
		if err := testPool.QueryRow(ctx, "SELECT port_id::text FROM floating_ips WHERE id = $1", fip).Scan(&port); err != nil {
			t.Fatalf("read floating IP %s: %v", fip, err)
		}
		return port
	}
	// failedUpdate sends a floating IP update whose router unit fails once, running during first.
	failedUpdate := func(t *testing.T, rig *unitRig, fip, body string, during func()) {
		t.Helper()
		rig.routers.hook = onceDuring(during)
		if code, resp := rig.do(t, http.MethodPut, "/v2.0/floatingips/"+fip, body); code != http.StatusInternalServerError {
			t.Fatalf("update with a failing router unit: code=%d body=%s; want 500", code, resp)
		}
	}
	// takeFixedIP gives the fixed IP another floating IP of the external network, as a concurrent association
	// commits it, and returns that floating IP.
	takeFixedIP := func(t *testing.T, rig *unitRig, router, port, fixed, last string) string {
		t.Helper()
		other := uuid.NewString()
		mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address, port_id, fixed_ip_address, router_id, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE')`, other, seededProjectID, rig.external, rig.externalPrefix+"."+last, port, fixed, router)
		return other
	}
	// wantUnassociated checks the undone floating IP came back without a port, and the later one kept its own.
	wantUnassociated := func(t *testing.T, fip, other, port string) {
		t.Helper()
		if got := portOf(t, fip); got != nil {
			t.Errorf("undone onto port %s; want it unassociated: the association it had is no longer allowed", *got)
		}
		if other == "" {
			return
		}
		if got := portOf(t, other); got == nil || *got != port {
			t.Errorf("the later floating IP's port is %v; want it kept on %s", got, port)
		}
	}

	t.Run("a floating IP's move is undone unassociated when its old fixed IP has another", func(t *testing.T) {
		rig := newUnitRig(t)
		router, network := rig.router(t)
		p, address := rig.port(t, network)
		q, _ := rig.port(t, network)
		fip, _ := rig.floatingIP(t, p)
		var other string
		failedUpdate(t, rig, fip, `{"floatingip":{"port_id":"`+q+`"}}`, func() { other = takeFixedIP(t, rig, router, p, address, "96") })
		wantUnassociated(t, fip, other, p)
	})

	t.Run("a floating IP's disassociation is undone unassociated when its old fixed IP has another", func(t *testing.T) {
		rig := newUnitRig(t)
		router, network := rig.router(t)
		p, address := rig.port(t, network)
		fip, _ := rig.floatingIP(t, p)
		var other string
		failedUpdate(t, rig, fip, `{"floatingip":{"port_id":null}}`, func() { other = takeFixedIP(t, rig, router, p, address, "95") })
		wantUnassociated(t, fip, other, p)
	})

	t.Run("a floating IP's move is undone unassociated when its old port is gone", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		p, _ := rig.port(t, network)
		q, _ := rig.port(t, network)
		fip, _ := rig.floatingIP(t, p)
		failedUpdate(t, rig, fip, `{"floatingip":{"port_id":"`+q+`"}}`, func() { mustExec(t, "DELETE FROM ports WHERE id = $1", p) })
		wantUnassociated(t, fip, "", "")
	})

	t.Run("a floating IP's move is undone unassociated when its old port no longer has the fixed IP", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		p, _ := rig.port(t, network)
		q, _ := rig.port(t, network)
		fip, _ := rig.floatingIP(t, p)
		failedUpdate(t, rig, fip, `{"floatingip":{"port_id":"`+q+`"}}`, func() {
			mustExec(t, "UPDATE ports SET fixed_ips = $1::jsonb WHERE id = $2", "[]", p)
		})
		wantUnassociated(t, fip, "", "")
	})

	t.Run("a floating IP's move is undone unassociated when no router joins its old fixed IP to the network", func(t *testing.T) {
		rig := newUnitRig(t)
		first, firstNetwork := rig.router(t)
		_, secondNetwork := rig.router(t)
		p, _ := rig.port(t, firstNetwork)
		q, _ := rig.port(t, secondNetwork)
		fip, _ := rig.floatingIP(t, p)
		failedUpdate(t, rig, fip, `{"floatingip":{"port_id":"`+q+`"}}`, func() {
			mustExec(t, "DELETE FROM router_interfaces WHERE router_id = $1", first)
		})
		wantUnassociated(t, fip, "", "")
	})
}

// lockLog records each transaction's FOR UPDATE reads, by table, in order, and the statement that wrote
// floating_ips, so a test reads the order an undo or a restore takes its locks in. The undo and the restore
// lock as an association does — the port, then its router (routerForFloatingIP), then the floating IP's row
// — so they serialise with associations of the same fixed IP and cannot deadlock with a port's deletion,
// which holds the port before it clears the floating IPs naming it (SPEC-033 T313, research R20).
type lockLog struct {
	mu  sync.Mutex
	txs []*lockTx
}

// wrote is the last transaction whose statement writing floating_ips starts with verb.
func (l *lockLog) wrote(t *testing.T, verb string) *lockTx {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.txs) - 1; i >= 0; i-- {
		if strings.HasPrefix(l.txs[i].write, verb) {
			return l.txs[i]
		}
	}
	t.Fatalf("no transaction ran %s on floating_ips", verb)
	return nil
}

type lockDB struct {
	database.DB
	log *lockLog
}

func (d lockDB) Begin(ctx context.Context) (database.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	rec := &lockTx{Tx: tx}
	d.log.mu.Lock()
	d.log.txs = append(d.log.txs, rec)
	d.log.mu.Unlock()
	return rec, nil
}

type lockTx struct {
	database.Tx
	locked []string // the tables FOR UPDATE reads locked, in order
	write  string   // the verb of the statement that wrote floating_ips
}

var (
	lockedTable   = regexp.MustCompile(`(?is)\bFROM\s+(\w+)\b.*\bFOR UPDATE\b`)
	floatingWrite = regexp.MustCompile(`(?is)^\s*(UPDATE|INSERT INTO)\s+floating_ips\b`)
)

func (t *lockTx) note(query string) {
	if m := lockedTable.FindStringSubmatch(query); m != nil {
		t.locked = append(t.locked, m[1])
	}
	if m := floatingWrite.FindStringSubmatch(query); m != nil {
		t.write = strings.ToUpper(m[1])
	}
}

func (t *lockTx) Query(ctx context.Context, query string, args ...any) (database.Rows, error) {
	t.note(query)
	return t.Tx.Query(ctx, query, args...)
}

func (t *lockTx) QueryRow(ctx context.Context, query string, args ...any) database.Row {
	t.note(query)
	return t.Tx.QueryRow(ctx, query, args...)
}

func (t *lockTx) Exec(ctx context.Context, query string, args ...any) (database.Result, error) {
	t.note(query)
	return t.Tx.Exec(ctx, query, args...)
}

func TestUndoAndRestoreLockAsAnAssociationDoes(t *testing.T) {
	requireDB(t)
	failOnce := func(rig *unitRig) {
		var once sync.Once
		rig.routers.hook = func(holderCall[networking.RouterState]) error {
			var err error
			once.Do(func() { err = errNode })
			return err
		}
	}

	t.Run("an update's undo", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		p, _ := rig.port(t, network)
		q, _ := rig.port(t, network)
		fip, _ := rig.floatingIP(t, p)
		log := &lockLog{}
		rig.api.pool = lockDB{testPool, log}
		failOnce(rig)
		if code, body := rig.do(t, http.MethodPut, "/v2.0/floatingips/"+fip, `{"floatingip":{"port_id":"`+q+`"}}`); code != http.StatusInternalServerError {
			t.Fatalf("update with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		if got := strings.Join(log.wrote(t, "UPDATE").locked, ","); got != "ports,routers,floating_ips" {
			t.Errorf("the undo locked %q; want ports,routers,floating_ips: the old port, its router, then the row", got)
		}
	})

	t.Run("a deletion's restore", func(t *testing.T) {
		rig := newUnitRig(t)
		_, network := rig.router(t)
		p, _ := rig.port(t, network)
		fip, _ := rig.floatingIP(t, p)
		log := &lockLog{}
		rig.api.pool = lockDB{testPool, log}
		failOnce(rig)
		if code, body := rig.do(t, http.MethodDelete, "/v2.0/floatingips/"+fip, ""); code != http.StatusInternalServerError {
			t.Fatalf("delete with a failing router unit: code=%d body=%s; want 500", code, body)
		}
		if got := strings.Join(log.wrote(t, "INSERT INTO").locked, ","); !strings.HasPrefix(got, "ports,routers") {
			t.Errorf("the restore locked %q; want the port, then its router, first, as a create takes them", got)
		}
	})
}
