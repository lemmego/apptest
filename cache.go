package apptest

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/lemmego/api/config"
)

// Every App gets its own cache namespace, unless WithSharedCache says
// otherwise.
//
// Without this, tests share one keyspace — and with Redis they share it
// across runs. A rate limiter is the clearest casualty: a limit of five per
// minute is consumed by the whole suite rather than by each test, so the
// third test to exercise it gets a 429 it did not earn, and the failure moves
// around depending on which tests ran first.
//
// It rewrites cache.prefix in the configuration and in the package-level
// config singleton, for the same reason WithPostgres rewrites both: the cache
// provider reads the application's config, but other packages read the
// global.
func (a *App) isolateCache(o *options) {
	block, ok := o.config["cache"].(config.M)
	if !ok {
		// No cache configuration, so nothing to isolate. A test that
		// configures its cache provider explicitly is already isolated by
		// whatever prefix it passed.
		return
	}

	prefix := "apptest_" + randomSuffix() + ":"
	block["prefix"] = prefix
	config.Set("cache.prefix", prefix)
	a.cachePrefix = prefix
}

// CachePrefix returns the namespace this App's cache entries live under, for a
// test that needs to inspect the store directly.
func (a *App) CachePrefix() string { return a.cachePrefix }

// WithSharedCache leaves the cache prefix alone, so the App shares a keyspace
// with everything else. Only useful for a test that is deliberately checking
// what two applications see of each other.
func WithSharedCache() Option {
	return func(o *options) { o.sharedCache = true }
}

func randomSuffix() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(buf)
}
