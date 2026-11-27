package networking

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// fakeRunner records the commands a RouterManager runs and answers them through respond.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []fakeCall
	respond func(ctx context.Context, c fakeCall) ([]byte, error)
}

type fakeCall struct {
	name        string
	args        []string
	stdin       string
	hasDeadline bool
	budget      time.Duration // time left until the deadline when the command started
}

func (f *fakeRunner) run(ctx context.Context, name string, args []string, stdin string) ([]byte, error) {
	c := fakeCall{name: name, args: slices.Clone(args), stdin: stdin}
	if d, ok := ctx.Deadline(); ok {
		c.hasDeadline, c.budget = true, time.Until(d)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.respond == nil {
		return nil, nil
	}
	return f.respond(ctx, c)
}

func (f *fakeRunner) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.name+" "+strings.Join(c.args, " "))
	}
	return out
}

func realModeRouter(f *fakeRunner) *RouterManager {
	rm := NewRouterManager("iptables")
	rm.run = f.run
	rm.enableForwarding = func() error { return nil }
	return rm
}
