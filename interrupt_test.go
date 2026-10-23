package apptest

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// interruptChildEnv makes this test binary act as the child of
// TestInterruptDropsTheScratchDatabase rather than as the test.
const interruptChildEnv = "APPTEST_INTERRUPT_CHILD"

// readyPrefix is how the child reports the database it created. The parent
// reads the name off stdout rather than guessing it, since it is random.
const readyPrefix = "apptest-interrupt-ready "

// TestInterruptDropsTheScratchDatabase interrupts a real process holding a
// real scratch database and asserts the database is gone afterwards.
//
// It has to re-execute this binary. The handler restores the default signal
// disposition and re-raises, so a test that signalled its own process would
// take down the run it is part of — and a test that called the handler's
// internals directly would prove only that a drop statement works, which is
// not the thing that was broken. What was broken is that nothing ran.
func TestInterruptDropsTheScratchDatabase(t *testing.T) {
	skipWithoutPostgres(t)
	adminDSN := testAdminDSN(t)

	if os.Getenv(interruptChildEnv) == "1" {
		runInterruptChild(adminDSN)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), interruptChildEnv+"=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}

	name := readScratchName(t, stdout)

	// A safety net, not the assertion: if the handler does not work, this
	// test must not itself leak the database it was written to catch.
	t.Cleanup(func() {
		if err := dropScratch(&scratchDB{adminDSN: adminDSN, name: name}); err != nil {
			t.Logf("apptest: cleaning up %s: %v", name, err)
		}
	})

	if !databaseExists(t, adminDSN, name) {
		t.Fatalf("the child reported %s but it does not exist, so this test would pass vacuously", name)
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupting the child: %v", err)
	}
	// An interrupted process exits non-zero, which is the point; only a
	// failure to exit at all is interesting here.
	waitFor(t, cmd)

	if databaseExists(t, adminDSN, name) {
		t.Errorf("scratch database %s survived the interrupt", name)
	}
}

// runInterruptChild creates a scratch database the way the harness does,
// reports it, and waits to be killed.
func runInterruptChild(adminDSN string) {
	scratch, err := createScratch(adminDSN)
	if err != nil {
		// stdout, so the parent's scanner reaches EOF with no ready line and
		// fails with this as the reason rather than timing out.
		os.Stdout.WriteString("child: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString(readyPrefix + scratch.name + "\n")
	// Long enough that a handler which never fires shows up as the parent's
	// wait timing out, rather than as the child exiting on its own and the
	// database disappearing for the wrong reason.
	time.Sleep(time.Minute)
}

func readScratchName(t *testing.T, stdout interface{ Read([]byte) (int, error) }) string {
	t.Helper()

	type result struct {
		name string
		rest string
	}
	done := make(chan result, 1)
	go func() {
		var other []string
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if after, ok := strings.CutPrefix(line, readyPrefix); ok {
				done <- result{name: after}
				return
			}
			other = append(other, line)
		}
		done <- result{rest: strings.Join(other, "\n")}
	}()

	select {
	case got := <-done:
		if got.name == "" {
			t.Fatalf("the child never reported a scratch database; it said:\n%s", got.rest)
		}
		return got.name
	case <-time.After(90 * time.Second):
		t.Fatalf("the child never reported a scratch database")
		return ""
	}
}

// waitFor waits for the child, failing if it does not exit — which is what a
// cleanup that hangs against an unreachable server would look like, and is a
// worse outcome than the leak this handler exists to prevent.
func waitFor(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the child did not exit after being interrupted")
	}
}
