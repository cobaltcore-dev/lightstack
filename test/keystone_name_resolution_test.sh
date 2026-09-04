#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# cloud-storage#515 — identity by-id lookups must return 404 (not 500) for a
# non-UUID id, so the openstack CLI / Terraform name->id resolution works.
#
# Requires a running Lightstack and OS_* admin credentials in the environment
# (e.g. `source ~/.lightstack-env`) plus the `openstack` CLI. Exits non-zero on failure.
set -euo pipefail

: "${OS_AUTH_URL:?source your Lightstack env (OS_AUTH_URL, OS_USERNAME, OS_PASSWORD, ...)}"
fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

TOKEN=$(openstack token issue -f value -c id) || fail "could not issue token"
hdr=(-H "X-Auth-Token: $TOKEN")

code() { curl -s -o /dev/null -w '%{http_code}' "${hdr[@]}" "$1"; }

# 1) non-UUID id -> 404 (the bug: used to be 500)
for res in projects users roles; do
  c=$(code "$OS_AUTH_URL/$res/not-a-uuid")
  [ "$c" = "404" ] || fail "GET /$res/not-a-uuid returned $c, want 404"
  pass "GET /$res/not-a-uuid -> 404"
done

# 2) valid-but-unknown UUID -> 404 (unchanged), and identical message to (1)
u="00000000-0000-0000-0000-0000000000ff"
body_bad=$(curl -s "${hdr[@]}" "$OS_AUTH_URL/projects/not-a-uuid")
body_unknown=$(curl -s "${hdr[@]}" "$OS_AUTH_URL/projects/$u")
echo "$body_bad"     | grep -q "Could not find project: not-a-uuid." || fail "malformed body not verbatim: $body_bad"
echo "$body_unknown" | grep -q "Could not find project: $u."         || fail "unknown body not verbatim: $body_unknown"
pass "malformed and unknown ids both return the Keystone-verbatim 404 message"

# 3) CLI name resolution: `project show <non-uuid-name>` must NOT be a 500/HttpException 500
if openstack project show not-a-uuid >/dev/null 2>err.txt; then
  : # a project literally named "not-a-uuid" exists; fine
else
  grep -qiE "HttpException 500|Internal Server Error" err.txt && fail "project show hit a 500: $(cat err.txt)"
fi
rm -f err.txt
pass "openstack project show <name> resolves without a 500"

# 4) name resolution end-to-end against a real project name
name=$(openstack project list -f value -c Name | head -1) || fail "project list failed"
openstack project show "$name" >/dev/null || fail "project show by name '$name' failed"
pass "resolved project by name: $name"

echo "ALL PASS"
