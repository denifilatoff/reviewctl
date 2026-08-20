package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

type codedError struct {
	code    string
	message string
}

func (e *codedError) Error() string { return e.message }

func fail(code, format string, args ...any) error {
	return &codedError{code: code, message: bounded(fmt.Sprintf(format, args...), 512)}
}

func normalizeGitHubLogin(login string) string {
	login = strings.ToLower(strings.TrimSpace(login))
	if slug, found := strings.CutPrefix(login, "app/"); found && slug != "" {
		return slug + "[bot]"
	}
	return login
}

type GitHubPullRequest struct {
	URL     string
	Number  int64
	State   string
	IsDraft bool
	HeadSHA string
	Author  string
}

type pullRequestSnapshot struct {
	PullRequest
	IsDraft bool
	HeadSHA string
}

func shouldEnqueueDiscovery(initialized bool, previous *pullRequestSnapshot, current pullRequestSnapshot) bool {
	if !initialized || current.IsDraft {
		return false
	}
	return previous == nil || previous.IsDraft || previous.HeadSHA != current.HeadSHA
}

func listGitHubPullRequests(ctx context.Context, repository Repository) ([]pullRequestSnapshot, error) {
	// ponytail: The 1,000-PR limit avoids custom pagination; use gh api --paginate if a repository can exceed it.
	command := exec.CommandContext(ctx, "gh", "pr", "list", "--repo", repository.Repository, "--state", "open",
		"--limit", "1000", "--json", "url,number,state,isDraft,headRefOid")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fail("github_failed", "gh failed for %s: %v: %s", repository.Repository, err,
			strings.TrimSpace(stderr.String()))
	}
	var response []struct {
		URL        string `json:"url"`
		Number     int64  `json:"number"`
		State      string `json:"state"`
		IsDraft    bool   `json:"isDraft"`
		HeadRefOID string `json:"headRefOid"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return nil, fail("github_failed", "decode gh response for %s: %v", repository.Repository, err)
	}
	snapshot := make([]pullRequestSnapshot, len(response))
	seen := make(map[int64]bool, len(response))
	for i, item := range response {
		pr := PullRequest{
			Provider: repository.Provider, Repository: repository.Repository, Number: item.Number,
			URL: fmt.Sprintf("https://github.com/%s/pull/%d", repository.Repository, item.Number),
		}
		if item.Number < 1 || item.State != "OPEN" || item.HeadRefOID == "" ||
			!samePullRequestIdentity(item.URL, pr) || seen[item.Number] {
			return nil, fail("github_failed", "gh returned an invalid open pull request for %s", repository.Repository)
		}
		seen[item.Number] = true
		snapshot[i] = pullRequestSnapshot{PullRequest: pr, IsDraft: item.IsDraft, HeadSHA: item.HeadRefOID}
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Number < snapshot[j].Number })
	return snapshot, nil
}

func ValidateGitHubPullRequest(cfg Config, expected PullRequest, resolved GitHubPullRequest) error {
	if !cfg.Publish {
		return fail("publication_disabled", "publication is disabled")
	}
	allowed := false
	for _, configured := range cfg.Repositories {
		if configured.Provider == expected.Provider && configured.Repository == expected.Repository {
			allowed = true
			break
		}
	}
	if !allowed {
		return fail("repository_not_allowed", "repository %s is not configured", expected.Repository)
	}
	trusted := false
	author := normalizeGitHubLogin(resolved.Author)
	for _, configured := range cfg.TrustedAuthors {
		if configured == author {
			trusted = true
			break
		}
	}
	if !trusted {
		return fail("untrusted_author", "pull request author %s is not trusted", resolved.Author)
	}
	if !samePullRequestIdentity(resolved.URL, expected) || resolved.Number != expected.Number || resolved.HeadSHA == "" {
		return fail("github_mismatch", "GitHub response does not match the queued pull request")
	}
	if resolved.State != "OPEN" {
		return fail("pr_not_open", "pull request is not open")
	}
	if resolved.IsDraft {
		return fail("pr_draft", "pull request is a draft")
	}
	return nil
}
