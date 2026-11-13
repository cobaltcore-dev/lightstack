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

func TestSQLiteServerGroupMigrationPreservesDuplicates(t *testing.T) {
	ctx := context.Background()
	db, url := migratedSQLite(t, 77)
	var projectID string
	if err := db.QueryRowContext(ctx, "SELECT id FROM projects ORDER BY id LIMIT 1").Scan(&projectID); err != nil {
		t.Fatalf("find seeded project: %v", err)
	}
	for _, row := range []struct{ id, members string }{
		{"duplicate-a", "[\"member-a\",\"shared\"]"},
		{"duplicate-b", "[\"member-b\",\"shared\"]"},
	} {
		if _, err := db.ExecContext(ctx, "INSERT INTO server_groups (id, name, policies, members, project_id) VALUES (?, 'migration-duplicate', '[]', ?, ?)",
			row.id, row.members, projectID); err != nil {
			t.Fatalf("seed duplicate server group: %v", err)
		}
	}
	if err := MigrateToVersion(url, "../../migrations", 78); err != nil {
		t.Fatalf("migrate server-group uniqueness: %v", err)
	}
	for _, row := range []struct{ id, members string }{
		{"duplicate-a", "[\"member-a\",\"shared\"]"},
		{"duplicate-b", "[\"member-b\",\"shared\"]"},
	} {
		var members string
		if err := db.QueryRowContext(ctx, "SELECT members FROM server_groups WHERE id=?", row.id).Scan(&members); err != nil || members != row.members {
			t.Fatalf("group %s changed during upgrade: members=%s err=%v", row.id, members, err)
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO server_groups (id, name, policies, members, project_id) VALUES ('duplicate-c', 'migration-duplicate', '[]', '[]', ?)", projectID); err != nil {
		t.Fatalf("duplicate group name rejected: %v", err)
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
