package nova

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// SPEC-020 (cloud-storage#533) — server create must resolve `security_groups[].name`
// by **id or name**.
//
// The field is spelled "name", but with Neutron as the network backend upstream Nova
// matches each entry against both the name and the UUID of the caller's groups.
// Horizon's launch wizard depends on that: setFinalSpecSecurityGroups pushes
// `securityGroup.id` whenever Neutron is enabled. Matching on name alone rejected every
// dashboard launch with `Security group <uuid> not found.` (HTTP 400) — which only
// started happening once the security-group list stopped 500ing, so fixing one bug
// exposed the next.

// seededSecurityGroupID returns the id of the seeded project's `default` group.
func seededSecurityGroupID(t *testing.T) string {
	t.Helper()
	var id string
	if err := database.DB.QueryRow(t.Context(),
		"SELECT id FROM security_groups WHERE name = 'default' AND project_id = $1",
		seededProjectID,
	).Scan(&id); err != nil {
		t.Fatalf("look up seeded default security group: %v", err)
	}
	return id
}

// createdServerID pulls the id out of a 202 create response.
func createdServerID(t *testing.T, body string) string {
	t.Helper()
	var env struct {
		Server struct {
			ID string `json:"id"`
		} `json:"server"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("parse server: %v; body=%s", err, body)
	}
	return env.Server.ID
}

func TestCreateServerResolvesSecurityGroupByIDOrName(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)
	sgID := seededSecurityGroupID(t)

	for _, tc := range []struct{ name, ref string }{
		{"by name (CLI and Terraform)", "default"},
		{"by id (Horizon's launch wizard)", sgID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(
				`{"server":{"name":"sgref-%s","flavorRef":"00000000-0000-0000-0000-000000000011","imageRef":"11111111-1111-1111-1111-111111111111","security_groups":[{"name":%q}]}}`,
				tc.name[:6], tc.ref)

			code, resp := doReq(r, http.MethodPost, "/v2.1/servers", body, hdr)
			if code != http.StatusAccepted {
				t.Fatalf("create with security group %q: code=%d body=%s; want 202", tc.ref, code, resp)
			}
			id := createdServerID(t, resp)
			// context.Background(), not t.Context(): the test context is already
			// cancelled by the time cleanups run, so the DELETE would never execute.
			t.Cleanup(func() {
				if _, err := database.DB.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id); err != nil {
					t.Logf("cleanup instance %s: %v", id, err)
				}
			})

			// The association must point at the real group either way — accepting the
			// reference is not enough if it silently attaches nothing.
			var attached string
			if err := database.DB.QueryRow(t.Context(),
				"SELECT security_group_id::text FROM server_security_groups WHERE server_id = $1",
				id,
			).Scan(&attached); err != nil {
				t.Fatalf("no security group attached to %s: %v", id, err)
			}
			if attached != sgID {
				t.Errorf("attached security group = %s; want %s", attached, sgID)
			}
		})
	}
}

// TestCreateServerRejectsUnknownSecurityGroup: broadening the lookup must not make it
// accept anything. A well-formed but unknown UUID is still a 400 — and must not fall
// through to the `default` auto-create branch.
func TestCreateServerRejectsUnknownSecurityGroup(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)

	for _, ref := range []string{
		"11111111-1111-1111-1111-111111111111",
		"no-such-group",
	} {
		code, resp := doReq(r, http.MethodPost, "/v2.1/servers", fmt.Sprintf(
			`{"server":{"name":"sgref-bad","flavorRef":"00000000-0000-0000-0000-000000000011","imageRef":"11111111-1111-1111-1111-111111111111","security_groups":[{"name":%q}]}}`,
			ref), hdr)
		if code != http.StatusBadRequest {
			t.Errorf("create with unknown security group %q: code=%d body=%s; want 400", ref, code, resp)
		}
	}
}
