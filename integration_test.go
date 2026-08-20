package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFullProcessReviewAndRunCycle(t *testing.T) {
	root := "."
	temp := t.TempDir()
	binary := filepath.Join(temp, "reviewctl")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	fakeBin := installProcessHelpers(t, temp)

	configHome := filepath.Join(temp, "config")
	stateHome := filepath.Join(temp, "state")
	if err := os.MkdirAll(filepath.Join(configHome, "reviewctl"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := `harness: codex
publish: true
trusted_authors: ["dependabot[bot]"]
repositories:
  - provider: github
    repository: acme/service
`
	if err := os.WriteFile(filepath.Join(configHome, "reviewctl", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GO_WANT_REVIEWCTL_HELPER=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+configHome,
		"XDG_STATE_HOME="+stateHome,
		"REVIEWCTL_FAKE_STATE="+filepath.Join(temp, "published"),
		"REVIEWCTL_FAKE_GIT_STATE="+filepath.Join(temp, "git-fetch"),
	)

	firstURL := "https://github.com/acme/service/pull/7"
	result := runCLI(t, env, binary, "--json", "review", firstURL)
	if result.exitCode != 0 || result.stderr != "" || result.object["status"] != "queued" {
		t.Fatalf("enqueue result: %+v", result)
	}
	result = runCLI(t, env, binary, "--json", "review", firstURL)
	if result.exitCode != 0 || result.object["status"] != "already_queued" {
		t.Fatalf("duplicate result: %+v", result)
	}
	result = runCLI(t, env, binary, "--json", "review", "not-a-url")
	if result.exitCode != 2 || result.object["status"] != "error" || result.stderr != "" {
		t.Fatalf("invalid input result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(temp, "published")); !os.IsNotExist(err) {
		t.Fatalf("review command invoked Codex: %v", err)
	}

	result = runCLI(t, env, binary, "--json", "run")
	if result.exitCode != 0 || result.stderr != "" || result.object["status"] != "success" {
		t.Fatalf("first run result: %+v", result)
	}
	if !strings.Contains(result.stdout, `"recovered":false`) {
		t.Fatalf("first run did not report publication: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(temp, "published")); err != nil {
		t.Fatalf("Codex double did not publish: %v", err)
	}

	result = runCLI(t, env, binary, "--json", "review", firstURL)
	if result.exitCode != 0 {
		t.Fatalf("re-enqueue: %+v", result)
	}
	result = runCLI(t, env, binary, "--json", "run")
	if result.exitCode != 0 || !strings.Contains(result.stdout, `"recovered":true`) {
		t.Fatalf("marker recovery result: %+v", result)
	}

	failedURL := "https://github.com/acme/service/pull/8"
	result = runCLI(t, env, binary, "--json", "review", failedURL)
	if result.exitCode != 0 {
		t.Fatalf("enqueue untrusted fixture: %+v", result)
	}
	result = runCLI(t, env, binary, "--json", "run")
	if result.exitCode != 1 || result.object["status"] != "failed" || result.stderr != "" {
		t.Fatalf("failed run result: %+v", result)
	}

	store, err := OpenStore(filepath.Join(stateHome, "reviewctl", "reviewctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	queue, _ := store.Snapshot(context.Background())
	history, _ := store.History(context.Background())
	if len(queue) != 1 || queue[0].Number != 8 || len(history) != 3 || history[2].ErrorCode != "untrusted_author" {
		t.Fatalf("unexpected durable state: queue=%+v history=%+v", queue, history)
	}

	result = runCLI(t, env, binary, "--json", "review", firstURL)
	if result.exitCode != 0 {
		t.Fatalf("enqueue moving-head fixture: %+v", result)
	}
	headChangeEnv := append(append([]string{}, env...),
		"REVIEWCTL_FAKE_HEAD_COUNTER="+filepath.Join(temp, "head-counter"))
	result = runCLI(t, headChangeEnv, binary, "--json", "run")
	if result.exitCode != 1 || !strings.Contains(result.stdout, `"code":"head_changed"`) {
		t.Fatalf("moving head result: %+v", result)
	}
}

func TestFullProcessDiscoveryCycles(t *testing.T) {
	temp := t.TempDir()
	binary := filepath.Join(temp, "reviewctl")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	fakeBin := installProcessHelpers(t, temp)
	configHome := filepath.Join(temp, "config")
	if err := os.MkdirAll(filepath.Join(configHome, "reviewctl"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := `harness: codex
publish: false
trusted_authors: ["dependabot[bot]"]
repositories:
  - provider: github
    repository: acme/service
  - provider: github
    repository: acme/other
`
	if err := os.WriteFile(filepath.Join(configHome, "reviewctl", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	discoveryDir := filepath.Join(temp, "discovery")
	if err := os.Mkdir(discoveryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(temp, "gh-list-calls")
	env := append(os.Environ(),
		"GO_WANT_REVIEWCTL_HELPER=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+configHome,
		"XDG_STATE_HOME="+filepath.Join(temp, "state"),
		"XDG_RUNTIME_DIR="+filepath.Join(temp, "runtime"),
		"REVIEWCTL_FAKE_DISCOVERY_DIR="+discoveryDir,
		"REVIEWCTL_FAKE_GH_CALLS="+calls,
	)

	writeDiscoveryFixture(t, discoveryDir, "acme/service", `[
		{"url":"https://github.com/acme/service/pull/7","number":7,"state":"OPEN","isDraft":false,"headRefOid":"a"},
		{"url":"https://github.com/acme/service/pull/8","number":8,"state":"OPEN","isDraft":true,"headRefOid":"a"}
	]`)
	writeDiscoveryFixture(t, discoveryDir, "acme/other", `[
		{"url":"https://github.com/acme/other/pull/20","number":20,"state":"OPEN","isDraft":false,"headRefOid":"a"}
	]`)
	result := runCLI(t, env, binary, "--json", "run")
	assertRunCounts(t, result, 0, map[string]float64{
		"repositories": 2, "discovery_succeeded": 2, "discovery_failed": 0, "discovered": 3,
		"enqueued": 0, "queued": 0, "attempted": 0, "succeeded": 0, "failed": 0,
	})
	callData, err := os.ReadFile(calls)
	if err != nil || string(callData) != "acme/service\nacme/other\n" {
		t.Fatalf("gh list calls = %q, err = %v", callData, err)
	}

	writeDiscoveryFixture(t, discoveryDir, "acme/service", `[
		{"url":"https://github.com/acme/service/pull/10","number":10,"state":"OPEN","isDraft":false,"headRefOid":"a"},
		{"url":"https://github.com/acme/service/pull/9","number":9,"state":"OPEN","isDraft":true,"headRefOid":"a"},
		{"url":"https://github.com/acme/service/pull/8","number":8,"state":"OPEN","isDraft":false,"headRefOid":"a"},
		{"url":"https://github.com/acme/service/pull/7","number":7,"state":"OPEN","isDraft":false,"headRefOid":"a"}
	]`)
	result = runCLI(t, env, binary, "--json", "run")
	assertRunCounts(t, result, 1, map[string]float64{
		"repositories": 2, "discovery_succeeded": 2, "discovery_failed": 0, "discovered": 5,
		"enqueued": 2, "queued": 2, "attempted": 2, "succeeded": 0, "failed": 2,
	})
	attempts := result.object["results"].([]any)
	if attempts[0].(map[string]any)["number"] != float64(8) || attempts[1].(map[string]any)["number"] != float64(10) {
		t.Fatalf("attempt order = %#v, want pull requests 8 then 10", attempts)
	}

	writeDiscoveryFixture(t, discoveryDir, "acme/service", `[
		{"url":"https://github.com/acme/service/pull/7","number":7,"state":"OPEN","isDraft":false,"headRefOid":"a"},
		{"url":"https://github.com/acme/service/pull/8","number":8,"state":"OPEN","isDraft":false,"headRefOid":"a"},
		{"url":"https://github.com/acme/service/pull/9","number":9,"state":"OPEN","isDraft":true,"headRefOid":"b"},
		{"url":"https://github.com/acme/service/pull/10","number":10,"state":"OPEN","isDraft":false,"headRefOid":"a"}
	]`)
	result = runCLI(t, env, binary, "--json", "run")
	assertRunCounts(t, result, 1, map[string]float64{
		"repositories": 2, "discovery_succeeded": 2, "discovery_failed": 0, "discovered": 5,
		"enqueued": 0, "queued": 2, "attempted": 2, "succeeded": 0, "failed": 2,
	})

	writeDiscoveryFixture(t, discoveryDir, "acme/service", `[
		{"url":"https://github.com/acme/service/pull/7","number":7,"state":"OPEN","isDraft":false,"headRefOid":"b"},
		{"url":"https://github.com/acme/service/pull/8","number":8,"state":"OPEN","isDraft":false,"headRefOid":"a"},
		{"url":"https://github.com/acme/service/pull/9","number":9,"state":"OPEN","isDraft":true,"headRefOid":"b"},
		{"url":"https://github.com/acme/service/pull/10","number":10,"state":"OPEN","isDraft":false,"headRefOid":"a"}
	]`)
	failingEnv := append(append([]string{}, env...), "REVIEWCTL_FAKE_DISCOVERY_FAIL=acme/other")
	result = runCLI(t, failingEnv, binary, "--json", "run")
	assertRunCounts(t, result, 1, map[string]float64{
		"repositories": 2, "discovery_succeeded": 1, "discovery_failed": 1, "discovered": 4,
		"enqueued": 1, "queued": 3, "attempted": 3, "succeeded": 0, "failed": 3,
	})
	discovery, ok := result.object["discovery"].([]any)
	if !ok || len(discovery) != 2 || discovery[0].(map[string]any)["repository"] != "acme/service" ||
		discovery[1].(map[string]any)["repository"] != "acme/other" ||
		discovery[1].(map[string]any)["status"] != "failed" {
		t.Fatalf("discovery outcomes = %#v", result.object["discovery"])
	}
}

func TestGitHubDiscoverySentinelPreservesBaseline(t *testing.T) {
	temp := t.TempDir()
	fakeBin := installProcessHelpers(t, temp)
	t.Setenv("GO_WANT_REVIEWCTL_HELPER", "1")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	discoveryDir := filepath.Join(temp, "discovery")
	if err := os.Mkdir(discoveryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEWCTL_FAKE_DISCOVERY_DIR", discoveryDir)
	repository := Repository{Provider: "github", Repository: "acme/service"}
	first := fakeDiscoveryPR{
		URL: "https://github.com/acme/service/pull/1", Number: 1, State: "OPEN", HeadRefOID: "a",
	}
	writeDiscoverySnapshots(t, discoveryDir, repository.Repository, []fakeDiscoveryPR{first})
	store := openTestStore(t)
	cfg := Config{Repositories: []Repository{repository}}
	result := discoverRepositories(context.Background(), cfg, store)
	if len(result) != 1 || result[0].Status != "success" || result[0].Observed != 1 || result[0].Enqueued != 0 {
		t.Fatalf("first discovery = %+v", result)
	}

	sentinel := make([]fakeDiscoveryPR, 1001)
	for i := range sentinel {
		number := int64(i + 1)
		sentinel[i] = fakeDiscoveryPR{
			URL: fmt.Sprintf("https://github.com/acme/service/pull/%d", number), Number: number, State: "OPEN",
			IsDraft: number != 1, HeadRefOID: "a",
		}
	}
	sentinel[0].HeadRefOID = "b"
	writeDiscoverySnapshots(t, discoveryDir, repository.Repository, sentinel)
	result = discoverRepositories(context.Background(), cfg, store)
	if len(result) != 1 || result[0].Status != "failed" || result[0].Error == nil ||
		result[0].Error.Code != "github_snapshot_too_large" {
		t.Fatalf("sentinel discovery = %+v", result)
	}
	queue, err := store.Snapshot(context.Background())
	if err != nil || len(queue) != 0 {
		t.Fatalf("sentinel changed queue: queue=%+v err=%v", queue, err)
	}

	first.HeadRefOID = "b"
	writeDiscoverySnapshots(t, discoveryDir, repository.Repository, []fakeDiscoveryPR{first})
	result = discoverRepositories(context.Background(), cfg, store)
	if len(result) != 1 || result[0].Status != "success" || result[0].Enqueued != 1 {
		t.Fatalf("sentinel advanced baseline: discovery=%+v", result)
	}
}

func assertRunCounts(t *testing.T, result cliResult, wantExit int, wants map[string]float64) {
	t.Helper()
	if result.exitCode != wantExit || result.stderr != "" {
		t.Fatalf("run result: %+v", result)
	}
	for field, want := range wants {
		if got := result.object[field]; got != want {
			t.Fatalf("%s = %#v, want %v; result=%+v", field, got, want, result)
		}
	}
}

func writeDiscoveryFixture(t *testing.T, dir, repository, body string) {
	t.Helper()
	name := strings.ReplaceAll(repository, "/", "__") + ".json"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakeDiscoveryPR struct {
	URL        string `json:"url"`
	Number     int64  `json:"number"`
	State      string `json:"state"`
	IsDraft    bool   `json:"isDraft"`
	HeadRefOID string `json:"headRefOid"`
}

func writeDiscoverySnapshots(t *testing.T, dir, repository string, snapshot []fakeDiscoveryPR) {
	t.Helper()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	writeDiscoveryFixture(t, dir, repository, string(data))
}

func TestProcessAttemptBindsCommentToAuthenticatedLogin(t *testing.T) {
	for _, test := range []struct {
		name      string
		login     string
		wantError string
	}{
		{name: "self-authored", login: "dependabot[bot]"},
		{name: "ordinary", login: "reviewer", wantError: "receipt_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			temp := t.TempDir()
			fakeBin := installProcessHelpers(t, temp)
			t.Setenv("GO_WANT_REVIEWCTL_HELPER", "1")
			t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("REVIEWCTL_FAKE_STATE", filepath.Join(temp, "published"))
			t.Setenv("REVIEWCTL_FAKE_GIT_STATE", filepath.Join(temp, "git-fetch"))
			t.Setenv("REVIEWCTL_FAKE_LOGIN", test.login)
			t.Setenv("REVIEWCTL_FAKE_VERDICT", "COMMENT")

			pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
			cfg := Config{
				Harness: "codex", Publish: true, TrustedAuthors: []string{"dependabot[bot]"},
				Repositories: []Repository{{Provider: "github", Repository: "acme/service"}},
			}
			result := ProcessAttempt(context.Background(), cfg, pr)
			if test.wantError == "" && (!result.Success || result.Verdict != "COMMENT") {
				t.Fatalf("self-authored COMMENT failed: %+v", result)
			}
			if test.wantError != "" && (result.Success || result.ErrorCode != test.wantError) {
				t.Fatalf("ordinary COMMENT result: %+v", result)
			}
		})
	}
}

func installProcessHelpers(t *testing.T, temp string) string {
	t.Helper()
	fakeBin := filepath.Join(temp, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gh", "apm", "git", "codex"} {
		script := fmt.Sprintf("#!/bin/sh\nREVIEWCTL_HELPER_NAME=%s exec %q -test.run=TestHelperProcess -- \"$@\"\n", name, self)
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return fakeBin
}

type cliResult struct {
	exitCode int
	stdout   string
	stderr   string
	object   map[string]any
}

func runCLI(t *testing.T, env []string, binary string, args ...string) cliResult {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		exitCode = exitErr.ExitCode()
	}
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(stdout.String()))
	if err := decoder.Decode(&object); err != nil {
		t.Fatalf("decode one JSON result from %q: %v", stdout.String(), err)
	}
	if decoder.Decode(&map[string]any{}) != io.EOF {
		t.Fatalf("stdout contained more than one JSON object: %q", stdout.String())
	}
	return cliResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String(), object: object}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_REVIEWCTL_HELPER") != "1" {
		return
	}
	separator := 0
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	args := os.Args[separator:]
	switch os.Getenv("REVIEWCTL_HELPER_NAME") {
	case "gh":
		helperGH(args)
	case "apm":
		helperAPM(args)
	case "git":
		helperGit(args)
	case "codex":
		helperCodex(args)
	default:
		os.Exit(90)
	}
	os.Exit(0)
}

func helperGit(args []string) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	state := os.Getenv("REVIEWCTL_FAKE_GIT_STATE")
	if len(args) == 5 && args[0] == "-C" && args[2] == "fetch" && args[3] == "origin" &&
		args[4] == "refs/pull/7/head" {
		if os.WriteFile(state, []byte(head), 0o600) != nil {
			os.Exit(96)
		}
		return
	}
	if len(args) == 5 && args[0] == "-C" && args[2] == "checkout" && args[3] == "--detach" && args[4] == head {
		fetched, err := os.ReadFile(state)
		if err != nil || string(fetched) != head {
			os.Exit(97)
		}
		if os.Remove(state) != nil {
			os.Exit(97)
		}
		return
	}
	os.Exit(98)
}

func helperGH(args []string) {
	if len(args) == 10 && args[0] == "pr" && args[1] == "list" && args[2] == "--repo" &&
		args[4] == "--state" && args[5] == "open" && args[6] == "--limit" && args[7] == "1001" &&
		args[8] == "--json" && args[9] == "url,number,state,isDraft,headRefOid" {
		repository := args[3]
		if calls := os.Getenv("REVIEWCTL_FAKE_GH_CALLS"); calls != "" {
			file, err := os.OpenFile(calls, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				os.Exit(89)
			}
			fmt.Fprintln(file, repository)
			file.Close()
		}
		if repository == os.Getenv("REVIEWCTL_FAKE_DISCOVERY_FAIL") {
			fmt.Fprintln(os.Stderr, "scripted discovery failure")
			os.Exit(88)
		}
		dir := os.Getenv("REVIEWCTL_FAKE_DISCOVERY_DIR")
		if dir == "" {
			fmt.Println("[]")
			return
		}
		data, err := os.ReadFile(filepath.Join(dir, strings.ReplaceAll(repository, "/", "__")+".json"))
		if err != nil {
			os.Exit(87)
		}
		os.Stdout.Write(data)
		return
	}
	if len(args) == 4 && args[0] == "api" && args[1] == "user" && args[2] == "--jq" && args[3] == ".login" {
		login := os.Getenv("REVIEWCTL_FAKE_LOGIN")
		if login == "" {
			login = "reviewer"
		}
		fmt.Println(login)
		return
	}
	if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
		if dir := os.Getenv("REVIEWCTL_FAKE_DISCOVERY_DIR"); dir != "" {
			pr, err := ParsePullRequestURL(args[2])
			if err != nil {
				os.Exit(86)
			}
			data, err := os.ReadFile(filepath.Join(dir, strings.ReplaceAll(pr.Repository, "/", "__")+".json"))
			if err != nil {
				os.Exit(86)
			}
			var snapshot []fakeDiscoveryPR
			if json.Unmarshal(data, &snapshot) != nil {
				os.Exit(86)
			}
			for _, item := range snapshot {
				if item.Number == pr.Number {
					json.NewEncoder(os.Stdout).Encode(map[string]any{
						"url": item.URL, "number": item.Number, "state": item.State, "isDraft": item.IsDraft,
						"headRefOid": item.HeadRefOID, "author": map[string]string{"login": "dependabot[bot]"},
					})
					return
				}
			}
			os.Exit(86)
		}
		number := int64(7)
		author := "dependabot[bot]"
		head := "0123456789abcdef0123456789abcdef01234567"
		if strings.HasSuffix(args[2], "/8") {
			number, author = 8, "mallory"
		}
		if counterPath := os.Getenv("REVIEWCTL_FAKE_HEAD_COUNTER"); counterPath != "" && number == 7 {
			count := 0
			if data, err := os.ReadFile(counterPath); err == nil {
				count, _ = strconv.Atoi(string(data))
			}
			if os.WriteFile(counterPath, []byte(strconv.Itoa(count+1)), 0o600) != nil {
				os.Exit(99)
			}
			if count > 0 {
				head = "fedcba9876543210fedcba9876543210fedcba98"
			}
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"url": args[2], "number": number, "state": "OPEN", "isDraft": false,
			"headRefOid": head, "author": map[string]string{"login": author},
		})
		return
	}
	if len(args) >= 4 && args[0] == "repo" && args[1] == "clone" {
		if err := os.MkdirAll(args[3], 0o700); err != nil {
			os.Exit(91)
		}
		return
	}
	os.Exit(92)
}

func helperAPM(args []string) {
	for i := range args {
		if args[i] == "--root" && i+1 < len(args) {
			dir := filepath.Join(args[i+1], ".agents", "skills", "adversarial-code-review")
			if os.MkdirAll(dir, 0o700) != nil || os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("review"), 0o600) != nil {
				os.Exit(93)
			}
			return
		}
	}
	os.Exit(94)
}

func helperCodex(args []string) {
	receiptPath := ""
	for i := range args {
		if (args[i] == "-o" || args[i] == "--output-last-message") && i+1 < len(args) {
			receiptPath = args[i+1]
		}
	}
	instruction, _ := io.ReadAll(os.Stdin)
	fields := map[string]string{}
	for _, line := range strings.Split(string(instruction), "\n") {
		if key, value, ok := strings.Cut(line, ": "); ok {
			fields[key] = value
		}
	}
	number, _ := strconv.ParseInt(fields["Pull request"], 10, 64)
	state := os.Getenv("REVIEWCTL_FAKE_STATE")
	_, statErr := os.Stat(state)
	recovered := statErr == nil
	if !recovered {
		os.WriteFile(state, []byte("123"), 0o600)
	}
	verdict := os.Getenv("REVIEWCTL_FAKE_VERDICT")
	if verdict == "" {
		verdict = "APPROVE"
	}
	receipt := Receipt{
		Provider: "github", Repository: fields["Repository"], Number: number, HeadSHA: fields["Expected head"],
		SkillDigest: fields["Skill digest"], Verdict: verdict, ReviewID: "123",
		ReviewURL: fmt.Sprintf("https://github.com/%s/pull/%d#pullrequestreview-123", fields["Repository"], number),
		Recovered: recovered,
	}
	data, _ := json.Marshal(receipt)
	if receiptPath == "" || os.WriteFile(receiptPath, data, 0o600) != nil {
		os.Exit(95)
	}
	os.Stdout.Write(data)
}
