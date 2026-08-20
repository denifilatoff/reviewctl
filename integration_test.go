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
	"syscall"
	"testing"
	"time"
)

func TestRunLockKeepsOtherCommandsAvailable(t *testing.T) {
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
	config := "harness: codex\npublish: true\ntrusted_authors: [alice]\nrepositories:\n" +
		"  - provider: github\n    repository: acme/service\n"
	if err := os.WriteFile(filepath.Join(configHome, "reviewctl", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(temp, "discovery-ready")
	release := filepath.Join(temp, "discovery-release")
	env := append(os.Environ(),
		"GO_WANT_REVIEWCTL_HELPER=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+configHome,
		"XDG_STATE_HOME="+filepath.Join(temp, "state"),
		"XDG_RUNTIME_DIR="+filepath.Join(temp, "runtime"),
		"REVIEWCTL_FAKE_DISCOVERY_READY="+ready,
		"REVIEWCTL_FAKE_DISCOVERY_RELEASE="+release,
	)
	first := exec.Command(binary, "--json", "run")
	first.Env = env
	var firstStdout, firstStderr bytes.Buffer
	first.Stdout, first.Stderr = &firstStdout, &firstStderr
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, ready)

	competing := runCLI(t, env, binary, "--json", "run")
	if competing.exitCode != 0 || competing.stderr != "" || competing.object["status"] != "already_running" {
		t.Fatalf("competing run = %+v", competing)
	}
	producer := runCLI(t, env, binary, "--json", "review", "https://github.com/acme/service/pull/8")
	if producer.exitCode != 0 || producer.stderr != "" || producer.object["status"] != "queued" {
		t.Fatalf("producer = %+v", producer)
	}
	status := runCLI(t, env, binary, "--json", "status")
	if status.exitCode != 0 || status.stderr != "" || status.object["run_active"] != true {
		t.Fatalf("active status = %+v", status)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err == nil {
		t.Fatalf("first run unexpectedly succeeded: stdout=%q stderr=%q", firstStdout.String(), firstStderr.String())
	}
	status = runCLI(t, env, binary, "--json", "status")
	if status.object["run_active"] != false {
		t.Fatalf("idle status = %+v", status)
	}
}

func TestAttemptTimeoutKillsCodexDescendantAndRetainsQueue(t *testing.T) {
	testInterruptedAttempt(t, 10*time.Second, false, false, "attempt_timeout")
}

func TestAttemptTimeoutBoundsPreCodexDescendant(t *testing.T) {
	testInterruptedAttempt(t, 10*time.Second, false, true, "apm_failed")
}

func TestRawGitHubCommandsCleanNonzeroDescendants(t *testing.T) {
	for _, target := range []string{"view", "login"} {
		t.Run(target, func(t *testing.T) {
			temp := t.TempDir()
			fakeBin := installProcessHelpers(t, temp)
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(temp, "gh-ready")
			heartbeatPath := filepath.Join(temp, "gh-descendant-heartbeat")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, self, "-test.run=TestRawGitHubCommandProcess")
			command.Env = append(os.Environ(),
				"GO_WANT_REVIEWCTL_HELPER=1",
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"REVIEWCTL_FAKE_RAW_GITHUB_TARGET="+target,
				"REVIEWCTL_FAKE_RAW_GITHUB_READY="+ready,
				"REVIEWCTL_FAKE_DESCENDANT_SURVIVED="+heartbeatPath,
			)
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			started := time.Now()
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waitForPath(t, ready)
			waitForPath(t, heartbeatPath)
			pidData, err := os.ReadFile(ready)
			if err != nil {
				t.Fatal(err)
			}
			descendantPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })
			if err := command.Wait(); err != nil {
				t.Fatalf("raw GitHub command process failed: %v\n%s", err, output.Bytes())
			}
			if ctx.Err() != nil || time.Since(started) >= 8*time.Second {
				t.Fatalf("raw GitHub command was not bounded: elapsed=%s context=%v", time.Since(started), ctx.Err())
			}
			heartbeat, err := os.ReadFile(heartbeatPath)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(300 * time.Millisecond)
			after, err := os.ReadFile(heartbeatPath)
			if err != nil || !bytes.Equal(heartbeat, after) {
				t.Fatalf("raw GitHub descendant survived cleanup: before=%q after=%q err=%v", heartbeat, after, err)
			}
		})
	}
}

func TestCancellationKillsCodexDescendantAndRetainsQueue(t *testing.T) {
	testInterruptedAttempt(t, 30*time.Second, true, false, "attempt_canceled")
}

func TestCodexDescriptorLeakIsBounded(t *testing.T) {
	testCodexDescriptorLeak(t, false, "codex_failed")
}

func TestCodexDescriptorLeakCleanupSurvivesCancellation(t *testing.T) {
	testCodexDescriptorLeak(t, true, "attempt_canceled")
}

func testCodexDescriptorLeak(t *testing.T, cancelDuringWait bool, wantCode string) {
	t.Helper()
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
	config := "harness: codex\npublish: true\nattempt_timeout: 30s\ntrusted_authors: [\"dependabot[bot]\"]\n" +
		"repositories:\n  - provider: github\n    repository: acme/service\n"
	if err := os.WriteFile(filepath.Join(configHome, "reviewctl", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(temp, "codex-leak-ready")
	leaderExited := filepath.Join(temp, "codex-leader-exited")
	heartbeatPath := filepath.Join(temp, "descendant-heartbeat")
	env := append(os.Environ(),
		"GO_WANT_REVIEWCTL_HELPER=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+configHome,
		"XDG_STATE_HOME="+filepath.Join(temp, "state"),
		"XDG_RUNTIME_DIR="+filepath.Join(temp, "runtime"),
		"REVIEWCTL_FAKE_GIT_STATE="+filepath.Join(temp, "git-fetch"),
		"REVIEWCTL_FAKE_CODEX_LEAK_READY="+ready,
		"REVIEWCTL_FAKE_CODEX_LEADER_EXITED="+leaderExited,
		"REVIEWCTL_FAKE_DESCENDANT_SURVIVED="+heartbeatPath,
	)
	if queued := runCLI(t, env, binary, "--json", "review", "https://github.com/acme/service/pull/7"); queued.exitCode != 0 {
		t.Fatalf("enqueue = %+v", queued)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--json", "run")
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	started := time.Now()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, ready)
	waitForPath(t, heartbeatPath)
	readyData, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSpace(string(readyData)), "\n")
	if len(parts) != 2 {
		t.Fatalf("ready data = %q", readyData)
	}
	groupID, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-groupID, syscall.SIGKILL) })
	if cancelDuringWait {
		waitForPath(t, leaderExited)
		if err := command.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
	}
	err = command.Wait()
	if ctx.Err() != nil {
		t.Fatalf("run exceeded the outer bound: %v", ctx.Err())
	}
	if time.Since(started) >= 15*time.Second {
		t.Fatalf("run was not bounded: %s", time.Since(started))
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 || stderr.String() != "" {
		t.Fatalf("run err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	results := result["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["error"].(map[string]any)["code"] != wantCode {
		t.Fatalf("run result = %#v", result)
	}
	if _, err := os.Stat(parts[0]); !os.IsNotExist(err) {
		t.Fatalf("attempt workspace was not removed: %v", err)
	}
	store, err := OpenStore(filepath.Join(temp, "state", "reviewctl", "reviewctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	queue, history, statusError := store.Status(context.Background(), 1)
	if statusError != nil || len(queue) != 1 || len(history) != 1 ||
		history[0].ErrorCode != wantCode || len(history[0].ErrorMessage) > 512 {
		t.Fatalf("queue=%+v history=%+v status_err=%v", queue, history, statusError)
	}
	heartbeat, err := os.ReadFile(heartbeatPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	after, err := os.ReadFile(heartbeatPath)
	if err != nil || !bytes.Equal(heartbeat, after) {
		t.Fatalf("Codex descendant survived WaitDelay cleanup: before=%q after=%q err=%v", heartbeat, after, err)
	}
}

func testInterruptedAttempt(t *testing.T, timeout time.Duration, cancelRun, preCodex bool, wantCode string) {
	t.Helper()
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
	config := fmt.Sprintf("harness: codex\npublish: true\nattempt_timeout: %s\ntrusted_authors: [\"dependabot[bot]\"]\nrepositories:\n  - provider: github\n    repository: acme/service\n", timeout)
	if err := os.WriteFile(filepath.Join(configHome, "reviewctl", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(temp, "codex-ready")
	survived := filepath.Join(temp, "descendant-survived")
	readyEnvironment := "REVIEWCTL_FAKE_CODEX_READY=" + ready
	if preCodex {
		readyEnvironment = "REVIEWCTL_FAKE_APM_READY=" + ready
	}
	env := append(os.Environ(),
		"GO_WANT_REVIEWCTL_HELPER=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+configHome,
		"XDG_STATE_HOME="+filepath.Join(temp, "state"),
		"XDG_RUNTIME_DIR="+filepath.Join(temp, "runtime"),
		"REVIEWCTL_FAKE_GIT_STATE="+filepath.Join(temp, "git-fetch"),
		readyEnvironment,
		"REVIEWCTL_FAKE_DESCENDANT_SURVIVED="+survived,
	)
	queued := runCLI(t, env, binary, "--json", "review", "https://github.com/acme/service/pull/7")
	if queued.exitCode != 0 {
		t.Fatalf("enqueue = %+v", queued)
	}
	outerContext, cancelOuter := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancelOuter()
	command := exec.CommandContext(outerContext, binary, "--json", "run")
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	started := time.Now()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, ready)
	waitForPath(t, survived)
	workspaceData, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	readyParts := strings.Split(strings.TrimSpace(string(workspaceData)), "\n")
	if readyParts[0] == "" || len(readyParts) > 2 {
		t.Fatalf("ready data = %q", workspaceData)
	}
	if len(readyParts) == 2 {
		descendantPID, err := strconv.Atoi(readyParts[1])
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })
	}
	if cancelRun {
		if err := command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	}
	err = command.Wait()
	if outerContext.Err() != nil {
		t.Fatalf("run exceeded the outer bound: %v", outerContext.Err())
	}
	if elapsed := time.Since(started); elapsed >= timeout+4*time.Second {
		t.Fatalf("run was not bounded: %s", elapsed)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || stderr.String() != "" {
		t.Fatalf("run err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	results, _ := result["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["error"].(map[string]any)["code"] != wantCode {
		t.Fatalf("run result = %#v", result)
	}
	if _, err := os.Stat(readyParts[0]); !os.IsNotExist(err) {
		t.Fatalf("attempt workspace was not removed: %v", err)
	}
	store, err := OpenStore(filepath.Join(temp, "state", "reviewctl", "reviewctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	queue, history, _ := store.Status(context.Background(), 2)
	if len(queue) != 1 || len(history) != 1 || history[0].ErrorCode != wantCode || len(history[0].ErrorMessage) > 512 {
		t.Fatalf("queue=%+v history=%+v", queue, history)
	}
	if preCodex && history[0].ErrorMessage != "apm failed: exit status 75: " {
		t.Fatalf("APM failure message = %q", history[0].ErrorMessage)
	}
	heartbeat, err := os.ReadFile(survived)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	after, err := os.ReadFile(survived)
	if err != nil || !bytes.Equal(heartbeat, after) {
		t.Fatalf("external command descendant survived process-group termination: before=%q after=%q err=%v",
			heartbeat, after, err)
	}
}

func TestDoctorReportsSequentialPrerequisites(t *testing.T) {
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
	config := "harness: codex\npublish: true\ntrusted_authors: [alice]\nrepositories:\n" +
		"  - provider: github\n    repository: acme/service\n" +
		"  - provider: github\n    repository: acme/other\n"
	configFile := filepath.Join(configHome, "reviewctl", "config.yaml")
	if err := os.WriteFile(configFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GO_WANT_REVIEWCTL_HELPER=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+configHome,
		"XDG_STATE_HOME="+filepath.Join(temp, "state"),
		"XDG_CACHE_HOME="+filepath.Join(temp, "cache"),
		"XDG_RUNTIME_DIR="+filepath.Join(temp, "runtime"),
		"REVIEWCTL_FAKE_DOCTOR_GITHUB_CALLS="+filepath.Join(temp, "github-calls"),
	)
	result := runCLI(t, env, binary, "--json", "doctor")
	checks, _ := result.object["prerequisites"].([]any)
	wantNames := []string{"config", "github", "codex", "apm", "state", "paths"}
	if result.exitCode != 0 || result.stderr != "" || result.object["status"] != "success" ||
		len(checks) != len(wantNames) {
		t.Fatalf("doctor success = %+v", result)
	}
	for i, want := range wantNames {
		check := checks[i].(map[string]any)
		if check["prerequisite"] != want || check["ready"] != true {
			t.Fatalf("check %d = %#v", i, check)
		}
	}
	githubCalls, err := os.ReadFile(filepath.Join(temp, "github-calls"))
	if err != nil || string(githubCalls) != "auth\nrepo:acme/service\nrepo:acme/other\n" {
		t.Fatalf("GitHub doctor calls = %q, err = %v", githubCalls, err)
	}

	if err := os.WriteFile(configFile, []byte("not: valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateHome := filepath.Join(temp, "blocked-state")
	runtimeHome := filepath.Join(temp, "blocked-runtime")
	if err := os.WriteFile(stateHome, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeHome, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failingEnv := append(append([]string{}, env...),
		"XDG_STATE_HOME="+stateHome,
		"XDG_RUNTIME_DIR="+runtimeHome,
		"REVIEWCTL_FAKE_DOCTOR_GITHUB_FAIL=1",
		"REVIEWCTL_FAKE_DOCTOR_CODEX_FAIL=1",
		"REVIEWCTL_FAKE_DOCTOR_APM_FAIL=1",
	)
	result = runCLI(t, failingEnv, binary, "--json", "doctor")
	checks, _ = result.object["prerequisites"].([]any)
	if result.exitCode != 1 || result.stderr != "" || result.object["status"] != "failed" ||
		len(checks) != len(wantNames) {
		t.Fatalf("doctor failure = %+v", result)
	}
	for i, want := range wantNames {
		check := checks[i].(map[string]any)
		errorObject, _ := check["error"].(map[string]any)
		if check["prerequisite"] != want || check["ready"] != false || errorObject["code"] == "" ||
			len(errorObject["message"].(string)) > 512 {
			t.Fatalf("failed check %d = %#v", i, check)
		}
	}
	human := exec.Command(binary, "doctor")
	human.Env = failingEnv
	var stdout, stderr bytes.Buffer
	human.Stdout, human.Stderr = &stdout, &stderr
	if err := human.Run(); err == nil || stderr.String() != "" {
		t.Fatalf("human doctor err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	for _, prerequisite := range wantNames {
		if !strings.Contains(stdout.String(), prerequisite+": failed") {
			t.Fatalf("human output does not identify %s: %q", prerequisite, stdout.String())
		}
	}
	invalid := runCLI(t, env, binary, "--json", "doctor", "extra")
	if invalid.exitCode != 2 || invalid.object["error"].(map[string]any)["code"] != "invalid_invocation" {
		t.Fatalf("invalid doctor = %+v", invalid)
	}
}

func TestDoctorMarksGitHubUnavailableWithoutValidConfig(t *testing.T) {
	temp := t.TempDir()
	binary := filepath.Join(temp, "reviewctl")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	fakeBin := installProcessHelpers(t, temp)
	for _, test := range []struct {
		name   string
		config string
	}{
		{name: "missing"},
		{name: "invalid", config: "not: valid\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(temp, test.name)
			configHome := filepath.Join(root, "config")
			githubCalls := filepath.Join(root, "github-calls")
			if test.config != "" {
				if err := os.MkdirAll(filepath.Join(configHome, "reviewctl"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(configHome, "reviewctl", "config.yaml"), []byte(test.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := append(os.Environ(),
				"GO_WANT_REVIEWCTL_HELPER=1",
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"XDG_CONFIG_HOME="+configHome,
				"XDG_STATE_HOME="+filepath.Join(root, "state"),
				"XDG_CACHE_HOME="+filepath.Join(root, "cache"),
				"XDG_RUNTIME_DIR="+filepath.Join(root, "runtime"),
				"REVIEWCTL_FAKE_DOCTOR_GITHUB_CALLS="+githubCalls,
			)
			result := runCLI(t, env, binary, "--json", "doctor")
			if result.exitCode != 1 || result.stderr != "" || result.object["status"] != "failed" {
				t.Fatalf("doctor = %+v", result)
			}
			var configCheck, githubCheck map[string]any
			for _, raw := range result.object["prerequisites"].([]any) {
				check := raw.(map[string]any)
				switch check["prerequisite"] {
				case "config":
					configCheck = check
				case "github":
					githubCheck = check
				}
			}
			githubError, githubFailed := githubCheck["error"].(map[string]any)
			if configCheck["ready"] != false || githubCheck["ready"] != false || !githubFailed ||
				githubError["code"] != "config_unavailable" ||
				!strings.Contains(fmt.Sprint(githubError["message"]), "configured repository access was not checked") {
				t.Fatalf("config=%#v github=%#v", configCheck, githubCheck)
			}
			human := exec.Command(binary, "doctor")
			human.Env = env
			var stdout, stderr bytes.Buffer
			human.Stdout, human.Stderr = &stdout, &stderr
			if err := human.Run(); err == nil || stderr.String() != "" ||
				!strings.Contains(stdout.String(), "github: failed (config_unavailable):") ||
				!strings.Contains(stdout.String(), "configured repository access was not checked") {
				t.Fatalf("human doctor err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(githubCalls); !os.IsNotExist(err) {
				t.Fatalf("GitHub doctor commands ran without valid config: %v", err)
			}
		})
	}
}

func TestLaunchdScheduledRunSmoke(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil is required for the macOS launchd smoke test")
	}
	convert := exec.Command(plutil, "-convert", "json", "-o", "-", "docs/com.denifilatoff.reviewctl.plist")
	data, err := convert.Output()
	if err != nil {
		t.Fatalf("decode launchd plist: %v", err)
	}
	var plist struct {
		Label                string            `json:"Label"`
		ProgramArguments     []string          `json:"ProgramArguments"`
		EnvironmentVariables map[string]string `json:"EnvironmentVariables"`
		StartInterval        int               `json:"StartInterval"`
	}
	if err := json.Unmarshal(data, &plist); err != nil {
		t.Fatal(err)
	}
	if plist.Label != "com.denifilatoff.reviewctl" || plist.StartInterval < 1 ||
		len(plist.ProgramArguments) != 3 || plist.ProgramArguments[1] != "--json" ||
		plist.ProgramArguments[2] != "run" {
		t.Fatalf("scheduled invocation = %+v", plist)
	}
	wantEnvironment := map[string]string{
		"XDG_CONFIG_HOME": "/Users/you/.config",
		"XDG_STATE_HOME":  "/Users/you/.local/state",
		"XDG_CACHE_HOME":  "/Users/you/.cache",
		"XDG_RUNTIME_DIR": "/Users/you/.local/state",
		"PATH":            "/Users/you/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
	}
	if len(plist.EnvironmentVariables) != len(wantEnvironment) {
		t.Fatalf("launchd environment = %#v", plist.EnvironmentVariables)
	}
	for name, want := range wantEnvironment {
		if got := plist.EnvironmentVariables[name]; got != want {
			t.Fatalf("launchd %s = %q, want %q", name, got, want)
		}
	}
	if plist.EnvironmentVariables["XDG_RUNTIME_DIR"] != plist.EnvironmentVariables["XDG_STATE_HOME"] ||
		filepath.Join(plist.EnvironmentVariables["XDG_STATE_HOME"], "reviewctl", "reviewctl.db") !=
			"/Users/you/.local/state/reviewctl/reviewctl.db" ||
		filepath.Join(plist.EnvironmentVariables["XDG_RUNTIME_DIR"], "reviewctl", "run.lock") !=
			"/Users/you/.local/state/reviewctl/run.lock" {
		t.Fatalf("launchd state and lock paths diverge: %#v", plist.EnvironmentVariables)
	}

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
	config := "harness: codex\npublish: true\ntrusted_authors: [alice]\nrepositories:\n" +
		"  - provider: github\n    repository: acme/service\n"
	if err := os.WriteFile(filepath.Join(configHome, "reviewctl", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GO_WANT_REVIEWCTL_HELPER=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+configHome,
		"XDG_STATE_HOME="+filepath.Join(temp, "state"),
		"XDG_RUNTIME_DIR="+filepath.Join(temp, "runtime"),
	)
	result := runCLI(t, env, binary, plist.ProgramArguments[1:]...)
	if result.exitCode != 0 || result.stderr != "" || result.object["command"] != "run" ||
		result.object["status"] != "success" {
		t.Fatalf("scheduled run = %+v", result)
	}
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

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
	queue, history, _ := store.Status(context.Background(), 3)
	if len(queue) != 1 || queue[0].Number != 8 || len(history) != 3 || history[0].ErrorCode != "untrusted_author" {
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

func TestAgentStyleBlackBoxAcceptance(t *testing.T) {
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
`
	if err := os.WriteFile(filepath.Join(configHome, "reviewctl", "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	discoveryDir := filepath.Join(temp, "discovery")
	if err := os.Mkdir(discoveryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDiscoveryFixture(t, discoveryDir, "acme/service", "[]")
	cwd := filepath.Join(temp, "caller")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"GO_WANT_REVIEWCTL_HELPER=1",
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+configHome,
		"XDG_STATE_HOME="+filepath.Join(temp, "state"),
		"XDG_CACHE_HOME="+filepath.Join(temp, "cache"),
		"XDG_RUNTIME_DIR="+filepath.Join(temp, "runtime"),
		"REVIEWCTL_FAKE_DISCOVERY_DIR="+discoveryDir,
	)

	help := exec.Command(binary, "--help")
	help.Dir, help.Env = cwd, env
	helpOutput, err := help.CombinedOutput()
	if err != nil || !bytes.Contains(helpOutput, []byte("init creates")) ||
		!bytes.Contains(helpOutput, []byte("doctor checks")) ||
		!bytes.Contains(helpOutput, []byte("review updates only the local queue")) ||
		!bytes.Contains(helpOutput, []byte("run processes one queue snapshot and may publish GitHub reviews")) {
		t.Fatalf("help err=%v output=%q", err, helpOutput)
	}

	if result := runCLIIn(t, cwd, env, binary, "--json", "init"); result.exitCode != 0 ||
		result.object["config_created"] != false || result.stderr != "" {
		t.Fatalf("init = %+v", result)
	}
	if result := runCLIIn(t, cwd, env, binary, "--json", "doctor"); result.exitCode != 0 ||
		result.object["status"] != "success" || len(result.object["prerequisites"].([]any)) != 6 || result.stderr != "" {
		t.Fatalf("doctor = %+v", result)
	}

	first := "https://github.com/acme/service/pull/7"
	second := "https://github.com/acme/service/pull/8"
	if result := runCLIIn(t, cwd, env, binary, "--json", "review", first); result.exitCode != 0 ||
		result.object["status"] != "queued" || result.stderr != "" {
		t.Fatalf("review = %+v", result)
	}
	if result := runCLIIn(t, cwd, env, binary, "--json", "review", first); result.exitCode != 0 ||
		result.object["status"] != "already_queued" {
		t.Fatalf("duplicate review = %+v", result)
	}
	bulk := runCLIIn(t, cwd, env, binary, "--json", "bulk-review", second, first)
	bulkResults := bulk.object["results"].([]any)
	if bulk.exitCode != 0 || bulk.stderr != "" || len(bulkResults) != 2 ||
		bulkResults[0].(map[string]any)["status"] != "queued" ||
		bulkResults[1].(map[string]any)["status"] != "already_queued" {
		t.Fatalf("bulk-review = %+v", bulk)
	}
	status := runCLIIn(t, cwd, env, binary, "--json", "status", "--limit", "1")
	queue := status.object["queue"].([]any)
	if status.exitCode != 0 || status.stderr != "" || len(queue) != 2 ||
		queue[0].(map[string]any)["number"] != float64(7) || queue[1].(map[string]any)["number"] != float64(8) ||
		len(status.object["history"].([]any)) != 0 || status.object["run_active"] != false {
		t.Fatalf("initial status = %+v", status)
	}
	unchangedStatus := runCLIIn(t, cwd, env, binary, "--json", "status", "--limit", "1")
	if unchangedStatus.stdout != status.stdout {
		t.Fatalf("unchanged status bytes differ:\nfirst:  %q\nsecond: %q", status.stdout, unchangedStatus.stdout)
	}

	run := runCLIIn(t, cwd, env, binary, "--json", "run")
	if run.exitCode != 1 || run.stderr != "" || run.object["status"] != "failed" ||
		run.object["discovery_succeeded"] != float64(1) || run.object["attempted"] != float64(2) ||
		len(run.object["results"].([]any)) != 2 {
		t.Fatalf("run = %+v", run)
	}
	status = runCLIIn(t, cwd, env, binary, "--json", "status", "--limit", "1")
	if len(status.object["queue"].([]any)) != 2 || len(status.object["history"].([]any)) != 1 {
		t.Fatalf("bounded retryable status = %+v", status)
	}

	invalid := runCLIIn(t, cwd, env, binary, "--json", "review", "not-a-url")
	if invalid.exitCode != 2 || invalid.stderr != "" || invalid.object["status"] != "error" {
		t.Fatalf("invalid JSON use = %+v", invalid)
	}
	human := exec.Command(binary, "review", "not-a-url")
	human.Dir, human.Env = cwd, env
	var stdout, stderr bytes.Buffer
	human.Stdout, human.Stderr = &stdout, &stderr
	if err := human.Run(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "invalid_url:") {
		t.Fatalf("human invalid use err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
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
	return runCLIIn(t, "", env, binary, args...)
}

func runCLIIn(t *testing.T, dir string, env []string, binary string, args ...string) cliResult {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Dir = dir
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

func TestRawGitHubCommandProcess(t *testing.T) {
	target := os.Getenv("REVIEWCTL_FAKE_RAW_GITHUB_TARGET")
	if target == "" {
		return
	}
	pr, _ := ParsePullRequestURL("https://github.com/acme/service/pull/7")
	var err error
	if target == "view" {
		_, err = resolveGitHub(context.Background(), pr)
	} else {
		_, err = resolveGitHubLogin(context.Background())
	}
	var coded *codedError
	if !errors.As(err, &coded) || coded.code != "github_failed" ||
		err.Error() != "gh failed: exit status 76: scripted raw GitHub failure" {
		t.Fatalf("raw GitHub error = %v", err)
	}
}

func TestCodexDescendantProcess(t *testing.T) {
	if os.Getenv("GO_WANT_REVIEWCTL_DESCENDANT") != "1" {
		return
	}
	leaderPID, _ := strconv.Atoi(os.Getenv("REVIEWCTL_FAKE_CODEX_LEADER_PID"))
	leaderExited := false
	for count := 0; ; count++ {
		if !leaderExited && leaderPID != 0 && os.Getppid() != leaderPID {
			if err := os.WriteFile(os.Getenv("REVIEWCTL_FAKE_CODEX_LEADER_EXITED"), nil, 0o600); err != nil {
				os.Exit(78)
			}
			leaderExited = true
		}
		if os.WriteFile(os.Getenv("REVIEWCTL_FAKE_DESCENDANT_SURVIVED"), []byte(strconv.Itoa(count)), 0o600) != nil {
			os.Exit(78)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
	rawTarget := os.Getenv("REVIEWCTL_FAKE_RAW_GITHUB_TARGET")
	rawView := rawTarget == "view" && len(args) == 5 && args[0] == "pr" && args[1] == "view" &&
		args[2] == "https://github.com/acme/service/pull/7" &&
		args[3] == "--json" && args[4] == "url,number,state,isDraft,headRefOid,author"
	rawLogin := rawTarget == "login" && len(args) == 4 && args[0] == "api" && args[1] == "user" &&
		args[2] == "--jq" && args[3] == ".login"
	if rawView || rawLogin {
		descendant := exec.Command(os.Args[0], "-test.run=TestCodexDescendantProcess")
		descendant.Env = append(os.Environ(), "GO_WANT_REVIEWCTL_DESCENDANT=1")
		descendant.Stdout, descendant.Stderr = os.Stdout, os.Stderr
		if descendant.Start() != nil || os.WriteFile(os.Getenv("REVIEWCTL_FAKE_RAW_GITHUB_READY"),
			[]byte(strconv.Itoa(descendant.Process.Pid)), 0o600) != nil {
			os.Exit(76)
		}
		fmt.Fprintln(os.Stderr, "scripted raw GitHub failure")
		os.Exit(76)
	}
	if len(args) == 2 && args[0] == "auth" && args[1] == "status" {
		appendDoctorGitHubCall("auth")
		if os.Getenv("REVIEWCTL_FAKE_DOCTOR_GITHUB_FAIL") != "" {
			fmt.Fprintln(os.Stderr, "scripted GitHub failure")
			os.Exit(76)
		}
		return
	}
	if len(args) == 5 && args[0] == "repo" && args[1] == "view" && args[3] == "--json" &&
		args[4] == "nameWithOwner" {
		appendDoctorGitHubCall("repo:" + args[2])
		fmt.Printf("{\"nameWithOwner\":%q}\n", args[2])
		return
	}
	if len(args) == 10 && args[0] == "pr" && args[1] == "list" && args[2] == "--repo" &&
		args[4] == "--state" && args[5] == "open" && args[6] == "--limit" && args[7] == "1001" &&
		args[8] == "--json" && args[9] == "url,number,state,isDraft,headRefOid" {
		repository := args[3]
		if ready := os.Getenv("REVIEWCTL_FAKE_DISCOVERY_READY"); ready != "" {
			if os.WriteFile(ready, nil, 0o600) != nil {
				os.Exit(89)
			}
			for {
				if _, err := os.Stat(os.Getenv("REVIEWCTL_FAKE_DISCOVERY_RELEASE")); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
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

func appendDoctorGitHubCall(call string) {
	path := os.Getenv("REVIEWCTL_FAKE_DOCTOR_GITHUB_CALLS")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(72)
	}
	fmt.Fprintln(file, call)
	if file.Close() != nil {
		os.Exit(72)
	}
}

func helperAPM(args []string) {
	if os.Getenv("REVIEWCTL_FAKE_DOCTOR_APM_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "scripted APM failure")
		os.Exit(75)
	}
	root := ""
	for i := range args {
		if args[i] == "--root" && i+1 < len(args) {
			root = args[i+1]
			break
		}
	}
	if root == "" {
		os.Exit(94)
	}
	if ready := os.Getenv("REVIEWCTL_FAKE_APM_READY"); ready != "" {
		descendant := exec.Command(os.Args[0], "-test.run=TestCodexDescendantProcess")
		descendant.Env = append(os.Environ(), "GO_WANT_REVIEWCTL_DESCENDANT=1")
		descendant.Stdout, descendant.Stderr = os.Stdout, os.Stderr
		if descendant.Start() != nil ||
			os.WriteFile(ready, []byte(root+"\n"+strconv.Itoa(descendant.Process.Pid)), 0o600) != nil {
			os.Exit(93)
		}
		os.Exit(75)
	}
	dir := filepath.Join(root, ".agents", "skills", "adversarial-code-review")
	if os.MkdirAll(dir, 0o700) != nil || os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("review"), 0o600) != nil {
		os.Exit(93)
	}
}

func helperCodex(args []string) {
	if len(args) == 2 && args[0] == "login" && args[1] == "status" {
		if os.Getenv("REVIEWCTL_FAKE_DOCTOR_CODEX_FAIL") != "" {
			fmt.Fprintln(os.Stderr, "scripted Codex failure")
			os.Exit(74)
		}
		fmt.Println("Logged in")
		return
	}
	if ready := os.Getenv("REVIEWCTL_FAKE_CODEX_READY"); ready != "" {
		descendant := exec.Command(os.Args[0], "-test.run=TestCodexDescendantProcess")
		descendant.Env = append(os.Environ(), "GO_WANT_REVIEWCTL_DESCENDANT=1")
		if descendant.Start() != nil {
			os.Exit(77)
		}
		workspace, _ := os.Getwd()
		if os.WriteFile(ready, []byte(workspace), 0o600) != nil {
			os.Exit(77)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if ready := os.Getenv("REVIEWCTL_FAKE_CODEX_LEAK_READY"); ready != "" {
		descendant := exec.Command(os.Args[0], "-test.run=TestCodexDescendantProcess")
		descendant.Env = append(os.Environ(), "GO_WANT_REVIEWCTL_DESCENDANT=1",
			"REVIEWCTL_FAKE_CODEX_LEADER_PID="+strconv.Itoa(os.Getpid()))
		descendant.Stdout, descendant.Stderr = os.Stdout, os.Stderr
		if descendant.Start() != nil {
			os.Exit(73)
		}
		workspace, _ := os.Getwd()
		if os.WriteFile(ready, []byte(workspace+"\n"+strconv.Itoa(os.Getpid())), 0o600) != nil {
			os.Exit(73)
		}
		return
	}
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
