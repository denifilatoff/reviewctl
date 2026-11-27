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
	Body       string
}

type githubReviewThread struct {
	ID         string
	IsResolved bool
	Comments   []githubReviewComment
}

type githubReviewThreadWire struct {
	ID         string                              `json:"id"`
	IsResolved *bool                               `json:"isResolved"`
	Comments   githubPage[githubReviewCommentWire] `json:"comments"`
}

type githubReviewCommentWire struct {
	DatabaseID int64  `json:"databaseId"`
	Body       string `json:"body"`
	Author     struct {
		Login string `json:"login"`
	} `json:"author"`
	ReplyTo *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"replyTo"`
}

type githubPage[T any] struct {
	Nodes    []T `json:"nodes"`
	PageInfo struct {
		HasNextPage *bool  `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
}

func (page githubPage[T]) nextCursor(seen map[string]bool) (string, error) {
	info := page.PageInfo
	if page.Nodes == nil || info.HasNextPage == nil ||
		(len(page.Nodes) == 0 && (*info.HasNextPage || len(seen) != 0)) {
		return "", fmt.Errorf("GitHub returned an incomplete page")
	}
	if info.EndCursor != "" && seen[info.EndCursor] {
		return "", fmt.Errorf("GitHub repeated a pagination cursor")
	}
	if !*info.HasNextPage {
		return "", nil
	}
	if info.EndCursor == "" {
		return "", fmt.Errorf("GitHub returned no cursor for the next page")
	}
	seen[info.EndCursor] = true
	return info.EndCursor, nil
}

const reviewCommentFields = `nodes{databaseId author{login} replyTo{databaseId} body} pageInfo{hasNextPage endCursor}`
const reviewThreadsQuery = `query($owner:String!,$name:String!,$number:Int!,$after:String){
	repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$after){
		nodes{id isResolved comments(first:100){` + reviewCommentFields + `}} pageInfo{hasNextPage endCursor}
	}}}
}`
const reviewThreadQuery = `query($id:ID!,$after:String){
	node(id:$id){... on PullRequestReviewThread{id isResolved comments(first:100,after:$after){` + reviewCommentFields + `}}}
}`

func listGitHubReviewThreads(ctx context.Context, pr PullRequest) ([]githubReviewThread, error) {
	owner, name, ok := strings.Cut(pr.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fail("github_failed", "invalid GitHub repository %s", pr.Repository)
	}
	args := []string{"graphql", "-f", "query=" + reviewThreadsQuery,
		"-f", "owner=" + owner, "-f", "name=" + name, "-F", fmt.Sprintf("number=%d", pr.Number)}
	var after string
	threads := []githubReviewThread{}
	seenCursors := make(map[string]bool)
	seenThreads := make(map[string]bool)
	for {
		page, err := readGitHubReviewThreadsPage(ctx, args, after)
		if err != nil {
			return nil, fail("github_failed", "read review threads for %s: %v", pr.URL, err)
		}
		after, err = page.nextCursor(seenCursors)
		if err != nil {
			return nil, fail("github_failed", "read review threads for %s: %v", pr.URL, err)
		}
		for _, item := range page.Nodes {
			if seenThreads[item.ID] {
				return nil, fail("github_failed", "GitHub repeated review thread %s for %s", item.ID, pr.URL)
			}
			thread, err := completeGitHubReviewThread(ctx, item)
			if err != nil {
				return nil, err
			}
			seenThreads[item.ID] = true
			threads = append(threads, thread)
		}
		if after == "" {
			return threads, nil
		}
	}
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

func readGitHubReviewThreadsPage(ctx context.Context, args []string, after string) (githubPage[githubReviewThreadWire], error) {
	if after != "" {
		args = append(args, "-f", "after="+after)
	}
	var response struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads githubPage[githubReviewThreadWire]
				}
			}
		}
		Errors []struct{ Message string }
	}
	if err := githubJSON(ctx, args, nil, &response); err != nil {
		return githubPage[githubReviewThreadWire]{}, err
	}
	if len(response.Errors) > 0 {
		return githubPage[githubReviewThreadWire]{}, fmt.Errorf("GitHub GraphQL: %s", response.Errors[0].Message)
	}
	if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
		return githubPage[githubReviewThreadWire]{}, fmt.Errorf("GitHub returned no pull request")
	}
	return response.Data.Repository.PullRequest.ReviewThreads, nil
}

func readGitHubReviewThread(ctx context.Context, id string) (githubReviewThread, error) {
	item, err := readGitHubReviewThreadPage(ctx, id, "")
	if err != nil {
		return githubReviewThread{}, err
	}
	return completeGitHubReviewThread(ctx, item)
}

func readGitHubReviewThreadPage(ctx context.Context, id, after string) (githubReviewThreadWire, error) {
	args := []string{"graphql", "-f", "query=" + reviewThreadQuery, "-f", "id=" + id}
	if after != "" {
		args = append(args, "-f", "after="+after)
	}
	var response struct {
		Data   struct{ Node *githubReviewThreadWire }
		Errors []struct{ Message string }
	}
	if err := githubJSON(ctx, args, nil, &response); err != nil {
		return githubReviewThreadWire{}, fail("github_failed", "read review thread %s: %v", id, err)
	}
	if len(response.Errors) != 0 {
		return githubReviewThreadWire{}, fail("github_failed", "read review thread %s: %s", id, response.Errors[0].Message)
	}
	if response.Data.Node == nil {
		return githubReviewThreadWire{}, fail("discussion_changed", "review thread %s is unavailable; reassess before writing", id)
	}
	item := *response.Data.Node
	if item.ID != id || item.IsResolved == nil {
		return githubReviewThreadWire{}, fail("github_failed", "GitHub returned an incomplete or mismatched review thread %s", id)
	}
	return item, nil
}

func completeGitHubReviewThread(ctx context.Context, item githubReviewThreadWire) (githubReviewThread, error) {
	if item.ID == "" || item.IsResolved == nil || len(item.Comments.Nodes) == 0 {
		return githubReviewThread{}, fail("github_failed", "GitHub returned an incomplete review thread %q", item.ID)
	}
	thread := githubReviewThread{ID: item.ID, IsResolved: *item.IsResolved}
	seenCursors := make(map[string]bool)
	seenComments := make(map[int64]bool)
	for {
		after, err := item.Comments.nextCursor(seenCursors)
		if err != nil {
			return githubReviewThread{}, fail("github_failed", "read review thread %s: %v", thread.ID, err)
		}
		for _, comment := range item.Comments.Nodes {
			if comment.DatabaseID <= 0 || seenComments[comment.DatabaseID] {
				return githubReviewThread{}, fail("github_failed", "GitHub returned a missing or repeated comment ID in review thread %s", thread.ID)
			}
			replyTo := int64(0)
			if comment.ReplyTo != nil {
				replyTo = comment.ReplyTo.DatabaseID
			}
			thread.Comments = append(thread.Comments, githubReviewComment{
				DatabaseID: comment.DatabaseID, Author: comment.Author.Login, ReplyToID: replyTo, Body: comment.Body,
			})
			seenComments[comment.DatabaseID] = true
		}
		if after == "" {
			return thread, nil
		}
		item, err = readGitHubReviewThreadPage(ctx, thread.ID, after)
		if err != nil {
			return githubReviewThread{}, err
		}
		if *item.IsResolved != thread.IsResolved {
			return githubReviewThread{}, fail("discussion_changed", "review thread %s changed state during pagination; reassess before writing", thread.ID)
		}
	}
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
		if !commentsPreserved(before.Comments, after.Comments) {
			return fmt.Errorf("pre-existing discussion comment changed during review")
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

func commentsPreserved(before, after []githubReviewComment) bool {
	current := make(map[int64]githubReviewComment, len(after))
	for _, comment := range after {
		if _, duplicate := current[comment.DatabaseID]; duplicate {
			return false
		}
		current[comment.DatabaseID] = comment
	}
	for _, comment := range before {
		preserved, found := current[comment.DatabaseID]
		if !found || preserved != comment {
			return false
		}
	}
	return true
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
	Author  string
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
		"--limit", "1001", "--json", "url,number,state,isDraft,headRefOid,author")
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
		Author     struct {
			Login string `json:"login"`
		} `json:"author"`
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
		snapshot[i] = pullRequestSnapshot{
			PullRequest: pr, IsDraft: item.IsDraft, HeadSHA: item.HeadRefOID, Author: item.Author.Login,
		}
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Number < snapshot[j].Number })
	return snapshot, nil
}

func ValidateGitHubPullRequest(cfg Config, expected PullRequest, resolved GitHubPullRequest) error {
	if err := validateGitHubReviewTarget(cfg, expected, resolved); err != nil {
		return err
	}
	if resolved.State != "OPEN" {
		return fail("pr_not_open", "pull request is not open")
	}
	if resolved.IsDraft {
		return fail("pr_draft", "pull request is a draft")
	}
	return nil
}

// Existing reviews can receive their accounting signature after a pull request closes.
func validateGitHubReviewTarget(cfg Config, expected PullRequest, resolved GitHubPullRequest) error {
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
	if !isTrustedGitHubAuthor(cfg, resolved.Author) {
		return fail("untrusted_author", "pull request author %s is not trusted", resolved.Author)
	}
	if !samePullRequestIdentity(resolved.URL, expected) || resolved.Number != expected.Number || resolved.HeadSHA == "" {
		return fail("github_mismatch", "GitHub response does not match the queued pull request")
	}
	return nil
}

func isTrustedGitHubAuthor(cfg Config, login string) bool {
	author := normalizeGitHubLogin(login)
	for _, configured := range cfg.TrustedAuthors {
		if configured == author {
			return true
		}
	}
	return false
}
