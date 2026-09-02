package nova

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/gin-gonic/gin"
)

// Requested-microversion handling: negotiation is read HERE and nowhere else.
//
// SPEC-025 left one gate in this file with a note that generalising negotiation was a
// concern of its own. SPEC-025-followup US6 is that concern, bounded by FR-042 to
// behaviours the product actually emits: the audit is recorded in
// docs/specs/025-followup/microversion-audit.md, and every gate it produced reads the
// version through this file.
//
// A MICROVERSION IS A PAIR OF INTEGERS, NOT A DECIMAL. 2.9 precedes 2.21 precedes 2.45,
// because the minor part counts revisions rather than naming a fraction. An earlier
// revision of this file compared parsed float64s, which inverts that ordering for every
// single-digit minor: as floats 2.9 > 2.45 > 2.21, so a client negotiating 2.3 through 2.9
// cleared both gates and was handed behaviour it had not asked for. Granting more than the
// client negotiated is the one thing these gates exist to prevent, so comparison goes
// through microversion.atLeast and nothing here parses a version as a number again.
// Recorded as finding A6 in the audit.
//
// TWO DELIBERATE DIVERGENCES FROM UPSTREAM live here, both recorded in the constitution's
// Known Divergences register:
//
//   - A request with NO version header resolves to the advertised MAXIMUM. Upstream assumes
//     its minimum. Adopting upstream's default would strip behaviour from every existing
//     caller in a single release, which FR-040 forbids — so the consequence is accepted
//     instead: a gate only ever affects a client that negotiates EXPLICITLY (register
//     entry 2).
//   - An unparseable header is treated as no negotiation rather than refused. Upstream
//     answers 400. This was SPEC-025's decision, recorded as A-006 below (register entry 1).
//
// An out-of-RANGE version, by contrast, IS refused — 406, as upstream refuses it — because
// a client asking for behaviour the deployment does not offer needs to be told to negotiate
// down. Out of range and unparseable are different cases, and upstream answers them
// differently too.

// microversion is a compute microversion: a major and a minor, both integers.
type microversion struct {
	major, minor int
}

// atLeast reports whether v is the given version or a later one.
func (v microversion) atLeast(o microversion) bool {
	return v.major > o.major || (v.major == o.major && v.minor >= o.minor)
}

// String renders the version the way clients spell it.
func (v microversion) String() string {
	return strconv.Itoa(v.major) + "." + strconv.Itoa(v.minor)
}

// parseMicroversion parses "2.21" into its two integers. It reports failure rather than a
// zero version, because an unparseable header means "did not negotiate" here (A-006) and a
// caller that cannot tell the two apart would silently apply the minimum.
func parseMicroversion(s string) (microversion, bool) {
	majorStr, minorStr, found := strings.Cut(s, ".")
	if !found {
		return microversion{}, false
	}
	major, majErr := strconv.Atoi(majorStr)
	minor, minErr := strconv.Atoi(minorStr)
	if majErr != nil || minErr != nil || major < 0 || minor < 0 {
		return microversion{}, false
	}
	return microversion{major, minor}, true
}

// microversionDeletedInstanceActions is the version from which upstream exposes action
// records for deleted instances.
var microversionDeletedInstanceActions = microversion{2, 21}

// microversionCreateImageID is the version from which upstream's createImage /
// createBackup action returns the new image's id in the response body. Before it, the
// image is identified by the Location header alone.
var microversionCreateImageID = microversion{2, 45}

// negotiatedVersionString returns the bare version the caller asked for, or "" if it did
// not negotiate.
//
// One reader for four callers. The range check, the two gates and the applied-version
// report must agree on what the client asked for; four copies of the header precedence and
// the "compute 2.21" unwrapping would be four chances to disagree, and a request refused at
// one version but answered at another is the worst of the available failures.
func negotiatedVersionString(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	// "OpenStack-API-Version: compute 2.21" carries the service name; the Nova-specific
	// header carries the bare version. The service-qualified form wins, as upstream says.
	raw := c.GetHeader("OpenStack-API-Version")
	if raw == "" {
		raw = c.GetHeader("X-OpenStack-Nova-API-Version")
	}
	value := strings.TrimSpace(raw)
	if fields := strings.Fields(value); len(fields) == 2 && strings.EqualFold(fields[0], "compute") {
		value = fields[1]
	}
	return value
}

// requestedMicroversion returns the compute microversion the caller asked for.
//
// An absent or unparseable header yields the advertised current version: a client that
// does not negotiate gets the product's full behaviour, which is what every existing
// caller already receives. "latest" is upstream's spelling for the same request.
func requestedMicroversion(c *gin.Context) microversion {
	current, ok := parseMicroversion(novaCurrentVersion)
	if !ok {
		current = microversionDeletedInstanceActions
	}

	value := negotiatedVersionString(c)
	if value == "" || strings.EqualFold(value, "latest") {
		return current
	}

	v, ok := parseMicroversion(value)
	if !ok {
		// An unparseable version is not a refusal here. Upstream answers 400, but this
		// gate exists to avoid granting MORE than a client negotiated — treating
		// nonsense as "no negotiation" keeps that property without inventing a new
		// rejection on a surface that never had one (A-006).
		return current
	}
	return v
}

// wantsDeletedInstanceActions reports whether the caller negotiated a version that
// exposes a deleted server's action records.
func wantsDeletedInstanceActions(c *gin.Context) bool {
	return requestedMicroversion(c).atLeast(microversionDeletedInstanceActions)
}

// wantsCreateImageIDInBody reports whether the caller negotiated a version that receives
// the image_id body.
//
// The comment at that handler claimed this gate existed for as long as the handler has —
// "Microversion 2.45+ returns image_id in response body" — while returning the body
// unconditionally. The audit's first finding was that the code already documented a gate
// it did not have.
func wantsCreateImageIDInBody(c *gin.Context) bool {
	return requestedMicroversion(c).atLeast(microversionCreateImageID)
}

// negotiatedVersionOutOfRange reports whether the caller asked for a version this
// deployment does not offer, and the message to refuse it with.
//
// Only a header that PARSES can be out of range: requestedMicroversion maps an unparseable
// one to the current version (A-006), so it can never land here. That is what keeps the two
// divergences independent — tolerating nonsense does not weaken the range check.
func negotiatedVersionOutOfRange(c *gin.Context) (string, bool) {
	value := negotiatedVersionString(c)
	if value == "" || strings.EqualFold(value, "latest") {
		return "", false
	}
	asked, ok := parseMicroversion(value)
	if !ok {
		return "", false // unparseable: tolerated, not refused (A-006)
	}
	minV, minOK := parseMicroversion(novaMinVersion)
	maxV, maxOK := parseMicroversion(novaCurrentVersion)
	if !minOK || !maxOK {
		return "", false
	}
	// Out of range means below the minimum or above the maximum. Both are expressed with
	// atLeast so the integer ordering is the only ordering used: "2.100" is a minor of 100
	// and therefore past 2.90, where a numeric read of the string would place it at 2.1.
	if !asked.atLeast(minV) || !maxV.atLeast(asked) {
		return fmt.Sprintf(
			"Version %s is not supported by the API. Minimum is %s and maximum is %s.",
			value, novaMinVersion, novaCurrentVersion), true
	}
	return "", false
}

// MicroversionMiddleware refuses an out-of-range negotiated version and reports the version
// actually applied on every response (FR-038, FR-041).
//
// A middleware rather than a per-handler call: the header must appear on EVERY compute
// response, and 24 handlers each remembering to set it is 24 chances to forget. It also
// puts the refusal ahead of every handler, so no work is done for a request that cannot be
// answered at the version it asked for.
func MicroversionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if msg, out := negotiatedVersionOutOfRange(c); out {
			c.Header("OpenStack-API-Minimum-Version", "compute "+novaMinVersion)
			c.Header("OpenStack-API-Version", "compute "+novaCurrentVersion)
			common.SendError(c, common.NewNotAcceptableError(msg))
			c.Abort()
			return
		}
		c.Header("OpenStack-API-Version", "compute "+appliedMicroversion(c))
		c.Header("OpenStack-API-Minimum-Version", "compute "+novaMinVersion)
		c.Next()
	}
}

// appliedMicroversion returns the version to report, echoed from the request rather than
// re-rendered from the parsed value.
//
// Echoing is exact: a client that spelled "2.09" is told "2.09", and one that spelled
// "2.90" is told "2.90". Re-rendering would also be correct now that the parse keeps the
// minor as an integer — it was NOT correct while this file parsed floats, because
// novaCurrentVersion "2.90" became 2.9, itself a real upstream microversion, so a client
// asking for 2.90 was told 2.9 had applied. That was the reporting half of audit finding
// A6; the comparison half is fixed above. Echoing survives both.
func appliedMicroversion(c *gin.Context) string {
	value := negotiatedVersionString(c)
	// No negotiation, `latest`, or a header that does not parse: the maximum applies
	// (FR-040a, A-006), and it is reported as the string the deployment advertises.
	if value == "" || strings.EqualFold(value, "latest") {
		return novaCurrentVersion
	}
	if _, ok := parseMicroversion(value); !ok {
		return novaCurrentVersion
	}
	return value
}
