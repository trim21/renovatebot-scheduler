package app

import (
	"testing"
	"time"
)

type recordingEnqueuer struct {
	repos []string
}

func (e *recordingEnqueuer) Enqueue(repo, _ string, _ bool) bool {
	e.repos = append(e.repos, repo)
	return true
}

func TestEnqueueStaleRepos(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	for _, name := range []string{"owner/never", "owner/fresh", "owner/stale"} {
		if err := store.Put(&TrackedRepo{
			FullName:      name,
			DefaultBranch: "main",
			HookID:        1,
			HookToken:     "token-" + name,
			EnabledBy:     "tester",
			EnabledAt:     time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	recordRunAt := func(repo string, started time.Time) {
		t.Helper()
		if err := recordRun(store, RunRecord{
			ID:         "run-" + repo,
			Repo:       repo,
			Trigger:    "push",
			StartedAt:  started,
			FinishedAt: started.Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	recordRunAt("owner/fresh", now.Add(-time.Hour))
	recordRunAt("owner/stale", now.Add(-25*time.Hour))

	enq := &recordingEnqueuer{}
	enqueueStaleRepos(store, enq, 24*time.Hour, now)

	want := map[string]bool{"owner/never": true, "owner/stale": true}
	if len(enq.repos) != len(want) {
		t.Fatalf("unexpected queued repositories: %v", enq.repos)
	}
	for _, repo := range enq.repos {
		if !want[repo] {
			t.Fatalf("repository %q should not be queued: %v", repo, enq.repos)
		}
	}
}
