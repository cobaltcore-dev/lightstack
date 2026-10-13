package keystone

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
)

func userRow(t *testing.T, name string) (id, hash string, enabled, ok bool) {
	t.Helper()
	err := testPool.QueryRow(context.Background(),
		"SELECT id, password_hash, enabled FROM users WHERE name = $1", name).Scan(&id, &hash, &enabled)
	if err == database.ErrNoRows {
		return "", "", false, false
	}
	if err != nil {
		t.Fatalf("userRow(%q): %v", name, err)
	}
	return id, hash, enabled, true
}

func verifies(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func hasAdminRole(t *testing.T, userID string) bool {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		"SELECT count(*) FROM role_assignments WHERE user_id=$1 AND project_id=$2 AND role_id=$3",
		userID, defaultProjectID, adminRoleID).Scan(&n); err != nil {
		t.Fatalf("hasAdminRole: %v", err)
	}
	return n > 0
}

func TestBootstrapAdmin(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	// (a) seeded admin still 'secret' → configured password applied, 'secret' rejected
	t.Run("provision_from_default", func(t *testing.T) {
		resetAdmin(t)
		if err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminPassword: "NewPass!23"}); err != nil {
			t.Fatal(err)
		}
		_, hash, _, ok := userRow(t, "admin")
		if !ok || !verifies(hash, "NewPass!23") || verifies(hash, "secret") {
			t.Fatalf("admin not provisioned with configured password")
		}
	})

	// (b) idempotent: re-run with same config is a no-op, still works
	t.Run("idempotent", func(t *testing.T) {
		resetAdmin(t)
		_ = BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminPassword: "NewPass!23"})
		if err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminPassword: "NewPass!23"}); err != nil {
			t.Fatal(err)
		}
		_, hash, _, _ := userRow(t, "admin")
		if !verifies(hash, "NewPass!23") {
			t.Fatalf("idempotent restart broke admin password")
		}
	})

	// (c) admin_password: secret → no-op write-once, still authenticates with secret
	t.Run("secret_configured_noop", func(t *testing.T) {
		resetAdmin(t)
		_, before, _, _ := userRow(t, "admin")
		if err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminPassword: "secret"}); err != nil {
			t.Fatal(err)
		}
		_, after, _, _ := userRow(t, "admin")
		if after != before {
			t.Fatalf("admin_password=secret should be a no-op (hash unchanged)")
		}
	})

	// (d) operator/API-rotated password is NOT overwritten by config
	t.Run("no_clobber_rotated", func(t *testing.T) {
		resetAdmin(t)
		rotated, _ := bcrypt.GenerateFromPassword([]byte("Rotated@9"), bcrypt.DefaultCost)
		if _, err := testPool.Exec(ctx, "UPDATE users SET password_hash=$1 WHERE name='admin'", string(rotated)); err != nil {
			t.Fatal(err)
		}
		if err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminPassword: "Configured@1"}); err != nil {
			t.Fatal(err)
		}
		_, hash, _, _ := userRow(t, "admin")
		if !verifies(hash, "Rotated@9") || verifies(hash, "Configured@1") {
			t.Fatalf("bootstrap clobbered a runtime-rotated password")
		}
	})

	// (e) custom admin_user → created with admin role + domain; seeded admin disabled
	t.Run("custom_admin_user", func(t *testing.T) {
		resetAdmin(t)
		if err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminUser: "spec007-ops", AdminPassword: "OpsPass!1"}); err != nil {
			t.Fatal(err)
		}
		id, hash, enabled, ok := userRow(t, "spec007-ops")
		if !ok || !verifies(hash, "OpsPass!1") || !enabled {
			t.Fatalf("custom admin not provisioned")
		}
		if !hasAdminRole(t, id) {
			t.Fatalf("custom admin missing admin role on default project")
		}
		var domainID string
		if err := testPool.QueryRow(ctx, "SELECT domain_id::text FROM users WHERE name='spec007-ops'").Scan(&domainID); err != nil {
			t.Fatal(err)
		}
		if domainID != defaultDomainID {
			t.Fatalf("custom admin domain_id = %q, want %q", domainID, defaultDomainID)
		}
		if _, _, adminEnabled, _ := userRow(t, "admin"); adminEnabled {
			t.Fatalf("seeded admin should be disabled when a custom admin_user is set")
		}
	})

	// custom admin_user naming a PRE-EXISTING account → admin role granted, but
	// the password is left unchanged (first-boot seed) and a warning is emitted.
	t.Run("custom_admin_user_existing", func(t *testing.T) {
		resetAdmin(t)
		old, _ := bcrypt.GenerateFromPassword([]byte("OldPass#1"), bcrypt.DefaultCost)
		if _, err := testPool.Exec(ctx,
			"INSERT INTO users (name, password_hash, enabled, domain_id) VALUES ('spec007-existing', $1, true, $2) ON CONFLICT (name) DO NOTHING",
			string(old), defaultDomainID); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		orig := log.Logger
		log.Logger = zerolog.New(&buf)
		err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminUser: "spec007-existing", AdminPassword: "NewPass#2"})
		log.Logger = orig
		if err != nil {
			t.Fatal(err)
		}
		id, hash, enabled, ok := userRow(t, "spec007-existing")
		if !ok || !enabled || !verifies(hash, "OldPass#1") || verifies(hash, "NewPass#2") {
			t.Fatalf("existing account's password must be left unchanged, not clobbered")
		}
		if !hasAdminRole(t, id) {
			t.Fatalf("existing custom admin should be granted the admin role")
		}
		if !strings.Contains(buf.String(), "NOT applied") {
			t.Fatalf("expected a warning that admin_password was not applied; got: %s", buf.String())
		}
	})

	// (f) no password → WARN, default retained, no error
	t.Run("no_password_warns_and_keeps_default", func(t *testing.T) {
		resetAdmin(t)
		if err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminPassword: "   "}); err != nil {
			t.Fatal(err)
		}
		_, hash, enabled, _ := userRow(t, "admin")
		if !verifies(hash, "secret") || !enabled {
			t.Fatalf("no-password run should leave the seeded default live")
		}
	})

	// (g) password value never appears in logs
	t.Run("never_logs_password", func(t *testing.T) {
		resetAdmin(t)
		var buf bytes.Buffer
		orig := log.Logger
		log.Logger = zerolog.New(&buf)
		const secretPw = "Sup3r-Secret-Marker-XYZ"
		err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminPassword: secretPw})
		log.Logger = orig
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), secretPw) {
			t.Fatalf("admin password leaked into logs: %s", buf.String())
		}
	})

	// (h) project isolation: no identity other than the admin is created/altered (FR-007)
	t.Run("preserves_other_identities", func(t *testing.T) {
		resetAdmin(t)
		if _, err := testPool.Exec(ctx,
			"INSERT INTO users (name, password_hash, enabled, domain_id) VALUES ('spec007-bystander', $1, true, $2) ON CONFLICT (name) DO NOTHING",
			seededSecretHash, defaultDomainID); err != nil {
			t.Fatal(err)
		}
		_, beforeHash, beforeEnabled, _ := userRow(t, "spec007-bystander")
		bid, _, _, _ := userRow(t, "spec007-bystander")
		if hasAdminRole(t, bid) {
			t.Fatalf("precondition: bystander should not have admin role")
		}
		if err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminPassword: "NewPass!23"}); err != nil {
			t.Fatal(err)
		}
		_, afterHash, afterEnabled, ok := userRow(t, "spec007-bystander")
		if !ok || afterHash != beforeHash || afterEnabled != beforeEnabled || hasAdminRole(t, bid) {
			t.Fatalf("bootstrap altered an unrelated identity")
		}
	})

	// (i) arbitrary non-default value accepted verbatim (no min-length/complexity/denylist)
	t.Run("permissive_no_validation", func(t *testing.T) {
		resetAdmin(t)
		const weird = "x" // 1 char; would fail any length policy
		if err := BootstrapAdmin(ctx, testPool, common.KeystoneConfig{AdminPassword: weird}); err != nil {
			t.Fatal(err)
		}
		_, hash, _, _ := userRow(t, "admin")
		if !verifies(hash, weird) {
			t.Fatalf("arbitrary password not accepted verbatim")
		}
	})
}
