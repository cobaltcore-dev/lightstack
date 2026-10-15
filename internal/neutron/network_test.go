package neutron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
)

func TestGenerateMAC(t *testing.T) {
	t.Run("format validation", func(t *testing.T) {
		mac := generateMAC()
		pattern := regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
		if !pattern.MatchString(mac) {
			t.Errorf("MAC address %q does not match expected format XX:XX:XX:XX:XX:XX", mac)
		}
	})

	t.Run("local bit set and multicast cleared", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			mac := generateMAC()
			first, err := strconv.ParseUint(mac[0:2], 16, 8)
			if err != nil {
				t.Fatalf("failed to parse first octet of MAC %q: %v", mac, err)
			}
			if first&0x02 == 0 {
				t.Errorf("MAC %q: local bit not set in first octet 0x%02x", mac, first)
			}
			if first&0x01 != 0 {
				t.Errorf("MAC %q: multicast bit set in first octet 0x%02x", mac, first)
			}
		}
	})

	t.Run("uniqueness over 100 iterations", func(t *testing.T) {
		seen := make(map[string]struct{}, 100)
		for i := 0; i < 100; i++ {
			mac := generateMAC()
			if _, exists := seen[mac]; exists {
				t.Errorf("duplicate MAC address generated: %q", mac)
			}
			seen[mac] = struct{}{}
		}
	})
}

// TestNeutronBool: the list-filter boolean parser accepts neutron's boolean spellings
// and reports absence distinctly from false (F4). Pure — no DB.
func TestNeutronBool(t *testing.T) {
	cases := []struct {
		in           string
		val, present bool
	}{
		{"", false, false},
		{"true", true, true}, {"True", true, true}, {"1", true, true},
		{"false", false, true}, {"False", false, true}, {"0", false, true},
		{"bogus", false, false},
	}
	for _, tc := range cases {
		v, p := neutronBool(tc.in)
		if v != tc.val || p != tc.present {
			t.Errorf("neutronBool(%q) = (%v,%v), want (%v,%v)", tc.in, v, p, tc.val, tc.present)
		}
	}
}

// TestNetworkListDecision: ListNetworks must honor the filters Horizon's
// network_list_for_tenant sends so its sub-queries are disjoint and each network
// renders once (F4, FR-009/010/012). Pure — no DB.
func TestNetworkListDecision(t *testing.T) {
	// SPEC-024 F11 changed this assertion, deliberately: router:external=true used to force an EMPTY
	// result because Lightstack modelled no external networks, which is why Horizon's Set Gateway dropdown
	// was always empty. It is now a real predicate applied by buildNetworkListFilter, so the decision
	// function must NOT short-circuit (FR-032).
	if sf, empty := networkListDecision("", "true"); empty || sf != sharedAny {
		t.Errorf("router:external=true → (%v, empty=%v), want (sharedAny, false) — it is a predicate now, not an empty result", sf, empty)
	}
	// shared=true → only shared networks.
	if sf, empty := networkListDecision("true", ""); sf != sharedOnly || empty {
		t.Errorf("shared=true → (%v, empty=%v), want (sharedOnly, false)", sf, empty)
	}
	// shared=false → exclude shared networks.
	if sf, empty := networkListDecision("false", ""); sf != nonSharedOnly || empty {
		t.Errorf("shared=false → (%v, empty=%v), want (nonSharedOnly, false)", sf, empty)
	}
	// no shared filter → caller's own OR shared (unchanged default).
	if sf, empty := networkListDecision("", ""); sf != sharedAny || empty {
		t.Errorf("default → (%v, empty=%v), want (sharedAny, false)", sf, empty)
	}
	// router:external=false with no shared filter → default, not empty.
	if sf, empty := networkListDecision("", "false"); sf != sharedAny || empty {
		t.Errorf("router:external=false → (%v, empty=%v), want (sharedAny, false)", sf, empty)
	}
}

// TestBuildNetworkListFilter: the extracted WHERE/args builder emits the right SQL and
// positional args per filter so Horizon's sub-queries stay disjoint (F4). Pure — no DB.
func TestBuildNetworkListFilter(t *testing.T) {
	const caller = "proj-A"

	// SPEC-024 F11: router:external is a real predicate now, complementary rather than always-empty
	// (FR-032, contract X6). The old assertion — empty=true — encoded the defect.
	f, empty := buildNetworkListFilter(caller, "", "true", "", "", "", false)
	if empty {
		t.Fatal("router:external=true must no longer force an empty result")
	}
	if where := f.Conds(); where != "(project_id = $1 OR shared = true OR external = true) AND external = $2" {
		t.Errorf("router:external=true where = %q", where)
	}
	if args := f.Args(); len(args) != 2 || args[1] != true {
		t.Errorf("router:external=true args = %v, want [caller true]", args)
	}

	// ...and its complement.
	f, _ = buildNetworkListFilter(caller, "", "false", "", "", "", false)
	if args := f.Args(); len(args) != 2 || args[1] != false {
		t.Errorf("router:external=false args = %v, want [caller false]", args)
	}

	// Default (no filters): scope = caller's own OR shared OR external, one arg (the caller).
	// `external` is a THIRD INDEPENDENT disjunct — it must not require `shared`, or a tenant cannot
	// see the external network they need to pick as a router gateway (FR-033a).
	f, empty = buildNetworkListFilter(caller, "", "", "", "", "", false)
	if empty {
		t.Fatal("default filter should not be empty")
	}
	if where := f.Conds(); where != "(project_id = $1 OR shared = true OR external = true)" {
		t.Errorf("default where = %q", where)
	}
	if args := f.Args(); len(args) != 1 || args[0] != caller {
		t.Errorf("default args = %v, want [%q]", args, caller)
	}

	// shared=true → only shared, and NO caller arg (so pagination starts at $1).
	f, _ = buildNetworkListFilter(caller, "true", "", "", "", "", false)
	if where := f.Conds(); where != "shared = true" {
		t.Errorf("shared=true where = %q", where)
	}
	if args := f.Args(); len(args) != 0 {
		t.Errorf("shared=true args = %v, want none", args)
	}

	// shared=false → exclude shared, scoped to the caller.
	f, _ = buildNetworkListFilter(caller, "false", "", "", "", "", false)
	if where := f.Conds(); !strings.Contains(where, "shared = false") {
		t.Errorf("shared=false where = %q", where)
	}

	// tenant + name append sequential placeholders after the scope arg ($1).
	f, _ = buildNetworkListFilter(caller, "", "", "proj-A", "", "netx", false)
	where := f.Conds()
	if !strings.Contains(where, "project_id = $2") || !strings.Contains(where, "name = $3") {
		t.Errorf("tenant+name where = %q, want $2/$3 placeholders", where)
	}
	if args := f.Args(); len(args) != 3 {
		t.Errorf("tenant+name args = %v, want 3", args)
	}

	// Admin + tenant: the tenant filter is the authoritative owner scope — the
	// caller's read scope must NOT be ANDed in, or another project's private
	// networks can never appear and `network list --project <id>` answers []
	// (SPEC-028). Upstream honours the filter for admin tokens.
	f, _ = buildNetworkListFilter(caller, "", "", "proj-B", "", "", true)
	where = f.Conds()
	if where != "project_id = $1" {
		t.Errorf("admin+tenant where = %q, want exactly \"project_id = $1\"", where)
	}
	// Non-admin + tenant keeps the scope intersection (unchanged semantics).
	f, _ = buildNetworkListFilter(caller, "", "", "proj-B", "", "", false)
	where = f.Conds()
	if !strings.Contains(where, "project_id = $1 OR shared = true OR external = true") || !strings.Contains(where, "project_id = $2") {
		t.Errorf("non-admin+tenant where = %q, want scope AND tenant", where)
	}
	// Admin without a tenant filter keeps the ordinary scope.
	f, _ = buildNetworkListFilter(caller, "", "", "", "", "", true)
	if where := f.Conds(); where != "(project_id = $1 OR shared = true OR external = true)" {
		t.Errorf("admin default where = %q, want the ordinary read scope", where)
	}
}

// TestBuildSubnetListFilter — SPEC-024 (cloud-storage#546) B6, FR-019/FR-020.
//
// The subnet listing applied no client filter at all. This mirrors TestBuildNetworkListFilter above:
// a pure function, placeholders numbered from $1, testable without a database — which is what makes
// the seven contracted filters cheap to pin.
func TestBuildSubnetListFilter(t *testing.T) {
	tests := []struct {
		name      string
		params    subnetFilterParams
		wantConds []string
		wantArgs  []any
	}{
		{
			name:      "no filters",
			params:    subnetFilterParams{},
			wantConds: nil,
			wantArgs:  nil,
		},
		{
			name:      "network only",
			params:    subnetFilterParams{NetworkID: "net-1"},
			wantConds: []string{"s.network_id::text = $1"},
			wantArgs:  []any{"net-1"},
		},
		{
			name:      "network and name number sequentially",
			params:    subnetFilterParams{NetworkID: "net-1", Name: "sub-a"},
			wantConds: []string{"s.network_id::text = $1", "s.name = $2"},
			wantArgs:  []any{"net-1", "sub-a"},
		},
		{
			name:      "explicit enable_dhcp false is a filter, not an absence",
			params:    subnetFilterParams{EnableDHCP: "false"},
			wantConds: []string{"s.enable_dhcp = $1"},
			wantArgs:  []any{false},
		},
		{
			name:      "unparseable ip_version is ignored rather than narrowing the result",
			params:    subnetFilterParams{IPVersion: "not-a-number"},
			wantConds: nil,
			wantArgs:  nil,
		},
		{
			name:      "project spelling maps onto project_id",
			params:    subnetFilterParams{ProjectID: "proj-1"},
			wantConds: []string{"s.project_id::text = $1"},
			wantArgs:  []any{"proj-1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &common.SQLFilter{}
			addSubnetListFilter(f, tc.params)

			// Conds() joins with " AND "; the cases express the predicates individually because what
			// matters is each one's text AND its placeholder number.
			want := strings.Join(tc.wantConds, " AND ")
			if got := f.Conds(); got != want {
				t.Errorf("conds = %q, want %q", got, want)
			}
			args := f.Args()
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("args = %v, want %v", args, tc.wantArgs)
			}
			for i := range args {
				if args[i] != tc.wantArgs[i] {
					t.Errorf("args[%d] = %#v, want %#v", i, args[i], tc.wantArgs[i])
				}
			}
		})
	}
}

// The subnet listing scopes by the owning network before adding client filters, so the contracted
// filters start at $2 in the real query rather than $1. The builder no longer takes a start index —
// it appends to a filter that already holds the scope — so this pins that the numbering follows the
// filter it is given rather than restarting.
func TestAddSubnetListFilterContinuesExistingNumbering(t *testing.T) {
	f := &common.SQLFilter{}
	caller := f.Arg("proj-A")
	f.Cond("(s.project_id = " + caller + " OR n.project_id = " + caller + " OR n.shared = true)")

	addSubnetListFilter(f, subnetFilterParams{NetworkID: "net-1", Name: "sub-a"})

	const want = "(s.project_id = $1 OR n.project_id = $1 OR n.shared = true) AND " +
		"s.network_id::text = $2 AND s.name = $3"
	if got := f.Conds(); got != want {
		t.Errorf("conds = %q, want %q", got, want)
	}
	// Three placeholders, but only three values: $1 appears twice and is appended once.
	if args := f.Args(); len(args) != 3 || args[0] != "proj-A" || args[1] != "net-1" || args[2] != "sub-a" {
		t.Errorf("args = %v, want [proj-A net-1 sub-a]", args)
	}
}

// SPEC-025 T012 (FR-007a, FR-007b): the network version document must carry an
// ABSOLUTE self href.
//
// It emitted the bare path "/v2.0". A client that has just resolved the catalog and
// is reading version documents to pick an endpoint cannot resolve that against
// anything — it is the same discovery defect as the compute version document (B4),
// one service over, and it is why FR-007b says "every service's", not "compute's".
func TestNeutronVersionDocumentSelfLinkIsAbsolute(t *testing.T) {
	gin.SetMode(gin.TestMode)

	svc := &Service{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v2.0", nil)
	c.Request.Host = "lightstack.example:9696"

	svc.GetVersion(c)

	var doc struct {
		Version struct {
			Links []struct {
				Rel  string `json:"rel"`
				HRef string `json:"href"`
			} `json:"links"`
		} `json:"version"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding version document: %v (body: %s)", err, w.Body.String())
	}
	if len(doc.Version.Links) == 0 {
		t.Fatal("version document has no links")
	}

	for _, l := range doc.Version.Links {
		u, err := url.Parse(l.HRef)
		if err != nil {
			t.Fatalf("href %q does not parse: %v", l.HRef, err)
		}
		if u.Scheme == "" || u.Host == "" {
			t.Errorf("href %q is not absolute; a client cannot resolve it", l.HRef)
		}
		if u.Host != "lightstack.example:9696" {
			t.Errorf("href %q does not name the host the client addressed", l.HRef)
		}
	}
}
