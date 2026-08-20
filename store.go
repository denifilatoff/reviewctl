package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Attempt struct {
	PullRequest
	HeadSHA      string    `json:"head_sha,omitempty"`
	SkillDigest  string    `json:"skill_digest,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Success      bool      `json:"success"`
	Verdict      string    `json:"verdict,omitempty"`
	ReviewID     string    `json:"review_id,omitempty"`
	ReviewURL    string    `json:"review_url,omitempty"`
	Recovered    bool      `json:"recovered,omitempty"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS queue (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			provider TEXT NOT NULL,
			repository TEXT NOT NULL,
			change_number INTEGER NOT NULL,
			url TEXT NOT NULL,
			queued_at TEXT NOT NULL,
			UNIQUE(provider, repository, change_number)
		)`,
		`CREATE TABLE IF NOT EXISTS history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			provider TEXT NOT NULL,
			repository TEXT NOT NULL,
			change_number INTEGER NOT NULL,
			url TEXT NOT NULL,
			head_sha TEXT NOT NULL,
			skill_digest TEXT NOT NULL,
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL,
			success INTEGER NOT NULL,
			verdict TEXT NOT NULL,
			review_id TEXT NOT NULL,
			review_url TEXT NOT NULL,
			error_code TEXT NOT NULL,
			error_message TEXT NOT NULL
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize state: %w", err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Enqueue(ctx context.Context, pr PullRequest) (bool, error) {
	added, err := s.EnqueueMany(ctx, []PullRequest{pr})
	if err != nil {
		return false, err
	}
	return added[0], nil
}

func (s *Store) EnqueueMany(ctx context.Context, pullRequests []PullRequest) ([]bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("start enqueue transaction: %w", err)
	}
	defer tx.Rollback()
	added := make([]bool, len(pullRequests))
	queuedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for i, pr := range pullRequests {
		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO queue(provider, repository, change_number, url, queued_at)
			VALUES (?, ?, ?, ?, ?)`, pr.Provider, pr.Repository, pr.Number, pr.URL, queuedAt)
		if err != nil {
			return nil, fmt.Errorf("enqueue pull request: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("read enqueue result: %w", err)
		}
		added[i] = rows == 1
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit enqueue transaction: %w", err)
	}
	return added, nil
}

func (s *Store) Snapshot(ctx context.Context) ([]PullRequest, error) {
	queue, _, err := s.Status(ctx, 0)
	return queue, err
}

func (s *Store) Status(ctx context.Context, historyLimit int) ([]PullRequest, []Attempt, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("start status transaction: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT provider, repository, change_number, url FROM queue ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("read queue: %w", err)
	}
	queue := make([]PullRequest, 0)
	for rows.Next() {
		var pr PullRequest
		if err := rows.Scan(&pr.Provider, &pr.Repository, &pr.Number, &pr.URL); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("read queue entry: %w", err)
		}
		queue = append(queue, pr)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("read queue: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("read queue: %w", err)
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT provider, repository, change_number, url, head_sha, skill_digest, started_at, finished_at,
			success, verdict, review_id, review_url, error_code, error_message FROM history ORDER BY id DESC LIMIT ?`,
		historyLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("read history: %w", err)
	}
	history := make([]Attempt, 0)
	for rows.Next() {
		var attempt Attempt
		var started, finished string
		if err := rows.Scan(&attempt.Provider, &attempt.Repository, &attempt.Number, &attempt.URL, &attempt.HeadSHA,
			&attempt.SkillDigest, &started, &finished, &attempt.Success, &attempt.Verdict, &attempt.ReviewID,
			&attempt.ReviewURL, &attempt.ErrorCode, &attempt.ErrorMessage); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("read history entry: %w", err)
		}
		attempt.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		attempt.FinishedAt, _ = time.Parse(time.RFC3339Nano, finished)
		history = append(history, attempt)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("read history: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("read history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit status transaction: %w", err)
	}
	return queue, history, nil
}

func (s *Store) Finish(ctx context.Context, attempt Attempt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start attempt transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO history(provider, repository, change_number, url, head_sha, skill_digest,
			started_at, finished_at, success, verdict, review_id, review_url, error_code, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.Provider, attempt.Repository, attempt.Number, attempt.URL, attempt.HeadSHA, attempt.SkillDigest,
		attempt.StartedAt.UTC().Format(time.RFC3339Nano), attempt.FinishedAt.UTC().Format(time.RFC3339Nano),
		attempt.Success, attempt.Verdict, attempt.ReviewID, attempt.ReviewURL, attempt.ErrorCode,
		bounded(attempt.ErrorMessage, 512)); err != nil {
		return fmt.Errorf("record attempt: %w", err)
	}
	if attempt.Success {
		if _, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE provider = ? AND repository = ? AND change_number = ?`,
			attempt.Provider, attempt.Repository, attempt.Number); err != nil {
			return fmt.Errorf("remove queue entry: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit attempt: %w", err)
	}
	return nil
}

func (s *Store) History(ctx context.Context) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, repository, change_number, url, head_sha, skill_digest, started_at, finished_at,
			success, verdict, review_id, review_url, error_code, error_message FROM history ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Attempt
	for rows.Next() {
		var attempt Attempt
		var started, finished string
		if err := rows.Scan(&attempt.Provider, &attempt.Repository, &attempt.Number, &attempt.URL, &attempt.HeadSHA,
			&attempt.SkillDigest, &started, &finished, &attempt.Success, &attempt.Verdict, &attempt.ReviewID,
			&attempt.ReviewURL, &attempt.ErrorCode, &attempt.ErrorMessage); err != nil {
			return nil, err
		}
		attempt.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		attempt.FinishedAt, _ = time.Parse(time.RFC3339Nano, finished)
		result = append(result, attempt)
	}
	return result, rows.Err()
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
