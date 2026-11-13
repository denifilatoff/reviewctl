package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerReopensResolvedDiscussion(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "open_with_reply")
	t.Setenv("REVIEWCTL_FAKE_VERDICT", "REQUEST_CHANGES")
	threads := []githubReviewThread{{ID: "PRRT_owned", IsResolved: true,
		Comments: []githubReviewComment{{DatabaseID: 1, Author: "reviewer", Body: "Original finding."}}}}
	data, err := json.Marshal(threads)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temp, "published.threads.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	if !result.Success || result.Verdict != "REQUEST_CHANGES" {
		t.Fatalf("ProcessAttempt(reopen resolved discussion) = %+v, want successful review", result)
	}
	current := fakeGHThreads()
	if len(current) != 1 || current[0].IsResolved || len(current[0].Comments) != 2 {
		t.Fatalf("reopened discussion = %+v, want open with exactly one reply", current)
	}
	reply := current[0].Comments[1]
	if reply.Author != "reviewer" || reply.ReplyToID != 1 ||
		!strings.HasPrefix(reply.Body, "Verified the updated implementation.\n\n<!-- reviewctl:discussion:") {
		t.Errorf("reopening reply = %+v, want the drafted reply addressed to the owned root", reply)
	}
}

func TestControllerSynchronizesOwnedThreadsIndependently(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "resolved_with_reply")
	threads := []githubReviewThread{
		{ID: "PRRT_first", Comments: []githubReviewComment{{DatabaseID: 11, Author: "reviewer", Body: "First finding."}}},
		{ID: "PRRT_foreign", Comments: []githubReviewComment{{DatabaseID: 12, Author: "human", Body: "Human finding."}}},
		{ID: "PRRT_second", Comments: []githubReviewComment{{DatabaseID: 13, Author: "reviewer", Body: "Second finding."}}},
	}
	data, err := json.Marshal(threads)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temp, "published.threads.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	if !result.Success {
		t.Fatalf("ProcessAttempt(two owned discussions) = %+v, want success", result)
	}
	current := fakeGHThreads()
	if len(current) != len(threads) {
		t.Fatalf("discussion count = %d, want %d", len(current), len(threads))
	}
	for i, thread := range current {
		if i == 1 {
			if thread.IsResolved || len(thread.Comments) != 1 || thread.Comments[0] != threads[i].Comments[0] {
				t.Errorf("foreign discussion changed: %+v", thread)
			}
			continue
		}
		if !thread.IsResolved || len(thread.Comments) != 2 {
			t.Errorf("owned discussion outcome = %+v, want resolved with one reply", thread)
			continue
		}
		if reply := thread.Comments[1]; reply.ReplyToID != threads[i].Comments[0].DatabaseID || reply.Author != "reviewer" {
			t.Errorf("reply addressed incorrectly: %+v", reply)
		}
	}
}

func TestControllerReassessesRejectedDiscussionReply(t *testing.T) {
	for _, status := range []string{"400", "422"} {
		t.Run(status, func(t *testing.T) {
			cfg, pr, store, temp := publicationFixture(t)
			t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
			t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "resolved_with_reply")
			t.Setenv("REVIEWCTL_FAKE_REJECT_REPLY", "1")
			t.Setenv("REVIEWCTL_FAKE_REPLY_STATUS", status)
			ctx := context.Background()
			if _, err := store.EnqueueMany(ctx, []PullRequest{pr}); err != nil {
				t.Fatal(err)
			}
			first := ProcessAttempt(ctx, cfg, store, pr)
			if first.Success || first.ErrorCode != "draft_rejected" || first.ReviewID != "123" {
				t.Errorf("ProcessAttempt(rejected discussion reply %s) = %+v, want draft_rejected with existing review", status, first)
			}
			if err := store.Finish(ctx, first); err != nil {
				t.Fatal(err)
			}
			pending, err := store.pendingPublication(ctx, pr)
			if err != nil || pending != nil {
				t.Errorf("pending rejected reply = %+v, %v; want retired draft", pending, err)
			}
			t.Setenv("REVIEWCTL_FAKE_REJECT_REPLY", "")
			second := ProcessAttempt(ctx, cfg, store, pr)
			if !second.Success || !second.Recovered || second.Cost == nil {
				t.Errorf("ProcessAttempt(corrected reply) = %+v, want successful reassessment with new accounting", second)
			}
			calls, _ := os.ReadFile(filepath.Join(temp, "codex-calls"))
			if string(calls) != "2" || readTestCounter(filepath.Join(temp, "published.posts")) != 1 {
				t.Errorf("reassessment calls=%q, review posts=%d; want 2 analyses and 1 review", calls, readTestCounter(filepath.Join(temp, "published.posts")))
			}
			instruction, _ := os.ReadFile(filepath.Join(temp, "codex-calls.instruction"))
			if !strings.Contains(string(instruction), "Previous rejection (diagnostic data)") || !strings.Contains(string(instruction), "body") {
				t.Errorf("reassessment instruction lacks reply validation diagnostic: %s", instruction)
			}
		})
	}
}

func TestControllerRecoversReplyDespiteValidationErrorResponse(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "resolved_with_reply")
	t.Setenv("REVIEWCTL_FAKE_REJECT_REPLY_AFTER_CREATE", "1")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if first.Success || first.ErrorCode != "publication_failed" {
		t.Fatalf("created reply with error response = %+v, want uncertain publication failure", first)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_REJECT_REPLY_AFTER_CREATE", "")
	second := ProcessAttempt(ctx, cfg, store, pr)
	if !second.Success || !second.Recovered || second.Cost != nil {
		t.Errorf("retry created reply = %+v, want recovered success without duplicate cost", second)
	}
	calls, _ := os.ReadFile(filepath.Join(temp, "codex-calls"))
	if string(calls) != "1" || readTestCounter(filepath.Join(temp, "published.reply-attempts")) != 1 {
		t.Errorf("created-reply recovery: analysis calls=%q, reply attempts=%d; want 1 each", calls, readTestCounter(filepath.Join(temp, "published.reply-attempts")))
	}
}

func TestControllerRetainsRejectedReplyWhenReadbackFails(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "resolved_with_reply")
	t.Setenv("REVIEWCTL_FAKE_REJECT_REPLY", "1")
	t.Setenv("REVIEWCTL_FAKE_REPLY_READBACK_FAILURE", "1")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if first.Success || first.ErrorCode != "publication_failed" {
		t.Fatalf("rejected reply without readback = %+v, want uncertain publication failure", first)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	pending, err := store.pendingPublication(ctx, pr)
	if err != nil || pending == nil {
		t.Fatalf("uncertain reply publication = %+v, %v; want retained draft", pending, err)
	}
	t.Setenv("REVIEWCTL_FAKE_REJECT_REPLY", "")
	t.Setenv("REVIEWCTL_FAKE_REPLY_READBACK_FAILURE", "")
	second := ProcessAttempt(ctx, cfg, store, pr)
	if !second.Success || !second.Recovered || second.Cost != nil {
		t.Errorf("retry after unavailable readback = %+v, want recovered success without duplicate cost", second)
	}
	calls, _ := os.ReadFile(filepath.Join(temp, "codex-calls"))
	if string(calls) != "1" {
		t.Errorf("analysis calls after uncertain rejection = %q, want 1", calls)
	}
}
