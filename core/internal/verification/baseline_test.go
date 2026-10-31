package verification

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// baselineScript writes a shell script that appends one line to counter on every run, then runs body.
// The returned command runs it, so the counter shows how many times the baseline really ran.
func baselineScript(t *testing.T, body string) (command, counter string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	dir := t.TempDir()
	counter = filepath.Join(dir, "runs")
	script := filepath.Join(dir, "check.sh")
	src := "echo run >> '" + counter + "'\n" + body + "\n"
	if err := os.WriteFile(script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	return "sh " + script, counter
}

func runCount(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "run")
}

func TestBaselineCacheHitRunsOnce(t *testing.T) {
	command, counter := baselineScript(t, "exit 1")
	cache := NewBaselineCache()
	base := t.TempDir()
	for i := 0; i < 2; i++ {
		if !cache.FailsOnBase(context.Background(), Runner{}, base, command) {
			t.Fatalf("call %d: want failsOnBase true", i)
		}
	}
	if n := runCount(t, counter); n != 1 {
		t.Fatalf("baseline ran %d times, want 1", n)
	}
}

func TestBaselinePassingCommandCachedAsNotFailing(t *testing.T) {
	command, counter := baselineScript(t, "exit 0")
	cache := NewBaselineCache()
	base := t.TempDir()
	for i := 0; i < 2; i++ {
		if cache.FailsOnBase(context.Background(), Runner{}, base, command) {
			t.Fatalf("call %d: want failsOnBase false for a passing baseline", i)
		}
	}
	if n := runCount(t, counter); n != 1 {
		t.Fatalf("baseline ran %d times, want 1", n)
	}
}

func TestBaselineNewCommitMisses(t *testing.T) {
	command, counter := baselineScript(t, "exit 1")
	cache := NewBaselineCache()
	cache.FailsOnBase(context.Background(), Runner{}, t.TempDir(), command)
	// A new base commit has its own baseline checkout, hence a new dir.
	cache.FailsOnBase(context.Background(), Runner{}, t.TempDir(), command)
	if n := runCount(t, counter); n != 2 {
		t.Fatalf("baseline ran %d times across two base commits, want 2", n)
	}
}

func TestBaselineTimeoutIsReused(t *testing.T) {
	old := CheckLimit
	CheckLimit = 200 * time.Millisecond
	t.Cleanup(func() { CheckLimit = old })

	command, counter := baselineScript(t, "sleep 5")
	cache := NewBaselineCache()
	base := t.TempDir()
	start := time.Now()
	if !cache.FailsOnBase(context.Background(), Runner{}, base, command) {
		t.Fatal("first call: timed-out baseline must count as failing")
	}
	if !cache.FailsOnBase(context.Background(), Runner{}, base, command) {
		t.Fatal("second call: cached timeout must count as failing")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %s: timed-out baseline was not stopped at CheckLimit", elapsed)
	}
	if n := runCount(t, counter); n != 1 {
		t.Fatalf("baseline ran %d times after a timeout, want 1", n)
	}
}

func TestBaselineConcurrentCallersShareOneRun(t *testing.T) {
	command, counter := baselineScript(t, "sleep 0.3\nexit 1")
	cache := NewBaselineCache()
	base := t.TempDir()
	const workers = 8
	var wg sync.WaitGroup
	results := make([]bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = cache.FailsOnBase(context.Background(), Runner{}, base, command)
		}(i)
	}
	wg.Wait()
	for i, got := range results {
		if !got {
			t.Fatalf("worker %d: want failsOnBase true", i)
		}
	}
	if n := runCount(t, counter); n != 1 {
		t.Fatalf("baseline ran %d times under %d concurrent workers, want 1", n, workers)
	}
}

func TestBaselineCancelledRunIsNotCached(t *testing.T) {
	command, counter := baselineScript(t, "exit 1")
	cache := NewBaselineCache()
	base := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cache.FailsOnBase(ctx, Runner{}, base, command) {
		t.Fatal("cancelled caller: outcome unknown, want false")
	}
	if !cache.FailsOnBase(context.Background(), Runner{}, base, command) {
		t.Fatal("after cancellation: want a fresh run that fails")
	}
	if n := runCount(t, counter); n != 1 {
		t.Fatalf("baseline ran %d times, want 1 (the cancelled attempt must not be cached)", n)
	}
}

func TestBaselineLeaderCancelledKeepsRunForWaiter(t *testing.T) {
	command, counter := baselineScript(t, "sleep 0.5\nexit 1")
	cache := NewBaselineCache()
	base := t.TempDir()
	leaderCtx, cancel := context.WithCancel(context.Background())

	leaderDone := make(chan bool, 1)
	go func() { leaderDone <- cache.FailsOnBase(leaderCtx, Runner{}, base, command) }()

	// Let the leader start the shared run, then have a second agent wait on it.
	waitForRuns(t, counter, 1)
	waiterDone := make(chan bool, 1)
	go func() { waiterDone <- cache.FailsOnBase(context.Background(), Runner{}, base, command) }()
	time.Sleep(100 * time.Millisecond)

	cancel()
	if got := <-leaderDone; got {
		t.Fatal("cancelled leader: outcome unknown to it, want false")
	}
	if got := <-waiterDone; !got {
		t.Fatal("waiter: want the real outcome (true) after the leader was cancelled")
	}
	if n := runCount(t, counter); n != 1 {
		t.Fatalf("baseline ran %d times, want the one shared run", n)
	}
	// The finished run is cached: later callers do not run it again.
	if !cache.FailsOnBase(context.Background(), Runner{}, base, command) {
		t.Fatal("later caller: want the cached outcome true")
	}
	if n := runCount(t, counter); n != 1 {
		t.Fatalf("baseline ran %d times after the cached hit, want 1", n)
	}
}

func TestBaselineLeaderCancelledRunIsNotAborted(t *testing.T) {
	command, counter := baselineScript(t, "sleep 0.5\nexit 0")
	cache := NewBaselineCache()
	base := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cache.FailsOnBase(ctx, Runner{}, base, command)
		close(done)
	}()
	waitForRuns(t, counter, 1)
	cancel()
	<-done
	// The run kept going after the leader was cancelled and passed, so it is cached as not failing.
	if _, known := cache.BaselineOutcome(context.Background(), Runner{}, base, command); !known {
		t.Fatal("want a known cached outcome")
	}
	if cache.FailsOnBase(context.Background(), Runner{}, base, command) {
		t.Fatal("want false: the detached run passed")
	}
	if n := runCount(t, counter); n != 1 {
		t.Fatalf("baseline ran %d times, want 1", n)
	}
}

func TestBaselineWaitersCancelledOutcomeUnknown(t *testing.T) {
	command, counter := baselineScript(t, "sleep 1\nexit 1")
	cache := NewBaselineCache()
	base := t.TempDir()
	leader := make(chan bool, 1)
	go func() { leader <- cache.FailsOnBase(context.Background(), Runner{}, base, command) }()
	waitForRuns(t, counter, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	fails, known := cache.BaselineOutcome(ctx, Runner{}, base, command)
	if fails || known {
		t.Fatalf("cancelled waiter: got fails=%v known=%v, want false, false", fails, known)
	}
	if !<-leader {
		t.Fatal("leader: want the real outcome true")
	}
	if fails, known := cache.BaselineOutcome(context.Background(), Runner{}, base, command); !fails || !known {
		t.Fatalf("later caller: got fails=%v known=%v, want true, true", fails, known)
	}
	if n := runCount(t, counter); n != 1 {
		t.Fatalf("baseline ran %d times, want 1", n)
	}
}

// waitForRuns waits until the baseline script has been started want times, so a test can act on the in-flight run.
func waitForRuns(t *testing.T, counter string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runCount(t, counter) < want {
		if time.Now().After(deadline) {
			t.Fatalf("baseline did not start after 5s: want %d runs", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
