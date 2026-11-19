package nova

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 T010: addSecurityGroup and removeSecurityGroup change the server's ports, rebuild their
// floating-IP filters before answering, and undo their own change, compare-and-set, when the rebuild
// fails (FR-001–FR-004, FR-008; contracts/compute-security-groups.md; research R4, R5).

// refreshRecorder is the Neutron service with RefreshPortFilters observed. Every call is recorded.
// The first call runs before, if set, and then fails with failFirst, if set; every other call goes
// to the real service, which in stub mode programs nothing.
type refreshRecorder struct {
	*neutron.Service
	mu        sync.Mutex
	calls     [][]string
	before    func()
	failFirst error
}

func (r *refreshRecorder) RefreshPortFilters(ctx context.Context, portIDs []string) error {
	r.mu.Lock()
	r.calls = append(r.calls, slices.Clone(portIDs))
	first := len(r.calls) == 1
	r.mu.Unlock()
	if first {
		if r.before != nil {
			r.before()
		}
		if r.failFirst != nil {
			return r.failFirst
		}
	}
	return r.Service.RefreshPortFilters(ctx, portIDs)
}

func (r *refreshRecorder) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func newSGActionRig(t *testing.T) (*gin.Engine, *refreshRecorder) {
	t.Helper()
	r, svc, network := newNovaWithNetwork(t)
	rec := &refreshRecorder{Service: network}
	svc.SetNeutronService(rec)
	return r, rec
}

// tenantHdr scopes a request to projectID without the administrator role.
func tenantHdr(projectID string) map[string]string {
	return map[string]string{"X-Test-Project": projectID, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
}

func sgAction(t *testing.T, r *gin.Engine, hdr map[string]string, serverID, verb, ref string) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{verb: map[string]string{"name": ref}})
	if err != nil {
		t.Fatal(err)
	}
	return doReq(r, http.MethodPost, "/v2.1/servers/"+serverID+"/action", string(body), hdr)
}

func groupsOf(t *testing.T, portID string) []string {
	t.Helper()
	ids := portGroupIDs(t, portID)
	slices.Sort(ids)
	return ids
}

func ids(v ...string) []string {
	out := slices.Clone(v)
	slices.Sort(out)
	return out
}

func defaultGroup(t *testing.T, projectID string) string {
	t.Helper()
	id, err := neutron.EnsureDefaultSecurityGroup(t.Context(), testPool, projectID)
	if err != nil {
		t.Fatalf("ensure default: %v", err)
	}
	return id
}

// tenantProject creates a project of its own, removed with everything in it by cleanup.
func tenantProject(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	createTestProject(t, id, "spec033-tenant-"+id[:8])
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM projects WHERE id = $1", id)
	})
	return id
}

// serverWithPorts creates a server in projectID and n ports for it in default.
func serverWithPorts(t *testing.T, r *gin.Engine, rec *refreshRecorder, projectID string, n int) (string, []string) {
	t.Helper()
	server, netID := spec033Server(t, r, projectID)
	var ports []string
	for range n {
		ports = append(ports, allocateTestPort(t, rec.Service, netID, projectID, server, nil))
	}
	return server, ports
}

func setGroups(t *testing.T, portID string, groupIDs ...string) {
	t.Helper()
	if err := database.WithTx(t.Context(), testPool, func(tx database.Tx) error {
		return neutron.SetPortSecurityGroups(t.Context(), tx, portID, groupIDs)
	}); err != nil {
		t.Fatalf("set the groups of %s: %v", portID, err)
	}
}

func wantStatus(t *testing.T, what string, code int, body string, want int) {
	t.Helper()
	if code != want {
		t.Fatalf("%s: code=%d body=%s; want %d", what, code, body, want)
	}
}

func TestAddSecurityGroup_ReachesThePorts(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	def := defaultGroup(t, seededProjectID)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, seededProjectID, 1)

	code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "addSecurityGroup", groupName(t, web))
	wantStatus(t, "add", code, body, http.StatusAccepted)
	if got := groupsOf(t, ports[0]); !slices.Equal(got, ids(def, web)) {
		t.Errorf("port groups: %v; want default and web", got)
	}
	if names, _ := reportOf(t, r, seededProjectID, server); !slices.Equal(names, wantNames("default", groupName(t, web))) {
		t.Errorf("report: %v; want default and web", names)
	}
	if calls := rec.recorded(); len(calls) != 1 || !slices.Equal(calls[0], ports) {
		t.Errorf("RefreshPortFilters calls: %v; want one with %v", calls, ports)
	}
}

func TestSecurityGroupActions_TwoPorts(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	def := defaultGroup(t, seededProjectID)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, seededProjectID, 2)

	code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "addSecurityGroup", groupName(t, web))
	wantStatus(t, "add", code, body, http.StatusAccepted)
	for _, p := range ports {
		if got := groupsOf(t, p); !slices.Equal(got, ids(def, web)) {
			t.Errorf("after add, port %s: %v; want default and web", p, got)
		}
	}
	if calls := rec.recorded(); len(calls) != 1 || !slices.Equal(ids(calls[0]...), ids(ports...)) {
		t.Errorf("RefreshPortFilters calls: %v; want one with both ports", calls)
	}

	code, body = sgAction(t, r, tenantHdr(seededProjectID), server, "removeSecurityGroup", groupName(t, web))
	wantStatus(t, "remove", code, body, http.StatusAccepted)
	for _, p := range ports {
		if got := groupsOf(t, p); !slices.Equal(got, []string{def}) {
			t.Errorf("after remove, port %s: %v; want default only", p, got)
		}
	}
}

func TestSecurityGroupActions_ByID(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	def := defaultGroup(t, seededProjectID)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, seededProjectID, 1)

	code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "addSecurityGroup", web)
	wantStatus(t, "add by id", code, body, http.StatusAccepted)
	if got := groupsOf(t, ports[0]); !slices.Equal(got, ids(def, web)) {
		t.Errorf("after add by id: %v; want default and web", got)
	}
	code, body = sgAction(t, r, tenantHdr(seededProjectID), server, "removeSecurityGroup", web)
	wantStatus(t, "remove by id", code, body, http.StatusAccepted)
	if got := groupsOf(t, ports[0]); !slices.Equal(got, []string{def}) {
		t.Errorf("after remove by id: %v; want default only", got)
	}
}

func TestSecurityGroupActions_UnknownOrForeignGroup_404(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	def := defaultGroup(t, seededProjectID)
	other := tenantProject(t)
	foreignName := "spec033-foreign-" + uuid.NewString()[:6]
	foreign := createTestGroup(t, other, foreignName)
	server, ports := serverWithPorts(t, r, rec, seededProjectID, 1)

	for _, verb := range []string{"addSecurityGroup", "removeSecurityGroup"} {
		for _, ref := range []string{"spec033-no-such-group", foreignName, foreign} {
			code, body := sgAction(t, r, tenantHdr(seededProjectID), server, verb, ref)
			wantStatus(t, verb+" "+ref, code, body, http.StatusNotFound)
		}
	}
	if got := groupsOf(t, ports[0]); !slices.Equal(got, []string{def}) {
		t.Errorf("port groups: %v; want default only, unchanged", got)
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("RefreshPortFilters calls: %v; want none", calls)
	}
}

// Register entry 5: an administrator acting on a tenant's server resolves the group in the server's
// project, so the tenant's group resolves by name and the administrator's own group does not.
func TestSecurityGroupActions_Administrator(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	tenant := tenantProject(t)
	tenantDefault := defaultGroup(t, tenant)
	webName := "spec033-web-" + uuid.NewString()[:6]
	tenantWeb := createTestGroup(t, tenant, webName)
	adminOwn := createTestGroup(t, seededProjectID, "spec033-admin-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, tenant, 1)
	admin := testAuthHdr(seededProjectID, seededUserID)

	code, body := sgAction(t, r, admin, server, "addSecurityGroup", webName)
	wantStatus(t, "administrator adds the tenant's group by name", code, body, http.StatusAccepted)
	if got := groupsOf(t, ports[0]); !slices.Equal(got, ids(tenantDefault, tenantWeb)) {
		t.Errorf("port groups: %v; want the tenant's default and web", got)
	}
	code, body = sgAction(t, r, admin, server, "addSecurityGroup", adminOwn)
	wantStatus(t, "administrator adds their own group by id", code, body, http.StatusNotFound)
	if got := groupsOf(t, ports[0]); !slices.Equal(got, ids(tenantDefault, tenantWeb)) {
		t.Errorf("port groups after the refused add: %v; want unchanged", got)
	}
}

func TestSecurityGroupActions_ServerWithoutPorts_404(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	defaultGroup(t, seededProjectID)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	server, _ := spec033Server(t, r, seededProjectID)

	for _, verb := range []string{"addSecurityGroup", "removeSecurityGroup"} {
		code, body := sgAction(t, r, tenantHdr(seededProjectID), server, verb, groupName(t, web))
		wantStatus(t, verb+" on a server without ports", code, body, http.StatusNotFound)
		if !strings.Contains(body, "could not be found as device id on any ports") {
			t.Errorf("%s: body %s; want upstream's message", verb, body)
		}
	}
	if names, present := reportOf(t, r, seededProjectID, server); present {
		t.Errorf("report: %v; want none", names)
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("RefreshPortFilters calls: %v; want none", calls)
	}
}

func TestAddSecurityGroup_AlreadyOnAPort_400(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	def := defaultGroup(t, seededProjectID)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, seededProjectID, 2)
	setGroups(t, ports[0], def, web)

	code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "addSecurityGroup", groupName(t, web))
	wantStatus(t, "add a group one port carries", code, body, http.StatusBadRequest)
	if got := groupsOf(t, ports[0]); !slices.Equal(got, ids(def, web)) {
		t.Errorf("port 0: %v; want unchanged", got)
	}
	if got := groupsOf(t, ports[1]); !slices.Equal(got, []string{def}) {
		t.Errorf("port 1: %v; want unchanged", got)
	}
}

func TestRemoveSecurityGroup_NotCarried_404(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	def := defaultGroup(t, seededProjectID)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, seededProjectID, 1)

	code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "removeSecurityGroup", groupName(t, web))
	wantStatus(t, "remove a group no port carries", code, body, http.StatusNotFound)
	if got := groupsOf(t, ports[0]); !slices.Equal(got, []string{def}) {
		t.Errorf("port groups: %v; want unchanged", got)
	}
}

// User Story 5, second scenario: a server shaped like v0.6.1's, whose port carries no group and for
// which nothing was recorded, can be given a group, and the port then carries it.
func TestAddSecurityGroup_EarlierReleaseServer(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, seededProjectID, 1)
	setGroups(t, ports[0])
	if _, err := testPool.Exec(t.Context(), "DELETE FROM server_security_groups WHERE server_id = $1", server); err != nil {
		t.Fatal(err)
	}

	code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "addSecurityGroup", groupName(t, web))
	wantStatus(t, "add on an earlier release's server", code, body, http.StatusAccepted)
	if got := groupsOf(t, ports[0]); !slices.Equal(got, []string{web}) {
		t.Errorf("port groups: %v; want web", got)
	}
	if names, _ := reportOf(t, r, seededProjectID, server); !slices.Equal(names, []string{groupName(t, web)}) {
		t.Errorf("report: %v; want web", names)
	}
}

// FR-008: a change the node cannot apply is undone and answered 500, and the filters are rebuilt
// again from what the database holds after the undo.
func TestSecurityGroupActions_FailedRebuildIsUndone(t *testing.T) {
	requireDB(t)
	errRebuild := errors.New("iptables-restore: line 4 failed")

	t.Run("add", func(t *testing.T) {
		r, rec := newSGActionRig(t)
		def := defaultGroup(t, seededProjectID)
		web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		server, ports := serverWithPorts(t, r, rec, seededProjectID, 2)
		rec.failFirst = errRebuild

		code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "addSecurityGroup", groupName(t, web))
		wantStatus(t, "add with a failed rebuild", code, body, http.StatusInternalServerError)
		for _, p := range ports {
			if got := groupsOf(t, p); !slices.Equal(got, []string{def}) {
				t.Errorf("port %s: %v; want default only, as before", p, got)
			}
		}
		if calls := rec.recorded(); len(calls) != 2 {
			t.Errorf("RefreshPortFilters calls: %v; want a second one after the undo", calls)
		}
	})

	t.Run("remove", func(t *testing.T) {
		r, rec := newSGActionRig(t)
		def := defaultGroup(t, seededProjectID)
		web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		server, ports := serverWithPorts(t, r, rec, seededProjectID, 2)
		for _, p := range ports {
			setGroups(t, p, def, web)
		}
		rec.failFirst = errRebuild

		code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "removeSecurityGroup", groupName(t, web))
		wantStatus(t, "remove with a failed rebuild", code, body, http.StatusInternalServerError)
		for _, p := range ports {
			if got := groupsOf(t, p); !slices.Equal(got, ids(def, web)) {
				t.Errorf("port %s: %v; want web back", p, got)
			}
		}
		if calls := rec.recorded(); len(calls) != 2 {
			t.Errorf("RefreshPortFilters calls: %v; want a second one after the undo", calls)
		}
	})
}

// FR-008's compare-and-set: the undo puts back only a port whose list is still what this request
// left; a port another request has changed since keeps that change.
func TestSecurityGroupActions_UndoIsCompareAndSet(t *testing.T) {
	requireDB(t)
	errRebuild := errors.New("iptables-restore: line 4 failed")

	t.Run("add", func(t *testing.T) {
		r, rec := newSGActionRig(t)
		def := defaultGroup(t, seededProjectID)
		web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := createTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		server, ports := serverWithPorts(t, r, rec, seededProjectID, 2)
		// Another request, between this one's commit and its rebuild, sets the port to [db, web]. It
		// keeps web, so an undo that removed web without comparing would undo part of that request.
		rec.before = func() { setGroups(t, ports[0], db, web) }
		rec.failFirst = errRebuild

		code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "addSecurityGroup", groupName(t, web))
		wantStatus(t, "add with a failed rebuild", code, body, http.StatusInternalServerError)
		if got := groupsOf(t, ports[0]); !slices.Equal(got, ids(db, web)) {
			t.Errorf("the port the other request changed: %v; want its list [db, web] kept", got)
		}
		if got := groupsOf(t, ports[1]); !slices.Equal(got, []string{def}) {
			t.Errorf("the other port: %v; want default, put back", got)
		}
	})

	t.Run("remove", func(t *testing.T) {
		r, rec := newSGActionRig(t)
		def := defaultGroup(t, seededProjectID)
		web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := createTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		server, ports := serverWithPorts(t, r, rec, seededProjectID, 2)
		for _, p := range ports {
			setGroups(t, p, def, web)
		}
		rec.before = func() { setGroups(t, ports[0], db) }
		rec.failFirst = errRebuild

		code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "removeSecurityGroup", groupName(t, web))
		wantStatus(t, "remove with a failed rebuild", code, body, http.StatusInternalServerError)
		if got := groupsOf(t, ports[0]); !slices.Equal(got, []string{db}) {
			t.Errorf("the port the other request changed: %v; want its list [db] kept", got)
		}
		if got := groupsOf(t, ports[1]); !slices.Equal(got, ids(def, web)) {
			t.Errorf("the other port: %v; want web put back", got)
		}
	})
}

// SPEC-033 US2 (FR-005): removing a server's last group answers 202, as upstream, and its ports then
// carry no group. A floating IP on such a port admits replies only.
func TestRemoveSecurityGroup_LastGroup(t *testing.T) {
	requireDB(t)

	t.Run("one port: closed, then a new group", func(t *testing.T) {
		r, rec := newSGActionRig(t)
		web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := createTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		server, ports := serverWithPorts(t, r, rec, seededProjectID, 1)
		setGroups(t, ports[0], web)

		code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "removeSecurityGroup", groupName(t, web))
		wantStatus(t, "remove the only group", code, body, http.StatusAccepted)
		if got := groupsOf(t, ports[0]); len(got) != 0 {
			t.Errorf("port groups: %v; want none", got)
		}
		if names, present := reportOf(t, r, seededProjectID, server); present {
			t.Errorf("report: %v; want no security_groups", names)
		}
		if calls := rec.recorded(); len(calls) != 1 || !slices.Equal(calls[0], ports) {
			t.Errorf("RefreshPortFilters calls: %v; want one with %v", calls, ports)
		}

		code, body = sgAction(t, r, tenantHdr(seededProjectID), server, "addSecurityGroup", groupName(t, db))
		wantStatus(t, "add a group to the closed server", code, body, http.StatusAccepted)
		if got := groupsOf(t, ports[0]); !slices.Equal(got, []string{db}) {
			t.Errorf("port groups: %v; want db", got)
		}
		if names, _ := reportOf(t, r, seededProjectID, server); !slices.Equal(names, []string{groupName(t, db)}) {
			t.Errorf("report: %v; want db", names)
		}
	})

	t.Run("two ports carrying only that group: both emptied", func(t *testing.T) {
		r, rec := newSGActionRig(t)
		web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		server, ports := serverWithPorts(t, r, rec, seededProjectID, 2)
		for _, p := range ports {
			setGroups(t, p, web)
		}

		code, body := sgAction(t, r, tenantHdr(seededProjectID), server, "removeSecurityGroup", groupName(t, web))
		wantStatus(t, "remove the only group of two ports", code, body, http.StatusAccepted)
		for _, p := range ports {
			if got := groupsOf(t, p); len(got) != 0 {
				t.Errorf("port %s: %v; want none", p, got)
			}
		}
	})
}
