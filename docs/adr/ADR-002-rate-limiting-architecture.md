# ADR-002: Rate Limiting Architecture

**Status**: Accepted
**Date**: 2026-04-20
**Deciders**: Lightstack Core Team
**Spec**: [SPEC-006](../specs/006-v070-foundation/README.md)

## Context

Lightstack has no rate limiting (finding H-6 from April 2026 codebase review). This exposes two attack surfaces:

1. **Unauthenticated abuse**: `POST /v3/auth/tokens` can be brute-forced or used to exhaust the database connection pool. No `project_id` is available for this endpoint.
2. **Authenticated abuse**: Any authenticated client can send unlimited API requests, exhausting server resources for other tenants.

The `common.NewRateLimitError()` constructor exists but has no middleware to trigger it. lightstack's architecture is single-binary with no Redis, so rate limiting must be in-process.

## Decision

**Two-tier rate limiting using `golang.org/x/time/rate.Limiter`.**

### Architecture

```
  REQUEST
    |
    v
  Authenticated?
   / \
 NO   YES
  |     |
  v     v
 IP-based        Project-based
 rate.Limiter    rate.Limiter
  |               |
  v               v
 sync.Map         sync.Map
 [IP -> limiter]  [project_id -> limiter]
  |               |
  +-- evict stale entries every 10 minutes
  +-- cap: 10,000 entries max
  +-- overflow: allow traffic (degrade open)
```

### Configuration

```yaml
rate_limiting:
  enabled: true
  auth_rate: 20        # requests/minute per IP (unauthenticated)
  api_rate: 100        # requests/minute per project (authenticated)
  eviction_interval: 10m
  max_entries: 10000
  overflow_policy: allow  # "allow" (degrade open) or "deny" (degrade closed)
```

### Behavior

- **Under limit**: Request passes through with zero overhead (one `sync.Map` lookup + `rate.Limiter.Allow()`)
- **At limit**: Returns HTTP 429 with `Retry-After` header and OpenStack-compatible error body via `common.NewRateLimitError()`
- **Eviction**: Background goroutine runs every 10 minutes, removes entries not accessed since last eviction
- **Overflow** (>10K entries): Stops tracking new IPs/projects, allows their traffic, logs a warning. Existing tracked entries continue to be limited.
- **Memory ceiling**: ~120 bytes per `rate.Limiter` x 10K max = ~1.2MB worst case

### Test Plan

12 unit tests + 1 integration test:
1. IP limiter: request under limit passes
2. IP limiter: request at limit returns 429
3. IP limiter: 429 includes Retry-After header
4. IP limiter: different IPs have independent limits
5. IP limiter: X-Forwarded-For extraction
6. IP limiter: fallback to RemoteAddr
7. Project limiter: request under limit passes
8. Project limiter: request at limit returns 429
9. Project limiter: different projects have independent limits
10. Eviction: stale entries cleaned after timeout
11. Cap: 10K+ entries triggers graceful degradation
12. Integration: rapid POST /v3/auth/tokens returns 429

## Alternatives Considered

### Custom token bucket implementation
Build a token bucket from scratch with `sync.Mutex` and manual refill logic.

**Rejected because**: `golang.org/x/time/rate` is the Go standard library rate limiter. It handles burst capacity, refill timing, and concurrent access correctly. Building a custom implementation would duplicate proven code and introduce concurrency risk.

### Redis-backed rate limiting
Store rate limit state in Redis for cross-instance coordination.

**Rejected because**: lightstack's architecture explicitly avoids Redis (single-binary, no external dependencies beyond PostgreSQL). In-process limiting is sufficient for single-binary deployment. If multi-instance deployment is added later (Phase 4: HA), Redis can be introduced then.

### Per-endpoint rate limiting
Configure different limits for each API endpoint.

**Rejected because**: Premature granularity. Two tiers (auth vs authenticated) cover 95% of the threat model. Per-endpoint configuration adds complexity without demonstrated need. Can be added later if specific endpoints prove problematic.

## Consequences

### Positive
- Auth endpoints protected from brute-force attacks
- Per-project fairness prevents single tenant from exhausting resources
- Standard library implementation (no custom concurrency bugs)
- Configurable via `lightstack.yaml` (can disable for development)
- OpenStack-compatible 429 response format

### Negative
- Adds `golang.org/x/time` dependency
- `sync.Map` does not shrink after deletes (Go runtime behavior), but 1.2MB ceiling is acceptable
- In-process only, does not coordinate across multiple Lightstack instances (acceptable for current single-binary architecture)
- Eviction goroutine adds one more background goroutine to manage during shutdown

### Neutral
- Rate limiter disabled by default (no impact on existing deployments)
- No database schema changes
- No API behavior changes for well-behaved clients
