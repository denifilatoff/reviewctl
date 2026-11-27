package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
)

type ReviewDraft struct {
	Provider           string              `json:"provider"`
	Repository         string              `json:"repository"`
	Number             int64               `json:"number"`
	HeadSHA            string              `json:"head_sha"`
	SkillDigest        string              `json:"skill_digest"`
	Verdict            string              `json:"verdict"`
	Body               string              `json:"body"`
	Comments           []ReviewComment     `json:"comments"`
	Discussions        []DiscussionAction  `json:"discussions"`
	NewFindings        bool                `json:"new_findings,omitempty"`
	FindingAssessments []FindingAssessment `json:"finding_assessments"`
	FindingPublication *FindingPublication `json:"finding_publication,omitempty"`
}

type ReviewComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

type DiscussionAction struct {
	ThreadID         string `json:"thread_id"`
	Action           string `json:"action"`
	Body             string `json:"body,omitempty"`
	FindingID        string `json:"finding_id,omitempty"`
	VerifiedSHA      string `json:"verified_sha,omitempty"`
	ExpectedResolver string `json:"expected_resolver,omitempty"`
}

type Publication struct {
	Attempt           Attempt
	Draft             ReviewDraft
	Login             string
	Threads           []githubReviewThread
	ExistingReviewID  int64
	PublishedReviewID int64 `json:",omitempty"`
}

func readDraft(filePath string) (ReviewDraft, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return ReviewDraft{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return ReviewDraft{}, err
	}
	if len(data) > 1<<20 {
		return ReviewDraft{}, fmt.Errorf("review draft exceeds 1 MiB")
	}
	var draft ReviewDraft
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&draft); err != nil {
		return draft, fmt.Errorf("decode review draft: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return draft, fmt.Errorf("review draft must contain one JSON object")
	}
	return draft, nil
}

func validateDraft(draft ReviewDraft, attempt Attempt, owned []string, selfAuthored bool) error {
	if draft.FindingPublication != nil || strings.Contains(draft.Body, signatureEnd) {
		return fmt.Errorf("review draft includes controller finding publication data")
	}
	if draft.Provider != attempt.Provider || strings.ToLower(draft.Repository) != attempt.Repository ||
		draft.Number != attempt.Number || draft.HeadSHA != attempt.HeadSHA || draft.SkillDigest != attempt.SkillDigest {
		return fmt.Errorf("review draft scope does not match the pinned attempt")
	}
	if draft.Verdict == "REVIEW_INCOMPLETE" {
		return fmt.Errorf("review incomplete: %s", strings.TrimSpace(draft.Body))
	}
	if draft.Verdict != "APPROVE" && draft.Verdict != "REQUEST_CHANGES" && !(selfAuthored && draft.Verdict == "COMMENT") {
		return fmt.Errorf("review draft has an invalid verdict")
	}
	if strings.TrimSpace(draft.Body) == "" || strings.Contains(draft.Body, "<!-- reviewctl:") ||
		draft.Comments == nil || draft.Discussions == nil {
		return fmt.Errorf("review draft requires body, comments, and discussions without controller markers")
	}
	for index, comment := range draft.Comments {
		field, constraint := "", ""
		switch {
		case comment.Path == "" || path.IsAbs(comment.Path) || path.Clean(comment.Path) != comment.Path ||
			comment.Path == ".." || strings.HasPrefix(comment.Path, "../") || strings.Contains(comment.Path, "\\"):
			field, constraint = "path", "must be a clean repository-relative file path"
		case comment.Line <= 0:
			field, constraint = "line", "must be positive"
		case comment.Side != "LEFT" && comment.Side != "RIGHT":
			field, constraint = "side", "must be LEFT or RIGHT"
		case strings.TrimSpace(comment.Body) == "":
			field, constraint = "body", "must not be empty"
		case comment.StartLine < 0 || comment.StartLine > comment.Line:
			field, constraint = "start_line", "must be zero or a positive line no greater than line"
		case comment.StartLine == 0 && comment.StartSide != "":
			field, constraint = "start_side", "must be omitted when start_line is zero"
		case comment.StartLine > 0 && comment.StartSide != comment.Side:
			field, constraint = "start_side", "must match side for a line range"
		}
		if field != "" {
			return fmt.Errorf("inline comment %d (%q:%d): %s %s", index+1, bounded(comment.Path, 128), comment.Line, field, constraint)
		}
	}
	if len(draft.Discussions) != len(owned) {
		return fmt.Errorf("discussion actions do not cover every owned discussion")
	}
	seen := map[string]bool{}
	for _, id := range owned {
		seen[id] = false
	}
	for _, action := range draft.Discussions {
		if action.FindingID != "" || action.VerifiedSHA != "" || action.ExpectedResolver != "" {
			return fmt.Errorf("review draft includes controller discussion action data")
		}
		if visited, exists := seen[action.ThreadID]; !exists || visited {
			return fmt.Errorf("discussion action does not match an owned discussion")
		}
		seen[action.ThreadID] = true
		switch action.Action {
		case "preserved", "resolved":
			if action.Body != "" {
				return fmt.Errorf("discussion action without a reply must omit body")
			}
		case "resolved_with_reply", "open_with_reply":
			if strings.TrimSpace(action.Body) == "" || strings.Contains(action.Body, "<!-- reviewctl:") {
				return fmt.Errorf("discussion reply requires body without controller markers")
			}
		default:
			return fmt.Errorf("review draft contains an invalid discussion action")
		}
	}
	return nil
}

func (s *Store) pendingPublication(ctx context.Context, pr PullRequest) (*Publication, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM review_publications
		WHERE provider = ? AND repository = ? AND change_number = ?`, pr.Provider, pr.Repository, pr.Number).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var publication Publication
	if err := json.Unmarshal([]byte(raw), &publication); err != nil {
		return nil, err
	}
	if publication.Attempt.PullRequest != pr {
		return nil, fmt.Errorf("saved publication does not match the queued pull request")
	}
	return &publication, nil
}

func (s *Store) savePublication(ctx context.Context, publication Publication) error {
	raw, err := json.Marshal(publication)
	if err != nil {
		return err
	}
	pr := publication.Attempt.PullRequest
	_, err = s.db.ExecContext(ctx, `INSERT INTO review_publications(provider, repository, change_number, payload)
		VALUES (?, ?, ?, ?)`, pr.Provider, pr.Repository, pr.Number, string(raw))
	return err
}

func readPublishedReview(ctx context.Context, attempt Attempt, id int64, login string, selfAuthored bool) (submittedReview, error) {
	pr := attempt.PullRequest
	var review submittedReview
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/reviews/%d", pr.Repository, pr.Number, id)
	if err := githubJSON(ctx, []string{endpoint}, nil, &review); err != nil {
		return review, err
	}
	verdict := map[string]string{"APPROVED": "APPROVE", "CHANGES_REQUESTED": "REQUEST_CHANGES", "COMMENTED": "COMMENT"}[review.State]
	receipt := Receipt{Provider: pr.Provider, Repository: pr.Repository, Number: pr.Number, HeadSHA: attempt.HeadSHA,
		SkillDigest: attempt.SkillDigest, Verdict: verdict, ReviewID: json.Number(strconv.FormatInt(id, 10)), ReviewURL: review.URL}
	if review.ID != id || review.Commit != attempt.HeadSHA || normalizeGitHubLogin(review.User.Login) != login ||
		!strings.Contains(review.Body, attempt.publicationMarker()) {
		return review, fmt.Errorf("submitted review does not match the pinned attempt and authenticated reviewer")
	}
	prURL, rawID, validURL := strings.Cut(review.URL, "#pullrequestreview-")
	if !validURL || !samePullRequestIdentity(prURL, pr) || rawID != strconv.FormatInt(id, 10) {
		return review, fmt.Errorf("submitted review URL does not match the pinned attempt")
	}
	if review.State == "DISMISSED" {
		return review, fail("review_dismissed", "published review %d was dismissed", id)
	}
	if err := ValidateReceipt(receipt, pr, attempt.HeadSHA, attempt.SkillDigest, selfAuthored); err != nil {
		return review, err
	}
	return review, nil
}

func resolvePublicationTarget(ctx context.Context, cfg Config, pr PullRequest, head string) (GitHubPullRequest, error) {
	current, err := resolveGitHub(ctx, pr)
	if err != nil {
		return current, err
	}
	if err := ValidateGitHubPullRequest(cfg, pr, current); err != nil {
		return current, err
	}
	if head != "" && current.HeadSHA != head {
		return current, fail("head_changed", "pull request head changed; reassess the current revision")
	}
	return current, nil
}

func validatePublicationSnapshot(initial, current []githubReviewThread) error {
	if len(initial) != len(current) {
		return fail("discussion_changed", "review discussions changed; reassess before publishing")
	}
	for _, before := range initial {
		found := false
		for _, after := range current {
			if before.ID == after.ID {
				found = before.IsResolved == after.IsResolved && len(before.Comments) == len(after.Comments) &&
					commentsPreserved(before.Comments, after.Comments)
				break
			}
		}
		if !found {
			return fail("discussion_changed", "review discussion %s changed; reassess before publishing", before.ID)
		}
	}
	return nil
}

func isGitHubValidationError(err error) bool {
	var apiError *githubAPIError
	return errors.As(err, &apiError) && (apiError.Status == 400 || apiError.Status == 422)
}

func deliverPublication(ctx context.Context, cfg Config, publication Publication, recordReviewID func(int64) error) (Attempt, error) {
	result := publication.Attempt
	pr := result.PullRequest
	if result.ObservedHeadSHA != "" && (result.RoundID == 0 ||
		result.ObservedHeadSHA == result.HeadSHA || publication.Draft.Verdict != "COMMENT") {
		return result, fmt.Errorf("stale publication requires a COMMENT and a distinct observed head")
	}
	current, err := publication.targetAfterStart(ctx, cfg)
	if err != nil {
		return result, err
	}
	login, err := resolveGitHubLogin(ctx)
	if err != nil {
		return result, err
	}
	if login != publication.Login {
		return result, fmt.Errorf("authenticated reviewer differs from the saved publication")
	}
	marker := result.publicationMarker()
	id, err := findMarkerReview(ctx, pr, marker)
	if err != nil {
		return result, err
	}
	result.Recovered = id != 0
	if publication.ExistingReviewID != 0 && id != publication.ExistingReviewID {
		return result, fmt.Errorf("saved review is missing or its marker changed")
	}
	if publication.PublishedReviewID != 0 && id != publication.PublishedReviewID {
		result.ReviewID = strconv.FormatInt(publication.PublishedReviewID, 10)
		if id == 0 && current.HeadSHA != result.HeadSHA {
			return result, fail("review_changed", "published review marker is missing; reassess in a new round")
		}
		return result, fmt.Errorf("published review does not match the saved review identity")
	}
	if id == 0 {
		if result.ObservedHeadSHA == "" && current.HeadSHA != result.HeadSHA {
			return result, fail("head_changed", "pull request head changed; reassess the current revision")
		}
		threads, err := listGitHubReviewThreads(ctx, pr)
		if err != nil {
			return result, err
		}
		if err := validatePublicationSnapshot(publication.Threads, threads); err != nil {
			return result, err
		}
		head := result.HeadSHA
		if result.ObservedHeadSHA != "" {
			head = ""
		}
		if _, err := resolvePublicationTarget(ctx, cfg, pr, head); err != nil {
			return result, err
		}
		var review submittedReview
		request := map[string]any{"commit_id": result.HeadSHA, "event": publication.Draft.Verdict,
			"body": publication.Draft.Body, "comments": publication.Draft.Comments}
		if err := githubJSON(ctx, []string{"--method", "POST", fmt.Sprintf("repos/%s/pulls/%d/reviews", pr.Repository, pr.Number)},
			request, &review); err != nil {
			if isGitHubValidationError(err) {
				found, lookupErr := findMarkerReview(ctx, pr, marker)
				if lookupErr == nil && found == 0 {
					return result, fail("draft_rejected", "GitHub rejected the prepared review; reassess before retrying: %v", err)
				}
			}
			return result, err
		}
		id = review.ID
		if id <= 0 {
			return result, fmt.Errorf("GitHub returned no published review ID")
		}
		if result.RoundID != 0 && recordReviewID != nil {
			result.ReviewID = strconv.FormatInt(id, 10)
			if err := recordReviewID(id); err != nil {
				return result, fail("state_failed", "record published review identity: %v", err)
			}
			recordReviewID = nil
		}
	}
	commentExpected := login == normalizeGitHubLogin(current.Author) || result.ObservedHeadSHA != ""
	review, err := readPublishedReview(ctx, result, id, login, commentExpected)
	dismissedAfterPush := false
	if err != nil {
		var coded *codedError
		if errors.As(err, &coded) && coded.code == "review_dismissed" {
			latest, lookupErr := resolvePublicationTarget(ctx, cfg, pr, "")
			if lookupErr == nil && latest.HeadSHA != result.HeadSHA && result.RoundID == 0 {
				result.ReviewID, result.ReviewURL = strconv.FormatInt(id, 10), review.URL
				return result, fail("head_changed", "a newer pull request head dismissed the published review")
			}
			dismissedAfterPush = lookupErr == nil && latest.HeadSHA != result.HeadSHA && !commentExpected
		}
		if !dismissedAfterPush {
			return result, err
		}
	}
	result.ReviewID, result.ReviewURL = strconv.FormatInt(id, 10), review.URL
	if result.RoundID != 0 && recordReviewID != nil && publication.PublishedReviewID == 0 {
		if err := recordReviewID(id); err != nil {
			return result, fail("state_failed", "record published review identity: %v", err)
		}
	}
	result.Verdict = map[string]string{"APPROVED": "APPROVE", "CHANGES_REQUESTED": "REQUEST_CHANGES", "COMMENTED": "COMMENT"}[review.State]
	if dismissedAfterPush {
		result.Verdict = publication.Draft.Verdict
	}
	current, err = publication.targetAfterStart(ctx, cfg)
	if err != nil {
		return result, err
	}
	if publication.ExistingReviewID != 0 && (publication.Draft.NewFindings || len(publication.Draft.Comments) != 0 ||
		result.Verdict != publication.Draft.Verdict) {
		return result, fail("review_conflict", "reassessment conflicts with published review %s; its draft is retained for operator assessment", review.URL)
	}
	if result.Verdict != publication.Draft.Verdict {
		return result, fmt.Errorf("published review event differs from the saved publication")
	}
	if publication.ExistingReviewID == 0 {
		if review.Body != publication.Draft.Body {
			if result.RoundID != 0 && current.HeadSHA != result.HeadSHA {
				return result, fail("review_changed", "published review body changed; reassess in a new round")
			}
			return result, fmt.Errorf("published review body differs from the saved publication")
		}
		if err := verifyPublishedComments(ctx, pr, id, publication.Draft.Comments, current.HeadSHA != result.HeadSHA); err != nil {
			return result, err
		}
	}
	result.DiscussionOutcomes, err = syncPublicationDiscussions(ctx, cfg, publication)
	if err != nil {
		return result, err
	}
	if _, err := publication.targetAfterStart(ctx, cfg); err != nil {
		return result, err
	}
	result.Success = true
	return result, nil
}

func verifyPublishedComments(ctx context.Context, pr PullRequest, id int64, expected []ReviewComment, preferOriginal bool) error {
	if len(expected) == 0 {
		return nil
	}
	var pages [][]struct {
		ReviewComment
		ReviewID          int64 `json:"pull_request_review_id"`
		OriginalLine      int   `json:"original_line"`
		OriginalStartLine int   `json:"original_start_line"`
	}
	// The review-scoped endpoint omits line and side coordinates.
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100", pr.Repository, pr.Number)
	if err := githubJSON(ctx, []string{endpoint, "--paginate", "--slurp"}, nil, &pages); err != nil {
		return err
	}
	remaining := append([]ReviewComment(nil), expected...)
	for _, page := range pages {
		for _, wire := range page {
			if wire.ReviewID != id {
				continue
			}
			comment := wire.ReviewComment
			if comment.Line == 0 || (preferOriginal && wire.OriginalLine != 0) {
				comment.Line = wire.OriginalLine
			}
			if comment.StartLine == 0 || (preferOriginal && wire.OriginalStartLine != 0) {
				comment.StartLine = wire.OriginalStartLine
			}
			found := false
			for i, wanted := range remaining {
				if comment == wanted {
					remaining = append(remaining[:i], remaining[i+1:]...)
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("published inline comment does not match the saved publication")
			}
		}
	}
	if len(remaining) != 0 {
		return fmt.Errorf("published review is missing inline comments")
	}
	return nil
}
