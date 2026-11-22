package app

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStoreRepositories(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	repo := &TrackedRepo{
		FullName:      "owner/repo",
		DefaultBranch: "main",
		HookID:        42,
		HookToken:     "token-1",
		EnabledBy:     "alice",
		EnabledAt:     time.Now(),
	}
	if err := store.Put(repo); err != nil {
		t.Fatal(err)
	}

	got, ok := store.Get("owner/repo")
	if !ok || got.HookID != 42 || got.HookToken != "token-1" {
		t.Fatalf("unexpected repo: %+v (found %v)", got, ok)
	}
	if byToken, ok := store.GetByToken("token-1"); !ok || byToken.FullName != "owner/repo" {
		t.Fatalf("unexpected lookup by token: %+v (found %v)", byToken, ok)
	}
	if _, ok := store.GetByToken("missing"); ok {
		t.Fatal("unknown token found")
	}

	repo.HookID = 43
	repo.HookToken = "token-2"
	if err := store.Put(repo); err != nil {
		t.Fatal(err)
	}
	if updated, _ := store.Get("owner/repo"); updated.HookID != 43 || updated.HookToken != "token-2" {
		t.Fatalf("update was not persisted: %+v", updated)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()

	list := reopened.List()
	if len(list) != 1 || list[0].FullName != "owner/repo" {
		t.Fatalf("repos were not persisted: %+v", list)
	}
	if err := reopened.Delete("owner/repo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("owner/repo"); ok {
		t.Fatal("repo still present after delete")
	}
	if err := reopened.Delete("owner/repo"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRunsForRepo(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	base := time.Now().UTC().Truncate(time.Second)
	runs := []struct {
		id   string
		repo string
	}{
		{"run-1", "owner/a"},
		{"run-2", "owner/b"},
		{"run-3", "owner/a"},
	}
	for i, item := range runs {
		if err := recordRun(store, RunRecord{
			ID:         item.id,
			Repo:       item.repo,
			StartedAt:  base.Add(time.Duration(i) * time.Second),
			FinishedAt: base.Add(time.Duration(i)*time.Second + time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.RunsForRepo("owner/a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "run-3" || got[1].ID != "run-1" {
		t.Fatalf("unexpected runs for owner/a: %+v", got)
	}

	none, err := store.RunsForRepo("owner/unknown", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("expected no runs for an unknown repository, got %+v", none)
	}
}

func TestStoreRuns(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		if err := recordRun(store, RunRecord{
			ID:         fmt.Sprintf("run-%d", i),
			Repo:       "owner/repo",
			Trigger:    "push",
			Attempt:    i + 1,
			StartedAt:  base.Add(time.Duration(i) * time.Second),
			FinishedAt: base.Add(time.Duration(i)*time.Second + time.Minute),
			ExitCode:   i,
			Log:        []byte("line-1\nline-2\n"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	recent, err := store.RecentRuns(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0].ID != "run-4" || recent[1].ID != "run-3" {
		t.Fatalf("unexpected recent runs: %+v", recent)
	}
	if recent[0].Log != nil {
		t.Fatal("RecentRuns should not load log bodies")
	}

	found, ok := store.FindRun("run-1")
	if !ok || found.ExitCode != 1 || len(found.Log) == 0 {
		t.Fatalf("unexpected run: %+v (found %v)", found, ok)
	}
	if _, ok := store.FindRun("nope"); ok {
		t.Fatal("unknown run found")
	}

	text, err := store.ReadRunLog("run-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if text != "line-1\nline-2" {
		t.Fatalf("unexpected log tail: %q", text)
	}
	if _, err := store.ReadRunLog("nope", 10); err == nil {
		t.Fatal("missing run log did not return an error")
	}
}

func recordRun(store *Store, record RunRecord) error {
	if err := store.StartRun(record); err != nil {
		return err
	}
	return store.FinishRun(record)
}

func TestStoreRunningRun(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	start := time.Now().UTC().Truncate(time.Second)
	if err := store.StartRun(RunRecord{
		ID:        "run-live",
		Repo:      "owner/repo",
		Trigger:   "push",
		Attempt:   1,
		StartedAt: start,
	}); err != nil {
		t.Fatal(err)
	}

	record, ok := store.FindRun("run-live")
	if !ok || !record.Running {
		t.Fatalf("expected a running record, got %+v (found %v)", record, ok)
	}
	if !record.FinishedAt.IsZero() {
		t.Fatalf("running record has finished_at set: %v", record.FinishedAt)
	}

	runs, err := store.RecentRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || !runs[0].Running {
		t.Fatalf("expected the running run in the list, got %+v", runs)
	}

	if err := store.UpdateRunLog("run-live", []byte("partial output\n")); err != nil {
		t.Fatal(err)
	}
	text, err := store.ReadRunLog("run-live", 10)
	if err != nil {
		t.Fatal(err)
	}
	if text != "partial output" {
		t.Fatalf("unexpected in-progress log: %q", text)
	}

	n, err := store.FailStaleRuns()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 stale run, got %d", n)
	}
	record, _ = store.FindRun("run-live")
	if record.Running || !strings.Contains(record.Error, "interrupted") {
		t.Fatalf("stale run not marked interrupted: %+v", record)
	}

	if err := store.StartRun(RunRecord{ID: "run-done", Repo: "owner/repo", StartedAt: start}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(RunRecord{
		ID:         "run-done",
		FinishedAt: start.Add(time.Minute),
		ExitCode:   0,
		Log:        []byte("done\n"),
	}); err != nil {
		t.Fatal(err)
	}
	record, _ = store.FindRun("run-done")
	if record.Running || record.ExitCode != 0 || record.Duration() != time.Minute {
		t.Fatalf("unexpected finished record: %+v", record)
	}
	if text, err := store.ReadRunLog("run-done", 10); err != nil || text != "done" {
		t.Fatalf("unexpected finished log: %q (err %v)", text, err)
	}
}
