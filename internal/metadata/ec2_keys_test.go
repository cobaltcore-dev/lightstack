package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// EC2 metadata key coverage — found during SPEC-024's real-mode host pass.
//
// A cirros guest booted, got its address, got the metadata route, and reached the service — then
// printed six `failed to get http://169.254.169.254/2009-04-04/meta-data/...` lines. GetEC2MetaData
// answered only hostname, instance-id, instance-type, local-ipv4 and public-keys/, so every other key
// the standard datasource asks for 404'd.
//
// This is NOT cloud-storage#546's F10: the network path was proven working (the TAP is enslaved to the
// bridge in the root namespace, DHCP delivers option 121, the DNAT rule is present). The ticket
// predicted metadata reachability "rides on" F10, and it does — but the console noise it cited has a
// second, unrelated cause, which is what this file covers.
//
// Every key here is one the 2009-04-04 datasource requests on a normal boot.

var metaTestDBAvailable bool

func TestMain(m *testing.M) {
	if dbURL := os.Getenv("O3K_TEST_DB_URL"); dbURL != "" {
		ctx := context.Background()
		if err := database.Connect(ctx, dbURL, &database.PoolConfig{
			MaxConns: 5, MinConns: 1, MaxConnLifetime: time.Hour,
			MaxConnIdleTime: 15 * time.Minute, HealthCheckPeriod: time.Minute,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "O3K_TEST_DB_URL set but connect failed: %v\n", err)
			os.Exit(1)
		}
		if err := database.MigrateUp(dbURL, "../../migrations"); err != nil {
			fmt.Fprintf(os.Stderr, "O3K_TEST_DB_URL set but migrate failed: %v\n", err)
			database.Close()
			os.Exit(1)
		}
		metaTestDBAvailable = true
	}
	code := m.Run()
	if metaTestDBAvailable {
		database.Close()
	}
	os.Exit(code)
}

func requireMetaDB(t *testing.T) {
	t.Helper()
	if !metaTestDBAvailable {
		t.Skip("set O3K_TEST_DB_URL to run DB-backed metadata tests")
	}
}

const metaProjectID = "00000000-0000-0000-0000-000000000002"

// seedInstanceWithPort creates an instance plus the network/subnet/port that give it a fixed IP, so
// instanceFromRequest can resolve it from the request's source address — the way a real guest is
// identified.
func seedInstanceWithPort(t *testing.T, name, fixedIP string) string {
	t.Helper()
	ctx := context.Background()

	var flavorID string
	if err := database.DB.QueryRow(ctx, "SELECT id FROM flavors LIMIT 1").Scan(&flavorID); err != nil {
		t.Skipf("no seeded flavor to attach: %v", err)
	}

	netID, subnetID, portID, instID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := database.DB.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared) VALUES ($1, $2, $3, false)`,
		netID, "ec2meta-"+netID[:8], metaProjectID); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	if _, err := database.DB.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp)
		 VALUES ($1, $2, $3, $4, '10.90.0.0/24', '10.90.0.1', 4, true)`,
		subnetID, "ec2meta-sub", netID, metaProjectID); err != nil {
		t.Fatalf("seed subnet: %v", err)
	}
	if _, err := database.DB.Exec(ctx,
		// user_id is set because Nova always sets it (handlers.go INSERT INTO instances), and
		// publicKeysForInstance resolves an instance's keys THROUGH its user. Leaving it NULL built
		// an instance in a state the product never creates, and #549's fail-closed key read then
		// answered public-keys/ with a 500 — which the root-listing walk correctly reports as a key
		// that is advertised but does not serve.
		`INSERT INTO instances (id, name, project_id, user_id, flavor_id, status)
		 VALUES ($1, $2, $3, (SELECT id FROM users ORDER BY created_at LIMIT 1), $4, 'ACTIVE')`,
		instID, name, metaProjectID, flavorID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	if _, err := database.DB.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, device_id, mac_address, admin_state_up, status, fixed_ips)
		 VALUES ($1, 'ec2meta-port', $2, $3, $4, '52:54:00:12:34:56', true, 'ACTIVE', $5)`,
		portID, netID, metaProjectID, instID,
		fmt.Sprintf(`[{"subnet_id":%q,"ip_address":%q}]`, subnetID, fixedIP)); err != nil {
		t.Fatalf("seed port: %v", err)
	}

	t.Cleanup(func() {
		database.DB.Exec(ctx, "DELETE FROM floating_ips WHERE port_id = $1", portID) //nolint:errcheck // best-effort
		database.DB.Exec(ctx, "DELETE FROM ports WHERE id = $1", portID)             //nolint:errcheck // best-effort
		database.DB.Exec(ctx, "DELETE FROM instances WHERE id = $1", instID)         //nolint:errcheck // best-effort
		database.DB.Exec(ctx, "DELETE FROM subnets WHERE id = $1", subnetID)         //nolint:errcheck // best-effort
		database.DB.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID)           //nolint:errcheck // best-effort
	})
	return portID
}

// getEC2 issues a request as if it came from the guest at fixedIP.
func getEC2(t *testing.T, key, fixedIP string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewService("stub").RegisterRoutes(&r.RouterGroup)

	req := httptest.NewRequest(http.MethodGet, "/2009-04-04/meta-data/"+key, nil)
	req.RemoteAddr = fixedIP + ":12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestEC2KeysCirrosRequestsOnEveryBoot — the six that 404'd on the host pass.
func TestEC2KeysCirrosRequestsOnEveryBoot(t *testing.T) {
	requireMetaDB(t)
	const ip = "10.90.0.55"
	seedInstanceWithPort(t, "ec2meta-guest", ip)

	for _, tc := range []struct {
		key  string
		want string
	}{
		{"ami-launch-index", "0"},
		{"local-hostname", "ec2meta-guest"},
		{"public-hostname", "ec2meta-guest"},
		{"placement/availability-zone", "nova"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			code, body := getEC2(t, tc.key, ip)
			if code != http.StatusOK {
				t.Fatalf("%s: code=%d body=%q; want 200 — a standard datasource asks for this on every boot", tc.key, code, body)
			}
			if strings.TrimSpace(body) != tc.want {
				t.Errorf("%s = %q, want %q", tc.key, body, tc.want)
			}
		})
	}
}

// TestEC2BlockDeviceMapping: the datasource lists the directory, then reads its entries.
func TestEC2BlockDeviceMapping(t *testing.T) {
	requireMetaDB(t)
	const ip = "10.90.0.56"
	seedInstanceWithPort(t, "ec2meta-bdm", ip)

	code, body := getEC2(t, "block-device-mapping", ip)
	if code != http.StatusOK {
		t.Fatalf("block-device-mapping: code=%d body=%q; want 200", code, body)
	}
	for _, entry := range []string{"ami", "root"} {
		if !strings.Contains(body, entry) {
			t.Errorf("listing %q is missing %q", body, entry)
		}
	}

	for _, k := range []string{"block-device-mapping/ami", "block-device-mapping/root"} {
		code, body := getEC2(t, k, ip)
		if code != http.StatusOK || strings.TrimSpace(body) == "" {
			t.Errorf("%s: code=%d body=%q; want 200 with a device name", k, code, body)
		}
	}
}

// TestEC2PublicIPv4 reports the floating IP when one is associated, and 404s when none is — the
// distinction upstream makes, and the reason it cannot simply mirror local-ipv4.
func TestEC2PublicIPv4(t *testing.T) {
	requireMetaDB(t)
	ctx := context.Background()
	const ip = "10.90.0.57"
	portID := seedInstanceWithPort(t, "ec2meta-fip", ip)

	if code, _ := getEC2(t, "public-ipv4", ip); code != http.StatusNotFound {
		t.Errorf("with no floating IP: code=%d, want 404", code)
	}

	if _, err := database.DB.Exec(ctx,
		`INSERT INTO floating_ips (id, project_id, floating_ip_address, fixed_ip_address, port_id, status)
		 VALUES ($1, $2, '192.0.2.77', $3, $4, 'ACTIVE')`,
		uuid.NewString(), metaProjectID, ip, portID); err != nil {
		t.Skipf("cannot seed floating ip: %v", err)
	}

	code, body := getEC2(t, "public-ipv4", ip)
	if code != http.StatusOK {
		t.Fatalf("with a floating IP: code=%d body=%q; want 200", code, body)
	}
	if strings.TrimSpace(body) != "192.0.2.77" {
		t.Errorf("public-ipv4 = %q, want 192.0.2.77", body)
	}
}

// TestEC2RootListingAdvertisesEveryServedKey: the directory listing is how a datasource discovers
// what exists. Advertising a key it cannot serve, or serving one it does not advertise, both mislead.
//
// This used to restate the ten expected names as a third hard-coded copy — alongside the handler's
// listing and the tree itself — and assert only that each appeared somewhere in the response. It
// never fetched them, so it could not catch either half of the invariant its name claims: a listed
// key that 404s passed, and a key served but never listed passed. It walks the listing now and
// fetches what it finds.
func TestEC2RootListingAdvertisesEveryServedKey(t *testing.T) {
	requireMetaDB(t)
	const ip = "10.90.0.58"
	seedInstanceWithPort(t, "ec2meta-listing", ip)

	code, listing := getEC2(t, "", ip)
	if code != http.StatusOK {
		t.Fatalf("root listing: code=%d", code)
	}

	// Every advertised entry must actually answer. A trailing slash marks a subtree, and the
	// listing for one is itself a valid response, so both shapes are fetched the same way.
	entries := strings.Split(strings.TrimSpace(listing), "\n")
	if len(entries) == 0 || entries[0] == "" {
		t.Fatalf("root listing is empty")
	}
	for _, entry := range entries {
		t.Run(entry, func(t *testing.T) {
			if code, body := getEC2(t, entry, ip); code != http.StatusOK {
				t.Errorf("listing advertises %q but GET returns %d (body %q) — a datasource treats "+
					"that as a failed boot step", entry, code, body)
			}
		})
	}

	// And the keys a normal boot depends on are present. Kept as an explicit floor so shrinking the
	// tree to nothing cannot pass the walk above vacuously.
	for _, want := range []string{
		"ami-launch-index", "block-device-mapping/", "hostname", "instance-id", "instance-type",
		"local-hostname", "local-ipv4", "placement/", "public-hostname", "public-keys/",
	} {
		if !strings.Contains(listing, want) {
			t.Errorf("root listing omits %q; got:\n%s", want, listing)
		}
	}
}

// TestEC2UnknownKeyStill404s — the fix must not turn the tree into a catch-all.
func TestEC2UnknownKeyStill404s(t *testing.T) {
	requireMetaDB(t)
	const ip = "10.90.0.59"
	seedInstanceWithPort(t, "ec2meta-unknown", ip)

	if code, body := getEC2(t, "not-a-real-key", ip); code != http.StatusNotFound {
		t.Errorf("unknown key: code=%d body=%q; want 404", code, body)
	}
}
