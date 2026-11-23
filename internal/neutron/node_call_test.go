package neutron

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/compute"
	"github.com/cobaltcore-dev/lightstack/internal/keystone"
	"github.com/cobaltcore-dev/lightstack/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T108 (FR-034; research R27; contracts/node-apply.md): the node-to-node call. A
// node token signed with the deployment's secret, for one node and one body, is the only way in; the
// target answers 409 for a unit it does not hold and gives up within 900 ms of receipt; the caller
// gives up within one second; a call to the node itself never goes over HTTP.

var testNodeSecret = []byte("spec033-node-call-test-secret")

// tokenSpec is a node token's claims, each settable, so a test can make one of them wrong at a time.
type tokenSpec struct {
	audience string
	node     string
	digestOf []byte // the body whose SHA-256 the token carries
	issued   time.Time
	lifetime time.Duration
	secret   []byte
	noExpiry bool // a token without exp, which would otherwise never expire
	noIssued bool // a token without iat, whose lifetime cannot be told
}

func goodToken(node string, body []byte) tokenSpec {
	return tokenSpec{audience: "lightstack-node", node: node, digestOf: body, issued: time.Now(),
		lifetime: 10 * time.Second, secret: testNodeSecret}
}

func signNodeToken(t *testing.T, s tokenSpec) string {
	t.Helper()
	sum := sha256.Sum256(s.digestOf)
	claims := jwt.MapClaims{
		"node":        s.node,
		"body_sha256": hex.EncodeToString(sum[:]),
		"iat":         s.issued.Unix(),
		"exp":         s.issued.Add(s.lifetime).Unix(),
	}
	if s.noExpiry {
		delete(claims, "exp")
	}
	if s.noIssued {
		delete(claims, "iat")
	}
	if s.audience != "" {
		claims["aud"] = s.audience
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		t.Fatalf("sign a node token: %v", err)
	}
	return token
}

// nodeTarget is a service on a node of the given name, with the given network node, and its routes.
func nodeTarget(t *testing.T, name, networkNode string) (*gin.Engine, *Service) {
	t.Helper()
	r, svc := newNeutronTestRouterWithService()
	svc.SetNodes(NodeOptions{Name: name, NetworkNode: networkNode, Port: 9696, Secret: testNodeSecret})
	return r, svc
}

func postConverge(r http.Handler, token string, body []byte) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/converge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Lightstack-Node-Token", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func routerBody(id string) []byte {
	return []byte(`{"unit":"router","ids":["` + id + `"],"absent":false}`)
}

func TestNodeCallToken(t *testing.T) {
	requireDB(t)
	const node = "spec033-node-b"
	r, _ := nodeTarget(t, node, node)
	body := routerBody(uuid.NewString())
	other := routerBody(uuid.NewString())

	for _, tc := range []struct {
		name  string
		spec  func(tokenSpec) tokenSpec
		send  []byte
		wants int
	}{
		{"a node token for this node and this body", func(s tokenSpec) tokenSpec { return s }, body, http.StatusNoContent},
		{"expired", func(s tokenSpec) tokenSpec { s.issued = time.Now().Add(-time.Minute); return s }, body, http.StatusUnauthorized},
		{"living longer than ten seconds", func(s tokenSpec) tokenSpec { s.lifetime = time.Hour; return s }, body, http.StatusUnauthorized},
		// SPEC-033 T211: a token issued in the future — by a node whose clock runs fast — is not
		// valid yet; within two seconds' skew it is.
		{"issued an hour from now", func(s tokenSpec) tokenSpec { s.issued = time.Now().Add(time.Hour); return s }, body, http.StatusUnauthorized},
		{"issued a second from now", func(s tokenSpec) tokenSpec { s.issued = time.Now().Add(time.Second); return s }, body, http.StatusNoContent},
		{"without an expiry", func(s tokenSpec) tokenSpec { s.noExpiry = true; return s }, body, http.StatusUnauthorized},
		{"without an issue time", func(s tokenSpec) tokenSpec { s.noIssued = true; return s }, body, http.StatusUnauthorized},
		{"another audience", func(s tokenSpec) tokenSpec { s.audience = "lightstack"; return s }, body, http.StatusUnauthorized},
		{"no audience", func(s tokenSpec) tokenSpec { s.audience = ""; return s }, body, http.StatusUnauthorized},
		{"for another node", func(s tokenSpec) tokenSpec { s.node = "spec033-node-c"; return s }, body, http.StatusUnauthorized},
		{"for another body", func(s tokenSpec) tokenSpec { return s }, other, http.StatusUnauthorized},
		{"signed with another secret", func(s tokenSpec) tokenSpec { s.secret = []byte("another"); return s }, body, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := signNodeToken(t, tc.spec(goodToken(node, body)))
			if code, resp := postConverge(r, token, tc.send); code != tc.wants {
				t.Errorf("code=%d body=%s; want %d", code, resp, tc.wants)
			}
		})
	}

	t.Run("no token", func(t *testing.T) {
		if code, _ := postConverge(r, "", body); code != http.StatusUnauthorized {
			t.Errorf("code=%d; want 401", code)
		}
	})

	// A user's token, however privileged, is not a node token.
	t.Run("a user token", func(t *testing.T) {
		user, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"user_id": uuid.NewString(), "project_id": seededProjectID, "roles": []string{"admin"},
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		}).SignedString(testNodeSecret)
		if err != nil {
			t.Fatal(err)
		}
		if code, _ := postConverge(r, user, body); code != http.StatusUnauthorized {
			t.Errorf("code=%d; want 401", code)
		}
	})

	// And a node token is not a user token: every /v2.0 route refuses it, as the services' shared
	// authentication does.
	t.Run("a node token on a /v2.0 route", func(t *testing.T) {
		auth := keystone.NewAuthService(testPool, string(testNodeSecret), time.Hour, nil)
		engine := gin.New()
		engine.Use(middleware.AuthMiddleware(auth))
		NewService(testPool, "stub", nil).RegisterRoutes(&engine.RouterGroup)
		req := httptest.NewRequest(http.MethodGet, "/v2.0/networks", nil)
		req.Header.Set("X-Auth-Token", signNodeToken(t, goodToken(node, nil)))
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET /v2.0/networks with a node token: code=%d; want 401", w.Code)
		}
	})
}

func TestNodeCallUnitsTheTargetHolds(t *testing.T) {
	requireDB(t)
	const network, other = "spec033-node-net", "spec033-node-other"

	// Routers and DHCP live on the network node only.
	r, _ := nodeTarget(t, other, network)
	for _, unit := range []string{"router", "dhcp"} {
		body := []byte(`{"unit":"` + unit + `","ids":["` + uuid.NewString() + `"],"absent":false}`)
		if code, resp := postConverge(r, signNodeToken(t, goodToken(other, body)), body); code != http.StatusConflict {
			t.Errorf("%s on a node that is not the network node: code=%d body=%s; want 409", unit, code, resp)
		}
	}

	// A port's chains live on the node it is bound to; absence is accepted anywhere.
	r, _ = nodeTarget(t, network, network)
	netID := seedNetwork(t, "spec033-node-call-"+uuid.NewString()[:6])
	here, elsewhere := seedBarePort(t, netID, seededProjectID), seedBarePort(t, netID, seededProjectID)
	mustExec(t, "UPDATE ports SET binding_host_id = $1 WHERE id = $2", network, here)
	mustExec(t, "UPDATE ports SET binding_host_id = $1 WHERE id = $2", other, elsewhere)
	for _, tc := range []struct {
		name  string
		body  string
		wants int
	}{
		{"a port bound here", `{"unit":"ports","ids":["` + here + `"]}`, http.StatusNoContent},
		{"a port bound elsewhere", `{"unit":"ports","ids":["` + elsewhere + `"]}`, http.StatusConflict},
		{"absence of a port bound elsewhere", `{"unit":"ports","ids":[],"absent_ids":["` + elsewhere + `"]}`, http.StatusNoContent},
		{"a router on the network node", string(routerBody(uuid.NewString())), http.StatusNoContent},
		{"an unknown unit", `{"unit":"bridge","ids":["` + uuid.NewString() + `"],"absent":false}`, http.StatusBadRequest},
		{"a router without an id", `{"unit":"router","ids":[],"absent":false}`, http.StatusBadRequest},
		{"a router's records read as gone", `{"unit":"router","ids":["` + uuid.NewString() + `"],"absent_ids":["` + uuid.NewString() + `"]}`, http.StatusNoContent},
		{"a router's absence given as a word", `{"unit":"router","ids":["` + uuid.NewString() + `"],"absent":"yes"}`, http.StatusBadRequest},
		{"a router's records read as gone given as absent", `{"unit":"router","ids":["` + uuid.NewString() + `"],"absent":["` + uuid.NewString() + `"]}`, http.StatusBadRequest},
		{"a router absent whole and in part", `{"unit":"router","ids":["` + uuid.NewString() + `"],"absent":true,"absent_ids":["` + uuid.NewString() + `"]}`, http.StatusBadRequest},
		{"ports absent as a whole", `{"unit":"ports","ids":["` + here + `"],"absent":true}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if code, resp := postConverge(r, signNodeToken(t, goodToken(network, body)), body); code != tc.wants {
				t.Errorf("code=%d body=%s; want %d", code, resp, tc.wants)
			}
		})
	}
}

func TestNodeCallDeadlines(t *testing.T) {
	requireDB(t)
	const node = "spec033-node-deadline"

	// The target's lock wait and apply share one deadline, 900 ms from receipt.
	t.Run("the target gives up within 900 ms, lock wait included", func(t *testing.T) {
		r, svc := nodeTarget(t, node, node)
		id := uuid.NewString()
		release, err := svc.units.acquire(context.Background(), unitRequest{Unit: unitRouter, IDs: []string{id}}.lockKey())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		body := routerBody(id)
		start := time.Now()
		code, resp := postConverge(r, signNodeToken(t, goodToken(node, body)), body)
		elapsed := time.Since(start)
		if code != http.StatusInternalServerError {
			t.Errorf("a unit whose lock another change holds: code=%d body=%s; want 500", code, resp)
		}
		if elapsed < 800*time.Millisecond || elapsed > 1000*time.Millisecond {
			t.Errorf("answered after %s; want at the 900 ms deadline", elapsed)
		}
	})

	// The caller's bound is one second end to end, whatever the target does.
	t.Run("the caller gives up within a second", func(t *testing.T) {
		unblock := make(chan struct{})
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-unblock:
			case <-r.Context().Done():
			}
		}))
		defer target.Close()
		defer close(unblock)
		svc, name := callerTo(t, target)
		start := time.Now()
		err := newHTTPNodeCaller(svc).converge(context.Background(), name, unitRequest{Unit: unitRouter, IDs: []string{uuid.NewString()}})
		if elapsed := time.Since(start); err == nil || elapsed > 1200*time.Millisecond {
			t.Errorf("a target that never answers: err=%v after %s; want an error within about a second", err, elapsed)
		}
	})
}

// callerTo registers the test server's address as an active node and returns a service on another
// node whose node caller reaches it, and the node's name.
func callerTo(t *testing.T, target *httptest.Server) (*Service, string) {
	t.Helper()
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	name := "spec033-node-" + uuid.NewString()[:8]
	seedComputeNode(t, name, host, "active", time.Now())
	registry, err := compute.NewNodeRegistry(testPool, "", "127.0.0.1", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, svc := newNeutronTestRouterWithService()
	svc.SetNodes(NodeOptions{Name: "spec033-node-caller", NetworkNode: name, Registry: registry, Port: p, Secret: testNodeSecret})
	return svc, name
}

func TestNodeCallRequest(t *testing.T) {
	requireDB(t)

	// The body names the unit and its ids, nothing else, and the token is for that node and body.
	t.Run("what the caller sends", func(t *testing.T) {
		var got []byte
		var token string
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, _ = io.ReadAll(r.Body)
			token = r.Header.Get("X-Lightstack-Node-Token")
			if r.URL.Path != "/internal/v1/converge" || r.Method != http.MethodPost {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer target.Close()
		svc, name := callerTo(t, target)
		port := uuid.NewString()
		if err := newHTTPNodeCaller(svc).converge(context.Background(), name,
			unitRequest{Unit: unitPorts, IDs: []string{port}, AbsentIDs: []string{uuid.NewString()}}); err != nil {
			t.Fatalf("converge: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(got, &fields); err != nil {
			t.Fatalf("decode %s: %v", got, err)
		}
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"absent_ids", "ids", "unit"}) {
			t.Errorf("the body's fields: %v; want absent_ids, ids and unit only", keys)
		}
		if err := checkNodeToken(testNodeSecret, token, name, got); err != nil {
			t.Errorf("the token sent does not check for the node and body: %v", err)
		}
	})

	// SPEC-033 T220: every unit's request arrives as it was sent — absent whole as a
	// boolean, absent in part as a list of ids, each in a field of its own — decoded as the node route
	// decodes it.
	t.Run("every unit's request arrives as sent", func(t *testing.T) {
		var got unitRequest
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			got = unitRequest{}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("decode %s: %v", body, err)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		defer target.Close()
		svc, name := callerTo(t, target)
		id, other := uuid.NewString(), uuid.NewString()
		for _, req := range []unitRequest{
			{Unit: unitRouter, IDs: []string{id}},
			{Unit: unitRouter, IDs: []string{id}, Absent: true},
			{Unit: unitRouter, IDs: []string{id}, AbsentIDs: []string{other}},
			{Unit: unitDHCP, IDs: []string{id}, Absent: true},
			{Unit: unitDHCP, IDs: []string{id}, AbsentIDs: []string{other}},
			{Unit: unitPorts, IDs: []string{id}, AbsentIDs: []string{other}},
			{Unit: unitPorts},
		} {
			if err := newHTTPNodeCaller(svc).converge(context.Background(), name, req); err != nil {
				t.Fatalf("converge %+v: %v", req, err)
			}
			if got.Unit != req.Unit || !slices.Equal(got.IDs, req.IDs) || got.Absent != req.Absent || !slices.Equal(got.AbsentIDs, req.AbsentIDs) {
				t.Errorf("sent %+v; the node read %+v", req, got)
			}
		}
	})

	t.Run("any answer but 204 fails the step", func(t *testing.T) {
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
		}))
		defer target.Close()
		svc, name := callerTo(t, target)
		if err := newHTTPNodeCaller(svc).converge(context.Background(), name,
			unitRequest{Unit: unitRouter, IDs: []string{uuid.NewString()}}); err == nil {
			t.Error("a 409 answer: no error; want the step to fail")
		}
	})

	// A call to this node is a function call: it never reaches the network.
	t.Run("a call to this node", func(t *testing.T) {
		const node = "spec033-node-self"
		_, svc := nodeTarget(t, node, node)
		caller := newHTTPNodeCaller(svc)
		caller.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("a call to this node went over HTTP")
			return nil, errors.New("no network in this test")
		})
		if err := caller.converge(context.Background(), node, unitRequest{Unit: unitRouter, IDs: []string{uuid.NewString()}}); err != nil {
			t.Errorf("converge on this node: %v", err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// SPEC-033 T275 (FR-033): the node-to-node call goes straight to the node's tunnel address, never through an
// HTTP proxy the environment names for egress (HTTP_PROXY, which the chart's extraEnv may set for S3): a
// proxy would carry the node token off the tunnel network, and its 204 would count as converged.
func TestNodeCallUsesNoProxy(t *testing.T) {
	_, svc := newNeutronTestRouterWithService()
	transport, ok := newHTTPNodeCaller(svc).client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Errorf("the node call's transport %T (proxy set: %v); want an *http.Transport with no proxy", newHTTPNodeCaller(svc).client.Transport, ok && transport.Proxy != nil)
	}
}
