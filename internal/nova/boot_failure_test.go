package nova

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/gin-gonic/gin"
)

// SPEC-025 US3 (cloud-storage#547 B13): a server must never report itself healthy
// without the network it asked for.
//
// A port-allocation failure was logged and the boot CONTINUED — "// Continue with empty
// networks rather than failing" — so the server reached ACTIVE with an empty address
// map and no interface at all. The user was told it was running; it had no
// connectivity, could not reach the metadata service, and nothing in its own record
// said why.
//
// Fixing the network-side cause does not fix this: it removes one trigger and leaves
// the fail-open in place for the next. During the sibling tenant-scoping work this
// exact state was investigated as a cross-tenant authorization defect before anyone
// checked the network's owner.

// fakeNeutron is a NeutronService whose behaviour each test chooses.
type fakeNeutron struct {
	allocErrFor map[string]error // network id -> error to return
	usableErr   map[string]error // network id -> pre-flight verdict
	allocCalls  int

	// The admin flag each call carried. Guarded because allocation runs in the
	// VM-creation goroutine while the test reads.
	mu          sync.Mutex
	usableAdmin []bool
	allocAdmin  []bool
}

func (f *fakeNeutron) AllocatePortForInstance(_ context.Context, networkID, _, _ string, _ []string, admin bool) (any, error) {
	f.mu.Lock()
	f.allocAdmin = append(f.allocAdmin, admin)
	f.mu.Unlock()
	f.allocCalls++
	if err, ok := f.allocErrFor[networkID]; ok && err != nil {
		return nil, err
	}
	return nil, errors.New("fakeNeutron: no port payload configured")
}

func (f *fakeNeutron) NetworkUsableBy(_ context.Context, networkID, _ string, admin bool) error {
	f.mu.Lock()
	f.usableAdmin = append(f.usableAdmin, admin)
	f.mu.Unlock()
	if err, ok := f.usableErr[networkID]; ok {
		return err
	}
	return nil
}

// adminFlags returns the admin flag of every pre-flight and every allocation so far.
func (f *fakeNeutron) adminFlags() (usable, alloc []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.usableAdmin...), append([]bool(nil), f.allocAdmin...)
}

// newBootService builds a real-mode compute service with a stub-backed hypervisor and
// the given fake network service.
func newBootService(t *testing.T, neutron NeutronService) (*Service, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// A stub-backed hypervisor is required, not optional: the port-allocation path
	// lives inside the VM-creation goroutine, which only runs when a manager is
	// present. Without one the create takes the no-hypervisor branch and marks the
	// server ACTIVE immediately, so every assertion about allocation failure would be
	// testing a path that never executed.
	mgr, err := hypervisor.NewVMManager("", "stub")
	if err != nil {
		t.Fatalf("building stub VM manager: %v", err)
	}

	svc := NewService(testPool, "", "real", nil)
	svc.vmManager = mgr
	svc.SetNeutronService(neutron)
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
	return svc, r
}

// seedBootNetwork inserts a network owned by the given project.
func seedBootNetwork(t *testing.T, projectID, name string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO networks (name, project_id, status, admin_state_up)
		 VALUES ($1, $2, 'ACTIVE', true) RETURNING id::text`, name, projectID,
	).Scan(&id); err != nil {
		t.Fatalf("seeding network: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, id)
	})
	return id
}

// awaitInstanceStatus polls until the boot goroutine has settled on a terminal state.
// The create path answers before allocation begins, so the outcome is necessarily
// asynchronous — that asymmetry is exactly why FR-016a needed two paths.
func awaitInstanceStatus(t *testing.T, name string, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if err := testPool.QueryRow(t.Context(),
			`SELECT status FROM instances WHERE name = $1 AND project_id = $2`, name, seededProjectID,
		).Scan(&last); err == nil && last == want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

// T053/T054 unit-level companion (FR-017, FR-016a): a network the caller cannot use is
// refused BEFORE anything is created.
func TestCreateRefusesAnUnusableNetworkUpFront(t *testing.T) {
	requireDB(t)

	const missingNet = "99999999-9999-9999-9999-999999999999"
	fake := &fakeNeutron{usableErr: map[string]error{
		missingNet: errors.New("network not found"),
	}}
	_, r := newBootService(t, fake)

	const name = "spec025-preflight"
	cleanupServerNamed(t, name)

	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q}]}}`,
		name, seededImageID(t), missingNet)
	code, respBody := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr())

	if code != http.StatusBadRequest {
		t.Errorf("create with an unusable network = %d; want 400 before anything is created. body: %s", code, respBody)
	}
	if !strings.Contains(respBody, missingNet) {
		t.Errorf("the refusal does not name the offending network; body: %s", respBody)
	}
	if n := countServersNamed(t, name); n != 0 {
		t.Errorf("%d instance rows created for a refused request; want 0 (FR-017)", n)
	}
	if fake.allocCalls != 0 {
		t.Errorf("%d port allocations attempted for a refused request; want 0", fake.allocCalls)
	}
}

// An unshared external network is refused as upstream refuses it: 403 with Nova's own
// ExternalNetworkAttachForbidden wording, before anything is created (PR #91 review).
// Upstream maps that exception to HTTPForbidden in the create handler; every other
// pre-flight refusal stays a 400.
func TestCreateRefusesAnExternalNetworkAsUpstreamDoes(t *testing.T) {
	requireDB(t)

	const externalNet = "88888888-8888-8888-8888-888888888888"
	fake := &fakeNeutron{usableErr: map[string]error{
		externalNet: &neutron.ExternalNetworkAttachForbiddenError{NetworkID: externalNet},
	}}
	_, r := newBootService(t, fake)

	const name = "spec025-preflight-external"
	cleanupServerNamed(t, name)

	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q}]}}`,
		name, seededImageID(t), externalNet)
	code, respBody := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr())

	if code != http.StatusForbidden {
		t.Errorf("create on an unshared external network = %d; want 403 as upstream. body: %s", code, respBody)
	}
	if want := "It is not allowed to create an interface on external network " + externalNet; !strings.Contains(respBody, want) {
		t.Errorf("body does not carry upstream's message %q; body: %s", want, respBody)
	}
	if n := countServersNamed(t, name); n != 0 {
		t.Errorf("%d instance rows created for a refused request; want 0 (FR-017)", n)
	}
	if fake.allocCalls != 0 {
		t.Errorf("%d port allocations attempted for a refused request; want 0", fake.allocCalls)
	}
}

// The attach rule depends on who is asking — an administrator may use an unshared external
// network, nobody else may — so the caller's role must reach BOTH the pre-flight and the
// allocation, or the two judge different callers and disagree again (FR-016a).
func TestAttachRuleIsToldTheCallersRole(t *testing.T) {
	requireDB(t)

	for _, tc := range []struct {
		roles string
		admin bool
	}{
		{"member", false},
		{"admin", true},
	} {
		netID := seedBootNetwork(t, seededProjectID, "spec025-role-net-"+tc.roles)
		fake := &fakeNeutron{allocErrFor: map[string]error{netID: errors.New("stop after recording")}}
		_, r := newBootService(t, fake)

		name := "spec025-role-" + tc.roles
		cleanupServerNamed(t, name)
		hdr := bootHdr()
		hdr["X-Test-Roles"] = tc.roles

		body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q}]}}`,
			name, seededImageID(t), netID)
		if code, respBody := doReq(r, http.MethodPost, "/v2.1/servers", body, hdr); code != http.StatusAccepted && code != http.StatusCreated {
			t.Fatalf("%s: create = %d; want it accepted. body: %s", tc.roles, code, respBody)
		}
		if got := awaitInstanceStatus(t, name, "ERROR"); got != "ERROR" {
			t.Fatalf("%s: status = %q; want ERROR once the fake allocation fails", tc.roles, got)
		}

		usable, alloc := fake.adminFlags()
		if len(usable) != 1 || usable[0] != tc.admin {
			t.Errorf("%s: pre-flight admin flags = %v; want [%v]", tc.roles, usable, tc.admin)
		}
		if len(alloc) != 1 || alloc[0] != tc.admin {
			t.Errorf("%s: allocation admin flags = %v; want [%v]", tc.roles, alloc, tc.admin)
		}
	}
}

// T055 (FR-015, FR-016, FR-018): a failure that only appears AFTER the create was
// answered lands the server in a failed state with a readable reason — never ACTIVE
// with an empty address map.
func TestPostAcceptAllocationFailureLandsInErrorWithAFault(t *testing.T) {
	requireDB(t)

	netID := seedBootNetwork(t, seededProjectID, "spec025-boot-net")
	fake := &fakeNeutron{allocErrFor: map[string]error{
		netID: errors.New("no addresses left in the subnet"),
	}}
	_, r := newBootService(t, fake)

	const name = "spec025-postaccept"
	cleanupServerNamed(t, name)

	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q}]}}`,
		name, seededImageID(t), netID)
	code, respBody := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr())
	if code != http.StatusAccepted && code != http.StatusCreated {
		t.Fatalf("create = %d; want the request accepted. body: %s", code, respBody)
	}

	if got := awaitInstanceStatus(t, name, "ERROR"); got != "ERROR" {
		t.Fatalf("status = %q after a failed allocation; want ERROR — the boot must not continue (FR-015)", got)
	}

	// FR-016: the reason must be readable from the server's own record.
	var code2 *int
	var message *string
	if err := testPool.QueryRow(t.Context(),
		`SELECT fault_code, fault_message FROM instances WHERE name = $1 AND project_id = $2`,
		name, seededProjectID).Scan(&code2, &message); err != nil {
		t.Fatalf("reading the fault: %v", err)
	}
	if message == nil || *message == "" {
		t.Error("no fault_message recorded; the user has nothing to read (FR-016)")
	}
	if code2 == nil {
		t.Error("no fault_code recorded")
	}

	// FR-018: state and addresses must never contradict each other.
	var id string
	if err := testPool.QueryRow(t.Context(),
		`SELECT id::text FROM instances WHERE name = $1 AND project_id = $2`, name, seededProjectID).Scan(&id); err != nil {
		t.Fatalf("reading the instance id: %v", err)
	}
	_, detail := doReq(r, http.MethodGet, "/v2.1/servers/"+id, "", bootHdr())
	var got struct {
		Server struct {
			Status    string         `json:"status"`
			Addresses map[string]any `json:"addresses"`
			Fault     *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"fault"`
		} `json:"server"`
	}
	if err := json.Unmarshal([]byte(detail), &got); err != nil {
		t.Fatalf("decoding server detail: %v (body %s)", err, detail)
	}
	if got.Server.Status == "ACTIVE" && len(got.Server.Addresses) == 0 {
		t.Error("server reports ACTIVE with no addresses — the state FR-018 forbids")
	}
	if got.Server.Fault == nil || got.Server.Fault.Message == "" {
		t.Errorf("server detail carries no fault object; `server show` still says nothing (FR-016). body: %s", detail)
	}
}

// T056 (FR-020, FR-021): a partial failure is a failure; a server that requested no
// network is untouched by any of this.
func TestPartialNetworkFailureIsATotalFailure(t *testing.T) {
	requireDB(t)

	good := seedBootNetwork(t, seededProjectID, "spec025-good-net")
	bad := seedBootNetwork(t, seededProjectID, "spec025-bad-net")
	fake := &fakeNeutron{allocErrFor: map[string]error{
		bad: errors.New("subnet exhausted"),
	}}
	_, r := newBootService(t, fake)

	const name = "spec025-partial"
	cleanupServerNamed(t, name)

	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q},{"uuid":%q}]}}`,
		name, seededImageID(t), good, bad)
	if code, respBody := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr()); code != http.StatusAccepted && code != http.StatusCreated {
		t.Fatalf("create = %d. body: %s", code, respBody)
	}

	if got := awaitInstanceStatus(t, name, "ERROR"); got != "ERROR" {
		t.Errorf("status = %q; one failed network out of two must fail the whole boot, not yield a partly-networked success (FR-020)", got)
	}
}

// FR-021: this story constrains REQUESTED networks. A server that asked for none must
// behave exactly as before.
func TestCreateWithNoNetworkRequestedIsUnaffected(t *testing.T) {
	requireDB(t)

	fake := &fakeNeutron{}
	_, r := newBootService(t, fake)

	const name = "spec025-nonet"
	cleanupServerNamed(t, name)

	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q}}`, name, seededImageID(t))
	code, respBody := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr())
	if code != http.StatusAccepted && code != http.StatusCreated {
		t.Fatalf("create with no network = %d; want accepted. body: %s", code, respBody)
	}
	if n := countServersNamed(t, name); n != 1 {
		t.Errorf("%d instance rows; want 1", n)
	}
	if fake.allocCalls != 0 {
		t.Errorf("%d allocations for a server that requested no network; want 0", fake.allocCalls)
	}
}

// T057 (FR-019): a failed boot must leave nothing unowned, and the server must still be
// deletable by its owner. An undeletable ERROR server is the failure mode this exists to
// prevent — it becomes a support ticket rather than something a user can clear.
func TestFailedBootLeavesADeletableServer(t *testing.T) {
	requireDB(t)

	netID := seedBootNetwork(t, seededProjectID, "spec025-del-net")
	fake := &fakeNeutron{allocErrFor: map[string]error{netID: errors.New("allocation refused")}}
	_, r := newBootService(t, fake)

	const name = "spec025-deletable"
	cleanupServerNamed(t, name)

	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q}]}}`,
		name, seededImageID(t), netID)
	if code, respBody := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr()); code != http.StatusAccepted && code != http.StatusCreated {
		t.Fatalf("create = %d. body: %s", code, respBody)
	}
	if got := awaitInstanceStatus(t, name, "ERROR"); got != "ERROR" {
		t.Fatalf("status = %q; want ERROR before testing deletability", got)
	}

	var id string
	if err := testPool.QueryRow(t.Context(),
		`SELECT id::text FROM instances WHERE name = $1 AND project_id = $2`, name, seededProjectID).Scan(&id); err != nil {
		t.Fatalf("reading the instance id: %v", err)
	}

	code, respBody := doReq(r, http.MethodDelete, "/v2.1/servers/"+id, "", bootHdr())
	if code != http.StatusNoContent && code != http.StatusAccepted {
		t.Fatalf("deleting a failed server = %d; want it to succeed. body: %s", code, respBody)
	}
	if n := countServersNamed(t, name); n != 0 {
		t.Errorf("%d rows remain after deleting a failed server", n)
	}

	// Nothing may be left pointing at the deleted server.
	var orphanPorts int
	if err := testPool.QueryRow(t.Context(),
		`SELECT count(*) FROM ports WHERE device_id = $1`, id).Scan(&orphanPorts); err != nil {
		t.Fatalf("counting orphaned ports: %v", err)
	}
	if orphanPorts != 0 {
		t.Errorf("%d ports still reference the deleted server (FR-019)", orphanPorts)
	}
}
