package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// SPEC-033 T195: a port's show and listing read its groups through the one reader
// (portsSecurityGroups), the listing all its ports' in one query after its rows are read: in order, and a
// failed read answers 500 instead of a port with no groups. A row with no name, MAC or status — as no
// path of Lightstack's writes, but a row it cannot scan used to abort the whole listing — lists too.

func TestPortReadsTheirGroupsThroughTheOneReader(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	network := seedNetwork(t, "spec033-src23-"+uuid.NewString()[:6])
	port := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, 'spec033-src23', $2, $3, 'fa:16:3e:23:00:01', 'ACTIVE', '[]'::jsonb)`, port, network, seededProjectID)
	bare := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, network_id, project_id, fixed_ips) VALUES ($1, $2, $3, '[]'::jsonb)`, bare, network, seededProjectID)
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id::text = ANY($1)", []string{port, bare}) })
	b, a := sgTestGroup(t, seededProjectID, "spec033-src23-b"), sgTestGroup(t, seededProjectID, "spec033-src23-a")
	mustExec(t, "INSERT INTO port_security_groups (port_id, security_group_id) VALUES ($1, $2), ($1, $3)", port, b, a)
	want := sorted(a, b)

	groupsOf := func(t *testing.T, ports []map[string]any, id string) []string {
		t.Helper()
		for _, p := range ports {
			if p["id"] == id {
				var out []string
				for _, g := range p["security_groups"].([]any) {
					out = append(out, g.(string))
				}
				return out
			}
		}
		t.Fatalf("port %s not listed", id)
		return nil
	}

	code, body := doReq(r, http.MethodGet, "/v2.0/ports?network_id="+network, hdr)
	var list struct {
		Ports []map[string]any `json:"ports"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil {
		t.Fatalf("list: code=%d body=%s; want 200", code, body)
	}
	if got := groupsOf(t, list.Ports, port); !equalIDs(got, want) {
		t.Errorf("listed groups %v; want %v, in order", got, want)
	}
	groupsOf(t, list.Ports, bare)

	code, body = doReq(r, http.MethodGet, "/v2.0/ports/"+port, hdr)
	var show struct {
		Port map[string]any `json:"port"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &show) != nil {
		t.Fatalf("show: code=%d body=%s; want 200", code, body)
	}
	if got := groupsOf(t, []map[string]any{show.Port}, port); !equalIDs(got, want) {
		t.Errorf("shown groups %v; want %v, in order", got, want)
	}

	// A read that fails answers 500, not a port without groups.
	release := dbtest.Unavailable(t, testPool, "port_security_groups")
	defer release()
	for _, path := range []string{"/v2.0/ports/" + port, "/v2.0/ports?network_id=" + network} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		cancel()
		if w.Code != http.StatusInternalServerError {
			t.Errorf("GET %s with the groups unreadable: code=%d body=%s; want 500", path, w.Code, w.Body.String())
		}
	}
}
