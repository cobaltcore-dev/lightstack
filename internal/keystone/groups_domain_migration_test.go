package keystone

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Migration 072 makes groups.domain_id what users.domain_id and projects.domain_id already
// are: a UUID referencing domains(id). Migration 010 created it as VARCHAR(255) DEFAULT
// 'default' with no foreign key, and the UUID definitions in 019 and 028 never applied —
// both are CREATE TABLE IF NOT EXISTS against the table 010 had created. Until SPEC-022 the
// group handler stored whatever domain_id it was given: the `default` alias, or a string
// naming no domain at all.
const (
	groupsDomainUp   = "../../migrations/072_groups_domain_id_uuid.up.sql"
	groupsDomainDown = "../../migrations/072_groups_domain_id_uuid.down.sql"
)

// TestGroupsDomainIDReferencesDomains pins the end state on the migrated test database.
func TestGroupsDomainIDReferencesDomains(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}

	var dataType, nullable string
	if err := testPool.QueryRow(t.Context(),
		`SELECT data_type, is_nullable FROM information_schema.columns
		  WHERE table_name = 'groups' AND column_name = 'domain_id'`,
	).Scan(&dataType, &nullable); err != nil {
		t.Fatalf("read groups.domain_id: %v", err)
	}
	if dataType != "uuid" || nullable != "NO" {
		t.Fatalf("groups.domain_id is %s (nullable %s), want uuid NOT NULL", dataType, nullable)
	}

	var refTable, onDelete string
	if err := testPool.QueryRow(t.Context(),
		`SELECT c.confrelid::regclass::text, c.confdeltype::text
		   FROM pg_constraint c
		  WHERE c.conrelid = 'groups'::regclass AND c.contype = 'f'
		    AND c.conkey = ARRAY[(SELECT attnum FROM pg_attribute
		                           WHERE attrelid = 'groups'::regclass AND attname = 'domain_id')]`,
	).Scan(&refTable, &onDelete); err != nil {
		t.Fatalf("groups.domain_id has no foreign key: %v", err)
	}
	if refTable != "domains" || onDelete != "c" {
		t.Fatalf("groups.domain_id references %s (on delete %q), want domains ON DELETE CASCADE", refTable, onDelete)
	}
}

// migrateLegacyGroups runs 072's down migration — back to the VARCHAR column 010 created —
// lets seed write rows the way the old handler could, then runs the up migration and
// returns its error. Everything happens in one transaction that is rolled back, so the
// shared test database keeps its schema.
func migrateLegacyGroups(t *testing.T, seed func(tx pgx.Tx)) (pgx.Tx, error) {
	t.Helper()
	up, err := os.ReadFile(groupsDomainUp)
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	down, err := os.ReadFile(groupsDomainDown)
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}

	tx, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// context.Background(), not t.Context(): the test context is cancelled just before
	// Cleanup-registered functions run (testing.T.Context).
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	if _, err := tx.Exec(t.Context(), string(down)); err != nil {
		t.Fatalf("down migration: %v", err)
	}
	seed(tx)
	_, err = tx.Exec(t.Context(), string(up))
	return tx, err
}

func insertLegacyGroup(t *testing.T, tx pgx.Tx, name string, domainID any) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), `INSERT INTO groups (name, domain_id) VALUES ($1, $2)`, name, domainID); err != nil {
		t.Fatalf("insert legacy group %q: %v", name, err)
	}
}

// TestGroupsDomainMigrationNormalisesTheAlias — the forms that meant the Default domain (the
// alias, an empty or missing reference) become its id, and an id in another case becomes
// the canonical form.
func TestGroupsDomainMigrationNormalisesTheAlias(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	var defaultID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM domains WHERE name = 'Default'`).Scan(&defaultID); err != nil {
		t.Fatalf("read the Default domain: %v", err)
	}

	tx, err := migrateLegacyGroups(t, func(tx pgx.Tx) {
		insertLegacyGroup(t, tx, "spec022-legacy-alias", "default")
		insertLegacyGroup(t, tx, "spec022-legacy-empty", "")
		insertLegacyGroup(t, tx, "spec022-legacy-null", nil)
		insertLegacyGroup(t, tx, "spec022-legacy-upper", strings.ToUpper(defaultID))
	})
	if err != nil {
		t.Fatalf("up migration over legacy rows: %v", err)
	}

	rows, err := tx.Query(t.Context(), `SELECT name, domain_id::text FROM groups WHERE name LIKE 'spec022-legacy-%' ORDER BY name`)
	if err != nil {
		t.Fatalf("read migrated rows: %v", err)
	}
	defer rows.Close()
	var seen int
	for rows.Next() {
		var name, domainID string
		if err := rows.Scan(&name, &domainID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if domainID != defaultID {
			t.Errorf("%s: domain_id = %s, want the Default domain %s", name, domainID, defaultID)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != 4 {
		t.Fatalf("migrated %d legacy rows, want 4", seen)
	}
}

// TestGroupsDomainMigrationRefusesToGuess — what cannot be normalised without guessing stops
// the migration and names the rows: a value that names no domain, and a name the
// normalisation would put into one domain twice.
func TestGroupsDomainMigrationRefusesToGuess(t *testing.T) {
	if !testDBAvailable {
		t.Skip("LIGHTSTACK_TEST_DB_URL not set")
	}
	var defaultID string
	if err := testPool.QueryRow(t.Context(), `SELECT id::text FROM domains WHERE name = 'Default'`).Scan(&defaultID); err != nil {
		t.Fatalf("read the Default domain: %v", err)
	}

	for _, tc := range []struct {
		name   string
		rows   [][2]any
		naming string
	}{
		{"a string that names no domain", [][2]any{{"spec022-legacy-junk", "no-such-domain"}}, "spec022-legacy-junk"},
		{"an id with no domain behind it", [][2]any{{"spec022-legacy-orphan", "11111111-1111-1111-1111-111111111111"}}, "spec022-legacy-orphan"},
		{"one name twice in the Default domain", [][2]any{{"spec022-legacy-dup", "default"}, {"spec022-legacy-dup", defaultID}}, "spec022-legacy-dup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := migrateLegacyGroups(t, func(tx pgx.Tx) {
				for _, r := range tc.rows {
					insertLegacyGroup(t, tx, r[0].(string), r[1])
				}
			})
			if err == nil {
				t.Fatal("up migration succeeded; want it to stop rather than guess")
			}
			if !strings.Contains(err.Error(), tc.naming) {
				t.Fatalf("up migration stopped with %q; want the message to name %s", err, tc.naming)
			}
		})
	}
}
