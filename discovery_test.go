package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestShouldEnqueueDiscoveryEventMatrix(t *testing.T) {
	readyA := pullRequestSnapshot{HeadSHA: "a"}
	readyB := pullRequestSnapshot{HeadSHA: "b"}
	draftA := pullRequestSnapshot{HeadSHA: "a", IsDraft: true}
	draftB := pullRequestSnapshot{HeadSHA: "b", IsDraft: true}

	for _, test := range []struct {
		name        string
		initialized bool
		previous    *pullRequestSnapshot
		current     pullRequestSnapshot
		want        bool
	}{
		{name: "first ready pull request establishes baseline", current: readyA},
		{name: "first draft pull request establishes baseline", current: draftA},
		{name: "new ready pull request", initialized: true, current: readyA, want: true},
		{name: "new draft pull request", initialized: true, current: draftA},
		{name: "draft becomes ready", initialized: true, previous: &draftA, current: readyA, want: true},
		{name: "ready head changes", initialized: true, previous: &readyA, current: readyB, want: true},
		{name: "draft head changes", initialized: true, previous: &draftA, current: draftB},
		{name: "ready becomes draft", initialized: true, previous: &readyA, current: draftA},
		{name: "ready is unchanged", initialized: true, previous: &readyA, current: readyA},
		{name: "draft is unchanged", initialized: true, previous: &draftA, current: draftA},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldEnqueueDiscovery(test.initialized, test.previous, test.current); got != test.want {
				t.Fatalf("shouldEnqueueDiscovery() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestStoreAppliesDiscoverySnapshots(t *testing.T) {
	store := openTestStore(t)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	first := []pullRequestSnapshot{
		discoverySnapshot(repository.Repository, 1, "a", false),
		discoverySnapshot(repository.Repository, 2, "a", true),
	}
	if enqueued, err := store.ApplyDiscoverySnapshot(context.Background(), repository, first); err != nil || enqueued != 0 {
		t.Fatalf("first snapshot: enqueued=%d err=%v", enqueued, err)
	}
	second := []pullRequestSnapshot{
		discoverySnapshot(repository.Repository, 1, "a", false),
		discoverySnapshot(repository.Repository, 2, "a", false),
		discoverySnapshot(repository.Repository, 3, "a", false),
		discoverySnapshot(repository.Repository, 4, "a", true),
	}
	if enqueued, err := store.ApplyDiscoverySnapshot(context.Background(), repository, second); err != nil || enqueued != 2 {
		t.Fatalf("second snapshot: enqueued=%d err=%v", enqueued, err)
	}
	third := []pullRequestSnapshot{
		discoverySnapshot(repository.Repository, 1, "b", false),
		discoverySnapshot(repository.Repository, 2, "a", false),
		discoverySnapshot(repository.Repository, 4, "b", true),
	}
	if enqueued, err := store.ApplyDiscoverySnapshot(context.Background(), repository, third); err != nil || enqueued != 1 {
		t.Fatalf("third snapshot: enqueued=%d err=%v", enqueued, err)
	}
	queue, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 3 || queue[0].Number != 2 || queue[1].Number != 3 || queue[2].Number != 1 {
		t.Fatalf("queue = %+v, want pull requests 2, 3, and 1", queue)
	}
}

func TestStoreTreatsReadyPullRequestAfterAbsenceAsNew(t *testing.T) {
	store := openTestStore(t)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	ready := discoverySnapshot(repository.Repository, 1, "a", false)
	if _, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{ready}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyDiscoverySnapshot(context.Background(), repository, nil); err != nil {
		t.Fatal(err)
	}
	if enqueued, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{ready}); err != nil || enqueued != 1 {
		t.Fatalf("reopen snapshot: enqueued=%d err=%v", enqueued, err)
	}
}

func TestStoreRemembersAnEmptyFirstObservation(t *testing.T) {
	store := openTestStore(t)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	if enqueued, err := store.ApplyDiscoverySnapshot(context.Background(), repository, nil); err != nil || enqueued != 0 {
		t.Fatalf("empty first snapshot: enqueued=%d err=%v", enqueued, err)
	}
	ready := discoverySnapshot(repository.Repository, 1, "a", false)
	if enqueued, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{ready}); err != nil || enqueued != 1 {
		t.Fatalf("later snapshot: enqueued=%d err=%v", enqueued, err)
	}
}

func TestDiscoveryDoesNotQueueNewUntrustedAuthor(t *testing.T) {
	temp := t.TempDir()
	fakeBin := installProcessHelpers(t, temp)
	t.Setenv("GO_WANT_REVIEWCTL_HELPER", "1")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	discoveryDir := filepath.Join(temp, "discovery")
	if err := os.Mkdir(discoveryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_DISCOVERY_DIR", discoveryDir)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	store := openTestStore(t)
	cfg := Config{TrustedAuthors: []string{"alice"}, Repositories: []Repository{repository}}
	writeDiscoverySnapshots(t, discoveryDir, repository.Repository, nil)
	discoverRepositories(context.Background(), cfg, store)
	writeDiscoverySnapshots(t, discoveryDir, repository.Repository, []fakeDiscoveryPR{{
		URL: "https://github.com/acme/service/pull/1", Number: 1, State: "OPEN", HeadRefOID: "a",
		Author: fakeDiscoveryAuthor{Login: "mallory"},
	}})

	result := discoverRepositories(context.Background(), cfg, store)
	if len(result) != 1 || result[0].Status != "success" || result[0].Observed != 1 || result[0].Enqueued != 0 {
		t.Fatalf("untrusted discovery = %+v", result)
	}
}

func TestDiscoveryQueuesPullRequestAfterAuthorBecomesTrusted(t *testing.T) {
	temp := t.TempDir()
	fakeBin := installProcessHelpers(t, temp)
	t.Setenv("GO_WANT_REVIEWCTL_HELPER", "1")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	discoveryDir := filepath.Join(temp, "discovery")
	if err := os.Mkdir(discoveryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_DISCOVERY_DIR", discoveryDir)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	store := openTestStore(t)
	pullRequest := fakeDiscoveryPR{
		URL: "https://github.com/acme/service/pull/1", Number: 1, State: "OPEN", HeadRefOID: "a",
		Author: fakeDiscoveryAuthor{Login: "mallory"},
	}
	writeDiscoverySnapshots(t, discoveryDir, repository.Repository, []fakeDiscoveryPR{pullRequest})
	discoverRepositories(context.Background(), Config{
		TrustedAuthors: []string{"alice"}, Repositories: []Repository{repository},
	}, store)

	result := discoverRepositories(context.Background(), Config{
		TrustedAuthors: []string{"mallory"}, Repositories: []Repository{repository},
	}, store)
	if len(result) != 1 || result[0].Status != "success" || result[0].Enqueued != 1 {
		t.Fatalf("trusted discovery = %+v", result)
	}
}

func TestStoreRollsBackDiscoveryQueueAndBaselineTogether(t *testing.T) {
	store := openTestStore(t)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	first := discoverySnapshot(repository.Repository, 1, "a", false)
	if _, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{first}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER fail_baseline BEFORE INSERT ON pull_request_baselines
		WHEN NEW.change_number = 2 BEGIN SELECT RAISE(ABORT, 'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	changed := discoverySnapshot(repository.Repository, 1, "b", false)
	newReady := discoverySnapshot(repository.Repository, 2, "a", false)
	if _, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{changed, newReady}); err == nil {
		t.Fatal("expected baseline insert to fail")
	}
	queue, err := store.Snapshot(context.Background())
	if err != nil || len(queue) != 0 {
		t.Fatalf("failed transaction changed queue: queue=%+v err=%v", queue, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_baseline`); err != nil {
		t.Fatal(err)
	}
	if enqueued, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{changed}); err != nil || enqueued != 1 {
		t.Fatalf("rolled-back baseline was advanced: enqueued=%d err=%v", enqueued, err)
	}
}

func discoverySnapshot(repository string, number int64, head string, draft bool) pullRequestSnapshot {
	return pullRequestSnapshot{
		PullRequest: PullRequest{
			Provider: "github", Repository: repository, Number: number,
			URL: "https://github.com/" + repository + "/pull/" + strconv.FormatInt(number, 10),
		},
		HeadSHA: head,
		IsDraft: draft,
	}
}
