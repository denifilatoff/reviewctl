package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMainVersionUsesBuildValue(t *testing.T) {
	previous := Version
	Version = "v9.8.7"
	t.Cleanup(func() { Version = previous })
	var stdout, stderr bytes.Buffer
	if exit := Main([]string{"--version"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit = %d", exit)
	}
	if stdout.String() != "v9.8.7\n" || stderr.String() != "" {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestRunLockObservationNeverClaimsConsumerLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	lock, alreadyRunning, err := acquireRunLock(path)
	if err != nil || alreadyRunning {
		t.Fatalf("seed lock: already_running=%t err=%v", alreadyRunning, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var observers sync.WaitGroup
	for range 8 {
		observers.Add(1)
		go func() {
			defer observers.Done()
			<-start
			for range 2_000 {
				if _, err := observeRunLock(path); err != nil {
					t.Errorf("observe lock: %v", err)
					return
				}
			}
		}()
	}
	close(start)
	for range 2_000 {
		lock, alreadyRunning, err := acquireRunLock(path)
		if err != nil {
			t.Fatal(err)
		}
		if alreadyRunning {
			t.Fatal("lock observation made a consumer report already_running")
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	}
	observers.Wait()
}

func TestWriteHumanRunResultShowsPartialDiscoveryFailure(t *testing.T) {
	result := runResult{
		Repositories: 2, DiscoverySucceeded: 1, DiscoveryFailed: 1, Discovered: 2,
		Discovery: []discoveryItem{
			{Provider: "github", Repository: "acme/service", Status: "success", Observed: 2},
			{
				Provider: "github", Repository: "acme/other", Status: "failed",
				Error: &resultError{Code: "github_failed", Message: "scripted discovery failure"},
			},
		},
		Results: []runItem{},
	}
	var stdout bytes.Buffer
	writeHumanRunResult(&stdout, result)
	want := "run: repositories=2 discovery_failed=1 discovered=2 enqueued=0 queued=0 attempted=0 succeeded=0 failed=0\n" +
		"discovery: repository=acme/service status=success observed=2 enqueued=0\n" +
		"discovery: repository=acme/other status=failed error=github_failed: scripted discovery failure\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestWriteHumanRunResultShowsAttemptFailure(t *testing.T) {
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	result := runResult{
		Repositories: 1, Queued: 1, Attempted: 1, Failed: 1, Discovery: []discoveryItem{},
		Results: []runItem{{
			PullRequest: pr, Status: "failed",
			Error: &resultError{Code: "publication_disabled", Message: "publication is disabled"},
		}},
	}
	var stdout bytes.Buffer
	writeHumanRunResult(&stdout, result)
	want := "run: repositories=1 discovery_failed=0 discovered=0 enqueued=0 queued=1 attempted=1 succeeded=0 failed=1\n" +
		"attempt: pull_request=github:acme/service#7 url=https://github.com/acme/service/pull/7 " +
		"status=failed error=publication_disabled: publication is disabled\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestWriteHumanRunResultPreservesSuccessOrdering(t *testing.T) {
	first, _ := ParsePullRequestURL("https://github.com/acme/service/pull/9")
	second, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	result := runResult{
		Repositories: 2, DiscoverySucceeded: 2, Discovered: 3, Enqueued: 2,
		Discovery: []discoveryItem{
			{Provider: "github", Repository: "acme/service", Status: "success", Observed: 2, Enqueued: 1},
			{Provider: "github", Repository: "acme/other", Status: "success", Observed: 1, Enqueued: 1},
		},
		Queued: 2, Attempted: 2, Succeeded: 2,
		Results: []runItem{
			{
				PullRequest: first, Status: "success", Verdict: "APPROVE",
				ReviewURL: first.URL + "#pullrequestreview-9",
			},
			{
				PullRequest: second, Status: "success", Verdict: "REQUEST_CHANGES",
				ReviewURL: second.URL + "#pullrequestreview-7", Recovered: true,
			},
		},
	}
	var stdout bytes.Buffer
	writeHumanRunResult(&stdout, result)
	want := "run: repositories=2 discovery_failed=0 discovered=3 enqueued=2 queued=2 attempted=2 succeeded=2 failed=0\n" +
		"discovery: repository=acme/service status=success observed=2 enqueued=1\n" +
		"discovery: repository=acme/other status=success observed=1 enqueued=1\n" +
		"attempt: pull_request=github:acme/service#9 url=https://github.com/acme/service/pull/9 " +
		"status=success verdict=APPROVE review_url=https://github.com/acme/service/pull/9#pullrequestreview-9 recovered=false\n" +
		"attempt: pull_request=github:acme/service#7 url=https://github.com/acme/service/pull/7 " +
		"status=success verdict=REQUEST_CHANGES " +
		"review_url=https://github.com/acme/service/pull/7#pullrequestreview-7 recovered=true\n"
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
		if _, err := store.EnqueueMany(context.Background(), []PullRequest{pr}); err != nil {
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

func TestMainHumanInitShowsCreatedAndPreservedPaths(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(temp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(temp, "state"))
	config, _ := configPath()
	state, _ := statePath()
	for _, test := range []struct {
		want string
	}{
		{want: fmt.Sprintf("config: %s (created)\nstate: %s\n", config, state)},
		{want: fmt.Sprintf("config: %s (preserved)\nstate: %s\n", config, state)},
	} {
		var stdout, stderr bytes.Buffer
		if exit := Main([]string{"init"}, &stdout, &stderr); exit != 0 || stderr.String() != "" {
			t.Fatalf("init exit = %d, stderr = %q", exit, stderr.String())
		}
		if stdout.String() != test.want {
			t.Fatalf("init output = %q, want %q", stdout.String(), test.want)
		}
	}
}

func TestMainHumanBulkReviewShowsEachInputResult(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	first := "https://github.com/acme/service/pull/1"
	second := "https://github.com/acme/service/pull/2"
	var stdout, stderr bytes.Buffer
	if exit := Main([]string{"review", first}, &stdout, &stderr); exit != 0 {
		t.Fatalf("seed review exit = %d, stderr = %q", exit, stderr.String())
	}
	stdout.Reset()
	if exit := Main([]string{"bulk-review", first, second, first}, &stdout, &stderr); exit != 0 || stderr.String() != "" {
		t.Fatalf("bulk-review exit = %d, stderr = %q", exit, stderr.String())
	}
	want := "already_queued " + first + "\nqueued " + second + "\nalready_queued " + first + "\n"
	if stdout.String() != want {
		t.Fatalf("bulk-review output = %q, want %q", stdout.String(), want)
	}
}

func TestMainHumanStatusShowsEmptyAndBoundedPopulatedState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	var stdout, stderr bytes.Buffer
	if exit := Main([]string{"status"}, &stdout, &stderr); exit != 0 || stderr.String() != "" {
		t.Fatalf("empty status exit = %d, stderr = %q", exit, stderr.String())
	}
	if want := "run_active: false\nqueue:\n  (empty)\nhistory:\n  (empty)\n"; stdout.String() != want {
		t.Fatalf("empty status output = %q, want %q", stdout.String(), want)
	}

	path, _ := statePath()
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	queued, _ := ParsePullRequestURL("https://github.com/acme/service/pull/3")
	older, _ := ParsePullRequestURL("https://github.com/acme/service/pull/1")
	newer, _ := ParsePullRequestURL("https://github.com/acme/service/pull/2")
	if _, err := store.EnqueueMany(context.Background(), []PullRequest{queued}); err != nil {
		t.Fatal(err)
	}
	finished := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	if err := store.Finish(context.Background(), Attempt{
		PullRequest: older, StartedAt: finished.Add(-2 * time.Second), FinishedAt: finished.Add(-time.Second),
		Success: true, Verdict: "APPROVE", ReviewURL: older.URL + "#pullrequestreview-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(context.Background(), Attempt{
		PullRequest: newer, StartedAt: finished.Add(-time.Second), FinishedAt: finished,
		ErrorCode: "codex_failed", ErrorMessage: "codex failed",
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	stdout.Reset()
	if exit := Main([]string{"status", "--limit", "1"}, &stdout, &stderr); exit != 0 || stderr.String() != "" {
		t.Fatalf("populated status exit = %d, stderr = %q", exit, stderr.String())
	}
	want := "run_active: false\nqueue:\n  " + queued.URL + "\nhistory:\n  2026-08-20T12:00:00Z " + newer.URL +
		" failed error=codex_failed: codex failed\n"
	if stdout.String() != want {
		t.Fatalf("populated status output = %q, want %q", stdout.String(), want)
	}
	if strings.Contains(stdout.String(), older.URL) {
		t.Fatalf("status exceeded history limit: %q", stdout.String())
	}
}
