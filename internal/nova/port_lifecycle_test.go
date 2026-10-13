package nova

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// Review 2026-10-13 N-8: DeleteServer used a blanket
// `DELETE FROM ports WHERE device_id = $1`, which destroyed ports the TENANT
// created and attached (os-interface with port_id) along with the ones Nova
// allocated for the boot. Upstream deletes only the ports it created and
// detaches the rest — the tenant's port survives the server, parked DOWN with
// no device, reusable or deletable by its owner. This pins both halves.
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
			INSERT INTO ports (name, network_id, project_id, device_id, device_owner, mac_address, status)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, 'ACTIVE') RETURNING id::text`,
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
	var deviceID, deviceOwner, status string
	var exists bool
	if err := testPool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ports WHERE id::text = $1)`, tenantPort).Scan(&exists); err != nil || !exists {
		t.Fatalf("the tenant-created port must survive the server delete (exists=%v err=%v)", exists, err)
	}
	if err := testPool.QueryRow(ctx, `
		SELECT COALESCE(device_id::text, ''), COALESCE(device_owner, ''), status
		FROM ports WHERE id::text = $1`, tenantPort).Scan(&deviceID, &deviceOwner, &status); err != nil {
		t.Fatalf("reading the surviving port: %v", err)
	}
	if deviceID != "" || deviceOwner != "" {
		t.Errorf("the surviving port is still attached (device_id=%q device_owner=%q); want it detached", deviceID, deviceOwner)
	}
	if status != "DOWN" {
		t.Errorf("the surviving port's status = %q; want DOWN (detached, as upstream parks it)", status)
	}
}
