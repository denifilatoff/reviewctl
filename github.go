package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
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

type githubReviewComment struct {
	DatabaseID int64
	Author     string
	ReplyToID  int64
}

type githubReviewThread struct {
	ID         string
	IsResolved bool
	Comments   []githubReviewComment
}

const reviewThreadsQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){nodes{id isResolved comments(first:100){nodes{databaseId author{login} replyTo{databaseId}} pageInfo{hasNextPage}}} pageInfo{hasNextPage}}}}}`

func listGitHubReviewThreads(ctx context.Context, pr PullRequest) ([]githubReviewThread, error) {
	owner, name, ok := strings.Cut(pr.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fail("github_failed", "invalid GitHub repository %s", pr.Repository)
	}
	var data json.RawMessage
	err := githubJSON(ctx, []string{"graphql", "-f", "query=" + reviewThreadsQuery, "-f", "owner=" + owner,
		"-f", "name=" + name, "-F", fmt.Sprintf("number=%d", pr.Number)}, nil, &data)
	if err != nil {
		return nil, fail("github_failed", "read review threads: %v", err)
	}
	threads, err := decodeGitHubReviewThreads(data)
	if err != nil {
		return nil, fail("github_failed", "read review threads: %v", err)
	}
	return threads, nil
}

func ownedDiscussionIDs(threads []githubReviewThread, login string) []string {
	login = normalizeGitHubLogin(login)
	var ids []string
	for _, thread := range threads {
		if len(thread.Comments) > 0 && normalizeGitHubLogin(thread.Comments[0].Author) == login {
			ids = append(ids, thread.ID)
		}
	}
	return ids
}

func decodeGitHubReviewThreads(data []byte) ([]githubReviewThread, error) {
	var response struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
									Author     struct {
										Login string `json:"login"`
									} `json:"author"`
									ReplyTo *struct {
										DatabaseID int64 `json:"databaseId"`
									} `json:"replyTo"`
								} `json:"nodes"`
								PageInfo struct {
									HasNextPage bool `json:"hasNextPage"`
								} `json:"pageInfo"`
							} `json:"comments"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	if len(response.Errors) > 0 {
		return nil, fmt.Errorf("GitHub GraphQL: %s", response.Errors[0].Message)
	}
	if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("GitHub returned no pull request")
	}
	wire := response.Data.Repository.PullRequest.ReviewThreads
	if wire.PageInfo.HasNextPage {
		return nil, fmt.Errorf("pull request has more than 100 review threads")
	}
	threads := make([]githubReviewThread, len(wire.Nodes))
	for i, item := range wire.Nodes {
		if item.ID == "" || item.Comments.PageInfo.HasNextPage || len(item.Comments.Nodes) == 0 {
			return nil, fmt.Errorf("GitHub returned an incomplete review thread")
		}
		threads[i] = githubReviewThread{ID: item.ID, IsResolved: item.IsResolved}
		for _, comment := range item.Comments.Nodes {
			replyTo := int64(0)
			if comment.ReplyTo != nil {
				replyTo = comment.ReplyTo.DatabaseID
			}
			threads[i].Comments = append(threads[i].Comments, githubReviewComment{
				DatabaseID: comment.DatabaseID, Author: comment.Author.Login, ReplyToID: replyTo,
			})
		}
	}
	return threads, nil
}

func validateDiscussionOutcomes(initial, current []githubReviewThread, login string, outcomes []DiscussionOutcome) error {
	if outcomes == nil {
		return fmt.Errorf("discussion_outcomes is required")
	}
	login = normalizeGitHubLogin(login)
	expected := make(map[string]githubReviewThread)
	for _, thread := range initial {
		if len(thread.Comments) > 0 && normalizeGitHubLogin(thread.Comments[0].Author) == login {
			expected[thread.ID] = thread
		}
	}
	if len(outcomes) != len(expected) {
		return fmt.Errorf("discussion outcomes do not cover every owned discussion")
	}
	final := make(map[string]githubReviewThread, len(current))
	for _, thread := range current {
		final[thread.ID] = thread
	}
	for _, before := range initial {
		after, ok := final[before.ID]
		if !ok {
			return fmt.Errorf("discussion outcome readback mismatch")
		}
		if _, owned := expected[before.ID]; !owned &&
			(after.IsResolved != before.IsResolved || len(newDiscussionReplyIDs(before, after, login)) != 0) {
			return fmt.Errorf("unowned discussion changed during review")
		}
	}
	seen := make(map[string]bool, len(outcomes))
	for _, outcome := range outcomes {
		if _, ok := expected[outcome.ThreadID]; !ok || seen[outcome.ThreadID] {
			return fmt.Errorf("discussion outcome does not match an owned discussion")
		}
		seen[outcome.ThreadID] = true
		thread, ok := final[outcome.ThreadID]
		if !ok {
			return fmt.Errorf("discussion outcome readback mismatch")
		}
		before := expected[outcome.ThreadID]
		newReplies := newDiscussionReplyIDs(before, thread, login)
		replyID, replyErr := strconv.ParseInt(string(outcome.ReplyID), 10, 64)
		hasReply := replyErr == nil && replyID > 0 && len(newReplies) == 1 && newReplies[0] == replyID
		switch outcome.Action {
		case "resolved":
			ok = thread.IsResolved && outcome.ReplyID == "" && len(newReplies) == 0
		case "resolved_with_reply":
			ok = thread.IsResolved && hasReply
		case "open_with_reply":
			ok = !thread.IsResolved && hasReply
		case "preserved":
			ok = thread.IsResolved == before.IsResolved && outcome.ReplyID == "" && len(newReplies) == 0
		default:
			ok = false
		}
		if !ok {
			return fmt.Errorf("discussion outcome readback mismatch")
		}
	}
	return nil
}

func newDiscussionReplyIDs(before, thread githubReviewThread, login string) []int64 {
	existing := make(map[int64]bool, len(before.Comments))
	for _, comment := range before.Comments {
		existing[comment.DatabaseID] = true
	}
	var ids []int64
	for _, comment := range thread.Comments {
		if !existing[comment.DatabaseID] && comment.ReplyToID != 0 && normalizeGitHubLogin(comment.Author) == login {
			ids = append(ids, comment.DatabaseID)
		}
	}
	return ids
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
	// ponytail: The sentinel avoids pagination until a repository exceeds 1,000 open PRs.
	command := exec.CommandContext(ctx, "gh", "pr", "list", "--repo", repository.Repository, "--state", "open",
		"--limit", "1001", "--json", "url,number,state,isDraft,headRefOid")
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
	if len(response) >= 1001 {
		return nil, fail("github_snapshot_too_large",
			"repository %s has more than 1,000 open pull requests; add pagination before discovery can continue",
			repository.Repository)
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
