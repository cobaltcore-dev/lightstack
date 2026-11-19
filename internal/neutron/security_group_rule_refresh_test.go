package neutron

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/google/uuid"
)

// SPEC-033 T037 (FR-008b; contracts/network-api.md; research R21): adding or deleting a rule rebuilds
// the filter of every port that carries its group and has a floating IP, before the answer; when the
// node cannot, the change is undone and the request answers 500.

type ruleFixture struct {
	fx                  *fipFixture
	web                 string
	withFIP, withoutFIP string // ports carrying web
	fixedWeb, fixedDB   string
}

func newRuleFixture(t *testing.T) *ruleFixture {
	t.Helper()
	fx := newFIPFixture(t)
	rf := &ruleFixture{fx: fx}
	rf.web = sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	db := sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
	// portX carries web and gets a floating IP; portY carries db and gets one; a third carries web
	// without one.
	mustExec(t, "DELETE FROM port_security_groups WHERE port_id = $1 OR port_id = $2", fx.portX, fx.portY)
	sgTestAttach(t, fx.portX, rf.web)
	sgTestAttach(t, fx.portY, db)
	rf.withFIP, rf.fixedWeb = fx.portX, fx.fixedX
	rf.withoutFIP, _ = seedPortWithIP(t, fx.network, fx.subnet)
	mustExec(t, "DELETE FROM port_security_groups WHERE port_id = $1", rf.withoutFIP)
	sgTestAttach(t, rf.withoutFIP, rf.web)
	fx.create(t, fx.portX)
	fx.create(t, fx.portY)
	rf.fixedDB = fx.fixedY
	return rf
}

func (rf *ruleFixture) addRule(t *testing.T) (int, string, string) {
	t.Helper()
	body := `{"security_group_rule":{"security_group_id":"` + rf.web + `","direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":8080,"port_range_max":8080}}`
	code, resp := doJSONReq(rf.fx.r, http.MethodPost, "/v2.0/security-group-rules", body, rf.fx.hdr)
	var out struct {
		Rule struct {
			ID string `json:"id"`
		} `json:"security_group_rule"`
	}
	_ = json.Unmarshal([]byte(resp), &out)
	return code, resp, out.Rule.ID
}

func has8080(rules []networking.IngressRule) bool {
	for _, r := range rules {
		if r.Protocol == "tcp" && r.PortMin == 8080 {
			return true
		}
	}
	return false
}

// failFirstFilter makes the first filter rebuild fail.
func (rf *ruleFixture) failFirstFilter() {
	failed := false
	rf.fx.fake.setHook(func(c dataplaneCall) error {
		if c.Method == "EnsureIngressFilter" && !failed {
			failed = true
			return errNode
		}
		return nil
	})
}

type ruleRow struct {
	ID, Group, Direction, Ether, Protocol string
	Min, Max                              int
	Created                               string
}

func readRule(t *testing.T, id string) (ruleRow, bool) {
	t.Helper()
	var r ruleRow
	var created time.Time
	var proto *string
	var min, max *int
	err := testPool.QueryRow(context.Background(), `
		SELECT id::text, security_group_id::text, direction, ethertype, protocol, port_range_min, port_range_max, created_at
		FROM security_group_rules WHERE id::text = $1`, id).Scan(&r.ID, &r.Group, &r.Direction, &r.Ether, &proto, &min, &max, &created)
	if err != nil {
		return ruleRow{}, false
	}
	if proto != nil {
		r.Protocol = *proto
	}
	if min != nil {
		r.Min = *min
	}
	if max != nil {
		r.Max = *max
	}
	r.Created = created.UTC().Format(time.RFC3339Nano)
	return r, true
}

func TestSecurityGroupRuleRefresh(t *testing.T) {
	requireDB(t)

	t.Run("add and delete rebuild only the floating-IP ports that carry the group", func(t *testing.T) {
		rf := newRuleFixture(t)
		before := len(rf.fx.fake.callsFor(rf.fixedDB))
		start := len(rf.fx.fake.callsFor(rf.fixedWeb))
		code, body, id := rf.addRule(t)
		if code != http.StatusCreated {
			t.Fatalf("add: code=%d body=%s", code, body)
		}
		calls := rf.fx.fake.callsFor(rf.fixedWeb)
		if len(calls) != start+1 || !has8080(calls[len(calls)-1].Rules) {
			t.Errorf("rebuilds of %s: %+v; want one more, with tcp/8080", rf.fixedWeb, calls[start:])
		}
		if n := len(rf.fx.fake.callsFor(rf.fixedDB)); n != before {
			t.Errorf("the db port's filter was rebuilt (%d → %d); want it left alone", before, n)
		}

		if code, body := doJSONReq(rf.fx.r, http.MethodDelete, "/v2.0/security-group-rules/"+id, "", rf.fx.hdr); code != http.StatusNoContent {
			t.Fatalf("delete: code=%d body=%s", code, body)
		}
		calls = rf.fx.fake.callsFor(rf.fixedWeb)
		if last := calls[len(calls)-1]; has8080(last.Rules) {
			t.Errorf("after the delete, the filter still has tcp/8080: %+v", last.Rules)
		}
	})

	t.Run("a failed rebuild after an add: 500, the rule gone, the filter rebuilt without it", func(t *testing.T) {
		rf := newRuleFixture(t)
		rf.failFirstFilter()
		code, body, _ := rf.addRule(t)
		if code != http.StatusInternalServerError {
			t.Fatalf("add: code=%d body=%s; want 500", code, body)
		}
		var n int
		if err := testPool.QueryRow(context.Background(),
			"SELECT count(*) FROM security_group_rules WHERE security_group_id = $1 AND port_range_min = 8080", rf.web).Scan(&n); err != nil || n != 0 {
			t.Errorf("rules with 8080: %d (%v); want the added rule removed", n, err)
		}
		calls := rf.fx.fake.callsFor(rf.fixedWeb)
		if len(calls) == 0 || has8080(calls[len(calls)-1].Rules) {
			t.Errorf("the last rebuild %+v; want one without tcp/8080 after the undo", calls)
		}
	})

	t.Run("a failed rebuild after a delete: 500, the rule back as it was", func(t *testing.T) {
		rf := newRuleFixture(t)
		code, body, id := rf.addRule(t)
		if code != http.StatusCreated {
			t.Fatalf("add: code=%d body=%s", code, body)
		}
		before, _ := readRule(t, id)
		rf.failFirstFilter()
		if code, body := doJSONReq(rf.fx.r, http.MethodDelete, "/v2.0/security-group-rules/"+id, "", rf.fx.hdr); code != http.StatusInternalServerError {
			t.Fatalf("delete: code=%d body=%s; want 500", code, body)
		}
		if after, ok := readRule(t, id); !ok || after != before {
			t.Errorf("rule %+v (present %v); want it back as %+v", after, ok, before)
		}
		calls := rf.fx.fake.callsFor(rf.fixedWeb)
		if len(calls) == 0 || !has8080(calls[len(calls)-1].Rules) {
			t.Errorf("the last rebuild %+v; want one with tcp/8080 after the undo", calls)
		}
	})

	t.Run("no uplink: an add answers 500 and is gone, a delete answers 500 and is back", func(t *testing.T) {
		rf := newRuleFixture(t)
		code, body, id := rf.addRule(t)
		if code != http.StatusCreated {
			t.Fatalf("add: code=%d body=%s", code, body)
		}
		rf.fx.svc.uplink = func() (string, error) { return "", errors.New("no IPv4 default route / uplink found") }
		if code, body := doJSONReq(rf.fx.r, http.MethodDelete, "/v2.0/security-group-rules/"+id, "", rf.fx.hdr); code != http.StatusInternalServerError {
			t.Errorf("delete: code=%d body=%s; want 500", code, body)
		}
		if _, ok := readRule(t, id); !ok {
			t.Error("delete: the rule did not come back")
		}
		code, body, _ = rf.addRule(t)
		if code != http.StatusInternalServerError {
			t.Errorf("add: code=%d body=%s; want 500", code, body)
		}
		var n int
		if err := testPool.QueryRow(context.Background(),
			"SELECT count(*) FROM security_group_rules WHERE security_group_id = $1 AND port_range_min = 8080", rf.web).Scan(&n); err != nil || n != 1 {
			t.Errorf("rules with 8080: %d (%v); want only the first, the second add removed", n, err)
		}
	})
}
