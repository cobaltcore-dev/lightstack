package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 T037, reworked by T130 (FR-008b; contracts/network-api.md; research R21, R32): adding or
// deleting a rule converges the firewall of every bound port that carries its group, on its node,
// before the answer; when the node cannot, the change is undone and the request answers 500. One node
// in stub mode with its firewall replaced by the fake; the ports bound to it.

type ruleFixture struct {
	r       *gin.Engine
	svc     *Service
	fw      *fakeFirewall
	hdr     map[string]string
	web     string
	bound   string // carries web, bound to the node
	other   string // carries another group, bound to the node
	unbound string // carries web, bound nowhere
}

func newRuleFixture(t *testing.T) *ruleFixture {
	t.Helper()
	r, svc := newNeutronTestRouterWithService()
	rf := &ruleFixture{r: r, svc: svc, fw: newFakeFirewall(), hdr: projectHdr(seededProjectID)}
	svc.firewall = rf.fw
	network := seedNetwork(t, "spec033-rule-net-"+uuid.NewString()[:6])
	subnet := seedSubnet(t, network, "spec033-rule-subnet", "10.36.0.0/16", nil, 4, false)
	rf.web = sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	db := sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
	for _, p := range []struct {
		port  *string
		group string
		bound bool
	}{{&rf.bound, rf.web, true}, {&rf.other, db, true}, {&rf.unbound, rf.web, false}} {
		*p.port, _ = seedPortWithIP(t, network, subnet)
		mustExec(t, "DELETE FROM port_security_groups WHERE port_id = $1", *p.port)
		sgTestAttach(t, *p.port, p.group)
		if p.bound {
			bind(t, *p.port, svc.nodes.Name)
		}
	}
	return rf
}

func (rf *ruleFixture) addRule(t *testing.T) (int, string, string) {
	t.Helper()
	body := `{"security_group_rule":{"security_group_id":"` + rf.web + `","direction":"ingress","ethertype":"IPv4","protocol":"tcp","port_range_min":8080,"port_range_max":8080}}`
	code, resp := doJSONReq(rf.r, http.MethodPost, "/v2.0/security-group-rules", body, rf.hdr)
	var out struct {
		Rule struct {
			ID string `json:"id"`
		} `json:"security_group_rule"`
	}
	_ = json.Unmarshal([]byte(resp), &out)
	return code, resp, out.Rule.ID
}

// has8080 reports whether the node's firewall lets tcp/8080 in to port.
func (rf *ruleFixture) has8080(port string) bool {
	p, ok := rf.fw.port(port)
	return ok && allows(p, "ingress", "tcp", 8080, "")
}

// failFirstApply makes the node's next convergence fail.
func (rf *ruleFixture) failFirstApply() { rf.fw.failFirst(1, errNode) }

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

	t.Run("add and delete reach the bound ports that carry the group", func(t *testing.T) {
		rf := newRuleFixture(t)
		code, body, id := rf.addRule(t)
		if code != http.StatusCreated {
			t.Fatalf("add: code=%d body=%s", code, body)
		}
		if !rf.has8080(rf.bound) {
			t.Errorf("the bound port carrying the group lacks tcp/8080 after the add")
		}
		if rf.has8080(rf.other) {
			t.Error("a port of another group got the rule")
		}
		if _, ok := rf.fw.port(rf.unbound); ok {
			t.Error("an unbound port has a firewall")
		}

		if code, body := doJSONReq(rf.r, http.MethodDelete, "/v2.0/security-group-rules/"+id, "", rf.hdr); code != http.StatusNoContent {
			t.Fatalf("delete: code=%d body=%s", code, body)
		}
		if rf.has8080(rf.bound) {
			t.Error("after the delete, the port still lets tcp/8080 in")
		}
	})

	t.Run("a failed add: 500, the rule gone, the port converged without it", func(t *testing.T) {
		rf := newRuleFixture(t)
		rf.failFirstApply()
		code, body, _ := rf.addRule(t)
		if code != http.StatusInternalServerError {
			t.Fatalf("add: code=%d body=%s; want 500", code, body)
		}
		var n int
		if err := testPool.QueryRow(context.Background(),
			"SELECT count(*) FROM security_group_rules WHERE security_group_id = $1 AND port_range_min = 8080", rf.web).Scan(&n); err != nil || n != 0 {
			t.Errorf("rules with 8080: %d (%v); want the added rule removed", n, err)
		}
		if _, ok := rf.fw.port(rf.bound); !ok || rf.has8080(rf.bound) {
			t.Errorf("the port after the undo: held %v, tcp/8080 %v; want it converged without the rule", ok, rf.has8080(rf.bound))
		}
	})

	t.Run("a failed delete: 500, the rule back as it was", func(t *testing.T) {
		rf := newRuleFixture(t)
		code, body, id := rf.addRule(t)
		if code != http.StatusCreated {
			t.Fatalf("add: code=%d body=%s", code, body)
		}
		before, _ := readRule(t, id)
		rf.failFirstApply()
		if code, body := doJSONReq(rf.r, http.MethodDelete, "/v2.0/security-group-rules/"+id, "", rf.hdr); code != http.StatusInternalServerError {
			t.Fatalf("delete: code=%d body=%s; want 500", code, body)
		}
		if after, ok := readRule(t, id); !ok || after != before {
			t.Errorf("rule %+v (present %v); want it back as %+v", after, ok, before)
		}
		if !rf.has8080(rf.bound) {
			t.Error("the port after the undo lacks tcp/8080; want it converged with the rule back")
		}
	})
}
