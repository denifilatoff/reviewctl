package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindingStageRecoversCommittedIDsAndEvidence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	attempt := findingTestAttempt(t, 1, 1, "verified-head")
	attempt.SkillDigest = "sha256:test"
	artifacts := t.TempDir()
	writeFindingTestReport(t, filepath.Join(artifacts, "review.json"), attempt,
		`[{"id":"F40","title":"Evidence retained","evidence":[{"basis":"fact","text":"evidence/check.txt"}]}]`, `[]`)
	if err := os.Mkdir(filepath.Join(artifacts, "evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "evidence", "check.txt"), []byte("checked at verified-head"), 0o600); err != nil {
		t.Fatal(err)
	}
	publication := Publication{Attempt: attempt, Login: "reviewer", Draft: findingTestDraft(
		FindingAssessment{ReportID: "F40", Status: findingOpen, Reason: "The check failed on this revision."})}
	if err := store.stageFindingPass(publication, artifacts); err != nil {
		t.Fatal(err)
	}
	stage := store.findingStage(attempt)
	input, err := store.prepareFindingPass(ctx, attempt, publication.Threads, publication.Login)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.recordFindingPass(ctx, attempt, input, filepath.Join(stage, "review.json"), &publication.Draft); err != nil {
		t.Fatal(err)
	}
	if latest, err := store.latestRoundPass(attempt); latest != nil || err != nil {
		t.Fatalf("latestRoundPass before stage recovery = %+v, %v, want no visible pass", latest, err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	expected := attempt
	expected.HeadSHA = "newer-unreviewed-head"
	recovered, err := store.completeFindingStage(ctx, expected)
	if err != nil || recovered == nil {
		t.Fatalf("completeFindingStage after restart = %+v, %v, want saved pass", recovered, err)
	}
	if recovered.Attempt.HeadSHA != "verified-head" || !strings.Contains(recovered.Draft.Body, "- F1 Evidence retained") {
		t.Errorf("restored pass = %+v, want F1 and the actual reviewed head", recovered)
	}
	final := store.passDirectory(attempt)
	evidence, err := os.ReadFile(filepath.Join(final, "evidence", "check.txt"))
	if err != nil || string(evidence) != "checked at verified-head" {
		t.Errorf("recovered evidence = %q, %v, want the original file content", evidence, err)
	}
}
