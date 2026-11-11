package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSignatureDeliveryRetryAndOwnership(t *testing.T) {
	temp := t.TempDir()
	bin := installProcessHelpers(t, temp)
	t.Setenv("GO_WANT_REVIEWCTL_HELPER", "1")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REVIEWCTL_FAKE_STATE", filepath.Join(temp, "published"))
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	amount := 0.10910315
	attempt := Attempt{PullRequest: pr, Success: true, HeadSHA: "0123456789abcdef0123456789abcdef01234567",
		SkillDigest: "digest", ReviewID: "42", ReviewURL: pr.URL + "#pullrequestreview-42",
		StartedAt: time.Now(), FinishedAt: time.Now(), Cost: &ReviewCost{
			Model: "gpt-test", Effort: "low", USD: &amount,
			Report: json.RawMessage(`{"sessions":[
				{"inputTokens":10000,"cacheReadTokens":40000,"outputTokens":2300,"reasoningOutputTokens":1300},
				{"inputTokens":2500,"cacheReadTokens":0,"outputTokens":700,"reasoningOutputTokens":400}
			]}`),
		}}
	cfg := Config{Harness: "codex", Publish: true, TrustedAuthors: []string{"dependabot[bot]"},
		Repositories: []Repository{{Provider: "github", Repository: "acme/service"}}}
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.Finish(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	// Missing remote review leaves delivery queued without invoking Codex.
	if errors := store.deliverSignatures(ctx, cfg); len(errors) != 1 || errors[0].Code != "signature_pending" {
		t.Fatalf("missing review: %+v", errors)
	}
	review := submittedReview{ID: 42, Commit: attempt.HeadSHA, URL: attempt.ReviewURL,
		Body: "Findings\n\n<!-- reviewctl:github:acme/service#7:" + attempt.HeadSHA + ":digest -->"}
	review.User.Login = "reviewer"
	path := filepath.Join(temp, "published.review.json")
	data, _ := json.Marshal(review)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_PR_STATE", "MERGED")
	t.Setenv("REVIEWCTL_FAKE_PR_DRAFT", "1")
	if errors := store.deliverSignatures(ctx, cfg); len(errors) != 0 {
		t.Fatal(errors)
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &review) != nil ||
		!strings.HasSuffix(review.Body, "\n\n<sub>🤖 gpt-test low in=12.5k cached=40k out=3k usd=0.11</sub>") || strings.Count(review.Body, signatureStart) != 1 {
		t.Fatalf("signature readback: %s %v", data, err)
	}
	if err := publishSignature(ctx, cfg, attempt); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != string(data) {
		t.Fatal("duplicate delivery changed review")
	}
	var pending int
	if err := store.db.QueryRow(`SELECT count(*) FROM review_signatures`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	t.Setenv("REVIEWCTL_FAKE_LOGIN", "someone-else")
	if err := publishSignature(ctx, cfg, attempt); err == nil {
		t.Fatal("updated another reviewer's review")
	}
	if _, err := os.Stat(filepath.Join(temp, "published")); !os.IsNotExist(err) {
		t.Fatal("signature retry invoked Codex")
	}
}

func TestSignatureDeliveryRotatesPersistentFailures(t *testing.T) {
	temp := t.TempDir()
	bin := installProcessHelpers(t, temp)
	t.Setenv("GO_WANT_REVIEWCTL_HELPER", "1")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REVIEWCTL_FAKE_STATE", filepath.Join(temp, "missing-review"))
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	cfg := Config{Harness: "codex", Publish: true, TrustedAuthors: []string{"dependabot[bot]"},
		Repositories: []Repository{{Provider: "github", Repository: "acme/service"}}}
	store := openTestStore(t)
	amount := 0.1
	for _, id := range []string{"1", "2", "3"} {
		attempt := Attempt{PullRequest: pr, Success: true, HeadSHA: "0123456789abcdef0123456789abcdef01234567",
			SkillDigest: "digest", ReviewID: id, ReviewURL: pr.URL + "#pullrequestreview-" + id,
			Cost: &ReviewCost{Model: "gpt-test", Effort: "low", USD: &amount}}
		if err := store.queueSignature(attempt); err != nil {
			t.Fatal(err)
		}
	}
	if failures := store.deliverSignatureBatch(context.Background(), cfg, 2); len(failures) != 2 {
		t.Fatalf("first delivery failures = %+v", failures)
	}
	var errorMessage string
	if err := store.db.QueryRow(`SELECT error_message FROM review_signatures WHERE review_id = '3'`).Scan(&errorMessage); err != nil {
		t.Fatal(err)
	}
	if errorMessage != "" {
		t.Fatalf("newest signature was attempted in the first batch: %q", errorMessage)
	}
	if failures := store.deliverSignatureBatch(context.Background(), cfg, 2); len(failures) != 2 {
		t.Fatalf("second delivery failures = %+v", failures)
	}
	if err := store.db.QueryRow(`SELECT error_message FROM review_signatures WHERE review_id = '3'`).Scan(&errorMessage); err != nil {
		t.Fatal(err)
	}
	if errorMessage == "" {
		t.Fatal("persistent old failures starved a new signature")
	}
}

func TestReviewSignatureReplacesLegacyFooter(t *testing.T) {
	const signature = "<sub>🤖 gpt-test usd=0.0042</sub>"
	const legacy = "Model: gpt-test (~$0.0)"
	body := "Findings\n\n" + signatureStart + "\n" + legacy + "\n" + signatureEnd + "\n\nFollow-up"
	got, err := reviewSignatureBody(body, signature, legacy)
	want := "Findings\n\n\n\nFollow-up\n\n" + signatureStart + "\n\n" + signature
	if err != nil || got != want {
		t.Fatalf("review body = %q, %v; want %q", got, err, want)
	}
	withFollowUp := got + "\n\nManual edit"
	moved, err := reviewSignatureBody(withFollowUp, signature, legacy)
	if err != nil || !strings.Contains(moved, "Follow-up\n\n\n\nManual edit") || !strings.HasSuffix(moved, signature) {
		t.Fatalf("review body with a follow-up = %q, %v", moved, err)
	}
	for _, edited := range []string{
		strings.Replace(got, "usd=0.0042", "usd=0.0042 (manually verified)", 1),
		strings.Replace(body, "(~$0.0)", "(~$0.0) manually verified", 1),
	} {
		if _, err := reviewSignatureBody(edited, signature, legacy); err == nil {
			t.Fatalf("accepted an ambiguous managed footer: %q", edited)
		}
	}
}
