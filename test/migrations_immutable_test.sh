#!/bin/bash
# SPEC-032 FR-025: scripts/check-migrations-immutable.sh fails a change to the baseline or to a
# release migration — a rename counts as a deletion — and passes fragments, a new release migration
# and the squash change, which moves the old history away. Runs in a temporary git repository,
# without root. Usage: bash test/migrations_immutable_test.sh
set -u

# The caller's git configuration (signing, hooks, default branch) must not change what this does.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$REPO_ROOT/scripts/check-migrations-immutable.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASSED=0
FAILED=0
pass() { echo "✓ $1"; PASSED=$((PASSED + 1)); }
fail() { echo "✗ $1"; FAILED=$((FAILED + 1)); }

REPO="$WORK/repo"
g() { git -C "$REPO" "$@"; }
mkdir -p "$REPO/migrations/next"
g init -q -b main
g config user.email test@example.invalid
g config user.name test
for f in 001_baseline.postgres.up.sql 001_baseline.sqlite.up.sql 002_v0.7.0.up.sql 002_v0.7.0.down.sql 044_volume_groups.up.sql; do
    echo "-- $f" > "$REPO/migrations/$f"
done
: > "$REPO/migrations/next/.gitkeep"
g add -A
g commit -qm base

# check DESCRIPTION WANT_EXIT [EXPECTED_ANNOTATED_FILE] changes the tree from base on a new branch
# with the commands on stdin, commits, and runs the check against base.
check() {
    local description=$1 want=$2 annotated=${3:-} out rc
    g checkout -q -B case main
    (cd "$REPO" && bash -s)
    g add -A
    g commit -qm "$description" --allow-empty
    out=$(cd "$REPO" && "$CHECK" main 2>&1); rc=$?
    if [ "$rc" -ne "$want" ]; then
        fail "$description: exit $rc, want $want: $out"
    elif [ -n "$annotated" ] && ! grep -q "::error file=$annotated::numbered migrations are immutable once on main" <<<"$out"; then
        fail "$description: no annotation for $annotated: $out"
    else
        pass "$description"
    fi
}

check "the baseline edited" 1 migrations/001_baseline.postgres.up.sql <<'EOF'
echo "-- edited" >> migrations/001_baseline.postgres.up.sql
EOF
check "a release migration renamed" 1 migrations/002_v0.7.0.up.sql <<'EOF'
git mv migrations/002_v0.7.0.up.sql migrations/002_v0.7.1.up.sql
EOF
check "a release migration deleted" 1 migrations/002_v0.7.0.down.sql <<'EOF'
git rm -q migrations/002_v0.7.0.down.sql
EOF
check "a fragment added" 0 <<'EOF'
echo "CREATE TABLE x (id TEXT);" > migrations/next/20990101000000-1-x.up.sql
echo "DROP TABLE x;" > migrations/next/20990101000000-1-x.down.sql
EOF
check "a new release migration" 0 <<'EOF'
echo "-- v0.8.0" > migrations/003_v0.8.0.up.sql
echo "-- v0.8.0" > migrations/003_v0.8.0.down.sql
EOF
check "the history moved away, as the squash does" 0 <<'EOF'
mkdir -p history && git mv migrations/044_volume_groups.up.sql history/
EOF

echo
echo "passed: $PASSED, failed: $FAILED"
[ "$FAILED" -eq 0 ]
