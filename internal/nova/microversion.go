package nova

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Requested-microversion handling.
//
// THIS IS THE PRODUCT'S FIRST MICROVERSION GATE, and deliberately the only one.
//
// Lightstack advertises a microversion range and has never read the request header: comments
// elsewhere describe behaviour as gated on a microversion (see the createImage handler's
// note about 2.45) when it is in fact unconditional. That is a fidelity gap, but a
// broad one, and closing it product-wide is a concern of its own.
//
// One gate is needed here because SPEC-025 US7 ADDS a behaviour that upstream gates:
// action records for a deleted server are available "for requests starting with
// microversion 2.21", restricted to the owner or an administrator. Serving them to a
// client that negotiated 2.1 would be a new incompatibility introduced while fixing an
// old one, which Article I does not permit. Generalising microversion negotiation is
// out of scope and flagged for its own ticket.

// microversionDeletedInstanceActions is the version from which upstream exposes action
// records for deleted instances.
const microversionDeletedInstanceActions = 2.21

// requestedMicroversion returns the compute microversion the caller asked for.
//
// An absent or unparseable header yields the advertised current version: a client that
// does not negotiate gets the product's full behaviour, which is what every existing
// caller already receives. "latest" is upstream's spelling for the same request.
//
// Returns a float rather than a (major, minor) pair because compute microversions are
// compared, not decomposed, and 2.x minor numbers do not carry into a second digit
// range where float comparison would misorder them.
func requestedMicroversion(c *gin.Context) float64 {
	current, err := strconv.ParseFloat(novaCurrentVersion, 64)
	if err != nil {
		current = microversionDeletedInstanceActions
	}
	if c == nil || c.Request == nil {
		return current
	}

	raw := c.GetHeader("OpenStack-API-Version")
	if raw == "" {
		raw = c.GetHeader("X-OpenStack-Nova-API-Version")
	}
	if raw == "" {
		return current
	}

	// "OpenStack-API-Version: compute 2.21" carries the service name; the Nova-specific
	// header carries the bare version.
	value := strings.TrimSpace(raw)
	if fields := strings.Fields(value); len(fields) == 2 && strings.EqualFold(fields[0], "compute") {
		value = fields[1]
	}
	if strings.EqualFold(value, "latest") {
		return current
	}

	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
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
	return requestedMicroversion(c) >= microversionDeletedInstanceActions
}
