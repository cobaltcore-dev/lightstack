package nova

import (
	"context"
	"errors"
	"fmt"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/cobaltcore-dev/lightstack/pkg/paths"
	"github.com/cobaltcore-dev/lightstack/pkg/storage"
	"github.com/google/uuid"
)

// stageBoot prepares a real-mode boot that reaches the domain, as the boot of
// the boot failure tests' success path does: qemu-img for the overlay, the node-local state root redirected into
// the test's directory, and the image's raw base there. It returns the image. Called before the
// service is built: the service's Shutdown, registered after, stops the boot before the root is put
// back.
func stageBoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed — a boot that reaches the domain needs overlay staging")
	}
	orig := paths.Base()
	paths.SetBase(t.TempDir())
	t.Cleanup(func() { paths.SetBase(orig) })
	image := seededImageID(t)
	base := storage.LocalImagePath(image)
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	return image
}

// SPEC-033 round 2, T129 (FR-041; research R32; contracts/port-firewall.md, "Binding moves and boot"):
// a port's firewall lives on the node its guest runs on. A boot binds each of its ports to this node
// and converges their chains before the domain exists; an interface attach binds its port and a
// detach unbinds it. A binding that cannot be converged fails the boot (ERROR, with a fault, no
// domain) or the attach (500, nothing attached). GET /v2.0/ports/{id}'s binding:host_id, an
// administrator's to read, is pinned with Neutron's port unit (T127).

func TestBootBindsItsPortsBeforeTheDomain(t *testing.T) {
	requireDB(t)
	image := stageBoot(t)
	network := seedBootNetwork(t, seededProjectID, "spec033-bind-net")
	port := &neutron.PortInfo{ID: uuid.NewString(), NetworkID: network, MAC: "fa:16:3e:00:00:09", IPAddress: "10.0.0.9"}
	fake := &fakeNeutron{portFor: map[string]*neutron.PortInfo{network: port}}
	svc, r := newBootService(t, fake)
	name := "spec033-bind-" + uuid.NewString()[:6]
	cleanupServerNamed(t, name)

	var mu sync.Mutex
	var domainAtBind, statusAtBind string
	fake.onBind = func(string, string) {
		mu.Lock()
		defer mu.Unlock()
		_ = testPool.QueryRow(context.Background(),
			"SELECT COALESCE(libvirt_domain_id, ''), status FROM instances WHERE name = $1", name).Scan(&domainAtBind, &statusAtBind)
	}
	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q}]}}`, name, image, network)
	if code, resp := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr()); code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s", code, resp)
	}
	if got := awaitInstanceStatus(t, name, "ACTIVE"); got != "ACTIVE" {
		var fault string
		_ = testPool.QueryRow(context.Background(), "SELECT COALESCE(fault_message, '') FROM instances WHERE name = $1", name).Scan(&fault)
		t.Fatalf("server status %q (fault %q); want ACTIVE", got, fault)
	}
	bound := fake.bindings()
	if len(bound) != 1 || bound[0].port != port.ID || bound[0].node != svc.node || svc.node == "" {
		t.Errorf("bindings %+v; want port %s bound to this node %q", bound, port.ID, svc.node)
	}
	mu.Lock()
	defer mu.Unlock()
	if domainAtBind != "" || statusAtBind != "BUILD" {
		t.Errorf("at the binding the server was %s with domain %q; want it still building, without a domain", statusAtBind, domainAtBind)
	}
}

func TestBootFailsWhenItsPortCannotBeBound(t *testing.T) {
	requireDB(t)
	image := stageBoot(t)
	network := seedBootNetwork(t, seededProjectID, "spec033-bind-fail-net")
	port := &neutron.PortInfo{ID: uuid.NewString(), NetworkID: network, MAC: "fa:16:3e:00:00:0a", IPAddress: "10.0.0.10"}
	// The host's own words, as a call to another node answers them: a node, its URL, iptables' output.
	fake := &fakeNeutron{portFor: map[string]*neutron.PortInfo{network: port},
		bindErr: errors.New(`converge ports [p] on node-b: Post "http://10.0.0.2:9696/internal/units": answered 500: iptables-restore: line 3 failed`)}
	_, r := newBootService(t, fake)
	name := "spec033-bind-fail-" + uuid.NewString()[:6]
	cleanupServerNamed(t, name)

	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q}]}}`, name, image, network)
	if code, resp := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr()); code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s", code, resp)
	}
	if got := awaitInstanceStatus(t, name, "ERROR"); got != "ERROR" {
		t.Fatalf("server status %q; want ERROR", got)
	}
	var domain, fault, instance string
	if err := testPool.QueryRow(context.Background(),
		"SELECT id::text, COALESCE(libvirt_domain_id, ''), COALESCE(fault_message, '') FROM instances WHERE name = $1", name,
	).Scan(&instance, &domain, &fault); err != nil {
		t.Fatal(err)
	}
	// The tenant reads the fault: the binding's failure, without the host's words, which the log keeps
	// (SPEC-033 T181).
	if domain != "" || fault != "failed to bind the server's ports" {
		t.Errorf("a failed binding left domain %q and fault %q; want no domain and the binding's fault without the host's words", domain, fault)
	}
	released := fake.releasedInstances()
	if len(released) != 1 || released[0] != instance {
		t.Errorf("released %v; want the server's ports released", released)
	}
}

// bindRecorder is the Neutron service with BindPort observed; the first call fails with failFirst,
// if set, and every other goes to the service.
type bindRecorder struct {
	*neutron.Service
	mu           sync.Mutex
	calls        []bindCall
	failFirst    error
	during       func(portID string) // runs in the failing call, as a concurrent request would
	failWithdraw error               // what WithdrawPorts answers, when set
}

func (b *bindRecorder) WithdrawPorts(ctx context.Context, portIDs []string) (func(), error) {
	if b.failWithdraw != nil {
		return nil, b.failWithdraw
	}
	return b.Service.WithdrawPorts(ctx, portIDs)
}

func (b *bindRecorder) BindPort(ctx context.Context, portID, node string) error {
	b.mu.Lock()
	b.calls = append(b.calls, bindCall{port: portID, node: node})
	first := len(b.calls) == 1
	b.mu.Unlock()
	if first && b.failFirst != nil {
		if b.during != nil {
			b.during(portID)
		}
		return b.failFirst
	}
	return b.Service.BindPort(ctx, portID, node)
}

func (b *bindRecorder) recorded() []bindCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bindCall(nil), b.calls...)
}

func portBinding(t *testing.T, port string) string {
	t.Helper()
	var node *string
	if err := testPool.QueryRow(context.Background(), "SELECT binding_host_id FROM ports WHERE id = $1", port).Scan(&node); err != nil {
		t.Fatal(err)
	}
	if node == nil {
		return ""
	}
	return *node
}

func TestInterfaceAttachBindsAndDetachUnbinds(t *testing.T) {
	requireDB(t)
	r, svc, network := newNovaWithNetwork(t)
	rec := &bindRecorder{Service: network}
	svc.SetNeutronService(rec)
	tenant := tenantProject(t)
	hdr := tenantHdr(tenant)
	server, _ := spec033Server(t, r, tenant)
	net, _ := createTestNetwork(t, tenant)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface", `{"interfaceAttachment":{"net_id":"`+net+`"}}`, hdr)
	if code != http.StatusOK {
		t.Fatalf("attach: code=%d body=%s", code, body)
	}
	calls := rec.recorded()
	if len(calls) != 1 || calls[0].node != svc.node {
		t.Fatalf("bindings %+v; want the new port bound to this node %q", calls, svc.node)
	}
	port := calls[0].port
	if got := portBinding(t, port); got != svc.node {
		t.Errorf("the port's binding %q; want %q", got, svc.node)
	}

	if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+server+"/os-interface/"+port, "", hdr); code != http.StatusAccepted {
		t.Fatalf("detach: code=%d body=%s", code, body)
	}
	if calls := rec.recorded(); len(calls) != 2 || calls[1] != (bindCall{port: port}) {
		t.Errorf("bindings %+v; want the port unbound by the detach", calls)
	}
	if got := portBinding(t, port); got != "" {
		t.Errorf("the port's binding after the detach %q; want none", got)
	}
}

func TestInterfaceAttachFailsWhenItsPortCannotBeBound(t *testing.T) {
	requireDB(t)
	r, svc, network := newNovaWithNetwork(t)
	rec := &bindRecorder{Service: network, failFirst: errors.New("the node could not converge the port's firewall")}
	svc.SetNeutronService(rec)
	tenant := tenantProject(t)
	hdr := tenantHdr(tenant)
	server, _ := spec033Server(t, r, tenant)
	net, _ := createTestNetwork(t, tenant)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface", `{"interfaceAttachment":{"net_id":"`+net+`"}}`, hdr)
	if code != http.StatusInternalServerError {
		t.Fatalf("attach with a failing binding: code=%d body=%s; want 500", code, body)
	}
	var ports, attachments int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM ports WHERE network_id = $1", net).Scan(&ports); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM interface_attachments WHERE instance_id = $1", server).Scan(&attachments); err != nil {
		t.Fatal(err)
	}
	if ports != 0 || attachments != 0 {
		t.Errorf("after a failed attach: %d port(s) on the network, %d attachment(s); want the attach undone", ports, attachments)
	}
}

// SPEC-033 T199 (FR-008): a failed attach's undo is compare-and-set. A port the attach
// made goes and one attached by id is released to its owner, unbound and DOWN, but only while it is
// still this server's: a port another server took meanwhile is left as it is.
func TestInterfaceAttachUndoIsCompareAndSet(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	device := func(t *testing.T, port string) (string, string, bool) {
		t.Helper()
		var id, status *string
		err := testPool.QueryRow(ctx, "SELECT device_id, status FROM ports WHERE id = $1", port).Scan(&id, &status)
		if err != nil {
			return "", "", false
		}
		deref := func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		}
		return deref(id), deref(status), true
	}
	attach := func(t *testing.T, takenMeanwhile bool, body func(net string) (string, string)) (server, port, other string) {
		t.Helper()
		r, svc, network := newNovaWithNetwork(t)
		other = "11111111-2222-4333-8444-555555555555"
		rec := &bindRecorder{Service: network, failFirst: errors.New("the node could not converge the port's firewall")}
		if takenMeanwhile {
			rec.during = func(portID string) { mustExecNova(t, "UPDATE ports SET device_id = $1 WHERE id = $2", other, portID) }
		}
		svc.SetNeutronService(rec)
		tenant := tenantProject(t)
		server, _ = spec033Server(t, r, tenant)
		net, subnet := createTestNetwork(t, tenant)
		reqBody, existing := body(net)
		if existing != "" {
			mustExecNova(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
				VALUES ($1, 'spec033-src27', $2, $3, 'fa:16:3e:27:00:01', 'DOWN', $4::jsonb)`,
				existing, net, tenant, `[{"subnet_id":"`+subnet+`","ip_address":"10.0.0.77"}]`)
			t.Cleanup(func() { mustExecNova(t, "DELETE FROM ports WHERE id = $1", existing) })
		}
		if code, resp := doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface", reqBody, tenantHdr(tenant)); code != http.StatusInternalServerError {
			t.Fatalf("attach with a failing binding: code=%d body=%s; want 500", code, resp)
		}
		calls := rec.recorded()
		if len(calls) == 0 {
			t.Fatal("no binding was tried")
		}
		return server, calls[0].port, other
	}
	existingPort := func(id string) func(string) (string, string) {
		return func(string) (string, string) {
			return `{"interfaceAttachment":{"port_id":"` + id + `"}}`, id
		}
	}

	t.Run("a port attached by id is released, not deleted", func(t *testing.T) {
		_, port, _ := attach(t, false, existingPort("aaaaaaaa-1111-4222-8333-444444444444"))
		if dev, status, ok := device(t, port); !ok || dev != "" || status != "DOWN" {
			t.Errorf("the port after the undo: device %q, status %q, kept %v; want it kept, unattached and DOWN", dev, status, ok)
		}
	})

	t.Run("a port attached by id another server took meanwhile is left", func(t *testing.T) {
		_, port, other := attach(t, true, existingPort("bbbbbbbb-1111-4222-8333-444444444444"))
		if dev, _, ok := device(t, port); !ok || dev != other {
			t.Errorf("the port after the undo: device %q, kept %v; want it the other server's", dev, ok)
		}
	})

	t.Run("the attach's own port another server took meanwhile is left", func(t *testing.T) {
		_, port, other := attach(t, true, func(net string) (string, string) {
			return `{"interfaceAttachment":{"net_id":"` + net + `"}}`, ""
		})
		t.Cleanup(func() { mustExecNova(t, "DELETE FROM ports WHERE id = $1", port) })
		if dev, _, ok := device(t, port); !ok || dev != other {
			t.Errorf("the port after the undo: device %q, kept %v; want it kept, the other server's", dev, ok)
		}
	})
}

// mustExecNova runs a statement of a test's own, failing the test when it fails.
func mustExecNova(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// SPEC-033 T208 (FR-040): a deletion or a detach that unbinds a port attached by id binds
// it back to its node when it does not stand, so the port is not left attached with no chains — its TAP
// falling to the dispatch's drop.
func TestFailedDeleteOrDetachBindsTheAttachedPortBack(t *testing.T) {
	requireDB(t)
	// A server with a port its boot made, and one the tenant's, attached by id: it keeps its owner and
	// stays the tenant's, so a deletion unbinds it rather than withdrawing it.
	setup := func(t *testing.T) (*gin.Engine, *Service, *bindRecorder, string, string, map[string]string) {
		r, svc, network := newNovaWithNetwork(t)
		rec := &bindRecorder{Service: network}
		svc.SetNeutronService(rec)
		tenant := tenantProject(t)
		hdr := tenantHdr(tenant)
		server, _ := spec033Server(t, r, tenant)
		net, subnet := createTestNetwork(t, tenant)
		allocateTestPort(t, network, net, tenant, server, nil)
		own := uuid.NewString()
		mustExecNova(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
			VALUES ($1, 'spec033-src31', $2, $3, 'fa:16:3e:31:00:01', 'DOWN', $4::jsonb)`,
			own, net, tenant, `[{"subnet_id":"`+subnet+`","ip_address":"10.0.0.88"}]`)
		t.Cleanup(func() { mustExecNova(t, "DELETE FROM ports WHERE id = $1", own) })
		if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface", `{"interfaceAttachment":{"port_id":"`+own+`"}}`, hdr); code != http.StatusOK {
			t.Fatalf("attach: code=%d body=%s", code, body)
		}
		attached := rec.recorded()[0].port
		if got := portBinding(t, attached); got != svc.node {
			t.Fatalf("the attached port's binding %q; want %q", got, svc.node)
		}
		return r, svc, rec, server, attached, hdr
	}

	t.Run("a deletion whose ports cannot be withdrawn", func(t *testing.T) {
		r, svc, rec, server, attached, hdr := setup(t)
		rec.failWithdraw = errors.New("the node could not withdraw the server's ports")
		if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+server, "", hdr); code != http.StatusInternalServerError {
			t.Fatalf("delete: code=%d body=%s; want 500", code, body)
		}
		if got := portBinding(t, attached); got != svc.node {
			t.Errorf("the attached port's binding after the failed delete %q; want %q back", got, svc.node)
		}
	})

	t.Run("a detach whose records cannot be written", func(t *testing.T) {
		r, svc, _, server, attached, hdr := setup(t)
		release := dbtest.Unavailable(t, testPool, "interface_attachments")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req := httptest.NewRequest(http.MethodDelete, "/v2.1/servers/"+server+"/os-interface/"+attached, nil).WithContext(ctx)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		cancel()
		release()
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("detach: code=%d body=%s; want 500", w.Code, w.Body.String())
		}
		if got := portBinding(t, attached); got != svc.node {
			t.Errorf("the port's binding after the failed detach %q; want %q back", got, svc.node)
		}
	})
}
