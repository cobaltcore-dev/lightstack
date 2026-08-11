package neutron

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/google/uuid"
)

// Branch review 2026-08-13, SRC-3 and SRC-4 — the external-network guard added for F11.
//
// requireExternalNetwork protects two things: that a router gateway or floating-IP pool names a
// network actually marked external (FR-034), and — SRC-4 — that it does not answer differently for a
// network the caller cannot see, which would make it an existence oracle.

// routerIDFromBody pulls the id out of a router create response.
func routerIDFromBody(t *testing.T, body string) string {
	t.Helper()
	var resp struct {
		Router struct {
			ID string `json:"id"`
		} `json:"router"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal router: %v (body=%s)", err, body)
	}
	if resp.Router.ID == "" {
		t.Fatalf("router response carries no id: %s", body)
	}
	return resp.Router.ID
}

// routerCount reports how many routers carry the given name, so a refusal can be checked to have
// written nothing rather than merely to have returned 400.
func routerCount(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := database.DB.QueryRow(context.Background(),
		"SELECT count(*) FROM routers WHERE name = $1", name).Scan(&n); err != nil {
		t.Fatalf("count routers %s: %v", name, err)
	}
	return n
}

// SRC-3: a gateway naming no usable network was accepted and PERSISTED with 201.
//
// requireExternalNetwork returned true for an empty or non-string network_id, deferring to
// "the existing handling", which on this path is configureExternalGateway — whose error CreateRouter
// deliberately swallows ("Log error but don't fail the creation"). So the row was written with a
// gateway that never passed validation, and reconcileNAT then treats it as a real gateway forever.
//
// This is the dishonest-success shape #546 exists to remove: the client asks for something, the
// server ignores it and reports 201.
func TestRouterGatewayWithUnusableNetworkIDIsRefused(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty string", `{"network_id":""}`},
		{"absent key", `{"enable_snat":true}`},
		{"not a string", `{"network_id":123}`},
		{"null", `{"network_id":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routerName := "src3-" + uuid.NewString()[:8]
			t.Cleanup(func() {
				database.DB.Exec(context.Background(), "DELETE FROM routers WHERE name = $1", routerName) //nolint:errcheck // best-effort cleanup
			})

			body := fmt.Sprintf(`{"router":{"name":%q,"external_gateway_info":%s}}`, routerName, tc.body)
			code, resp := doReqBody(r, http.MethodPost, "/v2.0/routers", body, neutronAuthHdr(seededProjectID, true))

			if code != http.StatusBadRequest {
				t.Errorf("POST /v2.0/routers with %s gateway: code=%d body=%s; want 400", tc.name, code, resp)
			}
			if n := routerCount(t, routerName); n != 0 {
				t.Errorf("%d router row(s) written despite the refusal; a refused request must persist nothing", n)
			}
		})
	}
}

// The same rule on update. UpdateRouter took the identical unvalidated path, so a router created
// without a gateway could acquire an unvalidated one afterwards.
func TestRouterGatewayUpdateWithUnusableNetworkIDIsRefused(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	routerName := "src3-upd-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		database.DB.Exec(context.Background(), "DELETE FROM routers WHERE name = $1", routerName) //nolint:errcheck // best-effort cleanup
	})

	code, resp := doReqBody(r, http.MethodPost, "/v2.0/routers",
		fmt.Sprintf(`{"router":{"name":%q}}`, routerName), neutronAuthHdr(seededProjectID, true))
	if code != http.StatusCreated {
		t.Fatalf("create router: code=%d body=%s; want 201", code, resp)
	}
	id := routerIDFromBody(t, resp)

	code, resp = doReqBody(r, http.MethodPut, "/v2.0/routers/"+id,
		`{"router":{"external_gateway_info":{"network_id":""}}}`, neutronAuthHdr(seededProjectID, true))
	if code != http.StatusBadRequest {
		t.Errorf("PUT gateway with an empty network_id: code=%d body=%s; want 400", code, resp)
	}

	var stored *string
	if err := database.DB.QueryRow(context.Background(),
		"SELECT external_gateway_info::text FROM routers WHERE id = $1", id).Scan(&stored); err != nil {
		t.Fatalf("read back router: %v", err)
	}
	if stored != nil && *stored != "null" {
		t.Errorf("external_gateway_info = %q after a refused update; want it unchanged (NULL)", *stored)
	}
}

// SRC-4: the lookup carried no visibility predicate, so a network the caller cannot see answered
// 400 ("not an external network") while an unknown id answered 404 — the two distinguish existence.
//
// The sibling handler added in the same commit, ListDHCPAgentsOnNetwork, applies
// (project_id = caller OR shared OR external) for exactly this reason. Nothing is lost by matching
// it: a legitimately usable gateway network is external, and external is its own visibility disjunct.
func TestRouterGatewayDoesNotDiscloseInvisibleNetworks(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()

	foreignPrivate := seedExternalNetwork(t, "src4-foreign-"+uuid.NewString()[:8], otherProjectID, false, false)

	for _, tc := range []struct {
		name      string
		networkID string
	}{
		{"another project's private network", foreignPrivate},
		{"a network that does not exist", uuid.NewString()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routerName := "src4-" + uuid.NewString()[:8]
			t.Cleanup(func() {
				database.DB.Exec(context.Background(), "DELETE FROM routers WHERE name = $1", routerName) //nolint:errcheck // best-effort cleanup
			})

			body := fmt.Sprintf(`{"router":{"name":%q,"external_gateway_info":{"network_id":%q}}}`,
				routerName, tc.networkID)
			code, resp := doReqBody(r, http.MethodPost, "/v2.0/routers", body, neutronAuthHdr(seededProjectID, true))

			// Both must be 404. A 400 for the first and 404 for the second is the oracle.
			if code != http.StatusNotFound {
				t.Errorf("gateway on %s: code=%d body=%s; want 404 — an invisible network must be "+
					"indistinguishable from an absent one", tc.name, code, resp)
			}
		})
	}
}

// A network that IS external stays usable and keeps reporting the helpful error when it is not —
// the guard must refuse the invisible, not everything.
func TestRouterGatewayAcceptsAVisibleExternalNetwork(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()

	// Owned by another project but marked external, so visible to everyone (FR-033a).
	ext := seedExternalNetwork(t, "src4-ext-"+uuid.NewString()[:8], otherProjectID, false, true)
	ownPrivate := seedExternalNetwork(t, "src4-own-"+uuid.NewString()[:8], seededProjectID, false, false)

	for _, tc := range []struct {
		name      string
		networkID string
		want      int
	}{
		{"a foreign but external network is accepted", ext, http.StatusCreated},
		{"the caller's own non-external network is refused 400, not hidden", ownPrivate, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routerName := "src4ok-" + uuid.NewString()[:8]
			t.Cleanup(func() {
				database.DB.Exec(context.Background(), "DELETE FROM routers WHERE name = $1", routerName) //nolint:errcheck // best-effort cleanup
			})

			body := fmt.Sprintf(`{"router":{"name":%q,"external_gateway_info":{"network_id":%q}}}`,
				routerName, tc.networkID)
			code, resp := doReqBody(r, http.MethodPost, "/v2.0/routers", body, neutronAuthHdr(seededProjectID, true))
			if code != tc.want {
				t.Errorf("%s: code=%d body=%s; want %d", tc.name, code, resp, tc.want)
			}
		})
	}
}
