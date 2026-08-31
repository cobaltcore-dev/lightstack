#!/bin/bash
# SPEC-025 — Nova gaps & client discovery (cloud-storage#547)
#
# Integration checks that need a real client rather than a Go test: the defects in
# this feature are ones a stock OpenStack client trips over, and several are invisible
# to gophercloud because it does not parse version documents the way keystoneauth does.
#
# Not using `set -e`: every check is counted, and one failure must not hide the rest.
# (The sibling harness ticket found `((X++))` under `set -e` silently killing scripts —
# the increments below are written `X=$((X+1))` for that reason.)

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
NC='\033[0m'

PASSED=0
FAILED=0
SKIPPED=0

pass() { echo -e "${GREEN}✓${NC} $1"; PASSED=$((PASSED+1)); }
fail() { echo -e "${RED}✗${NC} $1"; [ -n "$2" ] && echo "    $2"; FAILED=$((FAILED+1)); }
skip() { echo -e "${YELLOW}○${NC} $1 (skipped: $2)"; SKIPPED=$((SKIPPED+1)); }

: "${OS_AUTH_URL:=http://localhost:35357/v3}"
: "${OS_USERNAME:=admin}"
: "${OS_PASSWORD:=secret}"
: "${OS_PROJECT_NAME:=default}"
: "${OS_USER_DOMAIN_NAME:=Default}"
: "${OS_PROJECT_DOMAIN_NAME:=Default}"
: "${OS_IDENTITY_API_VERSION:=3}"
: "${NOVA_URL:=http://localhost:8774}"
export OS_AUTH_URL OS_USERNAME OS_PASSWORD OS_PROJECT_NAME
export OS_USER_DOMAIN_NAME OS_PROJECT_DOMAIN_NAME OS_IDENTITY_API_VERSION

if ! curl -sf "$OS_AUTH_URL" >/dev/null 2>&1; then
    echo "Lightstack is not reachable at $OS_AUTH_URL — start it first."
    exit 1
fi
if ! command -v openstack >/dev/null 2>&1; then
    echo "python-openstackclient is not installed."
    exit 1
fi

echo "=============================================="
echo " SPEC-025 — client discovery & compute gaps"
echo " client: $(openstack --version 2>&1)"
echo "=============================================="
echo

# ---------------------------------------------------------------------------
# US1 — discovery and self-links (B3, B4)
# ---------------------------------------------------------------------------
echo "--- US1: discovery and self-links ---"

# B4: the version document must carry links. Without them python-openstackclient
# aborts inside its own library on an unconditional index into version.links.
LINKS=$(curl -s "$NOVA_URL/v2.1" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(len(d["version"].get("links") or []))' 2>/dev/null)
if [ "$LINKS" -ge 1 ] 2>/dev/null; then
    pass "GET /v2.1 carries version.links (B4)"
else
    fail "GET /v2.1 has no version.links (B4)" "this is what aborts 'openstack flavor list'"
fi

# The href must be absolute — a relative or scheme-less link is unresolvable.
HREF=$(curl -s "$NOVA_URL/v2.1" | python3 -c 'import sys,json; d=json.load(sys.stdin); ls=d["version"].get("links") or []; print(next((l["href"] for l in ls if l.get("rel")=="self"), ""))' 2>/dev/null)
case "$HREF" in
    http://*|https://*) pass "version.links self href is absolute: $HREF" ;;
    "")                 fail "no rel=self link in the version document" ;;
    *)                  fail "self href is not absolute: $HREF" ;;
esac

# The version document must echo the host the CLIENT addressed, not one the
# deployment configures for itself (FR-002) — the observable half of B3.
ECHOED=$(curl -s -H "Host: client.chose.me:8774" "$NOVA_URL/v2.1" | python3 -c 'import sys,json; d=json.load(sys.stdin); ls=d["version"].get("links") or []; print(next((l["href"] for l in ls if l.get("rel")=="self"), ""))' 2>/dev/null)
if echo "$ECHOED" | grep -q "client.chose.me:8774"; then
    pass "version document echoes the addressed host (B3/FR-002)"
else
    fail "version document does not echo the addressed host (B3)" "got: $ECHOED"
fi

# B4, end to end: the client must list flavors with NO version pinned.
#
# Requires a deployment whose CATALOG host is resolvable from here. LIGHTSTACK_ENDPOINT_HOST
# rewrites the catalog endpoints (keystone/auth.go substituteURLTemplates) and that
# path still ignores the request host, so pointing it at a name this machine cannot
# resolve breaks every command before any version document is read. That is a
# pre-existing catalog concern owned by SPEC-007, not part of this feature — but it
# will make this check fail for a reason that has nothing to do with B4, so it is
# named here rather than left to be rediscovered.
unset OS_COMPUTE_API_VERSION
if FLAVORS=$(openstack flavor list -f value -c Name 2>&1); then
    COUNT=$(echo "$FLAVORS" | grep -c .)
    pass "openstack flavor list with no OS_COMPUTE_API_VERSION ($COUNT flavors)"
else
    fail "openstack flavor list failed with no version pinned (B4)" "$(echo "$FLAVORS" | tail -2)"
fi

# Endpoint-override smoke, NOT a proof about Lightstack.
#
# These two were written as the B3 proof and then shown not to be one: with the
# server-side defect deliberately reinstated (BaseURL reverted to configured-wins)
# BOTH still passed, because python-openstackclient 10.x honours
# OS_*_ENDPOINT_OVERRIDE client-side and never consults the version document for
# that service. A check that cannot fail is worse than no check, so they are kept
# only as client-behaviour smoke and labelled as such.
#
# The checks above are the real B3 evidence: with the defect reinstated, "echoes the
# addressed host" and "flavor list with no version pinned" both go red — the second
# because the client is sent to a hostname that does not resolve.
if OS_COMPUTE_ENDPOINT_OVERRIDE="http://127.0.0.1:18774/v2.1" openstack flavor list >/dev/null 2>&1; then
    fail "a dead endpoint override still succeeded" "the override was ignored entirely"
else
    pass "endpoint override smoke: a dead override is not silently replaced"
fi
if OS_COMPUTE_ENDPOINT_OVERRIDE="http://127.0.0.1:8774/v2.1" openstack flavor list >/dev/null 2>&1; then
    pass "endpoint override smoke: a live alternate name works"
else
    fail "endpoint override to a live alternate name failed"
fi

# FR-007a/FR-007b: sibling services' version documents must be absolute too. A
# client reads whichever service it is about to use.
for svc_port in "network:9696:/v2.0" "blockstorage:8776:/v3"; do
    NAME=${svc_port%%:*}
    REST=${svc_port#*:}
    PORT=${REST%%:*}
    PATH_=${REST#*:}
    H=$(curl -s "http://localhost:$PORT$PATH_" | python3 -c 'import sys,json; d=json.load(sys.stdin); v=d.get("version") or {}; ls=v.get("links") or []; print(ls[0]["href"] if ls else "")' 2>/dev/null)
    case "$H" in
        http://*|https://*)
            if echo "$H" | grep -q "://lightstack[:/]"; then
                fail "$NAME version href names the compose hostname: $H"
            else
                pass "$NAME version href is absolute: $H"
            fi
            ;;
        *) fail "$NAME version href is not absolute: '$H'" ;;
    esac
done

echo
# ---------------------------------------------------------------------------
# US2 — identity read-back (B16)
# ---------------------------------------------------------------------------
echo "--- US2: identity read-back ---"

# The response shape is the real check and it holds on any client. B16's *symptom*
# needs python-openstackclient 6.x, which does an unconditional _info.pop('links');
# 10.x tolerates the omission, so a green `project show` here proves much less than it
# looks like it does. Both are run, and the version-specific one is reported honestly.
CLIENT_VER=$(openstack --version 2>&1 | awk '{print $2}')
CLIENT_MAJOR=${CLIENT_VER%%.*}

TOKEN=$(openstack token issue -f value -c id 2>/dev/null)
if [ -z "$TOKEN" ]; then
    fail "could not issue a token; skipping the identity checks"
else
    PROJ_ID=$(openstack project show default -f value -c id 2>/dev/null)
    for res in "projects:$PROJ_ID"; do
        KIND=${res%%:*}
        RID=${res#*:}
        HAS=$(curl -s -H "X-Auth-Token: $TOKEN" "$OS_AUTH_URL/$KIND/$RID" | python3 -c 'import sys,json; d=json.load(sys.stdin); o=list(d.values())[0]; l=(o or {}).get("links") or {}; print(l.get("self",""))' 2>/dev/null)
        case "$HAS" in
            http://*|https://*) pass "GET /v3/$KIND/{id} carries an absolute links.self (B16)" ;;
            "")                 fail "GET /v3/$KIND/{id} has no links.self (B16)" "aborts a 6.x client with a bare 'links'" ;;
            *)                  fail "links.self is not absolute: $HAS" ;;
        esac
    done

    # The COLLECTION entries matter as much: `<resource> show <name>` resolves the
    # name through a list first, then pops links from the object it found there.
    for coll in projects users roles; do
        MISSING=$(curl -s -H "X-Auth-Token: $TOKEN" "$OS_AUTH_URL/$coll" | python3 -c "
import sys,json
d=json.load(sys.stdin)
items=d.get('$coll') or []
print(sum(1 for i in items if not ((i.get('links') or {}).get('self','').startswith('http'))))" 2>/dev/null)
        if [ "$MISSING" = "0" ]; then
            pass "every /v3/$coll entry carries an absolute links.self (B16, name lookup path)"
        else
            fail "$MISSING /v3/$coll entries lack an absolute links.self (B16)"
        fi
    done
fi

# The three commands B16 names. On 10.x this is a smoke test; on 6.x it is the defect.
for cmd in "project show default" "user show admin" "role show admin"; do
    if openstack $cmd -f value -c id >/dev/null 2>&1; then
        if [ "$CLIENT_MAJOR" -le 6 ] 2>/dev/null; then
            pass "openstack $cmd (6.x client — this is the B16 reproduction)"
        else
            pass "openstack $cmd (client $CLIENT_VER — smoke only; 6.x is where B16 bites)"
        fi
    else
        fail "openstack $cmd failed" "on a 6.x client this is B16"
    fi
done

# T031: the fixture pattern SPEC-023's real-mode runbook uses. The failure mode is
# silent — on a 6.x client these come back EMPTY rather than erroring, so every later
# step of that runbook reads the wrong thing.
FIX_PROJ=$(openstack project show default -f value -c id 2>/dev/null)
FIX_USER=$(openstack user show admin -f value -c id 2>/dev/null)
if [ -n "$FIX_PROJ" ] && [ -n "$FIX_USER" ]; then
    pass "runbook fixture pattern yields non-empty identifiers (B16 degradation)"
else
    fail "runbook fixture pattern degraded to empty identifiers (B16)" "proj='$FIX_PROJ' user='$FIX_USER'"
fi

if [ "$CLIENT_MAJOR" -gt 6 ] 2>/dev/null; then
    skip "B16 symptom reproduction" "needs python-openstackclient 6.x; this host has $CLIENT_VER"
fi

echo
# ---------------------------------------------------------------------------
# US8 — access-log attribution (B15)
# ---------------------------------------------------------------------------
echo "--- US8: access-log attribution ---"

# A cross-project action needs a second project and an administrator. The compute
# dispatcher rebinds the request's project to the server's OWNER so every sub-handler
# scopes correctly (SPEC-023) — and the log used to read that rebound value, naming the
# tenant acted upon rather than the operator who acted.
#
# Driven with curl rather than the CLI so the token and the target are chosen explicitly.
ADMIN_TOKEN=$(openstack token issue -f value -c id 2>/dev/null)
if [ -z "$ADMIN_TOKEN" ]; then
    fail "could not issue an admin token; skipping the attribution check"
else
    CALLER_PROJ=$(openstack project show default -f value -c id 2>/dev/null)

    # A second project owning a server, so caller and target genuinely differ.
    OTHER_PROJ=$(curl -s -X POST "$OS_AUTH_URL/projects" -H "X-Auth-Token: $ADMIN_TOKEN" \
        -H 'Content-Type: application/json' \
        -d '{"project":{"name":"spec025-b15","description":"SPEC-025 B15 probe","enabled":true}}' \
        | python3 -c 'import sys,json; d=json.load(sys.stdin); print((d.get("project") or {}).get("id",""))' 2>/dev/null)

    if [ -z "$OTHER_PROJ" ]; then
        skip "cross-project log attribution" "could not create a second project"
    else
        IMG=$(curl -s "http://localhost:9292/v2/images" -H "X-Auth-Token: $ADMIN_TOKEN" \
            | python3 -c 'import sys,json; imgs=[i["id"] for i in json.load(sys.stdin).get("images",[]) if i.get("status")=="active"]; print(imgs[0] if imgs else "")' 2>/dev/null)

        # Seed the server directly in the other project: creating it through the API
        # would scope it to the caller's own project, which is the opposite of the setup.
        # head -1 is load-bearing: psql prints the RETURNING value AND an "INSERT 0 1"
        # status line, so without it the variable holds both concatenated and every
        # later use silently addresses nothing.
        SRV=$(docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc \
            "INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state) VALUES ('spec025-b15-srv','$OTHER_PROJ','00000000-0000-0000-0000-000000000001',(SELECT id FROM flavors LIMIT 1),'ACTIVE',1) RETURNING id;" 2>/dev/null | head -1 | tr -d ' \r')

        if [ -z "$SRV" ]; then
            skip "cross-project log attribution" "could not seed a server in the second project"
        else
            # Act on it as the administrator, then read the log line for THIS request.
            REQ_MARK="/v2.1/servers/$SRV/action"
            curl -s -o /dev/null -X POST "http://localhost:8774$REQ_MARK" \
                -H "X-Auth-Token: $ADMIN_TOKEN" -H 'Content-Type: application/json' \
                -d '{"lock": null}'

            LINE=$(grep -F "\"path\":\"$REQ_MARK\"" "${LIGHTSTACK_LOG:-/tmp/lightstack.log}" 2>/dev/null | grep 'request completed' | tail -1)
            if [ -z "$LINE" ]; then
                skip "cross-project log attribution" "no log line for $REQ_MARK in ${LIGHTSTACK_LOG:-/tmp/lightstack.log}"
            else
                LOGGED_CALLER=$(echo "$LINE" | python3 -c 'import sys,json; print(json.loads(sys.stdin.read()).get("project_id",""))' 2>/dev/null)
                LOGGED_TARGET=$(echo "$LINE" | python3 -c 'import sys,json; print(json.loads(sys.stdin.read()).get("target_project_id",""))' 2>/dev/null)

                if [ "$LOGGED_CALLER" = "$CALLER_PROJ" ]; then
                    pass "the log names the OPERATOR's project, not the target (B15)"
                else
                    fail "the log names the wrong project (B15)" "logged=$LOGGED_CALLER caller=$CALLER_PROJ target=$OTHER_PROJ"
                fi
                if [ "$LOGGED_TARGET" = "$OTHER_PROJ" ]; then
                    pass "the target project is identifiable from the same line (FR-046)"
                else
                    fail "the target project is not on the log line (FR-046)" "target_project_id=$LOGGED_TARGET want=$OTHER_PROJ"
                fi
            fi

            # The action record must still be attributed to the OWNER, not the operator.
            REC=$(docker exec lightstack-postgres psql -U lightstack -d lightstack -tAc \
                "SELECT project_id FROM instance_actions WHERE instance_id='$SRV' ORDER BY start_time DESC LIMIT 1;" 2>/dev/null | tr -d ' \r')
            if [ "$REC" = "$OTHER_PROJ" ]; then
                pass "the action record is still attributed to the owning tenant (FR-048)"
            else
                fail "the action record names the wrong project (FR-048)" "recorded=$REC want=$OTHER_PROJ"
            fi

            docker exec lightstack-postgres psql -U lightstack -d lightstack -q -c \
                "DELETE FROM instance_actions WHERE instance_id='$SRV'; DELETE FROM instances WHERE id='$SRV';" >/dev/null 2>&1
        fi
        curl -s -o /dev/null -X DELETE "$OS_AUTH_URL/projects/$OTHER_PROJ" -H "X-Auth-Token: $ADMIN_TOKEN"
    fi
fi

echo
echo "=============================================="
echo -e " passed: ${GREEN}${PASSED}${NC}  failed: ${RED}${FAILED}${NC}  skipped: ${YELLOW}${SKIPPED}${NC}"
echo "=============================================="

[ "$FAILED" -eq 0 ]
