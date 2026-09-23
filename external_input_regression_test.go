package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExternalInputRejectsMissingConnection(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' '{"data":{"viewer":{"login":"reviewer"},"repository":{"pullRequest":{"body":"description"}}}}'
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	if input, err := readGitHubExternalInput(context.Background(), pr); err == nil {
		t.Fatalf("input=%+v, want error for absent conversation connections", input)
	}
}

func TestExternalInputRejectsMissingNextPage(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *ExternalInputInitial*)
    printf '%s\n' '{"data":{"viewer":{"login":"reviewer"},"repository":{"pullRequest":{"body":"description","comments":{"nodes":[{"id":"A","body":"message","author":{"login":"alice","__typename":"User"}}],"pageInfo":{"hasNextPage":true,"endCursor":"C1"}},"reviews":{"nodes":[],"pageInfo":{"hasNextPage":false}},"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}'
    ;;
  *) printf '%s\n' '{"data":{"repository":{"pullRequest":{"comments":{"nodes":[],"pageInfo":{}}}}}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	if input, err := readGitHubExternalInput(context.Background(), pr); err == nil {
		t.Fatalf("input=%+v, want error for absent requested page", input)
	}
}

func TestExternalInputRejectsRepeatedPageCursor(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf x >> "$REVIEW_PROBE_CALLS"
printf '%s\n' '{"data":{"repository":{"pullRequest":{"comments":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"C1"}}}}}}'
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REVIEW_PROBE_CALLS", countFile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var input githubExternalInput
	hasNext := true
	err := appendExternalMessagePages(ctx, &input, "reviewer", externalInputIssueCommentsQuery, "comments",
		githubExternalPageInfo{HasNextPage: &hasNext, EndCursor: "C1"}, nil)
	calls, readErr := os.ReadFile(countFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err == nil || len(calls) > 1 {
		t.Fatalf("requests=%d, err=%v; want one request and repeated-cursor error", len(calls), err)
	}
}

func TestDiscoveryIgnoresEditToObservedEmptyReview(t *testing.T) {
	store := openTestStore(t)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	current := discoverySnapshot(repository.Repository, 1, "head", false)
	messages := []githubExternalMessage{{ID: "R1", Author: &githubExternalActor{Login: "alice", Typename: "User"}}}
	observe := func() int {
		t.Helper()
		input := githubExternalInput{Body: "description"}
		if err := appendExternalMessages(&input, "reviewer", messages); err != nil {
			t.Fatal(err)
		}
		current.DescriptionDigest, current.MessageIDs, current.ObservedMessageIDs, current.InputRevision =
			mergeExternalInputState(nil, nil, input)
		n, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{current})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := observe(); n != 0 {
		t.Fatalf("initial enqueued=%d, want 0", n)
	}
	messages[0].Body = "edited"
	if n := observe(); n != 0 {
		t.Fatalf("enqueued=%d after editing existing R1, want 0", n)
	}
}

func TestDiscoveryDraftPreservesObservedMessageIDs(t *testing.T) {
	store := openTestStore(t)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	ready := discoverySnapshot(repository.Repository, 1, "head", false)
	ready.DescriptionDigest, ready.MessageIDs, ready.InputRevision = mergeExternalInput("", nil,
		githubExternalInput{Body: "description", MessageIDs: []string{"A"}})
	if _, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{ready}); err != nil {
		t.Fatal(err)
	}
	draft := discoverySnapshot(repository.Repository, 1, "head", true)
	if _, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{draft}); err != nil {
		t.Fatal(err)
	}
	var idsJSON string
	if err := store.db.QueryRow(`SELECT message_ids_json FROM pull_request_baselines`).Scan(&idsJSON); err != nil {
		t.Fatal(err)
	}
	if idsJSON != `["A"]` {
		t.Fatalf("message_ids_json=%s, want [\"A\"] after draft transition", idsJSON)
	}
}

func TestInputRaceHistoryKeepsPublishedRevision(t *testing.T) {
	temp := t.TempDir()
	fakeBin := installProcessHelpers(t, temp)
	t.Setenv("GO_WANT_REVIEWCTL_HELPER", "1")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REVIEWCTL_FAKE_STATE", filepath.Join(temp, "published"))
	t.Setenv("REVIEWCTL_FAKE_GIT_STATE", filepath.Join(temp, "git-fetch"))
	t.Setenv("REVIEWCTL_FAKE_LOGIN", "reviewer")
	t.Setenv("REVIEWCTL_FAKE_EXTERNAL_INPUT", "race")
	t.Setenv("REVIEWCTL_FAKE_EXTERNAL_INPUT_COUNTER", filepath.Join(temp, "input-counter"))
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	digest, ids, revision := mergeExternalInput("", nil,
		githubExternalInput{Body: "description", MessageIDs: []string{"A"}})
	pr.InputRevision = revision
	store := openTestStore(t)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	snapshot := pullRequestSnapshot{PullRequest: pr, HeadSHA: "0123456789abcdef0123456789abcdef01234567",
		DescriptionDigest: digest, MessageIDs: ids}
	if _, err := store.ApplyDiscoverySnapshot(context.Background(), repository, []pullRequestSnapshot{snapshot}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueMany(context.Background(), []PullRequest{pr}); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Harness: "codex", Publish: true, TrustedAuthors: []string{"dependabot[bot]"},
		Repositories: []Repository{repository}}
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	if result.ErrorCode != "input_changed" {
		t.Fatalf("result=%+v, want input_changed", result)
	}
	if err := store.Finish(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	_, history, err := store.Status(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var signatureJSON string
	if err := store.db.QueryRow(`SELECT attempt FROM review_signatures`).Scan(&signatureJSON); err != nil {
		t.Fatal(err)
	}
	var signature Attempt
	if err := json.Unmarshal([]byte(signatureJSON), &signature); err != nil {
		t.Fatal(err)
	}
	if history[0].InputRevision != revision || history[0].InputRevision != signature.InputRevision {
		t.Fatalf("history=%s signature=%s reviewed=%s", history[0].InputRevision, signature.InputRevision, revision)
	}
}
