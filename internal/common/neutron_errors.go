package common

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Neutron's error envelope (SPEC-024 / cloud-storage#546 B10).
//
// Upstream builds it in neutron/api/api_common.py:
//
//	body = serializer.serialize({'NeutronError': get_exception_data(e)})
//	err_data = {'type': e.__class__.__name__, 'message': e, 'detail': ''}
//
// So the wrapper is `NeutronError`, the inner object is exactly type/message/detail, and there is no
// numeric `code` — that field belongs to the compute envelope this package emits by default. The
// `type` is literally the name of the Python exception class upstream raised, which is why it names
// the *condition* ("NetworkNotFound") rather than describing it ("itemNotFound").
//
// Renaming only the wrapper would produce a NeutronError around a compute body: half-correct, and
// worse than leaving it because it looks finished. The whole shape is built here instead.
//
// This is the single place the type vocabulary is written down (FR-007a). New error sites inherit a
// name from the tables below or fall back to the documented generic; nothing gets an invented
// per-site name.

// neutronEnvelopeKey marks a request whose errors render in Neutron's envelope. Set once per engine
// by the network service (see neutron.ErrorEnvelope) rather than per call site, so all ~244 handler
// sites plus every middleware path — auth aborts, panic recovery, NoRoute, NoMethod — are covered
// without touching any of them.
const neutronEnvelopeKey = "lightstack.error_envelope.neutron"

// MarkNeutronErrors flags the request so SendError renders Neutron's envelope instead of the default.
func MarkNeutronErrors(c *gin.Context) {
	c.Set(neutronEnvelopeKey, true)
}

// usesNeutronEnvelope reports whether this request was marked. A nil context is treated as unmarked
// so a handler invoked outside a request (tests, background work) keeps the default rendering.
func usesNeutronEnvelope(c *gin.Context) bool {
	if c == nil {
		return false
	}
	marked, ok := c.Get(neutronEnvelopeKey)
	if !ok {
		return false
	}
	on, _ := marked.(bool)
	return on
}

// neutronNotFoundTypes maps the resource names this codebase passes to NewNotFoundError onto
// upstream's exception class names. Keys are lower-cased; see neutronType.
var neutronNotFoundTypes = map[string]string{
	"network":                 "NetworkNotFound",
	"subnet":                  "SubnetNotFound",
	"port":                    "PortNotFound",
	"router":                  "RouterNotFound",
	"router interface":        "RouterInterfaceNotFound",
	"floating ip":             "FloatingIPNotFound",
	"security group":          "SecurityGroupNotFound",
	"security group rule":     "SecurityGroupRuleNotFound",
	"rule":                    "SecurityGroupRuleNotFound",
	"policy":                  "QosPolicyNotFound",
	"subnet pool":             "SubnetPoolNotFound",
	"address scope":           "AddressScopeNotFound",
	"port forwarding":         "PortForwardingNotFound",
	"metering label":          "MeteringLabelNotFound",
	"metering label rule":     "MeteringLabelRuleNotFound",
	"rbac policy":             "RbacPolicyNotFound",
	"trunk":                   "TrunkNotFound",
	"agent":                   "AgentNotFound",
	"auto-allocated topology": "AutoAllocationFailure",
	"project":                 "ProjectNotFound",
}

// neutronInUseTypes covers the 409s upstream names after the resource. Anything not listed is a
// plain Conflict — upstream has no in-use class for it.
var neutronInUseTypes = map[string]string{
	"network": "NetworkInUse",
	"subnet":  "SubnetInUse",
	"port":    "PortInUse",
}

// neutronCodeTypes maps this package's envelope keys onto upstream class names for every status
// other than 404.
//
// `computeFault` is the row that matters most: it is Lightstack's internal-error key *and* a compute name
// that must never appear on a network response. It becomes NeutronException, upstream's base class —
// the one documented generic (FR-007a). Failures with no upstream counterpart land here: "failed to
// create bridge", "failed to attach TAP to bridge", database errors.
var neutronCodeTypes = map[string]string{
	"badRequest":         "BadRequest",
	"badMethod":          "BadRequest",
	"unauthorized":       "NotAuthorized",
	"forbidden":          "NotAuthorized",
	"conflict":           "Conflict",
	"conflictingRequest": "Conflict",
	"overLimit":          "OverQuota",
	"serviceUnavailable": "ServiceUnavailable",
	"computeFault":       "NeutronException",
	"itemNotFound":       "NotFound",
}

// neutronType derives the upstream condition name for an error.
//
// 404s and 409s are resolved from the resource, because that is what distinguishes an absent network
// from an absent port; everything else is resolved from the envelope key, which already carries the
// semantic. An unrecognized value falls back to the generic rather than inventing a name.
func neutronType(e *OpenStackError) string {
	resource := strings.ToLower(strings.TrimSpace(e.Resource))

	switch e.StatusCode {
	case http.StatusNotFound:
		if t, ok := neutronNotFoundTypes[resource]; ok {
			return t
		}
		return "NotFound"
	case http.StatusConflict:
		if t, ok := neutronInUseTypes[resource]; ok {
			return t
		}
		return "Conflict"
	}

	if t, ok := neutronCodeTypes[e.Code]; ok {
		return t
	}
	return "NeutronException"
}

// NeutronJSON renders the error in Neutron's envelope.
//
// `detail` is always present and usually empty, as upstream leaves it; it carries Details when the
// error has any. There is deliberately no `code` key.
func (e *OpenStackError) NeutronJSON() gin.H {
	return gin.H{
		"NeutronError": gin.H{
			"type":    neutronType(e),
			"message": e.Message,
			"detail":  e.Details,
		},
	}
}
