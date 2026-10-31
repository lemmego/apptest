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
// config singleton, for the same reason WithScratchDB rewrites both: the cache
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

	// A filesystem-backed cache also gets a directory of its own, because a
	// prefix alone leaks and the shortfall is invisible from inside one test.
	//
	// The file store gives every prefix its own subtree under
	// cache.stores.file.path, which defaults to storage/framework/cache —
	// inside the project. A fresh prefix per App therefore leaves a tree per
	// App behind, nothing removes it, and Flush would not help: it deletes
	// the entries and leaves the directories. Measured on a real application
	// after eight days of test runs: 15,244 prefix directories, 80,691 files,
	// 315MB. It had a second cost nobody would predict from here — Tailwind
	// scans the project for class names, so it read all of them on every
	// stylesheet build and a page load waited nine seconds for its CSS.
	//
	// t.TempDir is the whole fix. The testing package removes it recursively
	// when the test ends, so there is no path for this package to derive and
	// no cleanup of its own to get wrong — which also means no leak on an
	// interrupt, unlike the scratch database in postgres.go.
	//
	// Set whatever the configured driver is. The driver is usually chosen by
	// the environment, so isolating only when it currently reads "file" would
	// leak for whoever runs the suite with the other one.
	dir := a.t.TempDir()
	stores, ok := block["stores"].(config.M)
	if !ok {
		stores = config.M{}
		block["stores"] = stores
	}
	fileStore, ok := stores["file"].(config.M)
	if !ok {
		fileStore = config.M{}
		stores["file"] = fileStore
	}
	fileStore["path"] = dir
	config.Set("cache.stores.file.path", dir)
	a.cacheDir = dir
}

// CachePrefix returns the namespace this App's cache entries live under, for a
// test that needs to inspect the store directly.
func (a *App) CachePrefix() string { return a.cachePrefix }

// CacheDir returns the directory a filesystem-backed cache keeps this App's
// entries in, for a test that needs to inspect them. Empty when the App has no
// cache configuration, or under WithSharedCache.
func (a *App) CacheDir() string { return a.cacheDir }

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
