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
	"syscall"
	"time"
)

const Version = "0.1.0"

type resultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type runItem struct {
	PullRequest
	Status    string       `json:"status"`
	Verdict   string       `json:"verdict,omitempty"`
	ReviewURL string       `json:"review_url,omitempty"`
	Recovered bool         `json:"recovered"`
	Error     *resultError `json:"error,omitempty"`
}

type queueItem struct {
	PullRequest PullRequest `json:"pull_request"`
	Status      string      `json:"status"`
}

type discoveryItem struct {
	Provider   string       `json:"provider"`
	Repository string       `json:"repository"`
	Status     string       `json:"status"`
	Observed   int          `json:"observed"`
	Enqueued   int          `json:"enqueued"`
	Error      *resultError `json:"error,omitempty"`
}

type runResult struct {
	Command            string          `json:"command"`
	Status             string          `json:"status"`
	Repositories       int             `json:"repositories"`
	DiscoverySucceeded int             `json:"discovery_succeeded"`
	DiscoveryFailed    int             `json:"discovery_failed"`
	Discovered         int             `json:"discovered"`
	Enqueued           int             `json:"enqueued"`
	Discovery          []discoveryItem `json:"discovery"`
	Queued             int             `json:"queued"`
	Attempted          int             `json:"attempted"`
	Succeeded          int             `json:"succeeded"`
	Failed             int             `json:"failed"`
	Results            []runItem       `json:"results"`
}

func Main(args []string, stdout, stderr io.Writer) int {
	jsonMode := len(args) > 0 && args[0] == "--json"
	if jsonMode {
		args = args[1:]
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(stdout, Version)
		return 0
	}
	if len(args) == 0 {
		return writeFailure(stdout, stderr, jsonMode, "", 2, "invalid_invocation", "command is required")
	}
	switch args[0] {
	case "init":
		if len(args) != 1 {
			return writeFailure(stdout, stderr, jsonMode, "init", 2, "invalid_invocation", "init accepts no arguments")
		}
		return initCommand(stdout, stderr, jsonMode)
	case "review":
		if len(args) != 2 {
			return writeFailure(stdout, stderr, jsonMode, "review", 2, "invalid_invocation", "review requires exactly one pull request URL")
		}
		return reviewCommand(args[1], stdout, stderr, jsonMode)
	case "bulk-review":
		if len(args) < 2 {
			return writeFailure(stdout, stderr, jsonMode, "bulk-review", 2, "invalid_invocation", "bulk-review requires at least one pull request URL")
		}
		return bulkReviewCommand(args[1:], stdout, stderr, jsonMode)
	case "status":
		limit := 20
		if len(args) == 3 && args[1] == "--limit" {
			var err error
			limit, err = strconv.Atoi(args[2])
			if err != nil || limit < 1 {
				return writeFailure(stdout, stderr, jsonMode, "status", 2, "invalid_limit", "status limit must be a positive integer")
			}
		} else if len(args) != 1 {
			return writeFailure(stdout, stderr, jsonMode, "status", 2, "invalid_invocation", "status accepts only --limit <count>")
		}
		return statusCommand(limit, stdout, stderr, jsonMode)
	case "run":
		if len(args) != 1 {
			return writeFailure(stdout, stderr, jsonMode, "run", 2, "invalid_invocation", "run accepts no arguments")
		}
		return runCommandOnce(stdout, stderr, jsonMode)
	default:
		return writeFailure(stdout, stderr, jsonMode, args[0], 2, "invalid_invocation", "unknown command")
	}
}

func initCommand(stdout, stderr io.Writer, jsonMode bool) int {
	config, state, created, err := initializePaths()
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "init", 1, "init_failed", err.Error())
	}
	if !jsonMode {
		configState := "preserved"
		if created {
			configState = "created"
		}
		fmt.Fprintf(stdout, "config: %s (%s)\nstate: %s\n", config, configState, state)
		return 0
	}
	return writeResult(stdout, jsonMode, map[string]any{
		"command": "init", "status": "success", "config_path": config, "state_path": state, "config_created": created,
	})
}

func reviewCommand(raw string, stdout, stderr io.Writer, jsonMode bool) int {
	pr, err := ParsePullRequestURL(raw)
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "review", 2, "invalid_url", err.Error())
	}
	results, err := enqueuePullRequests([]PullRequest{pr})
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "review", 1, "state_failed", err.Error())
	}
	return writeResult(stdout, jsonMode, map[string]any{
		"command": "review", "status": results[0].Status, "pull_request": results[0].PullRequest,
	})
}

func bulkReviewCommand(raw []string, stdout, stderr io.Writer, jsonMode bool) int {
	pullRequests := make([]PullRequest, len(raw))
	for i := range raw {
		pr, err := ParsePullRequestURL(raw[i])
		if err != nil {
			return writeFailure(stdout, stderr, jsonMode, "bulk-review", 2, "invalid_url", err.Error())
		}
		pullRequests[i] = pr
	}
	results, err := enqueuePullRequests(pullRequests)
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "bulk-review", 1, "state_failed", err.Error())
	}
	if !jsonMode {
		for _, result := range results {
			fmt.Fprintf(stdout, "%s %s\n", result.Status, result.PullRequest.URL)
		}
		return 0
	}
	return writeResult(stdout, true, map[string]any{"command": "bulk-review", "status": "success", "results": results})
}

func enqueuePullRequests(pullRequests []PullRequest) ([]queueItem, error) {
	path, err := statePath()
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	added, err := store.EnqueueMany(context.Background(), pullRequests)
	if err != nil {
		return nil, err
	}
	results := make([]queueItem, len(pullRequests))
	for i, pr := range pullRequests {
		status := "already_queued"
		if added[i] {
			status = "queued"
		}
		results[i] = queueItem{PullRequest: pr, Status: status}
	}
	return results, nil
}

func statusCommand(limit int, stdout, stderr io.Writer, jsonMode bool) int {
	path, err := statePath()
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "status", 1, "state_failed", err.Error())
	}
	store, err := OpenStore(path)
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "status", 1, "state_failed", err.Error())
	}
	defer store.Close()
	queue, history, err := store.Status(context.Background(), limit)
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "status", 1, "state_failed", err.Error())
	}
	if !jsonMode {
		fmt.Fprintln(stdout, "queue:")
		if len(queue) == 0 {
			fmt.Fprintln(stdout, "  (empty)")
		}
		for _, pr := range queue {
			fmt.Fprintf(stdout, "  %s\n", pr.URL)
		}
		fmt.Fprintln(stdout, "history:")
		if len(history) == 0 {
			fmt.Fprintln(stdout, "  (empty)")
		}
		for _, attempt := range history {
			fmt.Fprintf(stdout, "  %s %s ", attempt.FinishedAt.UTC().Format(time.RFC3339Nano), attempt.URL)
			if attempt.Success {
				fmt.Fprint(stdout, "success")
				if attempt.Verdict != "" {
					fmt.Fprintf(stdout, " verdict=%s", attempt.Verdict)
				}
				if attempt.ReviewURL != "" {
					fmt.Fprintf(stdout, " review=%s", attempt.ReviewURL)
				}
			} else {
				fmt.Fprint(stdout, "failed")
				if attempt.ErrorCode != "" {
					fmt.Fprintf(stdout, " error=%s", attempt.ErrorCode)
				}
				if attempt.ErrorMessage != "" {
					fmt.Fprintf(stdout, ": %s", bounded(attempt.ErrorMessage, 512))
				}
			}
			fmt.Fprintln(stdout)
		}
		return 0
	}
	return writeResult(stdout, true, map[string]any{
		"command": "status", "status": "success", "queue": queue, "history": history,
	})
}

func runCommandOnce(stdout, stderr io.Writer, jsonMode bool) int {
	path, err := statePath()
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "run", 1, "state_failed", err.Error())
	}
	lock, alreadyRunning, err := acquireRunLock(lockPath(path))
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "run", 1, "lock_failed", err.Error())
	}
	if alreadyRunning {
		return writeResult(stdout, jsonMode, map[string]any{"command": "run", "status": "already_running"})
	}
	defer lock.Close()
	cfg, err := loadConfig()
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "run", 1, "config_invalid", err.Error())
	}
	store, err := OpenStore(path)
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "run", 1, "state_failed", err.Error())
	}
	defer store.Close()
	discovery := discoverRepositories(context.Background(), cfg, store)
	queue, err := store.Snapshot(context.Background())
	if err != nil {
		return writeFailure(stdout, stderr, jsonMode, "run", 1, "state_failed", err.Error())
	}
	result := runResult{
		Command: "run", Status: "success", Repositories: len(cfg.Repositories), Discovery: discovery,
		Queued: len(queue), Results: []runItem{},
	}
	for _, item := range discovery {
		result.Discovered += item.Observed
		result.Enqueued += item.Enqueued
		if item.Status == "success" {
			result.DiscoverySucceeded++
		} else {
			result.DiscoveryFailed++
		}
	}
	for _, pr := range queue {
		attempt := ProcessAttempt(context.Background(), cfg, pr)
		if err := store.Finish(context.Background(), attempt); err != nil {
			attempt.Success = false
			attempt.ErrorCode = "state_failed"
			attempt.ErrorMessage = bounded(err.Error(), 512)
		}
		item := runItem{PullRequest: pr, Status: "failed"}
		if attempt.Success {
			item.Status, item.Verdict, item.ReviewURL, item.Recovered = "success", attempt.Verdict, attempt.ReviewURL, attempt.Recovered
			result.Succeeded++
		} else {
			item.Error = &resultError{Code: attempt.ErrorCode, Message: bounded(attempt.ErrorMessage, 512)}
			result.Failed++
		}
		result.Results = append(result.Results, item)
		result.Attempted++
	}
	exitCode := 0
	if result.DiscoveryFailed > 0 || result.Failed > 0 {
		result.Status, exitCode = "failed", 1
	}
	if jsonMode {
		writeResult(stdout, true, result)
	} else {
		writeHumanRunResult(stdout, result)
	}
	return exitCode
}

func writeHumanRunResult(stdout io.Writer, result runResult) {
	fmt.Fprintf(stdout,
		"run: repositories=%d discovery_failed=%d discovered=%d enqueued=%d queued=%d attempted=%d succeeded=%d failed=%d\n",
		result.Repositories, result.DiscoveryFailed, result.Discovered, result.Enqueued, result.Queued,
		result.Attempted, result.Succeeded, result.Failed)
	for _, item := range result.Discovery {
		if item.Status == "success" {
			fmt.Fprintf(stdout, "discovery: repository=%s status=success observed=%d enqueued=%d\n",
				item.Repository, item.Observed, item.Enqueued)
			continue
		}
		code, message := humanError(item.Error)
		fmt.Fprintf(stdout, "discovery: repository=%s status=failed error=%s: %s\n",
			item.Repository, code, message)
	}
	for _, item := range result.Results {
		identity := fmt.Sprintf("%s:%s#%d", item.Provider, item.Repository, item.Number)
		if item.Status == "success" {
			fmt.Fprintf(stdout,
				"attempt: pull_request=%s url=%s status=success verdict=%s review_url=%s recovered=%t\n",
				identity, item.URL, item.Verdict, item.ReviewURL, item.Recovered)
			continue
		}
		code, message := humanError(item.Error)
		fmt.Fprintf(stdout, "attempt: pull_request=%s url=%s status=failed error=%s: %s\n",
			identity, item.URL, code, message)
	}
}

func humanError(err *resultError) (string, string) {
	if err == nil {
		return "operational_failure", "missing error detail"
	}
	message := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Message)
	return err.Code, bounded(message, 512)
}

func discoverRepositories(ctx context.Context, cfg Config, store *Store) []discoveryItem {
	type fetched struct {
		repository Repository
		snapshot   []pullRequestSnapshot
		err        error
	}
	fetchedSnapshots := make([]fetched, len(cfg.Repositories))
	for i, repository := range cfg.Repositories {
		snapshot, err := listGitHubPullRequests(ctx, repository)
		fetchedSnapshots[i] = fetched{repository: repository, snapshot: snapshot, err: err}
	}
	results := make([]discoveryItem, len(fetchedSnapshots))
	for i, fetched := range fetchedSnapshots {
		item := discoveryItem{
			Provider: fetched.repository.Provider, Repository: fetched.repository.Repository, Status: "success",
		}
		if fetched.err == nil {
			item.Observed = len(fetched.snapshot)
			item.Enqueued, fetched.err = store.ApplyDiscoverySnapshot(ctx, fetched.repository, fetched.snapshot)
		}
		if fetched.err != nil {
			item.Status = "failed"
			code := "state_failed"
			if coded, ok := fetched.err.(*codedError); ok {
				code = coded.code
			}
			item.Error = &resultError{Code: code, Message: bounded(fetched.err.Error(), 512)}
		}
		results[i] = item
	}
	return results
}

func writeFailure(stdout, stderr io.Writer, jsonMode bool, command string, exitCode int, code, message string) int {
	message = bounded(message, 512)
	if jsonMode {
		writeResult(stdout, true, map[string]any{
			"command": command, "status": "error", "error": resultError{Code: code, Message: message},
		})
	} else {
		fmt.Fprintf(stderr, "%s: %s\n", code, message)
	}
	return exitCode
}

func writeResult(stdout io.Writer, jsonMode bool, result any) int {
	if jsonMode {
		_ = json.NewEncoder(stdout).Encode(result)
		return 0
	}
	switch value := result.(type) {
	case map[string]any:
		fmt.Fprintln(stdout, value["status"])
	default:
		data, _ := json.Marshal(value)
		fmt.Fprintln(stdout, string(data))
	}
	return 0
}

type runLock struct{ file *os.File }

func lockPath(state string) string {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "reviewctl", "run.lock")
	}
	return filepath.Join(filepath.Dir(state), "run.lock")
}

func acquireRunLock(path string) (*runLock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return &runLock{file: file}, false, nil
}

func (l *runLock) Close() error {
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}

const helpText = `Usage:
  reviewctl [--json] init
  reviewctl [--json] review <pull-request-url>
  reviewctl [--json] bulk-review <pull-request-url>...
  reviewctl [--json] status [--limit <count>]
  reviewctl [--json] run
  reviewctl --help
  reviewctl --version

review updates only the local queue and never invokes Codex.
bulk-review validates and updates the local queue in one transaction.
status prints the pending queue and recent history.
run processes one queue snapshot and may publish GitHub reviews when publication is enabled.
`
