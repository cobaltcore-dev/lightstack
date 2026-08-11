package neutron

import (
	"strings"
	"testing"
)

// GetNetwork's cache was keyed on the network id alone, and the lookup runs BEFORE the scoping
// predicate that the database path applies. So once an owner had fetched a private network, any
// authenticated caller that knew its id got 200 with the full object for the whole TTL, where the
// uncached answer is 404 (branch review 2026-08-13, SRC-1).
//
// Making the cached body caller-independent — which #546 did, by reporting the row's owner rather
// than the caller's — fixed the CONTENT but not the authorization: a caller-independent body served
// to a caller who may not see it is still a disclosure, and now it names the real owner too.
//
// WHAT THIS TEST DOES AND DOES NOT PROVE. It pins the key, not the end-to-end isolation: pkg/cache
// talks to a real Redis and there is none in CI, so an end-to-end test would either add a dependency
// or skip forever — a check that cannot fail, which is the pattern this branch's review objected to
// twice. The isolation follows from the key: a caller can only read an entry it could have written.
// The regression this guards is someone dropping the project from the key.
func TestNetworkCacheKeyIsScopedToTheCaller(t *testing.T) {
	const netID = "9f1c2f3a-0000-4000-8000-000000000001"
	const projectA = "00000000-0000-0000-0000-000000000002"
	const projectB = "00000000-0000-0000-0000-0000000b0b0b"

	keyA := networkCacheKey(projectA, netID)
	keyB := networkCacheKey(projectB, netID)

	if keyA == keyB {
		t.Fatalf("networkCacheKey is the same for two projects (%q); one tenant's cached view would be "+
			"served to another, because the cache is read before any visibility check", keyA)
	}
	if keyA != networkCacheKey(projectA, netID) {
		t.Errorf("networkCacheKey is not stable for one caller; the entry could never be hit")
	}

	// A different network for the same caller must also differ, or one network's body would be
	// served for another's id.
	if networkCacheKey(projectA, netID) == networkCacheKey(projectA, "9f1c2f3a-0000-4000-8000-000000000002") {
		t.Errorf("networkCacheKey collides across network ids")
	}

	// The caller has to actually be in the key, not merely influence it via some digest that a
	// later refactor could drop without any test noticing.
	if !strings.Contains(keyA, projectA) {
		t.Errorf("networkCacheKey(%q, …) = %q, which does not contain the caller's project", projectA, keyA)
	}
}

// Entries are per caller, so DeleteNetwork and UpdateNetwork must evict every project's view of
// the network, not only the writer's. The eviction is a pattern; this pins that the pattern
// actually covers a key the writer could have produced.
func TestNetworkCacheInvalidationPatternCoversEveryCaller(t *testing.T) {
	const netID = "9f1c2f3a-0000-4000-8000-000000000001"
	const pattern = "network:*:" + netID

	for _, project := range []string{
		"00000000-0000-0000-0000-000000000002",
		"00000000-0000-0000-0000-0000000b0b0b",
	} {
		key := networkCacheKey(project, netID)
		if !matchesRedisGlob(pattern, key) {
			t.Errorf("the writers evict %q, which does not match %q written for project %s — "+
				"that project keeps serving the stale network until the TTL expires",
				pattern, key, project)
		}
	}
}

// matchesRedisGlob implements the only glob shape used here: a single `*` standing for one
// key segment. Redis SCAN MATCH is wider than this, but a narrower matcher cannot pass a
// pattern Redis would reject.
func matchesRedisGlob(pattern, key string) bool {
	star := strings.Index(pattern, "*")
	if star < 0 {
		return pattern == key
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	return strings.HasPrefix(key, prefix) && strings.HasSuffix(key, suffix) &&
		len(key) >= len(prefix)+len(suffix)
}
