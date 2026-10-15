#!/bin/bash
# smoke.sh — end-to-end proof that the harness itself works (SPEC-028).
#
# Tier S (smoke): a minimal pass through every service. Not part of the A/B/C
# coverage gate (manifest/smoke.json carries tier "S"); its job is to fail
# fast and loudly when the stack, the CLI, or the harness lib is broken.

# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

suite_begin smoke

# --- Keystone ---------------------------------------------------------------
t_begin smoke.token.issue
osc token issue
assert_rc 0
assert_jq '.id'
assert_jq_type '.' object
t_end

t_begin smoke.project.list
osc project list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "default")] | length' 1
t_end

t_begin smoke.project.crud
# Rerun-safe: remove a leftover from a previously crashed run first (delete of
# a missing project is allowed to fail silently).
osc project delete cli-conf-smoke-p1
osc project create cli-conf-smoke-p1
assert_rc 0
osc project show cli-conf-smoke-p1
assert_rc 0
assert_jq '.name' cli-conf-smoke-p1
osc project set cli-conf-smoke-p1 --description "smoke"
assert_rc 0
osc project delete cli-conf-smoke-p1
assert_rc 0
t_end

t_begin smoke.project.show.missing.neg
osc project show cli-conf-smoke-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent project must not exit 0"
else
    assert_err_contains .
fi
t_end

# --- Compute ----------------------------------------------------------------
t_begin smoke.flavor.list
osc flavor list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "m1.tiny")] | length' 1
t_end

t_begin smoke.server.list
osc server list
assert_rc 0
assert_jq_type '.' array
t_end

t_begin smoke.availabilityzone.list
osc availability zone list
assert_rc 0
assert_jq_type '.' array
t_end

# --- Network ----------------------------------------------------------------
t_begin smoke.network.list
osc network list
assert_rc 0
assert_jq_type '.' array
t_end

t_begin smoke.securitygroup.list
osc security group list
assert_rc 0
assert_jq_type '.' array
t_end

# --- Block storage ----------------------------------------------------------
t_begin smoke.volume.list
osc volume list
assert_rc 0
assert_jq_type '.' array
t_end

# --- Image ------------------------------------------------------------------
t_begin smoke.image.list
osc image list
assert_rc 0
assert_jq_type '.' array
t_end

suite_end
