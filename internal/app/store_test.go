package app

import (
	"fmt"
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

func TestStoreRuns(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		if err := store.AppendRun(RunRecord{
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
