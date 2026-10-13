package cinder

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestOperatorSurfacesAdminOnly pins review 2026-10-13 H-3: volume types, QoS
// specs and the manage/unmanage surface shape storage for every tenant (and
// manage addresses host-level backends). They were authentication-only — a
// plain member could create volume types every tenant then booted from, add
// its project to a private type, and import host backings into its quota.
// Reads of volume types stay open: tenants need them to pick one at create.
func TestOperatorSurfacesAdminOnly(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()

	const project = "00000000-0000-0000-0000-000000000002" // seeded default project
	member := map[string]string{"X-Test-Project": project, "X-Test-Roles": "member"}
	admin := map[string]string{"X-Test-Project": project, "X-Test-Roles": "admin"}

	memberDenied := []struct{ name, method, path, body string }{
		{"create volume type", http.MethodPost, "/v3/types", `{"volume_type":{"name":"member-type"}}`},
		{"manage a volume", http.MethodPost, "/v3/os-volume-manage", `{"volume":{"host":"h1","ref":{"source-name":"x"}}}`},
		{"manage a snapshot", http.MethodPost, "/v3/os-snapshot-manage", `{"snapshot":{"volume_id":"00000000-0000-0000-0000-000000000000","ref":{"source-name":"x"}}}`},
		{"list manageable volumes", http.MethodGet, "/v3/manageable_volumes", ""},
		{"create a QoS spec", http.MethodPost, "/v3/qos-specs", `{"qos_specs":{"name":"member-qos"}}`},
	}
	for _, tc := range memberDenied {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := doReq(r, tc.method, tc.path, tc.body, member); code != http.StatusForbidden {
				t.Errorf("%s %s as member: code=%d body=%s; want 403", tc.method, tc.path, code, body)
			}
		})
	}

	// The type catalog stays readable for members (they pick a type at create).
	if code, body := doReq(r, http.MethodGet, "/v3/types", "", member); code != http.StatusOK {
		t.Errorf("GET /v3/types as member: code=%d body=%s; want 200 (reads stay open)", code, body)
	}

	// And the admin keeps the write paths. Unique name + cleanup: the volume
	// types table is shared across runs of the shared test database.
	typeName := "authz-admin-type-" + uuid.NewString()[:8]
	if code, body := doReq(r, http.MethodPost, "/v3/types", `{"volume_type":{"name":"`+typeName+`"}}`, admin); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create volume type as admin: code=%d body=%s; want 2xx", code, body)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volume_types WHERE name = $1`, typeName)
	})
}

// Round 2 of this review: a volume type created without a description has a
// NULL one, and ListVolumeTypes scanned it into a plain string — the row
// failed to scan and the type silently vanished from the catalog (and
// GetVolumeType 500'd). The list must serve both descriptions and their
// absence.
func TestListVolumeTypesServesNullDescription(t *testing.T) {
	requireDB(t)
	r := newCinderTestRouter()

	const project = "00000000-0000-0000-0000-000000000002"
	admin := map[string]string{"X-Test-Project": project, "X-Test-Roles": "admin"}

	typeName := "authz-undescribed-" + uuid.NewString()[:8]
	code, body := doReq(r, http.MethodPost, "/v3/types", `{"volume_type":{"name":"`+typeName+`"}}`, admin)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create undescribed volume type: code=%d body=%s", code, body)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM volume_types WHERE name = $1`, typeName)
	})

	if code, body := doReq(r, http.MethodGet, "/v3/types", "", admin); code != http.StatusOK {
		t.Fatalf("list volume types: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, typeName) {
		t.Errorf("a volume type created without a description is missing from GET /v3/types (the NULL-description scan drop): %s", body)
	}
}
