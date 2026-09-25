package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// guestGet issues a metadata request the way a guest's arrives: from its own address,
// through the per-bridge DNAT, with whatever headers it chose to send.
func guestGet(r *gin.Engine, path, peer string, headers map[string]string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = peer + ":40000"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// seedGuest inserts an instance with one port holding ip, as a booted guest has, and
// removes both on cleanup. The addresses come from TEST-NET-2 so they cannot match a
// port another package's tests create in the shared database.
func seedGuest(t *testing.T, ip string) string {
	t.Helper()
	id := seedInstance(t, nil)
	ctx := context.Background()
	var portID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO ports (device_id, device_owner, fixed_ips) VALUES ($1, 'compute:nova', $2::jsonb) RETURNING id`,
		id, fmt.Sprintf(`[{"ip_address":%q}]`, ip)).Scan(&portID); err != nil {
		t.Fatalf("insert port: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM ports WHERE id=$1", portID) })
	return id
}

// A guest is identified by where its request came from. An X-Instance-ID header naming
// another instance used to be believed from any caller, so any guest could read any
// instance's metadata and user-data by its UUID.
func TestGuestCannotNameAnotherInstance_DB(t *testing.T) {
	requireDB(t)
	self := seedGuest(t, "198.51.100.10")
	other := seedGuest(t, "198.51.100.11")

	code, body := guestGet(newMetadataTestRouter(), "/latest/meta-data/instance-id", "198.51.100.10",
		map[string]string{"X-Instance-ID": other})
	if code != http.StatusOK || body != ec2InstanceID(self) {
		t.Errorf("got %d %q; want 200 %q, the caller's own id, not the named instance's %q",
			code, body, ec2InstanceID(self), ec2InstanceID(other))
	}
}

// X-Forwarded-For does not choose the instance either. gin's ClientIP believes it from
// any caller unless trusted proxies are set, so a guest could answer as any fixed IP it
// could name, and fixed IPs are easy to enumerate.
func TestGuestCannotForwardForAnotherAddress_DB(t *testing.T) {
	requireDB(t)
	self := seedGuest(t, "198.51.100.10")
	other := seedGuest(t, "198.51.100.11")

	code, body := guestGet(newMetadataTestRouter(), "/latest/meta-data/instance-id", "198.51.100.10",
		map[string]string{"X-Forwarded-For": "198.51.100.11"})
	if code != http.StatusOK || body != ec2InstanceID(self) {
		t.Errorf("got %d %q; want 200 %q, the caller's own id, not the forwarded address's %q",
			code, body, ec2InstanceID(self), ec2InstanceID(other))
	}
}

// metadata.trust_instance_header is the one way to have the header believed from a caller
// that is not loopback: a test stack whose requests arrive from a Docker bridge gateway.
func TestInstanceHeaderBelievedWhenConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(testPool, ":0")
	svc.SetTrustInstanceHeader(true)
	svc.RegisterRoutes(&r.RouterGroup)

	const id = "755a18e9-eb39-40f6-8208-267410268522"
	code, body := guestGet(r, "/latest/meta-data/instance-id", "172.18.0.1",
		map[string]string{"X-Instance-ID": id})
	if code != http.StatusOK || body != "i-755a18e9eb3940f68" {
		t.Errorf("got %d %q; want 200 %q", code, body, "i-755a18e9eb3940f68")
	}
}

// local-ipv4 is the caller's own address as well, whatever it forwards for.
func TestLocalIPv4IsTheCallersAddress_DB(t *testing.T) {
	requireDB(t)
	seedGuest(t, "198.51.100.10")
	seedGuest(t, "198.51.100.11")

	code, body := guestGet(newMetadataTestRouter(), "/latest/meta-data/local-ipv4", "198.51.100.10",
		map[string]string{"X-Forwarded-For": "198.51.100.11"})
	if code != http.StatusOK || body != "198.51.100.10" {
		t.Errorf("got %d %q; want 200 %q", code, body, "198.51.100.10")
	}
}
