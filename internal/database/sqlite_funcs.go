package database

import (
	"database/sql/driver"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"modernc.org/sqlite"
)

// Functions registered on every SQLite connection (research R6, FR-015). The driver
// applies a registration to every connection opened afterwards, so registering at
// package initialisation covers the pool, the migrator and every test database.
func init() {
	sqlite.MustRegisterScalarFunction("now", 0, sqlNow)
	sqlite.MustRegisterScalarFunction("gen_random_uuid", 0, sqlRandomUUID)
	sqlite.MustRegisterDeterministicScalarFunction("ls_add_seconds", 2, sqlAddSeconds)
	sqlite.MustRegisterDeterministicScalarFunction("regexp", 2, sqlRegexp)
	// Advisory locks do nothing on SQLite: immediate transactions already serialise
	// every writer. Registering them lets the statements run unchanged.
	sqlite.MustRegisterScalarFunction("pg_advisory_xact_lock", 1, sqlNoop)
	sqlite.MustRegisterDeterministicScalarFunction("hashtext", 1, sqlZero)
}

// sqlNow returns the current time in the storage format. Unlike PostgreSQL's now(),
// which is the transaction's start, it is the time of the call; nothing relies on two
// calls returning the same value.
func sqlNow(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
	return formatTime(time.Now()), nil
}

func sqlRandomUUID(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
	return uuid.NewString(), nil
}

// sqlAddSeconds is ls_add_seconds(ts, secs): the console-token expiry arithmetic
// (now() + make_interval(secs => $n)) with the database clock.
func sqlAddSeconds(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	ts, ok := args[0].(string)
	if !ok {
		if args[0] == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("ls_add_seconds: timestamp is %T, not text", args[0])
	}
	t, err := parseTime(ts)
	if err != nil {
		return nil, err
	}
	var secs float64
	switch v := args[1].(type) {
	case int64:
		secs = float64(v)
	case float64:
		secs = v
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("ls_add_seconds: seconds is %T, not a number", args[1])
	}
	return formatTime(t.Add(time.Duration(secs * float64(time.Second)))), nil
}

// regexpCacheCap bounds the compiled-pattern cache. Patterns come from the API — the
// `name` server filter is matched by the database (`name ~ $n` → REGEXP) — so an
// unbounded cache is a tenant-reachable memory leak; the cache is cleared when it is
// full, as translateCache is. The cap is small because a pattern recompiles in
// microseconds and the working set (Horizon's search box, Terraform name lookups) is
// tiny.
const regexpCacheCap = 256

var (
	regexpMu    sync.Mutex
	regexpCache = map[string]*regexp.Regexp{}
)

// cachedRegexp compiles pattern, caching the result within regexpCacheCap.
func cachedRegexp(pattern string) (*regexp.Regexp, error) {
	regexpMu.Lock()
	re, ok := regexpCache[pattern]
	regexpMu.Unlock()
	if ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regular expression %q: %w", pattern, err)
	}
	regexpMu.Lock()
	if len(regexpCache) >= regexpCacheCap {
		regexpCache = map[string]*regexp.Regexp{}
	}
	regexpCache[pattern] = re
	regexpMu.Unlock()
	return re, nil
}

func regexpCacheLen() int {
	regexpMu.Lock()
	defer regexpMu.Unlock()
	return len(regexpCache)
}

// sqlRegexp implements `value REGEXP pattern`, which SQLite calls as
// regexp(pattern, value), with the application's regular expressions.
func sqlRegexp(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	pattern, ok1 := args[0].(string)
	value, ok2 := args[1].(string)
	if !ok1 || !ok2 {
		return nil, nil // NULL operand: the comparison is NULL, as in PostgreSQL
	}
	re, err := cachedRegexp(pattern)
	if err != nil {
		return nil, err
	}
	if re.MatchString(value) {
		return int64(1), nil
	}
	return int64(0), nil
}

func sqlNoop(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { return nil, nil }

func sqlZero(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { return int64(0), nil }
