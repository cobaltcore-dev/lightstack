package keystone

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- AWS SigV2 mandates HMAC-SHA1; not a security control choice
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
)

// s3TokenRequest is the body Ceph RGW POSTs to validate an S3-signed request.
type s3TokenRequest struct {
	Credentials struct {
		Access    string `json:"access"`
		Token     string `json:"token"`     // base64-encoded string-to-sign
		Signature string `json:"signature"` // client signature to verify
	} `json:"credentials"`
}

// S3Tokens validates an S3-signed request on behalf of Ceph RGW
// (rgw s3 auth use keystone). Public + self-validating: the S3 signature IS the
// credential — Lightstack resolves the EC2 credential by access key and recomputes the
// signature from the stored secret (SPEC-014 FR-006). POST /v3/s3tokens.
func (svc *Service) S3Tokens(c *gin.Context) { svc.validateSignedRequest(c) }

// authFailed is Keystone's generic auth-failure message. Every 401 from the
// s3tokens/ec2tokens endpoints (and renderS3Token) returns it, never the
// specific reason — real Keystone masks Unauthorized reasons this way
// (SecurityError with insecure_debug off) so it never confirms *why* a request
// failed: a valid key with a bad signature returns exactly this string, not
// "signature mismatch". RGW keys off the 401 status, not the body. (An unknown
// key stays a 404 — see validateSignedRequest — which RGW needs to fall through
// to its local auth engine; that 404-vs-401 split is identical in real Keystone.)
const authFailed = "The request you have made requires authentication."

// EC2Tokens is the EC2-signing sibling of S3Tokens. POST /v3/ec2tokens.
func (svc *Service) EC2Tokens(c *gin.Context) { svc.validateSignedRequest(c) }

func (svc *Service) validateSignedRequest(c *gin.Context) {
	var req s3TokenRequest
	// Every 401 below returns Keystone's generic auth-failure message, never the
	// specific reason. Real Keystone masks Unauthorized reasons this way (SecurityError
	// with insecure_debug off) so it never confirms *why* a request failed — verified
	// against a live Keystone: a valid key with a bad signature returns exactly this
	// string, not "signature mismatch". RGW keys off the 401 status, not the body.
	// (An unknown key stays a 404 — see below — which RGW needs to fall through to its
	// local auth engine; that 404-vs-401 split is identical in real Keystone.)
	// authFailed is declared at package level so renderS3Token denies with the
	// same masked message.
	// The response stays masked (above), but every rejection is logged with its
	// specific reason server-side: these endpoints are public by design, so the log
	// is the only signal that someone is probing them (Constitution Art. V; PR #73
	// review r3710749072). The access key is an identifier, not a secret — the stored
	// secret and the client signature are never logged.
	denied := func(reason string) {
		log.Warn().
			Str("endpoint", c.FullPath()).
			Str("client_ip", c.ClientIP()).
			Str("access_key", req.Credentials.Access).
			Str("reason", reason).
			Msg("s3tokens: signature validation denied")
	}

	if err := c.ShouldBindJSON(&req); err != nil || req.Credentials.Access == "" {
		denied("malformed body or missing access key")
		common.SendError(c, common.NewUnauthorizedError(authFailed))
		return
	}

	// Resolve the credential (and its recoverable secret) by access key.
	var userID string
	var projectID *string
	var blob string
	err := database.DB.QueryRow(c.Request.Context(),
		`SELECT user_id, project_id, blob FROM credentials WHERE access_key = $1 AND type = 'ec2'`,
		req.Credentials.Access).Scan(&userID, &projectID, &blob)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown access key: not a Lightstack EC2 credential. Return 404 (as real
		// Keystone does) so RGW *falls through* to its next auth engine instead of
		// hard-rejecting the request — this lets local RGW users (e.g. the one
		// Glance uses for image storage) keep authenticating alongside Keystone.
		// Still fail-closed: RGW never grants on a 404, and a local miss → 403.
		// Debug, not Warn: this is the routine fall-through path for RGW-local users
		// (Glance's own image-storage user hits it on every request), so warning here
		// would drown the genuine denials below in expected noise.
		log.Debug().Str("client_ip", c.ClientIP()).Str("access_key", req.Credentials.Access).
			Msg("s3tokens: unknown access key; RGW falls through to its local auth engine")
		common.SendError(c, common.NewNotFoundError("credential"))
		return
	}
	if err != nil {
		log.Warn().Err(err).Msg("s3tokens: credential lookup failed")
		common.SendError(c, common.NewUnauthorizedError(authFailed)) // fail-closed
		return
	}
	var b ec2Blob
	if json.Unmarshal([]byte(blob), &b) != nil || b.Secret == "" {
		denied("stored credential blob is unusable")
		common.SendError(c, common.NewUnauthorizedError(authFailed))
		return
	}

	// RGW base64-encodes the string-to-sign in `token`; tolerate a raw value too.
	stringToSign, decErr := base64.StdEncoding.DecodeString(req.Credentials.Token)
	if decErr != nil {
		stringToSign = []byte(req.Credentials.Token)
	}

	if !verifyS3Signature(b.Secret, string(stringToSign), req.Credentials.Signature) {
		denied("signature mismatch")
		common.SendError(c, common.NewUnauthorizedError(authFailed))
		return
	}

	pid := b.TenantID
	if pid == "" && projectID != nil {
		pid = *projectID
	}
	svc.renderS3Token(c, userID, pid)
}

// verifyS3Signature recomputes the AWS signature over stringToSign from secret
// and constant-time compares it to provided. Supports SigV4 (default for modern
// SDKs and RGW) and SigV2 (legacy). Returns false on any malformed input —
// fail-closed.
func verifyS3Signature(secret, stringToSign, provided string) bool {
	if provided == "" {
		return false
	}
	if strings.HasPrefix(stringToSign, "AWS4-HMAC-SHA256") {
		expected, ok := sigV4Signature(secret, stringToSign)
		if !ok {
			return false
		}
		return hmac.Equal([]byte(expected), []byte(provided))
	}
	// SigV2: base64(HMAC-SHA1(secret, stringToSign)).
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(stringToSign))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(provided))
}

// sigV4Signature derives the SigV4 signing key from the credential scope embedded
// in line 3 of the string-to-sign ("<YYYYMMDD>/<region>/<service>/aws4_request")
// and returns hex(HMAC-SHA256(signingKey, stringToSign)).
func sigV4Signature(secret, stringToSign string) (string, bool) {
	lines := strings.Split(stringToSign, "\n")
	if len(lines) < 4 {
		return "", false
	}
	scope := strings.Split(lines[2], "/") // date / region / service / aws4_request
	if len(scope) < 4 {
		return "", false
	}
	date, region, service := scope[0], scope[1], scope[2]
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	return hex.EncodeToString(hmacSHA256(k, stringToSign)), true
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// renderS3Token returns the resolved identity RGW consumes to authorize and
// namespace the request (project id is the RGW tenant under implicit tenants).
func (svc *Service) renderS3Token(c *gin.Context, userID, projectID string) {
	// Fail closed for missing or disabled identities — the S3 analog of the
	// token-validation envelope (BuildTokenResponse) and the two token-issue
	// paths: the EC2 credential and the role assignments SURVIVE a disable, so
	// without this check a valid signature from a disabled user (or scoped to a
	// disabled project) would keep minting fresh, fully valid envelopes on every
	// request — S3 access with no expiry bound. Deleted identities already fail
	// closed via the credentials FK cascade (unknown key → 404 → RGW falls
	// through); a missing row here can only be a stale blob tenant — deny that
	// too. Like every other denial on these endpoints: generic 401, specific
	// reason logged server-side only.
	var userName string
	var userEnabled bool
	if err := database.DB.QueryRow(c.Request.Context(),
		"SELECT name, enabled FROM users WHERE id = $1", userID,
	).Scan(&userName, &userEnabled); err != nil || !userEnabled {
		log.Warn().Err(err).Str("user_id", userID).
			Msg("s3tokens: user is missing or disabled")
		common.SendError(c, common.NewUnauthorizedError(authFailed))
		return
	}
	var projectName string
	var projectEnabled bool
	if err := database.DB.QueryRow(c.Request.Context(),
		"SELECT name, enabled FROM projects WHERE id = $1", projectID,
	).Scan(&projectName, &projectEnabled); err != nil || !projectEnabled {
		log.Warn().Err(err).Str("project_id", projectID).
			Msg("s3tokens: project is missing or disabled")
		common.SendError(c, common.NewUnauthorizedError(authFailed))
		return
	}

	roles := []gin.H{}
	rows, err := database.DB.Query(c.Request.Context(), `
		SELECT DISTINCT r.name FROM roles r
		JOIN role_assignments ra ON ra.role_id = r.id
		WHERE ra.user_id = $1 AND ra.project_id = $2`, userID, projectID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil {
				roles = append(roles, gin.H{"name": name})
			}
		}
	}

	// RGW parses the response as a full Keystone token and requires expires_at
	// (and reads project.id as the tenant under implicit tenants=s3). Keystone
	// uses microsecond-precision UTC timestamps. Keep the window short: RGW may
	// cache the validation until expiry, so a small TTL bounds revocation latency.
	//
	// RGW derives the tenant namespace from project.id and only accepts
	// alphanumerics + underscore (no dashes). With keystone.rgw_compat_tenant_ids
	// off, project.id keeps the historical keystone-canonical (dash-free) form;
	// with the flag on it renders in the SAME underscored form as the Swift
	// token payloads (tokenProjectID), so S3 and Swift derive one implicit
	// tenant per project and objects stay visible across both protocols.
	// token.user.id is not used for tenant naming under implicit tenants and
	// keeps its dash-free form in either mode.
	const ksTime = "2006-01-02T15:04:05.000000Z"
	now := time.Now().UTC()
	c.JSON(200, gin.H{
		"token": gin.H{
			"methods":    []string{"s3"},
			"issued_at":  now.Format(ksTime),
			"expires_at": now.Add(5 * time.Minute).Format(ksTime),
			"user":       gin.H{"id": canonicalID(userID), "name": userName, "domain": gin.H{"id": "default", "name": "Default"}},
			"project":    gin.H{"id": svc.authService.s3TenantProjectID(projectID), "name": projectName, "domain": gin.H{"id": "default", "name": "Default"}},
			"roles":      roles,
		},
	})
}

// canonicalID renders a Lightstack UUID in the keystone-canonical, RGW-safe form:
// 32 hex chars with no dashes (RGW tenant/user names reject dashes).
func canonicalID(id string) string { return strings.ReplaceAll(id, "-", "") }
