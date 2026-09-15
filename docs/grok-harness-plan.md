# Grok Harness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Grok Build as an opt-in AI harness while keeping Codex as the default and preserving ReviewCTL's
publication, receipt, discussion-sync, timeout, and accounting guarantees.

**Architecture:** Keep the existing single-process flow and add one closed dispatch point that selects Codex or Grok.
Do not add a plugin registry, dynamic loading, or a generalized provider framework. Both runners receive the same trusted
review instruction and produce the existing `Receipt`; harness-specific code is limited to skill installation, process
arguments, output decoding, readiness checks, and cost extraction.

**Tech Stack:** Go, Grok Build CLI, Codex CLI, GitHub CLI, APM, SQLite, shell integration tests.

**Spec:** [ReviewCTL architecture](architecture.md), plus the official Grok Build documentation for
[headless mode](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/14-headless-mode.md),
[skills](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/08-skills.md), and
[sandboxing](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/18-sandbox.md).

## Feasibility and scope

This is a medium-sized integration, not a rewrite. The current code already has the right operational seam: one attempt
installs a locked skill, builds a trusted instruction, runs one CLI process, validates one receipt, verifies GitHub
state, and records the result. The Grok-specific work is concentrated around that seam.

The implementation should take about three to five engineering days, including deterministic tests and documentation.
A minimal runner that merely starts Grok is shorter, but it would omit readiness checks, exact skill selection, receipt
validation, launchd support, and trustworthy accounting. Those are existing ReviewCTL guarantees, so they are part of
the first usable implementation.

The implementation baseline was checked on 2026-09-15 with authenticated Grok Build `1.0.25 (f7e67d6988e2)` and the
available `grok-4.6` and `grok-4.5` models. Headless JSON Schema output returned one JSON envelope with a structured
result, `stopReason`, `sessionId`, usage, model usage, and cost fields. Recheck this contract if the installed Grok major
version changes.

## Decisions

- `harness` accepts `codex` or `grok`; existing configurations and defaults remain valid.
- Use a small `switch`, not a runtime plugin system. Promote it to an interface only when a third harness proves the
  common contract.
- Keep the current idempotency marker format. A review is identified by repository, pull request, head, and skill
  digest, not by the harness that happened to produce it. Switching harnesses must not publish a duplicate review.
- Install the same pinned APM package for both targets. Pass Grok the exact installed `SKILL.md` path because the source
  checkout is the Git repository root and Grok does not discover a sibling workspace skill automatically.
- Keep Grok's normal home so its existing authentication works. Do not copy credentials or synthesize `GROK_HOME`.
  Grok will retain its normal local session metadata under `~/.grok/sessions`; document this rather than deleting user
  state.
- Run Grok with the `workspace` sandbox and explicit tool restrictions. This preserves `gh` network access on Linux
  while limiting direct editing tools. The attempt checkout is temporary and remains covered by the trusted
  instruction and post-run GitHub/receipt validation.
- Preserve `codex_failed` for Codex compatibility and add `grok_failed` for Grok failures.
- Add the harness name to `ReviewCost` JSON for auditability. Keep the existing human review signature format; the model
  name already distinguishes Grok output, and changing the public signature is unnecessary.
- Trust Grok cost only when the successful JSON envelope contains complete cost data. Missing or partial cost is
  recorded as unavailable, never as zero. Do not infer the observed effort because Grok does not report it.

## Task 1: Make configuration defaults harness-aware

**Files:**

- Modify: `config.go`
- Modify: `config_test.go`

- [ ] Add failing table tests for `harness: grok`, rejection of unknown harnesses, and unchanged Codex defaults.

```go
func TestDecodeConfigAppliesHarnessDefaults(t *testing.T) {
	// codex -> gpt-6-astra/low
	// grok  -> grok-4.6/low
}
```

- [ ] Run `go test ./... -run 'TestDecodeConfig(AppliesHarnessDefaults|RequiresClosedMVPValues)'` and confirm the Grok
  case fails because only Codex is accepted.
- [ ] Decode the YAML first, validate `harness` with a two-case switch, then apply defaults only when `model` or
  `reasoning_effort` is omitted.

```go
switch cfg.Harness {
case "codex":
	model, effort = "gpt-6-astra", "low"
case "grok":
	model, effort = "grok-4.6", "low"
default:
	return cfg, fmt.Errorf("harness must be codex or grok")
}
```

- [ ] Keep the existing identifier and effort validation. Do not add a model catalog or a network lookup to config
  parsing.
- [ ] Update `initialConfig` only if needed to retain its existing Codex values verbatim.
- [ ] Run the focused tests and `go test ./...`.
- [ ] Commit: `feat: accept the Grok review harness`

## Task 2: Install the locked skill for the selected harness

**Files:**

- Modify: `apm.yml`
- Modify: `apm.lock.yaml` only if APM regeneration changes it
- Modify: `attempt.go`
- Modify: `attempt_test.go`
- Modify: `Makefile`

- [ ] Add failing tests that expect `installLockedSkills` to choose these target roots:

```text
codex -> <workspace>/.agents/skills/adversarial-code-review/SKILL.md
grok  -> <workspace>/.grok/skills/adversarial-code-review/SKILL.md
```

- [ ] Change `installLockedSkills` to accept the harness and return both the digest and exact skill path. Keep one
  implementation and one switch for target/path selection.

```go
type installedSkill struct {
	Digest string
	Path   string
}
```

- [ ] Pass `--target codex` or `--target grok-build` to the existing frozen APM install. Hash the harness-specific skill
  root, then verify the exact `SKILL.md` is a regular file below that root.
- [ ] Add `grok-build` to the manifest targets. Regenerate the lock only through APM; do not hand-edit generated hashes.
- [ ] Extend `make apm-check` to perform frozen installs for both targets in separate temporary roots and verify both
  expected skill files.
- [ ] Run `make apm-check` and the focused attempt tests.
- [ ] Commit: `build: install review skills for Codex and Grok`

## Task 3: Share the trusted contract without hiding harness differences

**Files:**

- Modify: `attempt.go`
- Modify: `attempt_test.go`

- [ ] Add failing tests for Codex and Grok instructions. Both must contain the pinned pull request, head, digest,
  idempotency marker, pre-existing discussion IDs, and exact installed skill path. Neither may contain instructions for
  the other CLI.
- [ ] Split `trustedInstruction` into a common contract plus a short harness-specific suffix. Keep GitHub publication,
  marker recovery, discussion synchronization, and receipt fields common.
- [ ] For Grok, explicitly require: "Open and follow this exact installed skill" followed by the absolute path. Require
  exactly one structured receipt in the final response; do not tell Grok to write the receipt file itself.
- [ ] For Codex, retain the current receipt-file and native-subagent/accounting instructions unchanged in effect.
- [ ] Run `go test ./... -run 'TestTrustedInstruction'` and `go test ./...`.
- [ ] Commit: `refactor: share the review receipt contract across harnesses`

## Task 4: Add the Grok runner and strict envelope decoder

**Files:**

- Create: `grok.go`
- Create: `grok_test.go`
- Modify: `attempt.go`
- Modify: `cost.go`
- Modify: `cost_test.go`

- [ ] Add decoder tests before the runner. Cover one valid envelope and rejection of: more than one JSON object,
  non-`end_turn` stops, missing session ID, missing structured output, schema-invalid receipts, negative or non-finite
  cost, and partial/incomplete usage. Ignore unknown envelope fields for forward compatibility; the embedded receipt
  remains strict.

```go
type grokResult struct {
	StructuredOutput json.RawMessage          `json:"structuredOutput"`
	StopReason       string                   `json:"stopReason"`
	SessionID        string                   `json:"sessionId"`
	ModelUsage       map[string]grokModelUsage `json:"modelUsage"`
	TotalCostUSD     *float64                 `json:"total_cost_usd"`
	CostIsPartial    bool                     `json:"cost_is_partial"`
	UsageIncomplete  bool                     `json:"usage_is_incomplete"`
}
```

- [ ] Store a minimal receipt JSON Schema as a Go constant that mirrors the existing `Receipt` contract. Set
  `additionalProperties: false` at the receipt level and for discussion outcomes; add a test that keeps the schema and
  Go validation cases aligned. Do not add a schema-generation dependency.
- [ ] Build the minimum supported command:

```text
grok --cwd <workspace> --prompt-file <instruction-path>
  --json-schema <receipt-schema> --always-approve --sandbox workspace
  --tools read_file,grep,list_dir,run_terminal_cmd --deny MCPTool
  --no-plan --no-subagents
  --disable-web-search --model <model> --effort <effort>
```

  Keep arguments as an `[]string`; never invoke a shell. Use the existing process-group cancellation and bounded output
  writers. Capture stdout and stderr separately because Grok may write plugin or hook warnings to stderr while stdout
  must remain machine-readable.
- [ ] On successful exit, require exactly one bounded JSON envelope and `stopReason == "end_turn"`. Decode
  `structuredOutput`, write it to the existing receipt path with mode `0600`, and let `readReceipt` plus
  `ValidateReceipt` remain the final trust boundary.
- [ ] Return `ReviewCost{Harness: "grok", RequestedModel: ..., RequestedEffort: ..., ThreadID: sessionId}`. Set the
  observed model only when exactly one `modelUsage` entry matches the requested model. Leave observed effort empty.
  Set `USD` only when `total_cost_usd` is present, positive and finite, and neither partial nor incomplete flag is set.
  Store only the bounded usage and model-usage fields in `Report`, not the response text or receipt.
- [ ] Add `Harness string \`json:"harness"\`` to `ReviewCost` and set it to `codex` in `collectCost`. No SQLite migration
  is needed because cost data is already stored as JSON.
- [ ] Add process tests with a fake `grok` executable for argv, stdout/stderr separation, cancellation, oversized output,
  malformed envelopes, and successful receipt/cost extraction.
- [ ] Run `go test ./... -run 'TestGrok|TestReviewCost'` and `go test ./...`.
- [ ] Commit: `feat: run reviews through Grok Build`

## Task 5: Dispatch attempts and doctor checks by harness

**Files:**

- Modify: `attempt.go`
- Modify: `cli.go`
- Modify: `integration_test.go`

- [ ] Add a failing integration test that runs one queued review with `harness: grok` and fake `gh`, `apm`, `git`, and
  `grok` executables. Assert one successful history row, queue removal, receipt validation, discussion validation, and a
  queued cost signature.
- [ ] Add one closed dispatch in `ProcessAttempt`:

```go
switch cfg.Harness {
case "codex":
	result.Cost, err = runCodex(...)
case "grok":
	result.Cost, err = runGrok(...)
}
```

  Map failures to `codex_failed` or `grok_failed`. Do not introduce a registry or reflection.
- [ ] Make `doctor` check only the configured harness. Preserve prerequisite name `codex` for Codex and use `grok` for
  Grok.
- [ ] Implement `checkGrok` with two bounded, non-billable checks: `grok models` for authentication/catalog access and
  an empty prompt-file compatibility probe using the production flags. Accept only the known "prompt is empty" failure;
  any successful run or unrelated error fails closed.
- [ ] Pass the selected harness into the locked-skill doctor check and retain `skill_digest` output.
- [ ] Extend fake-process tests for successful, unauthenticated, incompatible, timeout, and warning-on-stderr cases.
  Warnings may be diagnostic, but must never contaminate JSON stdout.
- [ ] Run `go test ./... -run 'TestDoctor|TestGrok|TestRun'` and `go test ./...`.
- [ ] Commit: `feat: verify and dispatch the selected harness`

## Task 6: Make launchd and live test infrastructure harness-aware

**Files:**

- Modify: `scripts/install-launchd.sh`
- Modify: `scripts/launchd-smoke.sh`
- Modify: `scripts/live-e2e.sh`
- Modify: `live_e2e_test.go`
- Modify: `integration_test.go`

- [ ] Add failing installer tests showing that a Codex config requires `codex`, a Grok config requires `grok`, and an
  unselected harness is not required in the launchd `PATH`.
- [ ] Resolve `codex` and `grok` as optional executables and add directories for whichever are installed to the
  scheduled `PATH`. Continue requiring `reviewctl`, `gh`, and `apm`; the existing doctor call then fails only when the
  selected harness is absent or unusable.
- [ ] Parameterize the fake live E2E harness while keeping Codex as the default. Assert the same pre-publication checks,
  exact marker recovery, duplicate rejection, discussion behavior, and post-run readback for Grok.
- [ ] Do not add the harness to the idempotency marker. To prove a real Grok publication later, use a newly authorized
  head on canonical PR #24; an existing marker should correctly exercise recovery instead.
- [ ] Run `go test ./... -run 'TestInstallLaunchd|TestLiveE2E'`, then `make launchd-smoke` on macOS.
- [ ] Commit: `test: cover Grok operations and publication flow`

## Task 7: Document the supported boundary and rollback

**Files:**

- Modify: `README.md`
- Modify: `docs/architecture.md`
- Modify: `docs/operations.md`
- Modify: `cli.go`

- [ ] Replace Codex-only product wording with "selected AI harness" where the behavior is shared. Keep specific Codex
  wording for Codex-only accounting and process details.
- [ ] Document `harness: grok`, its `grok-4.6`/`low` defaults, prerequisites, `doctor` behavior, local session retention,
  cost availability rules, and the fact that repository instructions and user-level Grok plugins/hooks may still load.
- [ ] Document the security trade-off: `workspace` keeps child network available for `gh`; direct editing tools are
  disabled, but shell commands can write inside the disposable attempt workspace. Receipt and GitHub readback remain
  authoritative.
- [ ] Document rollback as changing `harness` back to `codex`, running `reviewctl --json doctor`, and restarting the
  launchd job. No state migration or queue rewrite is required.
- [ ] Update help text so only `run` invokes the selected harness and may publish.
- [ ] Remove Grok from architecture non-goals but retain runtime harness plugins as a non-goal.
- [ ] Run `rg -n 'Codex is the only|only harness|harness other than Codex' README.md docs cli.go` and review every match.
- [ ] Run `git diff --check` and `make ci`.
- [ ] Commit: `docs: document Grok harness operations`

## Task 8: Review, publish, and validate

**Files:** all changed files.

- [ ] Rebase the implementation branch on the latest `origin/main` and repeat `make ci`.
- [ ] Run a focused fake end-to-end cycle for both `harness: codex` and `harness: grok` and compare queue, history,
  receipt, discussion, and signature results.
- [ ] Run `reviewctl --json doctor` locally once for each harness with temporary XDG configuration and state
  directories. Do not modify the user's active configuration.
- [ ] Request code review and resolve every blocking finding before merge.
- [ ] Open a ready pull request with a rollback section and the exact tested Grok version.
- [ ] Treat a real Grok review as a separate, explicitly authorized publication step. Use only canonical fixture PR #24,
  recheck its identity/open/head/marker preconditions, and verify GitHub readback and duplicate-free recovery afterward.
- [ ] Before closing any implementation issue, post either the qualifying ADR candidates or `ADR candidates: none` to
  the issue for owner approval. Do not create an ADR without that approval.

## Acceptance criteria

- Existing Codex configuration, reviews, failure codes, cost collection, and launchd behavior remain unchanged.
- `harness: grok` installs the pinned skill into the Grok target and passes its exact path to the trusted instruction.
- Grok runs headlessly without interactive approval, produces one schema-constrained receipt, and is killed with its
  descendants on timeout or cancellation.
- ReviewCTL rejects malformed, incomplete, duplicate, out-of-scope, or non-terminal Grok output before recording
  success.
- GitHub head, marker recovery, discussion lifecycle, review identity, and signature readback checks are identical for
  both harnesses.
- Missing or partial Grok cost is recorded as unavailable, not zero; complete reported cost is stored with
  `harness: grok`.
- `doctor`, launchd installation, fake E2E, documentation, and help follow the selected harness.
- No plugin system, provider rewrite, schema migration, or automatic deletion of Grok user state is introduced.
