package nova

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/gin-gonic/gin"
)

// SPEC-025 US4 (cloud-storage#547 B12): stop and start must persist what they did.
//
// The real-mode branch of lifecycleAction called the hypervisor and wrote NOTHING to
// the instance row — only the stub branch persisted status and power_state. So a
// SUCCESSFUL stop left "ACTIVE"/power_state=1 recorded, and the API went on serving a
// running server that was shut off. That half of the divergence is independent of
// whatever removes a guest, and it is what these tests pin.
//
// Test seam: libvirtMode "real" makes lifecycleAction take the real branch, while the
// VMManager itself is stub-backed so the hypervisor calls resolve against an in-memory
// map instead of libvirt. No production interface was introduced for this — the mode
// split already existed in both places.

// newLifecycleService returns a service that takes the REAL lifecycle branch but talks
// to a stub-backed hypervisor, plus the router to drive it through.
func newLifecycleService(t *testing.T) (*Service, *gin.Engine, *hypervisor.VMManager) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mgr, err := hypervisor.NewVMManager("", "stub")
	if err != nil {
		t.Fatalf("building stub VM manager: %v", err)
	}

	svc := NewService(testPool, "", "real", nil)
	svc.vmManager = mgr
	t.Cleanup(svc.Shutdown)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set("user_id", u)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	svc.RegisterRoutes(&r.RouterGroup)
	return svc, r, mgr
}

// seedLifecycleInstance inserts an ACTIVE instance whose libvirt_domain_id refers to a
// guest the stub hypervisor knows about, so the real branch has something to act on.
func seedLifecycleInstance(t *testing.T, mgr *hypervisor.VMManager, name string) (instanceID, domainID string) {
	t.Helper()
	ctx := t.Context()

	var err error
	domainID, err = mgr.CreateVM(ctx, "<domain/>")
	if err != nil {
		t.Fatalf("creating stub guest: %v", err)
	}

	if err := testPool.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state, libvirt_domain_id)
		 VALUES ($1, $2, $3, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1, $4)
		 RETURNING id::text`,
		name, seededProjectID, seededUserID, domainID,
	).Scan(&instanceID); err != nil {
		t.Fatalf("seeding instance: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, instanceID)
	})
	return instanceID, domainID
}

// readInstanceState returns the persisted status and power_state.
func readInstanceState(t *testing.T, instanceID string) (status string, power int) {
	t.Helper()
	if err := testPool.QueryRow(t.Context(),
		`SELECT status, power_state FROM instances WHERE id = $1`, instanceID,
	).Scan(&status, &power); err != nil {
		t.Fatalf("reading instance state: %v", err)
	}
	return status, power
}

func lifecycleHdr() map[string]string {
	return map[string]string{
		"X-Test-Project": seededProjectID,
		"X-Test-User":    seededUserID,
		"X-Test-Roles":   "member",
	}
}

// T032 (FR-022, FR-024a): a successful stop must be persisted.
func TestRealModeStopPersistsShutoff(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, _ := seedLifecycleInstance(t, mgr, "spec025-stop")

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-stop": null}`, lifecycleHdr())
	if code != http.StatusAccepted {
		t.Fatalf("os-stop status = %d; want 202. body: %s", code, body)
	}

	status, power := readInstanceState(t, id)
	if status != "SHUTOFF" {
		t.Errorf("status = %q after a successful stop; want SHUTOFF — the real branch persisted nothing", status)
	}
	if power != 4 {
		t.Errorf("power_state = %d after a successful stop; want 4", power)
	}
}

// T033 (FR-023, FR-024a): a successful start must be persisted.
func TestRealModeStartPersistsActive(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-start")

	// Put the RECORD into the stopped state first. Deliberately by direct write
	// rather than by calling os-stop: if the start writes nothing, the row must be
	// left saying SHUTOFF so this test fails. Driving it through os-stop instead
	// made the assertion tautological before the fix — the row was still ACTIVE
	// from seeding, so "want ACTIVE" passed without anything being persisted.
	if _, err := testPool.Exec(t.Context(),
		`UPDATE instances SET status = 'SHUTOFF', power_state = 4 WHERE id = $1`, id); err != nil {
		t.Fatalf("pre-setting the stopped state: %v", err)
	}
	if err := mgr.StopVM(t.Context(), domainID, 0); err != nil {
		t.Fatalf("stopping the stub guest: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-start": null}`, lifecycleHdr())
	if code != http.StatusAccepted {
		t.Fatalf("os-start status = %d; want 202. body: %s", code, body)
	}

	status, power := readInstanceState(t, id)
	if status != "ACTIVE" {
		t.Errorf("status = %q after a successful start; want ACTIVE", status)
	}
	if power != 1 {
		t.Errorf("power_state = %d after a successful start; want 1", power)
	}
}

// T032/T033 companion (FR-027): reboot reaches the hypervisor too, and must leave the
// record consistent rather than untouched.
func TestRealModeRebootPersistsState(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)
	id, _ := seedLifecycleInstance(t, mgr, "spec025-reboot")

	// Seed an INCONSISTENT power_state so "unchanged" and "correct" differ. Seeding
	// ACTIVE/1 and asserting ACTIVE/1 would pass whether or not anything was written.
	if _, err := testPool.Exec(t.Context(),
		`UPDATE instances SET power_state = 0 WHERE id = $1`, id); err != nil {
		t.Fatalf("pre-setting an inconsistent power state: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"reboot": {"type": "SOFT"}}`, lifecycleHdr())
	if code != http.StatusAccepted {
		t.Fatalf("reboot status = %d; want 202. body: %s", code, body)
	}

	status, power := readInstanceState(t, id)
	if status != "ACTIVE" || power != 1 {
		t.Errorf("after reboot: status=%q power=%d; want ACTIVE/1 — a reboot that reaches the "+
			"hypervisor must leave the record consistent, not merely untouched", status, power)
	}
}

// T034 (FR-026, FR-026a, FR-026b, FR-027): three outcomes, three statuses.
//
// Every libvirt failure was a flat 500, which is exactly what made B12 unreadable from
// outside: a state problem and an outage were indistinguishable, so a client could not
// tell whether retrying made sense.
func TestLifecycleFailureStatusMapping(t *testing.T) {
	requireDB(t)
	_, r, mgr := newLifecycleService(t)

	// A server whose recorded guest does not exist in the hypervisor. Stop and
	// reboot cannot proceed; start self-heals and is covered separately.
	id, domainID := seedLifecycleInstance(t, mgr, "spec025-absent-guest")
	if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
		t.Fatalf("removing the stub guest: %v", err)
	}

	for _, action := range []string{`{"os-stop": null}`, `{"reboot": {"type": "SOFT"}}`} {
		code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", action, lifecycleHdr())
		if code == http.StatusInternalServerError {
			t.Errorf("%s against an absent guest returned 500; an absent guest is a state problem, not a fault. body: %s", action, body)
		}
		if code != http.StatusConflict {
			t.Errorf("%s against an absent guest = %d; want 409 conflict (FR-026a). body: %s", action, code, body)
		}
	}

	// An unknown server is absent, not a conflict and not a fault.
	code, _ := doReq(r, http.MethodPost,
		"/v2.1/servers/11111111-2222-3333-4444-555555555555/action", `{"os-stop": null}`, lifecycleHdr())
	if code != http.StatusNotFound {
		t.Errorf("action on an unknown server = %d; want 404", code)
	}
}

// T039 (FR-028): stub mode is unchanged. Article IV makes stub mode mandatory, so this
// is an assertion rather than an eyeball — transitions recorded, no hypervisor touched.
func TestStubModeLifecycleUnchanged(t *testing.T) {
	requireDB(t)
	gin.SetMode(gin.TestMode)

	svc := NewService(testPool, "", "stub", nil) // no vmManager at all
	t.Cleanup(svc.Shutdown)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("project_id", c.GetHeader("X-Test-Project"))
		c.Set("user_id", c.GetHeader("X-Test-User"))
		c.Set("roles", strings.Split(c.GetHeader("X-Test-Roles"), ","))
	})
	svc.RegisterRoutes(&r.RouterGroup)

	var id string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state)
		 VALUES ('spec025-stub', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1)
		 RETURNING id::text`, seededProjectID, seededUserID).Scan(&id); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, id)
	})

	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-stop": null}`, lifecycleHdr()); code != http.StatusAccepted {
		t.Fatalf("stub os-stop = %d; want 202. body: %s", code, body)
	}
	if status, power := readInstanceState(t, id); status != "SHUTOFF" || power != 4 {
		t.Errorf("stub stop: status=%q power=%d; want SHUTOFF/4", status, power)
	}

	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-start": null}`, lifecycleHdr()); code != http.StatusAccepted {
		t.Fatalf("stub os-start = %d; want 202. body: %s", code, body)
	}
	if status, power := readInstanceState(t, id); status != "ACTIVE" || power != 1 {
		t.Errorf("stub start: status=%q power=%d; want ACTIVE/1", status, power)
	}
}

// T037 (FR-024c): reading a server must not contact the hypervisor.
//
// This is the feature's only performance requirement, and it is the direct cost of the
// design chosen in clarify: state is persisted by each action and drift is corrected by
// a periodic sweep, precisely so that a READ never has to ask libvirt anything. The
// rejected alternative — checking at read time — puts one hypervisor round trip on
// every server returned, so a listing degrades in proportion to how many servers a
// project has.
//
// Counted rather than asserted qualitatively: "does not call the hypervisor" is only
// meaningful if something would notice when it starts to.
func TestServerReadsDoNotContactTheHypervisor(t *testing.T) {
	requireDB(t)
	svc, r, mgr := newLifecycleService(t)

	const servers = 3
	var ids []string
	for i := range servers {
		id, _ := seedLifecycleInstance(t, mgr, fmt.Sprintf("spec025-noread-%d", i))
		ids = append(ids, id)
	}

	// Every guest disappears. If any read path consulted the hypervisor, it would
	// have to handle the absence — and the only way to observe that here is that the
	// answers stay exactly as recorded.
	for _, id := range ids {
		var domainID string
		if err := testPool.QueryRow(t.Context(),
			`SELECT libvirt_domain_id FROM instances WHERE id = $1`, id).Scan(&domainID); err != nil {
			t.Fatalf("reading the domain id: %v", err)
		}
		if err := mgr.DeleteVM(t.Context(), domainID); err != nil {
			t.Fatalf("removing a stub guest: %v", err)
		}
	}

	// A single read and a listing must both still report what the record says, and
	// must do so without the hypervisor being reachable at all.
	svc.vmManager = nil // any hypervisor call from a read path would now panic

	if code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+ids[0], "", lifecycleHdr()); code != http.StatusOK {
		t.Fatalf("server read = %d; want 200 with no hypervisor. body: %s", code, body)
	}
	if code, body := doReq(r, http.MethodGet, "/v2.1/servers/detail", "", lifecycleHdr()); code != http.StatusOK {
		t.Fatalf("server listing = %d; want 200 with no hypervisor. body: %s", code, body)
	}

	// And the reported state is still the recorded one — reads report the record,
	// correction is the reconciler's job (FR-024b/FR-024c).
	if status, _ := readInstanceState(t, ids[0]); status != "ACTIVE" {
		t.Errorf("status = %q; a read must not correct the record, only the sweep does", status)
	}
}

// TestResetStateAcceptsOnlyActiveAndError pins what os-resetState may write into
// instances.status (branch review, SRC-12's investigation).
//
// The handler mapped "active" and "error" and passed EVERYTHING ELSE THROUGH UNCHANGED, so
// `{"state":"banana"}` was answered 202 and became a permanent, advertised server status —
// in every listing, every server show, the ?status= filter and Horizon's status column.
// `{"state":"shutoff"}` stored a lowercase value only a case-folding comparison could find
// again, which is why the filter's UPPER(status) is load-bearing rather than gratuitous.
//
// Upstream accepts exactly two states: the api-ref gives os-resetState.state as "The state
// to set, 'active' or 'error'".
func TestResetStateAcceptsOnlyActiveAndError(t *testing.T) {
	requireDB(t)
	_, _, mgr := newLifecycleService(t)
	id, _ := seedLifecycleInstance(t, mgr, "spec025-resetstate")
	r := newNovaTestRouter()
	admin := testAuthHdr(seededProjectID, seededUserID) // os-resetState is admin-only

	reset := func(t *testing.T, state string) (int, string) {
		t.Helper()
		code, _ := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action",
			`{"os-resetState":{"state":"`+state+`"}}`, admin)
		var stored string
		if err := testPool.QueryRow(t.Context(),
			`SELECT status FROM instances WHERE id = $1`, id).Scan(&stored); err != nil {
			t.Fatalf("reading status: %v", err)
		}
		return code, stored
	}

	// Accepted, and stored in the canonical uppercase the rest of the product uses.
	//
	// Case-insensitively, which is deliberately laxer than upstream's enum: a caller that
	// sent "ACTIVE" had it stored by the old pass-through, so refusing it now would remove
	// behaviour rather than add correctness.
	for _, tc := range []struct{ send, want string }{
		{"active", "ACTIVE"},
		{"error", "ERROR"},
		{"ACTIVE", "ACTIVE"},
		{"Error", "ERROR"},
	} {
		code, stored := reset(t, tc.send)
		if code != http.StatusAccepted {
			t.Errorf("state=%q: code=%d; want 202", tc.send, code)
		}
		if stored != tc.want {
			t.Errorf("state=%q stored %q; want the canonical %q", tc.send, stored, tc.want)
		}
	}

	// Refused, and — the half that matters — nothing written.
	for _, state := range []string{"shutoff", "SHUTOFF", "banana", "'; DROP--", "build", "deleted"} {
		before := func() string {
			var s string
			_ = testPool.QueryRow(t.Context(), `SELECT status FROM instances WHERE id = $1`, id).Scan(&s)
			return s
		}()
		code, stored := reset(t, state)
		if code != http.StatusBadRequest {
			t.Errorf("state=%q: code=%d; want 400 — upstream accepts only active and error", state, code)
		}
		if stored != before {
			t.Errorf("state=%q was refused but the status changed from %q to %q; a refusal must write nothing",
				state, before, stored)
		}
	}
}
