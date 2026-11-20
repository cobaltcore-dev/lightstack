package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 round 2, T060: migrations 084 (the network layout's records) and 085 (duplicate
// group names, one default per project) on both backends. TestSchemaParity and
// TestMigrationsRoundTrip cover both migrations' shape and reversibility; these pin what
// they mean to the code that reads them.

const (
	m084Project      = "00000000-0000-0000-0000-000000000002" // seeded by 002
	m084DefaultGroup = "00000000-0000-0000-0000-000000000030" // seeded by 002
)

// migrationLeg is one backend to migrate: a fresh SQLite file always, and a scratch
// PostgreSQL database when LIGHTSTACK_TEST_DB_URL names one.
type migrationLeg struct {
	name string
	url  string
}

func migrationLegs(t *testing.T, tag string) []migrationLeg {
	t.Helper()
	legs := []migrationLeg{{"sqlite", "sqlite://" + filepath.Join(t.TempDir(), tag+".db")}}
	if env := os.Getenv("LIGHTSTACK_TEST_DB_URL"); strings.HasPrefix(env, "postgres://") || strings.HasPrefix(env, "postgresql://") {
		name := "lightstack_" + tag + "_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
		legs = append(legs, migrationLeg{"postgres", scratchPostgres(t, env, name)})
	}
	return legs
}

// openLeg opens a leg's database through the backend-neutral layer, so the checks below
// are written once in the shared SQL subset.
func openLeg(t *testing.T, url string) DB {
	t.Helper()
	pool := *DefaultPoolConfig()
	pool.MaxConns, pool.MinConns = 2, 1
	db, err := Open(context.Background(), url, Options{Pool: pool})
	if err != nil {
		t.Fatalf("open %s: %v", url, err)
	}
	return db
}

func migrateLegTo(t *testing.T, url string, version uint) {
	t.Helper()
	if err := MigrateToVersion(url, "../../migrations", version); err != nil {
		t.Fatalf("migrate to %03d: %v", version, err)
	}
}

func mustExec(t *testing.T, db DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func countLegRows(t *testing.T, db DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// insertNetworkAndSubnet creates a network with one /24 subnet in the seeded project.
func insertNetworkAndSubnet(t *testing.T, db DB) (networkID, subnetID string) {
	t.Helper()
	networkID, subnetID = uuid.NewString(), uuid.NewString()
	mustExec(t, db, `INSERT INTO networks (id, name, project_id) VALUES ($1, 'm084-net', $2)`, networkID, m084Project)
	mustExec(t, db, `INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version)
		VALUES ($1, 'm084-subnet', $2, $3, '10.84.0.0/24', '10.84.0.1', 4)`, subnetID, networkID, m084Project)
	return networkID, subnetID
}

func TestMigration084(t *testing.T) {
	ctx := context.Background()
	for _, leg := range migrationLegs(t, "m084") {
		t.Run(leg.name, func(t *testing.T) {
			migrateLegTo(t, leg.url, 84)
			db := openLeg(t, leg.url)

			t.Run("allocation pools follow their subnet", func(t *testing.T) {
				_, subnetID := insertNetworkAndSubnet(t, db)
				mustExec(t, db, `INSERT INTO subnet_allocation_pools (subnet_id, first_ip, last_ip)
					VALUES ($1, '10.84.0.10', '10.84.0.20'), ($1, '10.84.0.30', '10.84.0.40')`, subnetID)
				var first, last string
				if err := db.QueryRow(ctx, `SELECT id, first_ip, last_ip FROM subnet_allocation_pools
					WHERE subnet_id = $1 ORDER BY first_ip LIMIT 1`, subnetID).Scan(new(string), &first, &last); err != nil {
					t.Fatalf("read a pool back: %v", err)
				}
				if first != "10.84.0.10" || last != "10.84.0.20" {
					t.Errorf("pool read back as %s-%s; want 10.84.0.10-10.84.0.20", first, last)
				}
				var stored *bool
				if err := db.QueryRow(ctx, `SELECT pools_stored FROM subnets WHERE id = $1`, subnetID).Scan(&stored); err != nil {
					t.Fatalf("subnets.pools_stored: %v", err)
				}
				if stored != nil {
					t.Errorf("pools_stored of a subnet that never set it = %v; want NULL (the default pool, FR-032a)", *stored)
				}
				mustExec(t, db, `UPDATE subnets SET pools_stored = $1 WHERE id = $2`, true, subnetID)
				mustExec(t, db, `DELETE FROM subnets WHERE id = $1`, subnetID)
				if n := countLegRows(t, db, `SELECT count(*) FROM subnet_allocation_pools WHERE subnet_id = $1`, subnetID); n != 0 {
					t.Errorf("%d pools left after their subnet was deleted; want them deleted with it", n)
				}
			})

			t.Run("a port records its binding", func(t *testing.T) {
				networkID, _ := insertNetworkAndSubnet(t, db)
				portID := uuid.NewString()
				mustExec(t, db, `INSERT INTO ports (id, network_id, project_id) VALUES ($1, $2, $3)`, portID, networkID, m084Project)
				var host *string
				if err := db.QueryRow(ctx, `SELECT binding_host_id FROM ports WHERE id = $1`, portID).Scan(&host); err != nil {
					t.Fatalf("ports.binding_host_id: %v", err)
				}
				if host != nil {
					t.Errorf("binding_host_id of a new port = %q; want NULL", *host)
				}
				mustExec(t, db, `UPDATE ports SET binding_host_id = $1 WHERE id = $2`, "node-a", portID)
				if err := db.QueryRow(ctx, `SELECT binding_host_id FROM ports WHERE id = $1`, portID).Scan(&host); err != nil || host == nil || *host != "node-a" {
					t.Errorf("binding_host_id read back as %v, %v; want node-a", host, err)
				}
			})

			t.Run("a router gateway port has no project", func(t *testing.T) {
				// As upstream's (FR-038); 079 refused a port without one.
				networkID, _ := insertNetworkAndSubnet(t, db)
				mustExec(t, db, `INSERT INTO ports (id, network_id, project_id, device_owner) VALUES ($1, $2, NULL, 'network:router_gateway')`,
					uuid.NewString(), networkID)
			})

			t.Run("a connection-tracking zone belongs to one network", func(t *testing.T) {
				a, _ := insertNetworkAndSubnet(t, db)
				b, _ := insertNetworkAndSubnet(t, db)
				c, _ := insertNetworkAndSubnet(t, db)
				// Networks without a zone, as every network from before 084 is, may be many.
				if n := countLegRows(t, db, `SELECT count(*) FROM networks WHERE id IN ($1, $2, $3) AND ct_zone IS NULL`, a, b, c); n != 3 {
					t.Fatalf("%d of 3 new networks have no zone; want all three", n)
				}
				mustExec(t, db, `UPDATE networks SET ct_zone = 4097 WHERE id = $1`, a)
				mustExec(t, db, `UPDATE networks SET ct_zone = 4098 WHERE id = $1`, b)
				_, err := db.Exec(ctx, `UPDATE networks SET ct_zone = 4097 WHERE id = $1`, c)
				if !IsUniqueViolation(err) {
					t.Errorf("a second network taking zone 4097 = %v; want a unique violation", err)
				}
			})

			t.Run("a group carries a revision and is stateful unless told otherwise", func(t *testing.T) {
				id := uuid.NewString()
				mustExec(t, db, `INSERT INTO security_groups (id, name, project_id) VALUES ($1, 'm084-web', $2)`, id, m084Project)
				var revision int
				var stateful bool
				if err := db.QueryRow(ctx, `SELECT revision_number, stateful FROM security_groups WHERE id = $1`, id).Scan(&revision, &stateful); err != nil {
					t.Fatalf("read revision_number and stateful: %v", err)
				}
				if revision != 0 || !stateful {
					t.Errorf("a new group has revision %d, stateful %v; want 0, true (FR-050, FR-051)", revision, stateful)
				}
				mustExec(t, db, `UPDATE security_groups SET stateful = $1, revision_number = revision_number + 1 WHERE id = $2`, false, id)
				if err := db.QueryRow(ctx, `SELECT revision_number, stateful FROM security_groups WHERE id = $1`, id).Scan(&revision, &stateful); err != nil {
					t.Fatal(err)
				}
				if revision != 1 || stateful {
					t.Errorf("after the update: revision %d, stateful %v; want 1, false", revision, stateful)
				}
			})

			t.Run("the seeded default admits its own members", func(t *testing.T) {
				rows, err := db.Query(ctx, `SELECT ethertype, COALESCE(remote_ip_prefix, '') FROM security_group_rules
					WHERE security_group_id = $1 AND direction = 'ingress' AND remote_group_id = $1
					ORDER BY ethertype`, m084DefaultGroup)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for rows.Next() {
					var ethertype, prefix string
					if err := rows.Scan(&ethertype, &prefix); err != nil {
						t.Fatal(err)
					}
					if prefix != "" {
						t.Errorf("the %s member rule carries remote_ip_prefix %q; want none (cloud-storage#528)", ethertype, prefix)
					}
					got = append(got, ethertype)
				}
				rows.Close()
				if strings.Join(got, ",") != "IPv4,IPv6" {
					t.Errorf("default's ingress rules with itself as remote group: %v; want IPv4 and IPv6 (FR-043)", got)
				}
				// Still no ingress rule open to every address.
				if n := countLegRows(t, db, `SELECT count(*) FROM security_group_rules WHERE security_group_id = $1
					AND direction = 'ingress' AND remote_group_id IS NULL AND remote_ip_prefix IS NULL`, m084DefaultGroup); n != 0 {
					t.Errorf("default has %d ingress rules open to every address; want none", n)
				}
			})

			db.Close()

			t.Run("down removes all of it", func(t *testing.T) {
				migrateLegTo(t, leg.url, 83)
				db := openLeg(t, leg.url)
				defer db.Close()
				for _, probe := range []string{
					`SELECT count(*) FROM subnet_allocation_pools`,
					`SELECT count(pools_stored) FROM subnets`,
					`SELECT count(binding_host_id) FROM ports`,
					`SELECT count(ct_zone) FROM networks`,
					`SELECT count(revision_number) FROM security_groups`,
					`SELECT count(stateful) FROM security_groups`,
				} {
					var n int
					if err := db.QueryRow(ctx, probe).Scan(&n); err == nil {
						t.Errorf("%s still succeeds after 084's down; want the table or column gone", probe)
					}
				}
				if n := countLegRows(t, db, `SELECT count(*) FROM security_group_rules
					WHERE security_group_id = $1 AND direction = 'ingress'`, m084DefaultGroup); n != 0 {
					t.Errorf("%d ingress rules left on default after 084's down; want the two member rules gone", n)
				}
				if n := countLegRows(t, db, `SELECT count(*) FROM security_group_rules
					WHERE security_group_id = $1 AND direction = 'egress'`, m084DefaultGroup); n != 2 {
					t.Errorf("%d egress rules on default after 084's down; want 002's two kept", n)
				}
				if n := countLegRows(t, db, `SELECT count(*) FROM ports WHERE project_id IS NULL`); n != 0 {
					t.Errorf("%d ports without a project after 084's down; want the gateway ports gone", n)
				}
				if _, err := db.Exec(ctx, `INSERT INTO ports (id, project_id) VALUES ($1, NULL)`, uuid.NewString()); err == nil {
					t.Error("a port without a project is accepted after 084's down; want 079's refusal back")
				}
			})
		})
	}
}

func TestMigration085(t *testing.T) {
	ctx := context.Background()
	for _, leg := range migrationLegs(t, "m085") {
		t.Run(leg.name, func(t *testing.T) {
			migrateLegTo(t, leg.url, 84)

			// Rows that reference security_groups, written before the rebuild: a group with a
			// rule naming another group as its remote, and ports carrying both.
			db := openLeg(t, leg.url)
			web, db2 := uuid.NewString(), uuid.NewString()
			mustExec(t, db, `INSERT INTO security_groups (id, name, project_id, revision_number, stateful)
				VALUES ($1, 'm085-web', $3, 4, $4), ($2, 'm085-db', $3, 0, $5)`, web, db2, m084Project, false, true)
			mustExec(t, db, `INSERT INTO security_group_rules (security_group_id, direction, ethertype, protocol, port_range_min, port_range_max)
				VALUES ($1, 'ingress', 'IPv4', 'tcp', 80, 80)`, web)
			mustExec(t, db, `INSERT INTO security_group_rules (security_group_id, direction, ethertype, remote_group_id)
				VALUES ($1, 'ingress', 'IPv4', $2)`, db2, web)
			networkID, _ := insertNetworkAndSubnet(t, db)
			ports := []string{uuid.NewString(), uuid.NewString()}
			for _, p := range ports {
				mustExec(t, db, `INSERT INTO ports (id, network_id, project_id) VALUES ($1, $2, $3)`, p, networkID, m084Project)
				mustExec(t, db, `INSERT INTO port_security_groups (port_id, security_group_id) VALUES ($1, $2), ($1, $3)`, p, web, db2)
			}
			before := map[string]int{
				"security_groups":      countLegRows(t, db, `SELECT count(*) FROM security_groups`),
				"security_group_rules": countLegRows(t, db, `SELECT count(*) FROM security_group_rules`),
				"port_security_groups": countLegRows(t, db, `SELECT count(*) FROM port_security_groups`),
			}
			db.Close()

			migrateLegTo(t, leg.url, 85)
			db = openLeg(t, leg.url)
			defer db.Close()

			t.Run("every referencing row survives", func(t *testing.T) {
				for table, want := range before {
					if got := countLegRows(t, db, `SELECT count(*) FROM `+table); got != want {
						t.Errorf("%s holds %d rows after 085; want the %d it held before", table, got, want)
					}
				}
				var revision int
				var stateful bool
				if err := db.QueryRow(ctx, `SELECT revision_number, stateful FROM security_groups WHERE id = $1`, web).Scan(&revision, &stateful); err != nil {
					t.Fatal(err)
				}
				if revision != 4 || stateful {
					t.Errorf("m085-web after 085: revision %d, stateful %v; want 4, false", revision, stateful)
				}
			})

			t.Run("names may repeat", func(t *testing.T) {
				mustExec(t, db, `INSERT INTO security_groups (id, name, project_id) VALUES ($1, 'm085-dup', $3), ($2, 'm085-dup', $3)`,
					uuid.NewString(), uuid.NewString(), m084Project)
				if n := countLegRows(t, db, `SELECT count(*) FROM security_groups WHERE project_id = $1 AND name = 'm085-dup'`, m084Project); n != 2 {
					t.Errorf("%d groups named m085-dup; want 2 (FR-026)", n)
				}
			})

			t.Run("one default per project", func(t *testing.T) {
				_, err := db.Exec(ctx, `INSERT INTO security_groups (id, name, project_id) VALUES ($1, 'default', $2)`, uuid.NewString(), m084Project)
				if !IsUniqueViolation(err) {
					t.Errorf("a second default in the seeded project = %v; want a unique violation", err)
				}
				// Another project's default is its own.
				other := uuid.NewString()
				mustExec(t, db, `INSERT INTO projects (id, name) VALUES ($1, $2)`, other, "m085-"+other[:8])
				mustExec(t, db, `INSERT INTO security_groups (id, name, project_id) VALUES ($1, 'default', $2)`, uuid.NewString(), other)
			})
		})
	}
}
