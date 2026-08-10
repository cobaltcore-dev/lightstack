package common

import (
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
)

// queryBoolTrue and queryBoolFalse are upstream OpenStack's boolean vocabulary
// for query parameters, as documented for Nova's `all_tenants`.
//
// Both lists are needed, not just the affirmative one: a caller that filters must
// distinguish "the value said false" from "the value was not a boolean at all",
// because silently narrowing a collection on unparseable input is worse than
// ignoring the parameter. See ParseQueryBool.
//
// Go's strconv.ParseBool is deliberately not used: it accepts only
// 1/t/T/TRUE/true/True and 0/f/F/FALSE/false/False, so it rejects "on", "off",
// "y", "yes" and "no" — all of which real clients send — and it has no way to
// express the valueless form.
var (
	queryBoolTrue  = []string{"1", "t", "true", "on", "y", "yes"}
	queryBoolFalse = []string{"0", "f", "false", "off", "n", "no"}
)

// ParseQueryBool reports the boolean a query value denotes, using upstream's
// vocabulary, and whether it was recognised at all.
//
// The second return is the point. QueryFlag can fail closed on garbage because
// its only question is "did the caller opt in". A filter cannot: turning
// `?enabled=banana` into `enabled = false` returns a wrong subset, where ignoring
// the parameter returns an unfiltered one. Same words, two deliberate behaviours.
func ParseQueryBool(v string) (value, ok bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	if slices.Contains(queryBoolTrue, v) {
		return true, true
	}
	if slices.Contains(queryBoolFalse, v) {
		return false, true
	}
	return false, false
}

// QueryFlag reports whether a boolean query parameter is enabled, following
// upstream's conventions rather than Go's.
//
//   - absent                  → false
//   - present with no value   → TRUE  ("?all_tenants" and "?all_tenants=")
//   - a recognised true word  → true, case-insensitively
//   - a recognised false word → false
//   - anything else           → false
//
// The valueless case is the one that matters most and the one a naive
// implementation gets wrong: upstream documents that `all_tenants` "defaults to
// True" when used without a value, and that is the form Horizon sends. Reading
// the parameter through c.Query() alone cannot distinguish "?all_tenants" from
// an absent parameter — both yield "" — so this inspects the parsed query map
// for the key's presence first.
//
// Unrecognised values fail closed. Upstream parses non-strictly and treats them
// as false; here that is also the safe direction, because the only current
// caller uses this to decide whether to widen a listing beyond the caller's own
// project. An unreadable value must never widen.
//
// Lives in common rather than nova because the same question is already asked
// ad hoc in two other services (neutron's "shared", cinder's "usage"), which can
// adopt this without a behavioural change.
func QueryFlag(c *gin.Context, name string) bool {
	if c.Request == nil {
		return false
	}
	values, present := c.Request.URL.Query()[name]
	if !present {
		return false
	}
	// Present with no value, or with an explicitly empty value, means enabled.
	if len(values) == 0 || values[0] == "" {
		return true
	}
	// Unrecognised values fail closed here: the only current caller uses this to
	// decide whether to widen a listing beyond the caller's own project, and an
	// unreadable value must never widen.
	value, _ := ParseQueryBool(values[0])
	return value
}
