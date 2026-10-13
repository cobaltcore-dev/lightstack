package cinder

import (
	"net/http"
	"testing"
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

	// And the admin keeps the write paths.
	if code, body := doReq(r, http.MethodPost, "/v3/types", `{"volume_type":{"name":"authz-admin-type"}}`, admin); code != http.StatusCreated && code != http.StatusOK {
		t.Errorf("create volume type as admin: code=%d body=%s; want 2xx", code, body)
	}
}
