package keystone

import (
	"context"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"golang.org/x/crypto/bcrypt"
)

// runConcurrentBootstrap runs n concurrent BootstrapAdmin(cfg) calls against the
// same database and fails if any returns an error (FR-008a). The caller asserts
// the converged end state.
func runConcurrentBootstrap(t *testing.T, cfg common.KeystoneConfig, n int) {
	t.Helper()
	ctx := context.Background()
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { errs <- BootstrapAdmin(ctx, cfg) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent BootstrapAdmin returned error: %v", err)
		}
	}
}

// TestBootstrapAdminConcurrent verifies FR-008a / SC-009 for the default-name
// path: concurrent first-boot provisioning of the seeded `admin` converges to a
// single account with the configured password, with no duplicate-key error.
func TestBootstrapAdminConcurrent(t *testing.T) {
	requireDB(t)
	resetAdmin(t)
	ctx := context.Background()

	const pw = "Concurrent!9"
	runConcurrentBootstrap(t, common.KeystoneConfig{AdminPassword: pw}, 8)

	var count int
	if err := database.DB.QueryRow(ctx, "SELECT count(*) FROM users WHERE name='admin'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one admin row after concurrent bootstrap, got %d", count)
	}
	_, hash, _, _ := userRow(t, "admin")
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) != nil {
		t.Fatalf("configured password did not win after concurrent bootstrap")
	}
}

// TestBootstrapAdminConcurrentCustomUser verifies FR-008a / SC-009 for the
// custom-name path (seedCustomAdmin: INSERT ON CONFLICT, SELECT id,
// grantAdminRole, disable seeded admin). Concurrent first-boot provisioning of a
// custom admin_user must converge to exactly one account with the configured
// password and exactly one admin-role grant, with the seeded `admin` disabled —
// no duplicate-key error and no duplicate role assignment.
func TestBootstrapAdminConcurrentCustomUser(t *testing.T) {
	requireDB(t)
	resetAdmin(t)
	ctx := context.Background()

	const user = "spec007-conc"
	const pw = "Concurrent!9"
	runConcurrentBootstrap(t, common.KeystoneConfig{AdminUser: user, AdminPassword: pw}, 8)

	// Exactly one custom-admin row, enabled, with the configured password.
	var count int
	if err := database.DB.QueryRow(ctx, "SELECT count(*) FROM users WHERE name=$1", user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one %q row, got %d", user, count)
	}
	id, hash, enabled, ok := userRow(t, user)
	if !ok || !enabled {
		t.Fatalf("custom admin %q should exist and be enabled", user)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) != nil {
		t.Fatalf("configured password did not win for the custom admin")
	}

	// Exactly one admin-role grant on the default project (no duplicates).
	var roles int
	if err := database.DB.QueryRow(ctx,
		"SELECT count(*) FROM role_assignments WHERE user_id=$1 AND project_id=$2 AND role_id=$3",
		id, defaultProjectID, adminRoleID).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if roles != 1 {
		t.Fatalf("expected exactly one admin role assignment for %q, got %d", user, roles)
	}

	// Seeded admin neutralized.
	if _, _, adminEnabled, _ := userRow(t, "admin"); adminEnabled {
		t.Fatalf("seeded admin should be disabled when a custom admin_user is provisioned")
	}
}
