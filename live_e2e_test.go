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

func TestLiveE2EMarkerPublicationContract(t *testing.T) {
	const fixtureURL = "https://github.com/denifilatoff/reviewctl/pull/24"
	for _, test := range []struct {
		name          string
		mode          string
		initialMarker string
		wantSuccess   bool
		prURL         string
		repository    string
		author        string
		wantNoCalls   bool
	}{
		{name: "publishes then recovers", mode: "publish", initialMarker: "0", wantSuccess: true, prURL: fixtureURL},
		{name: "rejects recovery on first run", mode: "recover", initialMarker: "1", prURL: fixtureURL},
		{
			name: "rejects external repository before commands", mode: "recover", initialMarker: "0",
			prURL: "https://github.com/example/other/pull/1", repository: "example/other", author: "mallory",
			wantNoCalls: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			calls := filepath.Join(temp, "calls")
			if err := os.WriteFile(markerState, []byte(test.initialMarker), 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("sh", "scripts/live-e2e.sh")
			command.Env = append(os.Environ(),
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"REVIEWCTL_BIN="+filepath.Join(fakeBin, "reviewctl"),
				"REVIEWCTL_LIVE_MODE="+test.mode,
				"REVIEWCTL_LIVE_MARKER_STATE="+markerState,
				"REVIEWCTL_LIVE_RUN_STATE="+filepath.Join(temp, "runs"),
				"REVIEWCTL_LIVE_CALLS="+calls,
				"REVIEWCTL_LIVE_PR_URL="+test.prURL,
				"REVIEWCTL_LIVE_REPOSITORY="+test.repository,
				"REVIEWCTL_LIVE_TRUSTED_AUTHOR="+test.author,
			)
			output, err := command.CombinedOutput()
			if test.wantSuccess && err != nil {
				t.Fatalf("live script failed: %v\n%s", err, output)
			}
			if !test.wantSuccess && err == nil {
				t.Fatalf("live script accepted first-run recovery:\n%s", output)
			}
			if test.wantNoCalls {
				if _, statErr := os.Stat(calls); !os.IsNotExist(statErr) {
					t.Fatalf("external fixture invoked a helper: %v", statErr)
				}
			}
		})
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
	if calls := os.Getenv("REVIEWCTL_LIVE_CALLS"); calls != "" {
		file, err := os.OpenFile(calls, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(80)
		}
		fmt.Fprintln(file, name)
		file.Close()
	}
	const head = "7df844ae3da63875d3326249a49e04b639f93e36"
	switch name {
	case "gh":
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "auth status"):
		case strings.HasPrefix(joined, "pr view") && strings.Contains(joined, "state,isDraft,author,headRefOid"):
			fmt.Printf("OPEN\tfalse\tdenifilatoff\t%s\n", head)
		case strings.HasPrefix(joined, "pr view"):
			fmt.Println(head)
		case strings.Contains(joined, "--jq .user.login"):
			fmt.Println("denifilatoff")
		case strings.Contains(joined, "--jq .head.sha"):
			fmt.Println(head)
		case strings.Contains(joined, "/reviews"):
			count := readTestCounter(os.Getenv("REVIEWCTL_LIVE_MARKER_STATE"))
			for i := 0; i < count; i++ {
				fmt.Println(i + 1)
			}
		default:
			os.Exit(81)
		}
	case "codex":
	case "apm":
	case "sqlite3":
		query := strings.Join(args, " ")
		runs := readTestCounter(os.Getenv("REVIEWCTL_LIVE_RUN_STATE"))
		if strings.Contains(query, "repository_baselines") {
			fmt.Println("1")
		} else if strings.Contains(query, "success = 1") {
			fmt.Println("2")
		} else if strings.Contains(query, "success = 0") {
			fmt.Println("1")
		} else if strings.Contains(query, "history") {
			if runs <= 2 {
				fmt.Println("1")
			} else {
				fmt.Println("3")
			}
		} else {
			if runs <= 2 {
				fmt.Println("1")
			} else {
				fmt.Println("0")
			}
		}
	case "reviewctl":
		if len(args) < 2 {
			os.Exit(82)
		}
		if args[1] == "review" {
			fmt.Println(`{"command":"review","status":"queued"}`)
			break
		}
		runState := os.Getenv("REVIEWCTL_LIVE_RUN_STATE")
		runs := readTestCounter(runState)
		if os.WriteFile(runState, []byte(strconv.Itoa(runs+1)), 0o600) != nil {
			os.Exit(83)
		}
		if runs == 0 {
			fmt.Println(`{"command":"run","status":"success","discovery_succeeded":1,"queued":0,"attempted":0}`)
			break
		}
		if runs == 1 {
			fmt.Println(`{"command":"run","status":"failed","error":{"code":"publication_disabled"}}`)
			os.Exit(1)
		}
		recovered := true
		if os.Getenv("REVIEWCTL_LIVE_MODE") == "publish" && runs == 2 {
			recovered = false
			markerState := os.Getenv("REVIEWCTL_LIVE_MARKER_STATE")
			markers := readTestCounter(markerState)
			if os.WriteFile(markerState, []byte(strconv.Itoa(markers+1)), 0o600) != nil {
				os.Exit(84)
			}
		}
		fmt.Printf("{\"command\":\"run\",\"verdict\":\"COMMENT\",\"recovered\":%t}\n", recovered)
	default:
		os.Exit(85)
	}
	os.Exit(0)
}

func readTestCounter(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, _ := strconv.Atoi(string(data))
	return value
}
