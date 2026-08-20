package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestStoreEnqueueIsIdempotentAndSnapshotIsDeterministic(t *testing.T) {
	store := openTestStore(t)
	first, _ := ParsePullRequestURL("https://github.com/acme/service/pull/2")
	second, _ := ParsePullRequestURL("https://github.com/acme/service/pull/1")

	added, err := store.Enqueue(context.Background(), first)
	if err != nil || !added {
		t.Fatalf("first enqueue: added=%v err=%v", added, err)
	}
	if added, err = store.Enqueue(context.Background(), first); err != nil || added {
		t.Fatalf("duplicate enqueue: added=%v err=%v", added, err)
	}
	if _, err = store.Enqueue(context.Background(), second); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 2 || snapshot[0].Number != 2 || snapshot[1].Number != 1 {
		t.Fatalf("unexpected insertion order: %+v", snapshot)
	}
}

func TestStoreEnqueueManyReportsDuplicateInputsInOrder(t *testing.T) {
	store := openTestStore(t)
	first, _ := ParsePullRequestURL("https://github.com/acme/service/pull/2")
	second, _ := ParsePullRequestURL("https://github.com/acme/service/pull/1")

	added, err := store.EnqueueMany(context.Background(), []PullRequest{first, first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 3 || !added[0] || added[1] || !added[2] {
		t.Fatalf("added = %v, want [true false true]", added)
	}
	queue, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 || queue[0].Number != 2 || queue[1].Number != 1 {
		t.Fatalf("queue = %+v, want pull requests 2 then 1", queue)
	}
}

func TestStoreConcurrentProducersInsertOneQueueEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	stores := make([]*Store, 2)
	for i := range stores {
		var err error
		stores[i], err = OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer stores[i].Close()
	}
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/1")
	start := make(chan struct{})
	results := make(chan bool, 2)
	errors := make(chan error, 2)
	var producers sync.WaitGroup
	for _, store := range stores {
		producers.Add(1)
		go func(store *Store) {
			defer producers.Done()
			<-start
			added, err := store.EnqueueMany(context.Background(), []PullRequest{pr})
			if err != nil {
				errors <- err
				return
			}
			results <- added[0]
		}(store)
	}
	close(start)
	producers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	addedCount := 0
	for added := range results {
		if added {
			addedCount++
		}
	}
	queue, err := stores[0].Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if addedCount != 1 || len(queue) != 1 {
		t.Fatalf("added count = %d, queue = %+v", addedCount, queue)
	}
}

func TestStoreStatusOrdersQueueAndBoundsRecentHistory(t *testing.T) {
	store := openTestStore(t)
	first, _ := ParsePullRequestURL("https://github.com/acme/service/pull/2")
	second, _ := ParsePullRequestURL("https://github.com/acme/service/pull/1")
	if _, err := store.EnqueueMany(context.Background(), []PullRequest{first, second}); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	for number := int64(1); number <= 3; number++ {
		pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/" + fmt.Sprint(number))
		if err := store.Finish(context.Background(), Attempt{
			PullRequest: pr, StartedAt: started.Add(time.Duration(number) * time.Second),
			FinishedAt: started.Add(time.Duration(number+1) * time.Second), ErrorCode: "failed",
		}); err != nil {
			t.Fatal(err)
		}
	}

	queue, history, err := store.Status(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 || queue[0].Number != 2 || queue[1].Number != 1 {
		t.Fatalf("queue = %+v, want pull requests 2 then 1", queue)
	}
	if len(history) != 2 || history[0].Number != 3 || history[1].Number != 2 {
		t.Fatalf("history = %+v, want newest pull requests 3 then 2", history)
	}
}

func TestStoreSnapshotExcludesLaterInsertions(t *testing.T) {
	store := openTestStore(t)
	first, _ := ParsePullRequestURL("https://github.com/acme/service/pull/1")
	second, _ := ParsePullRequestURL("https://github.com/acme/service/pull/2")
	if _, err := store.Enqueue(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Number != 1 {
		t.Fatalf("snapshot changed after later insertion: %+v", snapshot)
	}
	next, err := store.Snapshot(context.Background())
	if err != nil || len(next) != 2 || next[1].Number != 2 {
		t.Fatalf("next snapshot = %+v, err = %v", next, err)
	}
}

func TestStoreFinishesAttemptsAtomically(t *testing.T) {
	store := openTestStore(t)
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/1")
	if _, err := store.Enqueue(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

	if err := store.Finish(context.Background(), Attempt{
		PullRequest:  pr,
		HeadSHA:      "abc123",
		StartedAt:    started,
		FinishedAt:   started.Add(time.Second),
		ErrorCode:    "codex_failed",
		ErrorMessage: "codex failed",
	}); err != nil {
		t.Fatal(err)
	}
	queue, _ := store.Snapshot(context.Background())
	if len(queue) != 1 {
		t.Fatalf("failed attempt removed queue entry: %+v", queue)
	}

	if err := store.Finish(context.Background(), Attempt{
		PullRequest: pr,
		HeadSHA:     "abc123",
		SkillDigest: "sha256:skill",
		StartedAt:   started,
		FinishedAt:  started.Add(2 * time.Second),
		Success:     true,
		Verdict:     "APPROVE",
		ReviewID:    "42",
		ReviewURL:   "https://github.com/acme/service/pull/1#pullrequestreview-42",
	}); err != nil {
		t.Fatal(err)
	}
	queue, _ = store.Snapshot(context.Background())
	if len(queue) != 0 {
		t.Fatalf("successful attempt retained queue entry: %+v", queue)
	}
	history, err := store.History(context.Background())
	if err != nil || len(history) != 2 || history[0].Success || !history[1].Success {
		t.Fatalf("unexpected history: %+v err=%v", history, err)
	}
}
