package nova

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Branch review 2026-08-28, SRC-1 — UpdateServer, one of the twenty Update* handlers the SQL refactor
// rewrote and one of the fifteen that had no test.
//
// UpdateServer assigns one caller-supplied column, so there is no second field to swap it with. What
// remains is the WHERE argument, so three instances are seeded — the target, a sibling in the same
// project, and one in another project — and all three are read back. Only the addressed row may move.
//
// Note what this does NOT assert. The caller here is an administrator (testAuthHdr hardcodes the
// role), and UpdateServer resolves its row through resolveServerOr404, which applies #549's
// administrator override: an administrator may update any project's server, matching upstream Nova's
// admin_or_owner default. So the untouched cross-project row proves the statement addressed one row,
// not that a tenant boundary held. The boundary is the next test's job, with a non-admin caller.
func TestUpdateServerWritesOnlyTheAddressedRow(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()

	const caller = "00000000-0000-0000-0000-000000000002"
	const other = "00000000-0000-0000-0000-0000000b0b0b"
	target, sibling, foreign := uuid.NewString(), uuid.NewString(), uuid.NewString()

	for _, id := range []string{target, sibling, foreign} {
		t.Cleanup(func() {
			testPool.Exec(context.Background(), //nolint:errcheck // best-effort cleanup
				"DELETE FROM instances WHERE id = $1", id)
		})
	}
	if _, err := testPool.Exec(context.Background(),
		"INSERT INTO projects (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING",
		other, "src1-nova-other"); err != nil {
		t.Fatalf("fixture project: %v", err)
	}
	// flavor_id, image_id and user_id are set because UpdateServer's read-back scans them into plain
	// types; a NULL in any is a 500 before the update is even reached. Not reachable through the API,
	// so this is a fixture requirement rather than something the test asserts. flavor_id and user_id
	// carry foreign keys, hence the subqueries; images is unseeded so image_id is a free uuid.
	seed := func(id, project, name string) {
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO instances (id, name, project_id, status, flavor_id, image_id, user_id)
			 VALUES ($1, $2, $3, 'ACTIVE',
			         (SELECT id FROM flavors LIMIT 1), $4, (SELECT id FROM users LIMIT 1))`,
			id, name, project, uuid.NewString()); err != nil {
			t.Fatalf("fixture instance %s: %v", name, err)
		}
	}
	seed(target, caller, "src1-target-before")
	seed(sibling, caller, "src1-sibling-before")
	seed(foreign, other, "src1-foreign-before")

	code, body := doReq(r, http.MethodPatch, "/v2.1/servers/"+target,
		`{"server":{"name":"src1-target-after"}}`,
		testAuthHdr(caller, "00000000-0000-0000-0000-00000000a0a0"))
	if code != http.StatusOK {
		t.Fatalf("PATCH server: code=%d body=%s; want 200", code, body)
	}

	read := func(id string) string {
		t.Helper()
		var name string
		if err := testPool.QueryRow(context.Background(),
			"SELECT name FROM instances WHERE id = $1", id).Scan(&name); err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		return name
	}
	for _, c := range []struct{ what, got, want string }{
		{"addressed instance", read(target), "src1-target-after"},
		{"sibling in the same project", read(sibling), "src1-sibling-before"},
		{"instance in another project", read(foreign), "src1-foreign-before"},
	} {
		if c.got != c.want {
			t.Errorf("%s: name = %q, want %q", c.what, c.got, c.want)
		}
	}
}

// The tenant boundary, read with a caller who does not hold the administrator override. A non-admin
// addressing another project's instance must change nothing and be refused — never a quiet success.
//
// The header is built here rather than with testAuthHdr, which hardcodes X-Test-Roles: admin. Using it
// would make this test assert the override instead of the boundary, and it would pass for the wrong
// reason — the mistake this branch's stub runbook made at S10 and had corrected.
func TestUpdateServerRefusesAnotherProjectsInstance(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()

	const caller = "00000000-0000-0000-0000-000000000002"
	const other = "00000000-0000-0000-0000-0000000b0b0b"
	foreign := uuid.NewString()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), //nolint:errcheck // best-effort cleanup
			"DELETE FROM instances WHERE id = $1", foreign)
	})
	if _, err := testPool.Exec(context.Background(),
		"INSERT INTO projects (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING",
		other, "src1-nova-other"); err != nil {
		t.Fatalf("fixture project: %v", err)
	}
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO instances (id, name, project_id, status, flavor_id, image_id, user_id)
		 VALUES ($1, 'src1-untouchable', $2, 'ACTIVE',
		         (SELECT id FROM flavors LIMIT 1), $3, (SELECT id FROM users LIMIT 1))`,
		foreign, other, uuid.NewString()); err != nil {
		t.Fatalf("fixture instance: %v", err)
	}

	tenantHdr := map[string]string{
		"X-Test-Project": caller,
		"X-Test-User":    "00000000-0000-0000-0000-00000000a0a0",
		"X-Test-Roles":   "member",
	}
	code, _ := doReq(r, http.MethodPatch, "/v2.1/servers/"+foreign,
		`{"server":{"name":"hijacked"}}`, tenantHdr)
	if code == http.StatusOK {
		t.Errorf("PATCH another project's server returned 200; want a refusal")
	}

	var name string
	if err := testPool.QueryRow(context.Background(),
		"SELECT name FROM instances WHERE id = $1", foreign).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "src1-untouchable" {
		t.Errorf("name = %q after a refused request; want it unchanged — %s", name,
			"a refusal that still writes is the defect this branch exists to remove")
	}
}
