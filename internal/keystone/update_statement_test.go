package keystone

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/google/uuid"
)

// Branch review 2026-08-28, SRC-1 — the five identity Update* handlers the SQL refactor rewrote.
//
// `refactor(sql)` converted twenty Update* handlers to common.SQLUpdate; fifteen had no test at all,
// five of them here. See internal/neutron/update_statement_test.go for the shape argument in full.
//
// In short: each test sends SEVERAL fields in one request, every one carrying a value distinguishable
// from the others, then reads each column back separately. That catches a value assigned to the wrong
// column, a field the handler ignores, and a SET wider than the request — all of which are silent.
//
// It does NOT catch a misnumbered placeholder, because SQLUpdate makes that unrepresentable: the
// placeholder is produced by appending the value it reads, so the two cannot drift. Reordering the
// assignments is a no-op (verified by mutation). That bug class is closed by construction, not by
// these tests.

// requireIdentityDB skips when no migrated test database is configured. The identity tests inline
// this check; it is a helper here only because five tests need it.
func requireIdentityDB(t *testing.T) {
	t.Helper()
	if !testDBAvailable {
		t.Skip("set O3K_TEST_DB_URL to run DB-backed identity tests")
	}
}

// mustExecIdentity runs a fixture statement and fails the test on error.
func mustExecIdentity(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := database.DB.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture %q: %v", sql, err)
	}
}

// cleanupIdentityRow removes a fixture row when the test ends.
func cleanupIdentityRow(t *testing.T, table, id string) {
	t.Helper()
	t.Cleanup(func() {
		database.DB.Exec(context.Background(), //nolint:errcheck // best-effort cleanup
			fmt.Sprintf("DELETE FROM %s WHERE id = $1", table), id)
	})
}

func TestUpdateDomainAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireIdentityDB(t)
	r := newIdentityTestRouter()

	id := uuid.NewString()
	cleanupIdentityRow(t, "domains", id)
	mustExecIdentity(t, `INSERT INTO domains (id, name, description, enabled)
		VALUES ($1, $2, 'before-description', true)`, id, "src1-domain-"+id[:8])

	code, body := patchJSON(t, r, "/v3/domains/"+id,
		`{"domain":{"name":"after-name","description":"after-description","enabled":false}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH domain: code=%d body=%v; want 200", code, body)
	}

	var name, description string
	var enabled bool
	if err := database.DB.QueryRow(context.Background(),
		"SELECT name, description, enabled FROM domains WHERE id = $1",
		id).Scan(&name, &description, &enabled); err != nil {
		t.Fatalf("read back domain: %v", err)
	}
	if name != "after-name" {
		t.Errorf("domains.name = %q, want %q", name, "after-name")
	}
	if description != "after-description" {
		t.Errorf("domains.description = %q, want %q — holding the name's value would be a "+
			"wrong-column assignment", description, "after-description")
	}
	if enabled {
		t.Errorf("domains.enabled = true, want false")
	}
}

func TestUpdateGroupAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireIdentityDB(t)
	r := newIdentityTestRouter()

	id := uuid.NewString()
	cleanupIdentityRow(t, "groups", id)
	mustExecIdentity(t, `INSERT INTO groups (id, name, description)
		VALUES ($1, $2, 'before-description')`, id, "src1-group-"+id[:8])

	code, body := patchJSON(t, r, "/v3/groups/"+id,
		`{"group":{"name":"after-name","description":"after-description"}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH group: code=%d body=%v; want 200", code, body)
	}

	var name, description string
	if err := database.DB.QueryRow(context.Background(),
		"SELECT name, description FROM groups WHERE id = $1", id).Scan(&name, &description); err != nil {
		t.Fatalf("read back group: %v", err)
	}
	if name != "after-name" {
		t.Errorf("groups.name = %q, want %q", name, "after-name")
	}
	if description != "after-description" {
		t.Errorf("groups.description = %q, want %q", description, "after-description")
	}
}

func TestUpdateServiceAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireIdentityDB(t)
	r := newIdentityTestRouter()

	id := uuid.NewString()
	cleanupIdentityRow(t, "services", id)
	mustExecIdentity(t, `INSERT INTO services (id, type, name, description, enabled)
		VALUES ($1, 'src1-type', 'before-name', 'before-description', true)`, id)

	code, body := patchJSON(t, r, "/v3/services/"+id,
		`{"service":{"name":"after-name","description":"after-description","enabled":false}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH service: code=%d body=%v; want 200", code, body)
	}

	var svcType, name, description string
	var enabled bool
	if err := database.DB.QueryRow(context.Background(),
		"SELECT type, name, description, enabled FROM services WHERE id = $1",
		id).Scan(&svcType, &name, &description, &enabled); err != nil {
		t.Fatalf("read back service: %v", err)
	}
	if name != "after-name" {
		t.Errorf("services.name = %q, want %q", name, "after-name")
	}
	if description != "after-description" {
		t.Errorf("services.description = %q, want %q", description, "after-description")
	}
	if enabled {
		t.Errorf("services.enabled = true, want false")
	}
	// `type` is not in the request, so it must be untouched — an over-wide SET shows up here.
	if svcType != "src1-type" {
		t.Errorf("services.type = %q, want it unchanged at %q", svcType, "src1-type")
	}
}

// UpdateUser assigns the most columns of the five, and one of them is `password_hash` — the request
// carries a plaintext `password` that the handler must hash before storing. A field landing in the
// wrong column here would put a name or an email where a hash belongs.
func TestUpdateUserAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireIdentityDB(t)
	r := newIdentityTestRouter()

	id := uuid.NewString()
	cleanupIdentityRow(t, "users", id)
	mustExecIdentity(t, `INSERT INTO users (id, name, email, description, enabled, password_hash)
		VALUES ($1, $2, 'before@example.invalid', 'before-description', true, 'before-hash')`,
		id, "src1-user-"+id[:8])

	code, body := patchJSON(t, r, "/v3/users/"+id,
		`{"user":{"name":"after-name","email":"after@example.invalid","description":"after-description","enabled":false}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH user: code=%d body=%v; want 200", code, body)
	}

	var name, email, description, hash string
	var enabled bool
	if err := database.DB.QueryRow(context.Background(),
		"SELECT name, email, description, enabled, password_hash FROM users WHERE id = $1",
		id).Scan(&name, &email, &description, &enabled, &hash); err != nil {
		t.Fatalf("read back user: %v", err)
	}
	for _, c := range []struct{ col, got, want string }{
		{"name", name, "after-name"},
		{"email", email, "after@example.invalid"},
		{"description", description, "after-description"},
	} {
		if c.got != c.want {
			t.Errorf("users.%s = %q, want %q", c.col, c.got, c.want)
		}
	}
	if enabled {
		t.Errorf("users.enabled = true, want false")
	}
	// No password was sent, so the stored hash must be untouched. This is the assertion that would
	// catch another field's value being written into the credential column.
	if hash != "before-hash" {
		t.Errorf("users.password_hash = %q, want it unchanged — a request that sent no password must "+
			"not rewrite the credential", hash)
	}
}

// UpdateCredential assigns one caller-supplied column, so there is nothing to swap it with except the
// WHERE argument. Two rows are seeded and both read back: a swapped id predicate writes the wrong one.
func TestUpdateCredentialWritesOnlyTheAddressedRow(t *testing.T) {
	requireIdentityDB(t)
	r := newIdentityTestRouter()

	userID := uuid.NewString()
	target, sibling := uuid.NewString(), uuid.NewString()
	cleanupIdentityRow(t, "credentials", target)
	cleanupIdentityRow(t, "credentials", sibling)
	cleanupIdentityRow(t, "users", userID)
	mustExecIdentity(t, `INSERT INTO users (id, name, password_hash) VALUES ($1, $2, 'x')`,
		userID, "src1-cred-user-"+userID[:8])
	for _, id := range []string{target, sibling} {
		mustExecIdentity(t, `INSERT INTO credentials (id, user_id, type, blob)
			VALUES ($1, $2, 'ec2', 'before-blob')`, id, userID)
	}

	code, body := patchJSON(t, r, "/v3/credentials/"+target,
		`{"credential":{"blob":"after-blob"}}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH credential: code=%d body=%v; want 200", code, body)
	}

	var targetBlob, siblingBlob string
	if err := database.DB.QueryRow(context.Background(),
		"SELECT blob FROM credentials WHERE id = $1", target).Scan(&targetBlob); err != nil {
		t.Fatalf("read back target: %v", err)
	}
	if err := database.DB.QueryRow(context.Background(),
		"SELECT blob FROM credentials WHERE id = $1", sibling).Scan(&siblingBlob); err != nil {
		t.Fatalf("read back sibling: %v", err)
	}
	if targetBlob != "after-blob" {
		t.Errorf("addressed credential blob = %q, want %q", targetBlob, "after-blob")
	}
	if siblingBlob != "before-blob" {
		t.Errorf("sibling credential blob = %q, want it untouched — a credential is the one column "+
			"here where writing the wrong row is a security event, not a data error", siblingBlob)
	}
}
