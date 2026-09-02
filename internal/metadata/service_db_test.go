package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDBAvailable is true when LIGHTSTACK_TEST_DB_URL is set and migrated; DB-backed
// metadata tests skip otherwise (Constitution: real dependencies over mocks). Mirrors
// internal/glance/images_db_test.go.
var testDBAvailable bool

// testPool is this package's test pool: opened by TestMain when LIGHTSTACK_TEST_DB_URL
// is set, and handed to the services the tests construct (SPEC-027).
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if dbURL := os.Getenv("LIGHTSTACK_TEST_DB_URL"); dbURL != "" {
		ctx := context.Background()
		var err error
		if testPool, err = database.Connect(ctx, dbURL, &database.PoolConfig{
			MaxConns: 5, MinConns: 1, MaxConnLifetime: time.Hour,
			MaxConnIdleTime: 15 * time.Minute, HealthCheckPeriod: time.Minute,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "LIGHTSTACK_TEST_DB_URL is set but connecting failed: %v\n", err)
			os.Exit(1)
		}
		if err := database.MigrateUp(dbURL, "../../migrations"); err != nil {
			fmt.Fprintf(os.Stderr, "LIGHTSTACK_TEST_DB_URL is set but migrations failed: %v\n", err)
			testPool.Close()
			os.Exit(1)
		}
		testDBAvailable = true
	}
	code := m.Run()
	if testDBAvailable {
		// pgxpool.Close blocks until every acquired connection is released, so a handler
		// that leaks rows would hang the run here with no diagnostic. Fail loudly instead.
		if n := testPool.Stat().AcquiredConns(); n != 0 {
			fmt.Fprintf(os.Stderr, "connection leak: %d pool connection(s) still acquired after the tests — a handler did not close its rows\n", n)
			os.Exit(1)
		}
		testPool.Close()
	}
	os.Exit(code)
}

func requireDB(t *testing.T) {
	t.Helper()
	if !testDBAvailable {
		t.Skip("set LIGHTSTACK_TEST_DB_URL to run DB-backed metadata tests")
	}
}

// Seeded scope from migration 002_seed_data.
const (
	seededProjectID = "00000000-0000-0000-0000-000000000002"
	seededUserID    = "00000000-0000-0000-0000-000000000001"
)

func newMetadataTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	NewService(testPool, ":0").RegisterRoutes(&r.RouterGroup)
	return r
}

// metaGet issues a metadata request identified by the X-Instance-ID test header — the
// same hook test/contract/ws4_userdata_test.go uses, so no source-IP lookup is needed.
// It comes from loopback, as a test run on the host does: the only callers the header
// is believed from by default.
func metaGet(r *gin.Engine, path, instanceID string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:40000"
	req.Header.Set("X-Instance-ID", instanceID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// seedKeypair inserts a keypair for the seeded user and removes it on cleanup. Names are
// suffixed with a uuid because keypairs is UNIQUE(user_id, name) and the test database
// is shared.
func seedKeypair(t *testing.T, prefix string) (name, pubkey string) {
	t.Helper()
	ctx := context.Background()
	name = prefix + "-" + uuid.NewString()[:8]
	pubkey = "ssh-ed25519 AAAA" + uuid.NewString() + " " + name
	if _, err := testPool.Exec(ctx,
		`INSERT INTO keypairs (name, user_id, public_key, fingerprint) VALUES ($1,$2,$3,'00:11')`,
		name, seededUserID, pubkey); err != nil {
		t.Fatalf("insert keypair %s: %v", name, err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM keypairs WHERE user_id=$1 AND name=$2", seededUserID, name) })
	return name, pubkey
}

// seedInstance inserts an instance owned by the seeded user with the given key_name
// (nil → NULL, i.e. booted without a keypair) and removes it on cleanup.
func seedInstance(t *testing.T, keyName *string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO instances (id, name, project_id, user_id, flavor_id, status, key_name)
		 VALUES ($1,'md-inst',$2,$3,(SELECT id FROM flavors WHERE name = 'm1.small'),'ACTIVE',$4)`,
		id, seededProjectID, seededUserID, keyName); err != nil {
		t.Fatalf("insert instance: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM instances WHERE id=$1", id) })
	return id
}

// TestMetaDataJSON_PublicKeysScopedToBootKey_DB: the owner holds two keypairs and the
// instance was booted with one of them. public_keys must carry exactly that one —
// upstream Nova serves the boot keypair alone (nova/api/metadata/base.py), never every
// key the owner happens to hold. The previous `WHERE user_id = $1` returned both.
func TestMetaDataJSON_PublicKeysScopedToBootKey_DB(t *testing.T) {
	requireDB(t)
	bootName, bootKey := seedKeypair(t, "md-boot")
	otherName, _ := seedKeypair(t, "md-other")
	instID := seedInstance(t, &bootName)

	code, body := metaGet(newMetadataTestRouter(), "/openstack/latest/meta_data.json", instID)
	if code != http.StatusOK {
		t.Fatalf("meta_data.json: code=%d body=%s", code, body)
	}
	var md struct {
		PublicKeys map[string]string `json:"public_keys"`
	}
	if err := json.Unmarshal([]byte(body), &md); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if got := md.PublicKeys[bootName]; got != bootKey {
		t.Errorf("boot key %s: got %q want %q", bootName, got, bootKey)
	}
	if _, leaked := md.PublicKeys[otherName]; leaked {
		t.Errorf("public_keys leaked the owner's other keypair %s: %v", otherName, md.PublicKeys)
	}
	if len(md.PublicKeys) != 1 {
		t.Errorf("public_keys should carry exactly the boot key, got %d: %v", len(md.PublicKeys), md.PublicKeys)
	}
}

// TestMetaDataJSON_NoKeyName_DB: an instance booted without a keypair gets no keys,
// even though its owner holds some (upstream omits public_keys entirely in that case;
// the response keeps the empty object it already defaults to).
func TestMetaDataJSON_NoKeyName_DB(t *testing.T) {
	requireDB(t)
	seedKeypair(t, "md-unused")
	instID := seedInstance(t, nil)

	code, body := metaGet(newMetadataTestRouter(), "/openstack/latest/meta_data.json", instID)
	if code != http.StatusOK {
		t.Fatalf("meta_data.json: code=%d body=%s", code, body)
	}
	var md struct {
		PublicKeys map[string]string `json:"public_keys"`
	}
	if err := json.Unmarshal([]byte(body), &md); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if len(md.PublicKeys) != 0 {
		t.Errorf("instance without key_name must get no keys, got %v", md.PublicKeys)
	}
}

// TestMetaDataJSON_ServesServerMetadata_DB: the server's metadata reaches the guest as
// meta_data.json's `meta`, as upstream serves it. The query named columns that do not
// exist (`key`, `value` — the table has meta_key and meta_value), and its error was
// swallowed, so every guest got an empty `meta`.
func TestMetaDataJSON_ServesServerMetadata_DB(t *testing.T) {
	requireDB(t)
	instID := seedInstance(t, nil)
	for k, v := range map[string]string{"role": "web", "tier": "1"} {
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO instance_metadata (instance_id, meta_key, meta_value) VALUES ($1,$2,$3)`,
			instID, k, v); err != nil {
			t.Fatalf("insert metadata %s: %v", k, err)
		}
	}

	code, body := metaGet(newMetadataTestRouter(), "/openstack/latest/meta_data.json", instID)
	if code != http.StatusOK {
		t.Fatalf("meta_data.json: code=%d body=%s", code, body)
	}
	var md struct {
		Meta map[string]string `json:"meta"`
	}
	if err := json.Unmarshal([]byte(body), &md); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if len(md.Meta) != 2 || md.Meta["role"] != "web" || md.Meta["tier"] != "1" {
		t.Errorf("meta = %v, want map[role:web tier:1]", md.Meta)
	}
}

// TestEC2PublicKeys_ScopedToBootKey_DB covers the EC2 tree cirros reads: the listing
// names only the boot key at index 0, index 0 serves it, and there is no index 1.
func TestEC2PublicKeys_ScopedToBootKey_DB(t *testing.T) {
	requireDB(t)
	bootName, bootKey := seedKeypair(t, "ec2-boot")
	seedKeypair(t, "ec2-other")
	instID := seedInstance(t, &bootName)
	r := newMetadataTestRouter()

	code, body := metaGet(r, "/2009-04-04/meta-data/public-keys/", instID)
	if code != http.StatusOK || body != "0="+bootName {
		t.Errorf("public-keys listing: code=%d body=%q want %q", code, body, "0="+bootName)
	}
	code, body = metaGet(r, "/2009-04-04/meta-data/public-keys/0/openssh-key", instID)
	if code != http.StatusOK || body != bootKey {
		t.Errorf("public-keys/0/openssh-key: code=%d body=%q want the boot key", code, body)
	}
	code, _ = metaGet(r, "/2009-04-04/meta-data/public-keys/1/openssh-key", instID)
	if code != http.StatusNotFound {
		t.Errorf("public-keys/1/openssh-key: code=%d want 404 (the owner's other key must not be served)", code)
	}
}

// TestEC2PublicKeys_NoKeyName_DB: no boot keypair → empty listing and no index 0.
func TestEC2PublicKeys_NoKeyName_DB(t *testing.T) {
	requireDB(t)
	seedKeypair(t, "ec2-unused")
	instID := seedInstance(t, nil)
	r := newMetadataTestRouter()

	code, body := metaGet(r, "/2009-04-04/meta-data/public-keys/", instID)
	if code != http.StatusOK || body != "" {
		t.Errorf("public-keys listing without key_name: code=%d body=%q want empty 200", code, body)
	}
	code, _ = metaGet(r, "/2009-04-04/meta-data/public-keys/0/openssh-key", instID)
	if code != http.StatusNotFound {
		t.Errorf("public-keys/0/openssh-key without key_name: code=%d want 404", code)
	}
}
