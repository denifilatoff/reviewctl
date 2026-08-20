package main

import (
	"fmt"
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
