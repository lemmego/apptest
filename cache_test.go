package apptest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemmego/api/config"
	"github.com/lemmego/cache"
	_ "github.com/lemmego/cache/store/file"
)

// The leak this file exists to prevent.
//
// Isolating by prefix alone is not enough: the file store gives each prefix a
// subtree under cache.stores.file.path, which defaults to
// storage/framework/cache — inside the project. One directory per App, kept
// forever. A real application reached 15,244 of them, 80,691 files and 315MB,
// over eight days of test runs.
//
// The assertion is deliberately about where the bytes landed rather than about
// the configuration: a test that only checked the config key would pass
// against a store that ignored it.
func TestAFileCacheWritesInsideTheTestsOwnDirectory(t *testing.T) {
	a := New(t, WithProviders(&cache.Provider{}), WithConfig(config.M{
		"cache": config.M{
			"driver": "file",
			"prefix": "unset_by_the_test:",
		},
	}))

	c := cache.FromApp(a.App())
	if err := c.Put(context.Background(), "who", "ada", time.Minute); err != nil {
		t.Fatalf("writing to the cache: %v", err)
	}

	// Nowhere near the project, checked first and unconditionally, because
	// this is the assertion that describes the bug. The default path is
	// relative and New has already chdir'd to the project root, so a store
	// that never saw the override writes right here — and a test that bailed
	// out earlier on a missing CacheDir would report something true but
	// uninformative instead.
	if _, err := os.Stat(filepath.Join("storage", "framework", "cache")); err == nil {
		t.Error("the cache wrote into storage/framework/cache, which is the leak this prevents")
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking the project cache directory: %v", err)
	}

	dir := a.CacheDir()
	if dir == "" {
		t.Fatal("no cache directory was set aside for this App")
	}
	if got := countFiles(t, dir); got == 0 {
		t.Errorf("the cache wrote nothing under %s, so the entry went somewhere else", dir)
	}
}

// The directory has to be one the testing package owns, or nothing removes it.
// Asserted by location rather than by deleting and re-checking, because
// t.TempDir's own cleanup runs after this test returns.
func TestTheCacheDirectoryIsTheTestsTempDir(t *testing.T) {
	a := New(t, WithConfig(config.M{"cache": config.M{"driver": "file"}}))

	dir := a.CacheDir()
	if dir == "" {
		t.Fatal("no cache directory was set aside for this App")
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("cache directory %q is relative, so it resolves against whatever the working directory is", dir)
	}
	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Errorf("cache directory %q is not under %q, so the testing package will not remove it", dir, os.TempDir())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("cache directory %q does not exist: %v", dir, err)
	}

	// Both the App's configuration and the package-level singleton, for the
	// reason the prefix does both: the provider reads the application's
	// config and other packages read the global.
	if got := config.Get("cache.stores.file.path"); got != dir {
		t.Errorf("config.Get(cache.stores.file.path) = %v, want %q", got, dir)
	}
}

// Two Apps must not share a directory, or one test's flush empties another's
// cache.
func TestEachAppGetsItsOwnCacheDirectory(t *testing.T) {
	first := New(t, WithConfig(config.M{"cache": config.M{"driver": "file"}})).CacheDir()
	second := New(t, WithConfig(config.M{"cache": config.M{"driver": "file"}})).CacheDir()

	if first == "" || second == "" {
		t.Fatal("an App got no cache directory")
	}
	if first == second {
		t.Errorf("both Apps were given %q", first)
	}
}

// WithSharedCache means "leave my cache alone", and that has to include where
// it lives — a test using it is deliberately looking at what another process
// wrote.
//
// Asserted against the configuration map this test owns, not against the
// package-level singleton: every test in this binary shares that singleton, so
// reading it here reports whatever the previous test set.
func TestWithSharedCacheLeavesTheDirectoryAlone(t *testing.T) {
	cfg := config.M{
		"cache": config.M{
			"driver": "file",
			"prefix": "chosen_by_the_project:",
			"stores": config.M{"file": config.M{"path": "chosen/by/the/project"}},
		},
	}

	a := New(t, WithSharedCache(), WithConfig(cfg))

	if got := a.CacheDir(); got != "" {
		t.Errorf("CacheDir() = %q, want empty under WithSharedCache", got)
	}

	block := cfg["cache"].(config.M)
	if got := block["stores"].(config.M)["file"].(config.M)["path"]; got != "chosen/by/the/project" {
		t.Errorf("the configured path became %v, so WithSharedCache rewrote it anyway", got)
	}
	if got := block["prefix"]; got != "chosen_by_the_project:" {
		t.Errorf("the configured prefix became %v", got)
	}
}

// An App with no cache configuration has nothing to isolate, and must not
// acquire a cache block by being tested.
func TestNoCacheConfigurationMeansNoCacheDirectory(t *testing.T) {
	a := New(t)

	if got := a.CacheDir(); got != "" {
		t.Errorf("CacheDir() = %q, want empty with no cache configuration", got)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return count
}
