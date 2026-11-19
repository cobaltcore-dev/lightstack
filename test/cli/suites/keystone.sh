#!/bin/bash
# keystone.sh — SPEC-031 tier A/B conformance cases for Keystone (identity).
#
# Case ids match manifest/keystone.json exactly (tier A = manifest id, tier B =
# "<id>.neg"). Every resource created here is named cli-conf-ke-* and deleted
# by the case that created it; bootstrap resources (project default, user
# admin, roles admin/member/reader, domain Default, service keystone) are only
# ever read.
#
# Known failures are recorded with t_known_failure and a precise reason; each
# one is written conditionally so the case flips to a real assertion the moment
# the divergence is fixed.

# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

suite_begin keystone

# --- helpers -----------------------------------------------------------------

# Assert a negative case produced the expected API error, not an unrelated
# auth, connection, server, or client argument failure.
_ke_assert_failed() {
    assert_http_error "${1:-404}"
}

# Assert the pinned client surfaced HTTP status <code> for the last osc()
# call. keystoneauth1 prints the code either as "(HTTP 409)" or
# "ConflictException: 409:" depending on whether it could parse the body, so
# only the number is asserted — never the surrounding text.
_ke_assert_http() {
    local code="$1"
    assert_http_error "$code"
}

# Assert the last `osc <noun> list` output holds exactly one entry named $1
# (list output is display-cased, show output is raw — cover both).
_ke_list_has() {
    assert_jq "[.[] | select((.Name // .name) == \"$1\")] | length" 1
}

_ke_list_lacks() {
    assert_jq "[.[] | select((.Name // .name) == \"$1\")] | length" 0
}

# Domains refuse to be deleted while enabled, so purging is disable-then-delete.
# Both calls are best-effort (a missing domain is fine — this is rerun-safety).
_ke_domain_purge() {
    osc domain set "$1" --disable
    osc domain delete "$1"
}

# Ensure a domain exists. (`domain create` answers 201 and echoes `links`,
# so the CLI renders it; it used to abort client-side on the missing key.)
_ke_domain_ensure() {
    _ke_domain_purge "$1"
    osc domain create "$1"
}

# --- projects ----------------------------------------------------------------

t_begin keystone.project.list
osc project list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "default")] | length' 1
t_end

t_begin keystone.project.create
osc project delete cli-conf-ke-proj
osc project create cli-conf-ke-proj --description "keystone suite"
assert_rc 0
assert_jq '.name' cli-conf-ke-proj
assert_jq_type '.id' string
assert_jq '.description' "keystone suite"
assert_jq '.enabled' true
assert_jq_type '.domain_id' string
osc project delete cli-conf-ke-proj
assert_rc 0
t_end

t_begin keystone.project.create.neg
osc project delete cli-conf-ke-proj
osc project create cli-conf-ke-proj
assert_rc 0
osc project create cli-conf-ke-proj
_ke_assert_failed 409
_ke_assert_http 409
osc project create ""
_ke_assert_failed 400
_ke_assert_http 400
osc project delete cli-conf-ke-proj
t_end

t_begin keystone.project.show
osc project delete cli-conf-ke-proj
osc project create cli-conf-ke-proj --description "shown by keystone suite"
assert_rc 0
osc project show cli-conf-ke-proj
assert_rc 0
assert_jq '.name' cli-conf-ke-proj
assert_jq '.description' "shown by keystone suite"
assert_jq_type '.id' string
assert_jq '.enabled' true
assert_jq_type '.domain_id' string
osc project delete cli-conf-ke-proj
t_end

t_begin keystone.project.show.neg
osc project show cli-conf-ke-missing
_ke_assert_failed
t_end

t_begin keystone.project.set
osc project delete cli-conf-ke-proj
osc project create cli-conf-ke-proj
assert_rc 0
osc project set cli-conf-ke-proj --description "updated by keystone suite"
assert_rc 0
osc project show cli-conf-ke-proj
assert_rc 0
assert_jq '.description' "updated by keystone suite"
osc project set cli-conf-ke-proj --disable
assert_rc 0
osc project show cli-conf-ke-proj
assert_jq '.enabled' false
osc project delete cli-conf-ke-proj
t_end

t_begin keystone.project.set.neg
osc project set cli-conf-ke-missing --description "nope"
_ke_assert_failed
t_end

t_begin keystone.project.delete
osc project delete cli-conf-ke-proj
osc project create cli-conf-ke-proj
assert_rc 0
osc project delete cli-conf-ke-proj
assert_rc 0
osc project show cli-conf-ke-proj
_ke_assert_failed
t_end

t_begin keystone.project.delete.neg
# the CLI resolves the name client-side and never reaches the server, so only
# the non-zero exit is asserted (same behaviour against upstream Keystone)
osc project delete cli-conf-ke-missing
_ke_assert_failed
t_end

# --- users -------------------------------------------------------------------

t_begin keystone.user.list
osc user delete cli-conf-ke-ul
osc user create cli-conf-ke-ul --password ke-pass-123
assert_rc 0
osc user list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "admin")] | length' 1
_ke_list_has cli-conf-ke-ul
osc user delete cli-conf-ke-ul
t_end

t_begin keystone.user.create
osc user delete cli-conf-ke-user
osc user create cli-conf-ke-user --password ke-pass-123 --email ke-user@cli-conf.invalid
assert_rc 0
assert_jq '.name' cli-conf-ke-user
assert_jq_type '.id' string
assert_jq '.email' ke-user@cli-conf.invalid
assert_jq '.enabled' true
assert_jq_type '.domain_id' string
osc user delete cli-conf-ke-user
assert_rc 0
t_end

t_begin keystone.user.create.neg
osc user delete cli-conf-ke-user
osc user create cli-conf-ke-user --password ke-pass-123
assert_rc 0
osc user create cli-conf-ke-user --password ke-pass-123
_ke_assert_failed 409
_ke_assert_http 409
osc user delete cli-conf-ke-user
t_end

t_begin keystone.user.show
osc user delete cli-conf-ke-user
osc user create cli-conf-ke-user --password ke-pass-123 --email ke-user@cli-conf.invalid
assert_rc 0
osc user show cli-conf-ke-user
assert_rc 0
assert_jq '.name' cli-conf-ke-user
assert_jq_type '.id' string
assert_jq '.enabled' true
assert_jq_type '.domain_id' string
if printf '%s' "$OSC_OUT" | jq -e '.email == "ke-user@cli-conf.invalid"' >/dev/null 2>&1; then
    assert_jq '.email' ke-user@cli-conf.invalid
else
    t_fail "user show omitted the email set at create"
fi
osc user delete cli-conf-ke-user
t_end

t_begin keystone.user.show.neg
osc user show cli-conf-ke-missing
_ke_assert_failed
t_end

t_begin keystone.user.set
osc user delete cli-conf-ke-user
osc user delete cli-conf-ke-user2
osc user create cli-conf-ke-user --password ke-pass-123 --email ke-user@cli-conf.invalid
assert_rc 0
osc user set cli-conf-ke-user --name cli-conf-ke-user2
assert_rc 0
osc user show cli-conf-ke-user2
assert_rc 0
assert_jq '.name' cli-conf-ke-user2
osc user set cli-conf-ke-user2 --email ke-user2@cli-conf.invalid
assert_rc 0
osc user show cli-conf-ke-user2
assert_rc 0
assert_jq '.email' ke-user2@cli-conf.invalid
osc user delete cli-conf-ke-user2
t_end

t_begin keystone.user.set.neg
osc user set cli-conf-ke-missing --email nope@cli-conf.invalid
_ke_assert_failed
t_end

t_begin keystone.user.delete
osc user delete cli-conf-ke-user
osc user create cli-conf-ke-user --password ke-pass-123
assert_rc 0
osc user delete cli-conf-ke-user
assert_rc 0
osc user show cli-conf-ke-user
_ke_assert_failed
t_end

t_begin keystone.user.delete.neg
osc user delete cli-conf-ke-missing
_ke_assert_failed
t_end

# --- groups ------------------------------------------------------------------

t_begin keystone.group.list
osc group delete cli-conf-ke-gl
osc group create cli-conf-ke-gl
assert_rc 0
osc group list
assert_rc 0
assert_jq_type '.' array
_ke_list_has cli-conf-ke-gl
osc group delete cli-conf-ke-gl
t_end

t_begin keystone.group.create
osc group delete cli-conf-ke-grp
osc group create cli-conf-ke-grp --description "keystone suite"
assert_rc 0
assert_jq '.name' cli-conf-ke-grp
assert_jq_type '.id' string
assert_jq '.description' "keystone suite"
assert_jq_type '.domain_id' string
osc group delete cli-conf-ke-grp
assert_rc 0
t_end

t_begin keystone.group.create.neg
osc group delete cli-conf-ke-grp
osc group create cli-conf-ke-grp
assert_rc 0
osc group create cli-conf-ke-grp
_ke_assert_failed 409
_ke_assert_http 409
osc group delete cli-conf-ke-grp
t_end

t_begin keystone.group.show
osc group delete cli-conf-ke-grp
osc group create cli-conf-ke-grp --description "shown by keystone suite"
assert_rc 0
osc group show cli-conf-ke-grp
assert_rc 0
assert_jq '.name' cli-conf-ke-grp
assert_jq '.description' "shown by keystone suite"
assert_jq_type '.id' string
assert_jq_type '.domain_id' string
osc group delete cli-conf-ke-grp
t_end

t_begin keystone.group.show.neg
osc group show cli-conf-ke-missing
_ke_assert_failed
t_end

t_begin keystone.group.set
osc group delete cli-conf-ke-grp
osc group create cli-conf-ke-grp
assert_rc 0
osc group set cli-conf-ke-grp --description "updated by keystone suite"
assert_rc 0
osc group show cli-conf-ke-grp
assert_rc 0
assert_jq '.description' "updated by keystone suite"
osc group delete cli-conf-ke-grp
t_end

t_begin keystone.group.set.neg
osc group set cli-conf-ke-missing --description "nope"
_ke_assert_failed
t_end

t_begin keystone.group.delete
osc group delete cli-conf-ke-grp
osc group create cli-conf-ke-grp
assert_rc 0
osc group delete cli-conf-ke-grp
assert_rc 0
osc group show cli-conf-ke-grp
_ke_assert_failed
t_end

t_begin keystone.group.delete.neg
osc group delete cli-conf-ke-missing
_ke_assert_failed
t_end

t_begin keystone.group.member
osc user delete cli-conf-ke-gu
osc group delete cli-conf-ke-gg
osc user create cli-conf-ke-gu --password ke-pass-123
assert_rc 0
osc group create cli-conf-ke-gg
assert_rc 0
osc group add user cli-conf-ke-gg cli-conf-ke-gu
assert_rc 0
# Membership proof through the CLI: removing a member succeeds, while removing
# a non-member is a 404 (keystone.group.member.neg). The direct probe
# (`group contains user`, HEAD /v3/groups/{id}/users/{user_id}) is not served
# by lightstack and therefore not in the manifest.
osc group remove user cli-conf-ke-gg cli-conf-ke-gu
assert_rc 0
osc user delete cli-conf-ke-gu
osc group delete cli-conf-ke-gg
t_end

t_begin keystone.group.member.neg
osc user delete cli-conf-ke-gu
osc group delete cli-conf-ke-gg
osc user create cli-conf-ke-gu --password ke-pass-123
assert_rc 0
osc group create cli-conf-ke-gg
assert_rc 0
# the user was never added to the group
osc group remove user cli-conf-ke-gg cli-conf-ke-gu
_ke_assert_failed
_ke_assert_http 404
osc user delete cli-conf-ke-gu
osc group delete cli-conf-ke-gg
t_end

# --- roles -------------------------------------------------------------------

t_begin keystone.role.list
osc role list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "admin")] | length' 1
assert_jq '[.[] | select((.Name // .name) == "member")] | length' 1
assert_jq '[.[] | select((.Name // .name) == "reader")] | length' 1
t_end

t_begin keystone.role.create
osc role delete cli-conf-ke-role
osc role create cli-conf-ke-role
assert_rc 0
assert_jq '.name' cli-conf-ke-role
assert_jq_type '.id' string
osc role delete cli-conf-ke-role
assert_rc 0
t_end

t_begin keystone.role.create.neg
# member is a bootstrap role; creating it again must conflict
osc role create member
_ke_assert_failed 409
_ke_assert_http 409
t_end

t_begin keystone.role.show
osc role delete cli-conf-ke-role
osc role create cli-conf-ke-role
assert_rc 0
osc role show cli-conf-ke-role
assert_rc 0
assert_jq '.name' cli-conf-ke-role
assert_jq_type '.id' string
osc role delete cli-conf-ke-role
t_end

t_begin keystone.role.show.neg
osc role show cli-conf-ke-missing
_ke_assert_failed
t_end

t_begin keystone.role.set
osc role delete cli-conf-ke-role
osc role delete cli-conf-ke-role2
osc role create cli-conf-ke-role
assert_rc 0
osc role set cli-conf-ke-role --name cli-conf-ke-role2
assert_rc 0
osc role show cli-conf-ke-role2
assert_rc 0
assert_jq '.name' cli-conf-ke-role2
osc role delete cli-conf-ke-role2
t_end

t_begin keystone.role.set.neg
osc role set cli-conf-ke-missing --name cli-conf-ke-nope
_ke_assert_failed
t_end

t_begin keystone.role.delete
osc role delete cli-conf-ke-role
osc role create cli-conf-ke-role
assert_rc 0
osc role delete cli-conf-ke-role
assert_rc 0
osc role show cli-conf-ke-role
_ke_assert_failed
t_end

t_begin keystone.role.delete.neg
osc role delete cli-conf-ke-missing
_ke_assert_failed
t_end

t_begin keystone.role.assignment.add
osc user delete cli-conf-ke-rau
osc project delete cli-conf-ke-rap
osc role delete cli-conf-ke-rar
osc user create cli-conf-ke-rau --password ke-pass-123
assert_rc 0
KE_USER_ID="$(printf '%s' "$OSC_OUT" | jq -r '.id')"
osc project create cli-conf-ke-rap
assert_rc 0
KE_PROJECT_ID="$(printf '%s' "$OSC_OUT" | jq -r '.id')"
osc role create cli-conf-ke-rar
assert_rc 0
KE_ROLE_ID="$(printf '%s' "$OSC_OUT" | jq -r '.id')"
osc role add cli-conf-ke-rar --user cli-conf-ke-rau --project cli-conf-ke-rap
assert_rc 0
osc role assignment list --user "$KE_USER_ID" --project "$KE_PROJECT_ID"
assert_rc 0
assert_jq_type '.' array
assert_jq 'length' 1
assert_jq '.[0].Role' "$KE_ROLE_ID"
assert_jq '.[0].User' "$KE_USER_ID"
assert_jq '.[0].Project' "$KE_PROJECT_ID"
assert_jq '.[0].Group' ''
assert_jq '.[0].Inherited' false
# role_assignments rows cascade on user/project/role delete
osc user delete cli-conf-ke-rau
osc project delete cli-conf-ke-rap
osc role delete cli-conf-ke-rar
t_end

t_begin keystone.role.assignment.add.neg
osc role add cli-conf-ke-missing --user admin --project default
_ke_assert_failed
t_end

t_begin keystone.role.assignment.remove
osc user delete cli-conf-ke-rau
osc project delete cli-conf-ke-rap
osc role delete cli-conf-ke-rar
osc user create cli-conf-ke-rau --password ke-pass-123
assert_rc 0
KE_USER_ID="$(printf '%s' "$OSC_OUT" | jq -r '.id')"
osc project create cli-conf-ke-rap
assert_rc 0
KE_PROJECT_ID="$(printf '%s' "$OSC_OUT" | jq -r '.id')"
osc role create cli-conf-ke-rar
assert_rc 0
osc role add cli-conf-ke-rar --user cli-conf-ke-rau --project cli-conf-ke-rap
assert_rc 0
osc role remove cli-conf-ke-rar --user cli-conf-ke-rau --project cli-conf-ke-rap
assert_rc 0
osc role assignment list --user "$KE_USER_ID" --project "$KE_PROJECT_ID"
assert_rc 0
assert_jq_type '.' array
assert_jq 'length' 0
osc user delete cli-conf-ke-rau
osc project delete cli-conf-ke-rap
osc role delete cli-conf-ke-rar
t_end

t_begin keystone.role.assignment.remove.neg
# Use a throwaway user and project with no assignments so this negative case
# never changes the admin user's real role bindings.
osc user delete cli-conf-ke-rnu
osc project delete cli-conf-ke-rnp
osc user create cli-conf-ke-rnu --password ke-pass-123
assert_rc 0
osc project create cli-conf-ke-rnp
assert_rc 0
osc role remove reader --user cli-conf-ke-rnu --project cli-conf-ke-rnp
_ke_assert_failed
_ke_assert_http 404
osc user delete cli-conf-ke-rnu
osc project delete cli-conf-ke-rnp
t_end

t_begin keystone.role.assignment.list
osc token issue
assert_rc 0
KE_ADMIN_ID="$(printf '%s' "$OSC_OUT" | jq -r '.user_id')"
KE_DEFPROJ_ID="$(printf '%s' "$OSC_OUT" | jq -r '.project_id')"
osc role assignment list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select(.User == \"$KE_ADMIN_ID\" and .Project == \"$KE_DEFPROJ_ID\")] | length >= 1"
osc role assignment list --user "$KE_ADMIN_ID" --project "$KE_DEFPROJ_ID"
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select(.User == \"$KE_ADMIN_ID\")] | length >= 1"
t_end

t_begin keystone.role.assignment.list.names
osc role assignment list --names
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select(.Role == "admin" and .User == "admin" and .Project == "default")] | length >= 1'
t_end

# --- domains -----------------------------------------------------------------

t_begin keystone.domain.list
osc domain list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "Default")] | length' 1
assert_jq '[.[] | select((.Name // .name) == "Default") | .Enabled] | first' true
t_end

t_begin keystone.domain.create
_ke_domain_purge cli-conf-ke-dom
osc domain create cli-conf-ke-dom
assert_rc 0
assert_jq '.name' cli-conf-ke-dom
assert_jq_type '.id' string
assert_jq '.enabled' true
osc domain list
assert_rc 0
_ke_list_has cli-conf-ke-dom
_ke_domain_purge cli-conf-ke-dom
t_end

t_begin keystone.domain.create.neg
# Default is the bootstrap domain — a duplicate name must conflict
osc domain create Default
_ke_assert_failed 409
_ke_assert_http 409
t_end

t_begin keystone.domain.show
_ke_domain_ensure cli-conf-ke-dom
osc domain show cli-conf-ke-dom
assert_rc 0
assert_jq '.name' cli-conf-ke-dom
assert_jq_type '.id' string
assert_jq '.enabled' true
_ke_domain_purge cli-conf-ke-dom
t_end

t_begin keystone.domain.show.neg
osc domain show cli-conf-ke-missing
_ke_assert_failed
t_end

t_begin keystone.domain.set
_ke_domain_ensure cli-conf-ke-dom
osc domain set cli-conf-ke-dom --description "updated by keystone suite"
assert_rc 0
# Confirm the write by reading the updated domain.
osc domain show cli-conf-ke-dom
assert_rc 0
assert_jq '.description' "updated by keystone suite"
osc domain list
assert_rc 0
assert_jq '[.[] | select(.Name == "cli-conf-ke-dom") | .Description] | first' "updated by keystone suite"
_ke_domain_purge cli-conf-ke-dom
t_end

t_begin keystone.domain.set.neg
osc domain set cli-conf-ke-missing --description "nope"
_ke_assert_failed
t_end

t_begin keystone.domain.delete
_ke_domain_ensure cli-conf-ke-dom
osc domain set cli-conf-ke-dom --disable
assert_rc 0
osc domain delete cli-conf-ke-dom
assert_rc 0
osc domain list
assert_rc 0
_ke_list_lacks cli-conf-ke-dom
t_end

t_begin keystone.domain.delete.neg
# an enabled domain must refuse deletion (upstream answers 403 as well)
_ke_domain_ensure cli-conf-ke-dom
osc domain delete cli-conf-ke-dom
_ke_assert_failed 403
_ke_assert_http 403
_ke_domain_purge cli-conf-ke-dom
t_end

# --- service catalog / endpoints ----------------------------------------------

t_begin keystone.service.list
osc service list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Type // .type) == "identity")] | length >= 1'
t_end

t_begin keystone.service.show
osc service show keystone
assert_rc 0
assert_jq '(.Name // .name)' keystone
assert_jq '(.Type // .type)' identity
assert_jq '(.Enabled // .enabled)' true
assert_jq_type '(.ID // .id)' string
t_end

t_begin keystone.service.show.neg
osc service show cli-conf-ke-missing
_ke_assert_failed
t_end

t_begin keystone.catalog.list
osc catalog list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select(.Type == "identity")] | length' 1
assert_jq_type '[.[] | select(.Type == "identity") | .Endpoints] | first' array
t_end

t_begin keystone.catalog.show
osc catalog show keystone
assert_rc 0
assert_jq '.name' keystone
assert_jq '.type' identity
assert_jq_type '.endpoints' array
assert_jq_type '.endpoints[0].url' string
assert_jq_type '.endpoints[0].interface' string
t_end

t_begin keystone.endpoint.list
osc endpoint list
assert_rc 0
assert_jq_type '.' array
assert_jq_gt 'length' 0
assert_jq '[.[] | select(.Interface == "public")] | length >= 1'
assert_jq_type '[.[] | .URL] | first' string
t_end

# --- application credentials ---------------------------------------------------

t_begin keystone.applicationcredential.create
osc application credential delete cli-conf-ke-ac
osc application credential create cli-conf-ke-ac
assert_rc 0
assert_jq '(.Name // .name)' cli-conf-ke-ac
assert_jq_type '(.ID // .id)' string
assert_jq_type '.Secret' string
# jq's `//` treats false as falsy, so an explicit false fallback is needed here
assert_jq '(.Unrestricted // .unrestricted // false)' false
assert_jq '.Roles' '[]'
osc application credential delete cli-conf-ke-ac
assert_rc 0
t_end

t_begin keystone.applicationcredential.create.neg
osc application credential delete cli-conf-ke-ac
osc application credential create cli-conf-ke-ac
assert_rc 0
osc application credential create cli-conf-ke-ac
_ke_assert_failed 409
_ke_assert_http 409
osc application credential delete cli-conf-ke-ac
t_end

t_begin keystone.applicationcredential.list
osc application credential delete cli-conf-ke-ac
osc application credential create cli-conf-ke-ac
assert_rc 0
osc application credential list
assert_rc 0
assert_jq_type '.' array
_ke_list_has cli-conf-ke-ac
osc application credential delete cli-conf-ke-ac
assert_rc 0
t_end

t_begin keystone.applicationcredential.show
osc application credential delete cli-conf-ke-ac
osc application credential create cli-conf-ke-ac
assert_rc 0
osc application credential show cli-conf-ke-ac
assert_rc 0
assert_jq '(.Name // .name)' cli-conf-ke-ac
assert_jq_type '(.ID // .id)' string
assert_jq_type '(.Roles // .roles)' array
osc application credential delete cli-conf-ke-ac
t_end

t_begin keystone.applicationcredential.show.neg
osc application credential show cli-conf-ke-missing
_ke_assert_failed
t_end

t_begin keystone.applicationcredential.delete
osc application credential delete cli-conf-ke-ac
osc application credential create cli-conf-ke-ac
assert_rc 0
osc application credential delete cli-conf-ke-ac
assert_rc 0
# absence proof: the name no longer resolves
osc application credential show cli-conf-ke-ac
_ke_assert_failed
t_end

t_begin keystone.applicationcredential.delete.neg
osc application credential delete cli-conf-ke-missing
_ke_assert_failed
t_end

# --- EC2 credentials -----------------------------------------------------------

t_begin keystone.ec2credential.create
osc ec2 credentials create
assert_rc 0
assert_jq_type '.access' string
assert_jq_type '.secret' string
assert_jq_type '.user_id' string
assert_jq_type '.project_id' string
KE_EC2_ACCESS="$(printf '%s' "$OSC_OUT" | jq -r '.access')"
osc ec2 credentials delete "$KE_EC2_ACCESS"
assert_rc 0
t_end

t_begin keystone.ec2credential.list
osc ec2 credentials create
assert_rc 0
KE_EC2_ACCESS="$(printf '%s' "$OSC_OUT" | jq -r '.access')"
osc ec2 credentials list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select(.Access == \"$KE_EC2_ACCESS\")] | length" 1
osc ec2 credentials delete "$KE_EC2_ACCESS"
assert_rc 0
t_end

t_begin keystone.ec2credential.show
osc ec2 credentials create
assert_rc 0
KE_EC2_ACCESS="$(printf '%s' "$OSC_OUT" | jq -r '.access')"
KE_EC2_PROJECT="$(printf '%s' "$OSC_OUT" | jq -r '.project_id')"
osc ec2 credentials show "$KE_EC2_ACCESS"
assert_rc 0
assert_jq '.access' "$KE_EC2_ACCESS"
assert_jq '.project_id' "$KE_EC2_PROJECT"
assert_jq_type '.secret' string
assert_jq_type '.user_id' string
osc ec2 credentials delete "$KE_EC2_ACCESS"
assert_rc 0
t_end

t_begin keystone.ec2credential.show.neg
osc ec2 credentials show 00000000000000000000000000000000
_ke_assert_failed
_ke_assert_http 404
t_end

t_begin keystone.ec2credential.delete
osc ec2 credentials create
assert_rc 0
KE_EC2_ACCESS="$(printf '%s' "$OSC_OUT" | jq -r '.access')"
osc ec2 credentials delete "$KE_EC2_ACCESS"
assert_rc 0
osc ec2 credentials list
assert_rc 0
assert_jq "[.[] | select(.Access == \"$KE_EC2_ACCESS\")] | length" 0
t_end

t_begin keystone.ec2credential.delete.neg
osc ec2 credentials create
assert_rc 0
KE_EC2_ACCESS="$(printf '%s' "$OSC_OUT" | jq -r '.access')"
osc ec2 credentials delete "$KE_EC2_ACCESS"
assert_rc 0
osc ec2 credentials delete "$KE_EC2_ACCESS"
_ke_assert_failed 404
_ke_assert_http 404
t_end

# --- tokens --------------------------------------------------------------------

t_begin keystone.token.issue
osc token issue
assert_rc 0
assert_jq_type '.id' string
assert_jq_gt '.id | length' 100
assert_jq_type '.expires' string
assert_jq_type '.user_id' string
assert_jq_type '.project_id' string
t_end

t_begin keystone.token.issue.neg
osc --os-password cli-conf-ke-wrong-password token issue
_ke_assert_failed 401
_ke_assert_http 401
t_end

t_begin keystone.token.revoke
osc token issue
assert_rc 0
KE_TOKEN_ID="$(printf '%s' "$OSC_OUT" | jq -r '.id')"
osc token revoke "$KE_TOKEN_ID"
assert_rc 0
t_end

t_begin keystone.token.revoke.neg
osc token revoke cli-conf-ke-not-a-real-token
# Upstream Keystone validates the subject token and answers 404 for an
# unknown one, so the CLI exits non-zero — lightstack matches since cfacd1b
# (the no-op 204 remains only for VALID tokens, the documented divergence).
_ke_assert_failed
t_end

suite_end
