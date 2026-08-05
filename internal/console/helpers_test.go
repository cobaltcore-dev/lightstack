package console

import (
	"context"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/database"
)

// The migration-001 seed data every O3K deployment starts with.
const (
	seededProjectID = "00000000-0000-0000-0000-000000000002"
	seededUserID    = "00000000-0000-0000-0000-000000000001"
)

// seedInstance creates a throwaway instance row and returns (instanceID,
// projectID). console_auth_tokens has an FK onto instances, so tokens cannot be
// issued against an id that does not exist — which is itself part of the
// contract (a token can never outlive its instance).
func seedInstance(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	var id string
	if err := database.DB.QueryRow(ctx,
		`INSERT INTO instances (name, project_id, user_id, status)
		 VALUES ($1, $2, $3, 'ACTIVE') RETURNING id`,
		"spec021-console-"+t.Name(), seededProjectID, seededUserID,
	).Scan(&id); err != nil {
		t.Fatalf("seedInstance: %v", err)
	}
	// context.Background(), not t.Context(): the test context is already cancelled by
	// the time cleanup runs, so t.Context() here would fail every delete.
	t.Cleanup(func() {
		// Cascades to console_auth_tokens; projection files live in a t.TempDir.
		_, _ = database.DB.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id)
	})
	return id, seededProjectID
}

func deleteInstance(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	if _, err := database.DB.Exec(ctx, "DELETE FROM instances WHERE id = $1", id); err != nil {
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
	if _, err := database.DB.Exec(ctx,
		"UPDATE console_auth_tokens SET expires_at = now() - interval '1 minute' WHERE token = $1", token,
	); err != nil {
		t.Fatalf("backdating token: %v", err)
	}
}
