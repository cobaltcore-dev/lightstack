package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
)

// SPEC-033 T250 (FR-050): a rule's delete takes its group's lock before the rule's, the order the group's
// delete takes them in (the group, then its rules through the cascade). Taken the other way round — the rule,
// then the group for its revision — the two deadlocked on PostgreSQL and one answered 500. PostgreSQL only:
// a transaction holds the group's lock as a group delete does; once the rule delete waits, the rule must still
// be free, and the delete goes through when the lock is released.
func TestRuleDeleteLocksTheGroupFirst(t *testing.T) {
	requireDB(t)
	dbtest.RequirePostgres(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	group := createGroupViaAPI(t, r, hdr, `{"security_group":{"name":"spec033-t250"}}`)["id"].(string)
	code, body := doJSONReq(r, http.MethodPost, "/v2.0/security-group-rules",
		`{"security_group_rule":{"security_group_id":"`+group+`","direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":22,"port_range_max":22}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create rule: code=%d body=%s", code, body)
	}
	var created struct {
		Rule struct {
			ID string `json:"id"`
		} `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	rule := created.Rule.ID

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op once committed
	holder := dbtest.HolderPID(t, tx)
	if _, err := tx.Exec(ctx, "SELECT id FROM security_groups WHERE id = $1 FOR UPDATE", group); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := doJSONReq(r, http.MethodDelete, "/v2.0/security-group-rules/"+rule, "", hdr)
		done <- code
	}()
	dbtest.WaitUntilBlocked(t, testPool, holder)
	probe, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	//sqlcheck:postgres-only
	_, probeErr := probe.Exec(ctx, "SELECT id FROM security_group_rules WHERE id = $1 FOR UPDATE NOWAIT", rule)
	_ = probe.Rollback(ctx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusNoContent {
		t.Errorf("the rule delete once the group's lock was released: code=%d; want 204", code)
	}
	if probeErr != nil {
		t.Errorf("the rule was locked while its delete waited for the group (%v): the group delete's cascade "+
			"waits for that lock while holding the group's, a deadlock", probeErr)
	}
}
