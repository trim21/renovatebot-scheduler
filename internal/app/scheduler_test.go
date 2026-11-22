package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/trim21/renovatebot-scheduler/internal/runner"
)

type fakeRunner struct {
	mu       sync.Mutex
	runs     []runner.RunRequest
	gate     chan struct{}
	failures int
}

func (f *fakeRunner) Run(ctx context.Context, req runner.RunRequest) runner.RunResult {
	f.mu.Lock()
	f.runs = append(f.runs, req)
	attempt := len(f.runs)
	failures := f.failures
	gate := f.gate
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return runner.RunResult{ExitCode: -1, Err: ctx.Err()}
		}
	}
	if attempt <= failures {
		return runner.RunResult{ExitCode: 1}
	}
	return runner.RunResult{ExitCode: 0}
}

func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestSchedulerCoalescesBurst(t *testing.T) {
	runner := &fakeRunner{}
	scheduler := NewScheduler(runner, 40*time.Millisecond, 1, 0, time.Millisecond)
	defer scheduler.Stop(time.Second)

	for i := 0; i < 10; i++ {
		if !scheduler.Enqueue("owner/repo", "push", false) {
			t.Fatal("enqueue rejected")
		}
	}

	if !waitFor(t, 2*time.Second, func() bool { return runner.count() == 1 }) {
		t.Fatalf("got %d runs, want 1", runner.count())
	}
	// Nothing else may start: the whole burst was coalesced into one run.
	time.Sleep(150 * time.Millisecond)
	if got := runner.count(); got != 1 {
		t.Fatalf("got %d runs after settling, want 1", got)
	}
}

func TestSchedulerRunsAgainForEventDuringRun(t *testing.T) {
	gate := make(chan struct{})
	runner := &fakeRunner{gate: gate}
	scheduler := NewScheduler(runner, 20*time.Millisecond, 1, 0, time.Millisecond)
	defer scheduler.Stop(2 * time.Second)

	scheduler.Enqueue("owner/repo", "push", true)
	if !waitFor(t, 2*time.Second, func() bool { return runner.count() == 1 }) {
		t.Fatalf("first run did not start, got %d", runner.count())
	}

	// This event arrives while the first run is still in progress.
	scheduler.Enqueue("owner/repo", "push", false)
	close(gate)

	if !waitFor(t, 3*time.Second, func() bool { return runner.count() == 2 }) {
		t.Fatalf("event during run was lost, got %d runs", runner.count())
	}
}

func TestSchedulerRetriesFailedRun(t *testing.T) {
	runner := &fakeRunner{failures: 1}
	scheduler := NewScheduler(runner, 10*time.Millisecond, 1, 1, 10*time.Millisecond)
	defer scheduler.Stop(2 * time.Second)

	scheduler.Enqueue("owner/repo", "push", true)
	if !waitFor(t, 3*time.Second, func() bool { return runner.count() == 2 }) {
		t.Fatalf("got %d runs, want 2", runner.count())
	}
	if !waitFor(t, time.Second, func() bool { return scheduler.Status("owner/repo").State == WorkerStateIdle }) {
		t.Fatal("scheduler did not become idle")
	}
	status := scheduler.Status("owner/repo")
	if status.LastExit != 0 || status.LastError != "" {
		t.Fatalf("unexpected status after successful retry: %+v", status)
	}
}

func TestSchedulerStopRejectsNewWork(t *testing.T) {
	scheduler := NewScheduler(&fakeRunner{}, 10*time.Millisecond, 1, 0, time.Millisecond)
	scheduler.Stop(time.Second)
	if scheduler.Enqueue("owner/repo", "push", true) {
		t.Fatal("enqueue must fail after Stop")
	}
}

func TestSchedulerIndependentRepositories(t *testing.T) {
	runner := &fakeRunner{}
	scheduler := NewScheduler(runner, 20*time.Millisecond, 2, 0, time.Millisecond)
	defer scheduler.Stop(time.Second)

	scheduler.Enqueue("owner/a", "push", false)
	scheduler.Enqueue("owner/b", "push", false)

	if !waitFor(t, 2*time.Second, func() bool { return runner.count() == 2 }) {
		t.Fatalf("got %d runs, want 2", runner.count())
	}
	for _, repo := range []string{"owner/a", "owner/b"} {
		if state := scheduler.Status(repo).State; state != WorkerStateIdle && state != WorkerStateQueued {
			t.Fatalf("%s has unexpected state %q", repo, state)
		}
	}
}
