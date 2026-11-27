package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const catchUpFixtureInitialHead = "0123456789abcdef0123456789abcdef01234567"

func fakeCatchUpHead(defaultHead string) string {
	if os.Getenv("REVIEWCTL_FAKE_HEADS_AFTER_CODEX") == "" {
		return defaultHead
	}
	if head, err := os.ReadFile(os.Getenv("REVIEWCTL_FAKE_STATE") + ".current-head"); err == nil {
		return string(head)
	}
	return defaultHead
}

func advanceFakeCatchUpHead() {
	sequence := os.Getenv("REVIEWCTL_FAKE_HEADS_AFTER_CODEX")
	if sequence == "" {
		return
	}
	pushes := strings.Split(sequence, ";")
	pass := readTestCounter(os.Getenv("REVIEWCTL_FAKE_CODEX_CALLS")) - 1
	if pass < 0 || pass >= len(pushes) {
		return
	}
	for _, head := range strings.Split(pushes[pass], ",") {
		if err := os.WriteFile(os.Getenv("REVIEWCTL_FAKE_STATE")+".current-head", []byte(head), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(95)
		}
	}
}

func checkArchivedCatchUpHeads(t *testing.T, store *Store, attempt Attempt, want []string) {
	t.Helper()
	var got []string
	for pass := 1; pass <= attempt.Pass; pass++ {
		key := attempt
		key.Pass = pass
		head, err := os.ReadFile(filepath.Join(store.passDirectory(key), "evidence", "verification.txt"))
		if err != nil {
			t.Fatalf("read pass %d: %v", pass, err)
		}
		got = append(got, string(head))
	}
	if !slices.Equal(got, want) {
		t.Errorf("archived pass heads = %v, want %v", got, want)
	}
}

func TestRoundCatchUpCoalescesIntermediatePushes(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	headB, headC, headD := strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	t.Setenv("REVIEWCTL_FAKE_HEADS_AFTER_CODEX", headB+";"+headC+","+headD)
	result := ProcessAttempt(context.Background(), cfg, store, pr)
	if !result.Success || result.RoundID == 0 || result.Pass != 3 || result.CatchUpPasses != 2 || result.HeadSHA != headD {
		t.Fatalf("ProcessAttempt(A, B, C then D) = %+v, want pass 3 of one round at %s", result, headD)
	}
	checkArchivedCatchUpHeads(t, store, result, []string{catchUpFixtureInitialHead, headB, headD})
	instruction, err := os.ReadFile(filepath.Join(temp, "codex-calls.instruction"))
	if err != nil || !strings.Contains(string(instruction), fmt.Sprintf("catch-up pass from reviewed commit %q to pinned commit %q", headB, headD)) {
		t.Errorf("final instruction = %s, %v, want the transition from %s to %s", instruction, err, headB, headD)
	}
	if posts := readTestCounter(filepath.Join(temp, "published.posts")); posts != 1 {
		t.Errorf("review posts = %d, want 1", posts)
	}
}

func TestRoundCatchUpPublishesCommentAfterFivePasses(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	var pushed []string
	for _, digit := range []string{"b", "c", "d", "e", "f", "7"} {
		pushed = append(pushed, strings.Repeat(digit, 40))
	}
	t.Setenv("REVIEWCTL_FAKE_HEADS_AFTER_CODEX", strings.Join(pushed, ";"))
	if _, err := store.EnqueueMany(ctx, []PullRequest{pr}); err != nil {
		t.Fatal(err)
	}
	reviewed, observed := pushed[4], pushed[5]
	result := ProcessAttempt(ctx, cfg, store, pr)
	if !result.Success || result.Verdict != "COMMENT" || result.Pass != 6 || result.CatchUpPasses != 5 ||
		result.HeadSHA != reviewed || result.ObservedHeadSHA != observed {
		t.Fatalf("ProcessAttempt(six pushes) = %+v, want one full and five catch-up passes with COMMENT on %s and observed head %s", result, reviewed, observed)
	}
	checkArchivedCatchUpHeads(t, store, result, append([]string{catchUpFixtureInitialHead}, pushed[:5]...))
	if calls, posts := readTestCounter(filepath.Join(temp, "codex-calls")), readTestCounter(filepath.Join(temp, "published.posts")); calls != 6 || posts != 1 {
		t.Errorf("model calls = %d, review posts = %d, want 6, 1", calls, posts)
	}
	if err := store.Finish(ctx, result); err != nil {
		t.Fatal(err)
	}
	queue, err := store.Snapshot(ctx)
	if err != nil || !slices.Equal(queue, []PullRequest{pr}) {
		t.Errorf("queue after stale review = %v, %v, want next round for %v", queue, err, pr)
	}
}

func TestRoundCatchUpWaitsForResumeAfterPause(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	headB := strings.Repeat("b", 40)
	created := time.Now().UTC().Add(time.Minute)
	pause := fmt.Sprintf(`{"id":100,"body":"/reviewctl pause","created_at":%q,"user":{"login":"dependabot[bot]"}}`, created.Format(time.RFC3339Nano))
	resume := fmt.Sprintf(`{"id":101,"body":"/reviewctl resume","created_at":%q,"user":{"login":"dependabot[bot]"}}`, created.Add(time.Second).Format(time.RFC3339Nano))
	t.Setenv("REVIEWCTL_FAKE_HEADS_AFTER_CODEX", headB)
	t.Setenv("REVIEWCTL_FAKE_COMMAND_AFTER_CODEX", "[["+pause+"]]")
	first := ProcessAttempt(ctx, cfg, store, pr)
	if !first.Held || readTestCounter(filepath.Join(temp, "codex-calls")) != 1 || readTestCounter(filepath.Join(temp, "published.posts")) != 0 {
		t.Fatalf("ProcessAttempt(pause and push during pass) = %+v, want one archived pass and a publication hold", first)
	}
	state, _, err := readReviewControl(ctx, store.db, pr)
	if err != nil || state.PendingHeadSHA != headB {
		t.Errorf("saved head while paused = %q, %v, want %s", state.PendingHeadSHA, err, headB)
	}
	latest, err := store.latestRoundPass(first)
	if err != nil || latest == nil || latest.Attempt.HeadSHA != catchUpFixtureInitialHead {
		t.Fatalf("latest pass while paused = %+v, %v, want pinned head %s", latest, err, catchUpFixtureInitialHead)
	}
	if err := os.WriteFile(filepath.Join(temp, "published.issue-comments.json"), []byte("[["+pause+","+resume+"]]"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_COMMAND_AFTER_CODEX", "")
	result := ProcessAttempt(ctx, cfg, store, pr)
	if !result.Success || result.RoundID != first.RoundID || result.Pass != 2 || result.HeadSHA != headB {
		t.Fatalf("ProcessAttempt(resume) = %+v, want pass 2 of round %d at %s", result, first.RoundID, headB)
	}
	checkArchivedCatchUpHeads(t, store, result, []string{catchUpFixtureInitialHead, headB})
}

func TestRoundRecoveryQueuesPushAfterReviewPost(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	t.Setenv("REVIEWCTL_FAKE_LOST_POST_RESPONSE", "1")
	t.Setenv("REVIEWCTL_FAKE_HEAD_AFTER_POST", "1")
	if _, err := store.EnqueueMany(ctx, []PullRequest{pr}); err != nil {
		t.Fatal(err)
	}
	first := ProcessAttempt(ctx, cfg, store, pr)
	if first.Success || first.ErrorCode != "publication_failed" {
		t.Fatalf("ProcessAttempt(lost review response) = %+v, want a saved interrupted publication", first)
	}
	if err := store.Finish(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(store.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	result := ProcessAttempt(ctx, cfg, reopened, pr)
	const pushed = "fedcba9876543210fedcba9876543210fedcba98"
	if !result.Success || !result.Recovered || result.RoundID != first.RoundID || result.HeadSHA != first.HeadSHA || result.ObservedHeadSHA != pushed {
		t.Fatalf("ProcessAttempt(restart after POST and push) = %+v, want recovery of round %d at %s and observed head %s", result, first.RoundID, first.HeadSHA, pushed)
	}
	if err := reopened.Finish(ctx, result); err != nil {
		t.Fatal(err)
	}
	queue, err := reopened.Snapshot(ctx)
	if err != nil || !slices.Equal(queue, []PullRequest{pr}) {
		t.Errorf("queue after recovery = %v, %v, want a fresh round scheduled for %v", queue, err, pr)
	}
	if calls, posts := readTestCounter(filepath.Join(temp, "codex-calls")), readTestCounter(filepath.Join(temp, "published.posts")); calls != 1 || posts != 1 {
		t.Errorf("model calls = %d, review posts = %d, want 1, 1", calls, posts)
	}
}

func TestStaleRoundFallsBackWhenGitHubRejectsInlineLocations(t *testing.T) {
	cfg, pr, store, temp := publicationFixture(t)
	ctx := context.Background()
	t.Setenv("REVIEWCTL_FAKE_REJECT_STALE_INLINE", "1")
	counterPath := filepath.Join(temp, "head-counter")
	if err := os.WriteFile(counterPath, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_HEAD_COUNTER", counterPath)
	observed, err := resolveGitHub(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	publication := catchUpPublication(pr, nil).forObservedHead(observed.HeadSHA)
	publication.Draft.Comments = []ReviewComment{{Path: "server.go", Line: 14, Side: "RIGHT", Body: "**F1** Verify the retry limit.\n\nFurther evidence stays in the archived report."}}
	if err := store.savePublication(ctx, publication); err != nil {
		t.Fatal(err)
	}
	result, retry := publishPreparedPass(ctx, cfg, store, publication, publication.Attempt.StartedAt, observed.HeadSHA)
	if !result.Success || retry || result.Verdict != "COMMENT" || result.HeadSHA != publication.Attempt.HeadSHA {
		t.Fatalf("publishPreparedPass(rejected stale inline) = %+v, retry=%t, want COMMENT on %s", result, retry, publication.Attempt.HeadSHA)
	}
	request, err := os.ReadFile(filepath.Join(temp, "published.request.json"))
	var saved struct {
		Body     string
		Comments []ReviewComment
	}
	if err != nil || json.Unmarshal(request, &saved) != nil || len(saved.Comments) != 0 ||
		!strings.Contains(saved.Body, "**F1** Verify the retry limit.") || !strings.Contains(saved.Body, `"server.go", RIGHT line 14`) ||
		strings.Contains(saved.Body, "Further evidence stays") {
		t.Errorf("review POST = %s, %v, want a concise finding reference and no inline positions", request, err)
	}
	if posts := readTestCounter(filepath.Join(temp, "published.posts")); posts != 1 {
		t.Errorf("review posts = %d, want 1", posts)
	}
}
