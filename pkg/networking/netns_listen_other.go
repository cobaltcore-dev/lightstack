//go:build !linux

package networking

import (
	"errors"
	"net"
)

// errNoNamespaces: network namespaces are Linux's; elsewhere only stub mode runs.
var errNoNamespaces = errors.New("network namespaces need Linux")

// ListenInNamespace needs Linux.
func ListenInNamespace(string, string) (net.Listener, error) { return nil, errNoNamespaces }

// NamespaceID needs Linux.
func NamespaceID(string) (string, error) { return "", errNoNamespaces }
