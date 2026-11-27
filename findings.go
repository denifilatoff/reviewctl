package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

const findingRegistryTable = `CREATE TABLE IF NOT EXISTS finding_registries (
	pr_url TEXT PRIMARY KEY, payload TEXT NOT NULL)`
const findingPassesTable = `CREATE TABLE IF NOT EXISTS finding_passes (
	pr_url TEXT NOT NULL, round_id INTEGER NOT NULL, pass INTEGER NOT NULL,
	head_sha TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY(pr_url, round_id, pass))`

const (
	findingOpen       = "open"
	findingFixed      = "fixed"
	findingRegression = "regressed"
	findingAccepted   = "accepted_risk"
	findingWithdrawn  = "withdrawn"
	findingUnverified = "unable_to_verify"
)

type FindingAssessment struct {
	KnownID        string `json:"known_id,omitempty"`
	ReportID       string `json:"report_id,omitempty"`
	LegacyThreadID string `json:"legacy_thread_id,omitempty"`
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	EvidenceKey    string `json:"evidence_key,omitempty"`
	Comment        int    `json:"comment,omitempty"`
}

type FindingPublication struct {
	Inline        []string          `json:"inline,omitempty"`
	Announced     []string          `json:"announced,omitempty"`
	Discrepancies map[string]string `json:"discrepancies,omitempty"`
}

type findingThread struct {
	ID                 string `json:"id"`
	URL                string `json:"url"`
	Resolved           bool   `json:"resolved"`
	ResolvedBy         string `json:"resolved_by,omitempty"`
	ClosedByController bool   `json:"closed_by_controller,omitempty"`
	ClosureVerifiedSHA string `json:"closure_verified_sha,omitempty"`
}

type findingEvent struct {
	RoundID     int64          `json:"round_id"`
	Pass        int            `json:"pass"`
	HeadSHA     string         `json:"head_sha,omitempty"`
	Status      string         `json:"status,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	EvidenceKey string         `json:"evidence_key,omitempty"`
	Thread      *findingThread `json:"thread,omitempty"`
}

type findingRecord struct {
	ID                    string          `json:"id"`
	Title                 string          `json:"title"`
	Status                string          `json:"status"`
	VerifiedSHA           string          `json:"verified_sha,omitempty"`
	Reason                string          `json:"reason,omitempty"`
	Evidence              json.RawMessage `json:"evidence,omitempty"`
	Thread                *findingThread  `json:"thread,omitempty"`
	Events                []findingEvent  `json:"events,omitempty"`
	Announced             bool            `json:"announced,omitempty"`
	ReportedDiscrepancies []string        `json:"reported_discrepancies,omitempty"`
	EvidenceKey           string          `json:"evidence_key,omitempty"`
}

type findingRegistry struct {
	Findings []findingRecord `json:"findings"`
}

type FindingPass struct {
	Registry     findingRegistry          `json:"registry"`
	LegacyClosed map[string]findingThread `json:"legacy_closed"`
	threads      map[string]githubReviewThread
	login        string
}

type recordedFindingPass struct {
	Report json.RawMessage
	Draft  ReviewDraft
}

func readFindingRegistry(ctx context.Context, tx *sql.Tx, pr PullRequest) (findingRegistry, error) {
	var registry findingRegistry
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM finding_registries WHERE pr_url = ?`, pr.URL).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return registry, nil
	}
	if err != nil {
		return registry, err
	}
	err = json.Unmarshal(data, &registry)
	return registry, err
}

func writeFindingRegistry(ctx context.Context, tx *sql.Tx, pr PullRequest, registry findingRegistry) error {
	data, err := json.Marshal(registry)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO finding_registries(pr_url, payload) VALUES(?, ?)
		ON CONFLICT(pr_url) DO UPDATE SET payload = excluded.payload`, pr.URL, data)
	return err
}

func (r *findingRegistry) add(title string, thread *findingThread) *findingRecord {
	r.Findings = append(r.Findings, findingRecord{ID: findingID(len(r.Findings) + 1), Title: title,
		Status: findingOpen, Thread: thread, Announced: thread != nil})
	return &r.Findings[len(r.Findings)-1]
}

func (r *findingRegistry) byID(id string) *findingRecord {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "F"))
	if err != nil || n <= 0 || n > len(r.Findings) || r.Findings[n-1].ID != id {
		return nil
	}
	return &r.Findings[n-1]
}

func (r *findingRegistry) byThread(id string) *findingRecord {
	for i := range r.Findings {
		entry := &r.Findings[i]
		if entry.Thread != nil && entry.Thread.ID == id {
			return entry
		}
	}
	return nil
}

func findingID(n int) string { return "F" + strconv.Itoa(n) }

func findingMarker(id string) string { return "<!-- reviewctl:finding:" + id + " -->" }

func controllerFindingThread(pr PullRequest, login string, thread githubReviewThread) bool {
	if len(thread.Comments) == 0 || normalizeGitHubLogin(thread.Comments[0].Author) != login {
		return false
	}
	markerScope := fmt.Sprintf("<!-- reviewctl:%s:%s#%d:", pr.Provider, pr.Repository, pr.Number)
	return strings.Contains(thread.Comments[0].ReviewBody, markerScope)
}

func observeFindingThread(pr PullRequest, thread githubReviewThread) findingThread {
	return findingThread{ID: thread.ID,
		URL:      fmt.Sprintf("%s#discussion_r%d", pr.URL, thread.Comments[0].DatabaseID),
		Resolved: thread.IsResolved, ResolvedBy: normalizeGitHubLogin(thread.ResolvedBy)}
}

func (f *findingRecord) observeThread(attempt Attempt, state findingThread) {
	if f.Thread != nil && f.Thread.Resolved && state.Resolved && f.Thread.ResolvedBy == state.ResolvedBy {
		state.ClosedByController = f.Thread.ClosedByController || state.ClosedByController
		if state.ClosureVerifiedSHA == "" {
			state.ClosureVerifiedSHA = f.Thread.ClosureVerifiedSHA
		}
	}
	if f.Thread != nil && *f.Thread == state {
		return
	}
	f.Thread = &state
	f.Events = append(f.Events, findingEvent{RoundID: attempt.RoundID, Pass: attempt.Pass, Thread: &state})
}

func (s *Store) prepareFindingPass(ctx context.Context, attempt Attempt, threads []githubReviewThread, login string) (FindingPass, error) {
	input := FindingPass{LegacyClosed: map[string]findingThread{}, threads: map[string]githubReviewThread{},
		login: normalizeGitHubLogin(login)}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return input, err
	}
	defer tx.Rollback()
	registry, err := readFindingRegistry(ctx, tx, attempt.PullRequest)
	if err != nil {
		return input, err
	}
	threads = slices.Clone(threads)
	slices.SortFunc(threads, func(a, b githubReviewThread) int { return strings.Compare(a.ID, b.ID) })
	for _, thread := range threads {
		input.threads[thread.ID] = thread
		if len(thread.Comments) == 0 || normalizeGitHubLogin(thread.Comments[0].Author) != input.login {
			continue
		}
		state := observeFindingThread(attempt.PullRequest, thread)
		if entry := registry.byThread(thread.ID); entry != nil {
			entry.observeThread(attempt, state)
			continue
		}
		if !controllerFindingThread(attempt.PullRequest, input.login, thread) {
			continue
		}
		root := thread.Comments[0]
		if strings.Contains(root.Body, "<!-- reviewctl:finding:") {
			continue
		}
		if thread.IsResolved {
			input.LegacyClosed[thread.ID] = state
			continue
		}
		title, _, _ := strings.Cut(root.Body, "\n")
		entry := registry.add(title, nil)
		entry.Announced = true
		entry.observeThread(attempt, state)
	}
	input.Registry = registry
	if err := writeFindingRegistry(ctx, tx, attempt.PullRequest, registry); err != nil {
		return input, err
	}
	return input, tx.Commit()
}

func (input FindingPass) instruction(inputPath string) (string, error) {
	data, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		return "", err
	}
	return fmt.Sprintf(`
Read the finding registry snapshot at %q. Use its text and linked GitHub discussions as evidence.
Use the review scope specified for this pass. Assess every open or regressed finding in the registry. Revisit fixed
or previously closed findings when the code or replies provide evidence of regression. Respect documented accepted risks.
Use the locked skill's review.json format. Finding IDs in that file are pass-local until the controller reconciles them.
In the final controller draft, leave discussions empty. Keep body to one short summary without restating finding text.
Include finding_assessments as an array, even when empty. Each assessment has status and a concrete reason.
For an existing finding, include known_id from the registry. For a confirmed regression of a closed legacy finding,
include legacy_thread_id from the snapshot. For a new finding, include report_id from review.json, without known_id
or legacy_thread_id. When review.json includes an updated record for a known or legacy finding, also include report_id.
Map every review.json finding to exactly one assessment. Use these statuses: open, fixed, regressed, accepted_risk,
withdrawn, unable_to_verify. Use fixed only after verification. Use regressed for a verified return of a fixed or
closed defect. Use accepted_risk only when the discussion documents explicit risk acceptance.
For an open finding without a GitHub thread, include comment as the 1-based index of its inline comment if the
finding can be anchored at the reviewed revision. Map each inline comment exactly once. Leave out comment when the
finding has no valid inline position. Do not duplicate an existing thread with an inline comment.
If new evidence materially changes a discrepancy, include evidence_key with a short stable reference to the
decisive code or reply. Keep the prior key when the evidence has not changed. ReviewCTL assigns IDs and decides
whether to post, close, or reopen discussions.
`, inputPath), nil
}
