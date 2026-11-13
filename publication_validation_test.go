package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDraftValidationIdentifiesInvalidCommentField(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	attempt := Attempt{PullRequest: pr, HeadSHA: "head", SkillDigest: "digest"}
	valid := ReviewComment{Path: "main.go", Line: 12, Side: "RIGHT", Body: "Private finding text."}
	for _, test := range []struct {
		name   string
		field  string
		change func(*ReviewComment)
	}{
		{"parent path", "path", func(c *ReviewComment) { c.Path = "../main.go" }},
		{"zero line", "line", func(c *ReviewComment) { c.Line = 0 }},
		{"unknown side", "side", func(c *ReviewComment) { c.Side = "CENTER" }},
		{"empty body", "body", func(c *ReviewComment) { c.Body = " " }},
		{"reversed range", "start_line", func(c *ReviewComment) { c.StartLine = 13 }},
		{"start side without range", "start_side", func(c *ReviewComment) { c.StartSide = "RIGHT" }},
		{"range side mismatch", "start_side", func(c *ReviewComment) { c.StartLine, c.StartSide = 1, "LEFT" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := valid
			test.change(&invalid)
			draft := ReviewDraft{Provider: pr.Provider, Repository: pr.Repository, Number: pr.Number,
				HeadSHA: "head", SkillDigest: "digest", Verdict: "APPROVE", Body: "Summary.",
				Comments: []ReviewComment{valid, invalid}, Discussions: []DiscussionAction{}}
			err := validateDraft(draft, attempt, nil, false)
			if err == nil {
				t.Fatalf("validateDraft(invalid %s) = nil, want a field-specific error", test.field)
			}
			if !strings.Contains(err.Error(), "inline comment 2") || !strings.Contains(err.Error(), test.field) {
				t.Errorf("validateDraft(invalid %s) = %q, want comment 2 and field name", test.field, err)
			}
			if strings.Contains(err.Error(), valid.Body) {
				t.Errorf("validateDraft(invalid %s) exposed finding text: %q", test.field, err)
			}
		})
	}
}

func TestReadDraftAcceptsMaximumSize(t *testing.T) {
	body := `{"body":"review"}`
	body += strings.Repeat(" ", (1<<20)-len(body))
	file := filepath.Join(t.TempDir(), "draft.json")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := readDraft(file)
	if err != nil || draft.Body != "review" {
		t.Errorf("readDraft(valid 1 MiB JSON) = %+v, %v; want review body and no error", draft, err)
	}
}

func TestReadDraftRejectsOversizedJSON(t *testing.T) {
	body := `{"body":"review"}`
	body += strings.Repeat(" ", (1<<20)+1-len(body))
	file := filepath.Join(t.TempDir(), "draft.json")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDraft(file); err == nil {
		t.Error("readDraft(valid JSON above 1 MiB) = nil error, want size rejection")
	}
}
