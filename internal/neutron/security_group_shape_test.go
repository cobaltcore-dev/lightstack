package neutron

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// This file covers the security-group response shapes Horizon depends on, across all
// four endpoints that emit them: the group list and group-by-id (which nest their
// rules), and the flat security-group-rules list and rule-by-id.
//
// SPEC-020 (cloud-storage#533) — two consecutive defects, FR-022 then FR-023. Fixing
// the first exposed the second, because Horizon abandons a request at the first failure:
//
//   - **FR-022, the missing array.** Horizon reads these endpoints through openstacksdk,
//     whose Resource.to_dict() emits every declared attribute, so an omitted
//     security_group_rules arrives as null rather than as a missing key.
//     SecurityGroup.__init__ only guards the missing-key case
//     ("if 'security_group_rules' not in sg"), so the null went into a comprehension and
//     the request died with "'NoneType' object is not iterable" — a 500 on
//     /api/network/securitygroups/ that emptied the launch wizard's security-group step.
//   - **FR-023, the missing keys.** With the array present, Horizon then read each rule's
//     fields as sgr['protocol'] and friends, unconditionally. Lightstack emitted those keys only
//     when non-NULL, so the seeded default group's egress rules — NULL protocol — produced
//     HTTP 500 "'protocol'" and the "Unable to retrieve security groups" toast.
//
// Hence the invariant the whole file asserts: **a null value is fine, an absent key is
// not**, on every emitter.

// requiredRuleKeys are the keys Horizon's SecurityGroupRule.__init__ dereferences with
// sgr['...'] — so every rule object Lightstack emits must carry all of them, null-valued when
// the column is NULL. Shared by the tests covering all three emitters
// (securityGroupRulesByGroup, ListSecurityGroupRules, GetSecurityGroupRule) so the
// three cannot drift apart again.
var requiredRuleKeys = []string{
	"id", "security_group_id", "direction", "ethertype",
	"protocol", "port_range_min", "port_range_max",
	"remote_ip_prefix", "remote_group_id",
}

// assertRuleKeys checks one rule object against requiredRuleKeys. A null *value* is
// fine; an absent *key* is a KeyError in the client.
func assertRuleKeys(t *testing.T, where string, rule map[string]any) {
	t.Helper()
	for _, key := range requiredRuleKeys {
		if _, present := rule[key]; !present {
			t.Errorf("%s: rule %v is missing key %q (Horizon dereferences it — KeyError)",
				where, rule["id"], key)
		}
	}
}

func securityGroupsFromBody(t *testing.T, body string) []map[string]any {
	t.Helper()
	var env struct {
		SecurityGroups []map[string]any `json:"security_groups"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse security_groups: %v; body=%s", err, body)
	}
	if env.SecurityGroups == nil {
		t.Fatalf("security_groups key missing or null; body=%s", body)
	}
	return env.SecurityGroups
}

// TestListSecurityGroupsCarriesRulesArray: every group in the list MUST carry a
// security_group_rules array — present and non-null, even when empty.
func TestListSecurityGroupsCarriesRulesArray(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	code, body := doReq(r, http.MethodGet, "/v2.0/security-groups", neutronAuthHdr(seededProjectID, true))
	if code != http.StatusOK {
		t.Fatalf("list security groups: code=%d body=%s; want 200", code, body)
	}

	groups := securityGroupsFromBody(t, body)
	if len(groups) == 0 {
		t.Fatal("no security groups returned; migration 002 seeds a default group for the seeded project")
	}

	for _, sg := range groups {
		name, _ := sg["name"].(string)

		raw, ok := sg["security_group_rules"]
		if !ok {
			t.Errorf("security group %q: security_group_rules key absent", name)
			continue
		}
		if raw == nil {
			t.Errorf("security group %q: security_group_rules is null (Horizon iterates it)", name)
			continue
		}
		if _, ok := raw.([]any); !ok {
			t.Errorf("security group %q: security_group_rules is %T; want an array", name, raw)
		}

		// Upstream Neutron reports the owning project under both spellings.
		for _, key := range []string{"project_id", "tenant_id"} {
			if got, _ := sg[key].(string); got != seededProjectID {
				t.Errorf("security group %q: %s = %v; want %q", name, key, sg[key], seededProjectID)
			}
		}
	}
}

// TestEmptySecurityGroupRulesArrayIsNotNull covers the nil→[] substitution in both
// emitters, which nothing else reaches: the seeded default group always has rules, so
// every other test in this file sees a populated array and the empty case never runs.
//
// It is the empty case that FR-022 is actually about. Go marshals a nil []gin.H as
// `null`, not `[]`, so a group with no rules reproduces the original failure exactly —
// openstacksdk hands Horizon a null, SecurityGroup.__init__ only guards the
// missing-key case, and the null reaches a comprehension as "'NoneType' object is not
// iterable". A freshly created group has zero rules: CreateSecurityGroup inserts into
// security_groups only.
func TestEmptySecurityGroupRulesArrayIsNotNull(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)

	code, body := doReqBody(r, http.MethodPost, "/v2.0/security-groups",
		`{"security_group":{"name":"spec020-empty-sg","description":"no rules"}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create security group: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		SecurityGroup struct {
			ID string `json:"id"`
		} `json:"security_group"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("parse created group: %v; body=%s", err, body)
	}
	sgID := created.SecurityGroup.ID
	if sgID == "" {
		t.Fatalf("created group has no id; body=%s", body)
	}
	t.Cleanup(func() {
		doReq(r, http.MethodDelete, "/v2.0/security-groups/"+sgID, hdr) //nolint:errcheck // best-effort fixture cleanup
	})

	// Emitter 1: the group list.
	code, body = doReq(r, http.MethodGet, "/v2.0/security-groups", hdr)
	if code != http.StatusOK {
		t.Fatalf("list security groups: code=%d body=%s; want 200", code, body)
	}
	var found bool
	for _, sg := range securityGroupsFromBody(t, body) {
		if id, _ := sg["id"].(string); id != sgID {
			continue
		}
		found = true
		assertEmptyRulesArray(t, "security-group list", sg)
	}
	if !found {
		t.Errorf("the created group %s is absent from the list; body=%s", sgID, body)
	}

	// Emitter 2: group-by-id, which substitutes separately.
	code, body = doReq(r, http.MethodGet, "/v2.0/security-groups/"+sgID, hdr)
	if code != http.StatusOK {
		t.Fatalf("get security group: code=%d body=%s; want 200", code, body)
	}
	var one struct {
		SecurityGroup map[string]any `json:"security_group"`
	}
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("parse security_group: %v; body=%s", err, body)
	}
	assertEmptyRulesArray(t, "security-group by id", one.SecurityGroup)
}

// assertEmptyRulesArray requires security_group_rules to be an empty array — present,
// non-null, and length zero. `null` is the regression; a missing key is worse.
func assertEmptyRulesArray(t *testing.T, where string, sg map[string]any) {
	t.Helper()
	raw, present := sg["security_group_rules"]
	if !present {
		t.Errorf("%s: security_group_rules key absent on a group with no rules", where)
		return
	}
	if raw == nil {
		t.Errorf("%s: security_group_rules is null on a group with no rules; want [] "+
			"(a nil slice marshals to null, which Horizon iterates → 'NoneType' object is not iterable)", where)
		return
	}
	rules, ok := raw.([]any)
	if !ok {
		t.Errorf("%s: security_group_rules is %T; want an array", where, raw)
		return
	}
	if len(rules) != 0 {
		t.Errorf("%s: security_group_rules has %d entries; the group was created with none", where, len(rules))
	}
}

// TestSecurityGroupRuleCarriesEveryKeyHorizonDereferences asserts FR-023 on the rules
// nested inside the group list. The seeded default group's egress rules have a NULL
// protocol, so a stock project is enough to trip it.
func TestSecurityGroupRuleCarriesEveryKeyHorizonDereferences(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	code, body := doReq(r, http.MethodGet, "/v2.0/security-groups", neutronAuthHdr(seededProjectID, true))
	if code != http.StatusOK {
		t.Fatalf("list security groups: code=%d body=%s; want 200", code, body)
	}

	seenRules := 0
	for _, sg := range securityGroupsFromBody(t, body) {
		rules, _ := sg["security_group_rules"].([]any)
		for i, raw := range rules {
			rule, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("rule %d is %T; want an object", i, raw)
				continue
			}
			seenRules++
			assertRuleKeys(t, "nested in security-group list", rule)
		}
	}
	if seenRules == 0 {
		t.Fatal("no rules seen; migration 002 seeds rules on the default security group")
	}
}

// TestRuleValuesAgreeAcrossAllThreeEmitters is the FR-023 parity guard that the
// list-vs-get test structurally cannot be: those two share securityGroupRulesByGroup, so
// they agree by construction. This compares the nested emitter against the two flat ones,
// which are separate code, for both a NULL and a non-NULL value of every nullable column.
//
// The drift it catches: securityGroupRulesByGroup used to substitute "" for a NULL
// remote_ip_prefix / remote_group_id while the flat emitters passed the pointer through as
// null. Upstream reports null ("remote_ip_prefix": null in the api-ref), and FR-023
// requires the same shape from all three — a client that reads a rule from one endpoint
// and compares it to the same rule from another must not see a difference.
//
// A stock database only supplies the NULL case (all three seeded rules have NULL
// remote_ip_prefix and remote_group_id), so the non-NULL case is created here.
// TestRuleValuesAgreeAcrossAllFourEmitters — SPEC-024 (cloud-storage#546) B9.
//
// #533 aligned three emitters and the create response was never in scope, which is what the old name
// of this test recorded. A rule created with the optional fields unset came back with "" and 0 from
// the create and null from every subsequent read: the same rule described two ways, which breaks any
// client that round-trips or diffs it.
func TestRuleValuesAgreeAcrossAllFourEmitters(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)

	// A rule with every nullable column populated, so "" and null cannot be confused.
	sgID := seededDefaultGroupID(t, r, hdr)

	// Count first rather than hardcode. The seeded rule count is a migration detail that
	// has already changed once — 002 seeded three until 070 dropped the world-open SSH
	// ingress (cloud-storage#528) and rewrote 002 to stop seeding it, leaving two egress
	// rules on a fresh database. An assertion pinned to a literal passes only against a
	// database carrying leftovers from an earlier run, which is how it came to be written.
	before := len(rulesFromGroupList(t, r, hdr))

	code, body := doReqBody(r, http.MethodPost, "/v2.0/security-group-rules",
		`{"security_group_rule":{"security_group_id":"`+sgID+`","direction":"ingress",`+
			`"ethertype":"IPv4","protocol":"tcp","port_range_min":22,"port_range_max":22,`+
			`"remote_ip_prefix":"10.0.0.0/8"}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create rule: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		Rule map[string]any `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("parse created rule: %v; body=%s", err, body)
	}
	populated, _ := created.Rule["id"].(string)
	if populated == "" {
		t.Fatalf("created rule has no id; body=%s", body)
	}
	t.Cleanup(func() {
		doReq(r, http.MethodDelete, "/v2.0/security-group-rules/"+populated, hdr) //nolint:errcheck // best-effort fixture cleanup
	})

	// Index every rule as each emitter renders it.
	nested := rulesFromGroupList(t, r, hdr)
	flatList := rulesFromRuleList(t, r, hdr)

	if _, ok := nested[populated]; !ok {
		t.Fatalf("the created rule %s is absent from the group list", populated)
	}
	if len(nested) != before+1 {
		t.Fatalf("the group list held %d rules before the create and %d after; want exactly one more — "+
			"a different count means the listing gained or lost rules the create did not ask for",
			before, len(nested))
	}

	for ruleID, fromNested := range nested {
		fromFlat, ok := flatList[ruleID]
		if !ok {
			t.Errorf("rule %s is in the group list but not in the rule list", ruleID)
			continue
		}

		code, body := doReq(r, http.MethodGet, "/v2.0/security-group-rules/"+ruleID, hdr)
		if code != http.StatusOK {
			t.Errorf("get rule %s: code=%d body=%s; want 200", ruleID, code, body)
			continue
		}
		var one struct {
			Rule map[string]any `json:"security_group_rule"`
		}
		if err := json.Unmarshal([]byte(body), &one); err != nil {
			t.Errorf("parse rule %s: %v; body=%s", ruleID, err, body)
			continue
		}

		// Compare marshalled values so null and "" are distinguishable — the whole point.
		for _, key := range requiredRuleKeys {
			n, _ := json.Marshal(fromNested[key])
			f, _ := json.Marshal(fromFlat[key])
			g, _ := json.Marshal(one.Rule[key])
			if string(n) != string(f) || string(n) != string(g) {
				t.Errorf("rule %s key %q disagrees across emitters: group-list=%s rule-list=%s rule-by-id=%s",
					ruleID, key, n, f, g)
			}

			// The fourth emitter: the create response, for the rule this test created.
			if ruleID == populated {
				cr, _ := json.Marshal(created.Rule[key])
				if string(cr) != string(g) {
					t.Errorf("rule %s key %q: create response=%s but rule-by-id=%s — B9, the create path is the fourth emitter",
						ruleID, key, cr, g)
				}
			}
		}
	}
}

// seededDefaultGroupID returns the id of the seeded project's `default` group, read
// through the API so the test does not depend on the seed's uuid.
func seededDefaultGroupID(t *testing.T, r *gin.Engine, hdr map[string]string) string {
	t.Helper()
	code, body := doReq(r, http.MethodGet, "/v2.0/security-groups", hdr)
	if code != http.StatusOK {
		t.Fatalf("list security groups: code=%d body=%s; want 200", code, body)
	}
	for _, sg := range securityGroupsFromBody(t, body) {
		if name, _ := sg["name"].(string); name == "default" {
			if id, _ := sg["id"].(string); id != "" {
				return id
			}
		}
	}
	t.Fatalf("no `default` security group for the seeded project; body=%s", body)
	return ""
}

// rulesFromGroupList indexes every rule the group list nests, by rule id.
func rulesFromGroupList(t *testing.T, r *gin.Engine, hdr map[string]string) map[string]map[string]any {
	t.Helper()
	code, body := doReq(r, http.MethodGet, "/v2.0/security-groups", hdr)
	if code != http.StatusOK {
		t.Fatalf("list security groups: code=%d body=%s; want 200", code, body)
	}
	out := map[string]map[string]any{}
	for _, sg := range securityGroupsFromBody(t, body) {
		rules, _ := sg["security_group_rules"].([]any)
		for _, raw := range rules {
			if rule, ok := raw.(map[string]any); ok {
				id, _ := rule["id"].(string)
				out[id] = rule
			}
		}
	}
	return out
}

// rulesFromRuleList indexes the flat security-group-rules list, by rule id.
func rulesFromRuleList(t *testing.T, r *gin.Engine, hdr map[string]string) map[string]map[string]any {
	t.Helper()
	code, body := doReq(r, http.MethodGet, "/v2.0/security-group-rules", hdr)
	if code != http.StatusOK {
		t.Fatalf("list security group rules: code=%d body=%s; want 200", code, body)
	}
	var env struct {
		Rules []map[string]any `json:"security_group_rules"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse security_group_rules: %v; body=%s", err, body)
	}
	out := map[string]map[string]any{}
	for _, rule := range env.Rules {
		id, _ := rule["id"].(string)
		out[id] = rule
	}
	return out
}

// TestSecurityGroupRuleShapeMatchesBetweenListAndGet: the list and the by-id view read
// the same rules through one helper, so their rule objects must be identical. Before
// this fix the list emitted no rules at all, which is exactly the drift this catches.
func TestSecurityGroupRuleShapeMatchesBetweenListAndGet(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)

	_, listBody := doReq(r, http.MethodGet, "/v2.0/security-groups", hdr)
	groups := securityGroupsFromBody(t, listBody)
	if len(groups) == 0 {
		t.Fatal("no security groups returned")
	}

	for _, sg := range groups {
		id, _ := sg["id"].(string)
		code, getBody := doReq(r, http.MethodGet, "/v2.0/security-groups/"+id, hdr)
		if code != http.StatusOK {
			t.Fatalf("get security group %s: code=%d body=%s; want 200", id, code, getBody)
		}

		var env struct {
			SecurityGroup map[string]any `json:"security_group"`
		}
		if err := json.Unmarshal([]byte(getBody), &env); err != nil {
			t.Fatalf("parse security_group: %v; body=%s", err, getBody)
		}

		fromList, _ := json.Marshal(sg["security_group_rules"])
		fromGet, _ := json.Marshal(env.SecurityGroup["security_group_rules"])
		if string(fromList) != string(fromGet) {
			t.Errorf("security group %s: rules differ between list and get:\n  list=%s\n  get =%s",
				id, fromList, fromGet)
		}
	}
}

// TestSecurityGroupRuleEndpointsCarryEveryKey asserts FR-023 on the two rule endpoints
// the CLI uses — GET /v2.0/security-group-rules and /{id} (network.go:134-135). They
// emit the same rule object as the nested view and carried the identical defect, but
// had no coverage at all: the fix was originally verified only on the nested emitter.
func TestSecurityGroupRuleEndpointsCarryEveryKey(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)

	// --- flat list ---
	code, body := doReq(r, http.MethodGet, "/v2.0/security-group-rules", hdr)
	if code != http.StatusOK {
		t.Fatalf("list security-group-rules: code=%d body=%s; want 200", code, body)
	}

	var listEnv struct {
		Rules []map[string]any `json:"security_group_rules"`
	}
	if err := json.Unmarshal([]byte(body), &listEnv); err != nil {
		t.Fatalf("parse security_group_rules: %v; body=%s", err, body)
	}
	if len(listEnv.Rules) == 0 {
		t.Fatal("no rules returned; migration 002 seeds rules on the default security group")
	}
	for _, rule := range listEnv.Rules {
		assertRuleKeys(t, "GET /v2.0/security-group-rules", rule)
	}

	// --- by id ---
	ruleID, _ := listEnv.Rules[0]["id"].(string)
	if ruleID == "" {
		t.Fatal("first rule has no id")
	}
	code, body = doReq(r, http.MethodGet, "/v2.0/security-group-rules/"+ruleID, hdr)
	if code != http.StatusOK {
		t.Fatalf("get security-group-rule %s: code=%d body=%s; want 200", ruleID, code, body)
	}

	var getEnv struct {
		Rule map[string]any `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &getEnv); err != nil {
		t.Fatalf("parse security_group_rule: %v; body=%s", err, body)
	}
	if getEnv.Rule == nil {
		t.Fatalf("security_group_rule key missing or null; body=%s", body)
	}
	assertRuleKeys(t, "GET /v2.0/security-group-rules/{id}", getEnv.Rule)
}

// TestSecurityGroupRuleUnknownIDIs404: the by-id endpoint must not 500 on an unknown
// rule. Guards the error path alongside the happy path above.
func TestSecurityGroupRuleUnknownIDIs404(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	code, body := doReq(r, http.MethodGet,
		"/v2.0/security-group-rules/11111111-1111-1111-1111-111111111111",
		neutronAuthHdr(seededProjectID, true))
	if code != http.StatusNotFound {
		t.Errorf("GET unknown security-group-rule: code=%d body=%s; want 404", code, body)
	}
}

// TestUnsetRuleOptionalsAreNullInTheCreateResponse — contracts G1 and G2.
//
// The narrow case B9 is actually about: nothing optional supplied. The create response reported ""
// for the remote fields and 0 for the port bounds, where every read reports null. 0 is not "unset" —
// it describes a different rule.
func TestUnsetRuleOptionalsAreNullInTheCreateResponse(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)
	sgID := seededDefaultGroupID(t, r, hdr)

	code, body := doReqBody(r, http.MethodPost, "/v2.0/security-group-rules",
		`{"security_group_rule":{"security_group_id":"`+sgID+`","direction":"ingress","ethertype":"IPv4"}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create rule: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		Rule map[string]any `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("parse created rule: %v; body=%s", err, body)
	}
	ruleID, _ := created.Rule["id"].(string)
	t.Cleanup(func() {
		doReq(r, http.MethodDelete, "/v2.0/security-group-rules/"+ruleID, hdr) //nolint:errcheck // best-effort fixture cleanup
	})

	for _, key := range []string{"remote_ip_prefix", "remote_group_id", "port_range_min", "port_range_max", "protocol"} {
		got, present := created.Rule[key]
		if !present {
			t.Errorf("key %q absent from the create response; clients index it directly", key)
			continue
		}
		if got != nil {
			t.Errorf("create response %q = %#v, want null for an unset optional — this is B9", key, got)
		}
	}
}

// TestDeliberateZeroPortSurvivesStorage — contract G6 / FR-028.
//
// Port 0 is a legitimate ICMP type, so it must round-trip as 0 rather than becoming null. This is the
// half spec A6 left open: the columns are nullable and can hold it, but the write path could not
// express it — `Valid: portMin > 0` stored a real 0 as NULL. Reporting it correctly while storing NULL
// would still be wrong, so this asserts the value after a re-read, not just the echo.
func TestDeliberateZeroPortSurvivesStorage(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)
	sgID := seededDefaultGroupID(t, r, hdr)

	code, body := doReqBody(r, http.MethodPost, "/v2.0/security-group-rules",
		`{"security_group_rule":{"security_group_id":"`+sgID+`","direction":"ingress","ethertype":"IPv4",`+
			`"protocol":"icmp","port_range_min":0,"port_range_max":0}}`, hdr)
	if code != http.StatusCreated {
		t.Fatalf("create rule: code=%d body=%s; want 201", code, body)
	}
	var created struct {
		Rule map[string]any `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("parse created rule: %v; body=%s", err, body)
	}
	ruleID, _ := created.Rule["id"].(string)
	t.Cleanup(func() {
		doReq(r, http.MethodDelete, "/v2.0/security-group-rules/"+ruleID, hdr) //nolint:errcheck // best-effort fixture cleanup
	})

	for _, key := range []string{"port_range_min", "port_range_max"} {
		got := created.Rule[key]
		if got == nil {
			t.Errorf("create response %q = null, want 0 — a deliberate zero is not 'unset'", key)
			continue
		}
		if n, ok := got.(float64); !ok || n != 0 {
			t.Errorf("create response %q = %#v, want 0", key, got)
		}
	}

	// Re-read: proves the zero reached storage rather than only the response.
	code, body = doReq(r, http.MethodGet, "/v2.0/security-group-rules/"+ruleID, hdr)
	if code != http.StatusOK {
		t.Fatalf("get rule: code=%d body=%s; want 200", code, body)
	}
	var one struct {
		Rule map[string]any `json:"security_group_rule"`
	}
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("parse rule: %v; body=%s", err, body)
	}
	for _, key := range []string{"port_range_min", "port_range_max"} {
		if one.Rule[key] == nil {
			t.Errorf("stored %q came back null, want 0 — `Valid: portMin > 0` discarded a real zero", key)
		}
	}
}
