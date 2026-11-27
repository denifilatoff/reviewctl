package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPauseAndPushInOneDiscoveryPoll(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	repo := cfg.Repositories[0]
	headA := discoverySnapshot(pr.Repository, pr.Number, "a", false)
	headB := discoverySnapshot(pr.Repository, pr.Number, "b", false)
	if _, err := store.pollRepositoryReviewInputs(ctx, cfg, repo, []pullRequestSnapshot{headA}); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ApplyDiscoverySnapshot(ctx, repo, []pullRequestSnapshot{headA}); err != nil || count != 0 {
		t.Fatalf("initial discovery = %d, %v; want baseline without queue", count, err)
	}
	writeRoundFixture(t, filepath.Join(temp, "published.issue-comments.json"), [][]map[string]any{{
		roundCommand(1, "/reviewctl pause", "dependabot[bot]"),
		roundCommand(2, "/reviewctl resume", "untrusted"),
	}})
	if _, err := store.pollRepositoryReviewInputs(ctx, cfg, repo, []pullRequestSnapshot{headB}); err != nil {
		t.Fatal(err)
	}
	if count, err := store.ApplyDiscoverySnapshot(ctx, repo, []pullRequestSnapshot{headB}); err != nil || count != 1 {
		t.Fatalf("push discovery = %d, %v; want one wakeup", count, err)
	}
	if allowed, err := store.reviewAllowed(ctx, pr); err != nil || allowed {
		t.Fatalf("allowed after concurrent pause and push = %t, %v; want false", allowed, err)
	}

	writeRoundFixture(t, filepath.Join(temp, "published.issue-comments.json"), [][]map[string]any{{
		roundCommand(1, "/reviewctl pause", "dependabot[bot]"),
		roundCommand(3, "/reviewctl resume", "dependabot[bot]"),
	}})
	path := store.path
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for range 2 {
		if _, err := reopened.pollRepositoryReviewInputs(ctx, cfg, repo, []pullRequestSnapshot{headB}); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := reopened.Snapshot(ctx)
	allowed, allowedErr := reopened.reviewAllowed(ctx, pr)
	if err != nil || allowedErr != nil || len(queue) != 1 || queue[0] != pr || !allowed {
		t.Errorf("after resume replay: queue=%v, allowed=%t, errors=%v/%v; want one runnable PR", queue, allowed, err, allowedErr)
	}
}

func TestManualReviewPublishesHeldPassAndLeavesPRPaused(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	repo := cfg.Repositories[0]
	if _, err := store.pollRepositoryReviewInputs(ctx, cfg, repo, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueMany(ctx, []PullRequest{pr}); err != nil {
		t.Fatal(err)
	}
	commands := [][]map[string]any{{roundCommand(1, "/reviewctl pause", "dependabot[bot]")}}
	payload, _ := json.Marshal(commands)
	t.Setenv("REVIEWCTL_FAKE_COMMAND_AFTER_CODEX", string(payload))
	held := ProcessAttempt(ctx, cfg, store, pr)
	postPath := filepath.Join(temp, "published.posts")
	if !held.Held || held.RoundID != 1 || readTestCounter(postPath) != 0 {
		t.Fatalf("pass completed during pause = %+v, posts=%d; want an unpublished pass", held, readTestCounter(postPath))
	}
	dir := store.passDirectory(held)
	for _, artifact := range []string{"review.json", "pass.json", filepath.Join("evidence", "verification.txt")} {
		if _, err := os.Stat(filepath.Join(dir, artifact)); err != nil {
			t.Errorf("saved pass artifact %s: %v", artifact, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "axis.json")); !os.IsNotExist(err) {
		t.Errorf("intermediate axis report: stat error=%v, want absent", err)
	}

	t.Setenv("REVIEWCTL_FAKE_COMMAND_AFTER_CODEX", "")
	commands[0] = append(commands[0], roundCommand(2, "/reviewctl review", "dependabot[bot]"),
		roundCommand(3, "/reviewctl review", "dependabot[bot]"))
	inputPath := filepath.Join(temp, "published.issue-comments.json")
	writeRoundFixture(t, inputPath, commands)
	review := ProcessAttempt(ctx, cfg, store, pr)
	if !review.Success || review.RoundID != held.RoundID || readTestCounter(postPath) != 1 {
		t.Fatalf("manual review = %+v, posts=%d; want the held round published once", review, readTestCounter(postPath))
	}
	if err := store.Finish(ctx, review); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.reviewAllowed(ctx, pr); err != nil || allowed {
		t.Errorf("allowed after manual review = %t, %v; want false", allowed, err)
	}
	commands[0] = append(commands[0], roundCommand(4, "/reviewctl resume", "dependabot[bot]"))
	writeRoundFixture(t, inputPath, commands)
	snapshot := discoverySnapshot(pr.Repository, pr.Number, review.HeadSHA, false)
	if _, err := store.pollRepositoryReviewInputs(ctx, cfg, repo, []pullRequestSnapshot{snapshot}); err != nil {
		t.Fatal(err)
	}
	queue, err := store.Snapshot(ctx)
	allowed, allowedErr := store.reviewAllowed(ctx, pr)
	if err != nil || allowedErr != nil || len(queue) != 0 || !allowed {
		t.Errorf("idle resume: queue=%v, allowed=%t, errors=%v/%v; want empty queue and automation enabled", queue, allowed, err, allowedErr)
	}
}

func TestAuthorReplySchedulesSameHeadRoundOnce(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	repo := cfg.Repositories[0]
	snapshot := discoverySnapshot(pr.Repository, pr.Number, "a", false)
	snapshot.Author = "dependabot[bot]"
	if _, err := store.pollRepositoryReviewInputs(ctx, cfg, repo, nil); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(time.Second)
	root := githubReviewComment{DatabaseID: 1, Author: "reviewer", ReviewBody: (Attempt{PullRequest: pr, RoundID: 1, RoundKey: "fixture-round"}).publicationMarker()}
	reply := githubReviewComment{DatabaseID: 2, Author: snapshot.Author, ReplyToID: 1, Body: "This edge case still fails.", CreatedAt: created}
	writeRoundFixture(t, filepath.Join(temp, "published.threads.json"), []githubReviewThread{
		{ID: "owned", Comments: []githubReviewComment{root, reply}},
	})
	writeRoundFixture(t, filepath.Join(temp, "published.review-inputs.json"), [][]map[string]any{{{
		"id": 2, "created_at": created, "body": reply.Body, "in_reply_to_id": 1,
		"user": map[string]string{"login": snapshot.Author}, "pull_request_url": "https://api.github.com/repos/acme/service/pulls/7",
	}}})
	if added, err := store.pollRepositoryReviewInputs(ctx, cfg, repo, []pullRequestSnapshot{snapshot}); err != nil || added != 1 {
		t.Fatalf("new same-head author reply added %d PRs, err=%v; want one", added, err)
	}
	if _, err := store.startReviewRound(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if added, err := store.pollRepositoryReviewInputs(ctx, cfg, repo, []pullRequestSnapshot{snapshot}); err != nil || added != 0 {
		t.Errorf("replayed author reply added %d PRs, err=%v; want zero", added, err)
	}
	state, _, err := readReviewControl(ctx, store.db, pr)
	if err != nil || state.Requested {
		t.Errorf("replayed reply state=%+v, err=%v; want no follow-up request", state, err)
	}
}

func TestLegacyPreparedPublicationAndNewRoundUseDistinctMarkers(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	resolved, err := resolveGitHub(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	legacy := Attempt{PullRequest: pr, HeadSHA: resolved.HeadSHA, SkillDigest: "sha256:legacy", StartedAt: time.Now().UTC()}
	draft := ReviewDraft{Provider: pr.Provider, Repository: pr.Repository, Number: pr.Number, HeadSHA: legacy.HeadSHA,
		SkillDigest: legacy.SkillDigest, Verdict: "APPROVE", Body: "Legacy review\n\n" + legacy.publicationMarker(),
		Comments: []ReviewComment{}, Discussions: []DiscussionAction{}}
	if err := store.savePublication(ctx, Publication{Attempt: legacy, Draft: draft, Login: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	original := ProcessAttempt(ctx, cfg, store, pr)
	if !original.Success || original.RoundID != 0 {
		t.Fatalf("legacy delivery = %+v, want a successful legacy review", original)
	}
	if err := store.Finish(ctx, original); err != nil {
		t.Fatal(err)
	}
	modern := ProcessAttempt(ctx, cfg, store, pr)
	if !modern.Success || modern.RoundID != 1 || modern.RoundKey == "" || modern.ReviewID == original.ReviewID ||
		modern.HeadSHA != original.HeadSHA || readTestCounter(filepath.Join(temp, "codex-calls")) != 1 {
		t.Errorf("new same-SHA round = %+v; want a new review and one agent invocation after legacy review %s", modern, original.ReviewID)
	}
}

func roundCommand(id int64, body, author string) map[string]any {
	return map[string]any{
		"id": id, "created_at": time.Now().UTC().Add(time.Duration(id) * time.Second),
		"body": body, "user": map[string]string{"login": author},
		"issue_url": fmt.Sprintf("https://api.github.com/repos/%s/issues/%d", "acme/service", 7),
	}
}

func writeRoundFixture(t *testing.T, path string, data any) {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
