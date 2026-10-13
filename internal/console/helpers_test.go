package console

import (
	"context"
	"testing"
	"time"
)

// The migration-001 seed data every Lightstack deployment starts with.
const (
	seededProjectID = "00000000-0000-0000-0000-000000000002"
	seededUserID    = "00000000-0000-0000-0000-000000000001"
)

// testSocket has the shape of a guest's VNC socket: the instance's own path in the
// console socket directory.
const testSocket = "/var/lib/lightstack/console-sockets/11111111-2222-3333-4444-555555555555.sock"

// testDomainID stands in for the libvirt domain ID of the run a token is issued for.
const testDomainID int32 = 7

// seedInstance creates a throwaway instance row and returns (instanceID,
// projectID). console_auth_tokens has an FK onto instances, so tokens cannot be
// issued against an id that does not exist — which is itself part of the
// contract (a token can never outlive its instance).
func seedInstance(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	var id string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
		 VALUES ($1, $2, $3, (SELECT id FROM flavors WHERE name = 'm1.small'), 'ACTIVE') RETURNING id`,
		"spec021-console-"+t.Name(), seededProjectID, seededUserID,
	).Scan(&id); err != nil {
		t.Fatalf("seedInstance: %v", err)
	}
	// context.Background(), not t.Context(): the test context is already cancelled by
	// the time cleanup runs, so t.Context() here would fail every delete.
	t.Cleanup(func() {
		// Cascades to console_auth_tokens; projection files live in a t.TempDir.
		_, _ = testPool.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id)
	})
	return id, seededProjectID
}

func deleteInstance(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	if _, err := testPool.Exec(ctx, "DELETE FROM instances WHERE id = $1", id); err != nil {
		t.Fatalf("deleteInstance(%s): %v", id, err)
	}
}

// backdate ages a token past its expiry without sleeping.
//
// Issuing with a negative TTL is not equivalent: Issue reaps the instance's expired
// tokens opportunistically, so an already-expired token is gone before the code
// under test ever runs. Backdating isolates the reaping path itself.
func backdate(t *testing.T, ctx context.Context, token string) {
	t.Helper()
	if _, err := testPool.Exec(ctx,
		"UPDATE console_auth_tokens SET expires_at = $2 WHERE token = $1", token, time.Now().Add(-time.Minute),
	); err != nil {
		t.Fatalf("backdating token: %v", err)
	}
}
