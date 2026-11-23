package metadata

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

// SPEC-033 round 2, T124 (FR-037; research R29): the listeners the DHCP unit opens and closes, one per
// network in its DHCP namespace on 169.254.169.254:80, serving the handler with the network. The
// namespace is the test's: listen and namespaceID are replaced, the socket is loopback's.

// fakeNamespaces stands in for the node's namespaces: each name's identity, and every listen asked of
// it.
type fakeNamespaces struct {
	mu     sync.Mutex
	ids    map[string]string
	listen []string
	fail   error
}

func (f *fakeNamespaces) id(ns string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.ids[ns]
	if !ok {
		return "", errors.New("no namespace " + ns)
	}
	return id, nil
}

func (f *fakeNamespaces) listenIn(ns, addr string) (net.Listener, error) {
	f.mu.Lock()
	f.listen = append(f.listen, ns+" "+addr)
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func (f *fakeNamespaces) listens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.listen)
}

// testListeners serves a handler that answers with the request's network.
func testListeners(f *fakeNamespaces) *Listeners {
	l := NewListeners(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, requestNetwork(r.Context()))
	}))
	l.listen, l.namespaceID = f.listenIn, f.id
	return l
}

// served asks the network's listener for the network it serves.
func served(t *testing.T, l *Listeners, network string) string {
	t.Helper()
	addr, ok := listenerAddress(l, network)
	if !ok {
		t.Fatalf("no listener for network %s", network)
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("ask the listener of network %s: %v", network, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// listenerAddress is where a network's listener listens.
func listenerAddress(l *Listeners, network string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.open[network]
	if !ok {
		return "", false
	}
	return cur.listener.Addr().String(), true
}

func refuses(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
	}
	return err != nil
}

func TestListenersServeEachNetworkInItsNamespace(t *testing.T) {
	f := &fakeNamespaces{ids: map[string]string{"qdhcp-n1": "ns-a", "qdhcp-n2": "ns-c"}}
	l := testListeners(f)
	ctx := context.Background()
	t.Cleanup(func() { _ = l.Close("n1"); _ = l.Close("n2") })

	if err := l.Open(ctx, "n1"); err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, want := f.listens(), []string{"qdhcp-n1 169.254.169.254:80"}; !slices.Equal(got, want) {
		t.Errorf("listened %q; want %q", got, want)
	}
	if got := served(t, l, "n1"); got != "n1" {
		t.Errorf("the listener serves network %q; want n1", got)
	}

	if err := l.Open(ctx, "n1"); err != nil || len(f.listens()) != 1 {
		t.Errorf("a second open: %v, %d listens; want the listener kept", err, len(f.listens()))
	}

	if err := l.Open(ctx, "n2"); err != nil || served(t, l, "n2") != "n2" || served(t, l, "n1") != "n1" {
		t.Errorf("a second network: %v; want each network served by its own listener", err)
	}
	// The reconciliation closes the listeners of networks without DHCP among those served (T142).
	if got := l.Serving(); !slices.Equal(got, []string{"n1", "n2"}) {
		t.Errorf("serving %q; want n1 and n2", got)
	}

	// A namespace deleted and made again holds no listener: the old one is closed, a new one opened.
	old, _ := listenerAddress(l, "n1")
	f.mu.Lock()
	f.ids["qdhcp-n1"] = "ns-b"
	f.mu.Unlock()
	if err := l.Open(ctx, "n1"); err != nil || len(f.listens()) != 3 {
		t.Fatalf("open in a replaced namespace: %v, %d listens; want a new listener", err, len(f.listens()))
	}
	if !refuses(old) {
		t.Errorf("the listener in the replaced namespace still answers at %s", old)
	}
	if served(t, l, "n1") != "n1" {
		t.Error("the new listener does not serve the network")
	}

	addr, _ := listenerAddress(l, "n1")
	if err := l.Close("n1"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !refuses(addr) {
		t.Errorf("a closed listener still answers at %s", addr)
	}
	if err := l.Close("n1"); err != nil {
		t.Errorf("a second close: %v; want none", err)
	}
	if got := l.Serving(); !slices.Equal(got, []string{"n2"}) {
		t.Errorf("serving %q after closing n1; want n2", got)
	}
}

func TestListenersOpenFailures(t *testing.T) {
	ctx := context.Background()

	f := &fakeNamespaces{ids: map[string]string{}}
	if err := testListeners(f).Open(ctx, "n1"); err == nil || len(f.listens()) != 0 {
		t.Errorf("no namespace: %v, %d listens; want an error and nothing opened", err, len(f.listens()))
	}

	f = &fakeNamespaces{ids: map[string]string{"qdhcp-n1": "ns-a"}, fail: errors.New("bind: cannot assign requested address")}
	l := testListeners(f)
	if err := l.Open(ctx, "n1"); err == nil {
		t.Error("a failing listen: no error")
	}
	if _, ok := listenerAddress(l, "n1"); ok {
		t.Error("a failing listen left a listener recorded")
	}
}
