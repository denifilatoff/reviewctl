package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControllerKeepsRejectionDiagnosticUntilNextAnalysis(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_REJECT_POST", "1")
	ctx := context.Background()
	rejected := ProcessAttempt(ctx, cfg, store, pr)
	if rejected.Success || rejected.ErrorCode != "draft_rejected" {
		t.Fatalf("rejected draft = %+v, want draft_rejected", rejected)
	}
	if err := store.Finish(ctx, rejected); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_REJECT_POST", "")
	t.Setenv("REVIEWCTL_FAKE_DOCTOR_APM_FAIL", "1")
	blocked := ProcessAttempt(ctx, cfg, store, pr)
	if blocked.Success || blocked.ErrorCode != "apm_failed" || blocked.Cost != nil {
		t.Fatalf("setup failure = %+v, want apm_failed without model invocation", blocked)
	}
	if err := store.Finish(ctx, blocked); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_DOCTOR_APM_FAIL", "")
	t.Setenv("REVIEWCTL_FAKE_RECEIPT_HEAD", "different-head")
	replacement := ProcessAttempt(ctx, cfg, store, pr)
	if replacement.Success || replacement.ErrorCode != "receipt_invalid" || replacement.Cost == nil {
		t.Fatalf("replacement analysis = %+v, want an invoked model with invalid draft scope", replacement)
	}
	instruction, err := os.ReadFile(filepath.Join(temp, "codex-calls.instruction"))
	if err != nil || !strings.Contains(string(instruction), "Previous rejection (diagnostic data)") ||
		!strings.Contains(string(instruction), "comments") {
		t.Errorf("next analysis lost GitHub validation diagnostic: %s, %v", instruction, err)
	}
	if err := store.Finish(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_RECEIPT_HEAD", "")
	last := ProcessAttempt(ctx, cfg, store, pr)
	if !last.Success {
		t.Fatalf("subsequent analysis = %+v, want success", last)
	}
	instruction, err = os.ReadFile(filepath.Join(temp, "codex-calls.instruction"))
	if err != nil || strings.Contains(string(instruction), "Previous rejection (diagnostic data)") {
		t.Errorf("consumed rejection leaked into later analysis: %s, %v", instruction, err)
	}
	calls, _ := os.ReadFile(filepath.Join(temp, "codex-calls"))
	if string(calls) != "3" || readTestCounter(filepath.Join(temp, "published.posts")) != 1 {
		t.Errorf("analysis calls=%q, review posts=%d; want 3 analyses and 1 published review", calls, readTestCounter(filepath.Join(temp, "published.posts")))
	}
}

func TestControllerDefersRecoveryCostWhenAccountingLookupFails(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		t.Run(fmt.Sprintf("original_recorded_%t", recorded), func(t *testing.T) {
			cfg, pr, store, temp := publicationFixture(t)
			t.Setenv("REVIEWCTL_FAKE_LOST_POST_RESPONSE", "1")
			ctx := context.Background()
			first := ProcessAttempt(ctx, cfg, store, pr)
			if first.Success || first.Cost == nil {
				t.Fatalf("interrupted publication = %+v, want original accounting", first)
			}
			if recorded {
				if err := store.Finish(ctx, first); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.ExecContext(ctx, `ALTER TABLE history RENAME TO unavailable_history`); err != nil {
				t.Fatal(err)
			}
			failed := ProcessAttempt(ctx, cfg, store, pr)
			if _, err := store.db.ExecContext(ctx, `ALTER TABLE unavailable_history RENAME TO history`); err != nil {
				t.Fatal(err)
			}
			if failed.Success || failed.ErrorCode != "state_failed" || !strings.Contains(failed.ErrorMessage, "read original publication accounting") {
				t.Fatalf("unavailable accounting history = %+v, want accounting lookup failure", failed)
			}
			if failed.Cost != nil {
				t.Error("failed accounting lookup returned original cost before checking whether it was recorded")
			}
			if err := store.Finish(ctx, failed); err != nil {
				t.Fatal(err)
			}
			pending, err := store.pendingPublication(ctx, pr)
			if err != nil || pending == nil || pending.Attempt.Cost == nil {
				t.Fatalf("pending publication after lookup failure = %+v, %v; want saved original cost", pending, err)
			}
			recovered := ProcessAttempt(ctx, cfg, store, pr)
			wantCost := !recorded
			if !recovered.Success || !recovered.Recovered || (recovered.Cost != nil) != wantCost {
				t.Fatalf("recovery (original recorded=%t) = %+v, want original cost only if not recorded", recorded, recovered)
			}
			if err := store.Finish(ctx, recovered); err != nil {
				t.Fatal(err)
			}
			var costs int
			if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM history WHERE cost_json != ''`).Scan(&costs); err != nil {
				t.Fatal(err)
			}
			calls, _ := os.ReadFile(filepath.Join(temp, "codex-calls"))
			if costs != 1 || string(calls) != "1" {
				t.Errorf("cost history rows=%d, analysis calls=%q; want 1 each", costs, calls)
			}
		})
	}
}

func TestControllerChecksHeadAfterMarkerRecovery(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if !first.Success {
		t.Fatal(first.ErrorMessage)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_HEAD_COUNTER", filepath.Join(temp, "head-counter"))
	recovered := ProcessAttempt(ctx, cfg, store, pr)
	current, err := resolveGitHub(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Success || recovered.ErrorCode != "head_changed" || recovered.HeadSHA == current.HeadSHA {
		t.Errorf("ProcessAttempt(recovery with concurrent push) = success for %s, current head %s; want head_changed", recovered.HeadSHA, current.HeadSHA)
	}
}

func TestControllerRejectsStaleDiscussionBeforeReviewPost(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	const head = "0123456789abcdef0123456789abcdef01234567"
	const digest = "sha256:test"
	before := githubReviewThread{ID: "PRRT_owned", Comments: []githubReviewComment{{DatabaseID: 1, Author: "reviewer", Body: "Original finding."}}}
	publication := Publication{
		Attempt: Attempt{PullRequest: pr, HeadSHA: head, SkillDigest: digest, StartedAt: time.Now().UTC()},
		Login:   "reviewer", Threads: []githubReviewThread{before},
		Draft: ReviewDraft{Provider: pr.Provider, Repository: pr.Repository, Number: pr.Number, HeadSHA: head, SkillDigest: digest,
			Verdict: "APPROVE", Body: "Result: APPROVE\n\n" + reviewMarker(pr, head, digest), Comments: []ReviewComment{},
			Discussions: []DiscussionAction{{ThreadID: before.ID, Action: "resolved_with_reply", Body: "Finding is resolved."}}},
	}
	if err := store.savePublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	after := before
	after.Comments = append(after.Comments, githubReviewComment{DatabaseID: 2, Author: "human", ReplyToID: 1, Body: "The proposed fix still loses data."})
	data, _ := json.Marshal([]githubReviewThread{after})
	if err := os.WriteFile(filepath.Join(temp, "published.threads.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	result := ProcessAttempt(ctx, cfg, store, pr)
	if result.ErrorCode != "discussion_changed" {
		t.Fatalf("ProcessAttempt(stale discussion) = %+v, want discussion_changed", result)
	}
	if _, err := os.Stat(filepath.Join(temp, "published.review.json")); !os.IsNotExist(err) {
		t.Errorf("ProcessAttempt(stale discussion) published review %s before rejecting its stale plan; want no review POST", result.ReviewID)
	}
}

func TestControllerPreservesConflictingReassessment(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "preserved")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if !first.Success {
		t.Fatal(first.ErrorMessage)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	threads := fakeGHThreads()
	threads[0].Comments = append(threads[0].Comments, githubReviewComment{
		DatabaseID: 3, Author: "human", ReplyToID: 1, Body: "A separate data-loss defect exists on this unchanged commit."})
	data, _ := json.Marshal(threads)
	if err := os.WriteFile(filepath.Join(temp, "published.threads.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_VERDICT", "REQUEST_CHANGES")
	t.Setenv("REVIEWCTL_FAKE_INLINE_COMMENT", "1")
	if _, err := store.EnqueueMany(ctx, []PullRequest{pr}); err != nil {
		t.Fatal(err)
	}
	second := ProcessAttempt(ctx, cfg, store, pr)
	calls, _ := os.ReadFile(filepath.Join(temp, "codex-calls"))
	if string(calls) != "2" {
		t.Fatalf("analysis calls = %q, want 2", calls)
	}
	if second.Success || second.ErrorCode != "review_conflict" {
		t.Errorf("ProcessAttempt(reassessment with a new blocker) = %+v, want review_conflict", second)
	}
	pending, err := store.pendingPublication(ctx, pr)
	if err != nil || pending == nil {
		t.Fatalf("pending reassessment = %+v, %v; want retained draft", pending, err)
	}
	if pending.Draft.Verdict != "REQUEST_CHANGES" || len(pending.Draft.Comments) != 1 {
		t.Errorf("pending reassessment = %+v, want new verdict and finding", pending.Draft)
	}
	if err := store.Finish(ctx, second); err != nil {
		t.Fatal(err)
	}
	queue, err := store.Snapshot(ctx)
	if err != nil || len(queue) != 1 || queue[0] != pr {
		t.Errorf("queue after review conflict = %+v, %v; want the PR retained", queue, err)
	}
	retried := ProcessAttempt(ctx, cfg, store, pr)
	if retried.Success || retried.ErrorCode != "review_conflict" || retried.Cost != nil {
		t.Errorf("retry conflicting reassessment = %+v, want retained conflict without new cost", retried)
	}
	calls, _ = os.ReadFile(filepath.Join(temp, "codex-calls"))
	if string(calls) != "2" {
		t.Errorf("analysis calls after conflict retry = %q, want 2", calls)
	}
	posts, _ := os.ReadFile(filepath.Join(temp, "published.posts"))
	if string(posts) != "1" {
		t.Errorf("review posts after conflict = %q, want 1", posts)
	}

}

func TestControllerKeepsDiscussionOpenAfterInterveningReply(t *testing.T) {
	cfg, pr, store, _ := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "resolved_with_reply")
	t.Setenv("REVIEWCTL_FAKE_HUMAN_REPLY_AFTER_REPLY", "1")
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	threads := fakeGHThreads()
	if len(threads) != 1 || len(threads[0].Comments) != 3 {
		t.Fatalf("discussion after the simulated human reply = %+v, want the original, controller reply, and human objection", threads)
	}
	if result.Success || result.ErrorCode != "discussion_changed" || threads[0].IsResolved {
		t.Errorf("ProcessAttempt(human objection before resolve) = success %v, resolved true; want discussion_changed with the thread open", result.Success)
	}
}

func TestControllerRetiresMissingReviewForNewHead(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "preserved")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if !first.Success {
		t.Fatal(first.ErrorMessage)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}

	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "resolved_with_reply")
	t.Setenv("REVIEWCTL_FAKE_LOST_REPLY_RESPONSE", "1")
	second := ProcessAttempt(ctx, cfg, store, pr)
	if second.Success || second.ErrorCode != "publication_failed" {
		t.Fatalf("second: %+v", second)
	}
	if err := store.Finish(ctx, second); err != nil {
		t.Fatal(err)
	}
	pending, err := store.pendingPublication(ctx, pr)
	if err != nil || pending == nil || pending.ExistingReviewID != 123 {
		t.Fatalf("pending: %+v, %v", pending, err)
	}

	file := filepath.Join(temp, "published.review.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var review submittedReview
	if err := json.Unmarshal(data, &review); err != nil {
		t.Fatal(err)
	}
	review.Body = "Manually edited summary."
	data, _ = json.Marshal(review)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_HEAD_AFTER_POST", "1")
	third := ProcessAttempt(ctx, cfg, store, pr)
	if third.ErrorCode != "head_changed" {
		t.Errorf("new head is blocked by old missing marker: success=%v, code=%s, message=%s", third.Success, third.ErrorCode, third.ErrorMessage)
	}
	if err := store.Finish(ctx, third); err != nil {
		t.Fatal(err)
	}
	pending, err = store.pendingPublication(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Error("old-head publication remains queued and blocks reassessment")
	}
}

func TestControllerPreservesNewGeneralFinding(t *testing.T) {
	cfg, pr, store, _ := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "preserved")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if !first.Success {
		t.Fatal(first.ErrorMessage)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_GENERAL_FINDING", "1")
	result := ProcessAttempt(ctx, cfg, store, pr)
	if result.Success || result.ErrorCode != "review_conflict" {
		t.Errorf("ProcessAttempt(new general finding) = %+v, want review_conflict", result)
	}
	pending, err := store.pendingPublication(ctx, pr)
	if err != nil || pending == nil {
		t.Fatalf("pending general finding = %+v, %v; want retained draft", pending, err)
	}
	if pending.Draft.Body != "A new general finding outside the diff." {
		t.Errorf("retained summary = %q, want the new general finding", pending.Draft.Body)
	}
}

func TestControllerKeepsDiscussionOpenAfterReplyHeadChange(t *testing.T) {
	cfg, pr, store, _ := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "resolved_with_reply")
	t.Setenv("REVIEWCTL_FAKE_HEAD_AFTER_REPLY", "1")
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	threads := fakeGHThreads()
	if len(threads) != 1 || len(threads[0].Comments) != 2 {
		t.Fatalf("discussion after reply = %+v, want the original and controller reply", threads)
	}
	if result.Success || result.ErrorCode != "head_changed" || threads[0].IsResolved {
		t.Errorf("ProcessAttempt(push after reply) = %+v, resolved=%v; want head_changed without resolving", result, threads[0].IsResolved)
	}
}

func TestControllerBatchesPreservedDiscussions(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "preserved")
	countFile := filepath.Join(temp, "thread-reads")
	t.Setenv("REVIEWCTL_FAKE_THREAD_READS", countFile)
	threads := make([]githubReviewThread, 100)
	for i := range threads {
		threads[i] = githubReviewThread{ID: fmt.Sprintf("PRRT_%d", i),
			Comments: []githubReviewComment{{DatabaseID: int64(i + 1), Author: "reviewer", Body: "Original finding."}}}
	}
	data, _ := json.Marshal(threads)
	if err := os.WriteFile(filepath.Join(temp, "published.threads.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	if !result.Success {
		t.Fatalf("ProcessAttempt(100 preserved discussions) = %+v, want success", result)
	}
	if reads := readTestCounter(countFile); reads > 4 {
		t.Errorf("full discussion reads for 100 preserved actions = %d, want at most 4", reads)
	}
}

func TestControllerRetiresConflictingReassessmentForNewHead(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "preserved")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if !first.Success {
		t.Fatal(first.ErrorMessage)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueMany(ctx, []PullRequest{pr}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_VERDICT", "REQUEST_CHANGES")
	conflict := ProcessAttempt(ctx, cfg, store, pr)
	if conflict.Success || conflict.ErrorCode != "review_conflict" {
		t.Fatalf("changed verdict without inline findings = %+v, want review_conflict", conflict)
	}
	if err := store.Finish(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_HEAD_AFTER_POST", "1")
	changed := ProcessAttempt(ctx, cfg, store, pr)
	if changed.Success || changed.ErrorCode != "head_changed" || changed.Cost != nil {
		t.Errorf("retry conflict after push = %+v, want head_changed without duplicate cost", changed)
	}
	if err := store.Finish(ctx, changed); err != nil {
		t.Fatal(err)
	}
	pending, err := store.pendingPublication(ctx, pr)
	if err != nil || pending != nil {
		t.Errorf("publication after new head = %+v, %v; want no old draft", pending, err)
	}
	queue, err := store.Snapshot(ctx)
	if err != nil || len(queue) != 1 || queue[0] != pr {
		t.Errorf("queue after new head = %+v, %v; want the PR retained", queue, err)
	}
	calls, _ := os.ReadFile(filepath.Join(temp, "codex-calls"))
	if string(calls) != "2" {
		t.Errorf("analysis calls before old-draft retirement = %q, want 2", calls)
	}
}

func TestReadGitHubReviewThreadRejectsMissingNode(t *testing.T) {
	publicationFixture(t)
	_, err := readGitHubReviewThread(context.Background(), "PRRT_missing")
	var coded *codedError
	if !errors.As(err, &coded) || coded.code != "discussion_changed" {
		t.Errorf("readGitHubReviewThread(missing) = %v, want discussion_changed", err)
	}
}
