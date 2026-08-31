package nova

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// A server with no recorded libvirt domain must not make an action fail internally
// (cloud-storage#547, found by R10 on the real-mode host).
//
// `instances.libvirt_domain_id` is nullable with no default and the create INSERT omits
// it, so EVERY server starts NULL: forever in stub mode, and in real mode until the
// goroutine that defines the guest writes it back. Three handlers scanned that column
// into a plain `string`, so pgx refused it — `cannot scan NULL into *string` — and the
// action answered `500 database error`.
//
// lifecycleAction already had this right: it scans into `any` precisely because the
// column is nullable. The same reasoning had not reached suspend, resume or shelve.
//
// Observed as four failures of TestNovaActionsByName_Contract (suspend, resume, shelve,
// shelveOffload — the last dispatching to ShelveInstance), which read as a name-resolution
// defect and were nothing of the kind. The name was resolved correctly; the row was NULL.

// seedInstanceWithoutDomain inserts an ACTIVE instance whose libvirt_domain_id is NULL —
// the state every server is in before a guest is defined for it.
func seedInstanceWithoutDomain(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state)
		 VALUES ($1, $2, $3, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1)
		 RETURNING id::text`,
		name, seededProjectID, seededUserID,
	).Scan(&id); err != nil {
		t.Fatalf("seeding instance: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, id)
	})

	// The point of the fixture: assert the column really is NULL, so the test cannot
	// quietly stop covering the case if the insert or the schema default changes.
	var isNull bool
	if err := testPool.QueryRow(t.Context(),
		`SELECT libvirt_domain_id IS NULL FROM instances WHERE id = $1`, id).Scan(&isNull); err != nil {
		t.Fatalf("reading the domain column: %v", err)
	}
	if !isNull {
		t.Fatalf("fixture is not covering the case: libvirt_domain_id is not NULL for %s", name)
	}
	return id
}

func TestActionsTolerateANullDomainID(t *testing.T) {
	requireDB(t)
	_, r, _ := newLifecycleService(t)

	for _, action := range []string{
		`{"suspend": null}`,
		`{"resume": null}`,
		`{"shelve": null}`,
		`{"shelveOffload": null}`,
	} {
		t.Run(action, func(t *testing.T) {
			id := seedInstanceWithoutDomain(t, fmt.Sprintf("spec025-nulldom-%x", len(action)*7+int(action[2])))

			code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", action, lifecycleHdr())
			if code == http.StatusInternalServerError {
				t.Errorf("%s against a server with no recorded guest = 500. body: %s\n"+
					"a nullable column scanned into a string is not a fault the caller can act on", action, body)
			}
		})
	}
}
