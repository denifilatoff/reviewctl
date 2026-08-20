package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
