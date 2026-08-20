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

func TestValidateReceiptBindsURLToReviewID(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	receipt := Receipt{
		Provider: "github", Repository: "acme/service", Number: 7, HeadSHA: "head", SkillDigest: "digest",
		Verdict: "APPROVE", ReviewID: "123",
		ReviewURL: "https://github.com/acme/service/pull/7#pullrequestreview-456",
	}
	if err := ValidateReceipt(receipt, pr, "head", "digest", false); err == nil {
		t.Fatal("expected mismatched review ID and URL to be rejected")
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
