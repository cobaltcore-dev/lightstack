package neutron

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/compute"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog/log"
)

// The node-to-node call (SPEC-033 FR-034; research R27; contracts/node-apply.md): a node asks the
// node that holds a unit — the network node for routers and DHCP, a port's node for its chains — to
// converge it, over HTTP on that node's tunnel address and Neutron port, authenticated by a token the
// nodes sign with the deployment's shared secret, bounded at one second end to end. The call names the
// unit, never its state, so it is idempotent; the caller never retries, and any failure is a failed
// step for the request that called.

const (
	nodeTokenHeader   = "X-Lightstack-Node-Token"
	nodeCallPath      = "/internal/v1/converge"
	nodeCallTimeout   = time.Second            // the caller's bound, end to end
	nodeApplyDeadline = 900 * time.Millisecond // the target's: lock wait and apply, from receipt
	nodeTokenLifetime = 10 * time.Second       // a token's longest life
	nodeCallBodyLimit = 1 << 20                // a body names ids only
)

// NodeOptions place a node in its deployment (R26, R27).
type NodeOptions struct {
	// Name is this node's name, the one the registry and a port's binding use (common.NodeName).
	Name string
	// NetworkNode names the node that holds every router and DHCP unit: this one when empty.
	NetworkNode string
	// Registry finds the other nodes. Nil on a single node, which reaches no other.
	Registry *compute.NodeRegistry
	// Port is the Neutron port, the same on every node.
	Port int
	// Secret signs and checks node tokens: keystone.jwt_secret, the same on every node.
	Secret []byte
}

// SetNodes places the service in its deployment. Without it the service is a single node, its own
// network node, reaching no other.
func (svc *Service) SetNodes(o NodeOptions) {
	if o.NetworkNode == "" {
		o.NetworkNode = o.Name
	}
	svc.nodes = o
}

// networkNodeName is the node that holds every router and DHCP unit.
func (svc *Service) networkNodeName() string { return svc.nodes.NetworkNode }

// isNetworkNode reports whether this node holds the routers and DHCP.
func (svc *Service) isNetworkNode() bool { return svc.nodes.NetworkNode == svc.nodes.Name }

// nodeAddress is where a node is reached: its registered tunnel address, on the Neutron port. A node
// the registry does not hold as active — never registered, stopped, or silent for two heartbeats —
// cannot be reached, and the step fails (FR-034).
func (svc *Service) nodeAddress(ctx context.Context, node string) (string, error) {
	if svc.nodes.Registry == nil {
		return "", fmt.Errorf("node %s cannot be reached: this node keeps no registry of others", node)
	}
	nodes, err := svc.nodes.Registry.ListActiveNodes(ctx)
	if err != nil {
		return "", fmt.Errorf("find node %s: %w", node, err)
	}
	for _, n := range nodes {
		if n.Hostname == node {
			return "http://" + net.JoinHostPort(n.TunnelIP, strconv.Itoa(svc.nodes.Port)), nil
		}
	}
	return "", fmt.Errorf("node %s is not an active node", node)
}

// nodeClaims are a node token's claims: the audience no user token carries, the node the call is
// for, and the SHA-256 of the body it was signed for.
type nodeClaims struct {
	Node       string `json:"node"`
	BodySHA256 string `json:"body_sha256"`
	jwt.RegisteredClaims
}

func bodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// mintNodeToken signs a token for one call: to node, with body, valid for ten seconds from now. It is
// signed as Keystone signs a user's token (internal/keystone/auth.go), under another audience.
func mintNodeToken(secret []byte, node string, body []byte, now time.Time) (string, error) {
	claims := nodeClaims{
		Node:       node,
		BodySHA256: bodyDigest(body),
		RegisteredClaims: jwt.RegisteredClaims{
			Audience:  jwt.ClaimStrings{common.NodeTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(nodeTokenLifetime)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// nodeTokenSkew is how far the nodes' clocks may disagree on a node token's issue and expiry times.
const nodeTokenSkew = 2 * time.Second

// errNodeToken: the call carries no node token for this node and this body. The call answers 401.
var errNodeToken = errors.New("no node token for this node and body")

// checkNodeToken accepts a token signed with secret, with the node audience, issued for node and for
// body, unexpired, not issued in the future — within two seconds of skew between the nodes' clocks, so
// a token a fast clock signed cannot be replayed until that clock's time comes (SPEC-033 T211) —
// and living no longer than ten seconds.
func checkNodeToken(secret []byte, token, node string, body []byte) error {
	if token == "" {
		return fmt.Errorf("%w: none given", errNodeToken)
	}
	if len(secret) == 0 {
		return fmt.Errorf("%w: this node has no secret to check it with", errNodeToken)
	}
	var claims nodeClaims
	if _, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithAudience(common.NodeTokenAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(nodeTokenSkew),
	); err != nil {
		return fmt.Errorf("%w: %w", errNodeToken, err)
	}
	if claims.IssuedAt == nil || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > nodeTokenLifetime {
		return fmt.Errorf("%w: it lives longer than %s", errNodeToken, nodeTokenLifetime)
	}
	if claims.Node != node {
		return fmt.Errorf("%w: it is for node %q", errNodeToken, claims.Node)
	}
	if !hmac.Equal([]byte(claims.BodySHA256), []byte(bodyDigest(body))) {
		return fmt.Errorf("%w: it was signed for another body", errNodeToken)
	}
	return nil
}

// ConvergeHandler answers the node call, POST /internal/v1/converge: 204 when the unit is converged,
// 401 without a node token for this node and body, 400 for a body that names no unit, 409 for a unit
// this node does not hold, 500 when the unit could not be converged within 900 ms of receipt. The node
// token is the route's only authentication; a user token is refused.
func (svc *Service) ConvergeHandler(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), nodeApplyDeadline)
	defer cancel()
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, nodeCallBodyLimit+1))
	if err != nil || len(body) > nodeCallBodyLimit {
		common.SendError(c, common.NewBadRequestError("unreadable node call"))
		return
	}
	if err := checkNodeToken(svc.nodes.Secret, c.GetHeader(nodeTokenHeader), svc.nodes.Name, body); err != nil {
		log.Warn().Err(err).Str("remote", c.ClientIP()).Msg("node call refused")
		common.SendError(c, common.NewUnauthorizedError("node token refused"))
		return
	}
	var req unitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		common.SendError(c, common.NewBadRequestError("the node call names no unit: "+err.Error()))
		return
	}
	switch err := svc.convergeUnit(ctx, req); {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, errBadUnit):
		common.SendError(c, common.NewBadRequestError(err.Error()))
	case errors.Is(err, errUnitNotHeld):
		common.SendError(c, common.NewConflictError(err.Error()))
	default:
		log.Error().Err(err).Str("unit", string(req.Unit)).Strs("ids", req.IDs).Msg("node call: the unit could not be converged")
		common.SendError(c, common.NewInternalServerError("the unit could not be converged"))
	}
}

// httpNodeCaller is the real nodeCaller: a unit of this node by a function call, another node's over
// HTTP at the address the registry holds for it.
type httpNodeCaller struct {
	svc    *Service
	client *http.Client
}

// newHTTPNodeCaller makes the node-to-node caller. Its transport names no proxy: the call goes straight to
// the node's tunnel address, never through an egress proxy the environment names (SPEC-033 T275).
func newHTTPNodeCaller(svc *Service) *httpNodeCaller {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &httpNodeCaller{svc: svc, client: &http.Client{Timeout: nodeCallTimeout, Transport: transport}}
}

func (h *httpNodeCaller) converge(ctx context.Context, node string, req unitRequest) error {
	if node == h.svc.nodes.Name {
		ctx, cancel := context.WithTimeout(ctx, nodeApplyDeadline)
		defer cancel()
		return h.svc.convergeUnit(ctx, req)
	}
	// One second end to end: finding the node, the call and the answer.
	ctx, cancel := context.WithTimeout(ctx, nodeCallTimeout)
	defer cancel()
	address, err := h.svc.nodeAddress(ctx, node)
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("the call to %s: %w", node, err)
	}
	token, err := mintNodeToken(h.svc.nodes.Secret, node, body, time.Now())
	if err != nil {
		return fmt.Errorf("sign the call to %s: %w", node, err)
	}
	call, err := http.NewRequestWithContext(ctx, http.MethodPost, address+nodeCallPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("the call to %s: %w", node, err)
	}
	call.Header.Set("Content-Type", "application/json")
	call.Header.Set(nodeTokenHeader, token)
	resp, err := h.client.Do(call)
	if err != nil {
		return fmt.Errorf("converge %s %v on %s: %w", req.Unit, req.IDs, node, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("converge %s %v on %s: answered %d: %s", req.Unit, req.IDs, node, resp.StatusCode, bytes.TrimSpace(answer))
	}
	return nil
}
