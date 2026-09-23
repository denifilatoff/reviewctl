package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type githubExternalInput struct {
	Body               string
	MessageIDs         []string
	ObservedMessageIDs []string
}

type githubExternalActor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

type githubExternalMessage struct {
	ID     string               `json:"id"`
	Body   string               `json:"body"`
	Author *githubExternalActor `json:"author"`
}

type githubExternalPageInfo struct {
	HasNextPage *bool  `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type githubExternalMessageConnection struct {
	Nodes    []githubExternalMessage `json:"nodes"`
	PageInfo *githubExternalPageInfo `json:"pageInfo"`
}

type githubExternalThread struct {
	ID       string                           `json:"id"`
	Comments *githubExternalMessageConnection `json:"comments"`
}

type githubExternalThreadConnection struct {
	Nodes    []githubExternalThread  `json:"nodes"`
	PageInfo *githubExternalPageInfo `json:"pageInfo"`
}

type githubGraphQLError struct {
	Message string `json:"message"`
}

const externalInputInitialQuery = `query ExternalInputInitial($owner:String!,$name:String!,$number:Int!){viewer{login}repository(owner:$owner,name:$name){pullRequest(number:$number){body comments(first:100){nodes{id body author{login __typename}}pageInfo{hasNextPage endCursor}}reviews(first:100){nodes{id body author{login __typename}}pageInfo{hasNextPage endCursor}}reviewThreads(first:100){nodes{id comments(first:100){nodes{id body author{login __typename}}pageInfo{hasNextPage endCursor}}}pageInfo{hasNextPage endCursor}}}}}`

const externalInputIssueCommentsQuery = `query ExternalInputIssueComments($owner:String!,$name:String!,$number:Int!,$cursor:String!){repository(owner:$owner,name:$name){pullRequest(number:$number){comments(first:100,after:$cursor){nodes{id body author{login __typename}}pageInfo{hasNextPage endCursor}}}}}`

const externalInputReviewsQuery = `query ExternalInputReviews($owner:String!,$name:String!,$number:Int!,$cursor:String!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviews(first:100,after:$cursor){nodes{id body author{login __typename}}pageInfo{hasNextPage endCursor}}}}}`

const externalInputReviewThreadsQuery = `query ExternalInputReviewThreads($owner:String!,$name:String!,$number:Int!,$cursor:String!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$cursor){nodes{id comments(first:100){nodes{id body author{login __typename}}pageInfo{hasNextPage endCursor}}}pageInfo{hasNextPage endCursor}}}}}`

const externalInputThreadCommentsQuery = `query ExternalInputThreadComments($thread:ID!,$cursor:String!){node(id:$thread){... on PullRequestReviewThread{comments(first:100,after:$cursor){nodes{id body author{login __typename}}pageInfo{hasNextPage endCursor}}}}}`

func mergeExternalInput(_ string, previousIDs []string, current githubExternalInput) (string, []string, string) {
	bodyDigest, messageIDs, _, revision := mergeExternalInputState(previousIDs, previousIDs, current)
	return bodyDigest, messageIDs, revision
}

func mergeExternalInputState(previousIDs, previousObservedIDs []string, current githubExternalInput) (
	string, []string, []string, string,
) {
	bodyHash := sha256.Sum256([]byte(current.Body))
	bodyDigest := "sha256:" + hex.EncodeToString(bodyHash[:])
	messageIDs, observedIDs := mergeExternalMessageIDs(previousIDs, previousObservedIDs, current.MessageIDs,
		current.ObservedMessageIDs)
	return bodyDigest, messageIDs, observedIDs, externalInputRevision(bodyDigest, messageIDs)
}

func mergeExternalMessageIDs(previousIDs, previousObservedIDs, currentIDs, currentObservedIDs []string) (
	[]string, []string,
) {
	observed := make(map[string]bool, len(previousObservedIDs)+len(currentObservedIDs))
	for _, id := range previousObservedIDs {
		if id != "" {
			observed[id] = true
		}
	}
	eligible := make(map[string]bool, len(previousIDs)+len(currentIDs))
	for _, id := range previousIDs {
		if id != "" {
			eligible[id] = true
		}
	}
	for _, id := range currentIDs {
		if id != "" && !observed[id] {
			eligible[id] = true
		}
	}
	for _, id := range currentObservedIDs {
		if id != "" {
			observed[id] = true
		}
	}
	messageIDs := make([]string, 0, len(eligible))
	for id := range eligible {
		messageIDs = append(messageIDs, id)
	}
	sort.Strings(messageIDs)
	observedIDs := make([]string, 0, len(observed))
	for id := range observed {
		observedIDs = append(observedIDs, id)
	}
	sort.Strings(observedIDs)
	return messageIDs, observedIDs
}

func externalInputRevision(bodyDigest string, messageIDs []string) string {
	hash := sha256.New()
	hash.Write([]byte(bodyDigest))
	hash.Write([]byte{0})
	for _, id := range messageIDs {
		hash.Write([]byte(id))
		hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func readGitHubExternalInput(ctx context.Context, pr PullRequest) (githubExternalInput, error) {
	owner, name, ok := strings.Cut(pr.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return githubExternalInput{}, fail("github_failed", "invalid GitHub repository %s", pr.Repository)
	}
	baseArgs := []string{"-f", "owner=" + owner, "-f", "name=" + name, "-F", fmt.Sprintf("number=%d", pr.Number)}
	data, err := runGitHubGraphQL(ctx, externalInputInitialQuery, baseArgs...)
	if err != nil {
		return githubExternalInput{}, err
	}
	var initial struct {
		Data struct {
			Viewer     *githubExternalActor `json:"viewer"`
			Repository *struct {
				PullRequest *struct {
					Body          *string                          `json:"body"`
					Comments      *githubExternalMessageConnection `json:"comments"`
					Reviews       *githubExternalMessageConnection `json:"reviews"`
					ReviewThreads *githubExternalThreadConnection  `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []githubGraphQLError `json:"errors"`
	}
	if err := json.Unmarshal(data, &initial); err != nil {
		return githubExternalInput{}, fail("github_failed", "decode external input: %v", err)
	}
	if err := validateGraphQLResponse(initial.Errors); err != nil {
		return githubExternalInput{}, err
	}
	if initial.Data.Viewer == nil || normalizeGitHubLogin(initial.Data.Viewer.Login) == "" ||
		initial.Data.Repository == nil || initial.Data.Repository.PullRequest == nil ||
		initial.Data.Repository.PullRequest.Body == nil || initial.Data.Repository.PullRequest.Comments == nil ||
		initial.Data.Repository.PullRequest.Reviews == nil ||
		initial.Data.Repository.PullRequest.ReviewThreads == nil ||
		!completeExternalPage(initial.Data.Repository.PullRequest.Comments.PageInfo) ||
		!completeExternalPage(initial.Data.Repository.PullRequest.Reviews.PageInfo) ||
		!completeExternalPage(initial.Data.Repository.PullRequest.ReviewThreads.PageInfo) {
		return githubExternalInput{}, fail("github_failed", "GitHub returned incomplete external input")
	}
	viewer := normalizeGitHubLogin(initial.Data.Viewer.Login)
	pullRequest := initial.Data.Repository.PullRequest
	input := githubExternalInput{Body: *pullRequest.Body}
	if err := appendExternalMessages(&input, viewer, pullRequest.Comments.Nodes); err != nil {
		return githubExternalInput{}, err
	}
	if err := appendExternalMessages(&input, viewer, pullRequest.Reviews.Nodes); err != nil {
		return githubExternalInput{}, err
	}
	if err := appendExternalThreads(ctx, &input, viewer, pullRequest.ReviewThreads.Nodes); err != nil {
		return githubExternalInput{}, err
	}
	if err := appendExternalMessagePages(ctx, &input, viewer, externalInputIssueCommentsQuery, "comments",
		*pullRequest.Comments.PageInfo, baseArgs); err != nil {
		return githubExternalInput{}, err
	}
	if err := appendExternalMessagePages(ctx, &input, viewer, externalInputReviewsQuery, "reviews",
		*pullRequest.Reviews.PageInfo, baseArgs); err != nil {
		return githubExternalInput{}, err
	}
	if err := appendExternalThreadPages(ctx, &input, viewer, *pullRequest.ReviewThreads.PageInfo,
		baseArgs); err != nil {
		return githubExternalInput{}, err
	}
	sort.Strings(input.MessageIDs)
	sort.Strings(input.ObservedMessageIDs)
	return input, nil
}

func runGitHubGraphQL(ctx context.Context, query string, variables ...string) ([]byte, error) {
	args := append([]string{"api", "graphql", "-f", "query=" + query}, variables...)
	command := exec.CommandContext(ctx, "gh", args...)
	prepareProcessGroup(command)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := cleanupProcessGroup(command, command.Run()); err != nil {
		return nil, fail("github_failed", "read external input: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func validateGraphQLResponse(errors []githubGraphQLError) error {
	if len(errors) > 0 {
		return fail("github_failed", "GitHub GraphQL: %s", errors[0].Message)
	}
	return nil
}

func completeExternalPage(page *githubExternalPageInfo) bool {
	return page != nil && page.HasNextPage != nil
}

func hasNextExternalPage(page githubExternalPageInfo) bool {
	return page.HasNextPage != nil && *page.HasNextPage
}

func appendExternalMessages(input *githubExternalInput, viewer string, messages []githubExternalMessage) error {
	for _, message := range messages {
		if message.Author == nil || message.Author.Typename != "User" ||
			normalizeGitHubLogin(message.Author.Login) == viewer {
			continue
		}
		if message.ID == "" {
			return fail("github_failed", "GitHub returned an external message without an ID")
		}
		input.ObservedMessageIDs = append(input.ObservedMessageIDs, message.ID)
		if strings.TrimSpace(message.Body) != "" {
			input.MessageIDs = append(input.MessageIDs, message.ID)
		}
	}
	return nil
}

func appendExternalThreads(ctx context.Context, input *githubExternalInput, viewer string,
	threads []githubExternalThread,
) error {
	for _, thread := range threads {
		if thread.ID == "" || thread.Comments == nil || !completeExternalPage(thread.Comments.PageInfo) {
			return fail("github_failed", "GitHub returned an external thread without an ID")
		}
		if err := appendExternalMessages(input, viewer, thread.Comments.Nodes); err != nil {
			return err
		}
		page := *thread.Comments.PageInfo
		seen := make(map[string]bool)
		for hasNextExternalPage(page) {
			if page.EndCursor == "" || seen[page.EndCursor] {
				return fail("github_failed", "GitHub returned an incomplete external input cursor")
			}
			seen[page.EndCursor] = true
			data, err := runGitHubGraphQL(ctx, externalInputThreadCommentsQuery,
				"-f", "thread="+thread.ID, "-f", "cursor="+page.EndCursor)
			if err != nil {
				return err
			}
			var response struct {
				Data struct {
					Node *struct {
						Comments *githubExternalMessageConnection `json:"comments"`
					} `json:"node"`
				} `json:"data"`
				Errors []githubGraphQLError `json:"errors"`
			}
			if err := json.Unmarshal(data, &response); err != nil || response.Data.Node == nil ||
				response.Data.Node.Comments == nil || !completeExternalPage(response.Data.Node.Comments.PageInfo) {
				return fail("github_failed", "decode external thread comments: %v", err)
			}
			if err := validateGraphQLResponse(response.Errors); err != nil {
				return err
			}
			if err := appendExternalMessages(input, viewer, response.Data.Node.Comments.Nodes); err != nil {
				return err
			}
			page = *response.Data.Node.Comments.PageInfo
		}
	}
	return nil
}

func appendExternalMessagePages(ctx context.Context, input *githubExternalInput, viewer, query, field string,
	page githubExternalPageInfo, baseArgs []string,
) error {
	seen := make(map[string]bool)
	for hasNextExternalPage(page) {
		if page.EndCursor == "" || seen[page.EndCursor] {
			return fail("github_failed", "GitHub returned an incomplete external input cursor")
		}
		seen[page.EndCursor] = true
		data, err := runGitHubGraphQL(ctx, query, append(baseArgs, "-f", "cursor="+page.EndCursor)...)
		if err != nil {
			return err
		}
		var response struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						Comments *githubExternalMessageConnection `json:"comments"`
						Reviews  *githubExternalMessageConnection `json:"reviews"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []githubGraphQLError `json:"errors"`
		}
		if err := json.Unmarshal(data, &response); err != nil || response.Data.Repository == nil ||
			response.Data.Repository.PullRequest == nil {
			return fail("github_failed", "decode external %s: %v", field, err)
		}
		if err := validateGraphQLResponse(response.Errors); err != nil {
			return err
		}
		connection := response.Data.Repository.PullRequest.Comments
		if field == "reviews" {
			connection = response.Data.Repository.PullRequest.Reviews
		}
		if connection == nil || !completeExternalPage(connection.PageInfo) {
			return fail("github_failed", "GitHub returned incomplete external %s", field)
		}
		if err := appendExternalMessages(input, viewer, connection.Nodes); err != nil {
			return err
		}
		page = *connection.PageInfo
	}
	return nil
}

func appendExternalThreadPages(ctx context.Context, input *githubExternalInput, viewer string,
	page githubExternalPageInfo,
	baseArgs []string,
) error {
	seen := make(map[string]bool)
	for hasNextExternalPage(page) {
		if page.EndCursor == "" || seen[page.EndCursor] {
			return fail("github_failed", "GitHub returned an incomplete external input cursor")
		}
		seen[page.EndCursor] = true
		data, err := runGitHubGraphQL(ctx, externalInputReviewThreadsQuery,
			append(baseArgs, "-f", "cursor="+page.EndCursor)...)
		if err != nil {
			return err
		}
		var response struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						ReviewThreads *githubExternalThreadConnection `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
			Errors []githubGraphQLError `json:"errors"`
		}
		if err := json.Unmarshal(data, &response); err != nil || response.Data.Repository == nil ||
			response.Data.Repository.PullRequest == nil {
			return fail("github_failed", "decode external review threads: %v", err)
		}
		if err := validateGraphQLResponse(response.Errors); err != nil {
			return err
		}
		threads := response.Data.Repository.PullRequest.ReviewThreads
		if threads == nil || !completeExternalPage(threads.PageInfo) {
			return fail("github_failed", "GitHub returned incomplete external review threads")
		}
		if err := appendExternalThreads(ctx, input, viewer, threads.Nodes); err != nil {
			return err
		}
		page = *threads.PageInfo
	}
	return nil
}

const reviewThreadsQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){nodes{id isResolved comments(first:100){nodes{databaseId author{login} replyTo{databaseId} body} pageInfo{hasNextPage}}} pageInfo{hasNextPage}}}}}`

func listGitHubReviewThreads(ctx context.Context, pr PullRequest) ([]githubReviewThread, error) {
	owner, name, ok := strings.Cut(pr.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fail("github_failed", "invalid GitHub repository %s", pr.Repository)
	}
	command := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+reviewThreadsQuery, "-f",
		"owner="+owner, "-f", "name="+name, "-F", fmt.Sprintf("number=%d", pr.Number))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fail("github_failed", "read review threads: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	threads, err := decodeGitHubReviewThreads(stdout.Bytes())
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
									DatabaseID int64  `json:"databaseId"`
									Body       string `json:"body"`
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
				DatabaseID: comment.DatabaseID, Author: comment.Author.Login, ReplyToID: replyTo, Body: comment.Body,
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
	IsDraft            bool
	HeadSHA            string
	Author             string
	DescriptionDigest  string
	MessageIDs         []string
	ObservedMessageIDs []string
}

func shouldEnqueueDiscovery(initialized bool, previous *pullRequestSnapshot, current pullRequestSnapshot) bool {
	if !initialized || current.IsDraft {
		return false
	}
	return previous == nil || previous.IsDraft || previous.HeadSHA != current.HeadSHA ||
		(previous.InputRevision != "" && previous.InputRevision != current.InputRevision)
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
