package keystone

import (
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// uuidParamMiddleware rejects a management request whose UUID path parameter is
// not a well-formed UUID, returning Keystone's verbatim 404
// ("Could not find <resource>: <id>.") before the handler runs. Lightstack's identity
// id columns hold canonical ids, so a non-UUID can never name a row — 404 is the
// faithful behavior, and validating up front avoids the `22P02` cast that would
// otherwise surface as 500 (cloud-storage#515).
//
// Well-formed UUID params have already been canonicalized by the parent management
// group's canonicalizeUUIDParams middleware, so this middleware only rejects malformed
// values. That ordering keeps the id comparison compatible with SQLite's text columns
// while preserving the same 404 behavior as PostgreSQL's `uuid` columns.
//
// It validates only params named "id" or "<something>_id"; the EC2 "access" key
// and any other param pass through untouched (FR-005). It is registered only on
// the non-authz-gated route group, so authentication (401) and authorization
// (403), which run for the gated routes, still precede a malformed-id 404.
func uuidParamMiddleware(c *gin.Context) {
	for i := range c.Params {
		p := c.Params[i]
		if !isUUIDParam(p.Key) {
			continue
		}
		if uuid.Validate(p.Value) != nil {
			common.AbortWithError(c, common.NewIdentityNotFoundError(resourceNoun(c.FullPath(), p.Key), p.Value))
			return
		}
	}
	// No c.Next() needed: when a middleware returns without aborting, gin's engine
	// advances to the next handler on its own.
}

// canonicalizeUUIDParams normalizes only well-formed UUID path params. It is
// installed after authentication on the management group, before uuidParamMiddleware
// on its validation-only subgroup, so authorization-gated handlers can still reject
// malformed IDs after their authorization check.
func canonicalizeUUIDParams(c *gin.Context) {
	for i := range c.Params {
		p := &c.Params[i]
		if !isUUIDParam(p.Key) {
			continue
		}
		if id, err := uuid.Parse(p.Value); err == nil {
			p.Value = id.String()
		}
	}
}

// isUUIDParam reports whether a gin path-param name addresses a UUID id.
func isUUIDParam(name string) bool {
	return name == "id" || strings.HasSuffix(name, "_id")
}

// resourceNoun derives the Keystone resource noun for a path param as the
// singular of the path segment immediately preceding ":<param>" in the route.
func resourceNoun(fullPath, param string) string {
	segs := strings.Split(strings.Trim(fullPath, "/"), "/")
	for i, s := range segs {
		if s == ":"+param && i > 0 {
			return singularize(segs[i-1])
		}
	}
	// Unreachable: a validated param (name "id" / "*_id") is present only because
	// the matched route registered it, so c.FullPath() always contains ":<param>"
	// with a preceding resource segment. Fail loud if that invariant is ever broken
	// (a routing bug) rather than emit a bogus "Could not find resource:" message.
	log.Error().Str("param", param).Str("path", fullPath).Msg("resourceNoun: uuid param not found in route template")
	panic("resourceNoun: param " + param + " not found in route " + fullPath)
}

// keystoneNouns maps a URL collection segment to the resource noun Keystone puts in
// its "Could not find <noun>: <id>." message. It mirrors keystone/exception.py,
// which hardcodes one *NotFound class per resource (PolicyNotFound -> "Could not
// find policy: …", ApplicationCredentialNotFound -> "… Application Credential: …")
// and does no pluralization at all. Deriving the noun by trimming a trailing "s"
// would therefore diverge on irregular plurals — "policies" would yield "policie" —
// so every identity collection gets an explicit entry here instead.
var keystoneNouns = map[string]string{
	"projects":                "project",
	"users":                   "user",
	"roles":                   "role",
	"groups":                  "group",
	"domains":                 "domain",
	"services":                "service",
	"endpoints":               "endpoint",
	"credentials":             "credential",
	"application_credentials": "Application Credential",
}

// singularize turns a URL collection segment into Keystone's resource noun.
// TestResourceNounCoversEveryRegisteredRoute requires every collection in the
// identity route table to have an entry in keystoneNouns, so the naive fallback
// below only ever serves a newly added route that CI has already flagged.
func singularize(seg string) string {
	if noun, ok := keystoneNouns[seg]; ok {
		return noun
	}
	return strings.TrimSuffix(strings.ReplaceAll(seg, "_", " "), "s")
}
