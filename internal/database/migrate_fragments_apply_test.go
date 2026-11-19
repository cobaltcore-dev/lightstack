package database

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// demoFragments is a migrations folder (the real numbered files) with two fixture fragments: one
// creates the table demo, created at createAt; the other adds a column to it, created at alterAt.
func demoFragments(t *testing.T, createAt, alterAt string) string {
	t.Helper()
	dir, _ := copyMigrationFiles(t)
	writeFiles(t, dir, map[string]string{
		"next/" + createAt + "-1-create-demo.up.sql":   "CREATE TABLE demo (id TEXT);\n",
		"next/" + createAt + "-1-create-demo.down.sql": "DROP TABLE demo;\n",
		"next/" + alterAt + "-2-add-demo-col.up.sql":   "ALTER TABLE demo ADD COLUMN note TEXT;\n",
		"next/" + alterAt + "-2-add-demo-col.down.sql": "ALTER TABLE demo DROP COLUMN note;\n",
	})
	return dir
}

func columnNames(table *modelTable) []string {
	if table == nil {
		return nil
	}
	var names []string
	for _, c := range table.columns {
		names = append(names, c.name)
	}
	return names
}

// TestFragmentsApplyInOrder pins SPEC-032 FR-015 and FR-018 on the backend LIGHTSTACK_TEST_DB_URL
// selects: a database gets every fragment, in creation order, as one open release migration that
// rolls back as one, and the start logs it as unreleased. A fragment that sorts before one it
// depends on is named by the per-fragment replay CI runs (US3 scenario 5).
func TestFragmentsApplyInOrder(t *testing.T) {
	ctx := context.Background()

	t.Run("in creation order, up, down and up again", func(t *testing.T) {
		dir := demoFragments(t, "20990101000000", "20990101000001")
		url, _ := freshDatabase(t, "lightstack_fragments")
		var logs bytes.Buffer
		saved := log.Logger
		log.Logger = zerolog.New(&logs)
		t.Cleanup(func() { log.Logger = saved })

		if err := MigrateUp(url, dir); err != nil {
			t.Fatalf("MigrateUp: %v", err)
		}
		if !strings.Contains(logs.String(), "applied the open release migration 002 (unreleased)") {
			t.Errorf("MigrateUp did not log the unreleased open migration (FR-018):\n%s", logs.String())
		}
		source := migrationsSource(t, dir, math.MaxUint64)
		first := databaseModel(t, ctx, url, source)
		if got := strings.Join(columnNames(first["demo"]), ","); got != "id,note" {
			t.Fatalf("demo has columns %q; want both fragments applied: id,note", got)
		}
		if err := MigrateDown(url, dir); err != nil {
			t.Fatalf("MigrateDown: %v", err)
		}
		if _, ok := databaseModel(t, ctx, url, source)["demo"]; ok {
			t.Fatal("rolling back the open migration left demo behind")
		}
		if err := MigrateUp(url, dir); err != nil {
			t.Fatalf("MigrateUp after down: %v", err)
		}
		if diffs := diffModels(first, databaseModel(t, ctx, url, source), "first up", "up after down"); len(diffs) > 0 {
			t.Errorf("up, down, up changed the schema:\n%s", strings.Join(diffs, "\n"))
		}
	})

	t.Run("a fragment sorting before what it depends on is named", func(t *testing.T) {
		dir := demoFragments(t, "20990101000001", "20990101000000")
		url, _ := freshDatabase(t, "lightstack_fragments_order")
		err := replayFragments(t, ctx, url, dir)
		if err == nil || !strings.Contains(err.Error(), "fragment 20990101000000-2-add-demo-col.up.sql") {
			t.Fatalf("replay = %v; want the failure named after 20990101000000-2-add-demo-col", err)
		}
	})

	// The CI check: the repository's own fragments apply one by one, in name order, on this
	// backend. It passes trivially while migrations/next/ holds none.
	t.Run("the repository's fragments", func(t *testing.T) {
		url, _ := freshDatabase(t, "lightstack_fragments_repo")
		if err := replayFragments(t, ctx, url, "../../migrations"); err != nil {
			t.Fatal(err)
		}
	})
}
