package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func migratedSQLite(t *testing.T, target uint) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "migration.db")
	url := "sqlite://" + path
	if err := MigrateToVersion(url, "../../migrations", target); err != nil {
		t.Fatalf("migrate SQLite to %d: %v", target, err)
	}
	db, err := openSQLiteHandle(ctx, path, defaultSQLiteTxLock, true, PoolConfig{MaxConns: 1})
	if err != nil {
		t.Fatalf("open migrated SQLite database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, url
}

func TestSQLiteServerGroupMigrationDropsPreReleaseUniqueness(t *testing.T) {
	ctx := context.Background()
	db, url := migratedSQLite(t, 80)
	var projectID string
	if err := db.QueryRowContext(ctx, "SELECT id FROM projects ORDER BY id LIMIT 1").Scan(&projectID); err != nil {
		t.Fatalf("find seeded project: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE UNIQUE INDEX uq_server_groups_project_name ON server_groups(project_id, name)"); err != nil {
		t.Fatalf("seed pre-release uniqueness index: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO server_groups (id, name, policies, members, project_id) VALUES ('legacy-a', 'migration-duplicate', '[]', '[]', ?)", projectID); err != nil {
		t.Fatalf("seed existing server group: %v", err)
	}
	if err := MigrateToVersion(url, "../../migrations", 81); err != nil {
		t.Fatalf("drop pre-release server-group uniqueness: %v", err)
	}
	var name, policies, members string
	if err := db.QueryRowContext(ctx, "SELECT name, policies, members FROM server_groups WHERE id='legacy-a'").Scan(&name, &policies, &members); err != nil {
		t.Fatalf("read existing server group: %v", err)
	}
	if name != "migration-duplicate" || policies != "[]" || members != "[]" {
		t.Fatalf("migration changed existing group: name=%s policies=%s members=%s", name, policies, members)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO server_groups (id, name, policies, members, project_id) VALUES ('legacy-b', 'migration-duplicate', '[]', '[]', ?)", projectID); err != nil {
		t.Fatalf("duplicate group name still rejected after migration 081: %v", err)
	}
}

func TestSQLiteOwnerMigrationsRejectNullProjectIDs(t *testing.T) {
	ctx := context.Background()
	db, _ := migratedSQLite(t, 80)
	for _, tc := range []struct{ table, query string }{
		{"volumes", "INSERT INTO volumes (project_id, size_gb) VALUES (NULL, 1)"},
		{"ports", "INSERT INTO ports (project_id) VALUES (NULL)"},
		{"networks", "INSERT INTO networks (name, project_id) VALUES ('null-owner-network', NULL)"},
		{"routers", "INSERT INTO routers (name, project_id) VALUES ('null-owner-router', NULL)"},
		{"floating_ips", "INSERT INTO floating_ips (floating_ip_address, project_id) VALUES ('192.0.2.1', NULL)"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, tc.query); err == nil {
				t.Fatalf("%s accepted a NULL project_id", tc.table)
			}
		})
	}
}
