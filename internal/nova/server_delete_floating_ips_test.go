package nova

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 T041 (FR-008d, FR-008e; research R25): a server's deletion, by DELETE or by the forceDelete
// action, disassociates the floating IPs of the ports its boot created in the deleting transaction, and
// converges them on this node before the answer. A port attached by id keeps its floating IP.

// convergeRecorder is the Neutron service with ConvergeDisassociated observed.
type convergeRecorder struct {
	*neutron.Service
	mu    sync.Mutex
	calls [][]string
}

func (r *convergeRecorder) ConvergeDisassociated(ctx context.Context, was []neutron.FloatingIPAssociation) {
	r.mu.Lock()
	r.calls = append(r.calls, floatingAddresses(was))
	r.mu.Unlock()
	r.Service.ConvergeDisassociated(ctx, was)
}

func (r *convergeRecorder) converged() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func newDeleteRig(t *testing.T) (*gin.Engine, *convergeRecorder) {
	t.Helper()
	r, svc, network := newNovaWithNetwork(t)
	rec := &convergeRecorder{Service: network}
	svc.SetNeutronService(rec)
	return r, rec
}

// portFixedIP reads a port's first fixed IP.
func testPortFixedIP(t *testing.T, portID string) string {
	t.Helper()
	var raw []byte
	if err := testPool.QueryRow(t.Context(), "SELECT fixed_ips FROM ports WHERE id = $1", portID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ips []struct {
		IPAddress string `json:"ip_address"`
	}
	if err := json.Unmarshal(raw, &ips); err != nil || len(ips) == 0 {
		t.Fatalf("fixed IPs of %s: %s (%v)", portID, raw, err)
	}
	return ips[0].IPAddress
}

// associateFIP records a floating IP associated with a port, and returns its id and address.
func associateFIP(t *testing.T, portID string) (string, string) {
	t.Helper()
	id := uuid.NewString()
	addr := fmt.Sprintf("198.18.%d.%d", rand.IntN(256), 1+rand.IntN(254))
	if _, err := testPool.Exec(t.Context(), `
		INSERT INTO floating_ips (id, project_id, floating_ip_address, fixed_ip_address, port_id, status)
		VALUES ($1, $2, $3, $4, $5, 'ACTIVE')`, id, seededProjectID, addr, testPortFixedIP(t, portID), portID); err != nil {
		t.Fatalf("seed floating IP: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM floating_ips WHERE id = $1", id) })
	return id, addr
}

type fipState struct {
	port, fixed, router *string
	status              string
}

func readFIPState(t *testing.T, id string) fipState {
	t.Helper()
	var s fipState
	if err := testPool.QueryRow(t.Context(),
		"SELECT port_id::text, fixed_ip_address, router_id::text, status FROM floating_ips WHERE id = $1", id,
	).Scan(&s.port, &s.fixed, &s.router, &s.status); err != nil {
		t.Fatalf("read floating IP %s: %v", id, err)
	}
	return s
}

func wantDisassociated(t *testing.T, id string) {
	t.Helper()
	if s := readFIPState(t, id); s.port != nil || s.fixed != nil || s.router != nil || s.status != "DOWN" {
		t.Errorf("floating IP %s: %+v; want no port, fixed IP or router, and DOWN", id, s)
	}
}

func deleteServerReq(r *gin.Engine, server string) (int, string) {
	return doReq(r, http.MethodDelete, "/v2.1/servers/"+server, "", testAuthHdr(seededProjectID, seededUserID))
}

func forceDeleteReq(r *gin.Engine, server string) (int, string) {
	return doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/action", `{"forceDelete": null}`,
		testAuthHdr(seededProjectID, seededUserID))
}

func TestServerDeletion_FloatingIPs(t *testing.T) {
	requireDB(t)

	t.Run("a deleted server's boot port takes its floating IP's association with it", func(t *testing.T) {
		r, rec := newDeleteRig(t)
		server, netID := spec033Server(t, r, seededProjectID)
		port := allocateTestPort(t, rec.Service, netID, seededProjectID, server, nil)
		fip, addr := associateFIP(t, port)

		if code, body := deleteServerReq(r, server); code != http.StatusNoContent {
			t.Fatalf("delete: code=%d body=%s; want 204", code, body)
		}
		wantDisassociated(t, fip)
		if calls := rec.converged(); len(calls) != 1 || !slices.Equal(calls[0], []string{addr}) {
			t.Errorf("ConvergeDisassociated calls: %v; want one with %s", calls, addr)
		}
	})

	t.Run("a port attached by id survives with its floating IP", func(t *testing.T) {
		r, rec := newDeleteRig(t)
		server, netID := spec033Server(t, r, seededProjectID)
		allocateTestPort(t, rec.Service, netID, seededProjectID, server, nil)
		tenantPort := freePortWithGroups(t, netID, seededProjectID)
		if _, err := testPool.Exec(t.Context(), `UPDATE ports SET device_id = $1, fixed_ips = '[{"ip_address":"10.33.0.250"}]'::jsonb WHERE id = $2`, server, tenantPort); err != nil {
			t.Fatal(err)
		}
		fip, _ := associateFIP(t, tenantPort)

		if code, body := deleteServerReq(r, server); code != http.StatusNoContent {
			t.Fatalf("delete: code=%d body=%s; want 204", code, body)
		}
		if s := readFIPState(t, fip); s.port == nil || *s.port != tenantPort || s.status != "ACTIVE" {
			t.Errorf("the tenant port's floating IP: %+v; want it still associated", s)
		}
	})

	t.Run("forceDelete deletes as DELETE does", func(t *testing.T) {
		r, rec := newDeleteRig(t)
		server, netID := spec033Server(t, r, seededProjectID)
		port := allocateTestPort(t, rec.Service, netID, seededProjectID, server, nil)
		fip, addr := associateFIP(t, port)
		volume := uuid.NewString()
		if _, err := testPool.Exec(t.Context(),
			`INSERT INTO volumes (id, name, size_gb, status, project_id, attached_to_instance_id) VALUES ($1, 'spec033-vol', 1, 'in-use', $2, $3)`,
			volume, seededProjectID, server); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(t.Context(),
			`INSERT INTO volume_attachments (volume_id, instance_id, device, delete_on_termination) VALUES ($1, $2, '/dev/vdb', false)`,
			volume, server); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM volumes WHERE id = $1", volume) })

		if code, body := forceDeleteReq(r, server); code != http.StatusNoContent {
			t.Fatalf("forceDelete: code=%d body=%s; want 204", code, body)
		}
		var ports int
		if err := testPool.QueryRow(t.Context(), "SELECT count(*) FROM ports WHERE id = $1", port).Scan(&ports); err != nil || ports != 0 {
			t.Errorf("the boot port: %d rows (%v); want it deleted", ports, err)
		}
		wantDisassociated(t, fip)
		if calls := rec.converged(); len(calls) != 1 || !slices.Equal(calls[0], []string{addr}) {
			t.Errorf("ConvergeDisassociated calls: %v; want one with %s", calls, addr)
		}
		var status string
		var attached *string
		if err := testPool.QueryRow(t.Context(), "SELECT status, attached_to_instance_id::text FROM volumes WHERE id = $1", volume).Scan(&status, &attached); err != nil {
			t.Fatal(err)
		}
		if status != "available" || attached != nil {
			t.Errorf("the volume: %s attached to %v; want available and unattached", status, attached)
		}
	})

	t.Run("two deletions of one server: the second answers 404", func(t *testing.T) {
		r, _ := newDeleteRig(t)
		for _, order := range [][2]func(*gin.Engine, string) (int, string){
			{deleteServerReq, forceDeleteReq},
			{forceDeleteReq, deleteServerReq},
		} {
			server, _ := spec033Server(t, r, seededProjectID)
			if code, body := order[0](r, server); code != http.StatusNoContent {
				t.Fatalf("first deletion: code=%d body=%s; want 204", code, body)
			}
			if code, body := order[1](r, server); code != http.StatusNotFound {
				t.Errorf("second deletion: code=%d body=%s; want 404", code, body)
			}
		}
	})

	t.Run("the deletion takes the instance before the ports", func(t *testing.T) {
		dbtest.RequirePostgres(t)
		r, rec := newDeleteRig(t)
		server, netID := spec033Server(t, r, seededProjectID)
		port := allocateTestPort(t, rec.Service, netID, seededProjectID, server, nil)
		ctx := context.Background()

		holder, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer holder.Rollback(ctx) //nolint:errcheck // a no-op after the commit
		var one int
		if err := holder.QueryRow(ctx, "SELECT 1 FROM instances WHERE id = $1 FOR UPDATE", server).Scan(&one); err != nil {
			t.Fatal(err)
		}
		type result struct {
			code int
			body string
		}
		done := make(chan result, 1)
		go func() {
			code, body := deleteServerReq(r, server)
			done <- result{code, body}
		}()
		time.Sleep(300 * time.Millisecond)

		other, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Rollback(ctx) //nolint:errcheck // a no-op after the commit
		//sqlcheck:postgres-only
		if _, err := other.Exec(ctx, "SET LOCAL lock_timeout = '500ms'"); err != nil {
			t.Fatal(err)
		}
		var locked string
		if err := other.QueryRow(ctx, "SELECT id::text FROM ports WHERE id = $1 FOR UPDATE", port).Scan(&locked); err != nil {
			t.Errorf("locking the server's port while the deletion waits for the instance: %v; want it free", err)
		}
		if err := other.Commit(ctx); err != nil {
			t.Logf("commit the port's lock: %v", err)
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if res := <-done; res.code != http.StatusNoContent {
			t.Errorf("delete: code=%d body=%s; want 204", res.code, res.body)
		}
	})
}
