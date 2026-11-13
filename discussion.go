package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
)

func syncPublicationDiscussions(ctx context.Context, publication Publication) error {
	pr, login := publication.Attempt.PullRequest, publication.Login
	outcomes := make([]DiscussionOutcome, 0, len(publication.Draft.Discussions))
	for _, action := range publication.Draft.Discussions {
		threads, err := listGitHubReviewThreads(ctx, pr)
		if err != nil {
			return err
		}
		var before, current githubReviewThread
		for _, thread := range publication.Threads {
			if thread.ID == action.ThreadID {
				before = thread
			}
		}
		for _, thread := range threads {
			if thread.ID == action.ThreadID {
				current = thread
			}
		}
		if len(before.Comments) == 0 || len(current.Comments) == 0 ||
			normalizeGitHubLogin(current.Comments[0].Author) != login || !commentsPreserved(before.Comments, current.Comments) {
			return fail("discussion_changed", "owned discussion %s changed; reassess it before writing", action.ThreadID)
		}
		resolved := before.IsResolved
		if action.Action != "preserved" {
			resolved = action.Action != "open_with_reply"
		}
		if current.IsResolved != before.IsResolved && current.IsResolved != resolved {
			return fail("discussion_changed", "discussion %s was resolved or reopened; reassess it before writing", action.ThreadID)
		}
		seed := reviewMarker(pr, publication.Attempt.HeadSHA, publication.Attempt.SkillDigest) + publication.Attempt.StartedAt.String()
		marker := fmt.Sprintf("<!-- reviewctl:discussion:%x:%s -->", sha256.Sum256([]byte(seed)), action.ThreadID)
		body := action.Body + "\n\n" + marker
		var replyID int64
		for _, comment := range current.Comments {
			known := false
			for _, original := range before.Comments {
				known = known || original.DatabaseID == comment.DatabaseID
			}
			if known {
				continue
			}
			if replyID != 0 || action.Body == "" || comment.Body != body || normalizeGitHubLogin(comment.Author) != login ||
				comment.ReplyToID != before.Comments[0].DatabaseID || comment.DatabaseID <= 0 {
				return fail("discussion_changed", "discussion %s has new comments; reassess it before writing", action.ThreadID)
			}
			replyID = comment.DatabaseID
		}
		outcome := DiscussionOutcome{ThreadID: action.ThreadID, Action: action.Action}
		if action.Body != "" {
			if replyID == 0 {
				endpoint := fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", pr.Repository, pr.Number, before.Comments[0].DatabaseID)
				var reply struct {
					ID int64 `json:"id"`
				}
				if err := githubJSON(ctx, []string{"--method", "POST", endpoint}, map[string]string{"body": body}, &reply); err != nil {
					return err
				}
				replyID = reply.ID
				if replyID <= 0 {
					return fmt.Errorf("GitHub returned no discussion reply ID")
				}
			}
			outcome.ReplyID = json.Number(strconv.FormatInt(replyID, 10))
		}
		if action.Action != "preserved" {
			if current.IsResolved != resolved {
				operation := "resolveReviewThread"
				if !resolved {
					operation = "unresolveReviewThread"
				}
				query := fmt.Sprintf("mutation($id:ID!){%s(input:{threadId:$id}){thread{id isResolved}}}", operation)
				var response struct {
					Errors []struct{ Message string }
				}
				if err := githubJSON(ctx, []string{"graphql"}, map[string]any{"query": query,
					"variables": map[string]string{"id": action.ThreadID}}, &response); err != nil {
					return err
				}
				if len(response.Errors) != 0 {
					return fmt.Errorf("update review thread: %s", response.Errors[0].Message)
				}
			}
		}
		outcomes = append(outcomes, outcome)
	}
	current, err := listGitHubReviewThreads(ctx, pr)
	if err != nil {
		return err
	}
	if err := validateDiscussionOutcomes(publication.Threads, current, login, outcomes); err != nil {
		return fail("discussion_changed", "discussion synchronization readback: %v", err)
	}
	return nil
}
