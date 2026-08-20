package reviewctl_test

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

	"github.com/denifilatoff/reviewctl/internal/reviewctl"
)

func TestFullProcessReviewAndRunCycle(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	temp := t.TempDir()
	binary := filepath.Join(temp, "reviewctl")
	build := exec.Command("go", "build", "-o", binary, "./cmd/reviewctl")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

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

	store, err := reviewctl.OpenStore(filepath.Join(stateHome, "reviewctl", "reviewctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	queue, _ := store.Snapshot(context.Background())
	history, _ := store.History(context.Background())
	if len(queue) != 1 || queue[0].Number != 8 || len(history) != 3 || history[2].ErrorCode != "untrusted_author" {
		t.Fatalf("unexpected durable state: queue=%+v history=%+v", queue, history)
	}
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
		os.Exit(0)
	case "codex":
		helperCodex(args)
	default:
		os.Exit(90)
	}
	os.Exit(0)
}

func helperGH(args []string) {
	if len(args) >= 3 && args[0] == "pr" && args[1] == "view" {
		number := int64(7)
		author := "dependabot[bot]"
		if strings.HasSuffix(args[2], "/8") {
			number, author = 8, "mallory"
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"url": args[2], "number": number, "state": "OPEN", "isDraft": false,
			"headRefOid": "0123456789abcdef0123456789abcdef01234567", "author": map[string]string{"login": author},
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
	receipt := reviewctl.Receipt{
		Provider: "github", Repository: fields["Repository"], Number: number, HeadSHA: fields["Expected head"],
		SkillDigest: fields["Skill digest"], Verdict: "APPROVE", ReviewID: "123",
		ReviewURL: fmt.Sprintf("https://github.com/%s/pull/%d#pullrequestreview-123", fields["Repository"], number),
		Recovered: recovered,
	}
	data, _ := json.Marshal(receipt)
	if receiptPath == "" || os.WriteFile(receiptPath, data, 0o600) != nil {
		os.Exit(95)
	}
	os.Stdout.Write(data)
}
