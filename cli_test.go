package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMainHumanRunPrintsSummary(t *testing.T) {
	temp := t.TempDir()
	configHome := filepath.Join(temp, "config")
	configDir := filepath.Join(configHome, "reviewctl")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "harness: codex\npublish: true\ntrusted_authors: [\"dependabot[bot]\"]\nrepositories:\n  - provider: github\n    repository: acme/service\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(temp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(temp, "runtime"))

	var stdout, stderr bytes.Buffer
	exitCode := Main([]string{"run"}, &stdout, &stderr)
	if exitCode != 0 || stderr.String() != "" {
		t.Fatalf("run failed: exit=%d stderr=%q", exitCode, stderr.String())
	}
	want := "run: queued=0 attempted=0 succeeded=0 failed=0\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestMainBulkReviewValidatesAllInputsBeforeInsertion(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(temp, "state"))
	valid := "https://github.com/acme/service/pull/1"
	var stdout, stderr bytes.Buffer
	if exit := Main([]string{"--json", "bulk-review", valid, "not-a-url"}, &stdout, &stderr); exit != 2 {
		t.Fatalf("invalid bulk exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
	}
	path, _ := statePath()
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := store.Snapshot(context.Background())
	store.Close()
	if err != nil || len(queue) != 0 {
		t.Fatalf("invalid bulk changed queue: %+v, err = %v", queue, err)
	}

	stdout.Reset()
	stderr.Reset()
	second := "https://github.com/acme/service/pull/2"
	if exit := Main([]string{"--json", "bulk-review", valid, valid, second}, &stdout, &stderr); exit != 0 {
		t.Fatalf("valid bulk exit = %d, stderr = %q", exit, stderr.String())
	}
	var result struct {
		Command string `json:"command"`
		Status  string `json:"status"`
		Results []struct {
			PullRequest PullRequest `json:"pull_request"`
			Status      string      `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Command != "bulk-review" || result.Status != "success" || len(result.Results) != 3 ||
		result.Results[0].Status != "queued" || result.Results[1].Status != "already_queued" ||
		result.Results[2].Status != "queued" || result.Results[0].PullRequest.Number != 1 ||
		result.Results[2].PullRequest.Number != 2 || stderr.String() != "" {
		t.Fatalf("bulk result = %+v, stderr = %q", result, stderr.String())
	}

	stdout.Reset()
	if exit := Main([]string{"--json", "review", valid}, &stdout, &stderr); exit != 0 ||
		!bytes.Contains(stdout.Bytes(), []byte(`"status":"already_queued"`)) {
		t.Fatalf("review did not reuse queue behavior: exit = %d, stdout = %q", exit, stdout.String())
	}
}

func TestMainStatusReturnsDeterministicBoundedState(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(temp, "state"))
	path, _ := statePath()
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for number := int64(1); number <= 2; number++ {
		pr, _ := ParsePullRequestURL(fmt.Sprintf("https://github.com/acme/service/pull/%d", number))
		if _, err := store.Enqueue(context.Background(), pr); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	for number := int64(1); number <= 25; number++ {
		pr, _ := ParsePullRequestURL(fmt.Sprintf("https://github.com/acme/history/pull/%d", number))
		if err := store.Finish(context.Background(), Attempt{
			PullRequest: pr, StartedAt: started, FinishedAt: started.Add(time.Second), ErrorCode: "failed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()

	for _, test := range []struct {
		name        string
		args        []string
		wantHistory int
		wantOldest  int64
	}{
		{name: "default", args: []string{"--json", "status"}, wantHistory: 20, wantOldest: 6},
		{name: "custom", args: []string{"--json", "status", "--limit", "2"}, wantHistory: 2, wantOldest: 24},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exit := Main(test.args, &stdout, &stderr); exit != 0 || stderr.String() != "" {
				t.Fatalf("status exit = %d, stderr = %q", exit, stderr.String())
			}
			var result struct {
				Command string        `json:"command"`
				Status  string        `json:"status"`
				Queue   []PullRequest `json:"queue"`
				History []Attempt     `json:"history"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Command != "status" || result.Status != "success" || len(result.Queue) != 2 ||
				result.Queue[0].Number != 1 || result.Queue[1].Number != 2 || len(result.History) != test.wantHistory ||
				result.History[0].Number != 25 || result.History[len(result.History)-1].Number != test.wantOldest {
				t.Fatalf("status result = %+v", result)
			}
		})
	}
}

func TestMainStatusRejectsInvalidLimit(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "status", "--limit", "0"},
		{"--json", "status", "--limit", "many"},
		{"--json", "status", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if exit := Main(args, &stdout, &stderr); exit != 2 || stderr.String() != "" {
			t.Fatalf("args = %v, exit = %d, stdout = %q, stderr = %q", args, exit, stdout.String(), stderr.String())
		}
	}
}

func TestMainEmptyStatusUsesJSONArrays(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	var stdout, stderr bytes.Buffer
	if exit := Main([]string{"--json", "status"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("status exit = %d, stderr = %q", exit, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"history":[]`)) ||
		!bytes.Contains(stdout.Bytes(), []byte(`"queue":[]`)) {
		t.Fatalf("empty status must use arrays: %q", stdout.String())
	}
}

func TestMainInitCreatesPathsWithoutOverwritingConfig(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(temp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(temp, "state"))
	var stdout, stderr bytes.Buffer
	if exit := Main([]string{"--json", "init"}, &stdout, &stderr); exit != 0 || stderr.String() != "" {
		t.Fatalf("init exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
	}
	config, _ := configPath()
	state, _ := statePath()
	data, err := os.ReadFile(config)
	if err != nil || len(data) == 0 {
		t.Fatalf("config was not created: bytes = %q, err = %v", data, err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state was not created: %v", err)
	}

	const existing = "keep: exactly\n"
	if err := os.WriteFile(config, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if exit := Main([]string{"--json", "init"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("second init exit = %d, stdout = %q, stderr = %q", exit, stdout.String(), stderr.String())
	}
	data, err = os.ReadFile(config)
	if err != nil || string(data) != existing {
		t.Fatalf("existing config changed to %q, err = %v", data, err)
	}
}
