package keystone

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
)

// Seed identifiers from migrations 002 (seed_data) and 009 (default domain).
const (
	seededAdminName  = "admin"
	defaultAdminPass = "secret" // the well-known seeded default password
	defaultDomainID  = "00000000-0000-0000-0000-000000000100"
	defaultProjectID = "00000000-0000-0000-0000-000000000002"
	adminRoleID      = "00000000-0000-0000-0000-000000000003"
)

// BootstrapAdmin provisions the administrative account from configuration at
// startup as a first-boot seed (SPEC-007 WS-2). It:
//   - applies keystone.admin_password only at initial provisioning — when the
//     account is absent or still carries the well-known default `secret` — and
//     never overwrites a password that has since been changed (FR-002/FR-003);
//   - for a custom keystone.admin_user, creates the user in the Default domain
//     with the standard admin role on the default project, and disables the
//     seeded `admin` account (FR-004/FR-004a/FR-004b);
//   - is idempotent and concurrency-safe via atomic, guarded SQL (FR-008a);
//   - never writes the password value to logs (FR-006b).
//
// It is fail-soft: a missing password yields a WARN and the well-known default
// stays live (the install-blocking gate is owned by the SPEC-008 chart, not the
// binary — FR-006). Returns a non-nil error only on an actual database failure.
func BootstrapAdmin(ctx context.Context, cfg common.KeystoneConfig) error {
	password := strings.TrimSpace(cfg.AdminPassword)
	adminUser := strings.TrimSpace(cfg.AdminUser)
	if adminUser == "" {
		adminUser = seededAdminName
	}

	if password == "" {
		log.Warn().Msg("keystone.admin_password is not set; booting with the well-known default admin/secret credential — set keystone.admin_password for any exposed deployment")
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("bootstrap admin: hash password: %w", err)
	}

	if adminUser == seededAdminName {
		return seedDefaultAdmin(ctx, password, string(hash))
	}
	return seedCustomAdmin(ctx, adminUser, password, string(hash))
}

// seedDefaultAdmin handles the default-name path: it operates on the seeded
// `admin` row, applying the configured password only while that row still
// carries the default credential.
func seedDefaultAdmin(ctx context.Context, password, hash string) error {
	var id, storedHash string
	err := database.DB.QueryRow(ctx,
		"SELECT id, password_hash FROM users WHERE name = $1", seededAdminName,
	).Scan(&id, &storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Seeded admin missing (unexpected) — create it fully scoped.
		return createAdmin(ctx, seededAdminName, hash)
	}
	if err != nil {
		return fmt.Errorf("bootstrap admin: look up admin: %w", err)
	}

	// Already the configured password → no-op (steady state; also covers admin_password=secret).
	if bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password)) == nil {
		return nil
	}
	// Still the well-known default → replace with the configured password.
	if bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(defaultAdminPass)) == nil {
		tag, err := database.DB.Exec(ctx,
			"UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2 AND password_hash = $3",
			hash, id, storedHash,
		)
		if err != nil {
			return fmt.Errorf("bootstrap admin: set password: %w", err)
		}
		if tag.RowsAffected() == 1 {
			log.Info().Str("user", seededAdminName).Msg("admin password provisioned from configuration")
		}
		return nil
	}
	// Operator- or API-set non-default password → leave untouched (no clobber, FR-002).
	return nil
}

// seedCustomAdmin handles the custom-name path: it provisions the named admin
// (first-boot seed) with admin scoping and disables the seeded default admin.
// As with the default path, an already-provisioned password is never
// overwritten; when the configured password is not the one in effect it warns
// so the no-op is visible.
func seedCustomAdmin(ctx context.Context, adminUser, password, hash string) error {
	var id, storedHash string
	err := database.DB.QueryRow(ctx,
		"SELECT id, password_hash FROM users WHERE name = $1", adminUser,
	).Scan(&id, &storedHash)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// New account → create with the configured password and admin scoping.
		if err := createAdmin(ctx, adminUser, hash); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("bootstrap admin: look up %q: %w", adminUser, err)
	default:
		// Account already exists. First-boot seed: never overwrite its password.
		// Ensure the admin grant is present (idempotent), and warn when the
		// configured password is not the one in effect — e.g. admin_user names a
		// pre-existing account, or the password was rotated — so the no-op is visible.
		if bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password)) != nil {
			log.Warn().Str("user", adminUser).Msg("keystone.admin_user names an existing account; keystone.admin_password was NOT applied — its password is left unchanged (rotate via the Identity API)")
		}
		if err := grantAdminRole(ctx, id); err != nil {
			return err
		}
	}

	// Neutralize the seeded default so admin/secret can no longer authenticate (FR-004a).
	if _, err := database.DB.Exec(ctx,
		"UPDATE users SET enabled = false, updated_at = NOW() WHERE name = $1", seededAdminName,
	); err != nil {
		return fmt.Errorf("bootstrap admin: disable seeded admin: %w", err)
	}
	log.Info().Str("user", adminUser).Msg("custom admin provisioned from configuration; seeded admin disabled")
	return nil
}

// createAdmin inserts the named user in the Default domain (no-op if it already
// exists — first-boot seed) and grants it the standard admin role on the
// default project. All statements are idempotent and concurrency-safe.
func createAdmin(ctx context.Context, name, hash string) error {
	if _, err := database.DB.Exec(ctx,
		"INSERT INTO users (name, password_hash, enabled, domain_id) VALUES ($1, $2, true, $3) ON CONFLICT (name) DO NOTHING",
		name, hash, defaultDomainID,
	); err != nil {
		return fmt.Errorf("bootstrap admin: insert user %q: %w", name, err)
	}

	var id string
	if err := database.DB.QueryRow(ctx, "SELECT id FROM users WHERE name = $1", name).Scan(&id); err != nil {
		return fmt.Errorf("bootstrap admin: resolve user %q: %w", name, err)
	}
	return grantAdminRole(ctx, id)
}

// grantAdminRole grants the standard admin role on the default project to the
// given user. Idempotent and concurrency-safe.
func grantAdminRole(ctx context.Context, userID string) error {
	if _, err := database.DB.Exec(ctx,
		"INSERT INTO role_assignments (user_id, project_id, role_id) VALUES ($1, $2, $3) ON CONFLICT (user_id, project_id, role_id) DO NOTHING",
		userID, defaultProjectID, adminRoleID,
	); err != nil {
		return fmt.Errorf("bootstrap admin: grant admin role to %q: %w", userID, err)
	}
	return nil
}
