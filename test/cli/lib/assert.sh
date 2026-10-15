#!/bin/bash
# assert.sh — assertions on the OpenStack CLI's last output (SPEC-028).
#
# Locked API (suites depend on these names):
#   assert_rc <expected-rc>                          last osc() exit code
#   assert_jq <jq-expr> [expected]                   evaluate jq on OSC_OUT;
#                                                    without <expected>: pass when
#                                                    the expression is truthy
#                                                    (non-null, non-false)
#   assert_jq_ne <jq-expr> <unexpected>
#   assert_jq_gt <jq-expr> <number>                  numeric greater-than
#   assert_jq_type <jq-expr> <type>                  "array", "object", "string", ...
#   assert_jq_len <jq-expr> <n>                      array/object length
#   assert_contains <substring>                      OSC_OUT contains substring
#   assert_err_contains <substring>                  OSC_ERR contains substring
#   assert_regex <posix-ERE>                         OSC_OUT matches
#
# A failed assertion calls t_fail() (the case fails at t_end) and execution
# CONTINUES — later assertions in the same case still run, so one bad field
# produces one case failure with all the details, not an aborted suite.

# Guard against double-sourcing.
[[ -n "${_CLI_ASSERT_SOURCED:-}" ]] && return 0
_CLI_ASSERT_SOURCED=1

# Truncate values in error messages so a full JSON body does not flood the log.
_snip() {
    local s="$1"
    if (( ${#s} > 300 )); then
        printf '%s...' "${s:0:300}"
    else
        printf '%s' "$s"
    fi
}

# Internal: the actual jq evaluation. Sets _JQ_RESULT / _JQ_ERROR.
_jq_eval() {
    local expr="$1" out
    if out=$(printf '%s' "$OSC_OUT" | jq -r "$expr" 2>&1); then
        _JQ_RESULT="$out"
        _JQ_ERROR=""
        return 0
    fi
    _JQ_RESULT=""
    _JQ_ERROR="$out"
    return 1
}

_jq_fail() {
    t_fail "$1 (expr: $2, got: $3, stderr: $4, output was: $(_snip "$OSC_OUT"))"
}

assert_rc() {
    local expected="$1"
    if [[ "$OSC_RC" != "$expected" ]]; then
        t_fail "exit code expected $expected, got $OSC_RC; stderr: $(_snip "$OSC_ERR")"
    fi
}

assert_jq() {
    local expr="$1" expected="${2:-}"
    if ! _jq_eval "$expr"; then
        _jq_fail "jq failed on expression" "$expr" "$_JQ_ERROR"
        return
    fi
    if (( $# >= 2 )); then
        if [[ "$_JQ_RESULT" != "$expected" ]]; then
            _jq_fail "unexpected jq result" "$expr" "want '$expected', got '$_JQ_RESULT'"
        fi
    else
        # Truthiness: null and false are falsy, everything else passes.
        if [[ -z "$_JQ_RESULT" || "$_JQ_RESULT" == "null" || "$_JQ_RESULT" == "false" ]]; then
            _jq_fail "jq expression not truthy" "$expr" "'$_JQ_RESULT'"
        fi
    fi
}

assert_jq_ne() {
    local expr="$1" unexpected="$2"
    if ! _jq_eval "$expr"; then
        _jq_fail "jq failed on expression" "$expr" "$_JQ_ERROR"
        return
    fi
    if [[ "$_JQ_RESULT" == "$unexpected" ]]; then
        _jq_fail "jq result must not equal" "$expr" "'$unexpected'"
    fi
}

assert_jq_gt() {
    local expr="$1" bound="$2"
    if ! _jq_eval "$expr"; then
        _jq_fail "jq failed on expression" "$expr" "$_JQ_ERROR"
        return
    fi
    if ! [[ "$_JQ_RESULT" =~ ^-?[0-9]+(\.[0-9]+)?$ ]]; then
        _jq_fail "jq result is not numeric" "$expr" "'$_JQ_RESULT'"
        return
    fi
    if ! awk -v a="$_JQ_RESULT" -v b="$bound" 'BEGIN { exit !(a > b) }'; then
        _jq_fail "jq result not greater than bound" "$expr" "$_JQ_RESULT <= $bound"
    fi
}

assert_jq_type() {
    local expr="$1" want="$2"
    local got
    got=$(printf '%s' "$OSC_OUT" | jq -r "$expr | type" 2>/dev/null)
    if [[ "$got" != "$want" ]]; then
        t_fail "jq type of '$expr' expected $want, got ${got:-unset}; output was: $(_snip "$OSC_OUT")"
    fi
}

assert_jq_len() {
    local expr="$1" want="$2"
    local got
    got=$(printf '%s' "$OSC_OUT" | jq -r "$expr | length" 2>/dev/null)
    if [[ "$got" != "$want" ]]; then
        t_fail "length of '$expr' expected $want, got ${got:-unset}; output was: $(_snip "$OSC_OUT")"
    fi
}

assert_contains() {
    local needle="$1"
    if [[ "$OSC_OUT" != *"$needle"* ]]; then
        t_fail "output does not contain '$needle'; output: $(_snip "$OSC_OUT")"
    fi
}

assert_err_contains() {
    local needle="$1"
    if [[ "$OSC_ERR" != *"$needle"* ]]; then
        t_fail "stderr does not contain '$needle'; stderr: $(_snip "$OSC_ERR")"
    fi
}

assert_regex() {
    local pattern="$1"
    if ! printf '%s' "$OSC_OUT" | grep -Eq "$pattern"; then
        t_fail "output does not match /$pattern/; output: $(_snip "$OSC_OUT")"
    fi
}
