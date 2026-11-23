package neutron

import (
	"context"
	"errors"
	"fmt"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ensureDefaultOf ensures the default group of the project a group or rule listing, or a group's create,
// concerns, as upstream's get_security_groups, get_security_group_rules and create_security_group do
// (SPEC-033 T193). A project that does not exist has none.
func (svc *Service) ensureDefaultOf(ctx context.Context, projectID string) error {
	var exists bool
	if err := svc.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id::text = $1)", projectID).Scan(&exists); err != nil {
		return fmt.Errorf("read project %s: %w", projectID, err)
	}
	if !exists {
		return nil
	}
	_, err := EnsureDefaultSecurityGroup(ctx, svc.pool, projectID)
	return err
}

// listedProject is the project a group or rule listing concerns, upstream's: the first one an
// administrator names (project_id, else tenant_id), else the caller's.
func listedProject(c *gin.Context) string {
	if isAdmin(c) {
		for _, key := range []string{"project_id", "tenant_id"} {
			if named := nonEmpty(c.QueryArray(key)); len(named) > 0 {
				return named[0]
			}
		}
	}
	return c.GetString("project_id")
}

// EnsureDefaultSecurityGroup returns the id of the project's `default` security
// group, creating it — with its default rules — only if it does not exist yet.
// OpenStack guarantees every project has a default security group (Neutron creates
// it lazily on first use); lightstack only seeds one for the seeded default project,
// so this closes that gap on demand for every caller that needs the group to exist:
// Nova at server create (explicit `default` or no group supplied) and Neutron at
// port creation.
//
// Idempotence rests on one statement: INSERT … ON CONFLICT DO NOTHING RETURNING id.
// A returned row means this call created the group, and only then are its rules
// written. No row means the group already existed — created by a concurrent caller,
// via the API, or seeded — and it is left exactly as found: an operator who removed
// rules from `default` keeps that decision. Concurrent first use is serialized by the
// partial unique index that allows one `default` per project (migration 085,
// security_groups_one_default): the second inserter blocks until the first commits,
// then takes the "existing" branch. Other names may repeat, as upstream's do (SPEC-033
// FR-026).
//
// The rules are upstream's for a default group (SPEC-033 FR-043): egress open to
// everything, IPv4 and IPv6, and ingress from the group's own members, IPv4 and IPv6.
// No ingress rule is open to every address: upstream's default denies inbound traffic
// from anywhere else, and a rule carrying neither remote_ip_prefix nor remote_group_id
// would be enforced as 0.0.0.0/0, opening that port to the world for every project
// (cloud-storage#528). The member rules name the group itself as their remote.
func EnsureDefaultSecurityGroup(ctx context.Context, pool database.DB, projectID string) (string, error) {
	// Read first, outside a transaction: once the group exists, a listing that ensures it writes nothing
	// and, on SQLite, takes no write lock (SPEC-033 T193).
	var sgID string
	err := pool.QueryRow(ctx, "SELECT id::text FROM security_groups WHERE name = 'default' AND project_id::text = $1", projectID).Scan(&sgID)
	if err == nil {
		return sgID, nil
	}
	if !errors.Is(err, database.ErrNoRows) {
		return "", fmt.Errorf("read default security group: %w", err)
	}
	if err := database.WithTx(ctx, pool, func(tx database.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO security_groups (id, name, project_id, description)
			 VALUES ($1, 'default', $2, 'Default security group')
			 ON CONFLICT (project_id) WHERE name = 'default' DO NOTHING
			 RETURNING id`,
			uuid.New().String(), projectID).Scan(&sgID)
		if errors.Is(err, database.ErrNoRows) {
			// Existing group: resolve its id and leave its rules alone.
			if err := tx.QueryRow(ctx,
				"SELECT id FROM security_groups WHERE name = 'default' AND project_id = $1",
				projectID,
			).Scan(&sgID); err != nil {
				return fmt.Errorf("re-select default security group id: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert default security group: %w", err)
		}
		// Freshly created by this call: seed its rules. No ON CONFLICT
		// here — security_group_rules has no unique constraint on the rule shape —
		// and none is needed, because only the creating transaction reaches this.
		if err := seedDefaultSecurityGroupRules(ctx, tx, sgID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO security_group_rules (security_group_id, direction, ethertype, remote_group_id)
			VALUES ($1, 'ingress', 'IPv4', $1), ($1, 'ingress', 'IPv6', $1)`, sgID); err != nil {
			return fmt.Errorf("seed default member rules for %s: %w", sgID, err)
		}
		return nil
	}); err != nil {
		return "", err
	}
	return sgID, nil
}

// seedDefaultSecurityGroupRules writes upstream's starting rules for a newly created group: egress
// open to everything, IPv4 and IPv6, and no ingress (SPEC-033 FR-043). A project's default also admits
// its own members, which EnsureDefaultSecurityGroup adds after these. Callers run it in the same
// transaction as group creation, so no successful create returns a group without its rules.
func seedDefaultSecurityGroupRules(ctx context.Context, tx database.Tx, sgID string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO security_group_rules (security_group_id, direction, ethertype, protocol, port_range_min, port_range_max)
		VALUES
			($1, 'egress', 'IPv4', NULL, NULL, NULL),
			($1, 'egress', 'IPv6', NULL, NULL, NULL)`, sgID); err != nil {
		return fmt.Errorf("seed default rules for %s: %w", sgID, err)
	}
	return nil
}
