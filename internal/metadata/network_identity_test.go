package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T121 (FR-037; research R29): a guest reaches metadata through its network's DHCP
// namespace, whose listener serves the handler knowing its network (ServeNetwork). There a guest is
// identified by its network and its address, which stays unambiguous when two networks use one range;
// a request without a network is identified by its address alone, as before. The loopback and
// trusted-header rules are unchanged.

// seedMDNetwork inserts a network of the seeded project.
func seedMDNetwork(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := testPool.Exec(ctx, `INSERT INTO networks (id, name, project_id, shared) VALUES ($1, $2, $3, false)`,
		id, "md-net-"+id[:8], seededProjectID); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", id) })
	return id
}

// seedGuestOn inserts an instance with one port on network holding ip, as a booted guest has.
func seedGuestOn(t *testing.T, network, ip string) string {
	t.Helper()
	id := seedInstance(t, nil)
	ctx := context.Background()
	var portID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO ports (project_id, network_id, device_id, device_owner, fixed_ips) VALUES ($1, $2, $3, 'compute:nova', $4::jsonb) RETURNING id`,
		seededProjectID, network, id, fmt.Sprintf(`[{"ip_address":%q}]`, ip)).Scan(&portID); err != nil {
		t.Fatalf("insert port: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM ports WHERE id = $1", portID) })
	return id
}

// networkGet issues a request as it arrives at the listener in network's DHCP namespace: from peer,
// served through ServeNetwork.
func networkGet(h http.Handler, network, path, peer string, headers map[string]string) (int, string) {
	return networkGetCtx(context.Background(), h, network, path, peer, headers)
}

// networkGetCtx is networkGet with the request's context, so a test that makes a table unavailable
// (dbtest.Unavailable) can give the request a deadline: a statement blocked on PostgreSQL's ACCESS
// EXCLUSIVE lock fails instead of waiting indefinitely.
func networkGetCtx(ctx context.Context, h http.Handler, network, path, peer string, headers map[string]string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	req.RemoteAddr = peer + ":40000"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	ServeNetwork(h, network).ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func TestGuestIdentifiedByNetworkAndAddress_DB(t *testing.T) {
	requireDB(t)
	n := seedMDNetwork(t)
	self := seedGuestOn(t, n, "198.51.100.60")

	code, body := networkGet(newMetadataTestRouter(), n, "/latest/meta-data/instance-id", "198.51.100.60", nil)
	if code != http.StatusOK || body != ec2InstanceID(self) {
		t.Errorf("got %d %q; want 200 %q", code, body, ec2InstanceID(self))
	}
}

// Two networks may use one range, as upstream's do: each network's listener identifies its own guest.
// Without a network the address alone is ambiguous, and identifies neither.
func TestSameAddressOnTwoNetworksResolvesPerNetwork_DB(t *testing.T) {
	requireDB(t)
	n1, n2 := seedMDNetwork(t), seedMDNetwork(t)
	a := seedGuestOn(t, n1, "198.51.100.61")
	b := seedGuestOn(t, n2, "198.51.100.61")
	r := newMetadataTestRouter()

	for _, c := range []struct{ network, want string }{{n1, a}, {n2, b}} {
		if code, body := networkGet(r, c.network, "/latest/meta-data/instance-id", "198.51.100.61", nil); code != http.StatusOK || body != ec2InstanceID(c.want) {
			t.Errorf("through network %s: got %d %q; want 200 %q, its own guest", c.network, code, body, ec2InstanceID(c.want))
		}
	}
	if code, body := hostGet(r, "/latest/meta-data/instance-id", "198.51.100.61", nil); code != http.StatusNotFound {
		t.Errorf("the host's own listener: got %d %q; want 404, it names no guest by address", code, body)
	}
}

// A request through one network's listener never identifies a guest of another network.
func TestAddressOnAnotherNetworkIdentifiesNoInstance_DB(t *testing.T) {
	requireDB(t)
	n1, n2 := seedMDNetwork(t), seedMDNetwork(t)
	other := seedGuestOn(t, n1, "198.51.100.62")

	if code, body := networkGet(newMetadataTestRouter(), n2, "/latest/meta-data/instance-id", "198.51.100.62", nil); code != http.StatusNotFound {
		t.Errorf("through network %s: got %d %q; want 404, not network %s's guest (%q)", n2, code, body, n1, ec2InstanceID(other))
	}
}

// The header rules stand behind a network's listener: X-Instance-ID is believed from loopback, or from
// anyone once trust_instance_header is on, and never from a guest otherwise.
func TestNetworkListenerKeepsTheHeaderRules_DB(t *testing.T) {
	requireDB(t)
	n := seedMDNetwork(t)
	self := seedGuestOn(t, n, "198.51.100.63")
	other := seedGuestOn(t, n, "198.51.100.64")
	named := map[string]string{"X-Instance-ID": other}
	r := newMetadataTestRouter()

	if code, body := networkGet(r, n, "/latest/meta-data/instance-id", "198.51.100.63", named); code != http.StatusOK || body != ec2InstanceID(self) {
		t.Errorf("a guest naming another instance: got %d %q; want 200 %q, its own", code, body, ec2InstanceID(self))
	}
	if code, body := networkGet(r, n, "/latest/meta-data/instance-id", "127.0.0.1", named); code != http.StatusOK || body != ec2InstanceID(other) {
		t.Errorf("loopback naming an instance: got %d %q; want 200 %q", code, body, ec2InstanceID(other))
	}

	gin.SetMode(gin.TestMode)
	trusting := gin.New()
	svc := NewService(testPool, ":0")
	svc.SetTrustInstanceHeader(true)
	svc.RegisterRoutes(&trusting.RouterGroup)
	if code, body := networkGet(trusting, n, "/latest/meta-data/instance-id", "172.18.0.1", named); code != http.StatusOK || body != ec2InstanceID(other) {
		t.Errorf("trust_instance_header on: got %d %q; want 200 %q", code, body, ec2InstanceID(other))
	}
}
