// Package health provides o3k's thin, honest readiness and liveness signals.
//
// Readiness is gated on the database being reachable AND all HTTP listeners
// being bound; liveness is dependency-free (process up). It also exposes an
// OpenStack/oslo-compatible /healthcheck alias of readiness. The endpoints are
// unauthenticated and served on the identity listener only. See
// docs/specs/013-readiness-liveness-probes/.
package health

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/cobaltcore-dev/o3k/internal/database"
)

// defaultCheckTimeout bounds each component check, honoring o3k's sub-second
// fail-fast principle (FR-003/SC-003): the endpoint never hangs on a wedged DB.
const defaultCheckTimeout = 800 * time.Millisecond

// Check is one named component check that gates readiness. Probe MUST return
// promptly once ctx is done — Ready relies on that for its never-hang guarantee
// (FR-003); the database check satisfies it via pgx, which honors ctx deadlines.
type Check struct {
	Name  string
	Probe func(ctx context.Context) error
}

// DatabaseCheck is the sole readiness-gating check (FR-001, Q1). It wraps the
// existing database.HealthCheck — a pgx pool Ping, i.e. a real server
// round-trip, not a TCP-only probe (Q7).
func DatabaseCheck() Check {
	return Check{
		Name:  "database",
		Probe: func(ctx context.Context) error { return database.HealthCheck(ctx) },
	}
}

// Result is the outcome of a readiness evaluation. Reason is a short, curated
// string with no error text/connection details/latencies/stack traces (Q3).
type Result struct {
	Ready  bool
	Reason string
	Failed []string
}

// Checker aggregates the readiness inputs of one o3k process. Construct with New.
type Checker struct {
	checks   []Check
	timeout  time.Duration
	logger   zerolog.Logger
	started  atomic.Bool // all HTTP listeners bound (FR-001)
	draining atomic.Bool // SIGTERM received; drain before shutdown (FR-009)

	mu        sync.Mutex // guards transition-log state
	havePrev  bool
	prevReady bool
}

// New builds a Checker over the given gating checks (typically DatabaseCheck()).
func New(logger zerolog.Logger, checks ...Check) *Checker {
	return &Checker{checks: checks, timeout: defaultCheckTimeout, logger: logger}
}

// MarkStarted records that all HTTP listeners are bound; until then readiness
// reports "starting" (FR-001).
func (c *Checker) MarkStarted() { c.started.Store(true) }

// MarkDraining flips readiness to not-ready for a graceful shutdown so the pod
// leaves the Service before listeners close (FR-009).
func (c *Checker) MarkDraining() { c.draining.Store(true) }

// Ready evaluates readiness: not-ready while draining or before all listeners
// are bound, otherwise runs every gating check concurrently, each under its own
// bounded timeout, so worst-case latency stays max(check) not sum(check) as the
// registry grows (PR #70 review). Never hangs. Handlers pass context.Background()
// (not the request context) so a client disconnect or a client-side probe
// timeout can't cancel the DB round-trip and be misreported as a failure — the
// per-check timeout is the only deadline.
func (c *Checker) Ready(ctx context.Context) Result {
	switch {
	case c.draining.Load():
		return c.record(Result{Reason: "draining"})
	case !c.started.Load():
		return c.record(Result{Reason: "starting"})
	}

	// Run the gating checks (today just the database) concurrently, each under
	// its own bounded context — a Check.Probe must return when ctx is done.
	var (
		mu     sync.Mutex
		failed []string
		wg     sync.WaitGroup
	)
	for _, chk := range c.checks {
		wg.Add(1)
		go func(chk Check) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()
			if err := chk.Probe(cctx); err != nil {
				mu.Lock()
				failed = append(failed, chk.Name)
				mu.Unlock()
			}
		}(chk)
	}
	wg.Wait()
	if len(failed) == 0 {
		return c.record(Result{Ready: true})
	}
	sort.Strings(failed)
	return c.record(Result{Reason: strings.Join(failed, ",") + " unavailable", Failed: failed})
}

// record logs a structured line on each ready<->not-ready transition (FR-015)
// and returns r unchanged.
func (c *Checker) record(r Result) Result {
	c.mu.Lock()
	changed := !c.havePrev || c.prevReady != r.Ready
	c.havePrev, c.prevReady = true, r.Ready
	c.mu.Unlock()
	if changed {
		reason := r.Reason
		if reason == "" {
			reason = "ok"
		}
		evt := c.logger.Info()
		if !r.Ready {
			evt = c.logger.Warn()
		}
		evt.Bool("ready", r.Ready).Str("reason", reason).Msg("readiness transition")
	}
	return r
}

// Health endpoint paths. Exported as Paths so callers (e.g. the logging
// middleware, the `o3k healthcheck` CLI) reference one list instead of
// duplicating the literals and silently drifting (PR #70 review) — change a
// path here and every consumer follows.
const (
	LivezPath       = "/livez"
	ReadyzPath      = "/readyz"
	HealthcheckPath = "/healthcheck"
)

// Paths are the unauthenticated health routes Register mounts.
var Paths = []string{LivezPath, ReadyzPath, HealthcheckPath}

// Register mounts the unauthenticated health routes on r (the identity engine,
// beside the root version route). /healthcheck answers GET and HEAD (FR-014).
func (c *Checker) Register(r gin.IRouter) {
	r.GET(LivezPath, c.Livez)
	r.GET(ReadyzPath, c.Readyz)
	r.GET(HealthcheckPath, c.Healthcheck)
	r.HEAD(HealthcheckPath, c.Healthcheck)
}

// Livez handles GET /livez: 200 while the process is up, no dependency calls.
func (c *Checker) Livez(ctx *gin.Context) { ctx.String(http.StatusOK, "ok") }

// Readyz handles GET /readyz: 200 when serving, 503 + short reason otherwise.
// A ?verbose query lists each check's status (names only).
func (c *Checker) Readyz(ctx *gin.Context) {
	r := c.Ready(context.Background())
	status := http.StatusOK
	if !r.Ready {
		status = http.StatusServiceUnavailable
	}
	if _, ok := ctx.GetQuery("verbose"); ok {
		ctx.String(status, c.verboseBody(r))
		return
	}
	if r.Ready {
		ctx.String(status, "ok")
		return
	}
	ctx.String(status, r.Reason)
}

// Healthcheck handles GET|HEAD /healthcheck: an OpenStack/oslo-compatible alias
// of readiness, content-negotiating text/plain (default) and JSON by Accept.
func (c *Checker) Healthcheck(ctx *gin.Context) {
	r := c.Ready(context.Background())
	status := http.StatusOK
	if !r.Ready {
		status = http.StatusServiceUnavailable
	}
	if ctx.Request.Method == http.MethodHead {
		ctx.Status(status)
		return
	}
	reasons := []string{"OK"}
	if !r.Ready {
		reasons = []string{r.Reason}
	}
	switch ctx.NegotiateFormat(gin.MIMEPlain, gin.MIMEJSON) {
	case gin.MIMEJSON:
		ctx.JSON(status, gin.H{"detailed": false, "reasons": reasons})
	default:
		ctx.String(status, reasons[0])
	}
}

// verboseBody renders one line per check/state (names only — no sensitive data).
func (c *Checker) verboseBody(r Result) string {
	var b strings.Builder
	switch {
	case r.Ready:
		for _, chk := range c.checks {
			fmt.Fprintf(&b, "[ok] %s\n", chk.Name)
		}
	case len(r.Failed) == 0: // a lifecycle state (starting / draining), not a check failure
		fmt.Fprintf(&b, "[not-ready] %s\n", r.Reason)
	default:
		for _, name := range r.Failed {
			fmt.Fprintf(&b, "[failed] %s\n", name)
		}
	}
	return b.String()
}
