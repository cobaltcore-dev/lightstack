package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

var (
	// ErrCacheMiss is returned when a key is not found in the cache
	ErrCacheMiss = errors.New("cache miss")
	// ErrCacheDisabled is returned when cache is not initialized
	ErrCacheDisabled = errors.New("cache disabled")
)

// Cache provides Redis-backed caching with TTL support
type Cache struct {
	client     *redis.Client
	enabled    bool
	prefix     string        // Key prefix for namespacing
	defaultTTL time.Duration // Default TTL applied when 0 is passed to Set
}

// Config holds cache configuration
type Config struct {
	RedisURL   string
	Enabled    bool
	KeyPrefix  string
	DefaultTTL time.Duration
}

// NewCache creates a new cache instance
func NewCache(config Config) (*Cache, error) {
	if !config.Enabled {
		return &Cache{enabled: false}, nil
	}

	opt, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
	}

	client := redis.NewClient(opt)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &Cache{
		client:     client,
		enabled:    true,
		prefix:     config.KeyPrefix,
		defaultTTL: config.DefaultTTL,
	}, nil
}

// Get retrieves a cached value and unmarshals it into dest
func (c *Cache) Get(ctx context.Context, key string, dest any) error {
	if !c.enabled {
		return ErrCacheDisabled
	}

	fullKey := c.prefix + key
	val, err := c.client.Get(ctx, fullKey).Result()
	if err == redis.Nil {
		return ErrCacheMiss
	}
	if err != nil {
		return fmt.Errorf("failed to get from cache: %w", err)
	}

	if err := json.Unmarshal([]byte(val), dest); err != nil {
		return fmt.Errorf("failed to unmarshal cached value: %w", err)
	}

	return nil
}

// Set stores a value in cache with TTL. If ttl is 0, defaultTTL is used.
func (c *Cache) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	if !c.enabled {
		return nil // Silently skip if disabled
	}

	if ttl == 0 {
		ttl = c.defaultTTL
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to marshal value: %w", err)
	}

	fullKey := c.prefix + key
	if err := c.client.Set(ctx, fullKey, data, ttl).Err(); err != nil {
		return fmt.Errorf("failed to set cache: %w", err)
	}

	return nil
}

// Delete removes a value from cache
func (c *Cache) Delete(ctx context.Context, key string) error {
	if !c.enabled {
		return nil
	}

	fullKey := c.prefix + key
	if err := c.client.Del(ctx, fullKey).Err(); err != nil {
		return fmt.Errorf("failed to delete from cache: %w", err)
	}

	return nil
}

// DeletePattern removes all keys matching a pattern
func (c *Cache) DeletePattern(ctx context.Context, pattern string) error {
	if !c.enabled {
		return nil
	}

	fullPattern := c.prefix + pattern
	iter := c.client.Scan(ctx, 0, fullPattern, 0).Iterator()

	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}

	if err := iter.Err(); err != nil {
		return fmt.Errorf("failed to scan keys: %w", err)
	}

	if len(keys) > 0 {
		if err := c.client.Del(ctx, keys...).Err(); err != nil {
			return fmt.Errorf("failed to delete keys: %w", err)
		}
	}

	return nil
}

// TrySet writes a cache entry best-effort. It logs a warning on failure
// but never returns an error. Use this where cache writes are an
// optimization, not a correctness boundary — failures degrade
// performance but never break correctness because the underlying
// database remains the source of truth.
func (c *Cache) TrySet(ctx context.Context, key string, value any, ttl time.Duration) {
	if err := c.Set(ctx, key, value, ttl); err != nil {
		log.Warn().Err(err).Str("key", key).Msg("cache set failed")
	}
}

// TryDelete removes a cache entry best-effort. It logs a warning on
// failure but never returns an error. Stale cache entries from a
// failed delete will be invalidated again the next time the source is
// updated.
func (c *Cache) TryDelete(ctx context.Context, key string) {
	if err := c.Delete(ctx, key); err != nil {
		log.Warn().Err(err).Str("key", key).Msg("cache delete failed")
	}
}

// TryDeletePattern removes cache entries matching the pattern
// best-effort. It logs a warning on failure but never returns an error.
func (c *Cache) TryDeletePattern(ctx context.Context, pattern string) {
	if err := c.DeletePattern(ctx, pattern); err != nil {
		log.Warn().Err(err).Str("pattern", pattern).Msg("cache delete-pattern failed")
	}
}

// Exists checks if a key exists in cache
func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	if !c.enabled {
		return false, ErrCacheDisabled
	}

	fullKey := c.prefix + key
	count, err := c.client.Exists(ctx, fullKey).Result()
	if err != nil {
		return false, fmt.Errorf("failed to check existence: %w", err)
	}

	return count > 0, nil
}

// GetTTL returns the remaining TTL for a key
func (c *Cache) GetTTL(ctx context.Context, key string) (time.Duration, error) {
	if !c.enabled {
		return 0, ErrCacheDisabled
	}

	fullKey := c.prefix + key
	ttl, err := c.client.TTL(ctx, fullKey).Result()
	if err != nil {
		return 0, fmt.Errorf("failed to get TTL: %w", err)
	}

	return ttl, nil
}

// FlushAll clears all keys in this cache's namespace (not the entire Redis instance).
func (c *Cache) FlushAll(ctx context.Context) error {
	if !c.enabled {
		return nil
	}

	return c.DeletePattern(ctx, "*")
}

// Close closes the cache connection
func (c *Cache) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

// Stats returns cache statistics
type Stats struct {
	Enabled    bool
	Connected  bool
	KeyCount   int64
	MemoryUsed string
	HitRate    float64
	MissRate   float64
}

// GetStats returns current cache statistics
func (c *Cache) GetStats(ctx context.Context) (*Stats, error) {
	stats := &Stats{
		Enabled: c.enabled,
	}

	if !c.enabled {
		return stats, nil
	}

	// Test connection
	if err := c.client.Ping(ctx).Err(); err == nil {
		stats.Connected = true
	}

	// Get key count (approximate)
	dbSize, err := c.client.DBSize(ctx).Result()
	if err == nil {
		stats.KeyCount = dbSize
	}

	// Get memory usage
	_, err = c.client.Info(ctx, "memory").Result()
	if err == nil {
		// Parse used_memory from info string
		// Format: "used_memory:1234567\r\n..."
		// Simplified - would need proper parsing in production
		stats.MemoryUsed = "available via INFO MEMORY"
	}

	// Get hit/miss rates
	_, err = c.client.Info(ctx, "stats").Result()
	if err == nil {
		// Parse keyspace_hits and keyspace_misses
		// Format: "keyspace_hits:123\r\nkeyspace_misses:456\r\n..."
		// Simplified - would need proper parsing in production
		stats.HitRate = 0.0  // Placeholder
		stats.MissRate = 0.0 // Placeholder
	}

	return stats, nil
}
