package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-024 (cloud-storage#546) US3 / B11 — a network's owning project is honest.
//
// Two halves, and BOTH are needed for the ticket's own probe to pass:
//
//  1. `CreateNetworkRequest.Network` declared no project attribute of either spelling, so the value
//     was discarded by ShouldBindJSON before any handler logic ran. `openstack network create
//     --project other-tenant` returned 201 with the network in the ADMINISTRATOR's project. Nothing
//     was ignored — the field never existed.
//  2. All the emitters reported `c.GetString("project_id")` — the CALLER — and the list and show
//     queries did not even select project_id. Since both can legitimately return a shared network
//     owned by someone else, a tenant was told a foreign resource was theirs.
//
// Upstream's rule, read from neutron_lib/api/attributes.py rather than the API reference (which does
// not document it): the two spellings normalise into each other and disagreeing values are a 400;
// a project differing from the authenticated one is a 400 unless context.can_set_project_id, which is
// `is_admin or is_service_role or …`. Lightstack has no service-account concept, so administrator is the
// whole privileged set. This deliberately differs from #549's compute decision — upstream Nova has no
// such attribute on server create to honour, upstream Neutron does (spec A10/A11).

const otherProjectID = "00000000-0000-0000-0000-0000000b0b0b"

// requireSecondProject guarantees the second project exists, creating it if needed. A missing
// fixture must be built, not skipped: skipping would let every cross-project test report green
// against a freshly migrated database while proving nothing.
func requireSecondProject(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO projects (id, name, domain_id, enabled, created_at, updated_at)
		VALUES ($1, 'spec024-other', (SELECT id FROM domains LIMIT 1), true, now(), now())
		ON CONFLICT (id) DO NOTHING
	`, otherProjectID); err != nil {
		t.Fatalf("seed second project: %v", err)
	}
}

// createNetwork posts a raw network body and returns status + decoded network object.
func createNetwork(t *testing.T, r http.Handler, hdr map[string]string, body string) (int, map[string]any) {
	t.Helper()
	code, raw := doReqBody(r, http.MethodPost, "/v2.0/networks", body, hdr)
	var resp struct {
		Network map[string]any `json:"network"`
	}
	if code == http.StatusCreated {
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			t.Fatalf("unmarshal network: %v (body=%s)", err, raw)
		}
		id, _ := resp.Network["id"].(string)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), "DELETE FROM networks WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
		})
	}
	if resp.Network == nil {
		resp.Network = map[string]any{"_raw": raw}
	}
	return code, resp.Network
}

// storedOwner reads project_id straight from storage. The response echoed the caller, so an
// echo-only assertion would pass on a broken fix — this is what makes N1 meaningful.
func storedOwner(t *testing.T, networkID string) string {
	t.Helper()
	var owner string
	if err := testPool.QueryRow(context.Background(),
		"SELECT project_id::text FROM networks WHERE id = $1", networkID).Scan(&owner); err != nil {
		t.Fatalf("read stored owner: %v", err)
	}
	return owner
}

// TestAdminCreatesNetworkIntoAnotherProject — contract N1.
func TestAdminCreatesNetworkIntoAnotherProject(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	code, network := createNetwork(t, r, admin,
		fmt.Sprintf(`{"network":{"name":"b11-admin-cross","project_id":%q}}`, otherProjectID))
	if code != http.StatusCreated {
		t.Fatalf("admin cross-project create: code=%d body=%v; want 201", code, network)
	}

	if got := network["project_id"]; got != otherProjectID {
		t.Errorf("response project_id = %#v, want %q", got, otherProjectID)
	}
	if got := network["tenant_id"]; got != otherProjectID {
		t.Errorf("response tenant_id = %#v, want %q", got, otherProjectID)
	}

	id, _ := network["id"].(string)
	if owner := storedOwner(t, id); owner != otherProjectID {
		t.Errorf("STORED project_id = %q, want %q — the request was accepted and discarded (B11)", owner, otherProjectID)
	}
}

// TestNonAdminCannotCreateIntoAnotherProject — contract N2. Refused, never silently redirected.
func TestNonAdminCannotCreateIntoAnotherProject(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	tenant := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	code, network := createNetwork(t, r, tenant,
		fmt.Sprintf(`{"network":{"name":"b11-tenant-cross","project_id":%q}}`, otherProjectID))
	if code != http.StatusBadRequest {
		t.Fatalf("non-admin cross-project create: code=%d body=%v; want 400", code, network)
	}

	var count int
	if err := testPool.QueryRow(context.Background(),
		"SELECT count(*) FROM networks WHERE name = 'b11-tenant-cross'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("a refused request created %d network(s); it must be refused, not redirected", count)
	}
}

// TestDisagreeingProjectSpellingsAreRefused — contract N3, upstream's populate_project_info.
func TestDisagreeingProjectSpellingsAreRefused(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	code, network := createNetwork(t, r, admin,
		fmt.Sprintf(`{"network":{"name":"b11-mismatch","project_id":%q,"tenant_id":%q}}`, otherProjectID, seededProjectID))
	if code != http.StatusBadRequest {
		t.Fatalf("disagreeing spellings: code=%d body=%v; want 400", code, network)
	}
}

// TestTenantSpellingIsAccepted: the legacy spelling alone must work and populate the other.
func TestTenantSpellingIsAccepted(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	code, network := createNetwork(t, r, admin,
		fmt.Sprintf(`{"network":{"name":"b11-tenant-spelling","tenant_id":%q}}`, otherProjectID))
	if code != http.StatusCreated {
		t.Fatalf("tenant_id spelling: code=%d body=%v; want 201", code, network)
	}
	if got := network["project_id"]; got != otherProjectID {
		t.Errorf("project_id = %#v, want %q — one spelling must populate the other", got, otherProjectID)
	}
}

// TestUnresolvableProjectIsRefused — contracts N4 and N5.
//
// 400 for all three ways of getting this attribute wrong, so a client has one rule. Never a 500 from
// the foreign key, and never a disclosure of whether some other project exists.
func TestUnresolvableProjectIsRefused(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	for _, tc := range []struct{ name, project string }{
		{"N4 absent project", uuid.New().String()},
		{"N5 malformed id", "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, network := createNetwork(t, r, admin,
				fmt.Sprintf(`{"network":{"name":"b11-bad-project","project_id":%q}}`, tc.project))
			if code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%v; want 400 — never a 500 from the FK", code, network)
			}
		})
	}
}

// TestNetworkResponsesReportTheRowsOwner — contracts N6 and N7.
//
// A shared network owned by another project is visible to a tenant. Reporting the caller as its owner
// tells them a foreign resource is theirs.
func TestNetworkResponsesReportTheRowsOwner(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	ctx := context.Background()
	r := newNeutronTestRouter()

	netID := uuid.New().String()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, network_type, mtu, created_at, updated_at)
		VALUES ($1, 'b11-shared-foreign', $2, true, 'ACTIVE', true, 'flat', 1500, now(), now())
	`, netID, otherProjectID); err != nil {
		t.Fatalf("seed shared network: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID) //nolint:errcheck // best-effort cleanup
	})

	tenant := map[string]string{"X-Test-Project": seededProjectID}

	t.Run("N6 list", func(t *testing.T) {
		code, body := doReq(r, http.MethodGet, "/v2.0/networks", tenant)
		if code != http.StatusOK {
			t.Fatalf("list: code=%d body=%s", code, body)
		}
		for _, n := range networksFromBody(t, body) {
			if n["name"] != "b11-shared-foreign" {
				continue
			}
			if got := n["project_id"]; got != otherProjectID {
				t.Errorf("list reports project_id = %#v for a foreign shared network, want %q (B11)", got, otherProjectID)
			}
			if got := n["tenant_id"]; got != otherProjectID {
				t.Errorf("list reports tenant_id = %#v, want %q", got, otherProjectID)
			}
			return
		}
		t.Fatal("the shared foreign network is not visible to the tenant at all")
	})

	t.Run("N7 show", func(t *testing.T) {
		code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+netID, tenant)
		if code != http.StatusOK {
			t.Fatalf("show: code=%d body=%s", code, body)
		}
		var resp struct {
			Network map[string]any `json:"network"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := resp.Network["project_id"]; got != otherProjectID {
			t.Errorf("show reports project_id = %#v, want %q (B11)", got, otherProjectID)
		}
	})
}

// TestGetNetworkCacheIsCallerIndependent — contract N10.
//
// GetNetwork caches the built response for 30 minutes. Once project_id is row-derived the entry is
// caller-independent and safe to share — but caching a caller-derived field would leak one tenant's
// view to another, so this asserts it rather than assuming it.
func TestGetNetworkCacheIsCallerIndependent(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	ctx := context.Background()
	r := newNeutronTestRouter()

	netID := uuid.New().String()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, network_type, mtu, created_at, updated_at)
		VALUES ($1, 'b11-cache', $2, true, 'ACTIVE', true, 'flat', 1500, now(), now())
	`, netID, otherProjectID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID) //nolint:errcheck // best-effort cleanup
	})

	// Warm the cache as one caller, then read as another.
	for _, caller := range []string{otherProjectID, seededProjectID} {
		code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+netID,
			map[string]string{"X-Test-Project": caller})
		if code != http.StatusOK {
			t.Fatalf("show as %s: code=%d body=%s", caller, code, body)
		}
		var resp struct {
			Network map[string]any `json:"network"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := resp.Network["project_id"]; got != otherProjectID {
			t.Errorf("caller %s sees project_id = %#v, want the owner %q", caller, got, otherProjectID)
		}
	}
}
