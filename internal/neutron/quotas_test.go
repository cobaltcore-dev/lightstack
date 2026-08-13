package neutron

import (
	"encoding/json"
	"net/http"
	"testing"
)

// SPEC-020 (cloud-storage#533) — network quota metadata Horizon reads before
// rendering the Project-panel Create Network action, plus the capability list it
// gates on. The limit assertions need no database.

func quotaBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var env struct {
		Quota map[string]any `json:"quota"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse quota: %v; body=%s", err, body)
	}
	return env.Quota
}

func neutronAuthHdr(projectID string, admin bool) map[string]string {
	h := map[string]string{"X-Test-Project": projectID}
	if admin {
		h["X-Test-Roles"] = "admin"
	}
	return h
}

// TestGetQuota_CarriesProjectScope: upstream reports both spellings of the owning
// project. Before the fix the handler ignored the URL id entirely and returned a
// bare limits map with neither key.
func TestGetQuota_CarriesProjectScope(t *testing.T) {
	// Resolves an admin-supplied :id against the projects table, so it needs a DB.
	requireDB(t)
	r := newNeutronTestRouter()
	code, body := doReq(r, http.MethodGet, "/v2.0/quotas/"+seededProjectID, neutronAuthHdr(seededProjectID, true))
	if code != http.StatusOK {
		t.Fatalf("quota: code=%d body=%s; want 200", code, body)
	}
	q := quotaBody(t, body)
	for _, key := range []string{"project_id", "tenant_id"} {
		if got, _ := q[key].(string); got != seededProjectID {
			t.Errorf("quota.%s = %v; want %q", key, q[key], seededProjectID)
		}
	}
}

// TestQuotaDefaultEndpoint: /quotas/{id}/default returned 404 before the fix (FR-002).
func TestQuotaDefaultEndpoint(t *testing.T) {
	// Resolves an admin-supplied :id against the projects table, so it needs a DB.
	requireDB(t)
	r := newNeutronTestRouter()
	for _, path := range []string{
		"/v2.0/quotas/" + seededProjectID + "/default",
		"/v2.0/quotas/" + seededProjectID + "/default.json",
	} {
		code, body := doReq(r, http.MethodGet, path, neutronAuthHdr(seededProjectID, true))
		if code != http.StatusOK {
			t.Fatalf("GET %s: code=%d body=%s; want 200", path, code, body)
		}
		assertNeutronDefaultLimits(t, quotaBody(t, body))
	}
}

// TestQuotaDetailsJSONAlias: python-neutronclient appends .json to the quota
// sub-resources, which 404'd before the fix (FR-002).
func TestQuotaDetailsJSONAlias(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := neutronAuthHdr(seededProjectID, true)

	code, plain := doReq(r, http.MethodGet, "/v2.0/quotas/"+seededProjectID+"/details", hdr)
	if code != http.StatusOK {
		t.Fatalf("details: code=%d body=%s", code, plain)
	}
	code, suffixed := doReq(r, http.MethodGet, "/v2.0/quotas/"+seededProjectID+"/details.json", hdr)
	if code != http.StatusOK {
		t.Fatalf("details.json: code=%d body=%s; want 200", code, suffixed)
	}
	if plain != suffixed {
		t.Errorf("details and details.json must return the same body:\n  %s\n  %s", plain, suffixed)
	}
}

// TestQuotaDetails_Shape: each entry reports limit, used and reserved, and the
// limits match the shared defaults so the three quota views cannot drift (FR-003).
func TestQuotaDetails_Shape(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	code, body := doReq(r, http.MethodGet, "/v2.0/quotas/"+seededProjectID+"/details",
		neutronAuthHdr(seededProjectID, true))
	if code != http.StatusOK {
		t.Fatalf("details: code=%d body=%s", code, body)
	}
	q := quotaBody(t, body)
	for resource, wantLimit := range neutronQuotaDefaults {
		entry, ok := q[resource].(map[string]any)
		if !ok {
			t.Errorf("details %q = %#v; want an object", resource, q[resource])
			continue
		}
		for _, key := range []string{"limit", "used", "reserved"} {
			if _, present := entry[key]; !present {
				t.Errorf("details %q is missing %q", resource, key)
			}
		}
		if got := entry["limit"]; got != float64(wantLimit) {
			t.Errorf("details %q limit=%v; want the shared default %d", resource, got, wantLimit)
		}
	}
	assertNeutronDefaultLimits(t, flattenLimits(q))

	// `used` must be counted, not a constant. The seeded project always has a `default`
	// security group with rules (migration 002), so these two cannot legitimately be 0 —
	// which is what a presence-only assertion would have accepted.
	for resource, atLeast := range map[string]float64{
		"security_group":      1,
		"security_group_rule": 1,
	} {
		entry, ok := q[resource].(map[string]any)
		if !ok {
			continue // already reported above
		}
		got, isNum := entry["used"].(float64)
		if !isNum {
			t.Errorf("details %q used=%#v; want a number", resource, entry["used"])
			continue
		}
		if got < atLeast {
			t.Errorf("details %q used=%v; want >=%v (migration 002 seeds a default security "+
				"group with rules, so 0 means `used` is not reading the table)", resource, got, atLeast)
		}
	}
}

// flattenLimits reduces a details-shaped quota ({limit,used,reserved} per resource) to
// resource→limit, so the same literal pins cover both the default and the details views.
func flattenLimits(q map[string]any) map[string]any {
	flat := map[string]any{}
	for resource, raw := range q {
		if entry, ok := raw.(map[string]any); ok {
			flat[resource] = entry["limit"]
		}
	}
	return flat
}

// assertNeutronDefaultLimits pins the network default limits as literals. Comparing the
// response to neutronQuotaDefaults — as these tests did — passes for whatever the map
// happens to hold, so it cannot catch a default changing.
//
// network/subnet/port/router/floatingip match upstream Neutron. security_group and
// security_group_rule deliberately do not (upstream defaults are 10 and 100); pinned so
// that gap is visible and any change to it is deliberate.
func assertNeutronDefaultLimits(t *testing.T, q map[string]any) {
	t.Helper()
	for resource, want := range map[string]float64{
		"network":             100,
		"subnet":              100,
		"port":                500,
		"router":              10,
		"floatingip":          50,
		"security_group":      100,
		"security_group_rule": 500,
	} {
		got, ok := q[resource].(float64)
		if !ok {
			t.Errorf("quota %q = %#v; want a number", resource, q[resource])
			continue
		}
		if got != want {
			t.Errorf("quota %q = %v; want %v", resource, got, want)
		}
	}
}

// TestListExtensions_AdvertisesQuotaDetails: with this alias Horizon reads network
// usage from the single details endpoint instead of its multi-call fallback (FR-013).
func TestListExtensions_AdvertisesQuotaDetails(t *testing.T) {
	r := newNeutronTestRouter()
	code, body := doReq(r, http.MethodGet, "/v2.0/extensions", neutronAuthHdr(seededProjectID, true))
	if code != http.StatusOK {
		t.Fatalf("extensions: code=%d body=%s", code, body)
	}
	var env struct {
		Extensions []struct {
			Alias string `json:"alias"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse extensions: %v", err)
	}
	// The exact set FR-013 names, now that the trace has closed it. Asserting the whole
	// set rather than only the addition covers FR-013's second half too — "only aliases
	// O3K genuinely implements may be advertised" — which a presence check cannot.
	//
	// dhcp_agent_scheduler joins it for Horizon's admin Project column (SPEC-024 host pass,
	// finding 2). It is advertised on the strength of GET /networks/{id}/dhcp-agents, which
	// answers with the empty list a node running dnsmasq directly genuinely has — the closed-set
	// rule above is what forces the endpoint and the alias to land together, since the alias alone
	// turns a blank column into an error pop-up per network row.
	want := map[string]bool{
		"security-group": true, "router": true, "port-security": true,
		"binding": true, "provider": true, "quotas": true, "quota_details": true,
		"dhcp_agent_scheduler": true,
	}
	got := map[string]bool{}
	for _, e := range env.Extensions {
		got[e.Alias] = true
	}
	for alias := range want {
		if !got[alias] {
			t.Errorf("extensions do not advertise %q, which FR-013 requires; got %v", alias, mapKeysOf(got))
		}
	}
	for alias := range got {
		if !want[alias] {
			t.Errorf("extensions advertise %q, which FR-013's closed set does not include — "+
				"advertising a capability O3K does not implement makes Horizon render a control that fails", alias)
		}
	}
}

func mapKeysOf(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestNeutronQuotaReadsAreProjectScoped (FR-002a): the package had no role check at
// all before the fix.
func TestNeutronQuotaReadsAreProjectScoped(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	// A REAL second project — a phantom id must 404 now (SC-007a), which is
	// TestNeutronQuotaTargetMustExist's job, not this test's.
	otherProject := createTestProject(t, "scoping-other")

	code, body := doReq(r, http.MethodGet, "/v2.0/quotas/"+otherProject,
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("non-admin quota read: code=%d body=%s", code, body)
	}
	if got, _ := quotaBody(t, body)["project_id"].(string); got != seededProjectID {
		t.Errorf("non-admin got quota for %q; want its own %q (FR-002a)", got, seededProjectID)
	}

	code, body = doReq(r, http.MethodGet, "/v2.0/quotas/"+otherProject,
		neutronAuthHdr(seededProjectID, true))
	if code != http.StatusOK {
		t.Fatalf("admin quota read: code=%d body=%s", code, body)
	}
	if got, _ := quotaBody(t, body)["project_id"].(string); got != otherProject {
		t.Errorf("admin got quota for %q; want the requested %q", got, otherProject)
	}
}
