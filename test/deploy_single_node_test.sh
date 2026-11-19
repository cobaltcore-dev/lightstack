#!/bin/bash
# scripts/deploy-single-node.sh: the build step installs make when the host lacks it and stops on a
# failed build, and the migration step migrates with lightstack-migrate, never by starting the
# server (the binary has no migrate subcommand, so `lightstack migrate` served in the foreground and
# the deployment never finished).
#
# Runs without root. The script is sourced in a subshell per case, its install directory pointed at a
# temporary one, and the tools the steps call are stubs on a PATH that holds nothing else, so a
# missing make is really missing. Usage: bash test/deploy_single_node_test.sh
set -u

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/deploy-single-node.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASSED=0
FAILED=0
pass() { echo "✓ $1"; PASSED=$((PASSED + 1)); }
fail() { echo "✗ $1"; FAILED=$((FAILED + 1)); }

# The Go version the build step pins: the go stub reports it, so no toolchain is downloaded.
GO_VERSION=$(sed -n 's/^ *local go_version="\(.*\)"$/\1/p' "$SCRIPT")
[ -n "$GO_VERSION" ] || { echo "cannot read the pinned Go version from $SCRIPT"; exit 1; }

# The stubs read these: where to record calls, the make stub, and the stub directory on the PATH.
export CALLS="$WORK/calls" MAKE_STUB="$WORK/make" STUBS="$WORK/stubs"
INSTALL="$WORK/install"

# make build writes both binaries, each recording how it was run; with MAKE_FAILS set it fails as a
# broken build does.
cat > "$MAKE_STUB" <<'STUB'
#!/bin/sh
echo "make $*" >> "$CALLS"
if [ -n "${MAKE_FAILS:-}" ]; then echo "cmd/lightstack: undefined: x" >&2; exit 2; fi
for b in lightstack lightstack-migrate; do
    printf '#!/bin/sh\necho "%s $*" >> "%s"\n' "$b" "$CALLS" > "bin/$b"
    chmod +x "bin/$b"
done
STUB
chmod +x "$MAKE_STUB"

# setup [make] prepares a fresh install directory and stub directory; with "make" the host has make.
setup() {
    rm -rf "$INSTALL" "$STUBS"
    mkdir -p "$INSTALL/bin" "$INSTALL/config" "$STUBS"
    : > "$CALLS"
    for tool in cat tail head grep sed tr mkdir rm ls chmod ln; do
        ln -s "$(command -v "$tool")" "$STUBS/$tool"
    done
    printf '#!/bin/sh\n[ "$1 $2" = "env GOVERSION" ] && echo "go%s"\nexit 0\n' "$GO_VERSION" > "$STUBS/go"
    # apt-get records what it installs; installing make puts make on the PATH.
    cat > "$STUBS/apt-get" <<'STUB'
#!/bin/sh
echo "apt-get $*" >> "$CALLS"
case " $* " in *" make "*) ln -s "$MAKE_STUB" "$STUBS/make" ;; esac
exit 0
STUB
    chmod +x "$STUBS/go" "$STUBS/apt-get"
    if [ "${1:-}" = make ]; then ln -s "$MAKE_STUB" "$STUBS/make"; fi
}

# step NAME runs one step of the sourced script; prints its output, returns its exit code.
step() {
    (
        # shellcheck source=/dev/null
        source "$SCRIPT"
        LIGHTSTACK_DIR="$INSTALL"
        BUILD_LOG="$WORK/build.log"
        PATH="$STUBS"
        "$1"
    ) 2>&1
}

# 1. A host without make gets it, and the build writes both binaries.
setup
out=$(step build_lightstack); rc=$?
if [ "$rc" -eq 0 ] && grep -q "apt-get install .*make" "$CALLS" && [ -x "$INSTALL/bin/lightstack" ] && [ -x "$INSTALL/bin/lightstack-migrate" ]; then
    pass "a host without make gets it, and both binaries are built"
else
    fail "a host without make (exit $rc, calls: $(tr '\n' ' ' < "$CALLS")): $out"
fi

# 2. A failed build stops the step, showing why.
setup make
out=$(MAKE_FAILS=1 step build_lightstack); rc=$?
if [ "$rc" -ne 0 ] && grep -q "Build failed" <<<"$out" && grep -q "undefined: x" <<<"$out"; then
    pass "a failed build stops the step and shows the build's error"
else
    fail "a failed build (exit $rc): $out"
fi

# 3. The migration step runs lightstack-migrate up, never the server.
setup make
(cd "$INSTALL" && PATH="$STUBS" "$MAKE_STUB" build) && : > "$CALLS"
out=$(step run_migrations); rc=$?
calls=$(tr '\n' ' ' < "$CALLS")
if [ "$rc" -eq 0 ] && [ "$calls" = "lightstack-migrate up -config config/lightstack.yaml -migrations migrations " ]; then
    pass "the migration step runs lightstack-migrate up"
else
    fail "the migration step (exit $rc, calls: ${calls:-none}): $out"
fi

# 4. A failed migration stops the deployment.
printf '#!/bin/sh\necho "lightstack-migrate $*" >> "%s"\nexit 1\n' "$CALLS" > "$INSTALL/bin/lightstack-migrate"
out=$(step run_migrations); rc=$?
if [ "$rc" -ne 0 ] && ! grep -q "Database migrations completed" <<<"$out"; then
    pass "a failed migration stops the deployment"
else
    fail "a failed migration (exit $rc): $out"
fi

echo
echo "passed: $PASSED, failed: $FAILED"
[ "$FAILED" -eq 0 ]
