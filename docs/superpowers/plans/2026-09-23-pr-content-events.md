# PR Content Events Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Queue and safely review eligible pull requests after description changes or new human messages.

**Architecture:** Extend discovery with a paginated GitHub external-input snapshot and persist a deterministic revision
alongside the existing PR baseline. Carry that revision through the queue, marker, trusted prompt, receipt, and history;
refresh it before and after an attempt to close races without changing manual queue behavior.

**Tech Stack:** Go, GitHub GraphQL through `gh`, SQLite through `modernc.org/sqlite`, existing Go tests.

**Spec:** `docs/superpowers/specs/2026-09-23-pr-content-events-design.md`

## Global Constraints

- Treat the description and every new non-empty `User` message as external input.
- Exclude the authenticated publishing account and GitHub `Bot` actors.
- Comment edits and deletions must not create a new revision.
- Preserve legacy head-only markers for manually queued and pre-migration entries.
- Add no dependency, webhook, table, or command.
- Repository content stays in English and Markdown body lines stay within 120 characters.

## Review Focus

- A message arriving while Codex runs must leave the PR queued with the newer revision; Task 3 adds the race test.
- A migrated baseline with no input revision must seed silently instead of queueing every open PR; Task 2 adds the
  migration test.
- More than 100 messages or review threads must be paginated without accepting a partial snapshot; Task 1 adds the
  pagination test.
- Editing or deleting an observed message must not queue another review; Task 2 adds the monotonic-ID test.
- Multiple events while a PR is already queued must retain the newest revision without increasing the enqueue count;
  Task 2 adds the coalescing test.

---

### Task 1: Read deterministic external input

**Files:**
- Modify: `github.go`
- Modify: `attempt_test.go`
- Modify: `integration_test.go`

**Interfaces:**
- Consumes: `PullRequest`, the existing `gh api graphql` process pattern, and `normalizeGitHubLogin`.
- Produces: `githubExternalInput`, `readGitHubExternalInput(context.Context, PullRequest)`,
  `mergeExternalInput(string, []string, githubExternalInput)`, and `externalInputRevision(string, []string)`.

- [ ] **Step 1: Add failing classification and revision tests**

Add table tests proving that the wished-for API:

```go
type githubExternalInput struct {
    Body       string
    MessageIDs []string
}

bodyDigest, ids, revision := mergeExternalInput(previousBodyDigest, previousIDs, current)
```

sorts and deduplicates IDs, retains previously seen IDs, changes for a different body or added ID, and stays unchanged
for edits, deletions, empty messages, `Bot` actors, and the authenticated publisher.

- [ ] **Step 2: Run the focused tests and verify RED**

Run: `env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... -run 'Test(ExternalInput|DecodeGitHubExternalInput)'`

Expected: compilation fails because `githubExternalInput` and its helpers do not exist.

- [ ] **Step 3: Implement the smallest snapshot and hash helpers**

Use `crypto/sha256`, `encoding/hex`, `sort`, and `encoding/json`. Hash the body digest followed by sorted unique GraphQL
node IDs with NUL separators. Keep message bodies only long enough to reject whitespace-only messages; persist IDs, not
content.

- [ ] **Step 4: Add a failing paginated GitHub fixture**

Extend the fake `gh api graphql` path in `integration_test.go` so one PR returns page cursors for issue comments,
reviews, review threads, and comments in a thread. Assert that `readGitHubExternalInput` returns every human message ID
and rejects a missing page or missing authenticated viewer.

- [ ] **Step 5: Run the pagination test and verify RED**

Run: `env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... -run TestReadGitHubExternalInputPagination`

Expected: FAIL because `readGitHubExternalInput` does not yet follow all cursors.

- [ ] **Step 6: Implement the paginated GraphQL reader**

Make one initial query for `viewer.login`, PR body, issue comments, reviews, and review threads. Follow each `pageInfo`
cursor, including per-thread comment cursors. Accept only `User` actors whose normalized login differs from the viewer;
return `github_failed` on incomplete or malformed data.

- [ ] **Step 7: Run Task 1 tests and commit**

Run:

```bash
env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... \
  -run 'Test(ExternalInput|DecodeGitHubExternalInput|ReadGitHubExternalInputPagination)'
```

Expected: PASS.

Commit: `feat(discovery): read PR external input`

### Task 2: Persist revisions and queue content events

**Files:**
- Modify: `store.go`
- Modify: `github.go`
- Modify: `cli.go`
- Modify: `discovery_test.go`
- Modify: `integration_test.go`

**Interfaces:**
- Consumes: Task 1's `githubExternalInput`, merge helper, and revision helper.
- Produces: `PullRequest.InputRevision`, persisted baseline state, discovery queue coalescing, and
  `Store.MergeQueuedInput(context.Context, PullRequest, githubExternalInput) (string, error)` for Task 3.

- [ ] **Step 1: Add failing schema and event-matrix tests**

Extend `TestShouldEnqueueDiscoveryEventMatrix` and store tests with snapshots containing:

```go
DescriptionDigest string
MessageIDs        []string
InputRevision     string
```

Cover description change, each new message class, an unchanged snapshot, a legacy blank revision, a deleted or edited
message, and a ready/head transition.

- [ ] **Step 2: Run the discovery tests and verify RED**

Run:

```bash
env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... \
  -run 'Test(ShouldEnqueueDiscovery|StoreAppliesDiscovery|DiscoveryInput)'
```

Expected: compilation or assertion failure because baseline and queue revisions are absent.

- [ ] **Step 3: Add additive migrations and revision-aware snapshots**

Add non-null columns with safe defaults:

```sql
queue.input_revision TEXT NOT NULL DEFAULT ''
history.input_revision TEXT NOT NULL DEFAULT ''
pull_request_baselines.description_digest TEXT NOT NULL DEFAULT ''
pull_request_baselines.message_ids_json TEXT NOT NULL DEFAULT '[]'
pull_request_baselines.input_revision TEXT NOT NULL DEFAULT ''
```

Add `InputRevision string` to `PullRequest` and `Attempt`. Decode stored ID JSON strictly and fail without replacing a
baseline when it is corrupt.

- [ ] **Step 4: Implement discovery enrichment and coalescing**

After the existing list and trusted-author filter, call `readGitHubExternalInput` for every ready PR and merge it with
the prior baseline. Queue when the old non-empty revision differs, or when existing new/ready/head rules apply. Use
`INSERT OR IGNORE` to preserve the enqueue count, followed by `UPDATE queue SET input_revision = ?` to retain the latest
revision for an already queued PR.

- [ ] **Step 5: Add and verify the coalescing and migration tests**

Assert that two changes before processing produce one queue row with the second revision; first observation of legacy
rows records the revision without queueing; deleting or editing a known message leaves the revision unchanged.

Run:

```bash
env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... \
  -run 'Test(ShouldEnqueueDiscovery|StoreAppliesDiscovery|DiscoveryInput|DiscoveryCoalesces)'
```

Expected: PASS.

- [ ] **Step 6: Implement `Store.MergeQueuedInput` with a failing test first**

Write a test that seeds a baseline and queue row, merges one new ID, and expects both rows to hold the same new
revision. Then implement one transaction that reads and validates the baseline, unions IDs, updates the baseline, and
updates the existing queue row without creating a manual queue entry.

Run: `env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... -run TestStoreMergeQueuedInput`

Expected: PASS after implementation.

- [ ] **Step 7: Run Task 2 tests and commit**

Run:

```bash
env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... \
  -run 'Test(ShouldEnqueueDiscovery|StoreAppliesDiscovery|Discovery|StoreMergeQueuedInput)'
```

Expected: PASS.

Commit: `feat(discovery): queue PR content events`

### Task 3: Pin attempts to the external input revision

**Files:**
- Modify: `attempt.go`
- Modify: `signature.go`
- Modify: `store.go`
- Modify: `attempt_test.go`
- Modify: `integration_test.go`

**Interfaces:**
- Consumes: Task 2's `PullRequest.InputRevision` and `Store.MergeQueuedInput`.
- Produces: revision-aware marker, trusted prompt, receipt validation, history, and the final input race check.

- [ ] **Step 1: Add failing marker, prompt, receipt, and history tests**

For a non-empty revision, require:

```text
<!-- reviewctl:github:owner/repo#1:HEAD:DIGEST:INPUT_REVISION -->
```

and an `input_revision` field in the trusted prompt, receipt, and history. For an empty revision, require the
byte-for-byte legacy marker and accept a receipt with no revision.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... \
  -run 'Test(ReviewMarker|TrustedInstruction|ValidateReceipt|Status)'
```

Expected: assertions fail because the revision is not represented.

- [ ] **Step 3: Implement revision-aware attempt identity**

Add `InputRevision string` to `Receipt`; have `reviewMarker`, `trustedInstruction`, `ValidateReceipt`, history
persistence, and signature recovery use `PullRequest.InputRevision`. Keep the old marker format only when the field is
empty.

- [ ] **Step 4: Add a failing attempt race test**

Extend the fake GitHub/Codex integration so the initial refresh adds message `A`, Codex publishes for that revision,
and the final refresh exposes message `B`. Assert `ProcessAttempt` returns `input_changed`, the queue remains, and its
stored revision includes both IDs.

- [ ] **Step 5: Run the race test and verify RED**

Run:

```bash
env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... \
  -run TestProcessAttemptRetainsQueueWhenExternalInputChanges
```

Expected: FAIL because the attempt neither refreshes nor compares external input.

- [ ] **Step 6: Refresh before and after event-triggered attempts**

When `InputRevision` is non-empty, read GitHub input and call `Store.MergeQueuedInput` before installing the skill; use
the returned revision for the marker and receipt. Repeat after GitHub review/discussion readback and before success. If
the revision changed, return `input_changed`; normal failure handling leaves the updated queue entry in place. Skip both
refreshes for legacy/manual entries.

- [ ] **Step 7: Run Task 3 tests and commit**

Run:

```bash
env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... \
  -run 'Test(ReviewMarker|TrustedInstruction|ValidateReceipt|ProcessAttempt|Status)'
```

Expected: PASS.

Commit: `feat(review): bind attempts to PR content`

### Task 4: Clarify instruction scope and verify the complete change

**Files:**
- Modify: `AGENTS.md`
- Modify: `integration_test.go`

**Interfaces:**
- Consumes: Tasks 1-3 complete behavior and the approved design's publication boundary.
- Produces: an unambiguous live-E2E rule and end-to-end discovery evidence.

- [ ] **Step 1: Capture instruction evidence and add a failing end-to-end test**

Preserve the complete pre-edit `AGENTS.md` in the plan workspace. Record the current `README.md` fixture command,
`scripts/live-e2e.sh`, trusted prompt, pinned skill contract, relevant task transcript, memory findings, and GitHub
review search as the evidence ledger required by `agents-md-authoring`.

Add `TestFullProcessQueuesPRContentEvents`: baseline an eligible PR, then change only its body, then add one human issue
comment, review body, inline comment, and reply across successive cycles. Assert one attempt per cycle and no attempt
for publisher or bot messages.

- [ ] **Step 2: Run the end-to-end test and verify RED**

Run: `env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... -run TestFullProcessQueuesPRContentEvents`

Expected: FAIL until all discovery fixtures and production paths are connected.

- [ ] **Step 3: Complete the integration fixture and make it pass**

Teach the fake `gh` process to serve the new GraphQL input queries and receipt revision. Do not add a separate mock
framework or production hook.

Run: `env GOCACHE=/private/tmp/reviewctl-go-cache go test ./... -run TestFullProcessQueuesPRContentEvents`

Expected: PASS.

- [ ] **Step 4: Rewrite only the ambiguous `AGENTS.md` boundary**

Replace the live E2E paragraph with this durable distinction:

```markdown
- Publish live E2E reviews only through `make live-e2e` or `scripts/live-e2e.sh`, and only to the canonical fixture PR
  documented in `README.md`. This fixture restriction does not apply to configured production `reviewctl run`
  executions; those may publish and synchronize owned discussions only within the trusted prompt and pinned skill
  contract.
```

Retain the existing pre-publication identity/head and post-publication readback requirements. Add no incident narrative.

- [ ] **Step 5: Run the required neutral instruction review**

Give a fresh reviewer the complete before and after `AGENTS.md`, diff, active instructions, evidence pointers, and the
`agents-md-authoring` selection contract. Apply actionable findings; allow at most two cycles.

- [ ] **Step 6: Run full verification and commit**

Run: `env GOCACHE=/private/tmp/reviewctl-go-cache go test ./...`

Expected: PASS with no warnings.

Run:

```bash
git diff --check
awk 'length($0) > 120 { print FNR ":" length($0) ":" $0 }' \
  AGENTS.md docs/superpowers/specs/2026-09-23-pr-content-events-design.md \
  docs/superpowers/plans/2026-09-23-pr-content-events.md
```

Expected: no output.

Commit: `docs: clarify ReviewCTL publication scope`

### Task 5: Review the branch and prepare handoff

**Files:**
- Modify only files required by Critical or Important review findings, each through a failing regression test.

**Interfaces:**
- Consumes: the full branch diff, spec, plan, ledger rulings, and Review Focus list.
- Produces: a clean whole-branch review and verified branch ready for integration choice.

- [ ] **Step 1: Run the complete repository gate**

Run: `env GOCACHE=/private/tmp/reviewctl-go-cache make ci`

Expected: PASS.

- [ ] **Step 2: Build and dispatch the final review package**

Use the `executing-plans` review-package script from merge base `origin/main` to `HEAD`. Dispatch one fresh-context
reviewer using `superpowers:requesting-code-review`, the spec, this plan, the Review Focus list, and ledger rulings.

- [ ] **Step 3: Apply at most one Critical/Important fix pass**

For every accepted finding, add a regression test, watch it fail, make the smallest fix, and rerun the full suite.
Record deferred Minor findings and explicit rulings in the ledger; do not silently change scope.

- [ ] **Step 4: Verify the final branch**

Run: `git diff --check origin/main...HEAD && env GOCACHE=/private/tmp/reviewctl-go-cache make ci`

Expected: PASS.

- [ ] **Step 5: Use `superpowers:finishing-a-development-branch`**

Present the verified integration choices without pushing, merging, or deploying unless the user explicitly authorizes
that external change.
