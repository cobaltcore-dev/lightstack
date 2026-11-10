package nova

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/console"
	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// Console handlers (SPEC-021 / cloud-storage#534, finding F1).
//
// Before this feature these returned a hardcoded http://localhost:6080 URL with
// a random token nothing recorded and a port derived from a hash of the instance
// id. Every part of that was fiction. Now:
//
//   - the console socket is read back from libvirt on every request (never
//     cached: the guest run that serves it changes on every start);
//   - the token is persisted and projected for the proxy, so it can be resolved
//     and revoked;
//   - the URL points at an operator-configured origin;
//   - and anything we cannot actually serve is refused instead of dressed up as
//     a URL that leads nowhere.
//
// The client page and ?path= query are appended here rather than configured, so
// they cannot drift from the deployed noVNC/websockify version.
//
// Every console surface resolves its target through resolveServerOr404, the same
// owner-or-administrator rule as every other server operation (SPEC-023 FR-001,
// FR-004): an administrator may open any project's console or read its boot log,
// everyone else only their own, and "absent" and "someone else's" are both 404.
// The console used to carry its own copy of that rule; it is no longer a special
// case. The RESOLVED id — not the raw path parameter, which may be a name — is
// what the token, the hypervisor readback and the serial log are keyed on.
const (
	consoleClientPage  = "/vnc_lite.html"
	consoleTypeNoVNC   = "novnc"
	consoleProtocolVNC = "vnc"
)

// GetConsoleRequest represents a console access request
type GetConsoleRequest struct {
	RemoteConsole struct {
		Protocol string `json:"protocol" binding:"required"` // "vnc" or "serial"
		Type     string `json:"type" binding:"required"`     // "novnc", "xvpvnc", "serial"
	} `json:"remote_console"`
}

// GetRemoteConsole handles POST /servers/{id}/remote-consoles (microversion 2.6+).
func (svc *Service) GetRemoteConsole(c *gin.Context) {
	var req GetConsoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	// Upstream picks the console by protocol and only then checks the type against
	// it, so a novnc type under another protocol is no VNC console there; answering
	// one here would label a VNC URL with a protocol it does not speak.
	if req.RemoteConsole.Protocol != consoleProtocolVNC {
		common.SendError(c, common.NewBadRequestError(fmt.Sprintf(
			"unsupported console protocol %q: this deployment serves only %q (type %q)",
			req.RemoteConsole.Protocol, consoleProtocolVNC, consoleTypeNoVNC)))
		return
	}

	if req.RemoteConsole.Type != consoleTypeNoVNC {
		// Honest refusal rather than a plausible URL. Lightstack serves one console type;
		// returning 200 with an unreachable URL for the others cost real debugging
		// time during the SPEC-018 verification pass.
		common.SendError(c, common.NewBadRequestError(unsupportedConsoleTypeMsg(req.RemoteConsole.Type)))
		return
	}

	consoleURL, ok := svc.issueConsoleURL(c)
	if !ok {
		return // issueConsoleURL already sent the error
	}

	c.JSON(http.StatusOK, gin.H{
		"remote_console": gin.H{
			"protocol": req.RemoteConsole.Protocol,
			"type":     req.RemoteConsole.Type,
			"url":      consoleURL,
		},
	})
}

// GetVNCConsole handles the legacy os-getVNCConsole body form.
func (svc *Service) GetVNCConsole(c *gin.Context) {
	var req struct {
		GetVNCConsole struct {
			Type string `json:"type"` // "novnc" or "xvpvnc"
		} `json:"os-getVNCConsole"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.SendError(c, common.NewBadRequestError("invalid request body"))
		return
	}

	svc.getVNCConsoleResponse(c, req.GetVNCConsole.Type)
}

// GetVNCConsoleAction handles os-getVNCConsole dispatched from the action API.
func (svc *Service) GetVNCConsoleAction(c *gin.Context, vncConsole any) {
	consoleMap, ok := vncConsole.(map[string]any)
	if !ok {
		common.SendError(c, common.NewBadRequestError("invalid console request"))
		return
	}

	consoleType, ok := consoleMap["type"].(string)
	if !ok {
		consoleType = consoleTypeNoVNC
	}

	svc.getVNCConsoleResponse(c, consoleType)
}

// getVNCConsoleResponse is the shared legacy path. It deliberately routes
// through the same issuance as the modern endpoint (FR-005): two code paths
// returning two different URLs for one instance is how consoles rot.
func (svc *Service) getVNCConsoleResponse(c *gin.Context, consoleType string) {
	if consoleType != consoleTypeNoVNC {
		common.SendError(c, common.NewBadRequestError(unsupportedConsoleTypeMsg(consoleType)))
		return
	}

	consoleURL, ok := svc.issueConsoleURL(c)
	if !ok {
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"console": gin.H{
			"type": consoleType,
			"url":  consoleURL,
		},
	})
}

// issueConsoleURL performs the whole issuance: authorize, read the live console
// endpoint from libvirt, persist a token, and build the URL. It writes the error
// response itself and reports success via the bool, so each caller stays a thin
// wrapper around its own response shape.
func (svc *Service) issueConsoleURL(c *gin.Context) (string, bool) {
	ctx := c.Request.Context()

	// Owner-or-administrator, resolved once and shared with every other server
	// operation (SPEC-023 FR-001): without the override the Admin panel's Console
	// tab is broken for every instance the administrator does not own. The owning
	// project — not the caller's — is what the token is issued against, so an
	// administrator's console lands in the affected tenant's records (FR-007).
	instanceID, ownerProject, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return "", false
	}

	if svc.consoleStore == nil {
		// No store means no revocable token, and an unrevocable console is worse
		// than none at all.
		common.SendError(c, common.NewInternalServerError("console token store is not configured"))
		return "", false
	}

	if !svc.consoleEnabled && svc.libvirtMode != "stub" {
		// No console proxy is deployed — multi-node, where v1 has none, or
		// consoleProxy.enabled=false — so any URL would lead nowhere. This is
		// upstream's answer when VNC is disabled (ConsoleTypeUnavailable → 400).
		// Stub mode keeps its own 501 below: "there is no guest" is the more
		// specific reason.
		common.SendError(c, common.NewBadRequestError(
			"Unavailable console type novnc: this deployment runs no console proxy"))
		return "", false
	}

	socket, domainID, err := svc.consoleSocket(c, instanceID)
	if err != nil {
		sendConsoleEndpointError(c, instanceID, err)
		return "", false
	}

	token, err := svc.consoleStore.Issue(ctx, instanceID, ownerProject, socket, domainID)
	if err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("console: issuing token failed")
		common.SendError(c, common.NewInternalServerError("failed to issue console token"))
		return "", false
	}

	return buildConsoleURL(svc.consoleProxyBaseURL, c, token), true
}

// revokeConsoleTokens drops every console token of an instance, for use by any
// operation that can move or free its console endpoint (SPEC-021 FR-014).
//
// It fails closed: on error it sends a 500 and returns false, and the caller must
// return without acting. The socket path is the instance's own, so a surviving token
// can never reach another guest; this keeps an action from reporting success while a
// link it should have ended still resolves (the path survives every restart). Every
// caller revokes before touching libvirt or the database, so refusing leaves the
// instance as it was.
func (svc *Service) revokeConsoleTokens(c *gin.Context, instanceID, reason string) bool {
	if svc.consoleStore == nil || instanceID == "" {
		return true
	}
	if err := svc.consoleStore.RevokeByInstance(c.Request.Context(), instanceID); err != nil {
		log.Error().Err(err).
			Str("instance_id", instanceID).
			Str("reason", reason).
			Msg("console: revoking tokens failed; refusing the action")
		common.SendError(c, common.NewInternalServerError(
			"failed to revoke console tokens; the action was not performed"))
		return false
	}
	return true
}

// errNoHypervisor is real mode without a hypervisor: InitHypervisor failed at startup
// (libvirt unreachable) and Lightstack carried on. An outage, not stub mode.
var errNoHypervisor = errors.New("hypervisor not initialized: libvirt was unreachable when Lightstack started")

// consoleSocket reads the guest's live console socket from the hypervisor, with the
// domain ID of the run it serves.
func (svc *Service) consoleSocket(c *gin.Context, instanceID string) (string, int32, error) {
	if svc.libvirtMode == "stub" {
		return "", 0, hypervisor.ErrConsoleUnavailable
	}
	if svc.vmManager == nil {
		return "", 0, errNoHypervisor
	}
	return svc.vmManager.GetConsoleSocket(c.Request.Context(), instanceID)
}

// CurrentConsoleSocket reports the console socket an instance's guest serves right
// now and the domain ID of its run, for the console-token reconciler
// (console.CurrentSocket): "" when it serves none, an error when libvirt cannot say.
func (svc *Service) CurrentConsoleSocket(ctx context.Context, instanceID string) (string, int32, error) {
	if svc.vmManager == nil {
		return "", 0, errNoHypervisor
	}
	return socketForSweep(svc.vmManager.GetConsoleSocket(ctx, instanceID))
}

// socketForSweep sorts a readback failure into the two answers the sweep acts on. A
// guest that is not running, has no console, or whose console the proxy cannot reach
// (a TCP listener, or a socket outside the console socket directory) serves no
// socket, so its tokens are revoked. Anything else — libvirtd down, a timeout — is
// not an answer, and the tokens stay for the next sweep.
func socketForSweep(socket string, domainID int32, err error) (string, int32, error) {
	switch {
	case err == nil:
		return socket, domainID, nil
	case errors.Is(err, hypervisor.ErrConsoleNotReady),
		errors.Is(err, hypervisor.ErrNoGraphicalConsole),
		errors.Is(err, hypervisor.ErrConsoleNotSocket),
		errors.Is(err, hypervisor.ErrConsoleLegacySocket):
		return "", 0, nil
	default:
		return "", 0, err
	}
}

// sendConsoleEndpointError maps a readback failure onto the status code that
// describes it, so a caller never has to guess whether the problem is their
// request, the instance's state, or the deployment.
func sendConsoleEndpointError(c *gin.Context, instanceID string, err error) {
	switch {
	case errors.Is(err, hypervisor.ErrConsoleUnavailable):
		// Stub mode: the deployment has no hypervisor, so no console can exist.
		// 501 rather than a 4xx — the caller did nothing wrong.
		c.JSON(http.StatusNotImplemented, gin.H{
			"notImplemented": gin.H{
				"code":    http.StatusNotImplemented,
				"message": "console unavailable: nova is running in stub mode, there is no guest to display",
			},
		})
	case errors.Is(err, hypervisor.ErrConsoleNotReady):
		common.SendError(c, common.NewConflictError(
			"console not available: the instance is not running"))
	case errors.Is(err, hypervisor.ErrNoGraphicalConsole):
		common.SendError(c, common.NewConflictError(
			"console not available: the instance has no graphical console device"))
	case errors.Is(err, hypervisor.ErrConsoleNotSocket):
		// A TCP console — a domain defined before consoles moved to unix sockets.
		// Its port can be handed to another guest while a token still names it,
		// and a wildcard one is reachable without any token. Refusing is the safe
		// direction; docs/UPGRADING.md says how to fix the domain.
		log.Error().Err(err).Str("instance_id", instanceID).
			Msg("console: guest console is not a unix socket; refusing to proxy")
		common.SendError(c, common.NewInternalServerError("console misconfigured on this host"))
	case errors.Is(err, hypervisor.ErrConsoleLegacySocket):
		// A domain defined before the console sockets moved: its socket is in libvirt's
		// per-domain directory, which the proxy deliberately cannot see (FR-017).
		// docs/UPGRADING.md says how to fix the domain.
		log.Error().Err(err).Str("instance_id", instanceID).
			Msg("console: guest console socket is outside the console socket directory; refusing to proxy")
		common.SendError(c, common.NewInternalServerError("console misconfigured on this host"))
	default:
		log.Error().Err(err).Str("instance_id", instanceID).Msg("console: endpoint readback failed")
		common.SendError(c, common.NewInternalServerError("failed to read the instance console endpoint"))
	}
}

// buildConsoleURL assembles <origin>/vnc_lite.html?path=%3Ftoken%3D<token>.
//
// The ?path= form is upstream's (nova returns the same shape): the noVNC client
// appends it to its WebSocket URL, so the browser opens ws://<origin>/?token=…
// against websockify, which resolves the token to a guest.
//
// The inner "?token=" is percent-encoded because it is a VALUE inside the outer
// ?path= parameter: left literal, its own ? and = would terminate that value and
// the client would receive an empty path. The token itself is 32 hex characters
// and needs no escaping — only the two delimiters around it do.
func buildConsoleURL(configuredOrigin string, c *gin.Context, token string) string {
	origin := strings.TrimRight(configuredOrigin, "/")
	if origin == "" {
		// Never a hardcoded localhost: fall back to the same endpoint settings
		// that govern the rest of Lightstack's advertised URLs (FR-021).
		origin = deriveConsoleOrigin(c)
	}
	return fmt.Sprintf("%s%s?path=%s", origin, consoleClientPage, url.QueryEscape("?token="+token))
}

// deriveConsoleOrigin returns the proxy origin for a request, with the API port
// replaced by the console port.
//
// The port argument is what does that work: common.BaseURL applies it in place of
// whatever port the source host carried, in both its request and its
// LIGHTSTACK_ENDPOINT_HOST branch (SPEC-025 FR-004). Without it the origin would name
// Nova's own 8774 — a link the browser resolves to the API, not the proxy, which is
// the last surviving form of the hardcoded-URL bug SPEC-021 exists to fix.
//
// The host follows the same precedence as every other advertised URL: the one the
// client addressed wins, and LIGHTSTACK_ENDPOINT_HOST is the fallback (SPEC-025 FR-002,
// FR-003).
//
// Only helps when the browser can reach the API host directly (the single-node
// case). Behind an ingress whose hostname differs from the proxy's, set
// nova.console_proxy_base_url explicitly — there is nothing in the request to
// derive that from.
//
// The scheme is always http, whatever the API speaks: the origin describes the proxy
// the chart and scripts ship, which serves plain HTTP. An https console needs
// something in front of the proxy to terminate TLS, and so an explicit origin.
func deriveConsoleOrigin(c *gin.Context) string {
	base := common.BaseURL(c, defaultConsoleProxyPort)
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	u.Scheme = "http"
	return u.String()
}

// defaultConsoleProxyPort is the port the chart publishes for the console proxy.
// Only used when no origin is configured.
const defaultConsoleProxyPort = 6080

func unsupportedConsoleTypeMsg(consoleType string) string {
	if consoleType == "" {
		consoleType = "(none)"
	}
	return fmt.Sprintf(
		"unsupported console type %q: this deployment serves only %q; use `openstack console log show` for serial output",
		consoleType, consoleTypeNoVNC)
}

// ShowConsoleAuthToken handles GET /os-console-auth-tokens/{token} (microversion
// 2.31+).
//
// This exists for API compatibility only. The proxy resolves tokens from the
// projection directory without asking us, so nothing about the console depends
// on this endpoint working — but `openstack` and third-party tooling expect it.
func (svc *Service) ShowConsoleAuthToken(c *gin.Context) {
	if !common.RequireAdmin(c, "os-console-auth-tokens:show") {
		return
	}
	if svc.consoleStore == nil {
		common.SendError(c, common.NewNotFoundError("console token"))
		return
	}

	rec, err := svc.consoleStore.Resolve(c.Request.Context(), c.Param("token"))
	if errors.Is(err, console.ErrTokenNotFound) {
		// Unknown, expired and instance-gone are deliberately indistinguishable.
		common.SendError(c, common.NewNotFoundError("console token"))
		return
	}
	if err != nil {
		// A failed lookup is not a missing token. Upstream answers 404 here as well,
		// but only because its tokens live in per-cell databases, where a cell that
		// does not answer looks like one without the token. Lightstack has one
		// database, so this is that database failing — and a 404 would tell the
		// caller a valid link does not exist. Independent of the token, so no oracle.
		log.Error().Err(err).Msg("console: token lookup failed")
		common.SendError(c, common.NewInternalServerError("failed to look up console token"))
		return
	}

	// A unix socket has neither host nor port. Both stay in the body, as null,
	// because clients read upstream's shape; the socket is the internal access
	// path, the field upstream uses for a target that is not host:port.
	c.JSON(http.StatusOK, gin.H{
		"console": gin.H{
			"instance_uuid":        rec.InstanceID,
			"host":                 nil,
			"port":                 nil,
			"internal_access_path": rec.Socket,
		},
	})
}

// GetConsoleOutputAction handles console output requests (os-getConsoleOutput action).
//
// The output itself is unchanged by SPEC-021 and deliberately so: the serial log is
// the documented fallback that worked throughout the F1 outage, and breaking it would
// be a regression (FR-026). Its AUTHORIZATION did change, to match the interactive
// console: both are admin_or_owner, so an admin reading one tenant's framebuffer and
// 404ing on the same instance's boot log would be incoherent.
func (svc *Service) GetConsoleOutputAction(c *gin.Context, consoleOutput any) {
	// The same resolution issueConsoleURL uses (SPEC-021 FR-013, SPEC-023 FR-001):
	// an administrator who may open a tenant's framebuffer must not 404 on that
	// same instance's boot log.
	instanceID, _, ok := resolveServerOr404(c, svc.pool, c.Param("id"))
	if !ok {
		return
	}

	// Parse length parameter
	length := 0
	if consoleMap, ok := consoleOutput.(map[string]any); ok {
		if lengthVal, ok := consoleMap["length"].(float64); ok {
			length = int(lengthVal)
		}
	}

	// In real mode, return the VM's captured serial console log (SPEC-009 WS-0).
	if svc.libvirtMode == "real" {
		// The resolved id, not the path parameter: the caller may have addressed
		// the instance by name, and the log is stored under the UUID.
		if out, rerr := hypervisor.ReadConsoleLog(instanceID, length); rerr == nil {
			c.JSON(http.StatusOK, gin.H{"output": out})
			return
		}
		// No console captured yet (VM still booting) — fall through to a placeholder.
	}

	// Stub mode (or no console captured yet): synthetic console output.
	output := fmt.Sprintf("Console output for instance %s\n", instanceID)
	output += "Booting from Hard Disk...\n"
	output += "Cloud-init v. 22.1-14 running 'init-local' at Wed, 11 Mar 2026 10:00:00 +0000\n"
	output += "Cloud-init v. 22.1-14 running 'init' at Wed, 11 Mar 2026 10:00:05 +0000\n"
	output += "System is ready.\n"

	// Trim to requested length if specified
	if length > 0 && len(output) > length {
		output = output[len(output)-length:]
	}

	c.JSON(http.StatusOK, gin.H{
		"output": output,
	})
}

// GetSerialConsoleAction handles os-getSerialConsole.
//
// Lightstack captures a serial LOG but serves no interactive serial console, so this
// refuses instead of returning a ws:// URL nothing listens on (FR-025).
func (svc *Service) GetSerialConsoleAction(c *gin.Context, _ any) {
	common.SendError(c, common.NewBadRequestError(unsupportedConsoleTypeMsg("serial")))
}

// GetSPICEConsoleAction handles os-getSPICEConsole — not served (FR-025).
func (svc *Service) GetSPICEConsoleAction(c *gin.Context, _ any) {
	common.SendError(c, common.NewBadRequestError(unsupportedConsoleTypeMsg("spice-html5")))
}

// GetRDPConsoleAction handles os-getRDPConsole — not served (FR-025). Upstream
// removed the Hyper-V driver and returns 400 here too.
func (svc *Service) GetRDPConsoleAction(c *gin.Context, _ any) {
	common.SendError(c, common.NewBadRequestError(unsupportedConsoleTypeMsg("rdp-html5")))
}
