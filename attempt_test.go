package main

import (
	"bytes"
	"context"
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
	for _, verdict := range []string{"APPROVE", "REQUEST_CHANGES", "COMMENT"} {
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
		if err := ValidateReceipt(receipt, pr, receipt.HeadSHA, receipt.SkillDigest); err != nil {
			t.Errorf("%s: %v", verdict, err)
		}
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
	if err := ValidateReceipt(receipt, pr, "expected-head", "expected-digest"); err == nil {
		t.Fatal("expected mismatched receipt to be rejected")
	}
}

func TestValidateReceiptBindsURLToReviewID(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	receipt := Receipt{
		Provider: "github", Repository: "acme/service", Number: 7, HeadSHA: "head", SkillDigest: "digest",
		Verdict: "APPROVE", ReviewID: "123",
		ReviewURL: "https://github.com/acme/service/pull/7#pullrequestreview-456",
	}
	if err := ValidateReceipt(receipt, pr, "head", "digest"); err == nil {
		t.Fatal("expected mismatched review ID and URL to be rejected")
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
