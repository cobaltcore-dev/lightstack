#!/usr/bin/env bash
#
# SPEC-028 phase 3 (lightstack #105) — Terraform OpenStack-provider conformance
# wrapper. Automates the manual runbook for the compat module:
#   docs/specs/016-terraform-openstack-provider-verification/HOST_TESTING.md
#
#   preflight -> prerequisites -> terraform init -> validate -> apply
#   -> re-plan (no drift) -> destroy -> empty-state + no-residue assertions
#
# Exit 0 iff: init + validate + apply + destroy all succeed, the post-apply
# re-plan is clean, and after destroy the state is empty with no tf-compat-*
# residue left in lightstack and no post-destroy drift.
#
# Repo hygiene: the module never runs in place — it is copied to a mktemp
# workspace, so .terraform/ and tfstate never land in the repo tree. Provider
# downloads are shared through TF_PLUGIN_CACHE_DIR so repeat runs (and the
# nightly CI job) don't re-download the provider.
#
# Usage:
#   make test-terraform          # from the repo root
#   bash test/terraform/run.sh   # equivalent
#
# Environment (all optional):
#   OS_*               standard OpenStack auth vars; the defaults below fit the
#                      local stub-mode deployment (admin/secret @ :35357).
#   TF_IMAGE_NAME      Glance image the module resolves. Default: a unique
#                      per-run tf-cirros-* name (created from a 64KiB zero file
#                      and deleted at exit) — collision-free on a shared server
#                      and unambiguous to clean up. Set TF_IMAGE_NAME (e.g.
#                      "cirros", the compat module's variables.tf default) to
#                      use a specific name instead: it is then only created if
#                      missing and only deleted if this run created it.
#   TF_FLAVOR_NAME     flavor name (default: m1.small — seeded by lightstack).
#   OPENSTACK_CLI      path to the openstack CLI (used for prerequisites).
#   TF_PLUGIN_CACHE_DIR  provider download cache (default ~/.terraform.d/plugin-cache).
#
# Pinning: terraform is pinned to TF_VERSION_PIN (see the install hint printed
# by the preflight if it is missing). The provider version is pinned in
# compat/provider.tf required_providers ("~> 3.0" — the 3.x series).

set -euo pipefail

readonly TF_VERSION_PIN="1.13.4"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE_DIR="$SCRIPT_DIR/compat"

# Defaults for the local stub deployment; standard OS_* vars override.
export OS_AUTH_URL="${OS_AUTH_URL:-http://localhost:35357/v3}"
export OS_USERNAME="${OS_USERNAME:-admin}"
export OS_PASSWORD="${OS_PASSWORD:-secret}"
export OS_PROJECT_NAME="${OS_PROJECT_NAME:-default}"
export OS_USER_DOMAIN_NAME="${OS_USER_DOMAIN_NAME:-Default}"
# A distinct project domain falls back to the user domain (the common
# single-domain case), never the other way round.
export OS_PROJECT_DOMAIN_NAME="${OS_PROJECT_DOMAIN_NAME:-${OS_USER_DOMAIN_NAME:-Default}}"
export OS_IDENTITY_API_VERSION="${OS_IDENTITY_API_VERSION:-3}"
export OS_REGION_NAME="${OS_REGION_NAME:-RegionOne}"
# The compat module's provider block is empty: terraform-provider-openstack
# resolves every service endpoint (nova/neutron/cinder/glance) from the
# Keystone catalog, so OS_AUTH_URL is the only endpoint plumbing needed —
# no per-service overrides are required (see HOST_TESTING.md §1.4–§1.5).

# Image the module resolves by name (data.openstack_images_image_v2). The
# default is a unique per-run name: the module takes image_name as a variable,
# and a unique tf- name both avoids duplicate-name data-source races when
# other clients share the server and makes cleanup ownership unambiguous.
# TF_IMAGE_NAME overrides it (e.g. "cirros", the module's variables.tf default).
IMAGE_NAME="${TF_IMAGE_NAME:-tf-cirros-$$-$RANDOM}"
FLAVOR_NAME="${TF_FLAVOR_NAME:-m1.small}"

# Every terraform invocation that evaluates the config (apply, plan, and
# destroy — destroy re-reads the data source during refresh) needs the image
# name wired through, or the module falls back to its variables.tf default.
TF_VAR_ARGS=(-var "image_name=$IMAGE_NAME" -var "flavor_name=$FLAVOR_NAME")

export TF_PLUGIN_CACHE_DIR="${TF_PLUGIN_CACHE_DIR:-$HOME/.terraform.d/plugin-cache}"
mkdir -p "$TF_PLUGIN_CACHE_DIR" # must exist before any terraform invocation
export TF_IN_AUTOMATION=1

log()  { printf '\n=== %s ===\n' "$*"; }
die()  { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

WORKSPACE=""
CREATED_IMAGE_ID=""
DESTROY_DONE=0
# Failure logs are copied here (and survive cleanup) so CI can archive them.
RESULTS_DIR="${TF_RESULTS_DIR:-$(cd "$(dirname "$0")" && pwd)/results}"

cleanup() {
    local rc=$?
    trap - EXIT
    # A failed run must not take its evidence with it: the wrapper works in a
    # mktemp workspace that this function removes, so the apply/destroy logs
    # are copied to the results dir first. Nothing is written on success —
    # the summary on stdout is the whole report.
    if [ "$rc" -ne 0 ] && [ -n "$WORKSPACE" ] && [ -d "$WORKSPACE" ]; then
        mkdir -p "$RESULTS_DIR"
        for f in "$WORKSPACE"/*.log; do
            [ -f "$f" ] && cp "$f" "$RESULTS_DIR/" 2>/dev/null || true
        done
        [ -f "$WORKSPACE/crash.log" ] && cp "$WORKSPACE/crash.log" "$RESULTS_DIR/" 2>/dev/null || true
    fi
    # Best-effort teardown so a failed run leaves no tf-compat-* residue.
    if [ "$DESTROY_DONE" -eq 0 ] && [ -n "$WORKSPACE" ] \
        && [ -f "$WORKSPACE/terraform.tfstate" ] \
        && [ -n "$(timeout 120 terraform -chdir="$WORKSPACE" state list 2>/dev/null || true)" ]; then
        log "cleanup: best-effort terraform destroy of the partial apply"
        timeout 600 terraform -chdir="$WORKSPACE" destroy \
            -auto-approve -input=false -no-color "${TF_VAR_ARGS[@]}" \
            >/dev/null 2>&1 || true
    fi
    if [ -n "$CREATED_IMAGE_ID" ]; then
        log "cleanup: deleting image created by this run ($CREATED_IMAGE_ID)"
        "$OSC" image delete "$CREATED_IMAGE_ID" >/dev/null 2>&1 || true
    fi
    if [ -n "$WORKSPACE" ] && [ -d "$WORKSPACE" ]; then
        rm -rf "$WORKSPACE"
    fi
    exit "$rc"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 1. Preflight
# ---------------------------------------------------------------------------
log "preflight"

command -v terraform >/dev/null 2>&1 || die "terraform not found on PATH.
Install the pinned version $TF_VERSION_PIN, e.g.:
  curl -fsSLO https://releases.hashicorp.com/terraform/${TF_VERSION_PIN}/terraform_${TF_VERSION_PIN}_linux_amd64.zip
  unzip terraform_${TF_VERSION_PIN}_linux_amd64.zip -d /usr/local/bin
  rm terraform_${TF_VERSION_PIN}_linux_amd64.zip && terraform version
(Or use any package manager, then check the version against the pin: $TF_VERSION_PIN.)"

OSC="${OPENSTACK_CLI:-$(command -v openstack || true)}"
[ -n "$OSC" ] || die "openstack CLI not found. Set OPENSTACK_CLI to its path \
(e.g. /opt/osc-venv/bin/openstack) or put it on PATH."

[ -d "$MODULE_DIR" ] || die "compat module not found at $MODULE_DIR"
for tool in curl jq timeout; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool not found on PATH"
done

log "terraform version"
terraform version | head -1

curl -sf --max-time 10 "$OS_AUTH_URL" -o /dev/null \
    || die "cannot reach lightstack at OS_AUTH_URL=$OS_AUTH_URL — is the server running?"

"$OSC" token issue -f value -c id >/dev/null 2>&1 \
    || die "authentication failed at $OS_AUTH_URL (user=$OS_USERNAME, project=$OS_PROJECT_NAME) — check OS_* credentials"

# Every catalog endpoint must be reachable, or apply will hang creating the
# resource that talks to the dead port (HOST_TESTING.md §7).
catalog_endpoints="$("$OSC" catalog list -f value -c Endpoints)" \
    || die "cannot list service catalog endpoints"
catalog_urls="$(printf '%s\n' "$catalog_endpoints" | grep -oE 'https?://[a-zA-Z0-9.:/-]+' | sort -u)" \
    || die "service catalog contains no endpoint URLs"
while IFS= read -r url; do
    curl -s -o /dev/null --max-time 10 "$url" \
        || die "catalog endpoint unreachable: $url (OS_AUTH_URL=$OS_AUTH_URL)"
done <<<"$catalog_urls"
log "auth OK, all catalog endpoints reachable"

# ---------------------------------------------------------------------------
# 2. Prerequisites (image + flavor)
# ---------------------------------------------------------------------------
log "prerequisites"

"$OSC" flavor show "$FLAVOR_NAME" -f value -c name >/dev/null 2>&1 \
    || die "flavor '$FLAVOR_NAME' not found — lightstack seeds m1.tiny..m1.xlarge; \
is the server a fresh deployment?"

if "$OSC" image show "$IMAGE_NAME" -f value -c id >/dev/null 2>&1; then
    log "image '$IMAGE_NAME' already present (not created by this run — will not delete it)"
else
    log "image '$IMAGE_NAME' missing — creating a 64KiB dummy (stub Glance stores metadata only)"
    imgfile="$(mktemp "${TMPDIR:-/tmp}/tf-compat-img.XXXXXX")"
    head -c 65536 /dev/zero >"$imgfile"
    set +e
    # 409-tolerant: a concurrent run may have created it between check and create.
    CREATED_IMAGE_ID="$("$OSC" image create --disk-format raw --container-format bare \
        --file "$imgfile" "$IMAGE_NAME" -f value -c id 2>/dev/null)"
    set -e
    rm -f "$imgfile"
    if [ -z "$CREATED_IMAGE_ID" ]; then
        "$OSC" image show "$IMAGE_NAME" -f value -c id >/dev/null 2>&1 \
            || die "failed to create image '$IMAGE_NAME'"
        log "image '$IMAGE_NAME' appeared concurrently (created by another run — will not delete it)"
    else
        log "created image '$IMAGE_NAME' ($CREATED_IMAGE_ID) — will delete it at exit"
    fi
fi

# ---------------------------------------------------------------------------
# 3. Workspace isolation + provider cache
# ---------------------------------------------------------------------------
WORKSPACE="$(mktemp -d "${TMPDIR:-/tmp}/tf-compat-workspace.XXXXXX")"
mkdir -p "$TF_PLUGIN_CACHE_DIR"
cp -r "$MODULE_DIR"/. "$WORKSPACE"/
# Never inherit local state/plugins if someone ran terraform in compat/ by hand.
rm -rf "$WORKSPACE/.terraform" "$WORKSPACE/.terraform.lock.hcl" \
    "$WORKSPACE"/terraform.tfstate* 2>/dev/null || true
log "workspace: $WORKSPACE (shared provider cache: $TF_PLUGIN_CACHE_DIR)"

tf() { timeout 900 terraform -chdir="$WORKSPACE" "$@"; }

# ---------------------------------------------------------------------------
# 4. init -> validate -> apply
# ---------------------------------------------------------------------------
log "terraform init (provider from the public registry; first run downloads it)"
tf init -input=false -no-color

log "terraform validate"
tf validate -no-color

log "terraform apply"
tf apply -auto-approve -input=false -no-color "${TF_VAR_ARGS[@]}" 2>&1 \
    | tee "$WORKSPACE/apply.log" || {
    # Root-cause hint for a historical lightstack divergence (fixed by the
    # SPEC-028 Glance name-filter change). The module itself stays
    # unmodified — see README.md "Known divergences".
    if grep -q 'data.openstack_images_image_v2' "$WORKSPACE/apply.log" 2>/dev/null \
        && grep -q 'more than one result' "$WORKSPACE/apply.log" 2>/dev/null; then
        n="$("$OSC" image list -f value -c Name 2>/dev/null | grep -c . || true)"
        cat >&2 <<EOF
FAIL: terraform apply failed — see the output above.

NOTE: the provider's data.openstack_images_image_v2 resolves images with a
server-side ?name= filter on GET /v2/images. Lightstack honours that filter
since the SPEC-028 fix, so a "more than one result" error now means genuinely
duplicate names (this server currently has $n image(s)) — not the historical
ignored-filter divergence. Check for images sharing the module's image name.
EOF
        exit 1
    fi
    die "terraform apply failed — see the output above"
}

# ---------------------------------------------------------------------------
# 5. Post-apply re-plan: no drift (SPEC-017/#524 read-back regression check)
# ---------------------------------------------------------------------------
log "post-apply re-plan (must be clean: -detailed-exitcode 0)"
set +e
plan_out="$(tf plan -detailed-exitcode -input=false -no-color "${TF_VAR_ARGS[@]}" 2>&1)"
plan_rc=$?
set -e
[ "$plan_rc" -eq 0 ] || die "post-apply drift detected (-detailed-exitcode=$plan_rc):
$plan_out"

# ---------------------------------------------------------------------------
# 6. destroy
# ---------------------------------------------------------------------------
# Floating IPs carry no name, so the post-destroy residue sweep cannot match
# them by prefix. Capture the module's own FIP ids from the state now —
# before destroy empties it — and assert each is gone afterwards.
# A failed pull or malformed state must not silently erase the IDs whose
# absence we need to prove after destroy. pipefail preserves both failures.
FIP_IDS_BEFORE_DESTROY="$(tf state pull \
    | jq -er '[.resources[] | select(.type == "openstack_networking_floatingip_v2")
               | .instances[].attributes.id]
              | if all(.[]; type == "string" and length > 0) then join(" ")
                else error("invalid floating IP ID in state") end')" \
    || die "unable to capture floating IP IDs from Terraform state"

log "terraform destroy"
tf destroy -auto-approve -input=false -no-color "${TF_VAR_ARGS[@]}" 2>&1 \
    | tee "$WORKSPACE/destroy.log" \
    || die "terraform destroy failed — see the output above"
DESTROY_DONE=1

# ---------------------------------------------------------------------------
# 7. Clean-state assertions
# ---------------------------------------------------------------------------
log "clean-state assertions"

state_list="$(tf state list -no-color 2>&1)" || die "terraform state list failed: $state_list"
[ -z "$state_list" ] || die "state not empty after destroy:
$state_list"
log "state list: empty"

outputs="$(tf output -json 2>/dev/null)" || die "terraform output failed"
[ "$outputs" = "{}" ] || die "outputs not empty after destroy: $outputs"
log "outputs: none"

# No tf-compat-* residue left in lightstack (the authoritative residue check —
# an empty state alone cannot prove the infra is gone). The router and
# floating-IP checks are defensive only: the current compat module creates
# neither (network, subnet, security group + rules, keypair, server, volume,
# attach), but a future module revision may add them, and the sweep must not
# silently stop covering the surface when it does. Floating IPs carry no
# name at all — they are covered by the id-based check below.
residue=0
for res in "server list" "network list" "security group list" "volume list" "keypair list" "subnet list" "router list"; do
    read -r -a resource_args <<<"$res"
    names="$("$OSC" "${resource_args[@]}" -f value -c Name)" \
        || die "cannot check residue: openstack $res failed"
    n="$(printf '%s\n' "$names" | grep -c '^tf-compat-' || true)"
    residue=$((residue + n))
done
# The module's own floating IPs, captured from state before destroy — the
# only precise check available for nameless resources. Resolved through the
# LIST, not 'floating ip show': the pinned OSC 7.0.0 fails that command
# client-side on every invocation ("Invalid formatter provided.", osc-lib
# 4.7.0 vs the plain-callable port_details formatter), so a show-based probe
# would report "gone" unconditionally — including for a leaked FIP.
if [ -n "$FIP_IDS_BEFORE_DESTROY" ]; then
    fip_list="$("$OSC" floating ip list -f value -c ID)" \
        || die "cannot check residue: openstack floating ip list failed"
    for fip in $FIP_IDS_BEFORE_DESTROY; do
        if printf '%s\n' "$fip_list" | grep -qx "$fip"; then
            residue=$((residue + 1))
        fi
    done
fi
[ "$residue" -eq 0 ] || die "tf-compat-* residue left in lightstack after destroy \
($residue object(s)) — destroy reported success but left objects behind"
log "openstack residue: none"

# Post-destroy plan: with an empty state the plan necessarily proposes the 9
# creates again (-detailed-exitcode=2), so a bare "require 0" cannot hold.
# What we require instead: no error, and a plan that contains ONLY adds —
# any to-change/to-destroy, or residue matching a would-be add (caught above
# by the CLI check), means destroy did not fully clean up.
set +e
plan_out="$(tf plan -detailed-exitcode -input=false -no-color "${TF_VAR_ARGS[@]}" 2>&1)"
plan_rc=$?
set -e
case "$plan_rc" in
    0) log "post-destroy plan: clean (no changes)" ;;
    2) summary="$(printf '%s\n' "$plan_out" | grep -E '^Plan:' || true)"
        if printf '%s\n' "$plan_out" | grep -qE '[1-9][0-9]* to (change|destroy)'; then
            die "post-destroy drift: plan proposes changes/destroys:
$plan_out"
        fi
        log "post-destroy plan: ${summary:-adds only} (expected after destroy; \
residue check above proves the adds are fresh creates, not leftovers)" ;;
    *) die "post-destroy plan errored (-detailed-exitcode=$plan_rc):
$plan_out" ;;
esac

# ---------------------------------------------------------------------------
# 8. Summary
# ---------------------------------------------------------------------------
prov_ver="$(grep -A3 'provider "registry.terraform.io/terraform-provider-openstack/openstack"' \
    "$WORKSPACE/.terraform.lock.hcl" 2>/dev/null | grep -oE '"[0-9]+\.[0-9]+\.[0-9]+"' \
    | head -1 | tr -d '"' || true)"
log "SUMMARY: PASS"
cat <<EOF
  terraform        : $(terraform version | head -1)
  provider         : ${prov_ver:-pinned ~> 3.0 in compat/provider.tf}
  module           : test/terraform/compat (unmodified terraform-provider-openstack)
  image            : '$IMAGE_NAME' $( [ -n "$CREATED_IMAGE_ID" ] && echo "(created and deleted by this run)" || echo "(pre-existing, kept)")
  flavor           : '$FLAVOR_NAME'
  init/validate    : OK
  apply            : OK
  post-apply plan  : no drift (exit 0)
  destroy          : OK
  state list       : empty
  outputs          : none
  openstack residue: 0 tf-compat-* objects
  post-destroy plan: adds only, no residue
EOF
exit 0
