package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	liveFixtureURL  = "https://github.com/denifilatoff/reviewctl/pull/24"
	liveFixtureHead = "7df844ae3da63875d3326249a49e04b639f93e36"
)

func TestLiveE2EMarkerPublicationContract(t *testing.T) {
	for _, test := range []struct {
		name          string
		initialMarker string
		wantMode      string
		wantSuccess   bool
	}{
		{name: "publishes then recovers", initialMarker: "0", wantMode: "publication", wantSuccess: true},
		{name: "recovers twice on later run", initialMarker: "1", wantMode: "recovery", wantSuccess: true},
		{name: "rejects duplicate markers", initialMarker: "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			temp, fakeBin, markerState, calls := prepareLiveHelpers(t, test.initialMarker)
			command := exec.Command("sh", "scripts/live-e2e.sh")
			command.Env = liveHelperEnv(temp, fakeBin, markerState, calls)
			output, err := command.CombinedOutput()
			if test.wantSuccess && err != nil {
				t.Fatalf("live script failed: %v\n%s", err, output)
			}
			if !test.wantSuccess && err == nil {
				t.Fatalf("live script accepted duplicate markers:\n%s", output)
			}
			if test.wantSuccess {
				if !strings.Contains(string(output), "mode="+test.wantMode) {
					t.Fatalf("live mode output = %q", output)
				}
				assertLiveCallOrder(t, calls)
			}
		})
	}
}

func TestLiveE2ERejectsExternalRepositoryBeforeCommands(t *testing.T) {
	temp, fakeBin, markerState, calls := prepareLiveHelpers(t, "0")
	env := liveHelperEnv(temp, fakeBin, markerState, calls)
	env = append(env,
		"REVIEWCTL_LIVE_PR_URL=https://github.com/example/other/pull/1",
		"REVIEWCTL_LIVE_REPOSITORY=example/other",
		"REVIEWCTL_LIVE_TRUSTED_AUTHOR=mallory",
	)
	command := exec.Command("sh", "scripts/live-e2e.sh")
	command.Env = env
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("live script accepted external repository:\n%s", output)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("external fixture invoked a helper: %v", err)
	}
}

func TestLiveE2EReviewctlHelperRejectsUnknownSubcommand(t *testing.T) {
	temp, fakeBin, markerState, calls := prepareLiveHelpers(t, "0")
	command := exec.Command(filepath.Join(fakeBin, "reviewctl"), "--json", "definitely-not-run")
	command.Env = liveHelperEnv(temp, fakeBin, markerState, calls)
	if err := command.Run(); err == nil {
		t.Fatal("reviewctl helper accepted an unknown subcommand")
	}
}

func prepareLiveHelpers(t *testing.T, initialMarker string) (string, string, string, string) {
	t.Helper()
	temp := t.TempDir()
	fakeBin := filepath.Join(temp, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gh", "apm", "codex", "sqlite3", "reviewctl"} {
		script := fmt.Sprintf("#!/bin/sh\nREVIEWCTL_LIVE_HELPER=%s exec %q -test.run=TestLiveE2EHelper -- \"$@\"\n", name, self)
		if err := os.WriteFile(filepath.Join(fakeBin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	markerState := filepath.Join(temp, "markers")
	if err := os.WriteFile(markerState, []byte(initialMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	return temp, fakeBin, markerState, filepath.Join(temp, "calls")
}

func liveHelperEnv(temp, fakeBin, markerState, calls string) []string {
	return append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REVIEWCTL_BIN="+filepath.Join(fakeBin, "reviewctl"),
		"REVIEWCTL_LIVE_MARKER_STATE="+markerState,
		"REVIEWCTL_LIVE_RUN_STATE="+filepath.Join(temp, "runs"),
		"REVIEWCTL_LIVE_CALLS="+calls,
	)
}

func assertLiveCallOrder(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"gh auth", "codex login", "gh preflight", "gh REST author", "gh REST head", "gh markers",
		"reviewctl baseline", "sqlite baseline", "reviewctl enqueue", "reviewctl safe failure",
		"sqlite retry history", "sqlite retry queue", "reviewctl first success", "gh markers", "gh head readback",
		"reviewctl enqueue", "reviewctl recovery", "gh markers", "gh head readback", "sqlite history",
		"sqlite successes", "sqlite failures", "sqlite queue",
	}, "\n") + "\n"
	if string(data) != want {
		t.Fatalf("helper calls:\n%s\nwant:\n%s", data, want)
	}
}

func TestLiveE2EHelper(t *testing.T) {
	name := os.Getenv("REVIEWCTL_LIVE_HELPER")
	if name == "" {
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
	switch name {
	case "gh":
		helperLiveGH(args)
	case "codex":
		if !sameArgs(args, "login", "status") {
			os.Exit(81)
		}
		appendLiveCall("codex login")
	case "sqlite3":
		helperLiveSQLite(args)
	case "reviewctl":
		helperLiveReviewctl(args)
	case "apm":
		os.Exit(81)
	default:
		os.Exit(81)
	}
	os.Exit(0)
}

func helperLiveGH(args []string) {
	markerQuery := fmt.Sprintf(".[] | select(.body != null and (.body | contains(\"<!-- reviewctl:github:denifilatoff/reviewctl#24:%s:\"))) | .id", liveFixtureHead)
	switch {
	case sameArgs(args, "auth", "status"):
		appendLiveCall("gh auth")
	case sameArgs(args, "pr", "view", liveFixtureURL, "--json", "state,isDraft,author,headRefOid", "--jq",
		"[.state, (.isDraft|tostring), .author.login, .headRefOid] | @tsv"):
		appendLiveCall("gh preflight")
		fmt.Printf("OPEN\tfalse\tdenifilatoff\t%s\n", liveFixtureHead)
	case sameArgs(args, "api", "repos/denifilatoff/reviewctl/pulls/24", "--jq", ".user.login"):
		appendLiveCall("gh REST author")
		fmt.Println("denifilatoff")
	case sameArgs(args, "api", "repos/denifilatoff/reviewctl/pulls/24", "--jq", ".head.sha"):
		appendLiveCall("gh REST head")
		fmt.Println(liveFixtureHead)
	case sameArgs(args, "api", "repos/denifilatoff/reviewctl/pulls/24/reviews", "--paginate", "--jq", markerQuery):
		appendLiveCall("gh markers")
		for i := 0; i < readTestCounter(os.Getenv("REVIEWCTL_LIVE_MARKER_STATE")); i++ {
			fmt.Println(i + 1)
		}
	case sameArgs(args, "pr", "view", liveFixtureURL, "--json", "headRefOid", "--jq", ".headRefOid"):
		appendLiveCall("gh head readback")
		fmt.Println(liveFixtureHead)
	default:
		os.Exit(81)
	}
}

func helperLiveSQLite(args []string) {
	if len(args) != 2 || !strings.HasSuffix(args[0], "/reviewctl/reviewctl.db") {
		os.Exit(81)
	}
	runs := readTestCounter(os.Getenv("REVIEWCTL_LIVE_RUN_STATE"))
	switch args[1] {
	case "SELECT COUNT(*) FROM repository_baselines WHERE provider = 'github' AND repository = 'denifilatoff/reviewctl';":
		appendLiveCall("sqlite baseline")
		fmt.Println("1")
	case "SELECT COUNT(*) FROM history;":
		if runs == 2 {
			appendLiveCall("sqlite retry history")
			fmt.Println("1")
		} else if runs == 4 {
			appendLiveCall("sqlite history")
			fmt.Println("3")
		} else {
			os.Exit(81)
		}
	case "SELECT COUNT(*) FROM history WHERE success = 1;":
		appendLiveCall("sqlite successes")
		fmt.Println("2")
	case "SELECT COUNT(*) FROM history WHERE success = 0;":
		appendLiveCall("sqlite failures")
		fmt.Println("1")
	case "SELECT COUNT(*) FROM queue;":
		if runs == 2 {
			appendLiveCall("sqlite retry queue")
			fmt.Println("1")
		} else if runs == 4 {
			appendLiveCall("sqlite queue")
			fmt.Println("0")
		} else {
			os.Exit(81)
		}
	default:
		os.Exit(81)
	}
}

func helperLiveReviewctl(args []string) {
	if sameArgs(args, "--json", "review", liveFixtureURL) {
		appendLiveCall("reviewctl enqueue")
		fmt.Println(`{"command":"review","status":"queued"}`)
		return
	}
	if !sameArgs(args, "--json", "run") {
		os.Exit(82)
	}
	runState := os.Getenv("REVIEWCTL_LIVE_RUN_STATE")
	runs := readTestCounter(runState)
	if os.WriteFile(runState, []byte(strconv.Itoa(runs+1)), 0o600) != nil {
		os.Exit(83)
	}
	switch runs {
	case 0:
		appendLiveCall("reviewctl baseline")
		fmt.Println(`{"command":"run","status":"success","discovery_succeeded":1,"queued":0,"attempted":0}`)
	case 1:
		appendLiveCall("reviewctl safe failure")
		fmt.Println(`{"command":"run","status":"failed","error":{"code":"publication_disabled"}}`)
		os.Exit(1)
	case 2:
		appendLiveCall("reviewctl first success")
		recovered := readTestCounter(os.Getenv("REVIEWCTL_LIVE_MARKER_STATE")) == 1
		if !recovered {
			if os.WriteFile(os.Getenv("REVIEWCTL_LIVE_MARKER_STATE"), []byte("1"), 0o600) != nil {
				os.Exit(84)
			}
		}
		fmt.Printf("{\"command\":\"run\",\"verdict\":\"COMMENT\",\"recovered\":%t}\n", recovered)
	case 3:
		appendLiveCall("reviewctl recovery")
		fmt.Println(`{"command":"run","verdict":"COMMENT","recovered":true}`)
	default:
		os.Exit(82)
	}
}

func appendLiveCall(value string) {
	file, err := os.OpenFile(os.Getenv("REVIEWCTL_LIVE_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		os.Exit(80)
	}
	if _, err := fmt.Fprintln(file, value); err != nil || file.Close() != nil {
		os.Exit(80)
	}
}

func sameArgs(actual []string, expected ...string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i := range actual {
		if actual[i] != expected[i] {
			return false
		}
	}
	return true
}

func readTestCounter(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, _ := strconv.Atoi(string(data))
	return value
}
