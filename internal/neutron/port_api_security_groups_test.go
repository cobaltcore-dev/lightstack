package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 T030 (FR-017, FR-018, FR-019; contracts/network-api.md; research R12): security_groups on
// POST and PUT /v2.0/ports. Absent keeps the list on an update and gives default on a create; null
// and [] give none; a list gives exactly that list. A change reaches the port's floating-IP filter
// before the answer, and is undone, compare-and-set, when the node cannot apply it.

func newPortAPIRig(t *testing.T) (*gin.Engine, *Service, *fakeDataplane) {
	t.Helper()
	r, svc := newNeutronTestRouterWithService()
	fake := newFakeDataplane()
	svc.dataplane = fake
	return r, svc, fake
}

func projectHdr(projectID string) map[string]string {
	return map[string]string{"X-Test-Project": projectID}
}

func putPort(t *testing.T, r *gin.Engine, hdr map[string]string, portID, body string) (int, string) {
	t.Helper()
	return doJSONReq(r, http.MethodPut, "/v2.0/ports/"+portID, body, hdr)
}

func sgBody(field string) string { return `{"port":{"security_groups":` + field + `}}` }

func quoted(ids ...string) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

func sgTestPortName(t *testing.T, portID string) string {
	t.Helper()
	var name string
	if err := testPool.QueryRow(context.Background(), "SELECT name FROM ports WHERE id = $1", portID).Scan(&name); err != nil {
		t.Fatalf("read port name: %v", err)
	}
	return name
}

func TestUpdatePortSecurityGroups(t *testing.T) {
	requireDB(t)
	hdr := projectHdr(seededProjectID)

	t.Run("a list replaces the port's groups and moves updated_at", func(t *testing.T) {
		r, _, _ := newPortAPIRig(t)
		port := sgTestPort(t)
		def := sgTestGroup(t, seededProjectID, "spec033-old-"+uuid.NewString()[:6])
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		sgTestAttach(t, port, def)

		code, body := putPort(t, r, hdr, port, sgBody(quoted(web)))
		if code != http.StatusOK {
			t.Fatalf("PUT: code=%d body=%s; want 200", code, body)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{web}) {
			t.Errorf("groups: %v; want [%s]", got, web)
		}
		if !strings.Contains(body, web) {
			t.Errorf("the answer does not show the new list: %s", body)
		}
		if at := sgTestUpdatedAt(t, port); !at.After(spec033Epoch) {
			t.Errorf("updated_at did not move: %v", at)
		}
	})

	for _, empty := range []string{"[]", "null"} {
		t.Run(empty+" leaves the port with no group", func(t *testing.T) {
			r, _, _ := newPortAPIRig(t)
			port := sgTestPort(t)
			sgTestAttach(t, port, sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6]))
			if code, body := putPort(t, r, hdr, port, sgBody(empty)); code != http.StatusOK {
				t.Fatalf("PUT %s: code=%d body=%s; want 200", empty, code, body)
			}
			if got := sgTestGroupsOf(t, port); len(got) != 0 {
				t.Errorf("groups: %v; want none", got)
			}
		})
	}

	t.Run("absent keeps the groups; with a name, both apply", func(t *testing.T) {
		r, _, _ := newPortAPIRig(t)
		port := sgTestPort(t)
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		sgTestAttach(t, port, web)

		if code, body := putPort(t, r, hdr, port, `{"port":{"name":"spec033-renamed"}}`); code != http.StatusOK {
			t.Fatalf("PUT name: code=%d body=%s", code, body)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{web}) || sgTestPortName(t, port) != "spec033-renamed" {
			t.Errorf("after a name-only update: groups %v, name %q; want [web] and the new name", got, sgTestPortName(t, port))
		}

		if code, body := putPort(t, r, hdr, port, `{"port":{"name":"spec033-both","security_groups":`+quoted(db)+`}}`); code != http.StatusOK {
			t.Fatalf("PUT both: code=%d body=%s", code, body)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{db}) || sgTestPortName(t, port) != "spec033-both" {
			t.Errorf("after name and groups: groups %v, name %q; want [db] and spec033-both", got, sgTestPortName(t, port))
		}
	})

	t.Run("refusals, in upstream's order, change nothing", func(t *testing.T) {
		r, _, _ := newPortAPIRig(t)
		port := sgTestPort(t)
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		sgTestAttach(t, port, web)
		other := createTestProject(t, "spec033-other")
		foreign := sgTestGroup(t, other, "spec033-foreign-"+uuid.NewString()[:6])

		for _, tc := range []struct {
			name, port, body string
			hdr              map[string]string
			code             int
			says             string
		}{
			{"not a UUID", port, sgBody(`["x"]`), hdr, http.StatusBadRequest, "is not a valid UUID"},
			{"repeated", port, sgBody(quoted(web, web)), hdr, http.StatusBadRequest, "Duplicate items in the list"},
			{"a missing port", uuid.NewString(), sgBody(quoted(web)), hdr, http.StatusNotFound, "PortNotFound"},
			{"another project's group", port, sgBody(quoted(foreign)), hdr, http.StatusNotFound, "SecurityGroupNotFound"},
		} {
			code, body := putPort(t, r, tc.hdr, tc.port, tc.body)
			if code != tc.code || !strings.Contains(body, tc.says) {
				t.Errorf("%s: code=%d body=%s; want %d saying %q", tc.name, code, body, tc.code, tc.says)
			}
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{web}) {
			t.Errorf("groups after the refusals: %v; want [web], unchanged", got)
		}
		if at := sgTestUpdatedAt(t, port); at.After(spec033Epoch) {
			t.Errorf("updated_at moved: %v", at)
		}
	})

	// SPEC-033 round 2, T105 (FR-018; research R52): an administrator's id names any project's group,
	// as upstream's (neutron/db/securitygroups_db.py, _get_security_groups_on_port: context.is_admin).
	t.Run("an administrator names another project's group by id", func(t *testing.T) {
		r, _, _ := newPortAPIRig(t)
		port := sgTestPort(t)
		other := createTestProject(t, "spec033-other")
		foreign := sgTestGroup(t, other, "spec033-foreign-"+uuid.NewString()[:6])
		admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
		if code, body := putPort(t, r, admin, port, sgBody(quoted(foreign))); code != http.StatusOK {
			t.Fatalf("PUT another project's group as an administrator: code=%d body=%s; want 200", code, body)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{foreign}) {
			t.Errorf("groups: %v; want [foreign]", got)
		}
	})
}

// A port with a floating IP: the change reaches the filter before the answer, or is undone.
func TestUpdatePortSecurityGroups_FloatingIP(t *testing.T) {
	requireDB(t)
	hdr := projectHdr(seededProjectID)

	setup := func(t *testing.T) (r *gin.Engine, svc *Service, fake *fakeDataplane, port, fixed, web, db string) {
		t.Helper()
		r, svc, fake = newPortAPIRig(t)
		port = sgTestPort(t)
		old := sgTestGroup(t, seededProjectID, "spec033-old-"+uuid.NewString()[:6])
		sgTestAttach(t, port, old)
		web = sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		sgTestRule(t, web, "tcp", 80)
		db = sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		fixed = testFixedIP()
		seedFloatingIP(t, port, fixed)
		return r, svc, fake, port, fixed, web, db
	}

	t.Run("the filter gets the new rules", func(t *testing.T) {
		r, _, fake, port, fixed, web, _ := setup(t)
		if code, body := putPort(t, r, hdr, port, sgBody(quoted(web))); code != http.StatusOK {
			t.Fatalf("PUT: code=%d body=%s", code, body)
		}
		got, ok := fake.filterOf(fixed)
		if !ok || len(got) != 1 || got[0].Protocol != "tcp" || got[0].PortMin != 80 {
			t.Errorf("the filter of %s: %+v (present %v); want web's tcp/80", fixed, got, ok)
		}
	})

	t.Run("a failed rebuild answers 500 and puts the list back", func(t *testing.T) {
		r, _, fake, port, _, web, _ := setup(t)
		before := sgTestGroupsOf(t, port)
		fake.failWith(errors.New("iptables-restore: line 4 failed"))
		if code, body := putPort(t, r, hdr, port, sgBody(quoted(web))); code != http.StatusInternalServerError {
			t.Fatalf("PUT: code=%d body=%s; want 500", code, body)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, before) {
			t.Errorf("groups: %v; want %v back", got, before)
		}
	})

	t.Run("the undo leaves a list another request wrote meanwhile", func(t *testing.T) {
		r, _, fake, port, _, web, db := setup(t)
		first := true
		fake.setHook(func(dataplaneCall) error {
			if !first {
				return nil
			}
			first = false
			if err := database.WithTx(context.Background(), testPool, func(tx database.Tx) error {
				return SetPortSecurityGroups(context.Background(), tx, port, []string{db})
			}); err != nil {
				t.Errorf("the other request: %v", err)
			}
			return errors.New("iptables-restore: line 4 failed")
		})
		if code, body := putPort(t, r, hdr, port, sgBody(quoted(web))); code != http.StatusInternalServerError {
			t.Fatalf("PUT: code=%d body=%s; want 500", code, body)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{db}) {
			t.Errorf("groups: %v; want the other request's [db] kept", got)
		}
	})

	t.Run("a busy node answers 500 within about a second and puts the list back", func(t *testing.T) {
		r, svc, fake, port, _, web, _ := setup(t)
		before := sgTestGroupsOf(t, port)
		otherPort, otherFixed, _ := portWithFloatingIP(t, 22)
		entered, release := fake.blockFirst(otherFixed)
		t.Cleanup(release)
		done := make(chan error, 1)
		go func() { done <- svc.RefreshPortFilters(context.Background(), []string{otherPort}) }()
		waitEntered(t, entered)

		start := time.Now()
		code, body := putPort(t, r, hdr, port, sgBody(quoted(web)))
		elapsed := time.Since(start)
		release()
		if code != http.StatusInternalServerError {
			t.Fatalf("PUT on a busy node: code=%d body=%s; want 500", code, body)
		}
		if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
			t.Errorf("PUT took %v; want about one second, for the lock and then the rebuild after the undo", elapsed)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, before) {
			t.Errorf("groups: %v; want %v back", got, before)
		}
		if err := <-done; err != nil {
			t.Errorf("the blocked rebuild: %v", err)
		}
	})
}

func TestCreatePortSecurityGroups(t *testing.T) {
	requireDB(t)
	hdr := projectHdr(seededProjectID)
	r, _, _ := newPortAPIRig(t)
	netID := seedNetwork(t, "spec033-create-port-"+uuid.NewString()[:8])
	seedSubnet(t, netID, "spec033-create-port-subnet", "10.35.0.0/24", nil, 4, true)
	defaultID, err := EnsureDefaultSecurityGroup(context.Background(), testPool, seededProjectID)
	if err != nil {
		t.Fatal(err)
	}
	other := createTestProject(t, "spec033-other")
	foreign := sgTestGroup(t, other, "spec033-foreign-"+uuid.NewString()[:6])
	web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])

	create := func(field string) (int, string) {
		body := `{"port":{"network_id":"` + netID + `"`
		if field != "" {
			body += `,"security_groups":` + field
		}
		return doJSONReq(r, http.MethodPost, "/v2.0/ports", body+`}}`, hdr)
	}
	portID := func(t *testing.T, body string) string {
		t.Helper()
		var out struct {
			Port struct {
				ID string `json:"id"`
			} `json:"port"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil || out.Port.ID == "" {
			t.Fatalf("no port in %s (%v)", body, err)
		}
		return out.Port.ID
	}

	for _, tc := range []struct {
		name, field string
		want        []string
	}{
		{"[] gives no group", "[]", nil},
		{"null gives no group", "null", nil},
		{"absent gives default", "", []string{defaultID}},
		{"a list gives exactly it", quoted(web), []string{web}},
	} {
		code, body := create(tc.field)
		if code != http.StatusCreated {
			t.Errorf("%s: code=%d body=%s; want 201", tc.name, code, body)
			continue
		}
		if got := sgTestGroupsOf(t, portID(t, body)); !equalIDs(got, sorted(tc.want...)) {
			t.Errorf("%s: groups %v; want %v", tc.name, got, tc.want)
		}
	}

	var before int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM ports WHERE network_id = $1", netID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, field string
		code        int
	}{
		{"not a UUID", `["x"]`, http.StatusBadRequest},
		{"repeated", quoted(web, web), http.StatusBadRequest},
		{"another project's group", quoted(foreign), http.StatusNotFound},
	} {
		if code, body := create(tc.field); code != tc.code {
			t.Errorf("%s: code=%d body=%s; want %d", tc.name, code, body, tc.code)
		}
	}
	var after int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM ports WHERE network_id = $1", netID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("ports on the network: %d after the refusals, %d before; want none created", after, before)
	}

	// SPEC-033 round 2, T105 (FR-018; research R52): an administrator's id names any project's group.
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
	code, body := doJSONReq(r, http.MethodPost, "/v2.0/ports",
		`{"port":{"network_id":"`+netID+`","security_groups":`+quoted(foreign)+`}}`, admin)
	if code != http.StatusCreated {
		t.Fatalf("an administrator creating a port with another project's group: code=%d body=%s; want 201", code, body)
	}
	if got := sgTestGroupsOf(t, portID(t, body)); !equalIDs(got, []string{foreign}) {
		t.Errorf("groups: %v; want [foreign]", got)
	}
}
