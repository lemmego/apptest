package apptest

import (
	"os"
	"path/filepath"
	"testing"
)

// A Lemmego application resolves several paths relative to the working
// directory: the Inertia root template (resources/views/root.html), the Vite
// manifest (public/build/manifest.json), the local filesystem disk
// (./storage), and the Go template directory. In production the working
// directory is the project root, because that is where the binary is started.
//
// Under `go test ./internal/...` it is the package directory, so every one of
// those lookups fails — and the Inertia provider's failure is a panic at boot,
// not a degraded page. So the harness makes the working directory what the
// application expects rather than making every project special-case it.
//
// t.Chdir restores the previous directory and refuses to run inside a
// parallel test, which is the correct constraint for a process-global
// property.
func chdirToProjectRoot(t *testing.T) {
	t.Helper()

	root, err := findProjectRoot()
	if err != nil {
		t.Logf("apptest: %v — leaving the working directory alone; "+
			"a provider that reads a project-relative path may fail", err)
		return
	}
	t.Chdir(root)
}

// findProjectRoot walks up from the working directory to the nearest go.mod,
// which is the directory a Lemmego binary is started from.
func findProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errNoProjectRoot
		}
		dir = parent
	}
}

type noProjectRootError struct{}

func (noProjectRootError) Error() string {
	return "no go.mod found above the working directory, so the project root is unknown"
}

var errNoProjectRoot = noProjectRootError{}

// WithWorkingDir overrides where the application runs from. Use it when the
// project root is not the nearest go.mod — a monorepo with the application in
// a subdirectory, say.
func WithWorkingDir(dir string) Option {
	return func(o *options) { o.workingDir = dir }
}

// WithoutChdir leaves the working directory alone. Only useful for a test that
// deliberately exercises path resolution.
func WithoutChdir() Option {
	return func(o *options) { o.keepWorkingDir = true }
}
