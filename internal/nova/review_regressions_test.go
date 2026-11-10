package nova

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestFlavorOperatorHandlersRequireAdmin(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	flavorID := uuid.NewString()
	flavorName := "review-flavor-" + flavorID[:8]
	if _, err := testPool.Exec(t.Context(), `
		INSERT INTO flavors (id, name, vcpus, ram_mb, disk_gb, is_public)
		VALUES ($1, $2, 1, 512, 1, false)
	`, flavorID, flavorName); err != nil {
		t.Fatalf("seed flavor: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM flavors WHERE id = $1", flavorID) })

	member := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
	admin := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "admin"}
	paths := []struct{ method, path, body string }{
		{http.MethodPost, "/v2.1/flavors", `{"flavor":{"name":"member-flavor","ram":512,"vcpus":1,"disk":1}}`},
		{http.MethodDelete, "/v2.1/flavors/" + flavorID, ""},
		{http.MethodPost, "/v2.1/flavors/" + flavorID + "/os-extra_specs", `{"extra_specs":{"review":"member"}}`},
		{http.MethodPut, "/v2.1/flavors/" + flavorID + "/os-extra_specs/review", `{"review":"member"}`},
		{http.MethodDelete, "/v2.1/flavors/" + flavorID + "/os-extra_specs/review", ""},
		{http.MethodPost, "/v2.1/flavors/" + flavorID + "/action", `{"addTenantAccess":{"tenant":"` + seededProjectID + `"}}`},
		{http.MethodGet, "/v2.1/flavors/" + flavorID + "/os-flavor-access", ""},
	}
	for _, tc := range paths {
		if code, body := doReq(r, tc.method, tc.path, tc.body, member); code != http.StatusForbidden {
			t.Errorf("member %s %s: code=%d body=%s; want 403", tc.method, tc.path, code, body)
		}
	}

	createdName := "review-created-" + uuid.NewString()[:8]
	if code, body := doReq(r, http.MethodPost, "/v2.1/flavors",
		fmt.Sprintf(`{"flavor":{"name":%q,"ram":512,"vcpus":1,"disk":1}}`, createdName), admin); code != http.StatusOK {
		t.Fatalf("admin create flavor: code=%d body=%s; want 200", code, body)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), "DELETE FROM flavors WHERE name = $1", createdName) })
	checkAdmin := func(method, path, body string, want int) {
		t.Helper()
		if code, response := doReq(r, method, path, body, admin); code != want {
			t.Errorf("admin %s %s: code=%d body=%s; want %d", method, path, code, response, want)
		}
	}
	checkAdmin(http.MethodPost, "/v2.1/flavors/"+flavorID+"/os-extra_specs", `{"extra_specs":{"review":"admin"}}`, http.StatusOK)
	checkAdmin(http.MethodPut, "/v2.1/flavors/"+flavorID+"/os-extra_specs/review", `{"review":"updated"}`, http.StatusOK)
	checkAdmin(http.MethodDelete, "/v2.1/flavors/"+flavorID+"/os-extra_specs/review", "", http.StatusNoContent)
	checkAdmin(http.MethodPost, "/v2.1/flavors/"+flavorID+"/action", `{"addTenantAccess":{"tenant":"`+seededProjectID+`"}}`, http.StatusOK)
	checkAdmin(http.MethodGet, "/v2.1/flavors/"+flavorID+"/os-flavor-access", "", http.StatusOK)
	checkAdmin(http.MethodDelete, "/v2.1/flavors/"+flavorID, "", http.StatusNoContent)
}

func TestKeypairImportLimitAndDuplicateConflict(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
	oversized := fmt.Sprintf(`{"keypair":{"name":"too-large","public_key":%q}}`, strings.Repeat("x", (64<<10)+1))
	if code, body := doReq(r, http.MethodPost, "/v2.1/os-keypairs", oversized, hdr); code != http.StatusBadRequest {
		t.Errorf("oversized keypair import: code=%d body=%s; want 400", code, body)
	}

	publicKey, _, fingerprint, err := generateSSHKeyPair()
	if err != nil {
		t.Fatalf("generate test SSH key: %v", err)
	}
	name := "review-duplicate-" + uuid.NewString()[:8]
	if _, err := testPool.Exec(t.Context(), `
		INSERT INTO keypairs (user_id, name, public_key, fingerprint)
		VALUES ($1, $2, $3, $4)
	`, seededUserID, name, publicKey, fingerprint); err != nil {
		t.Fatalf("seed existing keypair: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM keypairs WHERE user_id = $1 AND name = $2", seededUserID, name)
	})
	body := fmt.Sprintf(`{"keypair":{"name":%q,"public_key":%q}}`, name, publicKey)
	if code, response := doReq(r, http.MethodPost, "/v2.1/os-keypairs", body, hdr); code != http.StatusConflict {
		t.Errorf("duplicate keypair import: code=%d body=%s; want 409", code, response)
	}
}
