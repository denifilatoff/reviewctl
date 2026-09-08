package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockPathUsesXDGRuntimeDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/reviewctl-runtime")
	got := lockPath("/unused/reviewctl.db")
	want := filepath.Join("/tmp/reviewctl-runtime", "reviewctl", "run.lock")
	if got != want {
		t.Fatalf("lock path = %q, want %q", got, want)
	}
}

func TestDecodeConfigNormalizesTrustPolicy(t *testing.T) {
	cfg, err := DecodeConfig(strings.NewReader(`
harness: codex
publish: true
trusted_authors: ["Dependabot[bot]"]
repositories:
  - provider: github
    repository: Denifilatoff/Reviewctl
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedAuthors[0] != "dependabot[bot]" || cfg.Repositories[0].Repository != "denifilatoff/reviewctl" {
		t.Fatalf("config was not normalized: %+v", cfg)
	}
}

func TestDecodeConfigRejectsUnknownFields(t *testing.T) {
	_, err := DecodeConfig(strings.NewReader(`
harness: codex
publish: true
trusted_authors: [alice]
repositories:
  - provider: github
    repository: acme/service
unexpected: true
`))
	if err == nil {
		t.Fatal("expected unknown field to be rejected")
	}
}

func TestDecodeConfigRejectsMultipleDocuments(t *testing.T) {
	_, err := DecodeConfig(strings.NewReader(`
harness: codex
publish: true
trusted_authors: [alice]
repositories:
  - provider: github
    repository: acme/service
---
publish: false
`))
	if err == nil {
		t.Fatal("expected multiple YAML documents to be rejected")
	}
}

func TestDecodeConfigRequiresClosedMVPValues(t *testing.T) {
	_, err := DecodeConfig(strings.NewReader(`
harness: other
publish: false
trusted_authors: []
repositories: []
`))
	if err == nil {
		t.Fatal("expected unsupported and empty trust policy to be rejected")
	}
}

func TestDecodeConfigAllowsPublicationToFailClosedAtAttemptTime(t *testing.T) {
	_, err := DecodeConfig(strings.NewReader(`
harness: codex
publish: false
trusted_authors: [alice]
repositories:
  - provider: github
    repository: acme/service
`))
	if err != nil {
		t.Fatalf("publication policy should be checked before each attempt: %v", err)
	}
}

func TestDecodeConfigUsesBoundedAttemptTimeout(t *testing.T) {
	config := func(timeout string) string {
		return "harness: codex\npublish: true\ntrusted_authors: [alice]\nrepositories:\n" +
			"  - provider: github\n    repository: acme/service\n" + timeout
	}
	cfg, err := DecodeConfig(strings.NewReader(config("attempt_timeout: 2m\n")))
	if err != nil || cfg.AttemptTimeout != 2*time.Minute {
		t.Fatalf("configured timeout = %s, err = %v", cfg.AttemptTimeout, err)
	}
	cfg, err = DecodeConfig(strings.NewReader(config("")))
	if err != nil || cfg.AttemptTimeout != time.Hour || cfg.Model != "gpt-6-astra" || cfg.ReasoningEffort != "low" {
		t.Fatalf("defaults = timeout %s, model %q, effort %q, err = %v",
			cfg.AttemptTimeout, cfg.Model, cfg.ReasoningEffort, err)
	}
	for _, timeout := range []string{"0s", "-1s", "25h"} {
		if _, err := DecodeConfig(strings.NewReader(config("attempt_timeout: " + timeout + "\n"))); err == nil {
			t.Errorf("accepted timeout %s", timeout)
		}
	}
}
