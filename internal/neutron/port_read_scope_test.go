package neutron

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// Branch review SRC-6 (2026-11-23-review.md): a member read every project's ports on a shared network —
// their names, MACs, addresses, the servers they belong to (device_id) and their security groups.
// Upstream's get_port admits the port's project and the network's (PROJECT_READER, NET_OWNER_READER):
// a shared network is readable by every project, the ports on it are not (SPEC-033 FR-061).

// seedProjectPort seeds a port of project on network, removed again by cleanup.
func seedProjectPort(t *testing.T, project, network string) string {
	t.Helper()
	id := uuid.NewString()
	mustExec(t, `INSERT INTO ports (id, name, network_id, project_id, mac_address, status, fixed_ips)
		VALUES ($1, 'src6-port', $2, $3, $4, 'ACTIVE', '[]'::jsonb)`,
		id, network, project, fmt.Sprintf("fa:16:3e:56:%02x:%02x", rand.IntN(256), rand.IntN(256)))
	t.Cleanup(func() { mustExec(t, "DELETE FROM ports WHERE id = $1", id) })
	return id
}

func TestPortsOnASharedNetworkAreTheirProjectsAndItsOwners(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	owner := createTestProject(t, "src6-network-owner")
	other := createTestProject(t, "src6-other-tenant")
	network := seedExternalNetwork(t, "src6-shared-"+uuid.NewString()[:6], owner, true, false)
	mine := seedProjectPort(t, seededProjectID, network)
	theirs := seedProjectPort(t, other, network)
	owners := seedProjectPort(t, owner, network)
	member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	networkOwner := map[string]string{"X-Test-Project": owner, "X-Test-Roles": "member"}

	listed := func(t *testing.T, hdr map[string]string) []string {
		t.Helper()
		code, body := doReq(r, http.MethodGet, "/v2.0/ports?network_id="+network, hdr)
		if code != http.StatusOK {
			t.Fatalf("list: code=%d body=%s", code, body)
		}
		var out struct {
			Ports []struct {
				ID string `json:"id"`
			} `json:"ports"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, p := range out.Ports {
			ids = append(ids, p.ID)
		}
		slices.Sort(ids)
		return ids
	}

	t.Run("a member of a third project sees their own port only", func(t *testing.T) {
		if got := listed(t, member); !slices.Equal(got, []string{mine}) {
			t.Errorf("listed %v; want only %s", got, mine)
		}
		for _, foreign := range []string{theirs, owners} {
			if code, body := doReq(r, http.MethodGet, "/v2.0/ports/"+foreign, member); code != http.StatusNotFound {
				t.Errorf("show of another project's port %s: code=%d body=%s; want 404", foreign, code, body)
			}
		}
		// A marker is read only with a limit; one naming another project's port must read as one naming none.
		foreignMarker, _ := doReq(r, http.MethodGet, "/v2.0/ports?network_id="+network+"&limit=10&marker="+theirs, member)
		unknownMarker, _ := doReq(r, http.MethodGet, "/v2.0/ports?network_id="+network+"&limit=10&marker="+uuid.NewString(), member)
		if foreignMarker != unknownMarker || unknownMarker != http.StatusNotFound {
			t.Errorf("another project's port as a marker: %d; an unknown one: %d; want 404 for both", foreignMarker, unknownMarker)
		}
	})

	t.Run("the network's owner sees every port on it", func(t *testing.T) {
		want := []string{mine, theirs, owners}
		slices.Sort(want)
		if got := listed(t, networkOwner); !slices.Equal(got, want) {
			t.Errorf("listed %v; want %v", got, want)
		}
		if code, body := doReq(r, http.MethodGet, "/v2.0/ports/"+theirs, networkOwner); code != http.StatusOK {
			t.Errorf("show of a port on its network: code=%d body=%s; want 200", code, body)
		}
	})
}
