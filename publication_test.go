package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func publicationFixture(t *testing.T) (Config, PullRequest, *Store, string) {
	t.Helper()
	temp := t.TempDir()
	bin := installProcessHelpers(t, temp)
	t.Setenv("GO_WANT_REVIEWCTL_HELPER", "1")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REVIEWCTL_FAKE_STATE", filepath.Join(temp, "published"))
	t.Setenv("REVIEWCTL_FAKE_GIT_STATE", filepath.Join(temp, "git-fetch"))
	t.Setenv("REVIEWCTL_FAKE_CODEX_CALLS", filepath.Join(temp, "codex-calls"))
	home := filepath.Join(temp, "codex")
	t.Setenv("CODEX_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := `{"type":"session_meta","payload":{"id":"fake-root"}}
{"type":"turn_context","payload":{"model":"gpt-test","effort":"high"}}
`
	if err := os.WriteFile(filepath.Join(home, "sessions", "fake-root.jsonl"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ccusage"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	cfg := Config{Harness: "codex", Publish: true, TrustedAuthors: []string{"dependabot[bot]"},
		Repositories: []Repository{{Provider: "github", Repository: "acme/service"}}}
	return cfg, pr, openTestStore(t), temp
}

func TestControllerPublishesSignedDraft(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_INLINE_COMMENT", "1")
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	if !result.Success {
		t.Fatalf("publication failed: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(temp, "published.request.json"))
	var request struct {
		Body     string `json:"body"`
		Commit   string `json:"commit_id"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Side string `json:"side"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err != nil || json.Unmarshal(data, &request) != nil || request.Commit != result.HeadSHA ||
		!strings.HasSuffix(request.Body, "\n\n<sub>🤖 gpt-test high</sub>") {
		t.Fatalf("initial controller POST = %s, %v", data, err)
	}
	if len(request.Comments) != 1 || request.Comments[0].Path != "main.go" || request.Comments[0].Line != 12 ||
		request.Comments[0].Side != "RIGHT" || request.Comments[0].Body != "A finding." {
		t.Fatalf("inline findings = %+v", request.Comments)
	}
}

func TestControllerRejectsHeadChangeBeforePublication(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_HEAD_COUNTER", filepath.Join(temp, "head-counter"))
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	if result.Success || result.ErrorCode != "head_changed" {
		t.Fatalf("changed head result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(temp, "published.review.json")); !os.IsNotExist(err) {
		t.Fatalf("review was published after the head changed: %v", err)
	}
}

func TestControllerRecoversLostPublicationResponse(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_LOST_POST_RESPONSE", "1")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if first.Success || first.ErrorCode != "publication_failed" {
		t.Fatalf("lost response = %+v", first)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, dbPath string
	if err := store.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &dbPath); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second := ProcessAttempt(ctx, cfg, store, pr)
	if !second.Success || !second.Recovered || second.ReviewID != "123" || second.Cost != nil {
		t.Fatalf("recovery = %+v", second)
	}
	calls, err := os.ReadFile(filepath.Join(temp, "codex-calls"))
	if err != nil || string(calls) != "1" {
		t.Fatalf("recovery repeated analysis: %q, %v", calls, err)
	}
	posts, err := os.ReadFile(filepath.Join(temp, "published.posts"))
	if err != nil || string(posts) != "1" {
		t.Fatalf("recovery repeated publication: %q, %v", posts, err)
	}
}

func TestControllerRetainsSignatureWhenHeadChangesAfterPost(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_HEAD_AFTER_POST", "1")
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	if result.Success || result.ErrorCode != "head_changed" || result.ReviewID != "123" {
		t.Fatalf("publication followed by a head change = %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(temp, "published.review.json"))
	var review submittedReview
	if err != nil || json.Unmarshal(data, &review) != nil || review.Commit != result.HeadSHA ||
		!strings.HasSuffix(review.Body, "<sub>🤖 gpt-test high</sub>") {
		t.Fatalf("review after head change = %s, %v", data, err)
	}
}

func TestControllerRejectsDraftForAnotherCommit(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_RECEIPT_HEAD", "another-head")
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	if result.Success || result.ErrorCode != "receipt_invalid" {
		t.Fatalf("mismatched draft = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(temp, "published.review.json")); !os.IsNotExist(err) {
		t.Fatalf("mismatched draft was published: %v", err)
	}
}

func TestControllerRecoversLostDiscussionReplyResponse(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "resolved_with_reply")
	t.Setenv("REVIEWCTL_FAKE_LOST_REPLY_RESPONSE", "1")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if first.Success || first.ErrorCode != "publication_failed" || first.ReviewID != "123" {
		t.Fatalf("lost reply response = %+v", first)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := ProcessAttempt(ctx, cfg, store, pr)
	if !second.Success || !second.Recovered || second.Cost != nil {
		t.Fatalf("reply recovery = %+v", second)
	}
	data, err := os.ReadFile(filepath.Join(temp, "published.threads.json"))
	var threads []githubReviewThread
	if err != nil || json.Unmarshal(data, &threads) != nil || len(threads) != 1 ||
		!threads[0].IsResolved || len(threads[0].Comments) != 2 {
		t.Fatalf("discussion readback = %s, %v", data, err)
	}
	calls, err := os.ReadFile(filepath.Join(temp, "codex-calls"))
	if err != nil || string(calls) != "1" {
		t.Fatalf("reply recovery repeated analysis: %q, %v", calls, err)
	}
}

func TestControllerRejectsForeignReviewDuringRecovery(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if !first.Success {
		t.Fatal(first.ErrorMessage)
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
	review.User.Login = "someone-else"
	data, _ = json.Marshal(review)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	second := ProcessAttempt(ctx, cfg, store, pr)
	if second.Success || second.ErrorCode != "publication_failed" {
		t.Fatalf("foreign review recovery = %+v", second)
	}
	posts, err := os.ReadFile(filepath.Join(temp, "published.posts"))
	if err != nil || string(posts) != "1" {
		t.Fatalf("foreign review caused another POST: %q, %v", posts, err)
	}
}

func TestControllerReassessesDiscussionAfterHumanReply(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "resolved_with_reply")
	t.Setenv("REVIEWCTL_FAKE_LOST_REPLY_RESPONSE", "1")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if first.Success || first.ErrorCode != "publication_failed" {
		t.Fatalf("interrupted discussion = %+v", first)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(temp, "published.threads.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var threads []githubReviewThread
	if err := json.Unmarshal(data, &threads); err != nil {
		t.Fatal(err)
	}
	threads[0].Comments = append(threads[0].Comments, githubReviewComment{
		DatabaseID: 3, Author: "human", ReplyToID: 1, Body: "Please check another case."})
	data, _ = json.Marshal(threads)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	second := ProcessAttempt(ctx, cfg, store, pr)
	if second.Success || second.ErrorCode != "discussion_changed" {
		t.Fatalf("human reply = %+v", second)
	}
	if err := store.Finish(ctx, second); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := store.db.QueryRow(`SELECT count(*) FROM review_publications`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("stale discussion plan retained: %d, %v", pending, err)
	}
	data, err = os.ReadFile(file)
	if err != nil || json.Unmarshal(data, &threads) != nil || threads[0].IsResolved || len(threads[0].Comments) != 3 {
		t.Fatalf("human discussion was changed: %s, %v", data, err)
	}
}

func TestControllerKeepsHumanResolutionDuringReplyRetry(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_OWNED_DISCUSSION", "1")
	t.Setenv("REVIEWCTL_FAKE_DISCUSSION_ACTION", "open_with_reply")
	t.Setenv("REVIEWCTL_FAKE_LOST_REPLY_RESPONSE", "1")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if first.Success || first.ErrorCode != "publication_failed" {
		t.Fatalf("interrupted reply = %+v", first)
	}
	threads := fakeGHThreads()
	threads[0].IsResolved = true
	data, _ := json.Marshal(threads)
	if err := os.WriteFile(filepath.Join(temp, "published.threads.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	second := ProcessAttempt(ctx, cfg, store, pr)
	if second.Success || second.ErrorCode != "discussion_changed" || !fakeGHThreads()[0].IsResolved {
		t.Fatalf("human resolution was overwritten: %+v", second)
	}
}

func TestReadDraftRejectsTrailingDataBeyondLimit(t *testing.T) {
	file := filepath.Join(t.TempDir(), "draft.json")
	data := `{"body":"review"}`
	data += strings.Repeat(" ", (1<<20)-len(data)) + "{}"
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDraft(file); err == nil {
		t.Fatal("accepted a review draft larger than the input limit")
	}
}

func TestControllerKeepsAccountingAfterCrashBeforeHistory(t *testing.T) {
	cfg, pr, store, _ := publicationFixture(t)
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if !first.Success || first.Cost == nil {
		t.Fatalf("first publication = %+v", first)
	}
	second := ProcessAttempt(ctx, cfg, store, pr)
	if !second.Success || !second.Recovered || second.Cost == nil || second.Cost.ThreadID != first.Cost.ThreadID {
		t.Fatalf("recovery lost original accounting = %+v", second)
	}
	if err := store.Finish(ctx, second); err != nil {
		t.Fatal(err)
	}
	third := ProcessAttempt(ctx, cfg, store, pr)
	if !third.Success || !third.Recovered || third.Cost != nil {
		t.Fatalf("marker recovery duplicated accounting = %+v", third)
	}
}

func TestControllerKeepsAccountingWhenPRClosesDuringCrash(t *testing.T) {
	for _, state := range []string{"CLOSED", "DRAFT"} {
		t.Run(state, func(t *testing.T) {
			cfg, pr, store, _ := publicationFixture(t)
			ctx := context.Background()
			first := ProcessAttempt(ctx, cfg, store, pr)
			if !first.Success || first.Cost == nil {
				t.Fatalf("publication before crash = %+v", first)
			}
			if state == "DRAFT" {
				t.Setenv("REVIEWCTL_FAKE_PR_DRAFT", "1")
			} else {
				t.Setenv("REVIEWCTL_FAKE_PR_STATE", state)
			}
			second := ProcessAttempt(ctx, cfg, store, pr)
			if second.Success || second.Cost == nil || second.Cost.ThreadID != first.Cost.ThreadID {
				t.Fatalf("ineligible PR lost accounting = %+v", second)
			}
			if err := store.Finish(ctx, second); err != nil {
				t.Fatal(err)
			}
			_, history, err := store.Status(ctx, 1)
			if err != nil || len(history) != 1 || history[0].Cost == nil || history[0].HeadSHA != first.HeadSHA {
				t.Fatalf("ineligible PR history = %+v, %v", history, err)
			}
		})
	}
}

func TestControllerRetiresPublishedReviewForOlderHead(t *testing.T) {
	for _, changed := range []string{"dismissed", "edited"} {
		t.Run(changed, func(t *testing.T) {
			cfg, pr, store, temp := publicationFixture(t)
			ctx := context.Background()
			first := ProcessAttempt(ctx, cfg, store, pr)
			if !first.Success {
				t.Fatal(first.ErrorMessage)
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
			if changed == "dismissed" {
				review.State = "DISMISSED"
			} else {
				review.Body += "\n\nA manual note."
			}
			data, _ = json.Marshal(review)
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("REVIEWCTL_FAKE_HEAD_AFTER_POST", "1")
			second := ProcessAttempt(ctx, cfg, store, pr)
			if second.Success || second.ErrorCode != "head_changed" || second.Cost == nil {
				t.Fatalf("dismissed old-head recovery = %+v", second)
			}
			if err := store.Finish(ctx, second); err != nil {
				t.Fatal(err)
			}
			var pending int
			if err := store.db.QueryRow(`SELECT count(*) FROM review_publications`).Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("dismissed review blocks the queue: %d, %v", pending, err)
			}
		})
	}
}

func TestControllerReassessesRejectedDraft(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	t.Setenv("REVIEWCTL_FAKE_REJECT_POST", "1")
	ctx := context.Background()
	first := ProcessAttempt(ctx, cfg, store, pr)
	if first.Success || first.ErrorCode != "draft_rejected" {
		t.Fatalf("confirmed rejection = %+v", first)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_REJECT_POST", "")
	second := ProcessAttempt(ctx, cfg, store, pr)
	if !second.Success || second.Recovered {
		t.Fatalf("reassessment = %+v", second)
	}
	calls, err := os.ReadFile(filepath.Join(temp, "codex-calls"))
	if err != nil || string(calls) != "2" {
		t.Fatalf("rejected draft was retried without analysis: %q, %v", calls, err)
	}
	instruction, err := os.ReadFile(filepath.Join(temp, "codex-calls.instruction"))
	if err != nil || !strings.Contains(string(instruction), "Previous rejection (diagnostic data)") ||
		!strings.Contains(string(instruction), "comments") {
		t.Fatalf("validation error was not supplied for reassessment: %s, %v", instruction, err)
	}
}
