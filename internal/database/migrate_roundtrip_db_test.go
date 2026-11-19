package database

import (
	"context"
	"math"
	"strings"
	"testing"
)

// TestMigrationsRoundTrip pins the reversibility gate (constitution v1.3.0): an empty
// database migrated up, all the way down and up again — then its newest migration down
// and up — holds the schema and seed data of the first up. PostgreSQL goes down through
// 001, SQLite through its baseline's down file. The leg follows LIGHTSTACK_TEST_DB_URL;
// both run on scratch databases.
func TestMigrationsRoundTrip(t *testing.T) {
	ctx := context.Background()
	const migrations = "../../migrations"
	source := migrationsSource(t, "../../migrations", math.MaxUint64)
	url, _ := freshDatabase(t, "lightstack_roundtrip")
	model := func() schemaModel { return databaseModel(t, ctx, url, source) }

	up := func(step string) {
		t.Helper()
		if err := MigrateUp(url, migrations); err != nil {
			t.Fatalf("%s: up: %v", step, err)
		}
	}
	up("first")
	first := model()
	if len(first) < 50 {
		t.Fatalf("%d tables after the first up", len(first))
	}

	for steps := 0; ; steps++ {
		err := MigrateDown(url, migrations)
		if err != nil && strings.Contains(err.Error(), "no migrations to roll back") {
			break
		}
		if err != nil {
			t.Fatalf("down, step %d: %v", steps+1, err)
		}
		if steps > 1000 {
			t.Fatal("down never reached an empty database")
		}
	}
	if left := model(); len(left) != 0 {
		var names []string
		for n := range left {
			names = append(names, n)
		}
		t.Fatalf("tables left after going all the way down: %v", names)
	}

	up("second")
	if diffs := diffModels(first, model(), "first up", "second up"); len(diffs) > 0 {
		t.Fatalf("the schema after down and up again differs:\n%s", strings.Join(diffs, "\n"))
	}

	if err := MigrateDown(url, migrations); err != nil {
		t.Fatalf("newest down: %v", err)
	}
	up("third")
	if diffs := diffModels(first, model(), "first up", "newest down and up"); len(diffs) > 0 {
		t.Fatalf("the schema after the newest migration's down and up differs:\n%s", strings.Join(diffs, "\n"))
	}
}
