package nova

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 round 2, T082 (FR-026; research R53): group names may repeat within a project, as
// upstream's do. Where compute resolves a group by name — server create, addSecurityGroup,
// removeSecurityGroup — a name two of the project's groups share answers 409 with upstream Nova's
// NoUniqueMatch message and changes nothing; either group's id still works.

const noUniqueMatch = "Multiple security_group matches found for name"

func TestAmbiguousGroupName(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	tenant := tenantProject(t)
	hdr := tenantHdr(tenant)
	name := "spec033-twin-" + uuid.NewString()[:6]
	first := createTestGroup(t, tenant, name)
	second := createTestGroup(t, tenant, name)

	t.Run("server create", func(t *testing.T) {
		before := countServers(t, tenant)
		code, body := doReq(r, http.MethodPost, "/v2.1/servers", withImage(t, tenant,
			`{"server":{"name":"spec033-twin","flavorRef":"m1.small","security_groups":[{"name":"`+name+`"}]}}`), hdr)
		if code != http.StatusConflict || !strings.Contains(body, noUniqueMatch) {
			t.Fatalf("create naming a shared name: code=%d body=%s; want 409 NoUniqueMatch", code, body)
		}
		if after := countServers(t, tenant); after != before {
			t.Errorf("a refused create left %d server(s)", after-before)
		}
	})

	server, ports := serverWithPorts(t, r, rec, tenant, 1)
	setGroups(t, ports[0], first)

	t.Run("add and remove by the shared name", func(t *testing.T) {
		for _, verb := range []string{"addSecurityGroup", "removeSecurityGroup"} {
			code, body := sgAction(t, r, hdr, server, verb, name)
			if code != http.StatusConflict || !strings.Contains(body, noUniqueMatch) {
				t.Errorf("%s by a shared name: code=%d body=%s; want 409 NoUniqueMatch", verb, code, body)
			}
			if got := groupsOf(t, ports[0]); !slices.Equal(got, []string{first}) {
				t.Errorf("after the refused %s: %v; want [first] unchanged", verb, got)
			}
		}
	})

	t.Run("by either id", func(t *testing.T) {
		code, body := sgAction(t, r, hdr, server, "addSecurityGroup", second)
		wantStatus(t, "add the second by id", code, body, http.StatusAccepted)
		if got := groupsOf(t, ports[0]); !slices.Equal(got, ids(first, second)) {
			t.Errorf("after adding by id: %v; want both", got)
		}
		code, body = sgAction(t, r, hdr, server, "removeSecurityGroup", first)
		wantStatus(t, "remove the first by id", code, body, http.StatusAccepted)
		if got := groupsOf(t, ports[0]); !slices.Equal(got, []string{second}) {
			t.Errorf("after removing by id: %v; want [second]", got)
		}
	})

	t.Run("server create by id", func(t *testing.T) {
		createTestServer(t, r, tenant, `{"server":{"name":"spec033-twin-id","flavorRef":"m1.small","security_groups":[{"name":"`+first+`"}]}}`)
	})
}

func countServers(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(), "SELECT count(*) FROM instances WHERE project_id = $1", projectID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
