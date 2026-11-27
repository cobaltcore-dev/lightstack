package nova

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T180: a server's deletion withdrew, unbound, detached and deleted
// the ports naming it in device_id, and took the floating IPs off them, whichever project made them —
// device_id is any port creator's to set. Another project's port naming the server is not the server's:
// the deletion leaves it, and its floating IP, as they were.
func TestServerDeletionLeavesAnotherProjectsPortsNamingIt(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	fake := &fakeNeutron{}
	svc, r := newBootService(t, fake)
	instance, _ := seedLifecycleInstance(t, svc.vmManager, "src8-delete-"+uuid.NewString()[:6])
	network := seedBootNetwork(t, seededProjectID, "src8-delete-net")
	other := tenantProject(t)
	otherNetwork := seedBootNetwork(t, other, "src8-delete-other")

	seedPort := func(project, network, owner string) string {
		id := uuid.NewString()
		if _, err := testPool.Exec(ctx, `
			INSERT INTO ports (id, name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
			VALUES ($1, 'src8-port', $2, $3, $4, NULLIF($5, ''), $6, 'ACTIVE', '[]'::jsonb)`,
			id, network, project, instance, owner, fmt.Sprintf("fa:16:3e:58:%02x:%02x", rand.IntN(256), rand.IntN(256))); err != nil {
			t.Fatalf("seeding a port: %v", err)
		}
		t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM ports WHERE id = $1", id) })
		return id
	}
	own := seedPort(seededProjectID, network, "compute:nova")
	foreignNova := seedPort(other, otherNetwork, "compute:nova")
	foreignPlain := seedPort(other, otherNetwork, "")
	fip := uuid.NewString()
	if _, err := testPool.Exec(ctx, `INSERT INTO floating_ips (id, project_id, floating_ip_address, fixed_ip_address, port_id, status)
		VALUES ($1, $2, $3, '10.58.0.9', $4, 'ACTIVE')`, fip, other, fmt.Sprintf("198.18.58.%d", 1+rand.IntN(254)), foreignNova); err != nil {
		t.Fatalf("seeding a floating IP: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM floating_ips WHERE id = $1", fip) })

	if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+instance, "", bootHdr()); code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s; want 204", code, body)
	}

	var n int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM ports WHERE id = $1", own).Scan(&n); err != nil || n != 0 {
		t.Errorf("the server's own port: %d left (%v); want it deleted", n, err)
	}
	for _, foreign := range []struct{ id, owner string }{{foreignNova, "compute:nova"}, {foreignPlain, ""}} {
		var device, owner string
		if err := testPool.QueryRow(ctx, "SELECT COALESCE(device_id::text, ''), COALESCE(device_owner, '') FROM ports WHERE id = $1",
			foreign.id).Scan(&device, &owner); err != nil {
			t.Errorf("another project's %q port naming the server: %v; want it kept", foreign.owner, err)
			continue
		}
		if device != instance || owner != foreign.owner {
			t.Errorf("another project's port: device %q owner %q; want %q and %q, as it was", device, owner, instance, foreign.owner)
		}
	}
	var fipPort *string
	if err := testPool.QueryRow(ctx, "SELECT port_id::text FROM floating_ips WHERE id = $1", fip).Scan(&fipPort); err != nil || fipPort == nil || *fipPort != foreignNova {
		t.Errorf("the other project's floating IP: port %v (%v); want still %s", fipPort, err, foreignNova)
	}
	for _, withdrawal := range fake.withdrawn {
		if slices.Contains(withdrawal, foreignNova) {
			t.Errorf("withdrawals %v took another project's port %s", fake.withdrawn, foreignNova)
		}
	}
	for _, b := range fake.bindings() {
		if b.port == foreignPlain {
			t.Errorf("bindings %+v unbound another project's port %s", fake.bindings(), foreignPlain)
		}
	}
}
