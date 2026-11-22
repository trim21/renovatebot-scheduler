package app

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/trim21/renovatebot-scheduler/internal/runner"
)

// blockingBackend writes one line, reports that it started, and waits for
// release before writing the second line and returning.
type blockingBackend struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingBackend) Run(_ context.Context, _ runner.RunRequest, log io.Writer) runner.RunResult {
	_, _ = fmt.Fprintln(log, "first line")
	close(b.started)
	<-b.release
	_, _ = fmt.Fprintln(log, "last line")
	return runner.RunResult{ExitCode: 3}
}

func TestExecutorShowsRunningRun(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	backend := &blockingBackend{started: make(chan struct{}), release: make(chan struct{})}
	exec := newExecutor(backend, store, time.Minute)

	done := make(chan runner.RunResult, 1)
	go func() {
		done <- exec.Run(context.Background(), runner.RunRequest{Repo: "owner/repo", Trigger: "push", Attempt: 1})
	}()

	<-backend.started

	runs, err := store.RecentRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || !runs[0].Running {
		t.Fatalf("expected one running run, got %+v", runs)
	}

	if err := store.UpdateRunLog(runs[0].ID, []byte("first line\n")); err != nil {
		t.Fatal(err)
	}
	text, err := store.ReadRunLog(runs[0].ID, 10)
	if err != nil || text != "first line" {
		t.Fatalf("unexpected in-progress log: %q (err %v)", text, err)
	}

	close(backend.release)
	if result := <-done; result.ExitCode != 3 {
		t.Fatalf("unexpected result: %+v", result)
	}

	runs, err = store.RecentRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Running || runs[0].ExitCode != 3 {
		t.Fatalf("run not finished correctly: %+v", runs)
	}
	text, err = store.ReadRunLog(runs[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if text != "# repo: owner/repo\n# trigger: push\n# attempt: 1\n# started: "+runs[0].StartedAt.UTC().Format(time.RFC3339)+"\n\nfirst line\nlast line" {
		t.Fatalf("unexpected final log: %q", text)
	}
}

func TestFlushLogsWritesWhileRunning(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if err := store.StartRun(RunRecord{ID: "run-flush", Repo: "owner/repo", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	writer := &tailWriter{max: 1 << 10}
	_, _ = fmt.Fprintln(writer, "partial output")

	stop := flushLogs(store, "run-flush", writer, 5*time.Millisecond)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		text, err := store.ReadRunLog("run-flush", 10)
		if err != nil {
			t.Fatal(err)
		}
		if text == "partial output" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("log was not flushed while the run was still in progress")
}
