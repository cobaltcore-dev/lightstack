package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// guestGet issues a metadata request the way a guest's arrives: from its own address,
// through the per-bridge DNAT, with whatever headers it chose to send.
func guestGet(r *gin.Engine, path, peer string, headers map[string]string) (int, string) {
	return guestGetCtx(context.Background(), r, path, peer, headers)
}

// guestGetCtx is guestGet with a caller-supplied request context, so a test that makes a
// table unavailable (dbtest.Unavailable) can give the request a deadline — a statement
// blocked on PostgreSQL's ACCESS EXCLUSIVE lock fails instead of waiting indefinitely.
func guestGetCtx(ctx context.Context, r *gin.Engine, path, peer string, headers map[string]string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
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
		`INSERT INTO ports (project_id, device_id, device_owner, fixed_ips) VALUES ($1, $2, 'compute:nova', $3::jsonb) RETURNING id`,
		seededProjectID, id, fmt.Sprintf(`[{"ip_address":%q}]`, ip)).Scan(&portID); err != nil {
		t.Fatalf("insert port: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM ports WHERE id=$1", portID) })
	return id
}

// When two instances hold one address — distinct device_ids on two ports of the same
// project — the lookup is ambiguous and identifies neither, so the request is 404 rather
// than served one guest's data to the other (upstream answers only when exactly one port
// matches).
func TestAmbiguousAddressIdentifiesNoInstance_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	const addr = "198.51.100.30"
	a := seedInstance(t, nil)
	b := seedInstance(t, nil)
	for _, inst := range []string{a, b} {
		var portID string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO ports (project_id, device_id, device_owner, fixed_ips) VALUES ($1, $2, 'compute:nova', $3::jsonb) RETURNING id`,
			seededProjectID, inst, fmt.Sprintf(`[{"ip_address":%q}]`, addr)).Scan(&portID); err != nil {
			t.Fatalf("seed port for %s: %v", inst, err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM ports WHERE id = $1`, portID) })
	}
	code, body := guestGet(newMetadataTestRouter(), "/latest/meta-data/instance-id", addr, nil)
	if code != http.StatusNotFound {
		t.Errorf("two instances hold %s: got %d %q; want 404, not one of %s/%s", addr, code, body, ec2InstanceID(a), ec2InstanceID(b))
	}
}

// A database failure while identifying a guest answers 500, not a 404 that tells the guest
// it has no instance (SRC-22).
func TestMetadataDatabaseErrorIs500_DB(t *testing.T) {
	requireDB(t)
	seedGuest(t, "198.51.100.40")
	release := dbtest.Unavailable(t, testPool, "ports")
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	code, _ := guestGetCtx(ctx, newMetadataTestRouter(), "/latest/meta-data/instance-id", "198.51.100.40", nil)
	if code != http.StatusInternalServerError {
		t.Errorf("metadata lookup with ports unavailable = %d; want 500, not a 404", code)
	}
}

// A guest cannot read another project's instance by pointing its own port's device_id at
// that instance: a port's device_id is caller-settable, but the port carries the caller's
// project, so the project-scoped join drops it.
func TestGuestCannotClaimAnotherProjectsInstance_DB(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	victim := seedGuest(t, "198.51.100.20") // instance + port in the seeded project

	attacker := uuid.NewString()
	if _, err := testPool.Exec(ctx, `INSERT INTO projects (id, name) VALUES ($1, $2)`,
		attacker, "md-attacker-"+attacker[:8]); err != nil {
		t.Fatalf("seed attacker project: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM projects WHERE id=$1", attacker) })

	var portID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO ports (project_id, device_id, device_owner, fixed_ips) VALUES ($1, $2, 'compute:nova', $3::jsonb) RETURNING id`,
		attacker, victim, fmt.Sprintf(`[{"ip_address":%q}]`, "198.51.100.21")).Scan(&portID); err != nil {
		t.Fatalf("seed attacker port: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM ports WHERE id=$1", portID) })

	code, body := guestGet(newMetadataTestRouter(), "/latest/meta-data/instance-id", "198.51.100.21", nil)
	if code != http.StatusNotFound {
		t.Errorf("attacker at .21 pointing its port at the victim: got %d %q; want 404, not the victim's id (%q)",
			code, body, ec2InstanceID(victim))
	}
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

// A guest's address is matched whole: a port holding 198.51.100.50 does not answer for
// 198.51.100.5, whichever order the ports were created in.
func TestGuestAddressIsMatchedWhole_DB(t *testing.T) {
	requireDB(t)
	longer := seedGuest(t, "198.51.100.50")
	r := newMetadataTestRouter()

	if code, body := guestGet(r, "/latest/meta-data/instance-id", "198.51.100.5", nil); code != http.StatusNotFound {
		t.Errorf("no port holds 198.51.100.5: got %d %q; want 404, not the instance holding 198.51.100.50 (%q)",
			code, body, ec2InstanceID(longer))
	}
	self := seedGuest(t, "198.51.100.5")
	if code, body := guestGet(r, "/latest/meta-data/instance-id", "198.51.100.5", nil); code != http.StatusOK || body != ec2InstanceID(self) {
		t.Errorf("got %d %q; want 200 %q", code, body, ec2InstanceID(self))
	}
}
