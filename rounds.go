package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *Store) reviewDirectory(pr PullRequest) string {
	return filepath.Join(filepath.Dir(s.path), "reviews", pr.Provider, pr.Repository, strconv.FormatInt(pr.Number, 10))
}

func (s *Store) roundDirectory(attempt Attempt) string {
	return filepath.Join(s.reviewDirectory(attempt.PullRequest), "rounds", fmt.Sprintf("%012d", attempt.RoundID))
}

func (s *Store) passDirectory(attempt Attempt) string {
	return filepath.Join(s.roundDirectory(attempt), "passes", fmt.Sprintf("%06d", attempt.Pass))
}

func (s *Store) latestRoundPass(attempt Attempt) (*Publication, error) {
	passes := filepath.Join(s.roundDirectory(attempt), "passes")
	entries, err := os.ReadDir(passes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		number, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() || number <= 0 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(passes, entry.Name(), "pass.json"))
		if err != nil {
			return nil, err
		}
		var pass Publication
		if err := json.Unmarshal(raw, &pass); err != nil {
			return nil, err
		}
		if pass.Attempt.PullRequest != attempt.PullRequest || pass.Attempt.RoundID != attempt.RoundID ||
			pass.Attempt.Pass != number || pass.Attempt.HeadSHA == "" ||
			(attempt.RoundKey != "" && pass.Attempt.RoundKey != attempt.RoundKey) {
			return nil, fmt.Errorf("saved pass does not match its pull request, round, and pass")
		}
		return &pass, nil
	}
	return nil, nil
}

func (s *Store) previousReviewPass(attempt Attempt) (*Publication, error) {
	rounds := filepath.Join(s.reviewDirectory(attempt.PullRequest), "rounds")
	entries, err := os.ReadDir(rounds)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		number, err := strconv.ParseInt(entries[i].Name(), 10, 64)
		if err != nil || !entries[i].IsDir() || number >= attempt.RoundID || number <= 0 {
			continue
		}
		previous := attempt
		previous.RoundID, previous.RoundKey = number, ""
		pass, err := s.latestRoundPass(previous)
		if err != nil || pass != nil {
			return pass, err
		}
	}
	return nil, nil
}

func (s *Store) saveRoundPass(publication Publication, artifacts string) error {
	path := s.passDirectory(publication.Attempt)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".pass-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := copyReviewArtifacts(artifacts, stage); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(publication, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "pass.json"), raw, 0o600); err != nil {
		return err
	}
	return os.Rename(stage, path)
}

func copyReviewArtifacts(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("review artifacts path is not a directory")
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	report, err := root.Lstat("review.json")
	if err != nil {
		return fmt.Errorf("read review.json: %w", err)
	}
	if !report.Mode().IsRegular() {
		return fmt.Errorf("review.json is not a regular file")
	}
	const maxArtifactBytes = 32 << 20
	remaining := int64(maxArtifactBytes)
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == source {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		first, _, _ := strings.Cut(relative, string(filepath.Separator))
		if relative != "review.json" && first != "evidence" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("review artifact %q is not a regular file", relative)
		}
		file, err := root.Open(relative)
		if err != nil {
			return err
		}
		defer file.Close()
		if relative == "review.json" {
			data, err := io.ReadAll(io.LimitReader(file, remaining+1))
			if err != nil {
				return err
			}
			remaining -= int64(len(data))
			if remaining < 0 {
				return fmt.Errorf("review artifacts exceed %d bytes", maxArtifactBytes)
			}
			var report map[string]json.RawMessage
			if err := json.Unmarshal(data, &report); err != nil || report == nil {
				return fmt.Errorf("review.json must contain a JSON object")
			}
			return os.WriteFile(target, data, 0o600)
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		size, copyErr := io.Copy(output, io.LimitReader(file, remaining+1))
		remaining -= size
		if remaining < 0 {
			copyErr = fmt.Errorf("review artifacts exceed %d bytes", maxArtifactBytes)
		}
		return errors.Join(copyErr, output.Close())
	})
}

func (s *Store) savePassOutcome(attempt Attempt) error {
	if attempt.RoundID == 0 || attempt.Pass == 0 || attempt.Held {
		return nil
	}
	path := s.passDirectory(attempt)
	if _, err := os.Stat(filepath.Join(path, "pass.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	dir := filepath.Join(path, "outcomes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".outcome-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := errors.Join(json.NewEncoder(file).Encode(attempt), file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(dir, strconv.FormatInt(attempt.FinishedAt.UnixNano(), 10)+".json"))
}

func (s *Store) deliveryAttempt(ctx context.Context, pass Publication, resumedAt time.Time) (Attempt, error) {
	result := pass.Attempt
	result.StartedAt, result.Cost = resumedAt, nil
	var recorded bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM history
		WHERE provider = ? AND repository = ? AND change_number = ? AND started_at = ? AND cost_json != '')`,
		append(pullRequestKey(result.PullRequest), pass.Attempt.StartedAt.UTC().Format(time.RFC3339Nano))...).Scan(&recorded); err != nil {
		return result, err
	}
	if !recorded {
		result.StartedAt, result.Cost = pass.Attempt.StartedAt, pass.Attempt.Cost
	}
	return result, nil
}

func (s *Store) recordCatchUpTransition(ctx context.Context, pass Publication, observedHead string) error {
	attempt := pass.Attempt
	var recorded bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM history
		WHERE provider = ? AND repository = ? AND change_number = ? AND round_id = ? AND pass_number = ?
		AND error_code = 'catch_up')`, append(pullRequestKey(attempt.PullRequest), attempt.RoundID, attempt.Pass)...).Scan(&recorded); err != nil || recorded {
		return err
	}
	now := time.Now().UTC()
	attempt, err := s.deliveryAttempt(ctx, pass, now)
	if err != nil {
		return err
	}
	attempt.FinishedAt = now
	attempt.ObservedHeadSHA = observedHead
	attempt.ErrorCode = "catch_up"
	attempt.ErrorMessage = "A push arrived before publication. The completed pass is saved for catch-up."
	return s.Finish(ctx, attempt)
}
