package metadata

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/rs/zerolog/log"
)

// Listeners serve the metadata service in networks' DHCP namespaces on the network node (SPEC-033
// R29): one listener per network on 169.254.169.254:80 inside qdhcp-<network>, serving the handler
// through ServeNetwork, so a guest is identified by its network and its address. They are Neutron's
// MetadataListeners: the DHCP unit opens a network's with its DHCP and closes it before the namespace
// goes.
type Listeners struct {
	handler http.Handler
	mu      sync.Mutex
	open    map[string]*nsListener

	// listen and namespaceID reach the node's namespaces; tests replace them.
	listen      func(ns, addr string) (net.Listener, error)
	namespaceID func(ns string) (string, error)
}

// nsListener is a network's listener, and the identity of the namespace it was opened in.
type nsListener struct {
	server    *http.Server
	listener  net.Listener
	namespace string
}

// NewListeners makes the listeners that serve handler, the metadata engine.
func NewListeners(handler http.Handler) *Listeners {
	return &Listeners{handler: handler, open: map[string]*nsListener{},
		listen: networking.ListenInNamespace, namespaceID: networking.NamespaceID}
}

// metadataAddress is where a guest asks for its metadata.
var metadataAddress = net.JoinHostPort(networking.MetadataIP, "80")

// Open serves a network's DHCP namespace. A listener already serving it is kept; one left in a
// namespace that was deleted and made again since — by a teardown and the next reconciliation, say —
// serves nobody, and is closed and replaced.
func (l *Listeners) Open(_ context.Context, networkID string) error {
	ns := networking.DHCPNamespace(networkID)
	id, err := l.namespaceID(ns)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.open[networkID]; ok {
		if cur.namespace == id {
			return nil
		}
		_ = cur.server.Close()
		delete(l.open, networkID)
	}
	ln, err := l.listen(ns, metadataAddress)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           ServeNetwork(l.handler, networkID),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      common.HTTPWriteTimeout,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn().Err(err).Str("network_id", networkID).Msg("a network's metadata listener stopped; the next reconciliation opens it again")
		}
	}()
	l.open[networkID] = &nsListener{server: server, listener: ln, namespace: id}
	return nil
}

// Serving lists the networks served, in order.
func (l *Listeners) Serving() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(maps.Keys(l.open))
}

// Close stops serving a network's DHCP namespace. A network not served is not an error.
func (l *Listeners) Close(networkID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.open[networkID]
	if !ok {
		return nil
	}
	delete(l.open, networkID)
	return cur.server.Close()
}
