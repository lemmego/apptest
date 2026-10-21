package apptest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Interrupting a test run leaks a scratch database. A signal terminates the
// process without running t.Cleanup — golang/go#41891 — so the drop
// registered beside the CREATE never happens. The database is small, but it
// is indistinguishable from a real one at a glance and a run accumulates one
// per interruption, which is how somebody ends up reading a database list and
// wondering which of these matter.
//
// So the first test to create a scratch database also installs a handler that
// drops them. It is installed there rather than in an init(), because a
// harness used without WithPostgres has no business touching the process's
// signal disposition.
//
// This covers SIGINT and SIGTERM, which is Ctrl-C and an ordinary kill. It
// cannot cover SIGKILL, and it does not cover `go test -timeout` expiring:
// the testing package panics the process itself, sending no signal and
// running no cleanups. Those still leak.

// dropTimeout bounds a drop so an unreachable server cannot stall exit.
const dropTimeout = 5 * time.Second

var scratchRegistry struct {
	mu      sync.Mutex
	live    map[*scratchDB]struct{}
	install sync.Once
}

// registerScratch records a live scratch database and, on the first call,
// installs the interrupt handler.
func registerScratch(s *scratchDB) {
	scratchRegistry.mu.Lock()
	if scratchRegistry.live == nil {
		scratchRegistry.live = map[*scratchDB]struct{}{}
	}
	scratchRegistry.live[s] = struct{}{}
	scratchRegistry.mu.Unlock()

	scratchRegistry.install.Do(installInterruptCleanup)
}

// unregisterScratch forgets a database that has already been dropped, so the
// interrupt path does not try again.
func unregisterScratch(s *scratchDB) {
	scratchRegistry.mu.Lock()
	delete(scratchRegistry.live, s)
	scratchRegistry.mu.Unlock()
}

func installInterruptCleanup() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-ch

		// Restore the default disposition *before* dropping anything, so a
		// second Ctrl-C kills the process at once. Resetting afterwards would
		// leave this goroutine holding the only handler while it talks to a
		// database server — which may be exactly what somebody is
		// interrupting — and would turn a leaked database into a test run
		// that cannot be stopped. A leak is cheap; an unkillable run is not.
		signal.Stop(ch)
		signal.Reset(os.Interrupt, syscall.SIGTERM)

		dropLiveScratch()

		// Re-raise, so the exit status is the conventional one for the signal
		// rather than whatever this goroutine would produce. The default
		// disposition is back, so this terminates the process.
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(sig)
			time.Sleep(time.Second)
		}
		// Only reached if re-raising did not terminate us. Exiting is still
		// better than returning into a scheduler that would run more tests
		// against databases this goroutine has just dropped.
		os.Exit(130)
	}()
}

// dropLiveScratch drops every database the run still holds.
func dropLiveScratch() {
	scratchRegistry.mu.Lock()
	live := make([]*scratchDB, 0, len(scratchRegistry.live))
	for s := range scratchRegistry.live {
		live = append(live, s)
	}
	scratchRegistry.live = nil
	scratchRegistry.mu.Unlock()

	for _, s := range live {
		// Not t.Logf: the test goroutine may be anywhere by now, and the
		// testing package panics on a log after its test has completed. A
		// cleanup that crashes the process it is tidying up after would be
		// worse than the leak.
		if err := dropScratch(s); err != nil {
			fmt.Fprintf(os.Stderr, "apptest: interrupted, and dropping %s failed: %v — drop it by hand\n", s.name, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "apptest: interrupted; dropped scratch database %s\n", s.name)
	}
}

// dropScratch drops one scratch database. It is the single drop both the
// ordinary teardown and the interrupt handler go through, so the two cannot
// disagree about how a database is removed.
func dropScratch(s *scratchDB) error {
	admin, err := sql.Open("postgres", s.adminDSN)
	if err != nil {
		return err
	}
	defer admin.Close()

	ctx, cancel := context.WithTimeout(context.Background(), dropTimeout)
	defer cancel()

	// FORCE terminates connections the application left open; without it a
	// pool that has not finished closing keeps the database alive and every
	// subsequent run leaks another one. It is load-bearing on the interrupt
	// path in particular, where nothing has closed its pool at all.
	_, err = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+s.name+`" WITH (FORCE)`)
	return err
}
