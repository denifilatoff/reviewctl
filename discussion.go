package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
)

func discussionReplyBody(publication Publication, action DiscussionAction) string {
	seed := reviewMarker(publication.Attempt.PullRequest, publication.Attempt.HeadSHA, publication.Attempt.SkillDigest) + publication.Attempt.StartedAt.String()
	return action.Body + fmt.Sprintf("\n\n<!-- reviewctl:discussion:%x:%s -->", sha256.Sum256([]byte(seed)), action.ThreadID)
}

func inspectPublicationDiscussion(publication Publication, action DiscussionAction, before, current githubReviewThread) (bool, int64, error) {
	if len(before.Comments) == 0 || len(current.Comments) == 0 || current.ID != action.ThreadID ||
		normalizeGitHubLogin(current.Comments[0].Author) != publication.Login || !commentsPreserved(before.Comments, current.Comments) {
		return false, 0, fail("discussion_changed", "owned discussion %s changed; reassess before writing", action.ThreadID)
	}
	resolved := before.IsResolved
	if action.Action != "preserved" {
		resolved = action.Action != "open_with_reply"
	}
	if current.IsResolved != before.IsResolved && current.IsResolved != resolved {
		return false, 0, fail("discussion_changed", "discussion %s was resolved or reopened; reassess before writing", action.ThreadID)
	}
	body := discussionReplyBody(publication, action)
	var replyID int64
	for _, comment := range current.Comments {
		known := false
		for _, original := range before.Comments {
			known = known || original.DatabaseID == comment.DatabaseID
		}
		if known {
			continue
		}
		if replyID != 0 || action.Body == "" || comment.Body != body || normalizeGitHubLogin(comment.Author) != publication.Login ||
			comment.ReplyToID != before.Comments[0].DatabaseID || comment.DatabaseID <= 0 {
			return false, 0, fail("discussion_changed", "discussion %s has new comments; reassess before writing", action.ThreadID)
		}
		replyID = comment.DatabaseID
	}
	return resolved, replyID, nil
}

func (publication Publication) receivedDiscussionFeedback(before, current githubReviewThread) bool {
	if !publication.RecoverAcrossHeads {
		return false
	}
	original := make(map[int64]githubReviewComment, len(before.Comments))
	for _, comment := range before.Comments {
		original[comment.DatabaseID] = comment
	}
	for _, comment := range current.Comments {
		if normalizeGitHubLogin(comment.Author) != publication.Login &&
			original[comment.DatabaseID] != comment {
			return true
		}
	}
	return false
}

func syncPublicationDiscussions(ctx context.Context, cfg Config, publication Publication) ([]DiscussionOutcome, error) {
	pr := publication.Attempt.PullRequest
	threads, err := listGitHubReviewThreads(ctx, pr)
	if err != nil {
		return nil, err
	}
	initial := make(map[string]githubReviewThread, len(publication.Threads))
	currentThreads := make(map[string]githubReviewThread, len(threads))
	for _, thread := range publication.Threads {
		initial[thread.ID] = thread
	}
	for _, thread := range threads {
		currentThreads[thread.ID] = thread
	}
	outcomes := make([]DiscussionOutcome, 0, len(publication.Draft.Discussions))
	deferred := make(map[string]bool)
	for _, action := range publication.Draft.Discussions {
		before, current := initial[action.ThreadID], currentThreads[action.ThreadID]
		if action.Action != "preserved" {
			current, err = readGitHubReviewThread(ctx, action.ThreadID)
			if err != nil {
				return nil, err
			}
			if _, err := publication.targetAfterStart(ctx, cfg); err != nil {
				return nil, err
			}
		}
		if publication.receivedDiscussionFeedback(before, current) {
			deferred[action.ThreadID] = true
			continue
		}
		resolved, replyID, err := inspectPublicationDiscussion(publication, action, before, current)
		if err != nil {
			return nil, err
		}
		outcome := DiscussionOutcome{ThreadID: action.ThreadID, Action: action.Action}
		if action.Body != "" {
			if replyID == 0 {
				endpoint := fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", pr.Repository, pr.Number, before.Comments[0].DatabaseID)
				var reply struct {
					ID int64 `json:"id"`
				}
				if err := githubJSON(ctx, []string{"--method", "POST", endpoint},
					map[string]string{"body": discussionReplyBody(publication, action)}, &reply); err != nil {
					if isGitHubValidationError(err) {
						after, lookupErr := readGitHubReviewThread(ctx, action.ThreadID)
						if lookupErr == nil {
							if publication.receivedDiscussionFeedback(before, after) {
								deferred[action.ThreadID] = true
								continue
							}
							_, created, inspectErr := inspectPublicationDiscussion(publication, action, before, after)
							if inspectErr != nil {
								return nil, inspectErr
							}
							if created == 0 {
								return nil, fail("draft_rejected", "GitHub rejected the reply for discussion %s; reassess before retrying: %v", action.ThreadID, err)
							}
						}
					}
					return nil, err
				}
				replyID = reply.ID
				if replyID <= 0 {
					return nil, fmt.Errorf("GitHub returned no discussion reply ID")
				}
				current, err = readGitHubReviewThread(ctx, action.ThreadID)
				if err != nil {
					return nil, err
				}
				if _, err := publication.targetAfterStart(ctx, cfg); err != nil {
					return nil, err
				}
				if publication.receivedDiscussionFeedback(before, current) {
					deferred[action.ThreadID] = true
					continue
				}
				_, recovered, err := inspectPublicationDiscussion(publication, action, before, current)
				if err != nil {
					return nil, err
				}
				if recovered != replyID {
					return nil, fmt.Errorf("discussion reply %d readback did not match; retry delivery", replyID)
				}
			}
			outcome.ReplyID = json.Number(strconv.FormatInt(replyID, 10))
		}
		if action.Action != "preserved" && current.IsResolved != resolved {
			operation := "resolveReviewThread"
			if !resolved {
				operation = "unresolveReviewThread"
			}
			query := fmt.Sprintf("mutation($id:ID!){%s(input:{threadId:$id}){thread{id isResolved}}}", operation)
			var response struct{ Errors []struct{ Message string } }
			if err := githubJSON(ctx, []string{"graphql"}, map[string]any{"query": query,
				"variables": map[string]string{"id": action.ThreadID}}, &response); err != nil {
				return nil, err
			}
			if len(response.Errors) != 0 {
				return nil, fmt.Errorf("update review thread: %s", response.Errors[0].Message)
			}
		}
		outcomes = append(outcomes, outcome)
	}
	current, err := listGitHubReviewThreads(ctx, pr)
	if err != nil {
		return nil, err
	}
	clear(currentThreads)
	for _, thread := range current {
		currentThreads[thread.ID] = thread
	}
	for _, action := range publication.Draft.Discussions {
		before, after := initial[action.ThreadID], currentThreads[action.ThreadID]
		if publication.receivedDiscussionFeedback(before, after) {
			deferred[action.ThreadID] = true
		}
		if !deferred[action.ThreadID] {
			if _, _, err := inspectPublicationDiscussion(publication, action, before, after); err != nil {
				return nil, err
			}
		}
	}
	threads = publication.Threads
	if len(deferred) != 0 {
		threads = make([]githubReviewThread, 0, len(publication.Threads))
		for _, thread := range publication.Threads {
			if !deferred[thread.ID] {
				threads = append(threads, thread)
			}
		}
		verified := outcomes[:0]
		for _, outcome := range outcomes {
			if !deferred[outcome.ThreadID] {
				verified = append(verified, outcome)
			}
		}
		outcomes = verified
	}
	if err := validateDiscussionOutcomes(threads, current, publication.Login, outcomes); err != nil {
		return nil, fail("discussion_changed", "discussion synchronization readback: %v", err)
	}
	return outcomes, nil
}
