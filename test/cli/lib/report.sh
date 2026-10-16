#!/bin/bash
# report.sh — case recording and reporting for the CLI conformance harness (SPEC-028).
#
# Locked API:
#   _record_case <suite> <id> <status> <dur-ms> <command> <error>
#                       append one case record to $RESULTS_FILE (JSONL)
#   report_write <dir>  aggregate $RESULTS_FILE into <dir>/results.json and
#                       <dir>/results.junit.xml
#   report_summary      human-readable summary to stderr
#   report_failed       returns 0 (success) when some case FAILED — callers
#                       write `if report_failed; then rc=1; fi`. Strict mode
#                       (CLI_STRICT=1) also counts known-failing and skip.
#
# Case statuses: pass | fail | known-failing | skip
# Standalone suite runs (bash suites/foo.sh outside bin/run.sh) automatically
# get their own results directory under results/standalone/.

# Guard against double-sourcing.
[[ -n "${_CLI_REPORT_SOURCED:-}" ]] && return 0
_CLI_REPORT_SOURCED=1

_record_case() {
    local suite="$1" id="$2" status="$3" dur="$4" cmd="$5" error="$6"

    # Lazy RESULTS_FILE bootstrap for standalone suite runs.
    if [[ -z "${RESULTS_FILE:-}" ]]; then
        local dir="$CLI_ROOT/results/standalone"
        mkdir -p "$dir"
        RESULTS_FILE="$dir/${SUITE_NAME:-suite}-$(date -u +%Y%m%dT%H%M%SZ)-$$.jsonl"
        export RESULTS_FILE
        : >"$RESULTS_FILE"
        _STANDALONE_RUN=1
    fi

    # dur must be a non-negative integer even if a caller passed garbage.
    [[ "$dur" =~ ^[0-9]+$ ]] || dur=0

    _CASES_RECORDED=1

    jq -nc \
        --arg suite "$suite" \
        --arg id "$id" \
        --arg status "$status" \
        --argjson duration_ms "$dur" \
        --arg command "$cmd" \
        --arg error "$error" \
        '{suite: $suite, id: $id, status: $status, duration_ms: $duration_ms,
          command: $command, error: $error}' >>"$RESULTS_FILE"
}

_osc_version_cached() {
    if [[ -n "${_CLI_OSC_VERSION:-}" ]]; then
        return 0
    fi
    if [[ -n "${OSC_BIN:-}" ]] || OSC_BIN="$(command -v openstack 2>/dev/null)"; then
        _CLI_OSC_VERSION="$("$OSC_BIN" --version 2>&1 | head -n 1)"
    else
        _CLI_OSC_VERSION="unknown (CLI not found)"
    fi
}

_git_rev() {
    git -C "$CLI_ROOT/../../.." rev-parse --short HEAD 2>/dev/null || echo "unknown"
}

report_write() {
    local outdir="$1"
    mkdir -p "$outdir"

    _osc_version_cached
    local generated auth_url git_rev
    generated="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    auth_url="${OS_AUTH_URL:-unknown}"
    git_rev="$(_git_rev)"

    # results.json — full aggregate with metadata.
    jq -s \
        --arg generated "$generated" \
        --arg cli_version "$_CLI_OSC_VERSION" \
        --arg auth_url "$auth_url" \
        --arg git_rev "$git_rev" \
        '{generated: $generated, cli_version: $cli_version, auth_url: $auth_url,
          git_rev: $git_rev,
          total: length,
          passed: [.[] | select(.status == "pass")] | length,
          failed: [.[] | select(.status == "fail")] | length,
          known_failing: [.[] | select(.status == "known-failing")] | length,
          skipped: [.[] | select(.status == "skip")] | length,
          total_duration_ms: ([.[].duration_ms] | add // 0),
          cases: .}' \
        "$RESULTS_FILE" >"$outdir/results.json"

    # results.junit.xml — one <testsuite> per suite, standard JUnit 4 shape.
    {
        printf '<?xml version="1.0" encoding="UTF-8"?>\n'
        printf '<testsuites name="lightstack-cli-conformance" tests="%d" failures="%d" skipped="%d" time="%.3f">\n' \
            "$(jq -s 'length' "$RESULTS_FILE")" \
            "$(jq -s '[.[] | select(.status == "fail")] | length' "$RESULTS_FILE")" \
            "$(jq -s '[.[] | select(.status == "skip" or .status == "known-failing")] | length' "$RESULTS_FILE")" \
            "$(jq -s '([.[].duration_ms] | add // 0) / 1000' "$RESULTS_FILE")"

        local suites
        # JSONL input: each line is one record — no -s slurp, address records
        # directly (.suite), not .[].suite (which would iterate field values).
        suites=$(jq -r '.suite' "$RESULTS_FILE" | awk '!seen[$0]++')
        local s case_line
        while IFS= read -r s; do
            [[ -z "$s" ]] && continue
            local n fail skip time
            n=$(jq -s --arg s "$s" '[.[] | select(.suite == $s)] | length' "$RESULTS_FILE")
            fail=$(jq -s --arg s "$s" '[.[] | select(.suite == $s and .status == "fail")] | length' "$RESULTS_FILE")
            skip=$(jq -s --arg s "$s" '[.[] | select(.suite == $s and (.status == "skip" or .status == "known-failing"))] | length' "$RESULTS_FILE")
            time=$(jq -s --arg s "$s" '([.[] | select(.suite == $s) | .duration_ms] | add // 0) / 1000' "$RESULTS_FILE")
            printf '  <testsuite name="%s" tests="%d" failures="%d" skipped="%d" time="%.3f">\n' \
                "$(_xml_escape "$s")" "$n" "$fail" "$skip" "$time"
            while IFS= read -r case_line; do
                # case_line: "<classname>\t<name>\t<status>\t<duration_ms>\t<command>\t<error>"
                local cname name status dur cmd err
                IFS=$'\t' read -r cname name status dur cmd err <<<"$case_line"
                printf '    <testcase classname="%s" name="%s" time="%.3f"' \
                    "$(_xml_escape "$cname")" "$(_xml_escape "$name")" "$(awk -v d="${dur:-0}" 'BEGIN { printf "%.3f", d / 1000 }')"
                if [[ "$status" == "fail" ]]; then
                    printf '>\n      <failure message="%s">%s</failure>\n    </testcase>\n' \
                        "$(_xml_escape "${cmd:-}")" "$(_xml_escape "${err:-assertion failed}")"
                elif [[ "$status" == "known-failing" ]]; then
                    printf '>\n      <skipped message="%s">%s</skipped>\n    </testcase>\n' \
                        "$(_xml_escape "known failure: ${err:-}")" "$(_xml_escape "${err:-}")"
                elif [[ "$status" == "skip" ]]; then
                    printf '>\n      <skipped message="%s"/>\n    </testcase>\n' \
                        "$(_xml_escape "${err:-skipped}")"
                else
                    printf '/>\n'
                fi
            done < <(jq -r --arg s "$s" \
                'select(.suite == $s) | [.suite, .id, .status, (.duration_ms | tostring), .command, .error] | @tsv' \
                "$RESULTS_FILE")
            printf '  </testsuite>\n'
        done <<<"$suites"
        printf '</testsuites>\n'
    } >"$outdir/results.junit.xml"
}

_xml_escape() {
    # Escape the XML specials WITHOUT ${var//pat/repl}: bash >= 5.2 enables
    # patsub_replacement by default, and a replacement starting with '&'
    # then expands to the matched text — &amp; became &&amp; and the JUnit
    # file was no longer well-formed. A per-character case is portable
    # across every bash that can run this harness.
    local s="$1" out='' ch i
    for (( i = 0; i < ${#s}; i++ )); do
        ch="${s:i:1}"
        case "$ch" in
            '&') out+='&amp;' ;;
            '<') out+='&lt;' ;;
            '>') out+='&gt;' ;;
            '"') out+='&quot;' ;;
            "'") out+='&apos;' ;;
            *) out+="$ch" ;;
        esac
    done
    # Strip control characters that are illegal in XML 1.0.
    printf '%s' "$out" | tr -d '\000-\010\013\014\016-\037'
}

report_summary() {
    [[ -s "${RESULTS_FILE:-}" ]] || return 0
    echo
    echo "================ CLI conformance summary ================"
    jq -r -s '
        "  total:         \(length)",
        "  passed:        \([.[] | select(.status == "pass")] | length)",
        "  failed:        \([.[] | select(.status == "fail")] | length)",
        "  known-failing: \([.[] | select(.status == "known-failing")] | length)",
        "  skipped:       \([.[] | select(.status == "skip")] | length)"
    ' "$RESULTS_FILE" >&2
    echo "========================================================="
    local failed_ids
    failed_ids=$(jq -r -s '[.[] | select(.status == "fail") | "  \(.id): \(.error)"] | join("\n")' "$RESULTS_FILE")
    if [[ -n "$failed_ids" ]]; then
        echo "Failures:" >&2
        printf '%s\n' "$failed_ids" >&2
    fi
}

report_failed() {
    # Returns 0 when the run HAS failing cases (so callers write
    # `if report_failed; then rc=1; fi`), non-zero when it is clean.
    # Strict mode also counts known-failing and skipped cases: the nightly
    # wants to be told when its debt grows back, not only when it breaks.
    [[ -s "${RESULTS_FILE:-}" ]] || return 1
    if [[ "${CLI_STRICT:-0}" == "1" ]]; then
        jq -e -s 'any(.[]; .status == "fail" or .status == "known-failing" or .status == "skip")' "$RESULTS_FILE" >/dev/null
    else
        jq -e -s 'any(.[]; .status == "fail")' "$RESULTS_FILE" >/dev/null
    fi
}
