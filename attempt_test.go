package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedAPMFilesMatchCheckedInFiles(t *testing.T) {
	for name, embedded := range map[string][]byte{
		"apm.yml":       embeddedAPMManifest,
		"apm.lock.yaml": embeddedAPMLock,
	} {
		checkedIn, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(embedded, checkedIn) {
			t.Fatalf("embedded %s differs from the checked-in file", name)
		}
	}
}

func TestRunCommandPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runCommand(ctx, "", nil, "git", "--version")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestTrustedInstructionAuthorizesOwnedDiscussionSynchronization(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	instruction := trustedInstruction(pr, "head", "digest", "/source", "/receipt", []string{"PRRT_owned"})
	for _, required := range []string{
		`Pre-existing owned discussion thread IDs: ["PRRT_owned"]`,
		"Synchronize every listed discussion according to the installed skill",
	} {
		if !strings.Contains(instruction, required) {
			t.Fatalf("trusted instruction does not contain %q", required)
		}
	}
	if strings.Contains(instruction, "any other GitHub state") {
		t.Fatal("trusted instruction still forbids authorized discussion synchronization")
	}
}

func TestValidateGitHubPullRequestFailsClosed(t *testing.T) {
	cfg := Config{
		Harness:        "codex",
		Publish:        true,
		TrustedAuthors: []string{"dependabot[bot]"},
		Repositories:   []Repository{{Provider: "github", Repository: "acme/service"}},
	}
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	resolved := GitHubPullRequest{URL: pr.URL, Number: 7, State: "OPEN", HeadSHA: "abc", Author: "app/dependabot"}
	if err := ValidateGitHubPullRequest(cfg, pr, resolved); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config, *GitHubPullRequest){
		"publication disabled": func(c *Config, _ *GitHubPullRequest) { c.Publish = false },
		"repository denied":    func(c *Config, _ *GitHubPullRequest) { c.Repositories = nil },
		"author untrusted":     func(_ *Config, r *GitHubPullRequest) { r.Author = "mallory" },
		"pull request closed":  func(_ *Config, r *GitHubPullRequest) { r.State = "CLOSED" },
		"pull request draft":   func(_ *Config, r *GitHubPullRequest) { r.IsDraft = true },
		"wrong repository":     func(_ *Config, r *GitHubPullRequest) { r.URL = "https://github.com/acme/other/pull/7" },
		"wrong URL number":     func(_ *Config, r *GitHubPullRequest) { r.URL = "https://github.com/acme/service/pull/8" },
		"wrong response number": func(_ *Config, r *GitHubPullRequest) {
			r.Number = 8
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidateCfg, candidatePR := cfg, resolved
			mutate(&candidateCfg, &candidatePR)
			if err := ValidateGitHubPullRequest(candidateCfg, pr, candidatePR); err == nil {
				t.Fatal("expected trust check to fail")
			}
		})
	}
}

func TestValidateGitHubPullRequestAcceptsRepositoryDisplayCase(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/powershell/powershell/pull/7")
	cfg := Config{
		Harness: "codex", Publish: true, TrustedAuthors: []string{"octocat"},
		Repositories: []Repository{{Provider: "github", Repository: "powershell/powershell"}},
	}
	resolved := GitHubPullRequest{
		URL: "https://github.com/PowerShell/PowerShell/pull/7", Number: 7, State: "OPEN", HeadSHA: "abc",
		Author: "octocat",
	}
	if err := ValidateGitHubPullRequest(cfg, pr, resolved); err != nil {
		t.Fatal(err)
	}
}

func TestHashSkillsUsesRelativePathsAndContents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := HashSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := HashSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first[:7] != "sha256:" || second[:7] != "sha256:" {
		t.Fatalf("unexpected digests: %q %q", first, second)
	}
}

func TestValidateReceiptAcceptsSuccessfulReviewOutcomes(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	for _, verdict := range []string{"APPROVE", "REQUEST_CHANGES"} {
		receipt := Receipt{
			Provider:    "github",
			Repository:  "acme/service",
			Number:      7,
			HeadSHA:     "0123456789abcdef0123456789abcdef01234567",
			SkillDigest: "sha256:abc",
			Verdict:     verdict,
			ReviewID:    "123",
			ReviewURL:   "https://github.com/acme/service/pull/7#pullrequestreview-123",
		}
		if err := ValidateReceipt(receipt, pr, receipt.HeadSHA, receipt.SkillDigest, false); err != nil {
			t.Errorf("%s: %v", verdict, err)
		}
	}
}

func TestValidateDiscussionOutcomesRequiresEveryOwnedThread(t *testing.T) {
	initial := []githubReviewThread{
		{ID: "owned", Comments: []githubReviewComment{{DatabaseID: 1, Author: "reviewer"}}},
		{ID: "other", Comments: []githubReviewComment{{DatabaseID: 2, Author: "someone-else"}}},
	}
	current := []githubReviewThread{
		{ID: "owned", IsResolved: true, Comments: []githubReviewComment{{DatabaseID: 1, Author: "reviewer"}}},
		{ID: "other", Comments: []githubReviewComment{{DatabaseID: 2, Author: "someone-else"}}},
	}

	if err := validateDiscussionOutcomes(initial, current, "reviewer", []DiscussionOutcome{}); err == nil {
		t.Fatal("accepted a receipt that omitted an owned discussion")
	}
	if err := validateDiscussionOutcomes(initial, current, "reviewer", []DiscussionOutcome{
		{ThreadID: "owned", Action: "resolved"},
	}); err != nil {
		t.Fatalf("rejected complete discussion outcomes: %v", err)
	}
}

func TestValidateDiscussionOutcomesChecksRepliesAndFinalState(t *testing.T) {
	initial := []githubReviewThread{
		{ID: "accepted", Comments: []githubReviewComment{{DatabaseID: 1, Author: "reviewer"}}},
		{ID: "clarified", Comments: []githubReviewComment{{DatabaseID: 2, Author: "reviewer"}}},
		{ID: "remaining", IsResolved: true, Comments: []githubReviewComment{{DatabaseID: 3, Author: "reviewer"}}},
		{ID: "unchanged", Comments: []githubReviewComment{{DatabaseID: 4, Author: "reviewer"}}},
	}
	current := []githubReviewThread{
		{ID: "accepted", IsResolved: true, Comments: []githubReviewComment{{DatabaseID: 1, Author: "reviewer"}}},
		{ID: "clarified", IsResolved: true, Comments: []githubReviewComment{
			{DatabaseID: 2, Author: "reviewer"},
			{DatabaseID: 20, Author: "reviewer", ReplyToID: 2},
		}},
		{ID: "remaining", Comments: []githubReviewComment{
			{DatabaseID: 3, Author: "reviewer"},
			{DatabaseID: 30, Author: "reviewer", ReplyToID: 3},
		}},
		{ID: "unchanged", Comments: []githubReviewComment{{DatabaseID: 4, Author: "reviewer"}}},
	}
	outcomes := []DiscussionOutcome{
		{ThreadID: "accepted", Action: "resolved"},
		{ThreadID: "clarified", Action: "resolved_with_reply", ReplyID: "20"},
		{ThreadID: "remaining", Action: "open_with_reply", ReplyID: "30"},
		{ThreadID: "unchanged", Action: "preserved"},
	}

	if err := validateDiscussionOutcomes(initial, current, "reviewer", outcomes); err != nil {
		t.Fatalf("rejected verified discussion lifecycle: %v", err)
	}
}

func TestValidateDiscussionOutcomesRejectsUnverifiedResults(t *testing.T) {
	if err := validateDiscussionOutcomes(nil, nil, "reviewer", nil); err == nil {
		t.Fatal("accepted a receipt without discussion_outcomes")
	}
	initial := []githubReviewThread{{
		ID: "owned", Comments: []githubReviewComment{{DatabaseID: 1, Author: "reviewer"}},
	}}
	foreignReply := []githubReviewThread{{
		ID: "owned", Comments: []githubReviewComment{
			{DatabaseID: 1, Author: "reviewer"},
			{DatabaseID: 2, Author: "someone-else", ReplyToID: 1},
		},
	}}
	if err := validateDiscussionOutcomes(initial, foreignReply, "reviewer", []DiscussionOutcome{
		{ThreadID: "owned", Action: "open_with_reply", ReplyID: "2"},
	}); err == nil {
		t.Fatal("accepted another user's reply")
	}
	oldReply := []githubReviewThread{{
		ID: "owned", Comments: []githubReviewComment{
			{DatabaseID: 1, Author: "reviewer"},
			{DatabaseID: 3, Author: "reviewer", ReplyToID: 1},
		},
	}}
	if err := validateDiscussionOutcomes(oldReply, oldReply, "reviewer", []DiscussionOutcome{
		{ThreadID: "owned", Action: "open_with_reply", ReplyID: "3"},
	}); err == nil {
		t.Fatal("accepted an old reply as a new discussion outcome")
	}
	newReply := []githubReviewThread{{
		ID: "owned", Comments: []githubReviewComment{
			{DatabaseID: 1, Author: "reviewer"},
			{DatabaseID: 4, Author: "reviewer", ReplyToID: 1},
		},
	}}
	if err := validateDiscussionOutcomes(initial, newReply, "reviewer", []DiscussionOutcome{
		{ThreadID: "owned", Action: "preserved"},
	}); err == nil {
		t.Fatal("accepted a new reply as a preserved discussion")
	}
	duplicateReplies := []githubReviewThread{{
		ID: "owned", Comments: []githubReviewComment{
			{DatabaseID: 1, Author: "reviewer"},
			{DatabaseID: 4, Author: "reviewer", ReplyToID: 1},
			{DatabaseID: 5, Author: "reviewer", ReplyToID: 1},
		},
	}}
	if err := validateDiscussionOutcomes(initial, duplicateReplies, "reviewer", []DiscussionOutcome{
		{ThreadID: "owned", Action: "open_with_reply", ReplyID: "4"},
	}); err == nil {
		t.Fatal("accepted an unreported duplicate reply")
	}
	resolved := []githubReviewThread{{
		ID: "owned", IsResolved: true, Comments: []githubReviewComment{{DatabaseID: 1, Author: "reviewer"}},
	}}
	if err := validateDiscussionOutcomes(initial, resolved, "reviewer", []DiscussionOutcome{
		{ThreadID: "owned", Action: "preserved"},
	}); err == nil {
		t.Fatal("accepted a changed discussion as preserved")
	}
}

func TestValidateDiscussionOutcomesRejectsUnownedThreadMutationsButAllowsNewFindings(t *testing.T) {
	initial := []githubReviewThread{{
		ID: "other", Comments: []githubReviewComment{{DatabaseID: 1, Author: "someone-else"}},
	}}
	mutated := []githubReviewThread{{
		ID: "other", IsResolved: true, Comments: []githubReviewComment{{DatabaseID: 1, Author: "someone-else"}},
	}}
	if err := validateDiscussionOutcomes(initial, mutated, "reviewer", []DiscussionOutcome{}); err == nil {
		t.Fatal("accepted a resolution change to an unowned discussion")
	}
	replied := []githubReviewThread{{
		ID: "other", Comments: []githubReviewComment{
			{DatabaseID: 1, Author: "someone-else"},
			{DatabaseID: 2, Author: "reviewer", ReplyToID: 1},
		},
	}}
	if err := validateDiscussionOutcomes(initial, replied, "reviewer", []DiscussionOutcome{}); err == nil {
		t.Fatal("accepted a reply to an unowned discussion")
	}
	newFinding := append(initial, githubReviewThread{
		ID: "new", Comments: []githubReviewComment{{DatabaseID: 3, Author: "reviewer"}},
	})
	if err := validateDiscussionOutcomes(initial, newFinding, "reviewer", []DiscussionOutcome{}); err != nil {
		t.Fatalf("rejected a new current-review finding: %v", err)
	}
}

func TestDecodeGitHubReviewThreadsPreservesOwnershipRepliesAndState(t *testing.T) {
	payload := []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"thread","isResolved":true,"comments":{"nodes":[{"databaseId":1,"author":{"login":"reviewer"},"replyTo":null},{"databaseId":2,"author":{"login":"reviewer"},"replyTo":{"databaseId":1}}],"pageInfo":{"hasNextPage":false}}}],"pageInfo":{"hasNextPage":false}}}}}}`)

	threads, err := decodeGitHubReviewThreads(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].ID != "thread" || !threads[0].IsResolved ||
		len(threads[0].Comments) != 2 || threads[0].Comments[1].ReplyToID != 1 {
		t.Fatalf("decoded threads = %+v", threads)
	}
}

func TestDecodeGitHubReviewThreadsRejectsIncompletePagination(t *testing.T) {
	payload := []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":true}}}}}}`)
	if _, err := decodeGitHubReviewThreads(payload); err == nil {
		t.Fatal("accepted a partial review-thread page")
	}
}

func TestValidateReceiptAcceptsCommentForSelfAuthoredPR(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	receipt := Receipt{
		Provider: "github", Repository: "acme/service", Number: 7, HeadSHA: "head", SkillDigest: "digest",
		Verdict: "COMMENT", ReviewID: "123",
		ReviewURL: "https://github.com/acme/service/pull/7#pullrequestreview-123",
	}
	if err := ValidateReceipt(receipt, pr, "head", "digest", true); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReceiptRejectsDecisiveVerdictForSelfAuthoredPR(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	for _, verdict := range []string{"APPROVE", "REQUEST_CHANGES"} {
		receipt := Receipt{
			Provider: "github", Repository: "acme/service", Number: 7, HeadSHA: "head", SkillDigest: "digest",
			Verdict: verdict, ReviewID: "123",
			ReviewURL: "https://github.com/acme/service/pull/7#pullrequestreview-123",
		}
		if err := ValidateReceipt(receipt, pr, "head", "digest", true); err == nil {
			t.Errorf("accepted %s for a self-authored pull request", verdict)
		}
	}
}

func TestValidateReceiptRejectsCommentForOrdinaryPR(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	receipt := Receipt{
		Provider: "github", Repository: "acme/service", Number: 7, HeadSHA: "head", SkillDigest: "digest",
		Verdict: "COMMENT", ReviewID: "123",
		ReviewURL: "https://github.com/acme/service/pull/7#pullrequestreview-123",
	}
	if err := ValidateReceipt(receipt, pr, "head", "digest", false); err == nil {
		t.Fatal("expected COMMENT to be rejected for a pull request authored by another user")
	}
}

func TestValidateReceiptRejectsMismatchedScope(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	receipt := Receipt{
		Provider:    "github",
		Repository:  "acme/other",
		Number:      7,
		HeadSHA:     "wrong",
		SkillDigest: "wrong",
		Verdict:     "COMMENT",
		ReviewID:    "123",
		ReviewURL:   "https://example.com/review/123",
	}
	if err := ValidateReceipt(receipt, pr, "expected-head", "expected-digest", false); err == nil {
		t.Fatal("expected mismatched receipt to be rejected")
	}
}

func TestValidateReceiptAcceptsRepositoryDisplayCase(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/powershell/powershell/pull/7")
	receipt := Receipt{
		Provider: "github", Repository: "PowerShell/PowerShell", Number: 7, HeadSHA: "head", SkillDigest: "digest",
		Verdict: "APPROVE", ReviewID: "123",
		ReviewURL: "https://github.com/PowerShell/PowerShell/pull/7#pullrequestreview-123",
	}
	if err := ValidateReceipt(receipt, pr, "head", "digest", false); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReceiptRejectsWrongReviewIdentity(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/powershell/powershell/pull/7")
	for name, reviewURL := range map[string]string{
		"repository": "https://github.com/PowerShell/Other/pull/7#pullrequestreview-123",
		"number":     "https://github.com/PowerShell/PowerShell/pull/8#pullrequestreview-123",
		"review ID":  "https://github.com/PowerShell/PowerShell/pull/7#pullrequestreview-456",
	} {
		t.Run(name, func(t *testing.T) {
			receipt := Receipt{
				Provider: "github", Repository: "PowerShell/PowerShell", Number: 7, HeadSHA: "head",
				SkillDigest: "digest", Verdict: "APPROVE", ReviewID: "123", ReviewURL: reviewURL,
			}
			if err := ValidateReceipt(receipt, pr, "head", "digest", false); err == nil {
				t.Fatal("expected wrong review identity to be rejected")
			}
		})
	}
}

func TestValidateReceiptRejectsNonPositiveDecimalReviewIDs(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	for _, reviewID := range []json.Number{"", "0", "00", "-1", "1.5", "1e3", "１２３"} {
		receipt := Receipt{
			Provider: "github", Repository: "acme/service", Number: 7, HeadSHA: "head", SkillDigest: "digest",
			Verdict: "APPROVE", ReviewID: reviewID,
			ReviewURL: "https://github.com/acme/service/pull/7#pullrequestreview-" + string(reviewID),
		}
		if err := ValidateReceipt(receipt, pr, "head", "digest", false); err == nil {
			t.Errorf("accepted review ID %q", reviewID)
		}
	}
}

func TestReadReceiptAcceptsNumericAndQuotedReviewIDs(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	for _, body := range []string{
		`{"review_id":4985115476}`,
		`{"review_id":"4985115476"}`,
	} {
		path := filepath.Join(t.TempDir(), "receipt.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		receipt, err := readReceipt(path)
		if err != nil {
			t.Fatalf("read %s: %v", body, err)
		}
		if receipt.ReviewID != "4985115476" {
			t.Fatalf("review ID = %q", receipt.ReviewID)
		}
		receipt.Provider, receipt.Repository, receipt.Number = "github", "acme/service", 7
		receipt.HeadSHA, receipt.SkillDigest, receipt.Verdict = "head", "digest", "APPROVE"
		receipt.ReviewURL = "https://github.com/acme/service/pull/7#pullrequestreview-4985115476"
		if err := ValidateReceipt(receipt, pr, "head", "digest", false); err != nil {
			t.Fatalf("validate %s: %v", body, err)
		}
	}
}

func TestReadReceiptRejectsInvalidInput(t *testing.T) {
	for _, body := range []string{
		`{"review_id":"4985115476","unexpected":true}`,
		`{"review_id":"4985115476"} {}`,
		`{"review_id":""}`,
		`{"review_id":"not-a-number"}`,
	} {
		path := filepath.Join(t.TempDir(), "receipt.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readReceipt(path); err == nil {
			t.Fatalf("accepted invalid receipt %s", body)
		}
	}
}

func TestHashSkillsSeparatesFileContentsFromTheNextPath(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "a"), []byte("xb\x00y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "a"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "b"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstDigest, _ := HashSkills(first)
	secondDigest, _ := HashSkills(second)
	if firstDigest == secondDigest {
		t.Fatal("different skill trees produced the same digest")
	}
}
