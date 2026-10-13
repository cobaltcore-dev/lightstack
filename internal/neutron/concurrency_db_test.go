package neutron

import (
	"context"
	"fmt"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// TestNeutronConcurrencyMatrix runs networking's transactions under concurrent callers,
// once per SQLite transaction mode (FR-039). The table it logs decides the default
// (FR-010, research R4); the default must hold in every case.
func TestNeutronConcurrencyMatrix(t *testing.T) {
	requireDB(t)
	const n = 8
	freshProject := func(t *testing.T) string {
		project := uuid.NewString()
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO projects (id, name, description, enabled, domain_id)
			 VALUES ($1, $2, '', true, (SELECT id FROM domains WHERE name = 'Default'))`, project, "conc-"+project[:8]); err != nil {
			t.Fatalf("create a project: %v", err)
		}
		t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, project) })
		return project
	}
	cases := []dbtest.ConcurrencyCase{
		{Name: "default security group", Run: func(t *testing.T, db database.DB) string {
			project := freshProject(t)
			ids := make([]string, n)
			errs := make([]error, n)
			dbtest.Concurrently(n, func(i int) {
				ids[i], errs[i] = EnsureDefaultSecurityGroup(context.Background(), db, project)
			})
			for i := range n {
				if errs[i] != nil {
					return errs[i].Error()
				}
				if ids[i] != ids[0] {
					return "callers got different groups"
				}
			}
			var groups int
			if err := testPool.QueryRow(context.Background(),
				`SELECT count(*) FROM security_groups WHERE project_id = $1 AND name = 'default'`, project).Scan(&groups); err != nil || groups != 1 {
				return fmt.Sprintf("%d default groups (%v)", groups, err)
			}
			return ""
		}},
		{Name: "port allocation for a server", Run: func(t *testing.T, db database.DB) string {
			netID := seedNetwork(t, "conc-"+uuid.NewString()[:8])
			gw := "10.97.0.1"
			seedSubnet(t, netID, "conc", "10.97.0.0/24", &gw, 4, false)
			svc := NewService(db, "stub", nil)
			errs := make([]error, n)
			dbtest.Concurrently(n, func(i int) {
				_, errs[i] = svc.AllocatePortForInstance(context.Background(), netID, seededProjectID, uuid.NewString(), nil, false)
			})
			for _, err := range errs {
				if err != nil {
					return err.Error()
				}
			}
			return ""
		}},
	}
	for name, failure := range dbtest.RunConcurrencyMatrix(t, cases)["immediate"] {
		if failure != "" {
			t.Errorf("immediate transactions, %s: %s", name, failure)
		}
	}
}
