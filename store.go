package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Attempt struct {
	PullRequest
	Cost         *ReviewCost `json:"cost,omitempty"`
	HeadSHA      string      `json:"head_sha,omitempty"`
	SkillDigest  string      `json:"skill_digest,omitempty"`
	StartedAt    time.Time   `json:"started_at"`
	FinishedAt   time.Time   `json:"finished_at"`
	Success      bool        `json:"success"`
	Verdict      string      `json:"verdict,omitempty"`
	ReviewID     string      `json:"review_id,omitempty"`
	ReviewURL    string      `json:"review_url,omitempty"`
	Recovered    bool        `json:"recovered,omitempty"`
	ErrorCode    string      `json:"error_code,omitempty"`
	ErrorMessage string      `json:"error_message,omitempty"`
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
		`CREATE TABLE IF NOT EXISTS repository_baselines (
			provider TEXT NOT NULL,
			repository TEXT NOT NULL,
			PRIMARY KEY(provider, repository)
		)`,
		`CREATE TABLE IF NOT EXISTS review_signatures (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			review_id TEXT NOT NULL UNIQUE,
			attempt TEXT NOT NULL,
			error_message TEXT NOT NULL DEFAULT '',
			attempted_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS pull_request_baselines (
			provider TEXT NOT NULL,
			repository TEXT NOT NULL,
			change_number INTEGER NOT NULL,
			is_draft INTEGER NOT NULL,
			head_sha TEXT NOT NULL,
			PRIMARY KEY(provider, repository, change_number)
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize state: %w", err)
		}
	}
	for _, migration := range []struct{ table, column, definition string }{
		{"history", "cost_json", "TEXT NOT NULL DEFAULT ''"},
		{"history", "input_revision", "TEXT NOT NULL DEFAULT ''"},
		{"review_signatures", "attempted_at", "INTEGER NOT NULL DEFAULT 0"},
		{"queue", "input_revision", "TEXT NOT NULL DEFAULT ''"},
		{"pull_request_baselines", "description_digest", "TEXT NOT NULL DEFAULT ''"},
		{"pull_request_baselines", "message_ids_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"pull_request_baselines", "observed_message_ids_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"pull_request_baselines", "input_revision", "TEXT NOT NULL DEFAULT ''"},
	} {
		var exists int
		query := fmt.Sprintf("SELECT count(*) FROM pragma_table_info('%s') WHERE name = ?", migration.table)
		if err := db.QueryRow(query, migration.column).Scan(&exists); err != nil {
			db.Close()
			return nil, fmt.Errorf("inspect %s schema: %w", migration.table, err)
		}
		if exists != 0 {
			continue
		}
		statement := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s",
			migration.table, migration.column, migration.definition)
		if _, err := db.Exec(statement); err != nil {
			// Concurrent readers may have applied the same additive migration.
			if checkErr := db.QueryRow(query, migration.column).Scan(&exists); checkErr != nil || exists == 0 {
				db.Close()
				return nil, fmt.Errorf("migrate %s: %w", migration.table, err)
			}
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) ApplyDiscoverySnapshot(
	ctx context.Context, repository Repository, snapshot []pullRequestSnapshot,
) (int, error) {
	seen := make(map[int64]bool, len(snapshot))
	for _, current := range snapshot {
		if current.Provider != repository.Provider || current.Repository != repository.Repository ||
			current.Number < 1 || current.HeadSHA == "" || !samePullRequestIdentity(current.URL, current.PullRequest) ||
			seen[current.Number] {
			return 0, fmt.Errorf("invalid discovery snapshot for %s", repository.Repository)
		}
		seen[current.Number] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("start discovery transaction: %w", err)
	}
	defer tx.Rollback()
	var initialized bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM repository_baselines WHERE provider = ? AND repository = ?)`,
		repository.Provider, repository.Repository).Scan(&initialized); err != nil {
		return 0, fmt.Errorf("read repository baseline: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT change_number, is_draft, head_sha, description_digest, message_ids_json,
			observed_message_ids_json, input_revision
		FROM pull_request_baselines
		WHERE provider = ? AND repository = ?`, repository.Provider, repository.Repository)
	if err != nil {
		return 0, fmt.Errorf("read pull request baseline: %w", err)
	}
	previous := make(map[int64]pullRequestSnapshot)
	for rows.Next() {
		var item pullRequestSnapshot
		var messageIDs, observedMessageIDs string
		item.Provider, item.Repository = repository.Provider, repository.Repository
		if err := rows.Scan(&item.Number, &item.IsDraft, &item.HeadSHA, &item.DescriptionDigest, &messageIDs,
			&observedMessageIDs, &item.InputRevision); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read pull request baseline: %w", err)
		}
		if err := json.Unmarshal([]byte(messageIDs), &item.MessageIDs); err != nil {
			rows.Close()
			return 0, fmt.Errorf("decode pull request baseline: %w", err)
		}
		if err := json.Unmarshal([]byte(observedMessageIDs), &item.ObservedMessageIDs); err != nil {
			rows.Close()
			return 0, fmt.Errorf("decode observed pull request baseline: %w", err)
		}
		previous[item.Number] = item
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read pull request baseline: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("read pull request baseline: %w", err)
	}
	for i := range snapshot {
		current := &snapshot[i]
		if current.IsDraft || current.DescriptionDigest == "" {
			if old, found := previous[current.Number]; found {
				current.DescriptionDigest = old.DescriptionDigest
				current.MessageIDs = old.MessageIDs
				current.ObservedMessageIDs = old.ObservedMessageIDs
				current.InputRevision = old.InputRevision
			}
			continue
		}
		var oldIDs, oldObservedIDs []string
		if old, found := previous[current.Number]; found {
			oldIDs, oldObservedIDs = old.MessageIDs, old.ObservedMessageIDs
		}
		current.MessageIDs, current.ObservedMessageIDs = mergeExternalMessageIDs(
			oldIDs, oldObservedIDs, current.MessageIDs, current.ObservedMessageIDs)
		current.InputRevision = externalInputRevision(current.DescriptionDigest, current.MessageIDs)
	}
	queuedAt := time.Now().UTC().Format(time.RFC3339Nano)
	enqueued := 0
	for _, current := range snapshot {
		old, found := previous[current.Number]
		var oldSnapshot *pullRequestSnapshot
		if found {
			oldSnapshot = &old
		}
		if !shouldEnqueueDiscovery(initialized, oldSnapshot, current) {
			continue
		}
		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO queue(provider, repository, change_number, url, queued_at, input_revision)
			VALUES (?, ?, ?, ?, ?, ?)`, current.Provider, current.Repository, current.Number, current.URL, queuedAt,
			current.InputRevision)
		if err != nil {
			return 0, fmt.Errorf("enqueue discovered pull request: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("read discovery enqueue result: %w", err)
		}
		enqueued += int(rows)
		if _, err := tx.ExecContext(ctx, `
			UPDATE queue SET input_revision = ?
			WHERE provider = ? AND repository = ? AND change_number = ?`, current.InputRevision,
			current.Provider, current.Repository, current.Number); err != nil {
			return 0, fmt.Errorf("update discovered pull request: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pull_request_baselines WHERE provider = ? AND repository = ?`,
		repository.Provider, repository.Repository); err != nil {
		return 0, fmt.Errorf("replace pull request baseline: %w", err)
	}
	for _, current := range snapshot {
		messageIDs, err := json.Marshal(current.MessageIDs)
		if err != nil {
			return 0, fmt.Errorf("encode pull request baseline: %w", err)
		}
		observedMessageIDs, err := json.Marshal(current.ObservedMessageIDs)
		if err != nil {
			return 0, fmt.Errorf("encode observed pull request baseline: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pull_request_baselines(provider, repository, change_number, is_draft, head_sha,
				description_digest, message_ids_json, observed_message_ids_json, input_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, current.Provider, current.Repository, current.Number, current.IsDraft,
			current.HeadSHA, current.DescriptionDigest, string(messageIDs), string(observedMessageIDs),
			current.InputRevision); err != nil {
			return 0, fmt.Errorf("write pull request baseline: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO repository_baselines(provider, repository) VALUES (?, ?)`,
		repository.Provider, repository.Repository); err != nil {
		return 0, fmt.Errorf("write repository baseline: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit discovery transaction: %w", err)
	}
	return enqueued, nil
}

func (s *Store) MergeQueuedInput(
	ctx context.Context, pr PullRequest, current githubExternalInput,
) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("start input transaction: %w", err)
	}
	defer tx.Rollback()
	var previousJSON, previousObservedJSON string
	if err := tx.QueryRowContext(ctx, `
		SELECT message_ids_json, observed_message_ids_json FROM pull_request_baselines
		WHERE provider = ? AND repository = ? AND change_number = ?`,
		pr.Provider, pr.Repository, pr.Number).Scan(&previousJSON, &previousObservedJSON); err != nil {
		return "", fmt.Errorf("read pull request input: %w", err)
	}
	var previousIDs []string
	if err := json.Unmarshal([]byte(previousJSON), &previousIDs); err != nil {
		return "", fmt.Errorf("decode pull request input: %w", err)
	}
	var previousObservedIDs []string
	if err := json.Unmarshal([]byte(previousObservedJSON), &previousObservedIDs); err != nil {
		return "", fmt.Errorf("decode observed pull request input: %w", err)
	}
	descriptionDigest, messageIDs, observedMessageIDs, revision := mergeExternalInputState(
		previousIDs, previousObservedIDs, current)
	messageIDsJSON, err := json.Marshal(messageIDs)
	if err != nil {
		return "", fmt.Errorf("encode pull request input: %w", err)
	}
	observedMessageIDsJSON, err := json.Marshal(observedMessageIDs)
	if err != nil {
		return "", fmt.Errorf("encode observed pull request input: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE pull_request_baselines
		SET description_digest = ?, message_ids_json = ?, observed_message_ids_json = ?, input_revision = ?
		WHERE provider = ? AND repository = ? AND change_number = ?`,
		descriptionDigest, string(messageIDsJSON), string(observedMessageIDsJSON), revision,
		pr.Provider, pr.Repository, pr.Number); err != nil {
		return "", fmt.Errorf("update pull request input: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE queue SET input_revision = ?
		WHERE provider = ? AND repository = ? AND change_number = ?`,
		revision, pr.Provider, pr.Repository, pr.Number)
	if err != nil {
		return "", fmt.Errorf("update queued input: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("read queued input update: %w", err)
	}
	if updated != 1 {
		return "", fmt.Errorf("queued pull request input is missing")
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit input transaction: %w", err)
	}
	return revision, nil
}

func (s *Store) Close() error { return s.db.Close() }

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
			INSERT OR IGNORE INTO queue(provider, repository, change_number, url, queued_at, input_revision)
			VALUES (?, ?, ?, ?, ?, ?)`, pr.Provider, pr.Repository, pr.Number, pr.URL, queuedAt, pr.InputRevision)
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
		SELECT provider, repository, change_number, url, input_revision FROM queue ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("read queue: %w", err)
	}
	queue := make([]PullRequest, 0)
	for rows.Next() {
		var pr PullRequest
		if err := rows.Scan(&pr.Provider, &pr.Repository, &pr.Number, &pr.URL, &pr.InputRevision); err != nil {
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
		SELECT provider, repository, change_number, url, head_sha, input_revision, skill_digest, started_at, finished_at,
			success, verdict, review_id, review_url, error_code, error_message, cost_json FROM history ORDER BY id DESC LIMIT ?`,
		historyLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("read history: %w", err)
	}
	history := make([]Attempt, 0)
	for rows.Next() {
		var attempt Attempt
		var started, finished, cost string
		if err := rows.Scan(&attempt.Provider, &attempt.Repository, &attempt.Number, &attempt.URL, &attempt.HeadSHA,
			&attempt.InputRevision, &attempt.SkillDigest, &started, &finished, &attempt.Success, &attempt.Verdict, &attempt.ReviewID,
			&attempt.ReviewURL, &attempt.ErrorCode, &attempt.ErrorMessage, &cost); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("read history entry: %w", err)
		}
		attempt.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		if cost != "" {
			if err := json.Unmarshal([]byte(cost), &attempt.Cost); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("decode accounting: %w", err)
			}
		}
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
	cost, err := encodeCost(attempt.Cost)
	if err != nil {
		return fmt.Errorf("encode accounting: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start attempt transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO history(provider, repository, change_number, url, head_sha, input_revision, skill_digest,
			started_at, finished_at, success, verdict, review_id, review_url, error_code, error_message, cost_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.Provider, attempt.Repository, attempt.Number, attempt.URL, attempt.HeadSHA, attempt.InputRevision,
		attempt.SkillDigest,
		attempt.StartedAt.UTC().Format(time.RFC3339Nano), attempt.FinishedAt.UTC().Format(time.RFC3339Nano),
		attempt.Success, attempt.Verdict, attempt.ReviewID, attempt.ReviewURL, attempt.ErrorCode,
		bounded(attempt.ErrorMessage, 512), cost); err != nil {
		return fmt.Errorf("record attempt: %w", err)
	}
	removeFromQueue := attempt.Success
	switch attempt.ErrorCode {
	case "untrusted_author", "pr_not_open", "pr_draft":
		removeFromQueue = true
	}
	if attempt.Success {
		if attempt.Cost != nil && !attempt.Recovered {
			payload, err := json.Marshal(attempt)
			if err != nil {
				return fmt.Errorf("encode signature: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO review_signatures(review_id, attempt) VALUES (?, ?)
				ON CONFLICT(review_id) DO NOTHING`, attempt.ReviewID, string(payload)); err != nil {
				return fmt.Errorf("queue signature: %w", err)
			}
		}
	}
	if removeFromQueue {
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

func (s *Store) queueSignature(attempt Attempt) error {
	if attempt.Cost == nil || attempt.Recovered {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payload, err := json.Marshal(attempt)
	if err != nil {
		return fmt.Errorf("encode signature: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO review_signatures(review_id, attempt) VALUES (?, ?)
		ON CONFLICT(review_id) DO NOTHING`, attempt.ReviewID, string(payload)); err != nil {
		return fmt.Errorf("queue signature: %w", err)
	}
	return nil
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
