package keystone

import (
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- testing the SigV2 path, which mandates HMAC-SHA1
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The public s3tokens/ec2tokens endpoints must reject a malformed body with
// Keystone's generic, reason-free 401 — never a specific message that would confirm
// why the request failed (that response masking is what closed the access-key
// enumeration oracle, cloud-storage#518). The specific reason is logged server-side
// instead (PR #73 review r3710749072).
func TestValidateSignedRequestMasksRejectionReason(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{`{}`, `not json`, `{"credentials":{"access":""}}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v3/s3tokens", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")

		(&Service{}).S3Tokens(c)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("body %q: status = %d, want 401", body, w.Code)
		}
		if got := w.Body.String(); !strings.Contains(got, "The request you have made requires authentication.") {
			t.Errorf("body %q: response %s must carry Keystone's generic auth-failure message", body, got)
		}
		for _, leak := range []string{"malformed", "missing access", "signature", "blob"} {
			if strings.Contains(strings.ToLower(w.Body.String()), leak) {
				t.Errorf("body %q: response leaks the rejection reason %q: %s", body, leak, w.Body.String())
			}
		}
	}
}

func testHMAC256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// SPEC-014 FR-006: /v3/s3tokens must accept a correctly-signed request and
// reject tampered signatures / wrong secrets. Verifies the SigV4 signing-key
// derivation (scope parsing + HMAC chain) matches the AWS spec.
func TestVerifyS3Signature_SigV4(t *testing.T) {
	const secret = "supersecretkey"
	sts := "AWS4-HMAC-SHA256\n" +
		"20240101T000000Z\n" +
		"20240101/us-east-1/s3/aws4_request\n" +
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	k := testHMAC256([]byte("AWS4"+secret), "20240101")
	k = testHMAC256(k, "us-east-1")
	k = testHMAC256(k, "s3")
	k = testHMAC256(k, "aws4_request")
	good := hex.EncodeToString(testHMAC256(k, sts))

	if !verifyS3Signature(secret, sts, good) {
		t.Fatal("valid SigV4 signature was rejected")
	}
	if verifyS3Signature(secret, sts, "deadbeef") {
		t.Fatal("bad SigV4 signature was accepted")
	}
	if verifyS3Signature("othersecret", sts, good) {
		t.Fatal("SigV4 signature accepted under the wrong secret")
	}
	if verifyS3Signature(secret, sts, "") {
		t.Fatal("empty signature was accepted")
	}
}

// SigV2 path: base64(HMAC-SHA1(secret, stringToSign)).
func TestVerifyS3Signature_SigV2(t *testing.T) {
	const secret = "supersecretkey"
	sts := "GET\n\n\nMon, 01 Jan 2024 00:00:00 GMT\n/bucket/object"

	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(sts))
	good := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !verifyS3Signature(secret, sts, good) {
		t.Fatal("valid SigV2 signature was rejected")
	}
	if verifyS3Signature(secret, sts, "wrong-signature") {
		t.Fatal("bad SigV2 signature was accepted")
	}
	if verifyS3Signature("othersecret", sts, good) {
		t.Fatal("SigV2 signature accepted under the wrong secret")
	}
}

// A SigV4 string-to-sign with a malformed scope line must fail closed.
func TestVerifyS3Signature_SigV4MalformedScope(t *testing.T) {
	if verifyS3Signature("s", "AWS4-HMAC-SHA256\n20240101T000000Z\nbroken-scope\nhash", "anything") {
		t.Fatal("malformed SigV4 scope must fail closed")
	}
}
