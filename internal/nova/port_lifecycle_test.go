package nova

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// Review 2026-10-13 N-8: DeleteServer used a blanket
// `DELETE FROM ports WHERE device_id = $1`, which destroyed ports the TENANT
// created and attached (os-interface with port_id) along with the ones Nova
// allocated for the boot. Upstream deletes only the ports it created and
// detaches the rest — the tenant's port survives the server, parked DOWN with
// no device, reusable or deletable by its owner.
//
// The tenant port is attached through the real os-interface flow (round 2 of
// this review): the fix keys on device_owner='compute:nova' marking Nova's own
// ports, and AttachInterface used to stamp that owner on a tenant's port too —
// which would have made the discriminator, and this behaviour, unreachable.
func TestDeleteServerDetachesTenantPortsButDeletesItsOwn(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	netID := seedBootNetwork(t, seededProjectID, "n8-net")

	var instanceID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ('n8-server', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE')
		 RETURNING id::text`, seededProjectID, seededUserID).Scan(&instanceID); err != nil {
		t.Fatalf("seeding instance: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM ports WHERE network_id::text = $1`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id::text = $1`, instanceID)
	})

	seedPort := func(owner string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO ports (name, network_id, project_id, device_id, device_owner, mac_address, status, fixed_ips)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, 'ACTIVE', '[]'::jsonb) RETURNING id::text`,
			"n8-"+owner, netID, seededProjectID, instanceID, owner, fmt.Sprintf("52:54:00:00:00:%02x", len(owner))).Scan(&id); err != nil {
			t.Fatalf("seeding %s port: %v", owner, err)
		}
		return id
	}
	novaPort := seedPort("compute:nova")
	tenantPort := seedPort("") // tenant-created, attached via os-interface

	r := newNovaTestRouter()
	owner := map[string]string{
		"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}

	// Detach the tenant port from the instance first, then re-attach it
	// through the real os-interface flow, so the test exercises the handler
	// that used to stamp it compute:nova.
	if _, err := testPool.Exec(ctx,
		`UPDATE ports SET device_id = NULL WHERE id::text = $1`, tenantPort); err != nil {
		t.Fatalf("freeing the tenant port for attach: %v", err)
	}
	attachBody := fmt.Sprintf(`{"interfaceAttachment":{"port_id":%q}}`, tenantPort)
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+instanceID+"/os-interface", attachBody, owner); code != http.StatusOK {
		t.Fatalf("attaching the tenant port via os-interface: code=%d body=%s", code, body)
	}

	// The attach must NOT have claimed the port as Nova's: device_owner is the
	// ownership discriminator server delete keys on.
	var deviceOwner string
	if err := testPool.QueryRow(ctx,
		`SELECT COALESCE(device_owner, '') FROM ports WHERE id::text = $1`, tenantPort).Scan(&deviceOwner); err != nil {
		t.Fatalf("reading the attached port's owner: %v", err)
	}
	if deviceOwner == "compute:nova" {
		t.Fatal("os-interface stamped a tenant-created port as compute:nova; it would be deleted with the server instead of detached")
	}

	if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+instanceID, "", owner); code != http.StatusNoContent {
		t.Fatalf("deleting the server: code=%d body=%s", code, body)
	}

	// Nova's own port is gone with the server.
	var novaPortGone bool
	if err := testPool.QueryRow(ctx,
		`SELECT NOT EXISTS(SELECT 1 FROM ports WHERE id::text = $1)`, novaPort).Scan(&novaPortGone); err != nil {
		t.Fatalf("checking the nova-owned port: %v", err)
	}
	if !novaPortGone {
		t.Error("the boot-allocated port (device_owner compute:nova) survived the server delete; it must be deleted with it")
	}

	// The tenant's port survives, detached: no device, parked DOWN.
	var deviceID, status string
	var exists bool
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ports WHERE id::text = $1)`, tenantPort).Scan(&exists); err != nil || !exists {
		t.Fatalf("the tenant-created port must survive the server delete (exists=%v err=%v)", exists, err)
	}
	if err := testPool.QueryRow(ctx, `
		SELECT COALESCE(device_id::text, ''), status
		FROM ports WHERE id::text = $1`, tenantPort).Scan(&deviceID, &status); err != nil {
		t.Fatalf("reading the surviving port: %v", err)
	}
	if deviceID != "" {
		t.Errorf("the surviving port is still attached (device_id=%q); want it detached", deviceID)
	}
	if status != "DOWN" {
		t.Errorf("the surviving port's status = %q; want DOWN (detached, as upstream parks it)", status)
	}
}

// The net_id half of the same discriminator: a port Nova creates for an
// os-interface attach IS Nova's, and must be deleted with the server.
func TestDeleteServerDeletesInterfaceAttachCreatedPort(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	netID := seedBootNetwork(t, seededProjectID, "n8b-net")
	// The net_id path needs a subnet to allocate from.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (name, network_id, project_id, cidr)
		 VALUES ('n8b-sub', $1, $2, '192.168.77.0/24')`, netID, seededProjectID); err != nil {
		t.Fatalf("seeding subnet: %v", err)
	}

	// Redirect the node-local state root (image bases, overlays) into a test
	// directory so nothing touches /var/lib.
	origBase := paths.Base()
	paths.SetBase(t.TempDir())
	t.Cleanup(func() { paths.SetBase(origBase) })

	var instanceID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ('n8b-server', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE')
		 RETURNING id::text`, seededProjectID, seededUserID).Scan(&instanceID); err != nil {
		t.Fatalf("seeding instance: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM ports WHERE network_id::text = $1`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id::text = $1`, instanceID)
	})

	r := newNovaTestRouter()
	owner := map[string]string{
		"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member",
	}

	attachBody := fmt.Sprintf(`{"interfaceAttachment":{"net_id":%q}}`, netID)
	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+instanceID+"/os-interface", attachBody, owner)
	if code != http.StatusOK {
		t.Fatalf("attaching via net_id: code=%d body=%s", code, body)
	}
	var createdPort string
	if err := testPool.QueryRow(ctx,
		`SELECT id::text FROM ports WHERE network_id::text = $1 AND device_id::text = $2`,
		netID, instanceID).Scan(&createdPort); err != nil {
		t.Fatalf("resolving the attached port: %v", err)
	}
	var deviceOwner string
	if err := testPool.QueryRow(ctx,
		`SELECT COALESCE(device_owner, '') FROM ports WHERE id::text = $1`, createdPort).Scan(&deviceOwner); err != nil {
		t.Fatalf("reading the created port's owner: %v", err)
	}
	if deviceOwner != "compute:nova" {
		t.Fatalf("a port Nova created for os-interface must be stamped compute:nova (got %q); it is Nova's to clean up", deviceOwner)
	}

	if code, body := doReq(r, http.MethodDelete, "/v2.1/servers/"+instanceID, "", owner); code != http.StatusNoContent {
		t.Fatalf("deleting the server: code=%d body=%s", code, body)
	}
	var exists bool
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ports WHERE id::text = $1)`, createdPort).Scan(&exists); err != nil {
		t.Fatalf("checking the created port: %v", err)
	}
	if exists {
		t.Error("the port Nova created for os-interface survived the server delete; it must be deleted with it")
	}
}
