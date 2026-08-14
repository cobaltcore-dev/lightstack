package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-024 (cloud-storage#546) US6 / F11 — external networks are modelled.
//
// `router:external` was hard-coded false in all three emitters and `?router:external=true` returned
// an empty list, so Horizon's router "Set Gateway" dropdown was always empty and guest egress could
// not be configured from the UI. Meanwhile router gateways and floating IPs accepted ANY network as
// external, with no check at all.
//
// Two rules that are easy to get subtly wrong and are pinned here:
//
//   - Visibility is a THIRD independent disjunct: owner OR shared OR external. Requiring `shared` as
//     well would leave the tenant-facing dropdown empty and fix F11 for administrators only — the
//     clarify decision behind FR-033a.
//   - Visibility is READ-ONLY (FR-033b). Being able to see a network confers nothing, exactly as
//     #545 established for shared networks.

// seedExternalNetwork inserts a network owned by `owner` with the given flags.
func seedExternalNetwork(t *testing.T, name, owner string, shared, external bool) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.New().String()

	if _, err := testPool.Exec(ctx, `
		INSERT INTO networks (id, name, project_id, admin_state_up, status, shared, external, network_type, mtu, created_at, updated_at)
		VALUES ($1, $2, $3, true, 'ACTIVE', $4, $5, 'flat', 1500, now(), now())
	`, id, name, owner, shared, external); err != nil {
		t.Fatalf("seed network %s: %v", name, err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM subnets WHERE network_id = $1", id) //nolint:errcheck // best-effort cleanup
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", id)        //nolint:errcheck // best-effort cleanup
	})
	return id
}

func storedExternal(t *testing.T, networkID string) bool {
	t.Helper()
	var ext bool
	if err := testPool.QueryRow(context.Background(),
		"SELECT external FROM networks WHERE id = $1", networkID).Scan(&ext); err != nil {
		t.Fatalf("read stored external: %v", err)
	}
	return ext
}

// TestAdminCanMarkNetworkExternal — contracts X1 and X5.
func TestAdminCanMarkNetworkExternal(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	code, network := createNetwork(t, r, admin, `{"network":{"name":"f11-ext","router:external":true}}`)
	if code != http.StatusCreated {
		t.Fatalf("admin create external: code=%d body=%v; want 201", code, network)
	}
	if got := network["router:external"]; got != true {
		t.Errorf("router:external = %#v, want true — the attribute was discarded (F11)", got)
	}

	id, _ := network["id"].(string)
	if !storedExternal(t, id) {
		t.Error("stored external = false; the flag must persist, not just echo (X5)")
	}

	// Read back through show: the emitter must report the stored value, not a literal.
	code, body := doReq(r, http.MethodGet, "/v2.0/networks/"+id, admin)
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	var resp struct {
		Network map[string]any `json:"network"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := resp.Network["router:external"]; got != true {
		t.Errorf("show reports router:external = %#v, want true", got)
	}
}

// TestNonAdminCannotMarkNetworkExternal — contracts X2 and X3.
//
// Upstream gates this exact attribute on create to administrators
// (`create_network:router:external` → ADMIN), so this is fidelity rather than a local rule.
func TestNonAdminCannotMarkNetworkExternal(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	tenant := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	t.Run("X2 create", func(t *testing.T) {
		code, network := createNetwork(t, r, tenant, `{"network":{"name":"f11-tenant-ext","router:external":true}}`)
		if code == http.StatusCreated && network["router:external"] == true {
			t.Fatal("a non-administrator marked a network external")
		}
		if code != http.StatusForbidden && code != http.StatusBadRequest {
			t.Fatalf("code=%d body=%v; want a refusal", code, network)
		}
	})

	t.Run("X3 update own network", func(t *testing.T) {
		netID := seedExternalNetwork(t, "f11-tenant-own", seededProjectID, false, false)
		code, body := doReqBody(r, http.MethodPut, "/v2.0/networks/"+netID,
			`{"network":{"router:external":true}}`, tenant)
		if code == http.StatusOK && storedExternal(t, netID) {
			t.Fatal("a non-administrator marked their own network external")
		}
		if code != http.StatusForbidden && code != http.StatusBadRequest {
			t.Fatalf("code=%d body=%s; want a refusal", code, body)
		}
	})
}

// TestRouterExternalFalseIsGatedAsUpstreamGatesIt — create and update differ, deliberately.
//
// Upstream skips an attribute's policy on create when the value sent equals its default
// (neutron/policy.py, _is_attribute_explicitly_set), and router:external defaults to false, so a
// tenant may send it — `openstack network create --internal` does exactly that. On update the policy
// applies whenever the attribute is present, whatever its value. Gating create on presence instead
// would refuse every tenant's `--internal`.
func TestRouterExternalFalseIsGatedAsUpstreamGatesIt(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	tenant := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	t.Run("create with false is allowed", func(t *testing.T) {
		code, network := createNetwork(t, r, tenant, `{"network":{"name":"f11-tenant-internal","router:external":false}}`)
		if code != http.StatusCreated {
			t.Fatalf("code=%d body=%v; want 201", code, network)
		}
		if got := network["router:external"]; got != false {
			t.Errorf("router:external = %v, want false", got)
		}
	})

	t.Run("update with false is refused", func(t *testing.T) {
		netID := seedExternalNetwork(t, "f11-tenant-own-internal", seededProjectID, false, false)
		code, body := doReqBody(r, http.MethodPut, "/v2.0/networks/"+netID, `{"network":{"router:external":false}}`, tenant)
		if code != http.StatusForbidden {
			t.Fatalf("code=%d body=%s; want 403", code, body)
		}
	})
}

// TestAdminCanMarkAnotherProjectsNetworkExternal — contract X4.
//
// 🔑 The blocker research R6.4 found: UpdateNetwork's statement is owner-scoped
// (`WHERE id = $n AND project_id = $n+1`), so an administrator marking a TENANT's network external
// updated zero rows and reported success — the same dishonest-success defect as B11, inside the fix
// for a different one.
func TestAdminCanMarkAnotherProjectsNetworkExternal(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	netID := seedExternalNetwork(t, "f11-foreign", otherProjectID, false, false)

	code, body := doReqBody(r, http.MethodPut, "/v2.0/networks/"+netID,
		`{"network":{"router:external":true}}`, admin)
	if code != http.StatusOK {
		t.Fatalf("admin update of a tenant's network: code=%d body=%s; want 200", code, body)
	}
	if !storedExternal(t, netID) {
		t.Error("reported success but updated zero rows — the owner-scoped predicate swallowed it (research R6.4)")
	}
}

// TestAdminWriteToForeignNetworkIsReportedHonestly — the case the original X4 missed.
//
// Asserting only the external=true direction passed for the WRONG REASON: that write makes the row
// match GetNetwork's `OR external = true` disjunct, so the read-back happened to succeed. Renaming, and
// unmarking external, both applied and returned 404 — the row was modified and the caller was told it
// did not exist. Found by the host pass, not by this file.
//
// Every direction must agree: the status reflects what happened, and the body describes the row as it
// now stands.
func TestAdminWriteToForeignNetworkIsReportedHonestly(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	netID := seedExternalNetwork(t, "f11-honest", otherProjectID, false, false)

	readBack := func(t *testing.T, body string) map[string]any {
		t.Helper()
		var resp struct {
			Network map[string]any `json:"network"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal: %v (body=%s)", err, body)
		}
		return resp.Network
	}

	t.Run("mark external", func(t *testing.T) {
		code, body := doReqBody(r, http.MethodPut, "/v2.0/networks/"+netID, `{"network":{"router:external":true}}`, admin)
		if code != http.StatusOK {
			t.Fatalf("code=%d body=%s; want 200", code, body)
		}
		if got := readBack(t, body)["router:external"]; got != true {
			t.Errorf("response router:external = %#v, want true", got)
		}
		if !storedExternal(t, netID) {
			t.Error("stored external is false")
		}
	})

	t.Run("unmark external", func(t *testing.T) {
		code, body := doReqBody(r, http.MethodPut, "/v2.0/networks/"+netID, `{"network":{"router:external":false}}`, admin)
		if code != http.StatusOK {
			t.Fatalf("code=%d body=%s; want 200 — unmarking applied but used to answer 404, because the row stopped matching the read-back scope", code, body)
		}
		if got := readBack(t, body)["router:external"]; got != false {
			t.Errorf("response router:external = %#v, want false", got)
		}
		if storedExternal(t, netID) {
			t.Error("stored external is still true")
		}
	})

	t.Run("rename", func(t *testing.T) {
		code, body := doReqBody(r, http.MethodPut, "/v2.0/networks/"+netID, `{"network":{"name":"f11-honest-renamed"}}`, admin)
		if code != http.StatusOK {
			t.Fatalf("code=%d body=%s; want 200 — the rename applied but used to answer 404", code, body)
		}
		net := readBack(t, body)
		if got := net["name"]; got != "f11-honest-renamed" {
			t.Errorf("response name = %#v, want the new name", got)
		}
		if got := net["project_id"]; got != otherProjectID {
			t.Errorf("response project_id = %#v, want the real owner %q — the read-back must not echo the caller", got, otherProjectID)
		}
	})
}

// TestExternalFilterIsComplementary — contract X6.
func TestExternalFilterIsComplementary(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}

	seedExternalNetwork(t, "f11-is-ext", seededProjectID, false, true)
	seedExternalNetwork(t, "f11-not-ext", seededProjectID, false, false)

	code, body := doReq(r, http.MethodGet, "/v2.0/networks?router:external=true", hdr)
	if code != http.StatusOK {
		t.Fatalf("list external: code=%d body=%s", code, body)
	}
	ext := networksFromBody(t, body)
	if !hasNet(ext, "f11-is-ext") {
		t.Error("?router:external=true returned nothing for an external network — F11's empty dropdown")
	}
	if hasNet(ext, "f11-not-ext") {
		t.Error("?router:external=true returned a non-external network")
	}

	code, body = doReq(r, http.MethodGet, "/v2.0/networks?router:external=false", hdr)
	if code != http.StatusOK {
		t.Fatalf("list non-external: code=%d body=%s", code, body)
	}
	nonExt := networksFromBody(t, body)
	if !hasNet(nonExt, "f11-not-ext") {
		t.Error("?router:external=false lost the non-external network")
	}
	if hasNet(nonExt, "f11-is-ext") {
		t.Error("the two filters are not complements")
	}
}

// TestExternalNetworkIsVisibleCrossProjectReadOnly — contracts X7, X8 and X9.
//
// The clarify decision: external qualifies for visibility on its own, WITHOUT `shared`. Requiring
// both would leave the tenant-facing gateway selection empty, i.e. F11 fixed for administrators only.
func TestExternalNetworkIsVisibleCrossProjectReadOnly(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	tenant := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	// Owned by another project, NOT shared, external.
	netID := seedExternalNetwork(t, "f11-foreign-ext", otherProjectID, false, true)

	t.Run("X7 visible with its real owner", func(t *testing.T) {
		code, body := doReq(r, http.MethodGet, "/v2.0/networks", tenant)
		if code != http.StatusOK {
			t.Fatalf("list: code=%d body=%s", code, body)
		}
		for _, n := range networksFromBody(t, body) {
			if n["name"] != "f11-foreign-ext" {
				continue
			}
			if got := n["project_id"]; got != otherProjectID {
				t.Errorf("project_id = %#v, want the real owner %q", got, otherProjectID)
			}
			return
		}
		t.Fatal("an external network owned by another project is invisible to the tenant — F11 stays broken for non-admins (FR-033a)")
	})

	t.Run("X8 cannot attach a subnet", func(t *testing.T) {
		code, body := doReqBody(r, http.MethodPost, "/v2.0/subnets",
			fmt.Sprintf(`{"subnet":{"network_id":%q,"cidr":"10.99.1.0/24","name":"f11-steal"}}`, netID), tenant)
		if code == http.StatusCreated {
			t.Error("visibility must not confer authority to define address ranges (FR-033b)")
		}
		_ = body
	})

	t.Run("X9 cannot rename, delete or unmark", func(t *testing.T) {
		if code, _ := doReqBody(r, http.MethodPut, "/v2.0/networks/"+netID,
			`{"network":{"name":"f11-hijacked"}}`, tenant); code == http.StatusOK {
			t.Error("a non-owner renamed a network they can merely see")
		}
		if code, _ := doReqBody(r, http.MethodPut, "/v2.0/networks/"+netID,
			`{"network":{"router:external":false}}`, tenant); code == http.StatusOK && !storedExternal(t, netID) {
			t.Error("a non-owner unmarked an external network")
		}
		if code, _ := doReq(r, http.MethodDelete, "/v2.0/networks/"+netID, tenant); code == http.StatusNoContent || code == http.StatusOK {
			t.Error("a non-owner deleted a network they can merely see")
		}
	})
}

// TestExternalNetworkSubnetsAreReadableCrossProject — an external network's subnets are as visible as
// the network, and no more writable. A network response lists every subnet id of the network, so a
// caller shown the network must be able to resolve them (SPEC-020 FR-007); the subnet read scope had
// `shared` but not `external`, so a tenant saw the external network beside subnet ids it could not
// list or fetch. Upstream reads subnets through the network's RBAC entries with access_as_external
// as well as access_as_shared (neutron/db/external_net_db.py).
func TestExternalNetworkSubnetsAreReadableCrossProject(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()
	tenant := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	// Both networks belong to the other project; seedExternalNetwork's cleanup removes their subnets.
	seedForeignSubnet := func(networkID, cidr, gateway string) string {
		id := uuid.NewString()
		mustExec(t, `INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
			VALUES ($1, 'f11-foreign-subnet', $2, $3, $4, $5, 4, false, now(), now())`,
			id, networkID, otherProjectID, cidr, gateway)
		return id
	}
	external := seedExternalNetwork(t, "f11-ext-subnets", otherProjectID, false, true)
	private := seedExternalNetwork(t, "f11-private-subnets", otherProjectID, false, false)
	extSubnet := seedForeignSubnet(external, "10.95.0.0/24", "10.95.0.1")
	privSubnet := seedForeignSubnet(private, "10.95.1.0/24", "10.95.1.1")

	listed := func(networkID string) []string {
		code, body := doReq(r, http.MethodGet, "/v2.0/subnets?network_id="+networkID, tenant)
		if code != http.StatusOK {
			t.Fatalf("list subnets of %s: code=%d body=%s", networkID, code, body)
		}
		var ids []string
		for _, s := range subnetsFromBody(t, body) {
			ids = append(ids, fmt.Sprint(s["id"]))
		}
		return ids
	}

	t.Run("listed with the external network", func(t *testing.T) {
		if ids := listed(external); len(ids) != 1 || ids[0] != extSubnet {
			t.Errorf("subnets of the external network = %v, want [%s]", ids, extSubnet)
		}
	})
	t.Run("shown by id", func(t *testing.T) {
		if code, body := doReq(r, http.MethodGet, "/v2.0/subnets/"+extSubnet, tenant); code != http.StatusOK {
			t.Errorf("GET external network's subnet: code=%d body=%s, want 200", code, body)
		}
	})
	t.Run("not writable", func(t *testing.T) {
		if code, _ := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+extSubnet, `{"subnet":{"name":"f11-hijacked"}}`, tenant); code != http.StatusNotFound {
			t.Errorf("PUT by a non-owner: code=%d, want 404", code)
		}
		if code, _ := doReq(r, http.MethodDelete, "/v2.0/subnets/"+extSubnet, tenant); code != http.StatusNotFound {
			t.Errorf("DELETE by a non-owner: code=%d, want 404", code)
		}
		if ids := listed(external); len(ids) != 1 {
			t.Errorf("after refused writes the external network lists %v, want its one subnet", ids)
		}
	})
	t.Run("a private network's subnet stays hidden", func(t *testing.T) {
		if ids := listed(private); len(ids) != 0 {
			t.Errorf("subnets of another project's private network = %v, want none", ids)
		}
		if code, _ := doReq(r, http.MethodGet, "/v2.0/subnets/"+privSubnet, tenant); code != http.StatusNotFound {
			t.Errorf("GET another project's private subnet: code=%d, want 404", code)
		}
	})
}

// TestRouterGatewayRequiresExternalNetwork — contracts X10 and X12.
func TestRouterGatewayRequiresExternalNetwork(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	internal := seedExternalNetwork(t, "f11-gw-internal", seededProjectID, false, false)
	external := seedExternalNetwork(t, "f11-gw-external", seededProjectID, false, true)

	t.Run("X10 non-external is refused", func(t *testing.T) {
		code, body := doReqBody(r, http.MethodPost, "/v2.0/routers",
			fmt.Sprintf(`{"router":{"name":"f11-r-bad","external_gateway_info":{"network_id":%q}}}`, internal), admin)
		if code == http.StatusCreated {
			t.Errorf("a non-external network was accepted as a router gateway; body=%s", body)
		}
	})

	t.Run("X12 external succeeds", func(t *testing.T) {
		code, body := doReqBody(r, http.MethodPost, "/v2.0/routers",
			fmt.Sprintf(`{"router":{"name":"f11-r-good","external_gateway_info":{"network_id":%q}}}`, external), admin)
		if code != http.StatusCreated {
			t.Fatalf("external gateway: code=%d body=%s; want 201", code, body)
		}
		var resp struct {
			Router map[string]any `json:"router"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err == nil {
			if id, _ := resp.Router["id"].(string); id != "" {
				t.Cleanup(func() {
					testPool.Exec(context.Background(), "DELETE FROM routers WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
				})
			}
		}
	})
}

// TestFloatingIPRequiresExternalNetwork — contract X11.
func TestFloatingIPRequiresExternalNetwork(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	internal := seedExternalNetwork(t, "f11-fip-internal", seededProjectID, false, false)

	code, body := doReqBody(r, http.MethodPost, "/v2.0/floatingips",
		fmt.Sprintf(`{"floatingip":{"floating_network_id":%q}}`, internal), admin)
	if code == http.StatusCreated {
		t.Errorf("a floating IP was allocated from a non-external network; body=%s", body)
	}
}

// TestMultipleExternalNetworksAreAllowed — contract X15: nothing may assume there is exactly one.
func TestMultipleExternalNetworksAreAllowed(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID}

	seedExternalNetwork(t, "f11-ext-1", seededProjectID, false, true)
	seedExternalNetwork(t, "f11-ext-2", seededProjectID, false, true)

	code, body := doReq(r, http.MethodGet, "/v2.0/networks?router:external=true", hdr)
	if code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", code, body)
	}
	got := networksFromBody(t, body)
	if !hasNet(got, "f11-ext-1") || !hasNet(got, "f11-ext-2") {
		t.Errorf("both external networks must be listed; got %d", len(got))
	}
}

// TestGrandfatheredGatewayKeepsWorking — contracts X13 and X14 / FR-035a.
//
// Migration 066 defaults external to false, so every gateway configured before it would fail the new
// check. Validation therefore applies to NEW requests only: a stored gateway keeps working, and the
// reconciler must keep asserting its NAT rather than withdrawing it — an upgrade may not silently
// remove working guest egress.
func TestGrandfatheredGatewayKeepsWorking(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newNeutronTestRouter()
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "admin"}

	// A router whose gateway network is NOT external — the pre-migration state.
	netID := seedExternalNetwork(t, "f11-legacy-gw", seededProjectID, false, false)
	routerID := uuid.New().String()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO routers (id, name, project_id, admin_state_up, status, external_gateway_info, created_at, updated_at)
		VALUES ($1, 'f11-legacy-router', $2, true, 'ACTIVE', $3, now(), now())
	`, routerID, seededProjectID, fmt.Sprintf(`{"network_id":%q,"enable_snat":true}`, netID)); err != nil {
		t.Fatalf("seed legacy router: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM routers WHERE id = $1", routerID) //nolint:errcheck // best-effort cleanup
	})

	// X13: it is still readable and its gateway is intact — nothing withdrew it.
	code, body := doReq(r, http.MethodGet, "/v2.0/routers/"+routerID, admin)
	if code != http.StatusOK {
		t.Fatalf("grandfathered router: code=%d body=%s; want 200 — an upgrade must not break it", code, body)
	}

	// X14: a NEW gateway on that same non-external network is refused.
	code, body = doReqBody(r, http.MethodPost, "/v2.0/routers",
		fmt.Sprintf(`{"router":{"name":"f11-new-on-legacy","external_gateway_info":{"network_id":%q}}}`, netID), admin)
	if code == http.StatusCreated {
		t.Errorf("a NEW gateway on a non-external network must be refused even though old ones are grandfathered; body=%s", body)
	}
}
