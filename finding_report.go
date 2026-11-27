package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func validateFindingSchema(file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var schema struct {
		Defs struct {
			Finding struct {
				Required   []string
				Properties struct {
					ID struct {
						Type        string
						Description string
						Pattern     string
					}
				}
			}
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		return err
	}
	id := schema.Defs.Finding.Properties.ID
	if id.Type != "string" || !strings.Contains(id.Description, "e.g. "+findingID(3)+".") {
		return fmt.Errorf("locked review schema finding ID contract changed; update ReviewCTL before upgrading the skill")
	}
	for _, field := range []string{"id", "title"} {
		found := false
		for _, required := range schema.Defs.Finding.Required {
			found = found || required == field
		}
		if !found {
			return fmt.Errorf("locked review schema no longer requires finding %s; update ReviewCTL before upgrading the skill", field)
		}
	}
	if id.Pattern != "" {
		pattern, err := regexp.Compile(id.Pattern)
		if err != nil {
			return fmt.Errorf("locked review schema finding ID pattern: %w", err)
		}
		for _, n := range []int{1, 9, 10, 100, 1000000} {
			if !pattern.MatchString(findingID(n)) {
				return fmt.Errorf("locked review schema rejects finding ID %s; update ReviewCTL before upgrading the skill", findingID(n))
			}
		}
	}
	return nil
}

type findingReport struct {
	fields     map[string]json.RawMessage
	findings   []json.RawMessage
	identities map[string]struct {
		index int
		title string
	}
}

func readFindingReport(file string, attempt Attempt, verdict string) (findingReport, error) {
	var report findingReport
	data, err := os.ReadFile(file)
	if err != nil {
		return report, err
	}
	var scope struct {
		SchemaVersion int `json:"schema_version"`
		Target        struct {
			Platform string
			URL      string
		}
		Revision struct{ Head string }
		Result   string
		Findings []json.RawMessage
	}
	if err := json.Unmarshal(data, &scope); err != nil {
		return report, err
	}
	if scope.SchemaVersion != 1 || scope.Target.Platform != attempt.Provider || scope.Revision.Head != attempt.HeadSHA ||
		!samePullRequestIdentity(scope.Target.URL, attempt.PullRequest) || scope.Findings == nil {
		return report, fmt.Errorf("review.json does not describe the pinned pull request, revision, and schema")
	}
	if scope.Result != "approve" && scope.Result != "request_changes" ||
		verdict != "COMMENT" && strings.ToUpper(scope.Result) != verdict {
		return report, fmt.Errorf("review.json result %q differs from the draft verdict %q", scope.Result, verdict)
	}
	if err := json.Unmarshal(data, &report.fields); err != nil {
		return report, err
	}
	report.findings = scope.Findings
	report.identities = make(map[string]struct {
		index int
		title string
	}, len(scope.Findings))
	for i, raw := range scope.Findings {
		var key struct {
			ID    string
			Title string
		}
		if err := json.Unmarshal(raw, &key); err != nil {
			return report, err
		}
		if strings.TrimSpace(key.ID) == "" || strings.TrimSpace(key.Title) == "" {
			return report, fmt.Errorf("review.json finding %d requires an ID and title", i+1)
		}
		if _, exists := report.identities[key.ID]; exists {
			return report, fmt.Errorf("review.json repeats finding ID %q", key.ID)
		}
		report.identities[key.ID] = struct {
			index int
			title string
		}{i, key.Title}
	}
	return report, nil
}

func (r *findingReport) reconcileRecord(oldID string, finding *findingRecord) error {
	key := r.identities[oldID]
	var record map[string]json.RawMessage
	if err := json.Unmarshal(r.findings[key.index], &record); err != nil {
		return err
	}
	record["id"], _ = json.Marshal(finding.ID)
	var err error
	r.findings[key.index], err = json.Marshal(record)
	if err == nil {
		finding.Evidence = r.findings[key.index]
		finding.Title = key.title
	}
	return err
}

func (r findingReport) encode(ids map[string]string) (json.RawMessage, error) {
	var err error
	r.fields["findings"], err = json.Marshal(r.findings)
	if err != nil {
		return nil, err
	}
	if raw, exists := r.fields["previous_findings"]; exists {
		var previous []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &previous); err != nil {
			return nil, err
		}
		for _, record := range previous {
			var id string
			if raw, exists := record["finding_id"]; exists {
				if err := json.Unmarshal(raw, &id); err != nil {
					return nil, err
				}
				if id == "" {
					continue
				}
				resolved, ok := ids[id]
				if !ok {
					return nil, fmt.Errorf("previous finding refers to %q, which is missing from review.json", id)
				}
				record["finding_id"], _ = json.Marshal(resolved)
			}
		}
		r.fields["previous_findings"], err = json.Marshal(previous)
		if err != nil {
			return nil, err
		}
	}
	return json.MarshalIndent(r.fields, "", "  ")
}

func loadRecordedFindingPass(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, attempt Attempt) (*recordedFindingPass, error) {
	var head string
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT head_sha, payload FROM finding_passes
		WHERE pr_url = ? AND round_id = ? AND pass = ?`, attempt.URL, attempt.RoundID, attempt.Pass).Scan(&head, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if head != attempt.HeadSHA {
		return nil, fmt.Errorf("round %d pass %d finding record belongs to %s, not %s", attempt.RoundID, attempt.Pass, head, attempt.HeadSHA)
	}
	var saved recordedFindingPass
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, err
	}
	return &saved, nil
}

func (f recordedFindingPass) writeReport(path string) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".review-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(f.Report)
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), path)
}

func (s *Store) recordFindingPass(ctx context.Context, attempt Attempt, input FindingPass, reportPath string, draft *ReviewDraft) error {
	if attempt.RoundID <= 0 || attempt.Pass <= 0 {
		return fmt.Errorf("recording findings requires a round and pass")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	saved, err := loadRecordedFindingPass(ctx, tx, attempt)
	if err != nil {
		return err
	}
	if saved == nil {
		registry, err := readFindingRegistry(ctx, tx, attempt.PullRequest)
		if err != nil {
			return err
		}
		report, err := readFindingReport(reportPath, attempt, draft.Verdict)
		if err != nil {
			return err
		}
		prepared, resolved, err := applyFindingAssessments(attempt, input, &registry, report, *draft)
		if err != nil {
			return err
		}
		saved = &recordedFindingPass{Report: resolved, Draft: prepared}
		raw, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if err := writeFindingRegistry(ctx, tx, attempt.PullRequest, registry); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO finding_passes(pr_url, round_id, pass, head_sha, payload)
			VALUES (?, ?, ?, ?, ?)`, attempt.URL, attempt.RoundID, attempt.Pass, attempt.HeadSHA, raw); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := saved.writeReport(reportPath); err != nil {
		return err
	}
	*draft = saved.Draft
	return nil
}
