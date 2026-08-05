package nova

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/console"
	"github.com/cobaltcore-dev/lightstack/pkg/hypervisor"
	"github.com/gin-gonic/gin"
)

// Console contract tests (SPEC-021). These encode contracts/nova-console-api.md:
// the response shapes for the two console surfaces, and the status-code matrix
// for everything we refuse.
//
// The pre-SPEC-021 tests asserted only "some URL string came back", which is why
// they passed against a console that could never work. These assert the URL is
// actually resolvable — a token exists, and it maps to the instance.

// newConsoleTestRouter builds a router whose Nova service has a console store
// backed by a temp projection dir, so issuance is exercised end to end.
func newConsoleTestRouter(t *testing.T, mode string, opts ...func(*Service)) (*gin.Engine, *console.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if p := c.GetHeader("X-Test-Project"); p != "" {
			c.Set("project_id", p)
		}
		if u := c.GetHeader("X-Test-User"); u != "" {
			c.Set("user_id", u)
		}
		if roles := c.GetHeader("X-Test-Roles"); roles != "" {
			c.Set("roles", strings.Split(roles, ","))
		}
	})
	svc := NewService(testPool, "", mode, nil)
	store := console.NewStore(testPool, t.TempDir(), 10*time.Minute)
	svc.SetConsoleStore(store)
	svc.SetConsoleProxyBaseURL("http://console.example:6080")
	for _, opt := range opts {
		opt(svc)
	}
	svc.RegisterRoutes(&r.RouterGroup)
	return r, store
}

// testConsoleSocket has the shape libvirt gives a running domain's VNC socket.
const testConsoleSocket = "/var/lib/libvirt/qemu/domain-1-instance-test/vnc.sock"

// seedConsoleInstance inserts an instance owned by the given project.
func seedConsoleInstance(t *testing.T, projectID string) string {
	t.Helper()
	return seedConsoleInstanceNamed(t, projectID, "spec021-"+t.Name())
}

// seedConsoleInstanceNamed is the same, with the name chosen by the caller — needed
// when the name goes into a request path, since t.Name() carries a "/" inside a
// subtest.
func seedConsoleInstanceNamed(t *testing.T, projectID, name string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO instances (name, project_id, user_id, status)
		 VALUES ($1, $2, $3, 'ACTIVE') RETURNING id`,
		name, projectID, seededUserID,
	).Scan(&id); err != nil {
		t.Fatalf("seeding instance: %v", err)
	}
	// context.Background(), not t.Context(): the test context is already cancelled by
	// the time cleanup runs, so t.Context() here would fail every delete.
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id)
	})
	return id
}

// --- Refusals that need no database ------------------------------------------

// Every console type but novnc, and every protocol but vnc, must be refused with 400
// rather than handed a fabricated URL. Before SPEC-021 each of these returned 200
// with a localhost URL that led nowhere. Upstream picks the console by protocol, so
// a novnc type under another protocol is no VNC console there either.
func TestConsoleUnsupportedTypesAreRefused(t *testing.T) {
	r, _ := newConsoleTestRouter(t, "stub")
	hdr := testAuthHdr(seededProjectID, seededUserID)
	id := "11111111-2222-3333-4444-555555555555"

	cases := []struct{ name, path, body string }{
		{"remote-consoles xvpvnc", "/v2.1/servers/" + id + "/remote-consoles",
			`{"remote_console":{"protocol":"vnc","type":"xvpvnc"}}`},
		{"remote-consoles serial", "/v2.1/servers/" + id + "/remote-consoles",
			`{"remote_console":{"protocol":"serial","type":"serial"}}`},
		{"remote-consoles serial protocol, novnc type", "/v2.1/servers/" + id + "/remote-consoles",
			`{"remote_console":{"protocol":"serial","type":"novnc"}}`},
		{"remote-consoles spice protocol, novnc type", "/v2.1/servers/" + id + "/remote-consoles",
			`{"remote_console":{"protocol":"spice","type":"novnc"}}`},
		{"action os-getSerialConsole", "/v2.1/servers/" + id + "/action",
			`{"os-getSerialConsole":{"type":"serial"}}`},
		{"action os-getSPICEConsole", "/v2.1/servers/" + id + "/action",
			`{"os-getSPICEConsole":{"type":"spice-html5"}}`},
		{"action os-getRDPConsole", "/v2.1/servers/" + id + "/action",
			`{"os-getRDPConsole":{"type":"rdp-html5"}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := doReq(r, http.MethodPost, tc.path, tc.body, hdr)
			if code != http.StatusBadRequest {
				t.Errorf("status = %d; want 400. body: %s", code, body)
			}
			if strings.Contains(body, "http://") || strings.Contains(body, "ws://") {
				t.Errorf("refusal must not contain a URL; body: %s", body)
			}
		})
	}
}

// Stub mode has no hypervisor and therefore no guest. It must say so (501)
// rather than return a link that fails in the browser — the F1 experience.
func TestConsoleStubModeReturns501(t *testing.T) {
	requireDB(t)
	r, _ := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	code, body := doReq(r, http.MethodPost,
		"/v2.1/servers/"+id+"/remote-consoles",
		`{"remote_console":{"protocol":"vnc","type":"novnc"}}`,
		testAuthHdr(seededProjectID, seededUserID))

	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501. body: %s", code, body)
	}
	if !strings.Contains(body, "stub") {
		t.Errorf("501 body should name stub mode as the reason; got: %s", body)
	}
	if strings.Contains(body, "http://") {
		t.Errorf("stub mode must not return a URL; got: %s", body)
	}
}

// Real mode whose hypervisor never came up — InitHypervisor failed at startup and main
// carried on with no manager — is an outage, not stub mode: 500, and no claim that
// there is no guest to display. The token sweep gets the same answer.
func TestConsoleWithoutAHypervisorIsAnOutage(t *testing.T) {
	requireDB(t)
	r, _ := newConsoleTestRouter(t, "real") // no manager, as after a failed InitHypervisor
	id := seedConsoleInstance(t, seededProjectID)

	code, body := doReq(r, http.MethodPost,
		"/v2.1/servers/"+id+"/remote-consoles",
		`{"remote_console":{"protocol":"vnc","type":"novnc"}}`,
		testAuthHdr(seededProjectID, seededUserID))

	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500. body: %s", code, body)
	}
	if strings.Contains(body, "stub") {
		t.Errorf("a real-mode outage must not be reported as stub mode; got: %s", body)
	}

	svc := NewService(testPool, "", "real", nil)
	if _, err := svc.CurrentConsoleSocket(t.Context(), id); !errors.Is(err, errNoHypervisor) {
		t.Errorf("CurrentConsoleSocket error = %v; want errNoHypervisor", err)
	}
}

// With no console proxy deployed — multi-node, where v1 has none, or
// consoleProxy.enabled=false — any URL would lead nowhere, so both surfaces refuse
// with upstream's answer for a disabled VNC console: 400 "Unavailable console type
// novnc." Stub mode keeps its own, more specific 501.
func TestConsoleWithoutAProxyIsRefused(t *testing.T) {
	requireDB(t)
	noProxy := func(svc *Service) { svc.SetConsoleEnabled(false) }

	for _, tc := range []struct{ name, path, body string }{
		{"remote-consoles", "/v2.1/servers/%s/remote-consoles", `{"remote_console":{"protocol":"vnc","type":"novnc"}}`},
		{"os-getVNCConsole", "/v2.1/servers/%s/action", `{"os-getVNCConsole":{"type":"novnc"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newConsoleTestRouter(t, "real", noProxy)
			id := seedConsoleInstance(t, seededProjectID)

			code, body := doReq(r, http.MethodPost, fmt.Sprintf(tc.path, id), tc.body,
				testAuthHdr(seededProjectID, seededUserID))

			if code != http.StatusBadRequest {
				t.Errorf("status = %d; want 400. body: %s", code, body)
			}
			if !strings.Contains(body, "Unavailable console type novnc") {
				t.Errorf("refusal should carry upstream's message; got: %s", body)
			}
			if strings.Contains(body, "http://") {
				t.Errorf("refusal must not contain a URL; got: %s", body)
			}
		})
	}

	t.Run("stub mode still says stub", func(t *testing.T) {
		r, _ := newConsoleTestRouter(t, "stub", noProxy)
		id := seedConsoleInstance(t, seededProjectID)
		code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/remote-consoles",
			`{"remote_console":{"protocol":"vnc","type":"novnc"}}`, testAuthHdr(seededProjectID, seededUserID))
		if code != http.StatusNotImplemented {
			t.Errorf("status = %d; want 501 — stub mode's reason is the more specific one. body: %s", code, body)
		}
	})
}

// An instance in another project is reported as absent, never as forbidden — a
// 403 would confirm it exists.
func TestConsoleCrossProjectIs404(t *testing.T) {
	requireDB(t)
	r, _ := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	otherProject := "00000000-0000-0000-0000-0000000000c1"
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO projects (id, name, description, enabled) VALUES ($1, 'spec021-other', '', true)
		 ON CONFLICT (id) DO NOTHING`, otherProject); err != nil {
		t.Fatalf("seeding other project: %v", err)
	}

	// Non-admin caller in a different project.
	hdr := map[string]string{
		"X-Test-Project": otherProject,
		"X-Test-User":    seededUserID,
		"X-Test-Roles":   "member",
	}
	code, body := doReq(r, http.MethodPost,
		"/v2.1/servers/"+id+"/remote-consoles",
		`{"remote_console":{"protocol":"vnc","type":"novnc"}}`, hdr)

	if code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 for another project's instance. body: %s", code, body)
	}
}

// An admin may open a console anywhere, matching upstream's admin_or_owner —
// without it the Admin panel's Console tab is broken. In stub mode the request
// gets as far as the endpoint readback, so 501 (not 404) proves authorization
// passed.
func TestConsoleAdminCrossProjectPassesAuthz(t *testing.T) {
	requireDB(t)
	r, _ := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	otherProject := "00000000-0000-0000-0000-0000000000c2"
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO projects (id, name, description, enabled) VALUES ($1, 'spec021-admin', '', true)
		 ON CONFLICT (id) DO NOTHING`, otherProject); err != nil {
		t.Fatalf("seeding other project: %v", err)
	}

	hdr := map[string]string{
		"X-Test-Project": otherProject,
		"X-Test-User":    seededUserID,
		"X-Test-Roles":   "admin",
	}
	code, body := doReq(r, http.MethodPost,
		"/v2.1/servers/"+id+"/remote-consoles",
		`{"remote_console":{"protocol":"vnc","type":"novnc"}}`, hdr)

	if code == http.StatusNotFound {
		t.Fatalf("admin was refused another project's console (404); authz override is missing. body: %s", body)
	}
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d; want 501 (authz passed, stub has no guest). body: %s", code, body)
	}
}

// An instance that never got a libvirt domain — created without an image, so it
// reached ERROR — must be reported as not-running (409), not as a server fault.
// Found during the real-mode host pass, where it returned an opaque
// "computeFault 500: failed to read the instance console endpoint".
func TestConsoleEndpointErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"stub mode", hypervisor.ErrConsoleUnavailable, http.StatusNotImplemented},
		{"no libvirt domain / not running", hypervisor.ErrConsoleNotReady, http.StatusConflict},
		{"no graphical console", hypervisor.ErrNoGraphicalConsole, http.StatusConflict},
		{"guest console is a tcp listener", hypervisor.ErrConsoleNotSocket, http.StatusInternalServerError},
		{"unreadable xml", hypervisor.ErrConsoleParse, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

			sendConsoleEndpointError(c, "11111111-2222-3333-4444-555555555555", tc.err)

			if w.Code != tc.want {
				t.Errorf("status = %d; want %d (body: %s)", w.Code, tc.want, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "http://") {
				t.Errorf("refusal must not contain a URL: %s", w.Body.String())
			}
		})
	}
}

// The reconciler asks Nova which socket a guest serves now, and a readback failure
// means two different things: "this guest serves no console" (it stopped, it has
// none, it is a legacy TCP listener), on which its tokens are revoked, or "cannot
// tell right now", on which they are kept for the next sweep.
func TestSocketForSweep(t *testing.T) {
	blip := errors.New("libvirt unreachable")
	cases := []struct {
		name       string
		socket     string
		err        error
		wantSocket string
		wantErr    bool
	}{
		{"serving", testConsoleSocket, nil, testConsoleSocket, false},
		{"not running", "", hypervisor.ErrConsoleNotReady, "", false},
		{"no console device", "", hypervisor.ErrNoGraphicalConsole, "", false},
		{"legacy tcp listener", "", hypervisor.ErrConsoleNotSocket, "", false},
		{"libvirt unreachable", "", blip, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := socketForSweep(tc.socket, tc.err)
			if (err != nil) != tc.wantErr || got != tc.wantSocket {
				t.Errorf("socketForSweep = (%q, %v); want (%q, error=%v)", got, err, tc.wantSocket, tc.wantErr)
			}
		})
	}
}

// --- The compatibility view ---------------------------------------------------

func TestConsoleAuthTokenUnknownIs404(t *testing.T) {
	// Resolve goes to the database even for an unknown token, so this needs one.
	requireDB(t)
	r, _ := newConsoleTestRouter(t, "stub")
	code, _ := doReq(r, http.MethodGet,
		"/v2.1/os-console-auth-tokens/deadbeefdeadbeefdeadbeefdeadbeef", "",
		testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusNotFound {
		t.Errorf("status = %d; want 404 for an unknown token", code)
	}
}

// A lookup that fails is not a missing token. Upstream answers 404 here too, but only
// because its tokens live in per-cell databases and a cell that does not answer is
// indistinguishable from one without the token; Lightstack has one database, so a
// failure means that database failed. A cancelled request context makes Resolve
// fail without ErrNoRows, standing in for an outage.
func TestConsoleAuthTokenLookupFailureIs500(t *testing.T) {
	requireDB(t)
	r, _ := newConsoleTestRouter(t, "stub")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet,
		"/v2.1/os-console-auth-tokens/deadbeefdeadbeefdeadbeefdeadbeef", nil).WithContext(ctx)
	for k, v := range testAuthHdr(seededProjectID, seededUserID) {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 when the lookup itself fails. body: %s", w.Code, w.Body.String())
	}
}

// The response shape is upstream's: {"console": {instance_uuid, host, port,
// internal_access_path}}. A console here is a UNIX socket, which has neither host
// nor port, so both are null and the socket is the internal access path — rather
// than a fabricated endpoint, which is how the console reached F1 in the first place.
func TestConsoleAuthTokenShape(t *testing.T) {
	requireDB(t)
	r, store := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	tok, err := store.Issue(t.Context(), id, seededProjectID, testConsoleSocket)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	code, body := doReq(r, http.MethodGet, "/v2.1/os-console-auth-tokens/"+tok, "",
		testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200. body: %s", code, body)
	}

	var got struct {
		Console map[string]any `json:"console"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not the documented shape: %v\n%s", err, body)
	}
	for _, key := range []string{"instance_uuid", "host", "port", "internal_access_path"} {
		if _, ok := got.Console[key]; !ok {
			t.Errorf("console has no %q key: %s", key, body)
		}
	}
	if got.Console["instance_uuid"] != id || got.Console["internal_access_path"] != testConsoleSocket {
		t.Errorf("console = %v; want instance=%s internal_access_path=%s", got.Console, id, testConsoleSocket)
	}
	if got.Console["host"] != nil || got.Console["port"] != nil {
		t.Errorf("host/port = %v/%v; want null — a unix socket has neither", got.Console["host"], got.Console["port"])
	}
}

// --- The URL contract ----------------------------------------------------------

// buildConsoleURL is what Horizon iframes and the CLI prints, so its shape is a
// contract: <origin>/vnc_lite.html?path=%3Ftoken%3D<token>. The path form is
// upstream's — the noVNC client appends it to its WebSocket URL, so the browser
// opens ws://<origin>/?token=… against websockify.
func TestBuildConsoleURLShape(t *testing.T) {
	got := buildConsoleURL("http://console.example:6080", nil, "abc123")
	want := "http://console.example:6080/vnc_lite.html?path=%3Ftoken%3Dabc123"
	if got != want {
		t.Fatalf("url = %q; want %q", got, want)
	}

	// The page must be one the deployed client actually serves: noVNC 1.6.0 has
	// no vnc_auto.html, which is why the operator docs were wrong for years.
	if strings.Contains(got, "vnc_auto.html") {
		t.Error("vnc_auto.html does not exist in noVNC 1.6.0")
	}

	// And the token must survive a round trip through the query encoding.
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("issued URL does not parse: %v", err)
	}
	if p := u.Query().Get("path"); p != "?token=abc123" {
		t.Errorf("path param = %q; want %q", p, "?token=abc123")
	}
}

// With no origin configured the fallback must name the CONSOLE port, never Nova's
// own. common.BaseURL applies its port argument only in its LIGHTSTACK_ENDPOINT_HOST
// branch, so deriving from it alone advertised http://<host>:8774 — the API — and
// the browser got a 404 where a console should be. Both branches are pinned here
// because the chart ships endpointHost and consoleProxyBaseUrl empty by default, so
// this fallback is the DEFAULT path of a real-mode install, not a corner case.
func TestConsoleURLFallbackUsesTheProxyPort(t *testing.T) {
	t.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", "")

	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Request.Host = "10.2.199.101:8774" // Nova's port, as the request arrives
		return c
	}

	t.Run("derived from the request", func(t *testing.T) {
		t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "")
		got := buildConsoleURL("", newCtx(), "abc123")
		want := "http://10.2.199.101:6080/vnc_lite.html?path=%3Ftoken%3Dabc123"
		if got != want {
			t.Errorf("url = %q; want %q", got, want)
		}
		if strings.Contains(got, ":8774") {
			t.Error("the fallback advertised Nova's API port as the console origin")
		}
	})

	t.Run("derived from LIGHTSTACK_ENDPOINT_HOST", func(t *testing.T) {
		t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "lightstack.example")
		got := buildConsoleURL("", newCtx(), "abc123")
		want := "http://lightstack.example:6080/vnc_lite.html?path=%3Ftoken%3Dabc123"
		if got != want {
			t.Errorf("url = %q; want %q", got, want)
		}
	})

	// url.URL.Hostname drops an IPv6 literal's brackets; put back without them, the
	// origin "http://2001:db8::1:6080" does not even parse.
	t.Run("an IPv6 host keeps its brackets", func(t *testing.T) {
		t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "")
		c := newCtx()
		c.Request.Host = "[2001:db8::1]:8774"
		got := buildConsoleURL("", c, "abc123")
		want := "http://[2001:db8::1]:6080/vnc_lite.html?path=%3Ftoken%3Dabc123"
		if got != want {
			t.Errorf("url = %q; want %q", got, want)
		}
	})

	// The proxy the chart ships serves plain HTTP whatever the API speaks: an https
	// console needs something in front of it to terminate TLS, and that is the
	// explicit console_proxy_base_url.
	t.Run("plain http even when the API is https", func(t *testing.T) {
		t.Setenv("LIGHTSTACK_ENDPOINT_HOST", "")
		t.Setenv("LIGHTSTACK_ENDPOINT_SCHEME", "https")
		c := newCtx()
		c.Request.TLS = &tls.ConnectionState{}
		got := buildConsoleURL("", c, "abc123")
		want := "http://10.2.199.101:6080/vnc_lite.html?path=%3Ftoken%3Dabc123"
		if got != want {
			t.Errorf("url = %q; want %q", got, want)
		}
	})
}

// A configured origin is used verbatim: trailing slashes must not produce a
// double slash, which some proxies treat as a different route.
func TestBuildConsoleURLTrimsTrailingSlash(t *testing.T) {
	got := buildConsoleURL("http://console.example:6080/", nil, "abc123")
	if strings.Contains(got, "6080//") {
		t.Errorf("url has a doubled slash: %s", got)
	}
}

var consoleURLPattern = regexp.MustCompile(`^http://console\.example:6080/vnc_lite\.html\?path=%3Ftoken%3D[0-9a-f]{32}$`)

// The end-to-end happy path is only reachable in real mode, but the pieces below
// the hypervisor call can still be asserted: given a store, issuance produces a
// token that resolves, and the URL embeds exactly that token.
func TestIssuedURLEmbedsAResolvableToken(t *testing.T) {
	requireDB(t)
	_, store := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	tok, err := store.Issue(t.Context(), id, seededProjectID, testConsoleSocket)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	u := buildConsoleURL("http://console.example:6080", nil, tok)
	if !consoleURLPattern.MatchString(u) {
		t.Fatalf("url %q does not match the documented shape", u)
	}
	if _, err := store.Resolve(t.Context(), tok); err != nil {
		t.Fatalf("the token embedded in the URL does not resolve: %v", err)
	}
}

// Both console surfaces must apply the SAME authorization rule (FR-013). Before this
// was shared, remote-consoles had the admin override and os-getConsoleOutput did not,
// so an admin could open a tenant's framebuffer and get 404 on that same instance's
// boot log. The pairing is the point — asserting either endpoint alone would not have
// caught the divergence.
func TestConsoleSurfacesShareTheSameAuthzRule(t *testing.T) {
	requireDB(t)

	otherProject := "00000000-0000-0000-0000-0000000000c3"
	seedProject := func(t *testing.T) {
		t.Helper()
		if _, err := testPool.Exec(t.Context(),
			`INSERT INTO projects (id, name, description, enabled) VALUES ($1, 'spec021-authz', '', true)
			 ON CONFLICT (id) DO NOTHING`, otherProject); err != nil {
			t.Fatalf("seeding other project: %v", err)
		}
	}

	// Both surfaces, driven through the action/remote-console APIs.
	surfaces := []struct {
		name, path, body string
		method           string
	}{
		{"os-getConsoleOutput", "/v2.1/servers/%s/action", `{"os-getConsoleOutput":{"length":50}}`, http.MethodPost},
		{"remote-consoles", "/v2.1/servers/%s/remote-consoles", `{"remote_console":{"protocol":"vnc","type":"novnc"}}`, http.MethodPost},
	}

	for _, s := range surfaces {
		t.Run(s.name+"/admin may reach another project", func(t *testing.T) {
			r, _ := newConsoleTestRouter(t, "stub")
			id := seedConsoleInstance(t, seededProjectID)
			seedProject(t)

			code, body := doReq(r, s.method, fmt.Sprintf(s.path, id), s.body, map[string]string{
				"X-Test-Project": otherProject, // admin scoped elsewhere
				"X-Test-User":    seededUserID,
				"X-Test-Roles":   "admin",
			})

			if code == http.StatusNotFound {
				t.Fatalf("admin was refused another project's instance (404) — the admin override is "+
					"missing from %s. body: %s", s.name, body)
			}
		})

		t.Run(s.name+"/non-admin may not", func(t *testing.T) {
			r, _ := newConsoleTestRouter(t, "stub")
			id := seedConsoleInstance(t, seededProjectID)
			seedProject(t)

			code, body := doReq(r, s.method, fmt.Sprintf(s.path, id), s.body, map[string]string{
				"X-Test-Project": otherProject,
				"X-Test-User":    seededUserID,
				"X-Test-Roles":   "member",
			})

			if code != http.StatusNotFound {
				t.Errorf("status = %d; want 404 for a foreign project's instance on %s. body: %s",
					code, s.name, body)
			}
		})
	}
}

// --- Revocation on lifecycle transitions -----------------------------------------

// FR-014: every operation that can move or end a guest's console revokes its links,
// so none outlives the console it was issued for. Store.RevokeByInstance is tested
// directly in internal/console; what these cover is the WIRING, which is where the
// guarantee is actually kept. Without them a
// refactor that dropped the revoke from one handler, or moved it after the state
// change, would pass the whole suite.
//
// The action's own outcome is deliberately not asserted: revocation happens before
// the action is dispatched, so it must hold even when the action itself is refused.
func TestLifecycleActionsRevokeConsoleTokens(t *testing.T) {
	requireDB(t)

	for _, tc := range []struct{ name, method, path, body string }{
		{"delete", http.MethodDelete, "/v2.1/servers/%s", ""},
		{"reboot", http.MethodPost, "/v2.1/servers/%s/action", `{"reboot":{"type":"SOFT"}}`},
		{"os-stop", http.MethodPost, "/v2.1/servers/%s/action", `{"os-stop":null}`},
		{"os-start", http.MethodPost, "/v2.1/servers/%s/action", `{"os-start":null}`},
		{"suspend", http.MethodPost, "/v2.1/servers/%s/action", `{"suspend":null}`},
		{"shelve", http.MethodPost, "/v2.1/servers/%s/action", `{"shelve":null}`},
		{"shelveOffload", http.MethodPost, "/v2.1/servers/%s/action", `{"shelveOffload":null}`},
		{"forceDelete", http.MethodPost, "/v2.1/servers/%s/action", `{"forceDelete":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, store := newConsoleTestRouter(t, "stub")
			id := seedConsoleInstance(t, seededProjectID)

			// suspend and shelve scan libvirt_domain_id into a plain string, so the NULL
			// a bare seed leaves fails them before they reach the revoke. '' is what
			// every handler here reads as "no domain".
			if _, err := testPool.Exec(t.Context(),
				"UPDATE instances SET libvirt_domain_id = '' WHERE id = $1", id); err != nil {
				t.Fatalf("clearing libvirt_domain_id: %v", err)
			}

			tok, err := store.Issue(t.Context(), id, seededProjectID, testConsoleSocket)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}

			doReq(r, tc.method, fmt.Sprintf(tc.path, id), tc.body,
				testAuthHdr(seededProjectID, seededUserID))

			if _, err := store.Resolve(t.Context(), tok); !errors.Is(err, console.ErrTokenNotFound) {
				t.Errorf("token still resolves after %s: err = %v; want ErrTokenNotFound "+
					"— the link outlived the console it was issued for", tc.name, err)
			}

			// The projection file, not just the row: for `delete` and `forceDelete` the
			// FK cascade removes the row on its own, so a Resolve check alone would pass even
			// with the revoke wiring gone. The file is what the proxy reads, and
			// only revocation removes it.
			if _, err := os.Stat(filepath.Join(store.Dir(), tok)); !os.IsNotExist(err) {
				t.Errorf("projection entry survived %s — the proxy would still resolve it", tc.name)
			}
		})
	}
}

// ServerAction accepts an instance NAME as well as an id, and revokes against the
// UUID it reads back rather than the path parameter (handlers.go, resolvedInstanceID).
// Nothing else guards that: keying the revoke off c.Param("id") would silently
// no-op for every name-addressed request, and the id-based tests above would not
// notice.
func TestLifecycleRevokeWorksWhenAddressedByName(t *testing.T) {
	requireDB(t)
	r, store := newConsoleTestRouter(t, "stub")

	name := "spec021-by-name"
	id := seedConsoleInstanceNamed(t, seededProjectID, name)

	tok, err := store.Issue(t.Context(), id, seededProjectID, testConsoleSocket)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	doReq(r, http.MethodPost, "/v2.1/servers/"+name+"/action", `{"os-stop":null}`,
		testAuthHdr(seededProjectID, seededUserID))

	if _, err := store.Resolve(t.Context(), tok); !errors.Is(err, console.ErrTokenNotFound) {
		t.Errorf("token survived a name-addressed os-stop: err = %v; want ErrTokenNotFound", err)
	}
}

// forceDelete revokes before it deletes, and RevokeByInstance is not project-scoped,
// so the revoke is gated on the caller owning the instance. Without that gate any
// project could kill another's console links by id, while the project-scoped DELETE
// itself removed nothing.
func TestForceDeleteKeepsForeignProjectsTokens(t *testing.T) {
	requireDB(t)
	r, store := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	tok, err := store.Issue(t.Context(), id, seededProjectID, testConsoleSocket)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"forceDelete":null}`,
		map[string]string{
			"X-Test-Project": "00000000-0000-0000-0000-0000000000c4",
			"X-Test-User":    seededUserID,
			"X-Test-Roles":   "member",
		})

	if _, err := store.Resolve(t.Context(), tok); err != nil {
		t.Errorf("owner's token revoked by another project's forceDelete: %v", err)
	}
}

// An action this API does not know is refused before the instance is looked up — the
// order upstream's dispatcher uses (nova/api/openstack/wsgi.py). Refused after the
// revoke instead, any request naming no real action wiped the instance's live console
// links, and once revocation fails closed it would have turned into a 500.
func TestUnknownActionKeepsConsoleTokens(t *testing.T) {
	requireDB(t)
	r, store := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	tok, err := store.Issue(t.Context(), id, seededProjectID, testConsoleSocket)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", `{"os-frobnicate":null}`,
		testAuthHdr(seededProjectID, seededUserID))

	if code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400. body: %s", code, body)
	}
	if _, err := store.Resolve(t.Context(), tok); err != nil {
		t.Errorf("an unknown action revoked the console token: %v", err)
	}
}

// FR-014 fails closed. Revocation is the only thing keeping a token from resolving
// to a port libvirt has since handed to another instance — websockify cannot
// re-check the port at connect time the way upstream Nova's proxy does — and the
// reconciler reaps only expired and orphaned tokens. So an action whose revoke fails
// must not run at all: 500, and the instance exactly as it was.
//
// The poisoned token carries a "/", which validTokenFilename rejects, so the revoke
// fails deterministically, as root too. The flavor is there so shelve gets past its
// snapshot insert: without it shelve fails there on its own and would pass this test
// with the revoke failure ignored.
func TestLifecycleActionsFailClosedWhenRevocationFails(t *testing.T) {
	requireDB(t)

	for _, tc := range []struct{ name, status, method, path, body string }{
		{"delete", "ACTIVE", http.MethodDelete, "/v2.1/servers/%s", ""},
		{"reboot", "ACTIVE", http.MethodPost, "/v2.1/servers/%s/action", `{"reboot":{"type":"SOFT"}}`},
		{"os-stop", "ACTIVE", http.MethodPost, "/v2.1/servers/%s/action", `{"os-stop":null}`},
		{"os-start", "SHUTOFF", http.MethodPost, "/v2.1/servers/%s/action", `{"os-start":null}`},
		{"suspend", "ACTIVE", http.MethodPost, "/v2.1/servers/%s/action", `{"suspend":null}`},
		{"resume", "SUSPENDED", http.MethodPost, "/v2.1/servers/%s/action", `{"resume":null}`},
		{"shelve", "ACTIVE", http.MethodPost, "/v2.1/servers/%s/action", `{"shelve":null}`},
		{"shelveOffload", "ACTIVE", http.MethodPost, "/v2.1/servers/%s/action", `{"shelveOffload":null}`},
		{"forceDelete", "ACTIVE", http.MethodPost, "/v2.1/servers/%s/action", `{"forceDelete":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newConsoleTestRouter(t, "stub")
			id := seedConsoleInstance(t, seededProjectID)

			if _, err := testPool.Exec(t.Context(),
				`UPDATE instances SET status = $2, libvirt_domain_id = '',
				        flavor_id = (SELECT id FROM flavors WHERE name = 'm1.small')
				  WHERE id = $1`, id, tc.status); err != nil {
				t.Fatalf("preparing instance: %v", err)
			}
			if _, err := testPool.Exec(t.Context(),
				`INSERT INTO console_auth_tokens (token, instance_id, project_id, socket, expires_at)
				 VALUES ($1, $2, $3, $4, now() + interval '10 minutes')`,
				"bad/"+tc.name, id, seededProjectID, testConsoleSocket); err != nil {
				t.Fatalf("seeding the unrevocable token: %v", err)
			}

			code, body := doReq(r, tc.method, fmt.Sprintf(tc.path, id), tc.body,
				testAuthHdr(seededProjectID, seededUserID))

			if code != http.StatusInternalServerError {
				t.Errorf("status = %d; want 500 when revocation fails. body: %s", code, body)
			}
			var status string
			if err := testPool.QueryRow(t.Context(),
				"SELECT status FROM instances WHERE id = $1", id).Scan(&status); err != nil {
				t.Fatalf("instance gone after a refused %s: %v", tc.name, err)
			}
			if status != tc.status {
				t.Errorf("status = %q after a refused %s; want it unchanged at %q", status, tc.name, tc.status)
			}
		})
	}
}

// --- The serial fallback must not regress ---------------------------------------

// os-getConsoleOutput is the documented workaround that kept working throughout
// the F1 outage. SPEC-021 must not touch it (FR-026).
func TestConsoleOutputStillWorks(t *testing.T) {
	requireDB(t)
	r, _ := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action",
		`{"os-getConsoleOutput":{"length":50}}`,
		testAuthHdr(seededProjectID, seededUserID))

	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200. body: %s", code, body)
	}
	var got struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not {\"output\": …}: %v\n%s", err, body)
	}
	if got.Output == "" {
		t.Error("console log output is empty")
	}
}

// A lookup that fails, rather than finding no row, has not established that the caller
// may see the instance, so nothing may be served: in real mode the output is the
// guest's serial log. The cancelled context makes the lookup itself fail.
func TestConsoleOutputLookupFailureIs500(t *testing.T) {
	requireDB(t)
	r, _ := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v2.1/servers/"+id+"/action",
		strings.NewReader(`{"os-getConsoleOutput":{"length":50}}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range testAuthHdr(seededProjectID, seededUserID) {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500 when the lookup itself fails. body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"output"`) {
		t.Errorf("console output served without a successful lookup: %s", w.Body.String())
	}
}
