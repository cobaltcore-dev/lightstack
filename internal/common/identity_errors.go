package common

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Keystone's error envelope differs from every other service's, exactly as
// Neutron's does (see neutron_errors.go): upstream renders
//
//	{"error": {"code": 403, "message": "...", "title": "Forbidden"}}
//
// while this codebase's default renders the compute shape
// ({"forbidden": {"message": ...}}). The compute shape is fine for the SDK —
// openstacksdk's raise_from_response walks the body's values for a "message"
// key — but the identity client is keystoneauth1, whose from_response parses
// ONLY the {"error": {...}} (or API-SIG {"errors": [...]}) shape. Every other
// body degrades to "Unrecognized schema in response body.", which throws away
// the reason: 'domain delete <enabled>' told the operator nothing about
// disabling it first (SPEC-028).
//
// Marked once per engine by the identity service, so all handler sites and
// every middleware path render Keystone's shape without touching any of them.

// identityEnvelopeKey marks a request whose errors render in Keystone's envelope.
const identityEnvelopeKey = "lightstack.error_envelope.identity"

// MarkIdentityErrors flags the request so SendError renders Keystone's envelope
// instead of the default.
func MarkIdentityErrors(c *gin.Context) {
	c.Set(identityEnvelopeKey, true)
}

// usesIdentityEnvelope reports whether this request was marked. A nil context
// is treated as unmarked so a handler invoked outside a request (tests,
// background work) keeps the default rendering.
func usesIdentityEnvelope(c *gin.Context) bool {
	if c == nil {
		return false
	}
	marked, ok := c.Get(identityEnvelopeKey)
	if !ok {
		return false
	}
	on, _ := marked.(bool)
	return on
}

// IdentityJSON renders the error in Keystone's envelope. `title` is the HTTP
// reason phrase, as upstream's is ("Forbidden", "Not Found", …); `details`
// is folded into the message — upstream has no details field, and the message
// is the only thing keystoneauth shows the user.
func (e *OpenStackError) IdentityJSON() gin.H {
	message := e.Message
	if e.Details != "" {
		message = fmt.Sprintf("%s %s", message, e.Details)
	}
	return gin.H{
		"error": gin.H{
			"code":    e.StatusCode,
			"message": message,
			"title":   http.StatusText(e.StatusCode),
		},
	}
}
