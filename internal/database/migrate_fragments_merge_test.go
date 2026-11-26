package database

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFragmentsMergeInEitherOrder pins SPEC-032 SC-002: two schema changes made in parallel from
// one commit merge without a conflict, in either order, into the same tree; and every database
// gets both — a new one, and one migrated before the merge.
func TestFragmentsMergeInEitherOrder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git on PATH")
	}
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		// Without the caller's git configuration: signing, hooks or another default branch would
		// change what the test does.
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commitFragment := func(branch, name, table string) {
		t.Helper()
		writeFiles(t, repo, map[string]string{
			"migrations/next/" + name + ".up.sql":   "CREATE TABLE " + table + " (id TEXT);\n",
			"migrations/next/" + name + ".down.sql": "DROP TABLE " + table + ";\n",
		})
		git("add", "-A")
		git("commit", "-qm", branch)
	}

	// The base commit: the numbered migrations and an empty next/.
	numbered, _ := copyMigrationFiles(t)
	if err := os.CopyFS(filepath.Join(repo, "migrations"), os.DirFS(numbered)); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, repo, map[string]string{"migrations/next/.gitkeep": ""})
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-qm", "base")
	base := git("rev-parse", "HEAD")

	// Two changes made in parallel from the base, neither aware of the other.
	git("checkout", "-q", "-b", "a", base)
	commitFragment("a", "20990101000000-10-merge-a", "merge_a")
	git("checkout", "-q", "-b", "b", base)
	commitFragment("b", "20990101000001-11-merge-b", "merge_b")

	// Merged in either order: no conflict (git merge fails the test on one), the same tree.
	for _, order := range [][3]string{{"ab", "a", "b"}, {"ba", "b", "a"}} {
		git("checkout", "-q", "-b", order[0], base)
		git("merge", "-q", "--no-edit", order[1])
		git("merge", "-q", "--no-edit", order[2])
	}
	if ab, ba := git("rev-parse", "ab^{tree}"), git("rev-parse", "ba^{tree}"); ab != ba {
		t.Fatalf("merging a then b gives tree %s, b then a %s; want one tree", ab, ba)
	}

	// A database migrated at the base, before either change, and a new one: both get both.
	migrations := filepath.Join(repo, "migrations")
	git("checkout", "-q", base)
	older, _ := freshDatabase(t, "lightstack_merge_older")
	if err := MigrateUp(older, migrations); err != nil {
		t.Fatalf("migrate at the base: %v", err)
	}
	git("checkout", "-q", "ab")
	fresh, _ := freshDatabase(t, "lightstack_merge_fresh")
	source := migrationsSource(t, migrations, math.MaxUint64)
	for name, url := range map[string]string{"a database migrated at the base": older, "a new database": fresh} {
		if err := MigrateUp(url, migrations); err != nil {
			t.Fatalf("%s: migrate after the merge: %v", name, err)
		}
		m := databaseModel(t, ctx, url, source)
		if m["merge_a"] == nil || m["merge_b"] == nil {
			t.Errorf("%s lacks a merged change: merge_a %v, merge_b %v", name, m["merge_a"] != nil, m["merge_b"] != nil)
		}
	}
}
