package glance

import (
	"strings"
	"testing"
)

// GetImage's cache was keyed on the image id alone, and the lookup runs BEFORE the
// `visibility = 'public' OR project_id = $2` predicate that the database path applies. So once an
// owner had fetched a private image, any authenticated caller that knew its id got 200 with the
// full object — name, status, size and checksum — for the whole TTL, where the uncached answer is
// 404. Raised on PR #79 against the neutron twin of this code; the same shape was here.
//
// Like the neutron test, this pins the key rather than end-to-end isolation: pkg/cache talks to a
// real Redis and there is none in CI. The isolation follows from the key — a caller can only read
// an entry it could have written.
func TestImageCacheKeyIsScopedToTheCaller(t *testing.T) {
	const imageID = "7c0e1d2b-0000-4000-8000-000000000001"
	const projectA = "00000000-0000-0000-0000-000000000002"
	const projectB = "00000000-0000-0000-0000-0000000b0b0b"

	keyA := imageCacheKey(projectA, imageID)
	keyB := imageCacheKey(projectB, imageID)

	if keyA == keyB {
		t.Fatalf("imageCacheKey is the same for two projects (%q); one tenant's cached private image "+
			"would be served to another, because the cache is read before any visibility check", keyA)
	}
	if keyA != imageCacheKey(projectA, imageID) {
		t.Errorf("imageCacheKey is not stable for one caller; the entry could never be hit")
	}
	if imageCacheKey(projectA, imageID) == imageCacheKey(projectA, "7c0e1d2b-0000-4000-8000-000000000002") {
		t.Errorf("imageCacheKey collides across image ids")
	}
	if !strings.Contains(keyA, projectA) {
		t.Errorf("imageCacheKey(%q, …) = %q, which does not contain the caller's project", projectA, keyA)
	}
}

// DeleteImage evicts with a pattern because entries are per caller; it must cover every key the
// writer could have produced, or other projects keep serving the deleted image until the TTL.
func TestImageCacheInvalidationPatternCoversEveryCaller(t *testing.T) {
	const imageID = "7c0e1d2b-0000-4000-8000-000000000001"
	const pattern = "image:*:" + imageID

	for _, project := range []string{
		"00000000-0000-0000-0000-000000000002",
		"00000000-0000-0000-0000-0000000b0b0b",
	} {
		key := imageCacheKey(project, imageID)
		if !matchesRedisGlob(pattern, key) {
			t.Errorf("DeleteImage evicts %q, which does not match %q written for project %s — "+
				"that project keeps serving the deleted image until the TTL expires",
				pattern, key, project)
		}
	}
}

// matchesRedisGlob implements the only glob shape used here: a single `*` standing for one key
// segment. The length guard is load-bearing — without it an unscoped key `image:<id>` satisfies
// both HasPrefix("image:") and HasSuffix(":<id>") by overlapping on the same colon, and the check
// passes on exactly the layout it exists to reject.
func matchesRedisGlob(pattern, key string) bool {
	star := strings.Index(pattern, "*")
	if star < 0 {
		return pattern == key
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	return strings.HasPrefix(key, prefix) && strings.HasSuffix(key, suffix) &&
		len(key) >= len(prefix)+len(suffix)
}
