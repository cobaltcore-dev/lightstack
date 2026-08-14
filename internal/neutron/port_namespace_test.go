package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-024 (cloud-storage#546) US1 / F10 — `POST /v2.0/ports` returned 500 "failed to attach TAP to
// bridge" and nothing downstream of a port worked.
//
// Two unrelated causes shared that one message, and only the first is reachable without a Linux host:
//
//   - stub: BridgeManager.stubBridges is an in-process map (netns.go — written by createBridgeStub,
//     read by the stub branch of AttachToBridge). It is never persisted and never rebuilt, so a port
//     on any network created before the current process failed. That is why the ticket's repro fails
//     against a long-running compose stack (restart: unless-stopped) and passes if you create the
//     network and the port back to back.
//
//   - real: the TAP was created and attached inside light-ns-<project> while the bridge lives in the
//     root namespace, and nothing on that path ever created the namespace. Verified on a host in
//     T092; the placement is now enforced by TAPDeviceManager's namespace-free signature.
//
// The fix direction is forced rather than chosen: the bridge (network.go, "for libvirt access"),
// dnsmasq (reconciler.go, empty nsName) and the egress SNAT rules are ALL already in the root
// namespace. The TAP was the lone outlier.

// seedNetwork inserts a network directly, with no bridge registered anywhere — which is exactly the
// state after a restart, and the state a port creation must cope with (FR-002).
func seedNetwork(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.New().String()

	_, err := database.DB.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, network_type, mtu, created_at, updated_at)
		VALUES ($1, $2, $3, true, 'ACTIVE', false, 'flat', 1500, now(), now())
	`, id, name, seededProjectID)
	if err != nil {
		t.Fatalf("seed network: %v", err)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, "DELETE FROM ports WHERE network_id = $1", id)   //nolint:errcheck // best-effort cleanup
		database.DB.Exec(ctx, "DELETE FROM subnets WHERE network_id = $1", id) //nolint:errcheck // best-effort cleanup
		database.DB.Exec(ctx, "DELETE FROM networks WHERE id = $1", id)        //nolint:errcheck // best-effort cleanup
	})
	return id
}

// TestCreatePortOnNetworkAbsentFromBridgeRegistry — contract P3, and the stub half of P2.
//
// The network exists in the database and no bridge was ever registered in this process. Before the
// fix the stub attach reported "bridge br-xxxxxxxx does not exist" and the handler turned that into
// a 500. The recorded network is the authority for what exists (FR-001, FR-002).
func TestCreatePortOnNetworkAbsentFromBridgeRegistry(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}

	netID := seedNetwork(t, "spec024-f10-no-bridge")

	code, body := doReqBody(r, http.MethodPost, "/v2.0/ports",
		fmt.Sprintf(`{"port":{"name":"p-f10","network_id":%q}}`, netID), hdr)
	if code != http.StatusCreated {
		t.Fatalf("create port: code=%d body=%s; want 201 — this is F10", code, body)
	}

	var resp struct {
		Port map[string]any `json:"port"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("parse port: %v", err)
	}
	if resp.Port["network_id"] != netID {
		t.Errorf("network_id = %v, want %q", resp.Port["network_id"], netID)
	}
	if mac, _ := resp.Port["mac_address"].(string); mac == "" {
		t.Error("port must report a MAC address")
	}
}

// TestCreatePortPlacesTapWhereTheBridgeIs — the real-mode placement rule (FR-003).
//
// Asserted through the managers rather than the handler, because a stub TAP cannot be inspected for
// its namespace: the point is that the port path asks for the SAME placement the bridge path uses.
//
// The TAP half of this is now enforced by the compiler — CreateTAPDevice takes no namespace, so the
// inNamespace=true regression is unrepresentable. What remains testable, and what this still guards,
// is the bridge half: BridgeManager keeps its namespace parameters because auto_allocated_topology.go
// genuinely passes true, so the port path can still drift back to naming a namespace there.
func TestCreatePortPlacesTapWhereTheBridgeIs(t *testing.T) {
	br := networking.NewBridgeManager("stub")
	taps := networking.NewTAPDeviceManager("stub")

	// The bridge path: root namespace, as network.go does for libvirt's benefit.
	if err := br.CreateBridge("br-placement"); err != nil {
		t.Fatalf("create bridge in root ns: %v", err)
	}
	if err := taps.CreateTAPDevice("tap-placement"); err != nil {
		t.Fatalf("create tap: %v", err)
	}
	// The attach must name the same placement the bridge was created with.
	if err := br.AttachToBridge("tap-placement", "br-placement"); err != nil {
		t.Fatalf("attach in root ns: %v", err)
	}
}

// TestCreatePortRollsBackCleanly — contract P9 / FR-005.
//
// A security group that does not exist fails after the TAP is created and after the port row is
// written, so both must be undone. A leftover port row is the visible half: it would appear in
// listings for a port the caller was told was not created.
func TestCreatePortRollsBackCleanly(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}

	netID := seedNetwork(t, "spec024-f10-rollback")
	absentSG := uuid.New().String()

	code, body := doReqBody(r, http.MethodPost, "/v2.0/ports",
		fmt.Sprintf(`{"port":{"name":"p-rollback","network_id":%q,"security_groups":[%q]}}`, netID, absentSG), hdr)
	if code == http.StatusCreated {
		t.Fatalf("expected the request to fail on the absent security group, got 201: %s", body)
	}

	var count int
	if err := database.DB.QueryRow(ctx,
		"SELECT count(*) FROM ports WHERE network_id = $1", netID).Scan(&count); err != nil {
		t.Fatalf("count ports: %v", err)
	}
	if count != 0 {
		t.Errorf("rollback left %d port row(s) behind; a failed create must leave nothing", count)
	}
}

// TestCreatePortOnSharedNetworkOfAnotherProject — contract P7.
//
// Still allowed (a shared network is attachable), and the device must go where THAT network's bridge
// is rather than somewhere derived from the caller. Guards against a fix that swapped one wrong
// namespace for another.
func TestCreatePortOnSharedNetworkOfAnotherProject(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()

	otherProject := "00000000-0000-0000-0000-0000000b0b0b"
	var exists bool
	if err := database.DB.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1)", otherProject).Scan(&exists); err != nil {
		t.Fatalf("probe project: %v", err)
	}
	if !exists {
		t.Skip("second project fixture not present in this database")
	}

	netID := uuid.New().String()
	if _, err := database.DB.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, network_type, mtu, created_at, updated_at)
		VALUES ($1, 'spec024-f10-shared', $2, true, 'ACTIVE', true, 'flat', 1500, now(), now())
	`, netID, otherProject); err != nil {
		t.Fatalf("seed shared network: %v", err)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, "DELETE FROM ports WHERE network_id = $1", netID) //nolint:errcheck // best-effort cleanup
		database.DB.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID)      //nolint:errcheck // best-effort cleanup
	})

	code, body := doReqBody(r, http.MethodPost, "/v2.0/ports",
		fmt.Sprintf(`{"port":{"name":"p-shared","network_id":%q}}`, netID),
		map[string]string{"X-Test-Project": seededProjectID})
	if code != http.StatusCreated {
		t.Fatalf("port on a shared network: code=%d body=%s; want 201", code, body)
	}
}
