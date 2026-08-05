package neutron

import (
	"context"
	"errors"
	"fmt"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EnsureDefaultSecurityGroup returns the id of the project's `default` security
// group, creating it — with its default rules — only if it does not exist yet.
// OpenStack guarantees every project has a default security group (Neutron creates
// it lazily on first use); lightstack only seeds one for the seeded default project,
// so this closes that gap on demand for every caller that needs the group to exist:
// Nova at server create (explicit `default` or no group supplied) and Neutron at
// port creation.
//
// Idempotence rests on one statement: INSERT … ON CONFLICT DO NOTHING RETURNING id.
// A returned row means this call created the group, and only then are the seed
// rules written (mirroring migrations/002_seed_data.up.sql: egress IPv4/IPv6
// allow-all, ingress tcp/22). No row means the group already existed — created by
// a concurrent caller, via the API, or seeded — and it is left exactly as found:
// an operator who removed rules from `default` keeps that decision. Concurrent
// first use is serialized by the unique index on (project_id, name): the second
// inserter blocks until the first commits, then takes the "existing" branch.
func EnsureDefaultSecurityGroup(ctx context.Context, projectID string) (string, error) {
	var sgID string
	if err := database.WithTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO security_groups (id, name, project_id, description)
			 VALUES ($1, 'default', $2, 'Default security group')
			 ON CONFLICT (project_id, name) DO NOTHING
			 RETURNING id`,
			uuid.New().String(), projectID).Scan(&sgID)
		if errors.Is(err, pgx.ErrNoRows) {
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
		// Freshly created by this call: seed the default rules. No ON CONFLICT
		// here — security_group_rules has no unique constraint on the rule shape —
		// and none is needed, because only the creating transaction reaches this.
		if _, err := tx.Exec(ctx, `
			INSERT INTO security_group_rules (security_group_id, direction, ethertype, protocol, port_range_min, port_range_max)
			VALUES
				($1, 'egress', 'IPv4', NULL, NULL, NULL),
				($1, 'egress', 'IPv6', NULL, NULL, NULL),
				($1, 'ingress', 'IPv4', 'tcp', 22, 22)`, sgID); err != nil {
			return fmt.Errorf("seed default rules for %s: %w", sgID, err)
		}
		return nil
	}); err != nil {
		return "", err
	}
	return sgID, nil
}
