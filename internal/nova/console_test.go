package nova

import (
	"context"
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

	"github.com/cobaltcore-dev/o3k/internal/console"
	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/cobaltcore-dev/o3k/pkg/hypervisor"
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
func newConsoleTestRouter(t *testing.T, mode string) (*gin.Engine, *console.Store) {
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
	svc := NewService("", mode, nil)
	store := console.NewStore(t.TempDir(), 10*time.Minute)
	svc.SetConsoleStore(store)
	svc.SetConsoleProxyBaseURL("http://console.example:6080")
	svc.RegisterRoutes(&r.RouterGroup)
	return r, store
}

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
	if err := database.DB.QueryRow(t.Context(),
		`INSERT INTO instances (name, project_id, user_id, status)
		 VALUES ($1, $2, $3, 'ACTIVE') RETURNING id`,
		name, projectID, seededUserID,
	).Scan(&id); err != nil {
		t.Fatalf("seeding instance: %v", err)
	}
	// context.Background(), not t.Context(): the test context is already cancelled by
	// the time cleanup runs, so t.Context() here would fail every delete.
	t.Cleanup(func() {
		_, _ = database.DB.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id)
	})
	return id
}

// --- Refusals that need no database ------------------------------------------

// Every console type but novnc must be refused with 400 rather than handed a
// fabricated URL. Before SPEC-021 each of these returned 200 with a
// localhost URL that led nowhere.
func TestConsoleUnsupportedTypesAreRefused(t *testing.T) {
	r, _ := newConsoleTestRouter(t, "stub")
	hdr := testAuthHdr(seededProjectID, seededUserID)
	id := "11111111-2222-3333-4444-555555555555"

	cases := []struct{ name, path, body string }{
		{"remote-consoles xvpvnc", "/v2.1/servers/" + id + "/remote-consoles",
			`{"remote_console":{"protocol":"vnc","type":"xvpvnc"}}`},
		{"remote-consoles serial", "/v2.1/servers/" + id + "/remote-consoles",
			`{"remote_console":{"protocol":"serial","type":"serial"}}`},
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

// An instance in another project is reported as absent, never as forbidden — a
// 403 would confirm it exists.
func TestConsoleCrossProjectIs404(t *testing.T) {
	requireDB(t)
	r, _ := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	otherProject := "00000000-0000-0000-0000-0000000000c1"
	if _, err := database.DB.Exec(t.Context(),
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
	if _, err := database.DB.Exec(t.Context(),
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
		{"guest not on loopback", hypervisor.ErrConsoleNotLoopback, http.StatusInternalServerError},
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

// The response shape is upstream's: {"console": {instance_uuid, host, port}}.
func TestConsoleAuthTokenShape(t *testing.T) {
	requireDB(t)
	r, store := newConsoleTestRouter(t, "stub")
	id := seedConsoleInstance(t, seededProjectID)

	tok, err := store.Issue(t.Context(), id, seededProjectID, "127.0.0.1", 5901)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	code, body := doReq(r, http.MethodGet, "/v2.1/os-console-auth-tokens/"+tok, "",
		testAuthHdr(seededProjectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("status = %d; want 200. body: %s", code, body)
	}

	var got struct {
		Console struct {
			InstanceUUID string `json:"instance_uuid"`
			Host         string `json:"host"`
			Port         int    `json:"port"`
		} `json:"console"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body is not the documented shape: %v\n%s", err, body)
	}
	if got.Console.InstanceUUID != id || got.Console.Host != "127.0.0.1" || got.Console.Port != 5901 {
		t.Errorf("console = %+v; want instance=%s 127.0.0.1:5901", got.Console, id)
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
// own. common.BaseURL applies its port argument only in its O3K_ENDPOINT_HOST
// branch, so deriving from it alone advertised http://<host>:8774 — the API — and
// the browser got a 404 where a console should be. Both branches are pinned here
// because the chart ships endpointHost and consoleProxyBaseUrl empty by default, so
// this fallback is the DEFAULT path of a real-mode install, not a corner case.
func TestConsoleURLFallbackUsesTheProxyPort(t *testing.T) {
	t.Setenv("O3K_ENDPOINT_SCHEME", "")

	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Request.Host = "10.2.199.101:8774" // Nova's port, as the request arrives
		return c
	}

	t.Run("derived from the request", func(t *testing.T) {
		t.Setenv("O3K_ENDPOINT_HOST", "")
		got := buildConsoleURL("", newCtx(), "abc123")
		want := "http://10.2.199.101:6080/vnc_lite.html?path=%3Ftoken%3Dabc123"
		if got != want {
			t.Errorf("url = %q; want %q", got, want)
		}
		if strings.Contains(got, ":8774") {
			t.Error("the fallback advertised Nova's API port as the console origin")
		}
	})

	t.Run("derived from O3K_ENDPOINT_HOST", func(t *testing.T) {
		t.Setenv("O3K_ENDPOINT_HOST", "o3k.example")
		got := buildConsoleURL("", newCtx(), "abc123")
		want := "http://o3k.example:6080/vnc_lite.html?path=%3Ftoken%3Dabc123"
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

	tok, err := store.Issue(t.Context(), id, seededProjectID, "127.0.0.1", 5901)
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
		if _, err := database.DB.Exec(t.Context(),
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

// FR-014, and the hole this feature exists to close. libvirt hands out console ports
// sequentially, so an instance that frees port P can have P reassigned to a different
// domain while a still-valid token points at it — surfacing another tenant's
// framebuffer. Store.RevokeByInstance is tested directly in internal/console; what
// these cover is the WIRING, which is where the hole actually closes. Without them a
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, store := newConsoleTestRouter(t, "stub")
			id := seedConsoleInstance(t, seededProjectID)

			tok, err := store.Issue(t.Context(), id, seededProjectID, "127.0.0.1", 5901)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}

			doReq(r, tc.method, fmt.Sprintf(tc.path, id), tc.body,
				testAuthHdr(seededProjectID, seededUserID))

			if _, err := store.Resolve(t.Context(), tok); !errors.Is(err, console.ErrTokenNotFound) {
				t.Errorf("token still resolves after %s: err = %v; want ErrTokenNotFound "+
					"— a reused console port would expose another guest", tc.name, err)
			}

			// The projection file, not just the row: for `delete` the FK cascade
			// removes the row on its own, so a Resolve check alone would pass even
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

	tok, err := store.Issue(t.Context(), id, seededProjectID, "127.0.0.1", 5901)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	doReq(r, http.MethodPost, "/v2.1/servers/"+name+"/action", `{"os-stop":null}`,
		testAuthHdr(seededProjectID, seededUserID))

	if _, err := store.Resolve(t.Context(), tok); !errors.Is(err, console.ErrTokenNotFound) {
		t.Errorf("token survived a name-addressed os-stop: err = %v; want ErrTokenNotFound", err)
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
