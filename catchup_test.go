package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func catchUpPublication(pr PullRequest, threads []githubReviewThread) Publication {
	const head = "0123456789abcdef0123456789abcdef01234567"
	const digest = "sha256:test"
	return Publication{
		Attempt: Attempt{PullRequest: pr, HeadSHA: head, SkillDigest: digest, StartedAt: time.Now().UTC()},
		Draft: ReviewDraft{
			Provider: pr.Provider, Repository: pr.Repository, Number: pr.Number, HeadSHA: head,
			SkillDigest: digest, Verdict: "APPROVE", Body: "Review complete.\n\n" + reviewMarker(pr, head, digest),
			Comments: []ReviewComment{}, Discussions: []DiscussionAction{},
		},
		Login: "reviewer", Threads: threads, RecoverAcrossHeads: true,
	}
}

func TestRoundPublicationUsesReviewedCommitForStaleComment(t *testing.T) {
	cfg, pr, _, temp := publicationFixture(t)
	ctx := context.Background()
	publication := catchUpPublication(pr, nil)
	counterPath := filepath.Join(temp, "head-counter")
	if err := os.WriteFile(counterPath, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_HEAD_COUNTER", counterPath)
	observed, err := resolveGitHub(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := deliverPublication(ctx, cfg, publication); err == nil || result.ReviewID != "" {
		t.Fatalf("deliverPublication(outdated approval) = %+v, %v, want an error before any review was posted", result, err)
	}
	result, err := deliverPublication(ctx, cfg, publication.forObservedHead(observed.HeadSHA))
	if err != nil || !result.Success || result.Verdict != "COMMENT" || result.HeadSHA != publication.Attempt.HeadSHA {
		t.Fatalf("deliverPublication(stale comment) = %+v, %v, want COMMENT on %s", result, err, publication.Attempt.HeadSHA)
	}
	var request struct {
		Commit string `json:"commit_id"`
		Event  string `json:"event"`
		Body   string `json:"body"`
	}
	data, err := os.ReadFile(filepath.Join(temp, "published.request.json"))
	if err != nil || json.Unmarshal(data, &request) != nil || request.Commit != result.HeadSHA || request.Event != "COMMENT" ||
		!strings.Contains(request.Body, "covers commit `"+result.HeadSHA+"`") ||
		!strings.Contains(request.Body, "reported `"+observed.HeadSHA+"` as the head") {
		t.Errorf("review POST = %s, %v, want a COMMENT on %s identifying observed head %s", data, err, result.HeadSHA, observed.HeadSHA)
	}
	if posts := readTestCounter(filepath.Join(temp, "published.posts")); posts != 1 {
		t.Errorf("review posts = %d, want 1", posts)
	}
}

func TestRoundPublicationRecoversReplyAcrossPush(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_LOST_REPLY_RESPONSE", "1")
	t.Setenv("REVIEWCTL_FAKE_HEAD_AFTER_REPLY", "1")
	ctx := context.Background()
	publication := catchUpPublication(pr, fakeGHThreads())
	threadID := publication.Threads[0].ID
	publication.Draft.Discussions = []DiscussionAction{{ThreadID: threadID, Action: "resolved_with_reply", Body: "Fix verified."}}
	if err := store.savePublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	first := ProcessAttempt(ctx, cfg, store, pr)
	if first.Success || first.ReviewID == "" {
		t.Fatalf("ProcessAttempt(lost reply response) = %+v, want interrupted publication after review POST", first)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	result := ProcessAttempt(ctx, cfg, store, pr)
	if !result.Success || !result.Recovered || result.HeadSHA != publication.Attempt.HeadSHA || result.ReviewID != first.ReviewID {
		t.Fatalf("ProcessAttempt(push after reply) = %+v, want recovered review %s for %s", result, first.ReviewID, publication.Attempt.HeadSHA)
	}
	threads := fakeGHThreads()
	if len(threads) != 1 || !threads[0].IsResolved || len(threads[0].Comments) != 2 {
		t.Errorf("thread readback = %+v, want resolved thread with one reply", threads)
	}
	if len(result.DiscussionOutcomes) != 1 || result.DiscussionOutcomes[0].ThreadID != threadID || result.DiscussionOutcomes[0].ReplyID == "" {
		t.Errorf("discussion outcomes = %+v, want the verified reply to %s", result.DiscussionOutcomes, threadID)
	}
	if posts := readTestCounter(filepath.Join(temp, "published.posts")); posts != 1 {
		t.Errorf("review posts = %d, want 1", posts)
	}
}

func TestRoundPublicationLeavesLaterAuthorFeedbackOpen(t *testing.T) {
	cfg, pr, _, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_LOST_REPLY_RESPONSE", "1")
	ctx := context.Background()
	publication := catchUpPublication(pr, fakeGHThreads())
	threadID := publication.Threads[0].ID
	publication.Draft.Discussions = []DiscussionAction{{ThreadID: threadID, Action: "resolved_with_reply", Body: "Fix verified."}}
	first, err := deliverPublication(ctx, cfg, publication)
	if err == nil || first.ReviewID == "" {
		t.Fatalf("deliverPublication(lost reply response) = %+v, %v, want interrupted publication after review POST", first, err)
	}
	threads := fakeGHThreads()
	threads[0].Comments = append(threads[0].Comments, githubReviewComment{
		DatabaseID: 900, Author: "dependabot[bot]", ReplyToID: threads[0].Comments[0].DatabaseID,
		Body: "Please reconsider how the caller handles the returned error.",
	})
	data, err := json.Marshal(threads)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temp, "published.threads.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := deliverPublication(ctx, cfg, publication)
	if err != nil || !result.Success || !result.Recovered || len(result.DiscussionOutcomes) != 0 {
		t.Fatalf("deliverPublication(after author feedback) = %+v, %v, want existing review and no verified discussion mutation", result, err)
	}
	threads = fakeGHThreads()
	if len(threads) != 1 || threads[0].IsResolved || len(threads[0].Comments) != 3 {
		t.Errorf("thread readback = %+v, want an open thread with original, controller, and author comments", threads)
	}
	if posts := readTestCounter(filepath.Join(temp, "published.posts")); posts != 1 {
		t.Errorf("review posts = %d, want 1", posts)
	}
}
