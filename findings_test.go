package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindingPassReusesAllocatedIDsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.db.Close() }()
	attempt := findingTestAttempt(t, 1, 1, "first")
	report := filepath.Join(t.TempDir(), "review.json")
	writeFindingTestReport(t, report, attempt, `[{"id":"F12","title":"Missing destination check"}]`, `[]`)
	firstInput, err := store.prepareFindingPass(ctx, attempt, nil, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	draft := findingTestDraft(FindingAssessment{ReportID: "F12", Status: findingOpen, Reason: "Writes can escape the destination."})
	if err := store.recordFindingPass(ctx, attempt, firstInput, report, &draft); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(report); err != nil {
		t.Fatal(err)
	}
	var recovered ReviewDraft
	if err := store.recordFindingPass(ctx, attempt, FindingPass{}, report, &recovered); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recovered.Body, "- F1 Missing destination check") {
		t.Errorf("recovered short result = %q, want the first allocated finding F1", recovered.Body)
	}
	attempt.Pass, attempt.HeadSHA = 2, "second"
	input, err := store.prepareFindingPass(ctx, attempt, nil, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	writeFindingTestReport(t, report, attempt,
		`[{"id":"F1","title":"Incorrect path join"},{"id":"F9","title":"Missing destination check"}]`,
		`[{"thread_url":"https://example.test/pr","status":"fix_not_accepted","reason":"Still reachable","finding_id":"F9"}]`)
	draft = findingTestDraft(
		FindingAssessment{ReportID: "F1", Status: findingOpen, Reason: "The new base path is ignored."},
		FindingAssessment{KnownID: "F1", ReportID: "F9", Status: findingOpen, Reason: "Writes can escape the destination."},
	)
	if err := store.recordFindingPass(ctx, attempt, input, report, &draft); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Findings []struct{ ID, Title string }
		Previous []struct {
			ID string `json:"finding_id"`
		} `json:"previous_findings"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Findings) != 2 || saved.Findings[0].ID != "F2" || saved.Findings[1].ID != "F1" {
		t.Errorf("saved findings = %+v, want F2 for the new defect and F1 for the known defect", saved.Findings)
	}
	if len(saved.Previous) != 1 || saved.Previous[0].ID != "F1" {
		t.Errorf("previous finding references = %+v, want F1", saved.Previous)
	}
}

func TestLegacyFindingsMigrateOnlyManagedOpenThreads(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	attempt := findingTestAttempt(t, 1, 1, "first")
	legacy := reviewMarker(attempt.PullRequest, "historic", "digest")
	threads := []githubReviewThread{
		{ID: "managed-closed", IsResolved: true, ResolvedBy: "author", Comments: []githubReviewComment{
			{DatabaseID: 42, Author: "reviewer", Body: "Old defect", ReviewBody: legacy},
		}},
		{ID: "managed-open", Comments: []githubReviewComment{
			{DatabaseID: 21, Author: "reviewer", Body: "Open defect", ReviewBody: legacy},
		}},
		{ID: "handwritten", Comments: []githubReviewComment{{DatabaseID: 66, Author: "reviewer", Body: "Manual review"}}},
	}
	input, err := store.prepareFindingPass(ctx, attempt, threads, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if got := input.Registry.byID("F1"); len(input.Registry.Findings) != 1 || got == nil ||
		got.Thread.URL != attempt.URL+"#discussion_r21" {
		t.Errorf("migrated open findings = %+v, want F1 linked to discussion_r21", input.Registry.Findings)
	}
	report := filepath.Join(t.TempDir(), "review.json")
	writeFindingTestReport(t, report, attempt, `[{"id":"F1","title":"Old defect returned"}]`, `[]`)
	draft := findingTestDraft(
		FindingAssessment{KnownID: "F1", Status: findingOpen, Reason: "The route remains available."},
		FindingAssessment{LegacyThreadID: "managed-closed", ReportID: "F1", Status: findingRegression, Reason: "An equivalent route returned."},
	)
	if err := store.recordFindingPass(ctx, attempt, input, report, &draft); err != nil {
		t.Fatal(err)
	}
	if len(draft.Discussions) != 0 || !strings.Contains(draft.Body, "[F2]("+attempt.URL+"#discussion_r42) still applies") {
		t.Errorf("review = %+v, want one discrepancy for F2 and no human-thread mutations", draft)
	}
}

func TestFindingThreadDecisionsPreserveHumanClosureAndReopenControllerClosure(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	attempt := findingTestAttempt(t, 1, 1, "verified-fix")
	marker := reviewMarker(attempt.PullRequest, "historic", "digest")
	threads := []githubReviewThread{
		{ID: "controller", Comments: []githubReviewComment{{DatabaseID: 11, Author: "reviewer", Body: "First issue", ReviewBody: marker}}},
		{ID: "human", Comments: []githubReviewComment{{DatabaseID: 22, Author: "reviewer", Body: "Second issue", ReviewBody: marker}}},
	}
	report := filepath.Join(t.TempDir(), "review.json")
	writeFindingTestReport(t, report, attempt, `[]`, `[]`)
	input, err := store.prepareFindingPass(ctx, attempt, threads, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	draft := findingTestDraft(
		FindingAssessment{KnownID: "F1", Status: findingFixed, Reason: "The first path checks its input."},
		FindingAssessment{KnownID: "F2", Status: findingFixed, Reason: "The second path checks its input."},
	)
	if err := store.recordFindingPass(ctx, attempt, input, report, &draft); err != nil {
		t.Fatal(err)
	}
	if len(draft.Discussions) != 2 || draft.Discussions[0].VerifiedSHA != "verified-fix" {
		t.Fatalf("fixed actions = %+v, want two actions with the original verification SHA", draft.Discussions)
	}
	threads[0].IsResolved, threads[0].ResolvedBy = true, "reviewer"
	threads[1].IsResolved, threads[1].ResolvedBy = true, "author"
	published := Publication{Attempt: attempt, Draft: draft, Login: "reviewer"}
	published.Attempt.HeadSHA = "later-publication-head"
	result := attempt
	result.DiscussionOutcomes = []DiscussionOutcome{{ThreadID: "controller", Action: "resolved_with_reply", ReplyID: "123"}}
	if err := store.recordFindingPublication(ctx, published, result, threads); err != nil {
		t.Fatal(err)
	}
	attempt.RoundID, attempt.HeadSHA = 2, "regression"
	input, err = store.prepareFindingPass(ctx, attempt, threads, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if closure := input.Registry.byID("F1").Thread; !closure.ClosedByController || closure.ClosureVerifiedSHA != "verified-fix" {
		t.Errorf("controller closure = %+v, want the verified fix SHA", closure)
	}
	writeFindingTestReport(t, report, attempt, `[]`, `[]`)
	draft = findingTestDraft(
		FindingAssessment{KnownID: "F1", Status: findingRegression, Reason: "The first input check was removed."},
		FindingAssessment{KnownID: "F2", Status: findingRegression, Reason: "The second input check was removed.", EvidenceKey: "second-input-removed"},
	)
	if err := store.recordFindingPass(ctx, attempt, input, report, &draft); err != nil {
		t.Fatal(err)
	}
	if got := draft.Discussions; len(got) != 1 || got[0].ThreadID != "controller" || got[0].Action != "open_with_reply" ||
		got[0].ExpectedResolver != "reviewer" || got[0].VerifiedSHA != "regression" {
		t.Errorf("regression actions = %+v, want only a controller reopen verified at regression", got)
	}
	if !strings.Contains(draft.Body, "[F2]("+attempt.URL+"#discussion_r22)") {
		t.Errorf("short result = %q, want one discrepancy linked to the human-closed thread", draft.Body)
	}
	published = Publication{Attempt: attempt, Draft: draft, Login: "reviewer"}
	if err := store.recordFindingPublication(ctx, published, attempt, threads); err != nil {
		t.Fatal(err)
	}
	attempt.RoundID = 3
	input, err = store.prepareFindingPass(ctx, attempt, threads, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	writeFindingTestReport(t, report, attempt, `[]`, `[]`)
	draft = findingTestDraft(
		FindingAssessment{KnownID: "F1", Status: findingRegression, Reason: "The first input check was removed."},
		FindingAssessment{KnownID: "F2", Status: findingRegression, Reason: "The second input check remains absent.", EvidenceKey: "second-input-removed"},
	)
	if err := store.recordFindingPass(ctx, attempt, input, report, &draft); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(draft.Body, "discussion_r22") {
		t.Errorf("short result = %q, want no repeated discrepancy for the same evidence", draft.Body)
	}
}

func TestLockedSkillFindingIDCompatibility(t *testing.T) {
	path := os.Getenv("REVIEWCTL_LOCKED_SCHEMA")
	if path == "" {
		t.Skip("run make apm-check to validate the frozen review schema")
	}
	if err := validateFindingSchema(path); err != nil {
		t.Fatal(err)
	}
}

func TestFindingSchemaRejectsIncompatibleUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.schema.json")
	before := `{"$defs":{"finding":{"required":["id","title"],"properties":{"id":{"type":"string","description":"Stable across rounds, e.g. F3.","pattern":"^F[1-9][0-9]*$"}}}}}`
	after := strings.Replace(before, "^F[1-9][0-9]*$", "^F[1-9]$", 1)
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateFindingSchema(path); err != nil {
		t.Fatalf("original schema: %v", err)
	}
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateFindingSchema(path); err == nil || !strings.Contains(err.Error(), "rejects finding ID F10") {
		t.Errorf("validateFindingSchema(incompatible upgrade) = %v, want rejection of F10", err)
	}
}

func findingTestAttempt(t *testing.T, round int64, pass int, head string) Attempt {
	t.Helper()
	pr, err := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	return Attempt{PullRequest: pr, RoundID: round, Pass: pass, HeadSHA: head}
}

func findingTestDraft(assessments ...FindingAssessment) ReviewDraft {
	return ReviewDraft{Verdict: "APPROVE", Body: "Review complete.", FindingAssessments: assessments}
}

func writeFindingTestReport(t *testing.T, path string, attempt Attempt, findings, previous string) {
	t.Helper()
	report := map[string]any{
		"schema_version": 1, "target": map[string]string{"platform": "github", "url": attempt.URL},
		"revision": map[string]string{"head": attempt.HeadSHA}, "result": "approve",
		"findings": json.RawMessage(findings), "previous_findings": json.RawMessage(previous),
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
