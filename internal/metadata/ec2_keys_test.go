package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// seedInstanceWithPort creates an instance plus the network/subnet/port that give it a fixed IP, so
// instanceFromRequest can resolve it from the request's source address — the way a real guest is
// identified.
func seedInstanceWithPort(t *testing.T, name, fixedIP string) string {
	t.Helper()
	ctx := context.Background()

	var flavorID string
	if err := testPool.QueryRow(ctx, "SELECT id FROM flavors LIMIT 1").Scan(&flavorID); err != nil {
		t.Skipf("no seeded flavor to attach: %v", err)
	}

	netID, subnetID, portID, instID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO networks (id, name, project_id, shared) VALUES ($1, $2, $3, false)`,
		netID, "ec2meta-"+netID[:8], seededProjectID); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp)
		 VALUES ($1, $2, $3, $4, '10.90.0.0/24', '10.90.0.1', 4, true)`,
		subnetID, "ec2meta-sub", netID, seededProjectID); err != nil {
		t.Fatalf("seed subnet: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		// user_id is set because Nova always sets it (handlers.go INSERT INTO instances), and
		// publicKeysForInstance resolves an instance's keys THROUGH its user. Leaving it NULL built
		// an instance in a state the product never creates, and #549's fail-closed key read then
		// answered public-keys/ with a 500 — which the root-listing walk correctly reports as a key
		// that is advertised but does not serve.
		`INSERT INTO instances (id, name, project_id, user_id, flavor_id, status)
		 VALUES ($1, $2, $3, (SELECT id FROM users ORDER BY created_at LIMIT 1), $4, 'ACTIVE')`,
		instID, name, seededProjectID, flavorID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO ports (id, name, network_id, project_id, device_id, mac_address, admin_state_up, status, fixed_ips)
		 VALUES ($1, 'ec2meta-port', $2, $3, $4, '52:54:00:12:34:56', true, 'ACTIVE', $5)`,
		portID, netID, seededProjectID, instID,
		fmt.Sprintf(`[{"subnet_id":%q,"ip_address":%q}]`, subnetID, fixedIP)); err != nil {
		t.Fatalf("seed port: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM floating_ips WHERE port_id = $1", portID) //nolint:errcheck // best-effort
		testPool.Exec(ctx, "DELETE FROM ports WHERE id = $1", portID)             //nolint:errcheck // best-effort
		testPool.Exec(ctx, "DELETE FROM instances WHERE id = $1", instID)         //nolint:errcheck // best-effort
		testPool.Exec(ctx, "DELETE FROM subnets WHERE id = $1", subnetID)         //nolint:errcheck // best-effort
		testPool.Exec(ctx, "DELETE FROM networks WHERE id = $1", netID)           //nolint:errcheck // best-effort
	})
	return portID
}

// getEC2 issues a request as if it came from the guest at fixedIP.
func getEC2(t *testing.T, key, fixedIP string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewService(testPool, "stub").RegisterRoutes(&r.RouterGroup)

	req := httptest.NewRequest(http.MethodGet, "/2009-04-04/meta-data/"+key, nil)
	req.RemoteAddr = fixedIP + ":12345"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// TestEC2KeysCirrosRequestsOnEveryBoot — the six that 404'd on the host pass.
func TestEC2KeysCirrosRequestsOnEveryBoot(t *testing.T) {
	requireDB(t)
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
	requireDB(t)
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
	requireDB(t)
	ctx := context.Background()
	const ip = "10.90.0.57"
	portID := seedInstanceWithPort(t, "ec2meta-fip", ip)

	if code, _ := getEC2(t, "public-ipv4", ip); code != http.StatusNotFound {
		t.Errorf("with no floating IP: code=%d, want 404", code)
	}

	if _, err := testPool.Exec(ctx,
		`INSERT INTO floating_ips (id, project_id, floating_ip_address, fixed_ip_address, port_id, status)
		 VALUES ($1, $2, '192.0.2.77', $3, $4, 'ACTIVE')`,
		uuid.NewString(), seededProjectID, ip, portID); err != nil {
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
	requireDB(t)
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
	requireDB(t)
	const ip = "10.90.0.59"
	seedInstanceWithPort(t, "ec2meta-unknown", ip)

	if code, body := getEC2(t, "not-a-real-key", ip); code != http.StatusNotFound {
		t.Errorf("unknown key: code=%d body=%q; want 404", code, body)
	}
}

// Branch review 2026-08-28, SRC-6 — a malformed public-key index must be refused without a database
// round-trip.
//
// ec2PublicKey used to resolve the instance's keys and only then check whether the index parsed, so
// `public-keys/garbage` cost one query per request. This service is unauthenticated by design — EC2
// compatibility puts it on the link-local address for any guest to call — so that is an unbounded
// path from a guest to the shared connection pool.
//
// HOW THIS DISCRIMINATES. Asserting 404 for a malformed index against an unknown source address does
// NOT work: the request 404s at instance resolution and never reaches this handler, so both orderings
// pass and the test cannot fail. Verified by mutation before this version was written.
//
// Instead the request names an instance id that does not exist, via the X-Instance-ID header the
// service already honours for tests. Resolution then succeeds, the handler IS reached, and the two
// orderings diverge: querying first hits `SELECT user_id FROM instances` with no matching row, which
// is an error and a 500; parsing first returns 404 without ever asking. The status code is the
// observation — 500 means the lookup ran.
func TestEC2PublicKeyRejectsAMalformedIndexBeforeAnyLookup(t *testing.T) {
	requireDB(t)
	absent := uuid.NewString() // resolvable as an id, present in no row

	for _, key := range []string{
		"public-keys/garbage",
		"public-keys/garbage/openssh-key",
		"public-keys/-1",
	} {
		t.Run(key, func(t *testing.T) {
			code, body := getEC2AsInstance(t, key, absent)
			if code != http.StatusNotFound {
				t.Errorf("GET %s = %d (%q); want 404 — a malformed index must be refused before the "+
					"key lookup, and a 500 here means the lookup ran", key, code, body)
			}
		})
	}
}

// getEC2AsInstance issues a request naming an instance directly, bypassing source-IP resolution.
func getEC2AsInstance(t *testing.T, key, instanceID string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewService(testPool, "stub").RegisterRoutes(&r.RouterGroup)

	req := httptest.NewRequest(http.MethodGet, "/2009-04-04/meta-data/"+key, nil)
	req.Header.Set("X-Instance-ID", instanceID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// The valid path still works, so the reordering did not turn a good index into a refusal. The upper
// bound is the one arm that genuinely needs the query, and it is checked after it.
func TestEC2PublicKeyStillServesAValidIndex(t *testing.T) {
	requireDB(t)
	const ip, instName = "10.90.0.77", "ec2meta-keyidx"
	seedInstanceWithPort(t, instName, ip)

	// publicKeysForInstance resolves keys through the instance's BOOT key, not through every key
	// its owner holds: it reads instances.key_name and serves only the owner's key of that name
	// (pinned by TestEC2PublicKeys_ScopedToBootKey_DB). So the positive case needs both a keypair
	// owned by the instance's user and the instance naming it — with either missing, index 0 is
	// already past the end. The keypair is removed again so no other test in this package sees it;
	// the instance carrying key_name goes with seedInstanceWithPort's own cleanup.
	kpID := uuid.NewString()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), //nolint:errcheck // best-effort cleanup
			"DELETE FROM keypairs WHERE id = $1", kpID)
	})
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO keypairs (id, name, user_id, public_key)
		 VALUES ($1, 'ec2meta-key', (SELECT user_id FROM instances WHERE name = $2), 'ssh-ed25519 AAAAC3Nz')`,
		kpID, instName); err != nil {
		t.Fatalf("seed keypair: %v", err)
	}
	if _, err := testPool.Exec(context.Background(),
		`UPDATE instances SET key_name = 'ec2meta-key' WHERE name = $1`, instName); err != nil {
		t.Fatalf("name the boot key: %v", err)
	}

	if code, body := getEC2(t, "public-keys/0", ip); code != http.StatusOK || body != "openssh-key" {
		t.Errorf("GET public-keys/0 = %d %q; want 200 %q", code, body, "openssh-key")
	}
	// One past the end still needs `keys` to decide, so this is the arm left after the query.
	if code, _ := getEC2(t, "public-keys/99", ip); code != http.StatusNotFound {
		t.Errorf("GET public-keys/99 = %d; want 404", code)
	}
}
