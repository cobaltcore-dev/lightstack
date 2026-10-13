package neutron

import (
	"net/http"
	"testing"
)

// Review 2026-10-13 M-4: `shared` exposes a network to every tenant in the
// deployment, but CreateNetwork accepted it from anyone — a member could open
// their network to every project (and every project's guests to it). It is an
// operator decision, gated exactly like router:external on create: only
// `true` is refused to a member, so a client that echoes the default
// (shared=false) keeps working for tenants, as upstream's explicit-set rule
// does for external.
func TestNonAdminCannotCreateSharedNetwork(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	tenant := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	if code, network := createNetwork(t, r, tenant, `{"network":{"name":"m4-tenant-shared","shared":true}}`); code != http.StatusForbidden {
		t.Fatalf("member creating a shared network: code=%d body=%v; want 403", code, network)
	}

	// shared=false is the default — a tenant echoing it is not asking for
	// anything, and must not be refused (upstream's explicit-set rule).
	if code, _ := createNetwork(t, r, tenant, `{"network":{"name":"m4-tenant-internal","shared":false}}`); code != http.StatusCreated {
		t.Fatalf("member creating a normal network: want 201")
	}

	// An administrator keeps the shared path.
	if code, network := createNetwork(t, r, admin, `{"network":{"name":"m4-admin-shared","shared":true}}`); code != http.StatusCreated || network["shared"] != true {
		t.Fatalf("admin creating a shared network: code=%d body=%v; want 201 with shared=true", code, network)
	}
}
