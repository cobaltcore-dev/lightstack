package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/google/uuid"
)

// A network's owning project is reported from the row, not the caller: the list and show
// queries can legitimately return a SHARED network owned by someone else, and reporting the
// caller told a tenant that a foreign resource was theirs. Moved here from SPEC-024
// (cloud-storage#546, B11) on PR #79 review; #546 keeps the other half — CreateNetwork
// honouring a requested project.

const otherProjectID = "00000000-0000-0000-0000-0000000b0b0b"

// requireSecondProject guarantees the second project exists, creating it if needed. A missing
// fixture must be built, not skipped: skipping would let every cross-project test report green
// against a freshly migrated database while proving nothing.
func requireSecondProject(t *testing.T) {
	t.Helper()
	if _, err := database.DB.Exec(context.Background(), `
		INSERT INTO projects (id, name, domain_id, enabled, created_at, updated_at)
		VALUES ($1, 'spec024-other', (SELECT id FROM domains LIMIT 1), true, now(), now())
		ON CONFLICT (id) DO NOTHING
	`, otherProjectID); err != nil {
		t.Fatalf("seed second project: %v", err)
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
	if _, err := database.DB.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, network_type, mtu, created_at, updated_at)
		VALUES ($1, 'b11-shared-foreign', $2, true, 'ACTIVE', true, 'flat', 1500, now(), now())
	`, netID, otherProjectID); err != nil {
		t.Fatalf("seed shared network: %v", err)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID) //nolint:errcheck // best-effort cleanup
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
	if _, err := database.DB.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, network_type, mtu, created_at, updated_at)
		VALUES ($1, 'b11-cache', $2, true, 'ACTIVE', true, 'flat', 1500, now(), now())
	`, netID, otherProjectID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() {
		database.DB.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID) //nolint:errcheck // best-effort cleanup
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
