package neutron

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Pagination markers as upstream Neutron resolves them (neutron-lib db/utils.get_marker_obj): only when
// a limit is given, and through _get_<resource>, which applies the caller's visibility — so a marker
// that does not resolve is that resource's 404. The networks, subnets and ports listings read the
// marker with no visibility at all and ignored one that did not resolve: another project's private
// network narrowed the page where an absent id did not, telling the caller it exists.
func TestListingMarkersResolveOnlyWhatTheCallerMayRead(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newEnvelopeRouter() // the caller is the seeded project

	// Another project's private network, with a subnet and a port: none of them visible to the caller.
	foreignNet := seedExternalNetwork(t, "spec024-marker-foreign-"+uuid.NewString()[:8], otherProjectID, false, false)
	foreignSubnet, foreignPort := uuid.NewString(), uuid.NewString()
	mustExec(t, `INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
		VALUES ($1, 'spec024-marker-foreign', $2, $3, '10.83.0.0/24', NULL, 4, false, now(), now())`, foreignSubnet, foreignNet, otherProjectID)
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address)
		VALUES ($1, 'spec024-marker-foreign', $2, $3, 'fa:16:3e:00:00:07')`, foreignPort, foreignNet, otherProjectID)

	ownNet := seedNetwork(t, "spec024-marker-own-"+uuid.NewString()[:8])
	ownSubnet := seedSubnet(t, ownNet, "spec024-marker-own", "10.83.1.0/24", nil, 4, false)
	ownPort := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address)
		VALUES ($1, 'spec024-marker-own', $2, $3, 'fa:16:3e:00:00:08')`, ownPort, ownNet, seededProjectID)

	for _, tc := range []struct{ collection, foreign, own, notFound string }{
		{"networks", foreignNet, ownNet, "NetworkNotFound"},
		{"subnets", foreignSubnet, ownSubnet, "SubnetNotFound"},
		{"ports", foreignPort, ownPort, "PortNotFound"},
	} {
		t.Run(tc.collection, func(t *testing.T) {
			for _, marker := range []string{tc.foreign, uuid.NewString(), "not-a-uuid"} {
				code, body := doReq(r, http.MethodGet, "/v2.0/"+tc.collection+"?limit=5&marker="+marker, nil)
				if code != http.StatusNotFound {
					t.Errorf("marker %s: code=%d body=%s, want 404 — an invisible marker must answer as an absent one", marker, code, body)
					continue
				}
				assertNeutronShape(t, neutronError(t, body), tc.notFound)
			}
			if code, body := doReq(r, http.MethodGet, "/v2.0/"+tc.collection+"?marker="+tc.foreign, nil); code != http.StatusOK {
				t.Errorf("without a limit: code=%d body=%s, want 200 — upstream does not resolve the marker", code, body)
			}
			if code, body := doReq(r, http.MethodGet, "/v2.0/"+tc.collection+"?limit=5&marker="+tc.own, nil); code != http.StatusOK {
				t.Errorf("the caller's own marker: code=%d body=%s, want 200", code, body)
			}
		})
	}
}
