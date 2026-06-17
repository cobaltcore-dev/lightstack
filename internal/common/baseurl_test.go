package common

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

func ginCtx(useTLS bool, host string) *gin.Context {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = host
	if useTLS {
		req.TLS = &tls.ConnectionState{}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	return c
}

func TestBaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	save := func(k string) func() {
		v, had := os.LookupEnv(k)
		return func() {
			if had {
				os.Setenv(k, v)
			} else {
				os.Unsetenv(k)
			}
		}
	}
	defer save("O3K_ENDPOINT_HOST")()
	defer save("O3K_ENDPOINT_SCHEME")()

	// O3K_ENDPOINT_HOST + scheme=https → https with configured host+port
	os.Setenv("O3K_ENDPOINT_HOST", "o3k.example.com")
	os.Setenv("O3K_ENDPOINT_SCHEME", "https")
	if got := BaseURL(ginCtx(false, "ignored:1"), 9292); got != "https://o3k.example.com:9292" {
		t.Errorf("host+https: got %q", got)
	}

	// scheme default (unset) → http
	os.Unsetenv("O3K_ENDPOINT_SCHEME")
	if got := BaseURL(ginCtx(false, "ignored:1"), 9292); got != "http://o3k.example.com:9292" {
		t.Errorf("host+default: got %q", got)
	}

	// No O3K_ENDPOINT_HOST, scheme unset, TLS request → https (preserved auto-detect)
	os.Unsetenv("O3K_ENDPOINT_HOST")
	if got := BaseURL(ginCtx(true, "host:443"), 0); got != "https://host:443" {
		t.Errorf("tls autodetect: got %q", got)
	}

	// No O3K_ENDPOINT_HOST, scheme=https forces https on a non-TLS request
	os.Setenv("O3K_ENDPOINT_SCHEME", "https")
	if got := BaseURL(ginCtx(false, "host:80"), 0); got != "https://host:80" {
		t.Errorf("forced https: got %q", got)
	}
}
