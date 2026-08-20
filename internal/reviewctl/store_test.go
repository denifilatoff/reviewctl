package reviewctl

import (
	"context"
	"path/filepath"
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
