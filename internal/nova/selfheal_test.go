package nova

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// SPEC-025 US4 / FR-025: starting a server whose guest has vanished must re-create it
// from stored state and succeed.
//
// Before this, the start was an unexplained 500 and the record went on claiming the
// server was running — nothing surfaced the divergence until the next action also
// failed (cloud-storage#547 B12).

// withInstanceDisk creates the per-instance overlay the rebuild requires, under a temp
// root, and points the hypervisor's path helpers at it. Returns the instance directory.
//
// The rebuild deliberately REFUSES when the disk is missing (re-creating it would
// discard the guest's data), so a test that wants the happy path has to provide one.
func withInstanceDisk(t *testing.T, instanceID string) {
	t.Helper()
	// Pretend the node-local overlay is present. /var/lib/lightstack is root-owned and
	// paths.Base has no override, so a non-root run cannot stage a real one — and
	// covering the whole happy path with a skipped test is not coverage.
	prev := fileExists
	fileExists = func(path string) bool {
		return strings.HasSuffix(path, "disk.qcow2") || prev(path)
	}
	t.Cleanup(func() { fileExists = prev })
}

// T035 (FR-025, FR-025a, FR-025d): the guest is re-created from stored state, the
// rebuilt definition matches the server as recorded, and no new action name appears.
func TestStartSelfHealsAnAbsentGuest(t *testing.T) {
	requireDB(t)
	svc, r, mgr := newLifecycleService(t)
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-selfheal")
	withInstanceDisk(t, id)

	// The guest disappears out of band — the case B12 describes.
	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}
	if _, err := testPool.Exec(t.Context(),
		`UPDATE instances SET status = 'SHUTOFF', power_state = 4 WHERE id = $1`, id); err != nil {
		t.Fatalf("pre-setting the stopped state: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-start": null}`, lifecycleHdr())
	if code != http.StatusAccepted {
		t.Fatalf("os-start against an absent guest = %d; want 202 after a self-heal. body: %s", code, body)
	}

	if status, power := readInstanceState(t, id); status != "ACTIVE" || power != 1 {
		t.Errorf("after a self-heal: status=%q power=%d; want ACTIVE/1", status, power)
	}

	// Whatever identifier the rebuilt guest has, the row must name it — a cleared or
	// stale column means the next action repeats this whole exercise.
	//
	// Only the non-empty check says anything about real mode. Asserting the id CHANGED
	// is a stub artifact: stub CreateVM invents a UUID (libvirt.go:113) whereas the real
	// one reads it back off the domain it just defined (libvirt.go:144), and
	// buildVMSpecFromStored sets UUID to the instance id — so a real re-created guest
	// keeps the same identifier and there is nothing to replace. Confirmed on a host,
	// where the self-heal logged domain_id == instance_id.
	var newDomain string
	if err := testPool.QueryRow(t.Context(),
		`SELECT libvirt_domain_id FROM instances WHERE id = $1`, id).Scan(&newDomain); err != nil {
		t.Fatalf("reading the domain id: %v", err)
	}
	if newDomain == "" {
		t.Error("libvirt_domain_id was cleared rather than pointed at the re-created guest")
	}
	if newDomain == domainID {
		t.Error("libvirt_domain_id still names the vanished guest — under the stub, which mints a " +
			"fresh UUID, the re-created one was not persisted")
	}

	// FR-025a: the rebuilt definition must carry the flavor's sizing, not defaults.
	spec, err := svc.buildVMSpecFromStored(t.Context(), id)
	if err != nil {
		t.Fatalf("rebuilding the spec: %v", err)
	}
	var vcpus, ram int
	if err := testPool.QueryRow(t.Context(),
		`SELECT f.vcpus, f.ram_mb FROM instances i JOIN flavors f ON f.id = i.flavor_id WHERE i.id = $1`, id,
	).Scan(&vcpus, &ram); err != nil {
		t.Fatalf("reading the flavor: %v", err)
	}
	if spec.VCPUs != vcpus || spec.MemoryMB != ram {
		t.Errorf("rebuilt spec = %d vcpus / %d MB; the flavor says %d / %d", spec.VCPUs, spec.MemoryMB, vcpus, ram)
	}
	if spec.UUID != id {
		t.Errorf("rebuilt spec UUID = %q; want the instance id %q", spec.UUID, id)
	}
}

// T035 (FR-025b, FR-025d): the repair is recorded on the triggering action, under the
// existing action name.
func TestSelfHealIsRecordedOnTheStartAction(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-selfheal-record")
	withInstanceDisk(t, id)

	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}

	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-start": null}`, lifecycleHdr()); code != http.StatusAccepted {
		t.Fatalf("os-start = %d; want 202. body: %s", code, body)
	}

	// The dispatcher records actions under upstream's names, with the "os-" prefix
	// trimmed — a start is recorded as "start". The point of FR-025d is that the
	// self-heal adds no NEW name, so the assertion is that this is still the ordinary
	// start row and that there is exactly one of them.
	var rows int
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM instance_actions WHERE instance_id = $1`, id).Scan(&rows); err != nil {
		t.Fatalf("counting action records: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d action records for one start; the self-heal must not add an event of its own (FR-025d)", rows)
	}

	var action, message string
	if err := testPool.QueryRow(t.Context(),
		`SELECT action, COALESCE(message, '') FROM instance_actions
		  WHERE instance_id = $1 ORDER BY start_time DESC LIMIT 1`, id,
	).Scan(&action, &message); err != nil {
		t.Fatalf("reading the action record: %v", err)
	}

	if action != "start" {
		t.Errorf("recorded action = %q; want the ordinary %q row, with no new name invented (FR-025d)", action, "start")
	}
	if message == "" {
		t.Error("the self-heal left no note on the action record; a repeatedly vanishing guest would be invisible (FR-025b)")
	}
	if !strings.Contains(message, "re-created") {
		t.Errorf("action message = %q; it should say the guest was re-created (FR-025b)", message)
	}
}

// T038 (FR-025c, FR-026b): a self-heal that CANNOT complete is an action-impossible
// conflict, not an internal fault — the server exists and the request was well formed,
// so this is a state an operator has to resolve, not an outage.
func TestFailedSelfHealIsAConflictNotAFault(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-selfheal-fails")

	// No instance disk is staged, so the rebuild must refuse: re-creating the overlay
	// would silently discard whatever the guest had.
	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-start": null}`, lifecycleHdr())
	if code == http.StatusInternalServerError {
		t.Errorf("a self-heal that cannot complete returned 500; it is a state problem, not a fault (FR-026b). body: %s", body)
	}
	if code != http.StatusConflict {
		t.Errorf("failed self-heal = %d; want 409 conflict. body: %s", code, body)
	}

	// FR-025c: the reported state must end up consistent with reality — the server is
	// not running, so it must not claim to be.
	if status, _ := readInstanceState(t, id); status == "ACTIVE" {
		t.Error("the server still reports ACTIVE after a failed start; the record claims a guest that does not exist")
	}

	// The tenant reads this answer: why the rebuild failed — here a path on the node — is the
	// log's, not theirs.
	if strings.Contains(body, "disk.qcow2") || strings.Contains(body, "/var/lib") {
		t.Errorf("the refusal carries the node's internal detail; body: %s", body)
	}
}

// The rebuild must refuse rather than invent a disk. Pinned separately because the
// failure mode it prevents — a guest quietly re-created against a fresh empty overlay —
// destroys data instead of reporting a problem.
func TestRebuildRefusesWhenTheDiskIsMissing(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)
	id, _ := seedLifecycleInstance(t, mgr, "spec025-nodisk")

	// No disk staged: fileExists is untouched, so the real filesystem is consulted
	// and /var/lib/lightstack/instances/<id>/disk.qcow2 does not exist.
	if _, err := svc.buildVMSpecFromStored(context.Background(), id); err == nil {
		t.Error("rebuilding a guest with no disk succeeded; it must refuse rather than create an empty overlay")
	}
}

// ---------------------------------------------------------------------------
// The port read (SRC-4). storedNetworkConfigs was exercised only for instances with NO
// ports, so the loop that builds an interface never ran. The naming convention itself is
// now networking.BridgeNameForNetwork and is tested where it lives, in
// pkg/networking/netns_test.go.

// TestStoredNetworkConfigsRebuildsEachInterface covers the read that turns a server's
// ports back into hypervisor interfaces — the part of a self-heal that decides what the
// revived guest is plugged into.
func TestStoredNetworkConfigsRebuildsEachInterface(t *testing.T) {
	requireDB(t)
	svc, _, mgr := newLifecycleService(t)
	instanceID, _ := seedLifecycleInstance(t, mgr, "spec025-selfheal-ports")

	// Two ports, deliberately in a known creation order: storedNetworkConfigs orders by
	// created_at because interface order decides which one the guest calls eth0.
	networkID := "aaaaaaaa-1111-2222-3333-444444444444"
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO networks (id, name, project_id, status, admin_state_up)
		 VALUES ($1, 'selfheal-net', $2, 'ACTIVE', true) ON CONFLICT (id) DO NOTHING`,
		networkID, seededProjectID); err != nil {
		t.Fatalf("seeding network: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, networkID)
	})

	want := []struct{ mac string }{{"52:54:00:aa:bb:01"}, {"52:54:00:aa:bb:02"}}
	for i, w := range want {
		if _, err := testPool.Exec(t.Context(),
			`INSERT INTO ports (network_id, project_id, device_id, mac_address, status, admin_state_up, created_at)
			 VALUES ($1, $2, $3, $4, 'ACTIVE', true, $5)`,
			networkID, seededProjectID, instanceID, w.mac,
			time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("seeding port %d: %v", i, err)
		}
	}

	got, err := svc.storedNetworkConfigs(context.Background(), instanceID)
	if err != nil {
		t.Fatalf("storedNetworkConfigs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d interfaces; want 2 — a revived guest must get back every interface it had", len(got))
	}
	for i, w := range want {
		if got[i].MACAddress != w.mac {
			t.Errorf("interface %d MAC = %q; want %q in creation order — interface order decides which one is eth0",
				i, got[i].MACAddress, w.mac)
		}
		// The expected name is spelled out rather than computed with the function that
		// produced it: deriving it would make the assertion agree with any convention.
		if want := "br-" + networkID[:8]; got[i].BridgeName != want {
			t.Errorf("interface %d bridge = %q; want %q", i, got[i].BridgeName, want)
		}
		if got[i].PortID == "" {
			t.Errorf("interface %d has no port id; the guest's interface could not be matched back to its port", i)
		}
	}
}

// TestStoredNetworkConfigsIgnoresAnotherProjectsPort: device_id is whatever a port's creator
// wrote, so naming a server in it proves nothing. A port another project created with this
// server's id must not become an interface of the revived guest — it would plug the guest into
// that project's network. The server's own ports are those of its project, which is how the
// address map reads them and how upstream lists them (tenant_id with device_id).
func TestStoredNetworkConfigsIgnoresAnotherProjectsPort(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t) // the second project
	svc, _, mgr := newLifecycleService(t)
	instanceID, _ := seedLifecycleInstance(t, mgr, "spec025-selfheal-foreign-port")

	own := seedBootNetwork(t, seededProjectID, "spec025-selfheal-own-net")
	foreign := seedBootNetwork(t, authzProjectB, "spec025-selfheal-foreign-net")
	for _, p := range []struct{ network, project, mac string }{
		{own, seededProjectID, "52:54:00:aa:cc:01"},
		{foreign, authzProjectB, "52:54:00:aa:cc:02"},
	} {
		if _, err := testPool.Exec(t.Context(),
			`INSERT INTO ports (network_id, project_id, device_id, mac_address, status, admin_state_up, created_at)
			 VALUES ($1, $2, $3, $4, 'ACTIVE', true, now())`,
			p.network, p.project, instanceID, p.mac); err != nil {
			t.Fatalf("seeding port %s: %v", p.mac, err)
		}
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM ports WHERE device_id = $1`, instanceID)
	})

	got, err := svc.storedNetworkConfigs(context.Background(), instanceID)
	if err != nil {
		t.Fatalf("storedNetworkConfigs: %v", err)
	}
	if len(got) != 1 || got[0].MACAddress != "52:54:00:aa:cc:01" {
		t.Errorf("revived guest's interfaces = %+v; want only the server's own port (52:54:00:aa:cc:01)", got)
	}
}
