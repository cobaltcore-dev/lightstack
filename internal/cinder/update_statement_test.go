package cinder

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Branch review 2026-08-28, SRC-1 — UpdateVolumeType, one of the twenty Update* handlers the SQL
// refactor rewrote and one of the fifteen that had no test.
//
// Three fields are sent together with values that cannot be confused, then each column is read back
// separately. That catches a value assigned to the wrong column, an ignored field, and a SET wider
// than the request. It does not catch a misnumbered placeholder: common.SQLUpdate makes that
// unrepresentable, since the placeholder is produced by appending the value it reads. See
// internal/neutron/update_statement_test.go for the argument in full.
func TestUpdateVolumeTypeAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()

	id := uuid.NewString()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), //nolint:errcheck // best-effort cleanup
			"DELETE FROM volume_types WHERE id = $1", id)
	})
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO volume_types (id, name, description, is_public)
		 VALUES ($1, 'before-name', 'before-description', true)`, id); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	code, body := doReq(r, http.MethodPut, "/v3/types/"+id,
		`{"volume_type":{"name":"after-name","description":"after-description","os-volume-type-access:is_public":false}}`,
		cinderAuthHdr("00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-00000000a0a0"))
	if code != http.StatusOK {
		t.Fatalf("PUT volume type: code=%d body=%s; want 200", code, body)
	}

	var name, description string
	var isPublic bool
	if err := testPool.QueryRow(context.Background(),
		"SELECT name, description, is_public FROM volume_types WHERE id = $1",
		id).Scan(&name, &description, &isPublic); err != nil {
		t.Fatalf("read back volume type: %v", err)
	}
	if name != "after-name" {
		t.Errorf("volume_types.name = %q, want %q", name, "after-name")
	}
	if description != "after-description" {
		t.Errorf("volume_types.description = %q, want %q — holding the name's value would be a "+
			"wrong-column assignment", description, "after-description")
	}
	if isPublic {
		t.Errorf("volume_types.is_public = true, want false")
	}
}
