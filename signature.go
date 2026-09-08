package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const signatureStart = "<!-- reviewctl:model-cost -->"
const signatureEnd = "<!-- /reviewctl:model-cost -->"

func reviewSignatureBody(body, signature string) (string, error) {
	start, end := strings.Index(body, signatureStart), strings.Index(body, signatureEnd)
	block := signatureStart + "\n" + signature + "\n" + signatureEnd
	if start < 0 && end < 0 {
		return strings.TrimRight(body, "\n") + "\n\n" + block, nil
	}
	if start < 0 || end < start || strings.Count(body, signatureStart) != 1 || strings.Count(body, signatureEnd) != 1 {
		return "", fmt.Errorf("review contains an invalid cost signature block")
	}
	return body[:start] + block + body[end+len(signatureEnd):], nil
}

func githubJSON(ctx context.Context, args []string, input any, output any) error {
	var stdin bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&stdin).Encode(input); err != nil {
			return err
		}
		args = append(args, "--input", "-")
	}
	command := exec.CommandContext(ctx, "gh", append([]string{"api"}, args...)...)
	prepareProcessGroup(command)
	var stdout limitedOutput
	var stderr tailWriter
	command.Stdin, command.Stdout, command.Stderr = &stdin, &stdout, &stderr
	if err := cleanupProcessGroup(command, command.Run()); err != nil {
		return fmt.Errorf("gh: %w: %s", err, stderr)
	}
	if stdout.exceeded {
		return fmt.Errorf("GitHub response exceeds 8 MiB")
	}
	if output != nil {
		return json.Unmarshal(stdout.Bytes(), output)
	}
	return nil
}

type submittedReview struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	Commit string `json:"commit_id"`
	URL    string `json:"html_url"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
}

func publishSignature(ctx context.Context, cfg Config, attempt Attempt) error {
	if attempt.Cost == nil || !attempt.Success || attempt.Recovered {
		return fmt.Errorf("attempt has no original review accounting")
	}
	pr, err := resolveGitHub(ctx, attempt.PullRequest)
	if err != nil {
		return err
	}
	if err := validateGitHubReviewTarget(cfg, attempt.PullRequest, pr); err != nil {
		return err
	}
	id, err := strconv.ParseInt(attempt.ReviewID, 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("invalid review ID")
	}
	login, err := resolveGitHubLogin(ctx)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/reviews/%d", attempt.Repository, attempt.Number, id)
	var review submittedReview
	if err := githubJSON(ctx, []string{endpoint}, nil, &review); err != nil {
		return err
	}
	marker := fmt.Sprintf("<!-- reviewctl:%s:%s#%d:%s:%s -->", attempt.Provider, attempt.Repository, attempt.Number, attempt.HeadSHA, attempt.SkillDigest)
	if review.ID != id || review.Commit != attempt.HeadSHA || review.URL != attempt.ReviewURL ||
		normalizeGitHubLogin(review.User.Login) != login || !strings.Contains(review.Body, marker) {
		return fmt.Errorf("submitted review does not match the recorded attempt and authenticated reviewer")
	}
	body, err := reviewSignatureBody(review.Body, attempt.Cost.signature())
	if err != nil {
		return err
	}
	if body == review.Body {
		return nil
	}
	if err := githubJSON(ctx, []string{"--method", "PUT", endpoint}, map[string]string{"body": body}, nil); err != nil {
		return err
	}
	var readback submittedReview
	if err := githubJSON(ctx, []string{endpoint}, nil, &readback); err != nil {
		return err
	}
	if readback.ID != id || readback.Body != body {
		return fmt.Errorf("cost signature readback mismatch")
	}
	return nil
}

func (s *Store) deliverSignatures(ctx context.Context, cfg Config) []resultError {
	if !cfg.Publish {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, attempt FROM review_signatures ORDER BY id LIMIT 100`)
	if err != nil {
		return []resultError{{Code: "signature_state_failed", Message: bounded(err.Error(), 512)}}
	}
	type pending struct {
		id      int64
		attempt Attempt
	}
	var entries []pending
	for rows.Next() {
		var entry pending
		var raw string
		if err = rows.Scan(&entry.id, &raw); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(raw), &entry.attempt); err != nil {
			break
		}
		entries = append(entries, entry)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return []resultError{{Code: "signature_state_failed", Message: bounded(err.Error(), 512)}}
	}
	var failures []resultError
	for _, entry := range entries {
		attempt := entry.attempt
		deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := publishSignature(deliveryCtx, cfg, attempt)
		cancel()
		if err == nil {
			_, err = s.db.ExecContext(ctx, `DELETE FROM review_signatures WHERE id = ?`, entry.id)
		} else {
			_, _ = s.db.ExecContext(ctx, `UPDATE review_signatures SET error_message = ? WHERE id = ?`, bounded(err.Error(), 512), entry.id)
		}
		if err != nil {
			failures = append(failures, resultError{Code: "signature_pending", Message: bounded(attempt.ReviewURL+": "+err.Error(), 512)})
		}
	}
	return failures
}
