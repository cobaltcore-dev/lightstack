package nova

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cobaltcore-dev/o3k/internal/common"
	"github.com/cobaltcore-dev/o3k/internal/console"
	"github.com/cobaltcore-dev/o3k/pkg/hypervisor"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// Console handlers (SPEC-021 / cloud-storage#534, finding F1).
//
// Before this feature these returned a hardcoded http://localhost:6080 URL with
// a random token nothing recorded and a port derived from a hash of the instance
// id. Every part of that was fiction. Now:
//
//   - the port is read back from libvirt on every request (never cached: the
//     hypervisor reassigns it on each start);
//   - the token is persisted and projected for the proxy, so it can be resolved
//     and revoked;
//   - the URL points at an operator-configured origin;
//   - and anything we cannot actually serve is refused instead of dressed up as
//     a URL that leads nowhere.
//
// The client page and ?path= query are appended here rather than configured, so
// they cannot drift from the deployed noVNC/websockify version.
const (
	consoleClientPage = "/vnc_lite.html"
	consoleTypeNoVNC  = "novnc"
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

	if req.RemoteConsole.Type != consoleTypeNoVNC {
		// Honest refusal rather than a plausible URL. O3K serves one console type;
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
	instanceID := c.Param("id")

	ownerProject, err := lookupConsoleInstance(c, instanceID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deliberately the same answer for "absent" and "someone else's": a 403
		// here would confirm the instance exists.
		common.SendError(c, common.NewNotFoundError("instance"))
		return "", false
	}
	if err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("console: instance lookup failed")
		common.SendError(c, common.NewInternalServerError("failed to look up instance"))
		return "", false
	}

	if svc.consoleStore == nil {
		// No store means no revocable token, and an unrevocable console is worse
		// than none at all.
		common.SendError(c, common.NewInternalServerError("console token store is not configured"))
		return "", false
	}

	host, port, err := svc.consoleEndpoint(c, instanceID)
	if err != nil {
		sendConsoleEndpointError(c, instanceID, err)
		return "", false
	}

	token, err := svc.consoleStore.Issue(ctx, instanceID, ownerProject, host, port)
	if err != nil {
		log.Error().Err(err).Str("instance_id", instanceID).Msg("console: issuing token failed")
		common.SendError(c, common.NewInternalServerError("failed to issue console token"))
		return "", false
	}

	return buildConsoleURL(svc.consoleProxyBaseURL, c, token), true
}

// lookupConsoleInstance resolves an instance for a console request and returns the
// project that owns it.
//
// One rule, one implementation, shared by BOTH console surfaces — the interactive
// console and the serial log (SPEC-021 FR-013). Upstream's policy for these is
// admin_or_owner: an admin may address any project's instance, everyone else only
// their own. Without the override the Admin panel's Console tab is broken for every
// instance the admin does not own; with it in only one of the two handlers, an admin
// could open a tenant's framebuffer while 404ing on that same instance's boot log.
//
// SPEC-023 FR-001 widened that from "one rule for the two console surfaces" to one
// rule for EVERY server operation, so this now delegates to targetInstance rather
// than carrying its own copy — the console stops being the one place the override
// lives. Behaviour is unchanged for every caller that addresses an instance by id,
// which is all of them: upstream's server_id is a UUID and Horizon sends one.
// Delegating additionally makes a *name* resolvable within the caller's own project,
// a superset of what this accepted before and consistent with the rest of Nova.
//
// Returns pgx.ErrNoRows for both "absent" and "someone else's" so callers answer 404
// either way: a 403 would confirm the instance exists.
//
// The caller's project is read from the request context by the shared helper, so
// this no longer takes one — the parameter it used to carry became dead the
// moment the rule moved into targetInstance.
func lookupConsoleInstance(c *gin.Context, instanceID string) (string, error) {
	_, ownerProject, err := targetInstance(c, instanceID)
	return ownerProject, err
}

// revokeConsoleTokens drops every console token of an instance, for use by any
// operation that can move or free its console endpoint (SPEC-021 FR-014).
//
// Failures are logged, not returned: revocation is a safety measure attached to
// operations that must still succeed. A failure here leaves a token alive until
// the reconciler's next sweep, which is why that sweep exists.
func (svc *Service) revokeConsoleTokens(c *gin.Context, instanceID, reason string) {
	if svc.consoleStore == nil || instanceID == "" {
		return
	}
	if err := svc.consoleStore.RevokeByInstance(c.Request.Context(), instanceID); err != nil {
		log.Error().Err(err).
			Str("instance_id", instanceID).
			Str("reason", reason).
			Msg("console: revoking tokens failed; the reconciler will retry")
	}
}

// consoleEndpoint reads the guest's live console endpoint from the hypervisor.
func (svc *Service) consoleEndpoint(c *gin.Context, instanceID string) (string, int, error) {
	if svc.libvirtMode == "stub" || svc.vmManager == nil {
		return "", 0, hypervisor.ErrConsoleUnavailable
	}
	return svc.vmManager.GetConsoleEndpoint(c.Request.Context(), instanceID)
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
	case errors.Is(err, hypervisor.ErrConsoleNotLoopback):
		// A guest reachable off-host would make the token decorative. Refusing is
		// the safe direction.
		log.Error().Err(err).Str("instance_id", instanceID).
			Msg("console: guest console is not bound to loopback; refusing to proxy")
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
		// that govern the rest of O3K's advertised URLs (FR-021).
		origin = deriveConsoleOrigin(c)
	}
	return fmt.Sprintf("%s%s?path=%s", origin, consoleClientPage, url.QueryEscape("?token="+token))
}

// deriveConsoleOrigin returns the proxy origin for a request, with the API port
// replaced by the console port.
//
// common.BaseURL applies its port argument ONLY in its O3K_ENDPOINT_HOST branch;
// otherwise it returns scheme://c.Request.Host verbatim and drops the port
// (pinned by common.BaseURL's own test: BaseURL(ctx, 0) -> "https://host:443").
// Relying on it alone therefore advertises Nova's own port, 8774, as the console
// origin — a link the browser resolves to the API, not the proxy. That is the last
// surviving form of the hardcoded-URL bug SPEC-021 exists to fix, so the port is
// re-applied here rather than changing BaseURL, which every service's self-links
// share.
//
// Only helps when the browser can reach the API host directly (the single-node
// case). Behind an ingress whose hostname differs from the proxy's, set
// nova.console_proxy_base_url explicitly — there is nothing in the request to
// derive that from.
func deriveConsoleOrigin(c *gin.Context) string {
	base := strings.TrimRight(common.BaseURL(c, defaultConsoleProxyPort), "/")
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return base
	}
	u.Host = fmt.Sprintf("%s:%d", u.Hostname(), defaultConsoleProxyPort)
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
	if svc.consoleStore == nil {
		common.SendError(c, common.NewNotFoundError("console token"))
		return
	}

	rec, err := svc.consoleStore.Resolve(c.Request.Context(), c.Param("token"))
	if err != nil {
		// Unknown, expired and instance-gone are deliberately indistinguishable.
		if !errors.Is(err, console.ErrTokenNotFound) {
			log.Error().Err(err).Msg("console: token lookup failed")
		}
		common.SendError(c, common.NewNotFoundError("console token"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"console": gin.H{
			"instance_uuid": rec.InstanceID,
			"host":          rec.Host,
			"port":          rec.Port,
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
	instanceID := c.Param("id")

	// Verify the instance exists and the caller may see it, with the same admin
	// override issueConsoleURL uses (FR-013).
	_, err := lookupConsoleInstance(c, instanceID)

	if errors.Is(err, pgx.ErrNoRows) {
		// Absent and someone else's are the same answer: a 403 would confirm the
		// instance exists.
		common.SendError(c, common.NewNotFoundError("instance"))
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
		// instanceID, not a re-scanned column: the lookup matched on id = $1, so they
		// are the same value by construction.
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
// O3K captures a serial LOG but serves no interactive serial console, so this
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
