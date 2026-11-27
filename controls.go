package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

type reviewControl struct {
	Paused       bool
	Manual       bool
	Requested    bool
	RoundID      int64
	RoundKey     string
	LastRoundID  int64
	PermitPaused bool
	RetiredPass  int
	PolledAt     time.Time
}

type reviewInput struct {
	Source  string
	ID      int64
	Created time.Time
	Action  string
}

func pullRequestKey(pr PullRequest) []any {
	return []any{pr.Provider, pr.Repository, pr.Number}
}

type reviewControlReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readReviewControl(ctx context.Context, reader reviewControlReader, pr PullRequest) (reviewControl, bool, error) {
	var state reviewControl
	var raw string
	err := reader.QueryRowContext(ctx, `SELECT payload FROM review_controls
		WHERE provider = ? AND repository = ? AND change_number = ?`, pullRequestKey(pr)...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return state, false, nil
	}
	if err != nil {
		return state, false, err
	}
	err = json.Unmarshal([]byte(raw), &state)
	return state, true, err
}

func saveReviewControl(ctx context.Context, tx *sql.Tx, pr PullRequest, state reviewControl) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_controls(provider, repository, change_number, payload)
		VALUES (?, ?, ?, ?) ON CONFLICT(provider, repository, change_number)
		DO UPDATE SET payload = excluded.payload`, append(pullRequestKey(pr), string(raw))...)
	return err
}

func (s *Store) reviewAllowed(ctx context.Context, pr PullRequest) (bool, error) {
	state, _, err := readReviewControl(ctx, s.db, pr)
	return !state.Paused || state.Manual || state.PermitPaused, err
}

func (s *Store) startReviewRound(ctx context.Context, pr PullRequest) (reviewControl, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return reviewControl{}, err
	}
	defer tx.Rollback()
	state, _, err := readReviewControl(ctx, tx, pr)
	if err != nil {
		return reviewControl{}, err
	}
	if state.Paused && !state.Manual && !state.PermitPaused {
		return reviewControl{}, nil
	}
	if state.RoundID == 0 {
		state.LastRoundID++
		state.RoundID = state.LastRoundID
		state.RoundKey = rand.Text()
	}
	state.PermitPaused = state.PermitPaused || (state.Paused && state.Manual)
	state.Manual, state.Requested = false, false
	if err := saveReviewControl(ctx, tx, pr, state); err != nil {
		return reviewControl{}, err
	}
	return state, tx.Commit()
}

func reviewCommandAction(body string) string {
	switch strings.TrimSpace(body) {
	case "/reviewctl pause":
		return "pause"
	case "/reviewctl resume":
		return "resume"
	case "/reviewctl review":
		return "review"
	default:
		return ""
	}
}

func reviewMarkerPrefix(pr PullRequest) string {
	return fmt.Sprintf("<!-- reviewctl:%s:%s#%d:", pr.Provider, pr.Repository, pr.Number)
}

type reviewInputComment struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
	IssueURL    string    `json:"issue_url"`
	PullURL     string    `json:"pull_request_url"`
	InReplyToID int64     `json:"in_reply_to_id"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

func readReviewInputComments(ctx context.Context, endpoint string, since time.Time) ([]reviewInputComment, error) {
	endpoint += "?per_page=100&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	var pages [][]reviewInputComment
	if err := githubJSON(ctx, []string{endpoint, "--paginate", "--slurp"}, nil, &pages); err != nil {
		return nil, fail("github_failed", "read review inputs at %s: %v", endpoint, err)
	}
	var comments []reviewInputComment
	for _, page := range pages {
		comments = append(comments, page...)
	}
	return comments, nil
}

func newConversationInputs(cfg Config, comments []reviewInputComment, baseline time.Time) []reviewInput {
	var inputs []reviewInput
	for _, comment := range comments {
		if !comment.CreatedAt.After(baseline) {
			continue
		}
		if action := reviewCommandAction(comment.Body); action != "" {
			if !isTrustedGitHubAuthor(cfg, comment.User.Login) {
				action = ""
			}
			inputs = append(inputs, reviewInput{"conversation", comment.ID, comment.CreatedAt, action})
		}
	}
	return inputs
}

func newDiscussionInputs(cfg Config, pr PullRequest, author, login string,
	threads []githubReviewThread, baseline time.Time,
) []reviewInput {
	var inputs []reviewInput
	author = normalizeGitHubLogin(author)
	for _, thread := range threads {
		if len(thread.Comments) == 0 {
			continue
		}
		root := thread.Comments[0]
		controllerOwned := normalizeGitHubLogin(root.Author) == login &&
			strings.Contains(root.ReviewBody, reviewMarkerPrefix(pr))
		for _, comment := range thread.Comments {
			if !comment.CreatedAt.After(baseline) {
				continue
			}
			action := reviewCommandAction(comment.Body)
			switch {
			case action != "":
				if !isTrustedGitHubAuthor(cfg, comment.Author) {
					action = ""
				}
			case controllerOwned && comment.ReplyToID != 0 && normalizeGitHubLogin(comment.Author) == author:
				body := strings.TrimSpace(comment.Body)
				if !thread.IsResolved && body != "" && !strings.Contains(body, "<!-- reviewctl:discussion:") &&
					!strings.HasPrefix(body, "/reviewctl ") {
					action = "feedback"
				}
			default:
				continue
			}
			inputs = append(inputs, reviewInput{"discussion", comment.DatabaseID, comment.CreatedAt, action})
		}
	}
	return inputs
}

type reviewInputCursor struct {
	Baseline time.Time
	Latest   time.Time
}

func (s *Store) readReviewInputCursor(ctx context.Context, repo Repository) (reviewInputCursor, error) {
	var cursor reviewInputCursor
	err := s.db.QueryRowContext(ctx, `SELECT baseline, latest FROM review_input_cursors
		WHERE provider = ? AND repository = ?`, repo.Provider, repo.Repository).Scan(&cursor.Baseline, &cursor.Latest)
	if errors.Is(err, sql.ErrNoRows) {
		return cursor, nil
	}
	return cursor, err
}

func (s *Store) saveReviewInputCursor(ctx context.Context, repo Repository, cursor reviewInputCursor) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO review_input_cursors(provider, repository, baseline, latest)
		VALUES (?, ?, ?, ?) ON CONFLICT(provider, repository) DO UPDATE SET latest = excluded.latest`,
		repo.Provider, repo.Repository, cursor.Baseline, cursor.Latest)
	return err
}

func (s *Store) pollReviewInput(ctx context.Context, cfg Config, pr PullRequest, author, login string,
	threads []githubReviewThread,
) (int, error) {
	now := time.Now().UTC()
	repo := Repository{Provider: pr.Provider, Repository: pr.Repository}
	cursor, err := s.readReviewInputCursor(ctx, repo)
	if err != nil {
		return 0, err
	}
	if cursor.Baseline.IsZero() {
		cursor = reviewInputCursor{Baseline: now, Latest: now}
		if err := s.saveReviewInputCursor(ctx, repo, cursor); err != nil {
			return 0, err
		}
	}
	state, _, err := readReviewControl(ctx, s.db, pr)
	if err != nil {
		return 0, err
	}
	since := cursor.Latest
	if state.PolledAt.After(since) {
		since = state.PolledAt
	}
	endpoint := fmt.Sprintf("repos/%s/issues/%d/comments", pr.Repository, pr.Number)
	comments, err := readReviewInputComments(ctx, endpoint, reviewInputSince(cursor.Baseline, since))
	if err != nil {
		return 0, err
	}
	inputs := newConversationInputs(cfg, comments, cursor.Baseline)
	inputs = append(inputs, newDiscussionInputs(cfg, pr, author, login, threads, cursor.Baseline)...)
	return s.applyReviewInputs(ctx, pr, inputs, now)
}

func reviewInputSince(baseline, latest time.Time) time.Time {
	overlap := latest.Add(-time.Minute)
	if overlap.Before(baseline) {
		return baseline
	}
	return overlap
}

func (s *Store) pollRepositoryReviewInputs(ctx context.Context, cfg Config, repo Repository,
	prs []pullRequestSnapshot,
) (int, error) {
	now := time.Now().UTC()
	cursor, err := s.readReviewInputCursor(ctx, repo)
	if err != nil {
		return 0, err
	}
	if cursor.Baseline.IsZero() {
		return 0, s.saveReviewInputCursor(ctx, repo, reviewInputCursor{Baseline: now, Latest: now})
	}
	since := reviewInputSince(cursor.Baseline, cursor.Latest)
	conversations, err := readReviewInputComments(ctx, "repos/"+repo.Repository+"/issues/comments", since)
	if err != nil {
		return 0, err
	}
	reviewComments, err := readReviewInputComments(ctx, "repos/"+repo.Repository+"/pulls/comments", since)
	if err != nil {
		return 0, err
	}

	byIssue := make(map[string][]reviewInputComment)
	for _, comment := range conversations {
		if reviewCommandAction(comment.Body) != "" {
			byIssue[strings.ToLower(comment.IssueURL)] = append(byIssue[strings.ToLower(comment.IssueURL)], comment)
		}
	}
	byPull := make(map[string][]reviewInputComment)
	for _, comment := range reviewComments {
		byPull[strings.ToLower(comment.PullURL)] = append(byPull[strings.ToLower(comment.PullURL)], comment)
	}
	var login string
	var failures []error
	enqueued := 0
	for _, pr := range prs {
		if pr.IsDraft {
			continue
		}
		prefix := "https://api.github.com/repos/" + repo.Repository
		issueURL := fmt.Sprintf("%s/issues/%d", prefix, pr.Number)
		pullURL := fmt.Sprintf("%s/pulls/%d", prefix, pr.Number)
		inputs := newConversationInputs(cfg, byIssue[issueURL], cursor.Baseline)
		if hasCandidateDiscussionInput(cfg, pr.Author, byPull[pullURL], cursor.Baseline) {
			if login == "" {
				login, err = resolveGitHubLogin(ctx)
				if err != nil {
					failures = append(failures, err)
					continue
				}
			}
			threads, err := listGitHubReviewThreads(ctx, pr.PullRequest)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			inputs = append(inputs, newDiscussionInputs(cfg, pr.PullRequest, pr.Author, login, threads, cursor.Baseline)...)
		}
		if len(inputs) == 0 {
			continue
		}
		added, err := s.applyReviewInputs(ctx, pr.PullRequest, inputs, time.Time{})
		enqueued += added
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return enqueued, errors.Join(failures...)
	}
	cursor.Latest = now
	return enqueued, s.saveReviewInputCursor(ctx, repo, cursor)
}

func hasCandidateDiscussionInput(cfg Config, author string, comments []reviewInputComment, baseline time.Time) bool {
	for _, comment := range comments {
		if !comment.CreatedAt.After(baseline) {
			continue
		}
		if reviewCommandAction(comment.Body) != "" ||
			(comment.InReplyToID != 0 && normalizeGitHubLogin(comment.User.Login) == normalizeGitHubLogin(author)) {
			return true
		}
	}
	return false
}

func (s *Store) applyReviewInputs(ctx context.Context, pr PullRequest, inputs []reviewInput, polledAt time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	state, _, err := readReviewControl(ctx, tx, pr)
	if err != nil {
		return 0, err
	}
	if !polledAt.IsZero() {
		state.PolledAt = polledAt
	}
	sort.Slice(inputs, func(i, j int) bool {
		left, right := inputs[i], inputs[j]
		if !left.Created.Equal(right.Created) {
			return left.Created.Before(right.Created)
		}
		if left.Source != right.Source {
			return left.Source < right.Source
		}
		return left.ID < right.ID
	})
	for _, input := range inputs {
		if input.ID <= 0 {
			return 0, fmt.Errorf("GitHub returned a review input without a comment ID")
		}
		inserted, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO review_inputs
			(provider, repository, change_number, source, comment_id) VALUES (?, ?, ?, ?, ?)`,
			append(pullRequestKey(pr), input.Source, input.ID)...)
		if err != nil {
			return 0, err
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return 0, err
		}
		if count == 0 {
			continue
		}
		switch input.Action {
		case "pause":
			state.Paused, state.PermitPaused = true, false
			state.Requested = state.Requested || state.Manual
			state.Manual = false
		case "resume":
			state.Paused, state.PermitPaused = false, false
		case "review":
			state.Manual = true
		case "feedback":
			state.Requested = true
		}
	}
	if err := saveReviewControl(ctx, tx, pr, state); err != nil {
		return 0, err
	}
	enqueued := int64(0)
	if state.Manual || state.Requested {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO queue(provider, repository, change_number, url, queued_at)
			VALUES (?, ?, ?, ?, ?)`, append(pullRequestKey(pr), pr.URL, time.Now().UTC().Format(time.RFC3339Nano))...)
		if err != nil {
			return 0, err
		}
		enqueued, err = result.RowsAffected()
		if err != nil {
			return 0, err
		}
	}
	return int(enqueued), tx.Commit()
}

func (s *Store) permitHeldPublication(ctx context.Context, pr PullRequest, roundID int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	state, _, err := readReviewControl(ctx, tx, pr)
	if err != nil {
		return false, err
	}
	if state.Paused && state.Manual && !state.PermitPaused && state.RoundID == roundID {
		state.Manual, state.PermitPaused = false, true
		if err := saveReviewControl(ctx, tx, pr, state); err != nil {
			return false, err
		}
	}
	return !state.Paused || state.PermitPaused, tx.Commit()
}
