package neutron

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 T038 (FR-008c; contracts/network-api.md; research R22): a group a port carries cannot be
// deleted, as upstream: 409 SecurityGroupInUse, and nothing changes. A missing group, or another
// project's, is 404, administrators included (constitution register, entry 5).

func groupExists(t *testing.T, id string) bool {
	t.Helper()
	var ok bool
	if err := testPool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM security_groups WHERE id = $1)", id).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func ruleCount(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM security_group_rules WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeleteSecurityGroup(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := projectHdr(seededProjectID)
	del := func(id string, h map[string]string) (int, string) {
		return doReq(r, http.MethodDelete, "/v2.0/security-groups/"+id, h)
	}

	t.Run("a group a port carries: 409 SecurityGroupInUse, nothing changes", func(t *testing.T) {
		port := sgTestPort(t)
		web := sgTestGroup(t, seededProjectID, "spec033-inuse-"+uuid.NewString()[:6])
		sgTestRule(t, web, "tcp", 80)
		sgTestAttach(t, port, web)

		code, body := del(web, hdr)
		if code != http.StatusConflict || !strings.Contains(body, "SecurityGroupInUse") || !strings.Contains(body, web) {
			t.Errorf("delete: code=%d body=%s; want 409 SecurityGroupInUse naming the group", code, body)
		}
		if !groupExists(t, web) || ruleCount(t, "security_group_id = $1", web) != 1 {
			t.Error("the group or its rule is gone")
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{web}) {
			t.Errorf("the port's groups: %v; want [web], unchanged", got)
		}

		// Once the port drops it, it can go.
		mustExec(t, "DELETE FROM port_security_groups WHERE port_id = $1", port)
		if code, body := del(web, hdr); code != http.StatusNoContent {
			t.Errorf("delete after the port dropped it: code=%d body=%s; want 204", code, body)
		}
	})

	t.Run("no such group, or another project's: 404, administrators included", func(t *testing.T) {
		other := createTestProject(t, "spec033-del-other")
		foreign := sgTestGroup(t, other, "spec033-foreign-"+uuid.NewString()[:6])
		admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}
		for _, tc := range []struct {
			name, id string
			hdr      map[string]string
		}{
			{"no such group", uuid.NewString(), hdr},
			{"a malformed id", "not-a-uuid", hdr},
			{"another project's", foreign, hdr},
			{"another project's, as an administrator", foreign, admin},
		} {
			if code, body := del(tc.id, tc.hdr); code != http.StatusNotFound {
				t.Errorf("%s: code=%d body=%s; want 404", tc.name, code, body)
			}
		}
		if !groupExists(t, foreign) {
			t.Error("another project's group was deleted")
		}
	})

	t.Run("an unused group: 204, its rules and the rules naming it as remote go too", func(t *testing.T) {
		gone := sgTestGroup(t, seededProjectID, "spec033-gone-"+uuid.NewString()[:6])
		sgTestRule(t, gone, "tcp", 443)
		keeper := sgTestGroup(t, seededProjectID, "spec033-keeper-"+uuid.NewString()[:6])
		mustExec(t, `INSERT INTO security_group_rules (security_group_id, direction, ethertype, remote_group_id)
			VALUES ($1, 'ingress', 'IPv4', $2)`, keeper, gone)

		if code, body := del(gone, hdr); code != http.StatusNoContent {
			t.Fatalf("delete: code=%d body=%s; want 204", code, body)
		}
		if groupExists(t, gone) || ruleCount(t, "security_group_id = $1", gone) != 0 {
			t.Error("the group or its rules are still there")
		}
		if n := ruleCount(t, "remote_group_id = $1", gone); n != 0 {
			t.Errorf("%d rules still name the deleted group as remote; want none", n)
		}
		if !groupExists(t, keeper) {
			t.Error("the other group was deleted")
		}
	})
}
