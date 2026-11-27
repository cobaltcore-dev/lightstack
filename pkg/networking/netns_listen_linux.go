//go:build linux

package networking

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// netnsDir is where named network namespaces live, as `ip netns` keeps them.
const netnsDir = "/run/netns"

// ListenInNamespace opens a TCP listener on addr inside the named network namespace (SPEC-033 R29): a
// metadata listener in a network's DHCP namespace. The socket is made on an OS thread that enters the
// namespace and leaves it again, and stays in the namespace. A thread that cannot leave is never handed
// back to the runtime: its goroutine ends locked to it, and the runtime ends the thread with it.
func ListenInNamespace(ns, addr string) (net.Listener, error) {
	type result struct {
		ln  net.Listener
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		ln, back, err := listenOnThreadIn(ns, addr)
		if back {
			runtime.UnlockOSThread()
		}
		done <- result{ln, err}
	}()
	r := <-done
	return r.ln, r.err
}

// listenOnThreadIn enters ns on the locked thread, listens on addr and leaves again. back reports
// whether the thread is in its own namespace afterwards.
func listenOnThreadIn(ns, addr string) (ln net.Listener, back bool, err error) {
	origin, err := os.Open("/proc/thread-self/ns/net")
	if err != nil {
		return nil, true, fmt.Errorf("open this thread's network namespace: %w", err)
	}
	defer func() { _ = origin.Close() }()
	target, err := os.Open(filepath.Join(netnsDir, ns))
	if err != nil {
		return nil, true, fmt.Errorf("open network namespace %s: %w", ns, err)
	}
	defer func() { _ = target.Close() }()
	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		return nil, true, fmt.Errorf("enter network namespace %s: %w", ns, err)
	}
	ln, listenErr := net.Listen("tcp", addr)
	if err := unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET); err != nil {
		if ln != nil {
			_ = ln.Close()
		}
		return nil, false, fmt.Errorf("leave network namespace %s: %w", ns, err)
	}
	if listenErr != nil {
		return nil, true, fmt.Errorf("listen on %s in network namespace %s: %w", addr, ns, listenErr)
	}
	return ln, true, nil
}

// NamespaceID identifies the named network namespace as it is now, by its device and inode: one
// deleted and made again under the same name has another.
func NamespaceID(ns string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(netnsDir, ns), &st); err != nil {
		return "", fmt.Errorf("network namespace %s: %w", ns, err)
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}
