package nova

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T251 (FR-049): a server's deletion lets its boot ports' port forwardings go as a port's deletion
// does: the floating IP of a port forwarding whose internal port goes is freed with it — no router, DOWN.
// It kept its router, so clearing that router's gateway answered 409 and the floating IP's next forwarding
// went through the stale router.
func TestServerDeletionFreesItsForwardingsFloatingIP(t *testing.T) {
	requireDB(t)
	r, rec := newDeleteRig(t)
	server, netID := spec033Server(t, r, seededProjectID)
	port := allocateTestPort(t, rec.Service, netID, seededProjectID, server, nil)
	router, fip, pf := uuid.NewString(), uuid.NewString(), uuid.NewString()
	mustExecNova(t, `INSERT INTO routers (id, name, project_id) VALUES ($1, 'spec033-t251', $2)`, router, seededProjectID)
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM routers WHERE id = $1", router) })
	mustExecNova(t, `INSERT INTO floating_ips (id, project_id, floating_ip_address, router_id, status)
		VALUES ($1, $2, '198.19.7.7', $3, 'ACTIVE')`, fip, seededProjectID, router)
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM floating_ips WHERE id = $1", fip) })
	mustExecNova(t, `INSERT INTO port_forwardings (id, project_id, floatingip_id, internal_port_id, internal_ip_address,
		external_port, internal_port, protocol) VALUES ($1, $2, $3, $4, $5, 2222, 22, 'tcp')`,
		pf, seededProjectID, fip, port, testPortFixedIP(t, port))

	if code, body := deleteServerReq(r, server); code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s", code, body)
	}
	if s := readFIPState(t, fip); s.router != nil || s.status != "DOWN" {
		t.Errorf("floating IP after the deletion: router=%v status=%s; want no router, DOWN", s.router, s.status)
	}
}

// SPEC-033 T252 (FR-040): a deletion that loses a race — the instance gone when its transaction locks it —
// leaves the ports it unbound as the winning deletion left them. Binding them back unconditionally gave a
// port no server uses a node, or moved one attached to another server meanwhile back to the old node.
func TestALosingDeletionLeavesTheDetachedPortAlone(t *testing.T) {
	requireDB(t)
	r, svc, network := newNovaWithNetwork(t)
	tenant := tenantProject(t)
	hdr := tenantHdr(tenant)
	server, _ := spec033Server(t, r, tenant)
	net, subnet := createTestNetwork(t, tenant)
	own := uuid.NewString()
	mustExecNova(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-t252', $2, $3, 'fa:16:3e:52:00:01', 'DOWN', $4::jsonb)`,
		own, net, tenant, `[{"subnet_id":"`+subnet+`","ip_address":"10.0.0.89"}]`)
	t.Cleanup(func() { mustExecNova(t, "DELETE FROM ports WHERE id = $1", own) })
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+server+"/os-interface",
		`{"interfaceAttachment":{"port_id":"`+own+`"}}`, hdr); code != http.StatusOK {
		t.Fatalf("attach: code=%d body=%s", code, body)
	}
	if got := portBinding(t, own); got != svc.node {
		t.Fatalf("binding %q; want %q", got, svc.node)
	}
	// The winning deletion commits while this one unbinds the port: it detaches the port and deletes the
	// instance, as deleteServerRecords does.
	won := false
	svc.SetNeutronService(&raceBinder{bindRecorder: &bindRecorder{Service: network}, onUnbind: func() {
		if won {
			return
		}
		won = true
		mustExecNova(t, `UPDATE ports SET device_id = NULL, device_owner = NULL, binding_host_id = NULL, status = 'DOWN'
			WHERE id = $1`, own)
		mustExecNova(t, `DELETE FROM instances WHERE id = $1`, server)
	}})
	if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+server, "", hdr); code != http.StatusNotFound {
		t.Fatalf("the losing deletion: code=%d body=%s; want 404", code, body)
	}
	if got := portBinding(t, own); got != "" {
		t.Errorf("the detached port's binding %q after the losing deletion; want none", got)
	}
}

// raceBinder runs onUnbind once a port has been unbound, as a concurrent request would.
type raceBinder struct {
	*bindRecorder
	onUnbind func()
}

func (b *raceBinder) BindPort(ctx context.Context, portID, node string) error {
	err := b.bindRecorder.BindPort(ctx, portID, node)
	if node == "" && err == nil {
		b.onUnbind()
	}
	return err
}
