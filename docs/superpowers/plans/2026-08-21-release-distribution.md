# Release Distribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish verified macOS and Linux binaries and provide one short agent-friendly installation command.

**Architecture:** A manually dispatched GitHub Actions workflow validates the version, runs the repository checks,
cross-compiles four binaries, creates checksums, and publishes one GitHub Release with `gh`. A POSIX shell installer
selects the current macOS or Linux asset, verifies its checksum, and installs it under the user's home directory.

**Tech Stack:** Go, POSIX shell, GitHub Actions, GitHub CLI

**Spec:** This document records the release design approved in the 2026-08-21 task conversation.

## Global Constraints

- Support native macOS and Linux on `amd64` and `arm64`; do not claim native Windows support.
- Run releases manually from `main` with an exact version matching `vMAJOR.MINOR.PATCH`.
- Use native GitHub CLI release publication; do not add GoReleaser, release-drafter, or third-party release actions.
- Publish exactly four binaries, `SHA256SUMS`, and `install.sh`.
- Install to `${HOME}/.local/bin` by default without `sudo` or shell-profile mutation.
- Preserve the canonical live E2E pull request link in `README.md`.
- Keep repository content and commit messages in American English.
- Do not push or change GitHub issue status.

---

### Task 1: Release workflow and installer

**Files:**
- Create: `.github/workflows/release.yaml`
- Create: `scripts/install.sh`
- Create: `scripts/install_test.sh`
- Modify: `cli.go`
- Modify: `cli_test.go`
- Modify: `Makefile`
- Modify: `README.md`

**Interfaces:**
- Consumes: GitHub workflow input `version`, repository Go module, and GitHub-provided `gh` authentication.
- Produces: assets named `reviewctl-{darwin,linux}-{amd64,arm64}`, `SHA256SUMS`, and `install.sh`.
- Produces: `reviewctl --version` output set by `go build -ldflags "-X main.Version=vMAJOR.MINOR.PATCH"`.
- Produces: installer overrides `REVIEWCTL_INSTALL_VERSION`, `REVIEWCTL_INSTALL_BASE_URL`, and
  `REVIEWCTL_INSTALL_DIR` for pinned and test installations.

- [x] **Step 1: Add a failing version-injection test**

Add `TestMainVersionUsesBuildValue` to `cli_test.go`. Save `Version`, assign `v9.8.7`, restore it with `t.Cleanup`, call
`Main([]string{"--version"}, ...)`, and require exit code `0`, stdout `v9.8.7\n`, and empty stderr.

- [x] **Step 2: Run the focused test and verify RED**

Run: `go test ./... -run '^TestMainVersionUsesBuildValue$'`

Expected: compilation fails because the current `Version` declaration is a constant and cannot be assigned.

- [x] **Step 3: Make the release version injectable**

Replace `const Version = "0.1.0"` with `var Version = "dev"` in `cli.go`. Do not add a version package or runtime lookup.

- [x] **Step 4: Run the focused test and verify GREEN**

Run: `go test ./... -run '^TestMainVersionUsesBuildValue$'`

Expected: PASS.

- [x] **Step 5: Add a failing installer integration test**

Create `scripts/install_test.sh`. It must build a temporary release tree for the host OS and architecture, create a
small executable fixture, generate its checksum using `sha256sum` or `shasum`, run the real installer with all three
overrides, and execute the installed fixture. Then replace the checksum with zeros, require installation to fail, and
verify that the previously installed file was not overwritten.

- [x] **Step 6: Run the installer test and verify RED**

Run: `sh scripts/install_test.sh`

Expected: FAIL because `scripts/install.sh` does not exist.

- [x] **Step 7: Implement the minimal installer**

Create a POSIX `scripts/install.sh` that:

- accepts no arguments;
- maps `Darwin`/`Linux` and `x86_64|amd64`/`arm64|aarch64` to the published asset names;
- downloads the selected binary and `SHA256SUMS` with `curl -fsSL`;
- verifies SHA-256 with `sha256sum` or `shasum -a 256` before touching the destination;
- installs atomically as executable `${REVIEWCTL_INSTALL_DIR:-$HOME/.local/bin}/reviewctl`;
- defaults to the latest release but honors the three documented environment overrides; and
- prints the installed path and a concise PATH hint when the command is not discoverable.

- [x] **Step 8: Run installer tests and verify GREEN**

Run: `sh scripts/install_test.sh`

Expected: PASS with both successful-install and checksum-rejection assertions.

- [x] **Step 9: Add the release workflow**

Create `.github/workflows/release.yaml` based on the proven `ai-agent-telemetry` workflow shape, reduced to this CLI:

- `workflow_dispatch` requires `version`;
- one concurrency group prevents overlapping releases;
- preflight requires `main`, validates `^v[0-9]+\.[0-9]+\.[0-9]+$`, and fails closed unless the remote tag is absent;
- checks run formatting, vet, Go tests, race tests, the installer integration test, and shell syntax checks;
- a four-entry matrix builds with `CGO_ENABLED=0`, `-trimpath`, and
  `-ldflags "-s -w -X main.Version=${VERSION}"`;
- artifacts use pinned official checkout, setup-go, upload-artifact, and download-artifact action SHAs copied from the
  analogous repository;
- the release job stages `install.sh`, generates checksums for all four binaries, verifies the exact six-file payload,
  publishes it with `gh release create --generate-notes`, and reads back the asset names.

- [x] **Step 10: Connect installer verification to local checks**

Update `Makefile` so `test` runs `go test ./...` followed by `sh scripts/install_test.sh`. Keep existing targets and the
frozen APM check unchanged.

- [x] **Step 11: Replace the README with the approved minimal contract**

Keep a short description, a macOS/Linux install block containing the `curl ... | sh`, PATH export, and
`reviewctl --help` commands, one dependency sentence for authenticated GitHub CLI, Codex CLI, and APM, plus the
canonical pull request 24 live E2E restriction. Do not include build-from-source or release-maintainer instructions.

- [x] **Step 12: Run full verification**

Run:

```sh
make ci
go test -race ./...
sh -n scripts/install.sh scripts/install_test.sh
git diff --check
```

Expected: every command exits `0` with no formatting or test failures.

- [x] **Step 13: Commit the task**

Stage only the eight task files plus this plan and commit:

```text
feat: add release distribution
```
